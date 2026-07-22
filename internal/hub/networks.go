// ircthing — a self-hosted, always-connected web IRC client.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	ircv4 "gopkg.in/irc.v4"

	"ircthing/internal/irc"
	"ircthing/internal/netconf"
	"ircthing/internal/proxydial"
	"ircthing/internal/store"
)

// Network lifecycle: networks are defined in the store
// (network_configs) and started/stopped at runtime — at boot by main,
// afterwards by the put_network / delete_network session requests. The
// hub owns the goroutine pair per network (manager read loop + event
// consumer), consistent with it owning all fan-out.

const errLoadNetworks = "loading networks failed"

type netProc struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when both goroutines have exited
	mgr    *irc.Manager  // for the media proxy's tunnel dial (WireGuard networks)
}

// NetworkTunnelDial dials addr through the named network's LIVE WireGuard
// tunnel, for the media proxy fetching a link/image preview for a link seen on
// that network. It returns an error when the network isn't running or its
// tunnel isn't up (the media path then refuses the preview) — it NEVER dials
// directly, so a WireGuard network's real IP is never leaked to a preview
// target. Safe for concurrent use; the manager reads its tunnel under a lock
// and never lets this path build or tear one down.
func (h *Hub) NetworkTunnelDial(ctx context.Context, name, addr string) (net.Conn, error) {
	h.mu.Lock()
	p := h.procs[name]
	h.mu.Unlock()
	if p == nil || p.mgr == nil {
		return nil, fmt.Errorf("hub: network %q not running", name)
	}
	return p.mgr.MediaDialContext(ctx, addr)
}

// UseRoot supplies the process-lifetime context and waitgroup network
// connections run under. Called once at startup, before StartNetwork.
func (h *Hub) UseRoot(ctx context.Context, wg *sync.WaitGroup) {
	h.rootCtx = ctx
	h.rootWG = wg
}

// NoteStoppedNetwork keeps a stored definition that could not be started
// visible in get_buffers. That gives the owner a sidebar/context-menu path to
// edit or delete malformed and legacy-oversized rows instead of making the
// recovery target disappear from the UI entirely.
func (h *Hub) NoteStoppedNetwork(name string) {
	h.mu.Lock()
	if _, exists := h.states[name]; !exists {
		h.states[name] = irc.StateDisconnected.String()
	}
	h.mu.Unlock()
}

func invalidNetworkLabel(id int64) string {
	return store.ReservedRecoveryNetworkPrefix + strconv.FormatInt(id, 10) + "__"
}

// NoteInvalidNetwork exposes a row whose local name violates current bounds as
// a synthetic stopped network. The synthetic label is reserved from normal
// configs and maps back to an opaque rowid, so the UI can delete the row
// without ever materializing its attacker-sized/control-bearing real name.
func (h *Hub) NoteInvalidNetwork(id int64) string {
	label := invalidNetworkLabel(id)
	h.mu.Lock()
	h.recoveryRows[label] = id
	h.states[label] = irc.StateDisconnected.String()
	h.mu.Unlock()
	return label
}

func (h *Hub) recoveryRowID(label string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.recoveryRows[label]
}

// StartNetwork builds a manager for the definition and runs it under the
// root context. The caller must have stopped any previous instance of
// the same name (see netOps serialization in the session handlers).
func (h *Hub) StartNetwork(nc *netconf.Network) error {
	icfg, err := nc.IRCConfig()
	if err != nil {
		return err
	}
	if proxydial.CredsOverCleartext(nc.Proxy) {
		ircCrypto := "the IRC connection runs TLS inside the tunnel, so IRC traffic stays encrypted end-to-end"
		if !nc.TLS {
			ircCrypto = "this network is PLAINTEXT (no TLS), so the proxy also sees the IRC traffic itself — enable TLS"
		}
		log.Printf("network %q: proxy credentials are sent UNENCRYPTED to a non-loopback host (SOCKS5/HTTP proxy auth is cleartext); only use this if the connection to the proxy is itself protected (VPN/SSH tunnel). %s.", nc.EffectiveName(), ircCrypto)
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
	p := &netProc{cancel: cancel, done: make(chan struct{}), mgr: m}
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
	if p != nil {
		p.cancel()
		<-p.done
	}
	h.mu.Lock()
	delete(h.states, name)
	delete(h.presence, name)
	delete(h.motdWanted, name)
	delete(h.recoveryRows, name)
	h.mu.Unlock()
}

// ---- session request handlers ----

func (s *Session) handleGetNetworks(ctx context.Context, env Envelope) {
	var req GetNetworksReq
	if len(env.Data) != 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, &req); err != nil || req.After < 0 {
			s.push(errEnvelope(env.Seq, "bad_request", "malformed get_networks data"))
			return
		}
	}
	select {
	case s.hub.largeResponseSem <- struct{}{}:
		defer func() { <-s.hub.largeResponseSem }()
	case <-ctx.Done():
		return
	}
	const pageRows = 16
	configs, storeMore, err := s.hub.store.NetworkConfigsPage(ctx, req.After, pageRows)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", errLoadNetworks))
		return
	}
	s.hub.mu.Lock()
	states := make(map[string]string, len(s.hub.states))
	for k, v := range s.hub.states {
		states[k] = v
	}
	s.hub.mu.Unlock()
	data := NetworksData{Networks: make([]NetworkConfigData, 0, len(configs))}
	for _, nc := range configs {
		stateName := nc.Name
		if nc.InvalidName {
			stateName = invalidNetworkLabel(nc.PageID)
		}
		item := makeNetworkConfigData(nc, states[stateName])
		prevNext := data.Next
		data.Networks = append(data.Networks, item)
		data.HasMore = true // worst-case final metadata during admission
		data.Next = nc.PageID
		encoded, _ := json.Marshal(data)
		if len(encoded) > maxHistoryPayloadBytes {
			data.Networks = data.Networks[:len(data.Networks)-1]
			data.Next = prevNext
			data.HasMore = true
			break
		}
	}
	data.HasMore = len(data.Networks) < len(configs) || storeMore
	if !data.HasMore {
		data.Next = 0
	}
	s.push(envelope("networks", env.Seq, data))
}

func makeNetworkConfigData(nc store.NetworkConfig, state string) NetworkConfigData {
	item := NetworkConfigData{Name: nc.Name, State: state, Oversized: nc.Oversized}
	if nc.InvalidName {
		item.Name = invalidNetworkLabel(nc.PageID)
		item.InvalidName = true
		item.Invalid = true
		item.RecoveryID = nc.PageID
		return item
	}
	if nc.Config == "" {
		item.Invalid = !nc.Oversized
	} else if json.Valid([]byte(nc.Config)) {
		item.Config = json.RawMessage(nc.Config)
	} else {
		item.Invalid = true
	}
	return item
}

func (s *Session) handleGetNetwork(ctx context.Context, env Envelope) {
	var req GetNetworkReq
	if err := json.Unmarshal(env.Data, &req); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "get_network needs a valid network name"))
		return
	}
	if id := s.hub.recoveryRowID(req.Network); id != 0 {
		nc := store.NetworkConfig{InvalidName: true, PageID: id}
		s.push(envelope("network", env.Seq, NetworkData{Network: makeNetworkConfigData(nc, irc.StateDisconnected.String())}))
		return
	}
	if !validStoredNetworkName(req.Network) {
		s.push(errEnvelope(env.Seq, "bad_request", "get_network needs a valid network name"))
		return
	}
	select {
	case s.hub.largeResponseSem <- struct{}{}:
		defer func() { <-s.hub.largeResponseSem }()
	case <-ctx.Done():
		return
	}
	nc, found, err := s.hub.store.NetworkConfig(ctx, req.Network)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", errLoadNetworks))
		return
	}
	if !found {
		s.push(errEnvelope(env.Seq, "unknown_network", "network not found"))
		return
	}
	s.hub.mu.Lock()
	state := s.hub.states[nc.Name]
	s.hub.mu.Unlock()
	s.push(envelope("network", env.Seq, NetworkData{Network: makeNetworkConfigData(nc, state)}))
}

func (s *Session) handlePutNetwork(ctx context.Context, env Envelope) {
	nc, d, ok := s.parsePutNetwork(env)
	if !ok {
		return
	}
	name := nc.EffectiveName()

	// Serialize start/stop against other network operations.
	s.hub.netOps.Lock()
	defer s.hub.netOps.Unlock()

	prev, renamed, ok := s.checkPutPreconditions(ctx, env, d, name)
	if !ok {
		return
	}
	canonical, err := json.Marshal(nc)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "encoding network failed"))
		return
	}
	if err := store.ValidateNetworkConfigRecord(name, string(canonical)); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", err.Error()))
		return
	}

	if err := s.hub.applyPutNetwork(ctx, nc, d.OldName, prev, renamed, string(canonical)); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", err.Error()))
		return
	}
	if renamed {
		s.hub.broadcast(envelope("network_removed", 0, NetworkRef{Network: d.OldName}))
		// Pending pushes carry the OLD network name in their keys and
		// payloads; cancel rather than deliver stale names.
		s.hub.notifyPushCancel(d.OldName, "", "")
	}
	s.push(envelope("ok", env.Seq, nil))
	s.hub.broadcast(envelope("networks_changed", 0, nil))
}

// parsePutNetwork unmarshals and fully validates a put_network request
// before any lock is taken, pushing the error reply itself when it fails.
func (s *Session) parsePutNetwork(env Envelope) (*netconf.Network, PutNetworkReq, bool) {
	var d PutNetworkReq
	if err := json.Unmarshal(env.Data, &d); err != nil || len(d.Config) == 0 {
		s.push(errEnvelope(env.Seq, "bad_request", "put_network needs a config"))
		return nil, d, false
	}
	nc, err := netconf.Parse(d.Config)
	if err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", err.Error()))
		return nil, d, false
	}
	// Full validation up front — the complete irc.Config checks
	// (plaintext opt-in, SASL, fingerprints, proxy) plus client
	// certificate loading — so an invalid edit is rejected while the
	// existing definition and its connection are still untouched.
	if err := ValidateNetwork(nc); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", err.Error()))
		return nil, d, false
	}
	// Invalid legacy names are represented to clients only by an opaque
	// synthetic recovery label. Never accept the raw spelling on the protocol:
	// current write invariants could not restore it after a failed edit.
	if d.OldName != "" && !validStoredNetworkName(d.OldName) {
		s.push(errEnvelope(env.Seq, "bad_request", "old network name is invalid; remove it through its recovery entry"))
		return nil, d, false
	}
	return nc, d, true
}

// checkPutPreconditions validates the stored state a put replaces — the
// previous row exists (for an edit), the target name is free (for an add or
// rename), and the previous row is replaceable — returning the previous
// definition to roll back to (nil for an add) and whether this is a rename.
// It pushes the error reply itself when a check fails. Caller holds netOps.
func (s *Session) checkPutPreconditions(ctx context.Context, env Envelope, d PutNetworkReq, name string) (prev *store.NetworkConfig, renamed bool, ok bool) {
	prevName := d.OldName
	if prevName == "" {
		prevName = name
	}
	prevRow, prevFound, err := s.hub.store.NetworkConfig(ctx, prevName)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", errLoadNetworks))
		return nil, false, false
	}
	renamed = d.OldName != "" && d.OldName != name
	if d.OldName != "" && !prevFound {
		s.push(errEnvelope(env.Seq, "bad_request", fmt.Sprintf("network %q not found", d.OldName)))
		return nil, false, false
	}
	if d.OldName == "" || renamed {
		if !s.putNameAvailable(ctx, env, name) {
			return nil, false, false
		}
	}
	if legacyNetworkRequiresRecreate(prevFound, prevRow, d.OldName != "") {
		// ReplaceNetworkConfig deliberately rejects an old >64 KiB blob or an
		// empty legacy value. A later StartNetwork failure could not roll either
		// one back atomically, so keep the contract honest: malformed legacy rows
		// are delete-and-recreate only.
		s.push(errEnvelope(env.Seq, "bad_request", "stored network config is invalid or exceeds the safe limit; remove and recreate it"))
		return nil, false, false
	}
	if prevFound && prevRow.Config != "" {
		copy := prevRow
		prev = &copy
	}
	return prev, renamed, true
}

// putNameAvailable reports whether no stored network already uses the
// target name, pushing the error reply when one does (or the lookup fails).
func (s *Session) putNameAvailable(ctx context.Context, env Envelope, name string) bool {
	_, targetFound, lerr := s.hub.store.NetworkConfig(ctx, name)
	if lerr != nil {
		s.push(errEnvelope(env.Seq, "internal", errLoadNetworks))
		return false
	}
	if targetFound {
		s.push(errEnvelope(env.Seq, "bad_request", fmt.Sprintf("network %q already exists", name)))
		return false
	}
	return true
}

func legacyNetworkRequiresRecreate(found bool, row store.NetworkConfig, explicitEdit bool) bool {
	return found && explicitEdit && (row.Oversized || row.Config == "")
}

func validStoredNetworkName(name string) bool {
	return store.ValidateNetworkConfigRecord(name, `{}`) == nil
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
	// A rename moves the networks row, so gate it against session
	// create-on-demand writes for the same reason as delete.
	h.lifecycleGate.Lock()
	err := h.store.ReplaceNetworkConfig(ctx, oldForReplace, name, canonical)
	h.lifecycleGate.Unlock()
	if err != nil {
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
	// These networks/network_configs writes must hold the lifecycleGate, like
	// every other one (put at :197, delete at :319): without it a session's
	// create-on-demand can interleave and resurrect the orphan the rollback
	// is undoing. The caller holds netOps, so the netOps->lifecycleGate order
	// matches those sites (no deadlock).
	if prev != nil {
		h.lifecycleGate.Lock()
		err := h.store.ReplaceNetworkConfig(ctx, name, prev.Name, prev.Config)
		h.lifecycleGate.Unlock()
		if err != nil {
			log.Printf("network %q: rollback failed: %v", name, err)
		}
		h.restartNetwork(prev)
		return
	}
	h.lifecycleGate.Lock()
	err := h.store.DeleteNetwork(ctx, name)
	h.lifecycleGate.Unlock()
	if err != nil {
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
		h.NoteStoppedNetwork(prev.Name)
	}
}

func (s *Session) handleDeleteNetwork(ctx context.Context, env Envelope) {
	var d NetworkRef
	if err := json.Unmarshal(env.Data, &d); err != nil || d.Network == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "delete_network needs a network"))
		return
	}
	s.hub.netOps.Lock()
	defer s.hub.netOps.Unlock()
	if recoveryID := s.hub.recoveryRowID(d.Network); recoveryID != 0 {
		// The real legacy name is intentionally never materialized. Delete its
		// definition+history by opaque rowid inside SQLite, and clear the
		// synthetic placeholder only after commit. On failure the mapping stays
		// live so the owner can retry.
		s.hub.lifecycleGate.Lock()
		err := s.hub.store.DeleteNetworkByPageID(ctx, recoveryID)
		s.hub.lifecycleGate.Unlock()
		if err != nil {
			s.push(errEnvelope(env.Seq, "internal", "deleting network failed"))
			return
		}
		s.hub.StopNetwork(d.Network)
		log.Printf("network recovery row %d deleted", recoveryID)
		s.push(envelope("ok", env.Seq, nil))
		s.hub.broadcast(envelope("network_removed", 0, NetworkRef{Network: d.Network}))
		s.hub.broadcast(envelope("networks_changed", 0, nil))
		s.hub.notifyPushCancel(d.Network, "", "")
		return
	}
	// A raw invalid legacy name must never bypass the synthetic row-ID path:
	// doing so would leave the recovery placeholder behind after deletion.
	if !validStoredNetworkName(d.Network) {
		s.push(errEnvelope(env.Seq, "bad_request", "network name is invalid; remove it through its recovery entry"))
		return
	}

	// Capture the definition first: if the delete transaction fails
	// after the stop, the network must come back up, not stay stopped
	// with its definition intact.
	prevRow, found, err := s.hub.store.NetworkConfig(ctx, d.Network)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", errLoadNetworks))
		return
	}
	var prev *store.NetworkConfig
	if found && prevRow.Config != "" {
		copy := prevRow
		prev = &copy
	}
	s.hub.StopNetwork(d.Network)
	// One transaction: the definition and the stored history (that is
	// what "delete" means) go together or not at all. Under the
	// lifecycle gate so no session create-on-demand write interleaves
	// and resurrects an orphan.
	s.hub.lifecycleGate.Lock()
	err = s.hub.store.DeleteNetwork(ctx, d.Network)
	s.hub.lifecycleGate.Unlock()
	if err != nil {
		s.hub.restartNetwork(prev)
		if found && prev == nil {
			s.hub.NoteStoppedNetwork(d.Network)
		}
		s.push(errEnvelope(env.Seq, "internal", "deleting network failed"))
		return
	}
	log.Printf("network %q deleted", d.Network)
	s.push(envelope("ok", env.Seq, nil))
	s.hub.broadcast(envelope("network_removed", 0, NetworkRef{Network: d.Network}))
	s.hub.broadcast(envelope("networks_changed", 0, nil))
	s.hub.notifyPushCancel(d.Network, "", "")
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
	// Length-clamp to the same bound the persisted path enforces
	// (maxPersistedChannelLen): Send validates only against the LIVE line
	// limit, which a server may raise via ISUPPORT LINELEN — so without this,
	// a >505-byte channel could be joined AND persisted here, then fail the
	// 512-byte registration-line validator on every restart (network skipped
	// until manually edited). Real CHANNELLEN is tens of bytes.
	if strings.ContainsAny(d.Channel, " ,\r\n") || len(d.Channel) > maxPersistedChannelLen {
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
	// A failed autojoin update is an ERROR, not a logged shrug: the client
	// gates the follow-up close_buffer (history delete) on this request
	// succeeding, and reporting success with the part not durably persisted
	// meant the channel could rejoin after a restart with its history gone.
	if err := s.hub.updateAutojoin(ctx, d.Network, []string{d.Channel}, join, c.Fold); err != nil {
		log.Printf("network %q: updating autojoin for %s: %v", d.Network, d.Channel, err)
		s.push(errEnvelope(env.Seq, "internal", "persisting the channel change failed"))
		return
	}
	s.push(envelope("ok", env.Seq, nil))
	s.hub.broadcast(envelope("networks_changed", 0, nil))
}

// handleCloseBuffer removes a buffer from every device's sidebar (the
// sidebar is store-driven; without a store change a closed buffer
// resurrects on the next refresh). With purge missing or true it deletes
// the stored history outright; with purge:false it archives the buffer —
// history kept, hidden from get_buffers, resurfacing with its scrollback
// on real new conversation. Leaving a channel via the UI sends
// part_channel first, then this.
func (s *Session) handleCloseBuffer(ctx context.Context, env Envelope) {
	var d BufferRef
	if err := json.Unmarshal(env.Data, &d); err != nil || d.Network == "" || d.Buffer == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "close_buffer needs a network and a buffer"))
		return
	}
	if !validCloseBuffer(d.Buffer) {
		s.push(errEnvelope(env.Seq, "bad_request", "invalid buffer name"))
		return
	}
	known, err := s.hub.knownNetwork(ctx, d.Network)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "checking network failed"))
		return
	}
	if !known {
		s.push(errEnvelope(env.Seq, "unknown_network", "unknown network"))
		return
	}
	// purge defaults to true: an older client's bare close_buffer keeps its
	// destructive delete semantics unchanged.
	purge := d.Purge == nil || *d.Purge
	// ok/buffer_closed payloads carry the RESOLVED boolean so every device
	// can tell a destructive purge from an archive. (&purge always
	// serializes — omitempty omits only a nil pointer, not *false.)
	d.Purge = &purge
	// Resolve/mutate (and for purge, install the close tombstone) under the
	// store lock, the same ordering used by guarded appends. No append can
	// land in a gap between those operations.
	fold := asciiFold
	if c := s.hub.network(d.Network); c != nil {
		fold = c.Fold
	}
	// Mutation+tombstone+publication is one observable mutation. Live append
	// paths hold the same mutex through their event publication, preventing an
	// older append from resurfacing the buffer after buffer_closed.
	s.hub.bufferMutationMu.Lock()
	defer s.hub.bufferMutationMu.Unlock()
	var canonical string
	if purge {
		canonical, err = s.hub.store.DeleteBufferFolded(ctx, d.Network, d.Buffer, fold, func(name string) {
			s.hub.markClosed(d.Network, fold(name), time.Now().UnixMilli())
		})
	} else {
		// Archive: history kept, no close tombstone. The straggler problem the
		// tombstone solves for deletes (our PART echo racing this request) is
		// handled by archived buffers swallowing membership fan-out without
		// publication (persistEvent's stillArchived check); a tombstone here
		// would instead DROP a live channel message that should resurface the
		// buffer with its history.
		canonical, err = s.hub.store.ArchiveBufferFolded(ctx, d.Network, d.Buffer, fold)
	}
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "closing buffer failed"))
		return
	}
	d.Buffer = canonical
	s.push(envelope("ok", env.Seq, d))
	s.hub.broadcast(envelope("buffer_closed", 0, d))
	// A pending push for a buffer the user just closed (either mode)
	// must not fire — its tap would navigate into a deleted or hidden
	// buffer.
	s.hub.notifyPushCancel(d.Network, canonical, "")
}

func validCloseBuffer(name string) bool {
	if name == "" || len(name) > 512 || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// knownNetwork reports whether name is a real network — a stored
// definition or a currently-connected one. Neither is forgeable by a
// client, so gating monitor operations on it prevents an arbitrary name
// from minting phantom network/monitor rows via create-on-demand.
func (h *Hub) knownNetwork(ctx context.Context, name string) (bool, error) {
	if h.network(name) != nil {
		return true, nil
	}
	_, found, err := h.store.NetworkConfig(ctx, name)
	return found, err
}

// maxPersistedChannels caps a stored definition's channel list, matching the
// manager's rejoin-set bound so a server forcing joins to endlessly many
// channels can't grow the config JSON without limit.
const maxPersistedChannels = 4096

// updateAutojoin adds or removes channels in a stored definition's channels
// list (case-insensitive dedup). Holds netOps — use ONLY from a goroutine that
// StopNetwork does not wait on (a session handler, not the Hub event loop; see
// persistAutojoin, which uses TryLock to avoid that deadlock).
func (h *Hub) updateAutojoin(ctx context.Context, network string, channels []string, add bool, fold func(string) string) error {
	h.netOps.Lock()
	defer h.netOps.Unlock()
	return h.updateAutojoinLocked(ctx, network, channels, add, fold)
}

// updateAutojoinLocked is the read-modify-write body; the caller holds netOps.
// It applies the WHOLE channels slice in one read+write (one store round-trip
// regardless of comma-list length), so a hostile JOIN/PART flood cannot drive
// one full-table read-modify-write per token.
func (h *Hub) updateAutojoinLocked(ctx context.Context, network string, channels []string, add bool, fold func(string) string) error {
	stored, found, err := h.store.NetworkConfig(ctx, network)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no stored definition")
	}
	if stored.Oversized {
		return fmt.Errorf("stored definition exceeds the safe size limit")
	}
	nc, err := netconf.Parse([]byte(stored.Config))
	if err != nil {
		return err
	}
	out, changed := editChannelList(nc.Channels, channels, add, fold)
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

// editChannelList adds or removes a set of channels (deduplicated under the
// network's casemapping), reporting whether the list changed. It folds each
// stored name and each wanted name exactly ONCE (into a set), so cost is
// O(stored + wanted) rather than O(stored × wanted) — a hostile comma-list of
// thousands of tokens can't turn one line into a fold storm.
func editChannelList(chans []string, wanted []string, add bool, fold func(string) string) ([]string, bool) {
	// Fold each wanted channel once; keep the first spelling seen for the add
	// path, and preserve wanted order for a deterministic stored config.
	wantFold := make(map[string]string, len(wanted))
	order := make([]string, 0, len(wanted))
	for _, w := range wanted {
		fw := fold(w)
		if _, ok := wantFold[fw]; !ok {
			wantFold[fw] = w
			order = append(order, fw)
		}
	}
	if !add {
		// Remove: keep stored channels whose fold is not in the wanted set.
		out := make([]string, 0, len(chans))
		changed := false
		for _, ch := range chans {
			if _, drop := wantFold[fold(ch)]; drop {
				changed = true
				continue
			}
			out = append(out, ch)
		}
		return out, changed
	}
	// Add: keep every stored channel, then append wanted names not already
	// present, in wanted order, up to the persisted-count cap.
	have := make(map[string]bool, len(chans))
	for _, ch := range chans {
		have[fold(ch)] = true
	}
	out := chans
	changed := false
	for _, fw := range order {
		if have[fw] || len(out) >= maxPersistedChannels {
			continue // already stored, or the list is at the cap; don't grow it
		}
		have[fw] = true
		out = append(out, wantFold[fw])
		changed = true
	}
	return out, changed
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
