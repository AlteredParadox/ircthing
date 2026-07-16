package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

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
	name := nc.EffectiveName()

	// Serialize start/stop against other network operations.
	s.hub.netOps.Lock()
	defer s.hub.netOps.Unlock()

	stored, err := s.hub.store.NetworkConfigs(ctx)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "loading networks failed"))
		return
	}
	exists := func(n string) bool {
		for _, c := range stored {
			if c.Name == n {
				return true
			}
		}
		return false
	}
	renamed := d.OldName != "" && d.OldName != name
	switch {
	case d.OldName == "" && exists(name):
		s.push(errEnvelope(env.Seq, "bad_request", fmt.Sprintf("network %q already exists", name)))
		return
	case d.OldName != "" && !exists(d.OldName):
		s.push(errEnvelope(env.Seq, "bad_request", fmt.Sprintf("network %q not found", d.OldName)))
		return
	case renamed && exists(name):
		s.push(errEnvelope(env.Seq, "bad_request", fmt.Sprintf("network %q already exists", name)))
		return
	}

	if renamed {
		if err := s.hub.store.RenameNetworkData(ctx, d.OldName, name); err != nil {
			s.push(errEnvelope(env.Seq, "bad_request", err.Error()))
			return
		}
		if err := s.hub.store.DeleteNetworkConfig(ctx, d.OldName); err != nil {
			s.push(errEnvelope(env.Seq, "internal", "renaming network failed"))
			return
		}
	}
	canonical, err := json.Marshal(nc)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "encoding network failed"))
		return
	}
	if err := s.hub.store.PutNetworkConfig(ctx, name, string(canonical)); err != nil {
		s.push(errEnvelope(env.Seq, "internal", "storing network failed"))
		return
	}

	// Editing reconnects: stop the old instance (under either name),
	// then start with the new definition.
	if d.OldName != "" {
		s.hub.StopNetwork(d.OldName)
	}
	s.hub.StopNetwork(name)
	if err := s.hub.StartNetwork(nc); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", err.Error()))
		return
	}
	if renamed {
		s.hub.broadcast(envelope("network_removed", 0, NetworkRef{Network: d.OldName}))
	}
	s.push(envelope("ok", env.Seq, nil))
	s.hub.broadcast(envelope("networks_changed", 0, nil))
}

func (s *Session) handleDeleteNetwork(ctx context.Context, env Envelope) {
	var d NetworkRef
	if err := json.Unmarshal(env.Data, &d); err != nil || d.Network == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "delete_network needs a network"))
		return
	}
	s.hub.netOps.Lock()
	defer s.hub.netOps.Unlock()

	s.hub.StopNetwork(d.Network)
	if err := s.hub.store.DeleteNetworkConfig(ctx, d.Network); err != nil {
		s.push(errEnvelope(env.Seq, "internal", "deleting network failed"))
		return
	}
	// Removing a network removes its history too — that is what
	// "delete" means; the definition alone is put_network's domain.
	if err := s.hub.store.DeleteNetworkData(ctx, d.Network); err != nil {
		s.push(errEnvelope(env.Seq, "internal", "deleting network data failed"))
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
	if err := s.hub.updateAutojoin(ctx, d.Network, d.Channel, join); err != nil {
		log.Printf("network %q: updating autojoin for %s: %v", d.Network, d.Channel, err)
	}
	s.push(envelope("ok", env.Seq, nil))
	s.hub.broadcast(envelope("networks_changed", 0, nil))
}

// updateAutojoin adds or removes a channel in a stored definition's
// channels list (case-insensitive dedup).
func (h *Hub) updateAutojoin(ctx context.Context, network, channel string, add bool) error {
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
	out := nc.Channels[:0]
	found := false
	for _, ch := range nc.Channels {
		if strings.EqualFold(ch, channel) {
			found = true
			if !add {
				continue
			}
		}
		out = append(out, ch)
	}
	if add && !found {
		out = append(out, channel)
	} else if add || !found {
		return nil // nothing to change
	}
	nc.Channels = out
	canonical, err := json.Marshal(nc)
	if err != nil {
		return err
	}
	return h.store.PutNetworkConfig(ctx, network, string(canonical))
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
