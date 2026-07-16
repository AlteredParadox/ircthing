package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	ircv4 "gopkg.in/irc.v4"

	"ircthing/internal/irc"
	"ircthing/internal/netconf"
	"ircthing/internal/store"
)

// Network lifecycle: networks are defined in the store
// (network_configs) and started/stopped at runtime — at boot by main,
// afterwards by the put_network / delete_network session requests. The
// hub owns the goroutine pair per network (manager read loop + event
// consumer), consistent with it owning all fan-out.

type netProc struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when both goroutines have exited
}

// UseRoot supplies the process-lifetime context and waitgroup network
// connections run under. Called once at startup, before StartNetwork.
func (h *Hub) UseRoot(ctx context.Context, wg *sync.WaitGroup) {
	h.rootCtx = ctx
	h.rootWG = wg
}

// StartNetwork builds a manager for the definition and runs it under the
// root context. The caller must have stopped any previous instance of
// the same name (see netOps serialization in the session handlers).
func (h *Hub) StartNetwork(nc *netconf.Network) error {
	icfg, err := nc.IRCConfig()
	if err != nil {
		return err
	}
	// STS policies persist in the store so upgrade-to-TLS survives
	// restarts.
	icfg.STS = h.store
	m, err := irc.NewManager(icfg)
	if err != nil {
		return err
	}
	name := nc.EffectiveName()
	nctx, cancel := context.WithCancel(h.rootCtx)
	p := &netProc{cancel: cancel, done: make(chan struct{})}
	h.mu.Lock()
	h.procs[name] = p
	h.mu.Unlock()

	var pair sync.WaitGroup
	pair.Add(2)
	h.rootWG.Add(2)
	go func() {
		defer h.rootWG.Done()
		defer pair.Done()
		m.Run(nctx)
	}()
	go func() {
		defer h.rootWG.Done()
		defer pair.Done()
		h.Run(nctx, m)
	}()
	go func() {
		pair.Wait()
		close(p.done)
	}()
	return nil
}

// StopNetwork tears down a running network's connection and waits for
// its goroutines to exit, then forgets its ephemeral state. A no-op for
// names that are not running.
func (h *Hub) StopNetwork(name string) {
	h.mu.Lock()
	p := h.procs[name]
	delete(h.procs, name)
	h.mu.Unlock()
	if p == nil {
		return
	}
	p.cancel()
	<-p.done
	h.mu.Lock()
	delete(h.states, name)
	delete(h.presence, name)
	delete(h.motdWanted, name)
	h.mu.Unlock()
}

// ---- session request handlers ----

func (s *Session) handleGetNetworks(ctx context.Context, env Envelope) {
	configs, err := s.hub.store.NetworkConfigs(ctx)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "loading networks failed"))
		return
	}
	s.hub.mu.Lock()
	states := make(map[string]string, len(s.hub.states))
	for k, v := range s.hub.states {
		states[k] = v
	}
	s.hub.mu.Unlock()
	out := make([]NetworkConfigData, 0, len(configs))
	for _, nc := range configs {
		out = append(out, NetworkConfigData{
			Name:   nc.Name,
			State:  states[nc.Name],
			Config: json.RawMessage(nc.Config),
		})
	}
	s.push(envelope("networks", env.Seq, NetworksData{Networks: out}))
}

func (s *Session) handlePutNetwork(ctx context.Context, env Envelope) {
	var d PutNetworkReq
	if err := json.Unmarshal(env.Data, &d); err != nil || len(d.Config) == 0 {
		s.push(errEnvelope(env.Seq, "bad_request", "put_network needs a config"))
		return
	}
	nc, err := netconf.Parse(d.Config)
	if err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", err.Error()))
		return
	}
	// Full validation up front — the complete irc.Config checks
	// (plaintext opt-in, SASL, fingerprints, proxy) plus client
	// certificate loading — so an invalid edit is rejected while the
	// existing definition and its connection are still untouched.
	if err := ValidateNetwork(nc); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", err.Error()))
		return
	}
	name := nc.EffectiveName()

	// Serialize start/stop against other network operations.
	s.hub.netOps.Lock()
	defer s.hub.netOps.Unlock()

	stored, err := s.hub.store.NetworkConfigs(ctx)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "loading networks failed"))
		return
	}
	if msg := putNetworkConflict(stored, d.OldName, name); msg != "" {
		s.push(errEnvelope(env.Seq, "bad_request", msg))
		return
	}
	canonical, err := json.Marshal(nc)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "encoding network failed"))
		return
	}

	renamed := d.OldName != "" && d.OldName != name
	prev := findConfig(stored, d.OldName, name)
	if err := s.hub.applyPutNetwork(ctx, nc, d.OldName, prev, renamed, string(canonical)); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", err.Error()))
		return
	}
	if renamed {
		s.hub.broadcast(envelope("network_removed", 0, NetworkRef{Network: d.OldName}))
	}
	s.push(envelope("ok", env.Seq, nil))
	s.hub.broadcast(envelope("networks_changed", 0, nil))
}

// applyPutNetwork performs the state change of a validated put: stop the
// old connection, atomically replace the stored definition, start the
// new one. Both failure points roll back so an error always leaves the
// previous definition stored and connected. Caller holds netOps.
func (h *Hub) applyPutNetwork(ctx context.Context, nc *netconf.Network, oldName string, prev *store.NetworkConfig, renamed bool, canonical string) error {
	name := nc.EffectiveName()
	// Stop before persisting: a rename moves history, and the running
	// connection must not append rows under the old name mid-move.
	if oldName != "" {
		h.StopNetwork(oldName)
	}
	h.StopNetwork(name)
	oldForReplace := ""
	if renamed {
		oldForReplace = oldName
	}
	if err := h.store.ReplaceNetworkConfig(ctx, oldForReplace, name, canonical); err != nil {
		// Atomic replace failed = nothing changed; put the previous
		// definition's connection back.
		h.restartNetwork(prev)
		return err
	}
	if err := h.StartNetwork(nc); err != nil {
		// Validated by the caller, so this is exceptional (e.g. a
		// certificate file changed on disk since). Restore the previous
		// definition.
		h.rollbackPut(ctx, name, prev)
		return err
	}
	return nil
}

// rollbackPut undoes a persisted-but-unstartable put: an edit restores
// (and restarts) the previous definition, an add is deleted.
func (h *Hub) rollbackPut(ctx context.Context, name string, prev *store.NetworkConfig) {
	if prev != nil {
		if err := h.store.ReplaceNetworkConfig(ctx, name, prev.Name, prev.Config); err != nil {
			log.Printf("network %q: rollback failed: %v", name, err)
		}
		h.restartNetwork(prev)
		return
	}
	if err := h.store.DeleteNetwork(ctx, name); err != nil {
		log.Printf("network %q: rollback failed: %v", name, err)
	}
}

// ValidateNetwork runs the full connect-time validation without
// connecting: certificate files load and a manager constructs, or the
// definition is rejected. Used by put_network and by main for
// config-file seeds — both persistence paths validate identically.
func ValidateNetwork(nc *netconf.Network) error {
	icfg, err := nc.IRCConfig()
	if err != nil {
		return err
	}
	_, err = irc.NewManager(icfg)
	return err
}

// findConfig returns the stored definition a put would displace (the
// old name when editing, the target name otherwise), nil for an add.
func findConfig(stored []store.NetworkConfig, oldName, name string) *store.NetworkConfig {
	want := oldName
	if want == "" {
		want = name
	}
	for i := range stored {
		if stored[i].Name == want {
			return &stored[i]
		}
	}
	return nil
}

// restartNetwork best-effort restarts a stored definition after a
// failed put, so an error leaves the previous network running.
func (h *Hub) restartNetwork(prev *store.NetworkConfig) {
	if prev == nil {
		return
	}
	nc, err := netconf.Parse([]byte(prev.Config))
	if err == nil {
		err = h.StartNetwork(nc)
	}
	if err != nil {
		log.Printf("network %q: restart after failed edit: %v", prev.Name, err)
	}
}

// putNetworkConflict reports (as a user-facing message, "" for none)
// name conflicts for an add or edit: an existing name may not be taken,
// and an edit must reference a definition that exists.
func putNetworkConflict(stored []store.NetworkConfig, oldName, name string) string {
	exists := func(n string) bool {
		for _, c := range stored {
			if c.Name == n {
				return true
			}
		}
		return false
	}
	renamed := oldName != "" && oldName != name
	if oldName != "" && !exists(oldName) {
		return fmt.Sprintf("network %q not found", oldName)
	}
	if (oldName == "" || renamed) && exists(name) {
		return fmt.Sprintf("network %q already exists", name)
	}
	return ""
}

func (s *Session) handleDeleteNetwork(ctx context.Context, env Envelope) {
	var d NetworkRef
	if err := json.Unmarshal(env.Data, &d); err != nil || d.Network == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "delete_network needs a network"))
		return
	}
	s.hub.netOps.Lock()
	defer s.hub.netOps.Unlock()

	// Capture the definition first: if the delete transaction fails
	// after the stop, the network must come back up, not stay stopped
	// with its definition intact.
	stored, err := s.hub.store.NetworkConfigs(ctx)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "loading networks failed"))
		return
	}
	prev := findConfig(stored, d.Network, d.Network)
	s.hub.StopNetwork(d.Network)
	// One transaction: the definition and the stored history (that is
	// what "delete" means) go together or not at all.
	if err := s.hub.store.DeleteNetwork(ctx, d.Network); err != nil {
		s.hub.restartNetwork(prev)
		s.push(errEnvelope(env.Seq, "internal", "deleting network failed"))
		return
	}
	log.Printf("network %q deleted", d.Network)
	s.push(envelope("ok", env.Seq, nil))
	s.hub.broadcast(envelope("network_removed", 0, NetworkRef{Network: d.Network}))
	s.hub.broadcast(envelope("networks_changed", 0, nil))
}

// handleJoinChannel joins a channel and adds it to the network's
// autojoin list; handlePartChannel is the inverse (the sidebar "Leave"
// action). Autojoin edits apply on the next connect — the live JOIN or
// PART takes effect immediately.
func (s *Session) handleJoinChannel(ctx context.Context, env Envelope, join bool) {
	var d ChannelRef
	if err := json.Unmarshal(env.Data, &d); err != nil || d.Network == "" || d.Channel == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "need a network and a channel"))
		return
	}
	if strings.ContainsAny(d.Channel, " ,\r\n") {
		s.push(errEnvelope(env.Seq, "bad_request", "invalid channel name"))
		return
	}
	c, ok := s.conn(env.Seq, d.Network)
	if !ok {
		return
	}
	if !c.IsChannel(d.Channel) {
		s.push(errEnvelope(env.Seq, "bad_request", "not a channel name on this network"))
		return
	}
	cmd := "JOIN"
	if !join {
		cmd = "PART"
	}
	if err := c.Send(&ircv4.Message{Command: cmd, Params: []string{d.Channel}}); err != nil {
		s.push(errEnvelope(env.Seq, "send_failed", err.Error()))
		return
	}
	if err := s.hub.updateAutojoin(ctx, d.Network, d.Channel, join, c.Fold); err != nil {
		log.Printf("network %q: updating autojoin for %s: %v", d.Network, d.Channel, err)
	}
	s.push(envelope("ok", env.Seq, nil))
	s.hub.broadcast(envelope("networks_changed", 0, nil))
}

// handleCloseBuffer deletes a buffer's stored history so it stays
// closed (the sidebar is store-driven; without this a closed buffer
// resurrects on the next refresh). Leaving a channel via the UI sends
// part_channel first, then this.
func (s *Session) handleCloseBuffer(ctx context.Context, env Envelope) {
	var d BufferRef
	if err := json.Unmarshal(env.Data, &d); err != nil || d.Network == "" || d.Buffer == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "close_buffer needs a network and a buffer"))
		return
	}
	if err := s.hub.store.DeleteBuffer(ctx, d.Network, d.Buffer); err != nil {
		s.push(errEnvelope(env.Seq, "internal", "closing buffer failed"))
		return
	}
	// Guard against straggler inbound traffic re-creating the buffer we
	// just deleted (see persistEvent). Fold with the network's
	// casemapping when it is connected; otherwise a plain key still
	// matches most cases.
	fold := func(x string) string { return x }
	if c := s.hub.network(d.Network); c != nil {
		fold = c.Fold
	}
	s.hub.markClosed(d.Network, fold(d.Buffer), time.Now().UnixMilli())
	s.push(envelope("ok", env.Seq, nil))
	s.hub.broadcast(envelope("buffer_closed", 0, d))
}

// knownNetwork reports whether name is a real network — a stored
// definition or a currently-connected one. Neither is forgeable by a
// client, so gating monitor operations on it prevents an arbitrary name
// from minting phantom network/monitor rows via create-on-demand.
func (h *Hub) knownNetwork(ctx context.Context, name string) (bool, error) {
	if h.network(name) != nil {
		return true, nil
	}
	configs, err := h.store.NetworkConfigs(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range configs {
		if c.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// updateAutojoin adds or removes a channel in a stored definition's
// channels list (case-insensitive dedup).
func (h *Hub) updateAutojoin(ctx context.Context, network, channel string, add bool, fold func(string) string) error {
	h.netOps.Lock()
	defer h.netOps.Unlock()

	configs, err := h.store.NetworkConfigs(ctx)
	if err != nil {
		return err
	}
	var raw string
	for _, nc := range configs {
		if nc.Name == network {
			raw = nc.Config
			break
		}
	}
	if raw == "" {
		return fmt.Errorf("no stored definition")
	}
	nc, err := netconf.Parse([]byte(raw))
	if err != nil {
		return err
	}
	out, changed := editChannelList(nc.Channels, channel, add, fold)
	if !changed {
		return nil
	}
	nc.Channels = out
	canonical, err := json.Marshal(nc)
	if err != nil {
		return err
	}
	return h.store.PutNetworkConfig(ctx, network, string(canonical))
}

// editChannelList adds or removes a channel (deduplicated under the
// network's casemapping), reporting whether the list changed.
func editChannelList(chans []string, channel string, add bool, fold func(string) string) ([]string, bool) {
	out := make([]string, 0, len(chans)+1)
	found := false
	for _, ch := range chans {
		if fold(ch) == fold(channel) {
			found = true
			if !add {
				continue
			}
		}
		out = append(out, ch)
	}
	if add {
		if found {
			return chans, false
		}
		return append(out, channel), true
	}
	return out, found
}

// SeedRows converts config-file networks into store rows for
// SeedNetworkConfigs (a main-startup helper).
func SeedRows(networks []netconf.Network) ([]store.NetworkConfig, error) {
	rows := make([]store.NetworkConfig, 0, len(networks))
	for _, n := range networks {
		j, err := json.Marshal(n)
		if err != nil {
			return nil, err
		}
		rows = append(rows, store.NetworkConfig{Name: n.EffectiveName(), Config: string(j)})
	}
	return rows, nil
}
