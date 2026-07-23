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
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ircthing/internal/irc"
	"ircthing/internal/store"

	ircv4 "gopkg.in/irc.v4"
)

// sessionBuffer is the per-session outbound queue depth. A session that
// falls this far behind is disconnected (see push).
var sessionBuffer = 64

const (
	defaultSessionQueueBytes  = int64(2 << 20) // 2 MiB per browser connection
	defaultHubQueueBytes      = int64(8 << 20) // 8 MiB across all connections
	maxHistoryPayloadBytes    = 512 << 10      // encoded history data, before envelope
	maxHistoryMessages        = 64             // also bounds pre-encoding materialization
	maxSearchQueryBytes       = 1024
	maxSearchTerms            = 32
	maxSearchMessages         = 64
	maxBufferResponseRows     = 512
	maxNetworkResponseRows    = 64
	maxChannelResponseMembers = 256
)

// OutboundFrame is a fully encoded WebSocket text frame and owns one
// queued-byte reservation. Encoding before admission is important: retaining
// an Envelope and then allocating another full JSON copy in the write pump
// could make the real queue footprint roughly twice its accounted budget.
// The transport must call Release exactly once after receiving it; Release is
// idempotent so concurrent session teardown can safely drain the queue.
type OutboundFrame struct {
	Data    []byte
	session *Session
	bytes   int64
	once    sync.Once
}

func (q *OutboundFrame) Release() {
	if q == nil || q.session == nil {
		return
	}
	q.once.Do(func() {
		q.session.queuedBytes.Add(-q.bytes)
		q.session.hub.queuedBytes.Add(-q.bytes)
	})
}

// Session is one connected client (one browser tab / device). The
// transport (internal/api) feeds client envelopes to Handle and writes
// everything from Outbound to the wire; when Done closes, the transport
// must drop the connection.
type Session struct {
	hub         *Hub
	out         chan *OutboundFrame
	done        chan struct{}
	once        sync.Once
	queuedBytes atomic.Int64
	queueMu     sync.Mutex
	closed      bool
}

// maxHubSessions bounds concurrent hub (WebSocket) sessions: each costs
// goroutines and an O(N) term in every broadcast, and one login token
// can open many connections. Generous for one user across devices/tabs.
const maxHubSessions = 64

// maxMonitorsPerNetwork bounds the persisted MONITOR buddy list. A reconnect
// reconciles the whole list as one atomic send; 1024 buddies is ~103 chunked
// MONITOR + messages (plus any removals) — comfortably under the manager's
// 512-message send queue even for a full replacement — while being far above
// any real buddy list.
const maxMonitorsPerNetwork = 1024

// NewSession registers a session, or returns nil when the session cap is
// reached (the caller rejects the upgrade). A nil session needs no
// Close.
func (h *Hub) NewSession() *Session {
	s := &Session{
		hub:  h,
		out:  make(chan *OutboundFrame, sessionBuffer),
		done: make(chan struct{}),
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sessions) >= maxHubSessions {
		return nil
	}
	h.sessions[s] = struct{}{}
	return s
}

// Close unregisters the session; the transport calls it exactly once when
// the connection ends.
func (s *Session) Close() {
	s.hub.mu.Lock()
	delete(s.hub.sessions, s)
	s.hub.mu.Unlock()
	s.disconnect()
}

func (s *Session) Outbound() <-chan *OutboundFrame { return s.out }

// Done closes when the session has been evicted (or Closed); the
// transport must stop writing and drop the connection.
func (s *Session) Done() <-chan struct{} { return s.done }

// push enqueues without blocking. A session too slow to drain its buffer
// is disconnected rather than stalling the hub or silently dropping
// events mid-stream — after a reconnect the client refetches history and
// misses nothing.
func (s *Session) push(env Envelope) {
	frame, err := json.Marshal(env)
	if err != nil {
		return // all envelopes are composed from JSON-safe application types
	}
	s.pushFrame(frame)
}

// pushFrame admits one immutable, fully encoded frame. Broadcasts share the
// same backing allocation between sessions, while accounting it
// conservatively once per session so the cap remains safe even if that sharing
// changes later.
func (s *Session) pushFrame(frame []byte) {
	n := int64(len(frame))
	if !reserveBytes(&s.queuedBytes, n, s.hub.sessionQueueBytes) {
		s.disconnect()
		return
	}
	if !reserveBytes(&s.hub.queuedBytes, n, s.hub.hubQueueBytes) {
		s.queuedBytes.Add(-n)
		s.disconnect()
		return
	}
	q := &OutboundFrame{Data: frame, session: s, bytes: n}
	s.queueMu.Lock()
	if s.closed {
		s.queueMu.Unlock()
		q.Release()
		return
	}
	full := false
	select {
	case s.out <- q:
	default:
		q.Release()
		full = true
	}
	s.queueMu.Unlock()
	if full {
		s.disconnect()
	}
}

func (s *Session) disconnect() {
	s.once.Do(func() {
		s.queueMu.Lock()
		defer s.queueMu.Unlock()
		s.closed = true
		close(s.done)
		// Release frames still owned by the queue. A transport may already own
		// one received frame; its independent idempotent Release handles that.
		for {
			select {
			case q := <-s.out:
				q.Release()
			default:
				return
			}
		}
	})
}

func reserveBytes(counter *atomic.Int64, n, limit int64) bool {
	if n <= 0 || n > limit {
		return false
	}
	for {
		old := counter.Load()
		if old > limit-n {
			return false
		}
		if counter.CompareAndSwap(old, old+n) {
			return true
		}
	}
}

// conn resolves a network name to its live connection, pushing the
// standard error (tagged with seq) when it is not connected.
func (s *Session) conn(seq int64, network string) (Conn, bool) {
	c := s.hub.network(network)
	if c == nil {
		s.push(errEnvelope(seq, "unknown_network", "network is not connected"))
		return nil, false
	}
	return c, true
}

// Handle processes one client envelope. Responses and errors are pushed
// to this session's Outbound, tagged with the request's Seq; side effects
// other sessions must see are broadcast.
func (s *Session) Handle(ctx context.Context, env Envelope) {
	if env.V != ProtocolVersion {
		return // unknown protocol version: ignore
	}
	switch env.Type {
	case "send":
		s.handleSend(ctx, env)
	case "command":
		s.handleCommand(ctx, env)
	case "redact":
		s.handleRedact(ctx, env)
	case "get_monitors":
		s.handleGetMonitors(ctx, env)
	case "monitor_add":
		s.handleMonitor(ctx, env, true)
	case "monitor_remove":
		s.handleMonitor(ctx, env, false)
	case "get_channel":
		s.handleGetChannel(ctx, env)
	case "get_buffers":
		s.handleGetBuffers(ctx, env)
	case "typing":
		s.handleTyping(ctx, env)
	case "get_history":
		s.handleGetHistory(ctx, env)
	case "search":
		s.handleSearch(ctx, env)
	case "get_read_marker":
		s.handleGetMarker(ctx, env)
	case "set_read_marker":
		s.handleSetMarker(ctx, env)
	case "get_prefs":
		s.handleGetPrefs(ctx, env)
	case "set_prefs":
		s.handleSetPrefs(ctx, env)
	case "get_rules":
		s.handleGetRules(ctx, env)
	case "set_rules":
		s.handleSetRules(ctx, env)
	case "get_filters":
		s.handleGetFilters(ctx, env)
	case "set_filters":
		s.handleSetFilters(ctx, env)
	case "get_networks":
		s.handleGetNetworks(ctx, env)
	case "get_network":
		s.handleGetNetwork(ctx, env)
	case "put_network":
		s.handlePutNetwork(ctx, env)
	case "delete_network":
		s.handleDeleteNetwork(ctx, env)
	case "join_channel":
		s.handleJoinChannel(ctx, env, true)
	case "part_channel":
		s.handleJoinChannel(ctx, env, false)
	case "close_buffer":
		s.handleCloseBuffer(ctx, env)
	default:
		// Unknown message types are ignored, not errored (protocol
		// forward-compatibility rule).
	}
}

func (s *Session) handleSend(ctx context.Context, env Envelope) {
	var d SendData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed send data"))
		return
	}
	if d.Network == "" || d.Target == "" || strings.TrimSpace(d.Text) == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "send needs network, target and text"))
		return
	}
	if isServerBuffer(d.Target) {
		// The synthetic server buffer is read-only; a PRIVMSG * would leak the
		// placeholder name onto the wire.
		s.push(errEnvelope(env.Seq, "bad_request", "the server buffer is read-only"))
		return
	}
	conn, ok := s.conn(env.Seq, d.Network)
	if !ok {
		return
	}
	// With echo-message negotiated the server reflects our messages and
	// the regular event path persists them (with server-time and msgid);
	// otherwise persist and broadcast ourselves so every device sees
	// what this one sent.
	echo := conn.CapEnabled("echo-message")

	if s.trySendMultiline(ctx, conn, env, d, echo) {
		return
	}

	// Fallback: one PRIVMSG per non-empty line, enqueued ATOMICALLY (all or
	// none). Per-line sends could fail midway — an oversized later line, a
	// filled queue — after earlier lines already went out; the composer still
	// holds the whole draft, so a retry would duplicate the delivered prefix.
	lines := nonEmptyLines(d.Text)
	msgs := make([]*ircv4.Message, len(lines))
	for i, line := range lines {
		msgs[i] = newPrivmsg(d.Target, line)
	}
	if err := conn.SendAll(msgs); err != nil {
		s.push(errEnvelope(env.Seq, "send_failed", err.Error()))
		return
	}
	if !echo {
		for _, line := range lines {
			s.persistOwn(ctx, conn, d.Network, d.Target, line)
		}
	}
	s.push(envelope("ok", env.Seq, nil))
}

// trySendMultiline sends a genuinely multi-line message as one
// draft/multiline batch (reconstructed into a single message on echo) and
// reports whether it handled the send — the reply envelope is already
// pushed either way when it did. The batch preserves interior blank lines —
// they are legal, meaningful content (a blank PRIVMSG line reconstructs to
// a blank line) — unlike the per-PRIVMSG fallback, where an empty PRIVMSG
// cannot be sent.
func (s *Session) trySendMultiline(ctx context.Context, conn Conn, env Envelope, d SendData, echo bool) bool {
	full := multilineLines(d.Text)
	if len(full) <= 1 || !conn.CapEnabled("draft/multiline") {
		return false
	}
	if err := conn.SendMultiline(d.Target, full); err != nil {
		s.push(errEnvelope(env.Seq, "send_failed", err.Error()))
		return true
	}
	if !echo {
		s.persistOwn(ctx, conn, d.Network, d.Target, strings.Join(full, "\n"))
	}
	s.push(envelope("ok", env.Seq, nil))
	return true
}

// multilineLines splits text for a draft/multiline batch, preserving
// interior blank lines and dropping only trailing empty lines (from
// trailing newlines).
func multilineLines(text string) []string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func nonEmptyLines(text string) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// persistOwn stores and broadcasts one of our own sent messages, used
// when echo-message is unavailable to reflect it.
func (s *Session) persistOwn(ctx context.Context, conn Conn, network, target, text string) {
	// Under the lifecycle gate: a concurrent delete_network must not be
	// interleaved, and a network deleted before this write is skipped so
	// it cannot resurrect an orphan buffer.
	s.hub.lifecycleGate.RLock()
	defer s.hub.lifecycleGate.RUnlock()
	if known, _ := s.hub.knownNetwork(ctx, network); !known {
		return
	}
	s.hub.bufferMutationMu.Lock()
	defer s.hub.bufferMutationMu.Unlock()
	// The target is client-supplied: file under the canonical stored
	// spelling (atomically, so "/msg #Go" cannot race the event
	// consumer into a second buffer next to #go). Strip a leading STATUSMSG
	// prefix ("/msg @#chan") the same way the echo/replay path does
	// (directTarget), so an own message files under the bare channel buffer
	// rather than a phantom "@#chan" one. The Raw line keeps the original
	// wire target, matching what the server would echo.
	msg := &ircv4.Message{Command: "PRIVMSG", Params: []string{target, text}}
	buffer := stripStatusPrefix(target, conn.StatusPrefixes(), conn.IsChannel)
	message := store.Message{
		Time:    time.Now(),
		Sender:  conn.Nick(),
		Command: "PRIVMSG",
		Raw:     msg.String(),
		// Normalize the same way the event path does (hub.go persistEvent
		// stores searchText(ev.Msg)): a CTCP ACTION ("/me") is unwrapped to
		// its bare text so this placeholder's indexed Text matches the
		// replayed value and AdoptOwnMsgID dedups it after chathistory
		// backfill on no-echo servers — storing the raw \x01ACTION…\x01
		// here would never match and leave a duplicate.
		Text: searchText(msg),
	}
	var (
		stored store.Message
		err    error
	)
	if conn.IsChannel(buffer) {
		// A send whose local echo loses a race with close_buffer must not
		// recreate a channel the user just closed. Query sends, on the other
		// hand, are explicit reopen intent and clear the tombstone below.
		stored, err = s.hub.store.AppendFoldedGuarded(ctx, network, buffer, conn.Fold,
			func(bool) bool { return s.hub.recentlyClosed(network, conn.Fold(buffer)) }, message)
	} else {
		s.hub.unmarkClosed(network, conn.Fold(buffer))
		stored, err = s.hub.store.AppendFolded(ctx, network, buffer, conn.Fold, message)
	}
	if err != nil {
		return
	}
	// AppendFolded returns a zero-value Message (ID 0) when the target buffer is
	// at the per-network cap and was dropped rather than created; broadcasting it
	// would push a blank event to the UI. Nothing was stored, so nothing to emit.
	if stored.ID == 0 {
		return
	}
	s.hub.broadcast(envelope("event", 0, eventData(stored)))
}

// handleTyping relays our typing state as a TAGMSG with the +typing
// client tag (https://ircv3.net/specs/client-tags/typing, fetched
// 2026-07-15). Best-effort: on a network without message-tags (the
// transport requirement) it is a silent no-op. Acked only when the client
// asked (Seq set) — typing is fire-and-forget.
func (s *Session) handleTyping(ctx context.Context, env Envelope) {
	var d TypingData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return
	}
	ok := d.Network != "" && d.Buffer != "" && !isServerBuffer(d.Buffer) &&
		(d.State == "active" || d.State == "paused" || d.State == "done")
	if ok {
		if conn := s.hub.network(d.Network); conn != nil && conn.CapEnabled("message-tags") {
			_ = conn.Send(&ircv4.Message{
				Tags:    ircv4.Tags{"+typing": d.State},
				Command: "TAGMSG",
				Params:  []string{d.Buffer},
			})
		}
	}
	if env.Seq != 0 {
		s.push(envelope("ok", env.Seq, nil))
	}
}

func (s *Session) handleGetMonitors(ctx context.Context, env Envelope) {
	var d struct {
		Network string `json:"network"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil || d.Network == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "get_monitors needs a network"))
		return
	}
	list, err := s.hub.monitorList(ctx, d.Network)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "monitor list query failed"))
		return
	}
	if list == nil {
		list = []MonitorEntry{}
	}
	s.push(envelope("monitors", env.Seq, MonitorsData{Network: d.Network, Monitors: list}))
}

// handleMonitor adds or removes a monitored nick: it persists the change
// and tells the connection to update its MONITOR list, which prompts the
// server for the nick's current presence.
func (s *Session) handleMonitor(ctx context.Context, env Envelope, add bool) {
	d, ok := s.parseMonitorReq(env, add)
	if !ok {
		return
	}
	// Only a real (configured or connected) network may accrue monitors:
	// AddMonitor creates the networks row on demand, so an unvalidated
	// name would otherwise mint phantom network/monitor rows. The
	// check-and-add runs under the lifecycle gate so a concurrent
	// delete_network cannot slip between them (TOCTOU).
	s.hub.lifecycleGate.RLock()
	defer s.hub.lifecycleGate.RUnlock()
	if ok, err := s.hub.knownNetwork(ctx, d.Network); err != nil {
		s.push(errEnvelope(env.Seq, "internal", "network lookup failed"))
		return
	} else if !ok {
		s.push(errEnvelope(env.Seq, "bad_request", "unknown network"))
		return
	}
	conn := s.hub.network(d.Network)
	fold := monitorFold(conn)

	// The WHOLE mutation — store write AND the wire send — runs under monMu,
	// the same lock startMonitor's restoration (snapshot + MONITOR C + list)
	// holds. Unserialized, a mutation could interleave with a reconnect's
	// restoration ("+bob, C, +stale-list"), leaving SQLite and the server's
	// monitor set disagreeing until the next reconnect.
	s.hub.monMu.Lock()
	defer s.hub.monMu.Unlock()

	nick := d.Nick // the spelling used for store/presence/wire (resolved for removes)
	if add {
		if !s.addMonitorLocked(ctx, env, d, fold) {
			return
		}
	} else if resolved, removed := s.removeMonitorLocked(ctx, env, d, fold); !removed {
		return
	} else {
		nick = resolved
	}
	if conn != nil {
		s.reconcileMonitorsLocked(ctx, d.Network, conn)
		if !add {
			s.hub.broadcast(envelope("presence", 0, PresenceData{Network: d.Network, Nick: nick, Online: false}))
		}
	}
	// Tell every session (other tabs/devices included) the authoritative list
	// changed, so their buddy lists refetch instead of drifting on local
	// optimistic state.
	s.hub.broadcast(envelope("monitors_changed", 0, MonitorsChangedData{Network: d.Network}))
	s.push(envelope("ok", env.Seq, nil))
}

// parseMonitorReq unmarshals and validates a monitor_add/monitor_remove
// request, pushing the error reply itself when it fails.
func (s *Session) parseMonitorReq(env Envelope, add bool) (MonitorReq, bool) {
	var d MonitorReq
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed monitor data"))
		return d, false
	}
	if d.Network == "" || d.Nick == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "monitor needs a network and a nick"))
		return d, false
	}
	// ValidMonitorTarget also rejects NUL and excessive length — an invalid
	// value must not be PERSISTED, or it would poison its ten-nick MONITOR
	// chunk on every reconnect (the live send would reject it anyway). Gate
	// ADDS only: a REMOVE of an invalid nick must still delete the row —
	// rows persisted by older, laxer versions are filtered from IRC
	// restoration but would otherwise be stuck visible forever, removable
	// only by editing SQLite. The wire MONITOR - is skipped for them.
	if add && !irc.ValidMonitorTarget(d.Nick) {
		s.push(errEnvelope(env.Seq, "bad_request", "monitor needs a network and a valid nick"))
		return d, false
	}
	return d, true
}

// monitorFold picks the casemapping for monitor-list comparisons: the live
// connection's CASEMAPPING when connected, ASCII-only lowercase otherwise.
func monitorFold(conn Conn) func(string) string {
	if conn == nil {
		return asciiFold
	}
	return conn.Fold
}

// addMonitorLocked persists one new monitored nick, pushing the reply
// envelope itself on every failure (and on the idempotent already-monitored
// no-op). It reports whether the list actually changed, so the caller knows
// to reconcile and broadcast. Caller holds monMu and the lifecycle gate.
func (s *Session) addMonitorLocked(ctx context.Context, env Envelope, d MonitorReq, fold func(string) string) bool {
	// Reject a casemapping-equivalent duplicate (Alice vs alice): SQLite
	// uniqueness is byte-exact, but the server folds both to ONE monitor —
	// presence keyed by fold would collapse them while the list renders
	// both spellings, one stuck perpetually offline. Fold with the live
	// connection's CASEMAPPING when connected, ASCII-only lowercase
	// otherwise (covers the common conflict; rfc1459's []{}|~ extras need
	// the server's mapping, and any residual pair is removable). Under
	// monMu, two concurrent adds can't both pass and both persist.
	existing, lerr := s.hub.store.Monitors(ctx, d.Network)
	if lerr != nil {
		// FAIL CLOSED: without the list we can enforce neither the
		// duplicate check nor the cap, so don't blindly persist.
		s.push(errEnvelope(env.Seq, "internal", "monitor list unavailable"))
		return false
	}
	// Reject a casemapping-equivalent duplicate (Alice vs alice) — but check
	// existence BEFORE the cap so an already-monitored nick is an idempotent
	// no-op even at capacity, not a spurious "list full". An EXACT-spelling
	// duplicate is likewise a no-op.
	for _, nk := range existing {
		if nk == d.Nick {
			s.push(envelope("ok", env.Seq, nil)) // already monitored, idempotent
			return false
		}
		if fold(nk) == fold(d.Nick) {
			s.push(errEnvelope(env.Seq, "bad_request", "already monitored as "+nk))
			return false
		}
	}
	// Cap the persisted list. A reconnect reconciles the WHOLE list as one
	// atomic delta (up to a full remove+re-add), so an unbounded list could
	// eventually exceed the send queue and become permanently unreconcilable
	// on a server that advertises no limit. maxMonitorsPerNetwork keeps even
	// a complete replacement well under the queue; far above any real list.
	if len(existing) >= maxMonitorsPerNetwork {
		s.push(errEnvelope(env.Seq, "bad_request", "monitor list is full"))
		return false
	}
	if err := s.hub.store.AddMonitor(ctx, d.Network, d.Nick); err != nil {
		s.push(errEnvelope(env.Seq, "internal", "updating the monitor list failed"))
		return false
	}
	return true
}

// removeMonitorLocked deletes one monitored nick, returning the STORED
// spelling it removed and whether it succeeded; it pushes the error reply
// itself on failure. Caller holds monMu and the lifecycle gate.
func (s *Session) removeMonitorLocked(ctx context.Context, env Envelope, d MonitorReq, fold func(string) string) (string, bool) {
	// Resolve the STORED spelling by fold before deleting: the store is
	// byte-exact, so removing "alice" when the row says "Alice" would
	// delete nothing — yet the wire MONITOR - (casemapped server-side)
	// would still stop upstream monitoring, leaving a row (and cached
	// presence) for a nick the server no longer watches. FAIL CLOSED on a
	// read failure (as the add path does): without the list we can't
	// resolve the stored spelling, so don't report success for a delete
	// that may have removed nothing.
	list, lerr := s.hub.store.Monitors(ctx, d.Network)
	if lerr != nil {
		s.push(errEnvelope(env.Seq, "internal", "monitor list unavailable"))
		return "", false
	}
	nick := d.Nick
	for _, nk := range list {
		if nk == d.Nick || fold(nk) == fold(d.Nick) {
			nick = nk
			if nk == d.Nick {
				break // exact match wins over a fold match
			}
		}
	}
	if err := s.hub.store.RemoveMonitor(ctx, d.Network, nick); err != nil {
		s.push(errEnvelope(env.Seq, "internal", "updating the monitor list failed"))
		return "", false
	}
	// Drop the removed nick's cached presence: a stale entry would be
	// served straight back by get_monitors if the nick is re-added
	// before a fresh 730/731 arrives (or while disconnected).
	s.hub.removePresence(d.Network, nick)
	return nick, true
}

// reconcileMonitorsLocked pushes the authoritative persisted list to the
// live connection. Reconcile the whole list, not just the mutated nick: it
// drives the exact delta atomically (so a full queue never records a
// half-sent list) and PROMOTES an overflow buddy into the slot a removal
// frees. Caller holds monMu; conn is non-nil.
func (s *Session) reconcileMonitorsLocked(ctx context.Context, network string, conn Conn) {
	if desired, lerr := s.hub.store.Monitors(ctx, network); lerr == nil {
		if rerr := conn.ReconcileMonitored(desired); rerr != nil {
			log.Printf("network %q: monitor reconcile: %v", network, rerr)
		}
	} else {
		// The mutation is persisted but couldn't be reconciled to the
		// server this pass; log it (recovered on the next mutation or
		// reconnect) rather than dropping it silently.
		log.Printf("network %q: monitor reconcile skipped (list read failed): %v", network, lerr)
	}
}

// MonitorsChangedData announces that a network's persisted monitor list
// changed; clients refetch via get_monitors.
type MonitorsChangedData struct {
	Network string `json:"network"`
}

// asciiFold lowercases A–Z only — the monitor-duplicate fallback when no
// connection (hence no authoritative CASEMAPPING) exists. strings.ToLower is
// the wrong tool here: it is Unicode-aware, so it both misses rfc1459's
// []{}|~ equivalences AND folds non-ASCII codepoints (İ→i̇, Σ→σ) that IRC
// servers treat as distinct bytes — rejecting legitimately different nicks.
func asciiFold(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// handleRedact issues a REDACT for one of our messages
// (draft/message-redaction). The server decides authorization; on success
// it echoes the REDACT back, which applyRedaction then stores and
// broadcasts. A no-op on networks without the cap.
func (s *Session) handleRedact(ctx context.Context, env Envelope) {
	var d RedactReq
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed redact data"))
		return
	}
	if d.Network == "" || d.Buffer == "" || d.MsgID == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "redact needs network, buffer and msgid"))
		return
	}
	conn, ok := s.conn(env.Seq, d.Network)
	if !ok {
		return
	}
	if !conn.CapEnabled("draft/message-redaction") {
		s.push(errEnvelope(env.Seq, "unsupported", "server does not support message redaction"))
		return
	}
	params := []string{d.Buffer, d.MsgID}
	if d.Reason != "" {
		params = append(params, d.Reason)
	}
	if err := conn.Send(&ircv4.Message{Command: "REDACT", Params: params}); err != nil {
		s.push(errEnvelope(env.Seq, "send_failed", err.Error()))
		return
	}
	s.push(envelope("ok", env.Seq, nil))
}

// commandSpec is the allowlist of client-issued IRC commands with their
// permitted parameter counts. Everything else is rejected — the client
// does not get raw connection access. Replies to the informational
// commands flow back as "server_info" pushes (see serverInfo).
var commandSpec = map[string]struct{ min, max int }{
	"JOIN":   {1, 2}, // channel [key]
	"PART":   {1, 2}, // channel [reason]
	"NICK":   {1, 1},
	"TOPIC":  {1, 2}, // channel [new topic]
	"WHOIS":  {1, 2}, // [server] nick
	"WHOWAS": {1, 2}, // nick [count]
	"WHO":    {1, 2}, // mask [flags]
	"LIST":   {0, 2}, // [channels [server]]
	"MODE":   {1, 6}, // target [modes [args...]]
	"KICK":   {2, 3}, // channel nick [reason]
	"INVITE": {2, 2}, // nick channel
	"AWAY":   {0, 1}, // [message]; none marks us back
	"NOTICE": {2, 2}, // target text
	"MOTD":   {0, 1}, // [server]
}

func (s *Session) handleCommand(ctx context.Context, env Envelope) {
	var d CommandData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed command data"))
		return
	}
	cmd := strings.ToUpper(d.Command)
	spec, allowed := commandSpec[cmd]
	if !allowed {
		s.push(errEnvelope(env.Seq, "bad_request", "command not allowed: "+d.Command))
		return
	}
	if len(d.Params) < spec.min || len(d.Params) > spec.max {
		s.push(errEnvelope(env.Seq, "bad_request", "wrong number of parameters"))
		return
	}
	if !validCommandParams(d.Params) {
		s.push(errEnvelope(env.Seq, "bad_request", "invalid parameter"))
		return
	}
	conn, ok := s.conn(env.Seq, d.Network)
	if !ok {
		return
	}
	// MOTD replies also arrive unsolicited at every (re)connect; only an
	// explicit /motd opens the gate for forwarding them (see serverInfo).
	// Arm the gate BEFORE the command is queued — a fast server can reply
	// before a gate armed afterwards opens, leaving a stale gate that would
	// expose the next unsolicited MOTD instead — and roll it back if the
	// send fails.
	motdTarget := ""
	if cmd == "MOTD" {
		if len(d.Params) > 0 {
			motdTarget = d.Params[0] // targeted "MOTD <server>": lets a 402 correlate
		}
		s.hub.expectMOTD(d.Network, motdTarget)
	}
	if err := conn.Send(&ircv4.Message{Command: cmd, Params: d.Params}); err != nil {
		if cmd == "MOTD" {
			s.hub.retractMOTD(d.Network, motdTarget)
		}
		s.push(errEnvelope(env.Seq, "send_failed", err.Error()))
		return
	}
	s.push(envelope("ok", env.Seq, nil))
}

// validCommandParams reports whether every parameter is safe to place on
// the wire: no empties, no CR/LF/NUL injection.
func validCommandParams(params []string) bool {
	for i, p := range params {
		if p == "" || strings.ContainsAny(p, "\r\n\x00") {
			return false
		}
		// Only the final parameter may contain spaces or lead with ':'
		// (it is sent as the trailing parameter).
		if i < len(params)-1 && (strings.Contains(p, " ") || strings.HasPrefix(p, ":")) {
			return false
		}
	}
	return true
}

func (s *Session) handleGetChannel(ctx context.Context, env Envelope) {
	var d ChannelReq
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed get_channel data"))
		return
	}
	select {
	case s.hub.largeResponseSem <- struct{}{}:
		defer func() { <-s.hub.largeResponseSem }()
	case <-ctx.Done():
		return
	}
	s.push(envelope("channel", env.Seq, s.buildChannelData(d)))
}

// buildChannelData assembles the get_channel response: topic, joined state,
// and a byte-capped member page (with a next-page cursor when the roster
// source supports paging).
func (s *Session) buildChannelData(d ChannelReq) ChannelData {
	data := ChannelData{Network: d.Network, Buffer: d.Buffer, Members: []MemberData{}}
	conn := s.hub.network(d.Network)
	if conn == nil {
		return data
	}
	// Under no-implicit-names the roster is empty until we ask; this
	// fetches lazily the first time a channel is viewed. The 366 reply
	// raises members_changed and the client refetches.
	conn.EnsureNames(d.Buffer)
	topic, members, truncated, paged, ok := channelRoster(conn, d.Buffer, d.After)
	if !ok {
		return data
	}
	data.Joined = true
	data.Topic = topic
	data.Truncated = truncated
	fillChannelMembers(&data, members, paged)
	// Next-page cursor: the folded key of the last member actually
	// sent (Truncated covers both "roster has more" and the byte-cap
	// break in fillChannelMembers). Stored nicks are already clamped, so
	// Fold(nick) equals the roster's map key — and computing it from
	// data.Members rather than the roster page means a byte-capped page
	// yields the correspondingly earlier cursor. Zero members sent leaves no
	// cursor; a cursor-aware client treats that as a degraded stop.
	if paged && data.Truncated {
		if n := len(data.Members); n > 0 {
			data.MembersAfter = conn.Fold(data.Members[n-1].Nick)
		}
	}
	return data
}

// channelRoster fetches one page of a channel's roster. paged reports a
// cursor-capable source: safe to hand out MembersAfter.
func channelRoster(conn Conn, buffer, after string) (topic string, members []irc.Member, truncated, paged, ok bool) {
	if pager, supportsPage := conn.(interface {
		ChannelPage(string, int, string) (string, []irc.Member, bool, bool)
	}); supportsPage {
		topic, members, truncated, ok = pager.ChannelPage(buffer, maxChannelResponseMembers, after)
		return topic, members, truncated, true, ok
	}
	// Test/alternate connections implement the original interface. Cap
	// their snapshot immediately after the call; production Managers use
	// ChannelPage and never copy the full hostile roster here.
	topic, members, ok = conn.Channel(buffer)
	if len(members) > maxChannelResponseMembers {
		members = members[:maxChannelResponseMembers]
		truncated = true
	}
	return topic, members, truncated, false, ok
}

// fillChannelMembers appends members to the response under the encoded
// payload cap, setting Truncated when the cap cuts the page short.
func fillChannelMembers(data *ChannelData, members []irc.Member, paged bool) {
	base, _ := json.Marshal(*data)
	used := len(base)
	if paged {
		// Reserve room for the members_after cursor the caller appends:
		// a folded, clamped nick — ≤512 bytes (the irc package's
		// maxRosterField) at worst all JSON-escaped to 6 bytes each —
		// plus field syntax. Reserving up front means appending the
		// cursor can never push the payload past the byte cap this
		// loop enforces (~3 KB of headroom against a 512 KB cap).
		used += 512*6 + len(`,"members_after":""`)
	}
	for _, m := range members {
		md := MemberData{
			Nick: m.Nick, Prefix: m.Prefix, Away: m.Away,
			Account: m.Account, Bot: m.Bot, User: m.User, Host: m.Host,
		}
		encoded, _ := json.Marshal(md)
		extra := len(encoded)
		if len(data.Members) > 0 {
			extra++
		}
		if used+extra > maxHistoryPayloadBytes {
			data.Truncated = true
			break
		}
		used += extra
		data.Members = append(data.Members, md)
	}
}

func (s *Session) handleGetBuffers(ctx context.Context, env Envelope) {
	select {
	case s.hub.largeResponseSem <- struct{}{}:
		defer func() { <-s.hub.largeResponseSem }()
	case <-ctx.Done():
		return
	}
	infos, storeMore, err := s.hub.store.RecentBuffers(ctx, maxBufferResponseRows)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "buffer list query failed"))
		return
	}
	data := BuffersData{
		Networks:  make([]NetworkInfo, 0, 4),
		Buffers:   make([]BufferInfo, 0, len(infos)),
		Truncated: storeMore,
	}
	s.hub.mu.Lock()
	names := s.hub.orderedNetworkNamesLocked(infos)
	if len(names) > maxNetworkResponseRows {
		names = names[:maxNetworkResponseRows]
		data.Truncated = true
	}
	base, _ := json.Marshal(data)
	used := len(base)
	includedNetworks, used := s.hub.fillBufferNetworksLocked(&data, names, used)
	s.hub.mu.Unlock()
	fillBufferRows(&data, infos, includedNetworks, used)
	s.push(envelope("buffers", env.Seq, data))
}

// orderedNetworkNamesLocked lists the network names for a buffers response.
// Prefer networks represented by the recent buffer page, then fill with
// the remaining configured/running names (alphabetically). This avoids
// spending the buffer row budget on networks that an alphabetical network
// cap would omit. Caller holds h.mu.
func (h *Hub) orderedNetworkNamesLocked(infos []store.BufferInfo) []string {
	names := make([]string, 0, len(h.states))
	for name := range h.states {
		names = append(names, name)
	}
	sort.Strings(names)
	ordered := make([]string, 0, len(names))
	seenNames := make(map[string]struct{}, len(names))
	for _, b := range infos {
		if _, exists := h.states[b.Network]; !exists {
			continue
		}
		if _, seen := seenNames[b.Network]; seen {
			continue
		}
		seenNames[b.Network] = struct{}{}
		ordered = append(ordered, b.Network)
	}
	for _, name := range names {
		if _, seen := seenNames[name]; seen {
			continue
		}
		seenNames[name] = struct{}{}
		ordered = append(ordered, name)
	}
	return ordered
}

// fillBufferNetworksLocked appends per-network info rows under the encoded
// payload cap, returning the set of networks actually included and the
// updated byte tally. Caller holds h.mu.
func (h *Hub) fillBufferNetworksLocked(data *BuffersData, names []string, used int) (map[string]struct{}, int) {
	includedNetworks := make(map[string]struct{}, len(names))
	for _, name := range names {
		info := NetworkInfo{Name: name, State: h.states[name]}
		if c := h.networks[name]; c != nil {
			info.Nick = c.Nick()
			info.ChanTypes = c.ChanTypes()
		}
		encoded, _ := json.Marshal(info)
		extra := len(encoded)
		if len(data.Networks) > 0 {
			extra++
		}
		if used+extra > maxHistoryPayloadBytes {
			data.Truncated = true
			break
		}
		used += extra
		data.Networks = append(data.Networks, info)
		includedNetworks[name] = struct{}{}
	}
	return includedNetworks, used
}

// fillBufferRows appends buffer rows (for included networks only) under the
// same encoded payload cap the network rows already consumed from.
func fillBufferRows(data *BuffersData, infos []store.BufferInfo, includedNetworks map[string]struct{}, used int) {
	for _, b := range infos {
		if _, ok := includedNetworks[b.Network]; !ok {
			data.Truncated = true
			continue
		}
		info := BufferInfo{
			Network: b.Network, Buffer: b.Target,
			LastTime: b.LastTS, Marker: b.Marker, Unread: b.Unread,
		}
		encoded, _ := json.Marshal(info)
		extra := len(encoded)
		if len(data.Buffers) > 0 {
			extra++
		}
		if used+extra > maxHistoryPayloadBytes {
			data.Truncated = true
			break
		}
		used += extra
		data.Buffers = append(data.Buffers, info)
	}
}

func (s *Session) handleGetHistory(ctx context.Context, env Envelope) {
	var d HistoryReq
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed get_history data"))
		return
	}
	if d.Network == "" || d.Buffer == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "get_history needs network and buffer"))
		return
	}
	// SQLite access is serialized, but without an admission bound many request
	// goroutines could each retain their materialized page while waiting to
	// enqueue it. Keep that transient memory bounded independently of the
	// outbound queue budgets.
	select {
	case s.hub.largeResponseSem <- struct{}{}:
		defer func() { <-s.hub.largeResponseSem }()
	case <-ctx.Done():
		return
	}
	limit := d.Limit
	if limit <= 0 || limit > maxHistoryMessages {
		limit = maxHistoryMessages
	}
	// Fetch one sentinel row so HasMore remains correct even when a count-full
	// page happens to be the end of history. The store's public cap is 500, so
	// maxHistoryMessages+1 remains within it.
	fetchLimit := limit + 1

	var (
		msgs []store.Message
		err  error
	)
	direction := historyBackward
	switch {
	case d.Before != nil:
		msgs, err = s.hub.store.Before(ctx, d.Network, d.Buffer, store.Cursor(*d.Before), fetchLimit)
	case d.After != nil:
		direction = historyForward
		msgs, err = s.hub.store.After(ctx, d.Network, d.Buffer, store.Cursor(*d.After), fetchLimit)
	case d.Around != nil:
		direction = historyAround
		msgs, err = s.hub.store.Around(ctx, d.Network, d.Buffer, store.Cursor(*d.Around), fetchLimit)
	case d.BeforeMsgID != "":
		var c store.Cursor
		if c, err = s.hub.store.CursorForMsgID(ctx, d.Network, d.Buffer, d.BeforeMsgID); err == nil {
			msgs, err = s.hub.store.Before(ctx, d.Network, d.Buffer, c, fetchLimit)
		}
	case d.AfterMsgID != "":
		direction = historyForward
		var c store.Cursor
		if c, err = s.hub.store.CursorForMsgID(ctx, d.Network, d.Buffer, d.AfterMsgID); err == nil {
			msgs, err = s.hub.store.After(ctx, d.Network, d.Buffer, c, fetchLimit)
		}
	default:
		msgs, err = s.hub.store.Latest(ctx, d.Network, d.Buffer, fetchLimit)
	}
	if errors.Is(err, store.ErrMsgIDNotFound) {
		s.push(errEnvelope(env.Seq, "unknown_msgid", "no message with that msgid"))
		return
	}
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "history query failed"))
		return
	}

	page := boundedHistoryPage(d.Network, d.Buffer, msgs, limit, direction, d.Around)
	s.push(envelope("history", env.Seq, page))
}

type historyDirection uint8

const (
	historyBackward historyDirection = iota
	historyForward
	historyAround
)

// boundedHistoryPage keeps the portion adjacent to the request anchor while
// enforcing an encoded payload limit. That detail is essential for Before:
// retaining the oldest prefix would create a permanent gap between the page
// and its anchor on the next request.
func boundedHistoryPage(network, buffer string, msgs []store.Message, limit int, direction historyDirection, around *Cursor) HistoryData {
	page := HistoryData{Network: network, Buffer: buffer, Messages: []EventData{}}
	countMore := len(msgs) > limit
	if countMore {
		switch direction {
		case historyBackward:
			msgs = msgs[len(msgs)-limit:]
		default:
			msgs = msgs[:limit]
		}
	}
	b := newHistoryPageBuilder(&page, msgs)
	switch direction {
	case historyBackward:
		b.selectBackward()
	case historyAround:
		b.selectAround(around)
	default:
		b.selectForward()
	}
	page.HasMore = countMore || len(page.Messages) < len(msgs)
	return page
}

// historyPageBuilder fills one history page from pre-encoded events while
// enforcing the encoded payload cap.
type historyPageBuilder struct {
	page   *HistoryData
	events []EventData
	costs  []int // encoded size of each event
	used   int   // encoded bytes admitted so far
}

func newHistoryPageBuilder(page *HistoryData, msgs []store.Message) *historyPageBuilder {
	b := &historyPageBuilder{
		page:   page,
		events: make([]EventData, len(msgs)),
		costs:  make([]int, len(msgs)),
	}
	base, _ := json.Marshal(*page) // includes the empty [] and false (longer than true)
	b.used = len(base)
	for i, m := range msgs {
		b.events[i] = eventData(m)
		enc, _ := json.Marshal(b.events[i])
		b.costs[i] = len(enc)
	}
	return b
}

// add appends event i to the page unless it would exceed the payload cap.
func (b *historyPageBuilder) add(i int) bool {
	extra := b.costs[i]
	if len(b.page.Messages) > 0 {
		extra++ // comma
	}
	if b.used+extra > maxHistoryPayloadBytes {
		return false
	}
	b.used += extra
	b.page.Messages = append(b.page.Messages, b.events[i])
	return true
}

// selectBackward selects newest-to-oldest, then restores chronological order.
func (b *historyPageBuilder) selectBackward() {
	for i := len(b.events) - 1; i >= 0; i-- {
		if !b.add(i) {
			break
		}
	}
	for i, j := 0, len(b.page.Messages)-1; i < j; i, j = i+1, j-1 {
		b.page.Messages[i], b.page.Messages[j] = b.page.Messages[j], b.page.Messages[i]
	}
}

func (b *historyPageBuilder) selectForward() {
	for i := range b.events {
		if !b.add(i) {
			break
		}
	}
}

// selectAround expands outward from the anchor, then restores chronological
// order.
func (b *historyPageBuilder) selectAround(around *Cursor) {
	selected := b.expandAround(b.aroundPivot(around))
	b.page.Messages = b.page.Messages[:0]
	for i, ok := range selected {
		if ok {
			b.page.Messages = append(b.page.Messages, b.events[i])
		}
	}
}

// aroundPivot locates the first event at or after the anchor cursor.
func (b *historyPageBuilder) aroundPivot(around *Cursor) int {
	if around == nil {
		return 0
	}
	for i, ev := range b.events {
		if ev.Time > around.TS || ev.Time == around.TS && ev.ID >= around.ID {
			return i
		}
	}
	return 0
}

// expandAround alternately admits events on either side of the pivot until
// the cap or both ends are reached, returning which indices were selected.
func (b *historyPageBuilder) expandAround(pivot int) []bool {
	selected := make([]bool, len(b.events))
	for left, right := pivot, pivot+1; left >= 0 || right < len(b.events); {
		if left >= 0 {
			if !b.add(left) {
				break
			}
			selected[left] = true
			left--
		}
		if right < len(b.events) {
			if !b.add(right) {
				break
			}
			selected[right] = true
			right++
		}
	}
	return selected
}

func (s *Session) handleSearch(ctx context.Context, env Envelope) {
	var d SearchReq
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed search data"))
		return
	}
	query := strings.TrimSpace(d.Query)
	if query == "" {
		s.push(errEnvelope(env.Seq, "bad_request", "search needs a query"))
		return
	}
	if len(query) > maxSearchQueryBytes || len(strings.Fields(query)) > maxSearchTerms {
		s.push(errEnvelope(env.Seq, "bad_request", "search query is too long"))
		return
	}
	select {
	case s.hub.largeResponseSem <- struct{}{}:
		defer func() { <-s.hub.largeResponseSem }()
	case <-ctx.Done():
		return
	}
	limit := d.Limit
	if limit <= 0 || limit > maxSearchMessages {
		limit = maxSearchMessages
	}
	opts := store.SearchOptions{
		Query: query, Network: d.Network, Target: d.Buffer, Limit: limit + 1,
	}
	if d.Before != nil {
		opts.Before = store.Cursor(*d.Before)
	}
	msgs, err := s.hub.store.Search(ctx, opts)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "search failed"))
		return
	}
	out := SearchData{Query: query, Messages: make([]EventData, 0, min(limit, len(msgs)))}
	if len(msgs) > limit {
		out.HasMore = true
		msgs = msgs[:limit]
	}
	base, _ := json.Marshal(out)
	used := len(base)
	for _, m := range msgs {
		ev := eventData(m)
		encoded, _ := json.Marshal(ev)
		extra := len(encoded)
		if len(out.Messages) > 0 {
			extra++
		}
		if used+extra > maxHistoryPayloadBytes {
			out.HasMore = true
			break
		}
		used += extra
		out.Messages = append(out.Messages, ev)
	}
	s.push(envelope("search_results", env.Seq, out))
}

func (s *Session) handleGetMarker(ctx context.Context, env Envelope) {
	var d MarkerRef
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed get_read_marker data"))
		return
	}
	t, err := s.hub.store.ReadMarker(ctx, d.Network, d.Buffer)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "read marker query failed"))
		return
	}
	s.push(envelope("read_marker", env.Seq, MarkerData{
		Network: d.Network, Buffer: d.Buffer, Time: markerMillis(t),
	}))
}

func (s *Session) handleSetMarker(ctx context.Context, env Envelope) {
	var d SetMarkerData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed set_read_marker data"))
		return
	}
	if d.Network == "" || d.Buffer == "" || d.Time <= 0 {
		s.push(errEnvelope(env.Seq, "bad_request", "set_read_marker needs network, buffer and time"))
		return
	}
	if err := s.hub.store.SetReadMarker(ctx, d.Network, d.Buffer, time.UnixMilli(d.Time)); err != nil {
		s.push(errEnvelope(env.Seq, "internal", "setting read marker failed"))
		return
	}
	// Read back: markers never move backwards, so the stored value is the
	// authoritative one for every device, including a requester that
	// tried to regress it.
	t, err := s.hub.store.ReadMarker(ctx, d.Network, d.Buffer)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "read marker query failed"))
		return
	}
	data := MarkerData{Network: d.Network, Buffer: d.Buffer, Time: markerMillis(t)}
	s.push(envelope("read_marker", env.Seq, data))
	s.hub.broadcastExcept(s, envelope("read_marker", 0, data))
	s.hub.notifyMarkerAdvance(d.Network, d.Buffer, t)
	// Bridge to draft/read-marker: other clients of this account (e.g.
	// on a bouncer) learn our read position. The authoritative value is
	// sent, never a regression. The synthetic server buffer is local-only —
	// never emit MARKREAD * upstream (it is not a real IRC target).
	if conn := s.hub.network(d.Network); conn != nil && conn.CapEnabled("draft/read-marker") && !t.IsZero() && !isServerBuffer(d.Buffer) {
		_ = conn.Send(&ircv4.Message{
			Command: "MARKREAD",
			Params:  []string{d.Buffer, "timestamp=" + t.UTC().Format(markreadTimeLayout)},
		})
	}
}

// MaxPrefsBytes caps the stored prefs blob; it carries the user's custom
// CSS, so it is generous, but not an unbounded write path into SQLite.
// Exported so the WebSocket read limit can be sized to admit it (see
// internal/api).
const MaxPrefsBytes = 64 * 1024

// prefsKey is the settings-table key for the client preferences blob.
const prefsKey = "prefs"

func (s *Session) handleGetPrefs(ctx context.Context, env Envelope) {
	v, err := s.hub.store.Setting(ctx, prefsKey)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "prefs query failed"))
		return
	}
	var d PrefsData
	// Only ever written via set_prefs, but never trust stored bytes to be
	// JSON — a bad blob here would silently corrupt the envelope.
	if v != "" && json.Valid([]byte(v)) {
		d.Prefs = json.RawMessage(v)
	}
	s.push(envelope("prefs", env.Seq, d))
}

// handleSetPrefs stores the preferences blob and pushes it to the user's
// other connected sessions, so appearance changes apply everywhere live.
func (s *Session) handleSetPrefs(ctx context.Context, env Envelope) {
	var d PrefsData
	if err := json.Unmarshal(env.Data, &d); err != nil || len(d.Prefs) == 0 {
		s.push(errEnvelope(env.Seq, "bad_request", "set_prefs needs a prefs value"))
		return
	}
	if len(d.Prefs) > MaxPrefsBytes {
		s.push(errEnvelope(env.Seq, "bad_request", "prefs too large"))
		return
	}
	if err := s.hub.store.SetSetting(ctx, prefsKey, string(d.Prefs)); err != nil {
		s.push(errEnvelope(env.Seq, "internal", "storing prefs failed"))
		return
	}
	s.push(envelope("ok", env.Seq, nil))
	s.hub.broadcastExcept(s, envelope("prefs", 0, d))
}

// rulesKey is the settings-table key for the synced highlight rules.
const rulesKey = "highlight_rules"

// Rule-list caps: rules are matched per live message in the pusher, so
// the list stays small by construction; these are sanity bounds, not
// expected sizes.
const (
	maxRules            = 64
	maxRulePatternChars = 256
	// Sized to store.MaxNetworkNameBytes: an unnamed network's effective
	// name is its host:port, and a rule scoped to it must survive the
	// server cap or the client-side clamp silently drops it.
	maxRuleNetworkChars = store.MaxNetworkNameBytes
	maxRuleIDChars      = 64
)

// loadRules parses the stored highlight rules; corrupt or absent data
// yields an empty list (mentions-only), never an error the caller must
// distinguish.
func (h *Hub) loadRules(ctx context.Context) []Rule {
	v, err := h.store.Setting(ctx, rulesKey)
	if err != nil {
		return nil
	}
	return parseRules(v)
}

// parseRules decodes a stored rules blob; "" or corrupt yields nil.
func parseRules(v string) []Rule {
	if v == "" {
		return nil
	}
	var d RulesData
	if err := json.Unmarshal([]byte(v), &d); err != nil {
		return nil
	}
	return d.Rules
}

// loadRulesChecked is loadRules distinguishing UNREADABLE (store error
// or corrupt blob — ok=false) from legitimately absent/empty, so the
// pusher can keep its last-known-good policy instead of silently
// adopting an empty one.
func (h *Hub) loadRulesChecked(ctx context.Context) ([]Rule, bool) {
	v, present, err := h.store.SettingValue(ctx, rulesKey)
	if err != nil {
		return nil, false
	}
	if !present {
		return nil, true // genuinely never stored: valid empty policy
	}
	// A PRESENT row that is not a JSON OBJECT is corruption/tampering
	// (handleSetRules only ever writes `{"rules":...}`), not a legitimate
	// empty policy — fail closed. `null`, scalars, and arrays all decode
	// without error into the zero value, so check the shape explicitly.
	var d RulesData
	if !isJSONObject(v) || json.Unmarshal([]byte(v), &d) != nil {
		return nil, false
	}
	return d.Rules, true
}

// loadFiltersChecked: same absent-vs-present-corrupt distinction for the
// ignore/mute lists, so a present-but-corrupt filter row (including the
// leak-prone `null`) fails closed rather than becoming "nothing filtered".
func (h *Hub) loadFiltersChecked(ctx context.Context) (FiltersData, bool) {
	v, present, err := h.store.SettingValue(ctx, filtersKey)
	if err != nil {
		return FiltersData{}, false
	}
	if !present {
		return FiltersData{}, true
	}
	var d FiltersData
	if !isJSONObject(v) || json.Unmarshal([]byte(v), &d) != nil {
		return FiltersData{}, false
	}
	return d, true
}

// isJSONObject reports whether s is a JSON object (`{...}`) — the only
// valid shape for our stored policy blobs. Rejects "null", arrays, and
// scalars, which json.Unmarshal would otherwise accept into a zero value.
func isJSONObject(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) > 0 && s[0] == '{'
}

func (s *Session) handleGetRules(ctx context.Context, env Envelope) {
	// Seeded distinguishes never-stored (client may seed from its
	// localStorage cache) from stored-but-empty (the user deleted every
	// rule — seeding would resurrect them). Rules is [] (not null) either
	// way.
	v, present, err := s.hub.store.SettingValue(ctx, rulesKey)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "rules query failed"))
		return
	}
	rules := parseRules(v)
	if rules == nil {
		rules = []Rule{}
	}
	s.push(envelope("rules", env.Seq, RulesData{Rules: rules, Seeded: present}))
}

// handleSetRules stores the highlight rules and pushes them to the
// user's other sessions — the same shape as set_prefs, except the server
// validates the schema because the pusher must parse it back.
func (s *Session) handleSetRules(ctx context.Context, env Envelope) {
	var d RulesData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed set_rules data"))
		return
	}
	if len(d.Rules) > maxRules {
		s.push(errEnvelope(env.Seq, "bad_request", "too many rules"))
		return
	}
	s.hub.syncedSettingsMu.Lock()
	defer s.hub.syncedSettingsMu.Unlock()
	// A client that was dirty across a network rename re-pushes scopes
	// under the old name; rewrite them so last-write-wins cannot undo
	// the rename's blob rewrite. A store error reading the map fails the
	// write closed — storing un-rewritten old-name scopes would silently
	// bypass the rename.
	renames, ok := s.hub.loadRenameMapChecked(ctx)
	if !ok {
		s.push(errEnvelope(env.Seq, "internal", "reading rename map failed"))
		return
	}
	rewrote := false
	kept := make([]Rule, 0, len(d.Rules))
	for _, r := range d.Rules {
		if len(r.Pattern) > maxRulePatternChars || len(r.Network) > maxRuleNetworkChars || len(r.ID) > maxRuleIDChars {
			s.push(errEnvelope(env.Seq, "bad_request", "rule field too long"))
			return
		}
		// The settings UI keeps a row while its pattern is still being
		// typed; storing it is harmless (matching skips empty patterns)
		// but dropping it here keeps the synced set canonical.
		if r.Pattern == "" {
			continue
		}
		if resolved := resolveRenamed(renames, r.Network); resolved != r.Network {
			r.Network = resolved
			rewrote = true
		}
		kept = append(kept, r)
	}
	blob, err := json.Marshal(RulesData{Rules: kept})
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "encoding rules failed"))
		return
	}
	if err := s.hub.store.SetSetting(ctx, rulesKey, string(blob)); err != nil {
		s.push(errEnvelope(env.Seq, "internal", "storing rules failed"))
		return
	}
	s.push(envelope("ok", env.Seq, nil))
	s.hub.broadcastExcept(s, envelope("rules", 0, RulesData{Rules: kept, Seeded: true}))
	// The rename map rewrote stale references the WRITER still holds
	// locally: echo the canonical set back so it re-adopts (its dirty
	// flag clears on the ok above, so the adopt is not suppressed) —
	// otherwise this device keeps an obsolete policy until its next
	// connect. Echoed only when something was rewritten, so a routine
	// save cannot clobber an editor row mid-typing.
	if rewrote {
		s.push(envelope("rules", 0, RulesData{Rules: kept, Seeded: true}))
	}
	s.hub.notifyPushConfigChanged()
}

// filtersKey is the settings-table key for the synced ignore/mute lists.
const filtersKey = "filters"

// Filter caps: like the rule caps, sanity bounds rather than expected
// sizes — the lists are consulted per live message in the pusher.
// maxMuteKeyChars covers the longest legal bufKey: a 300-byte network
// name (store.MaxNetworkNameBytes — an unnamed network's host:port
// fallback) + "\n" + a 512-byte stored target.
const (
	maxFilterNetworks    = 128
	maxIgnoresPerNetwork = 512
	maxIgnoreNickChars   = 128
	maxMutes             = 1024
	maxMuteKeyChars      = 1024
)

// renamesKey is the settings-table key for the network rename map
// (old name -> current name, flattened). It exists for one reason: a
// client that was DIRTY (offline edit, debounce window) during a rename
// later re-pushes rules/filters still referencing the old name, and
// last-write-wins would silently undo the rename rewrite. Applying this
// map at set_rules/set_filters time makes such stale writes self-heal.
// An entry is dropped the moment a network with the old name exists
// again (the mapping would then corrupt legitimate references).
const renamesKey = "network_renames"

// maxNetworkRenames bounds the map; renames are rare one-off events.
const maxNetworkRenames = 128

func (h *Hub) loadRenameMap(ctx context.Context) map[string]string {
	m, _ := h.loadRenameMapChecked(ctx)
	return m
}

// loadRenameMapChecked reads the rename map distinguishing a STORE READ
// ERROR (ok=false — the set path must reject rather than store
// un-rewritten old-name references, re-introducing the filter bypass the
// map exists to prevent) from an absent/corrupt map (empty, ok=true).
func (h *Hub) loadRenameMapChecked(ctx context.Context) (map[string]string, bool) {
	v, present, err := h.store.SettingValue(ctx, renamesKey)
	if err != nil {
		return map[string]string{}, false
	}
	if !present || strings.TrimSpace(v) == "" {
		return map[string]string{}, true
	}
	var m map[string]string
	if !isJSONObject(v) || json.Unmarshal([]byte(v), &m) != nil || m == nil {
		return map[string]string{}, true // corrupt: best-effort empty, don't block saves
	}
	return m, true
}

func (h *Hub) saveRenameMap(ctx context.Context, m map[string]string) {
	blob, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := h.store.SetSetting(ctx, renamesKey, string(blob)); err != nil {
		log.Printf("push: storing network rename map: %v", err)
	}
}

// clearNetworkRename drops name from the map when a network with that
// name (re)appears — rewriting its references would then be corruption.
// Standalone form (tests); the put path folds the clear into the create
// transaction via computeNameClearWrites.
func (h *Hub) clearNetworkRename(ctx context.Context, name string) {
	h.syncedSettingsMu.Lock()
	defer h.syncedSettingsMu.Unlock()
	m := h.loadRenameMap(ctx)
	if _, ok := m[name]; !ok {
		return
	}
	delete(m, name)
	h.saveRenameMap(ctx, m)
}

// computeNameClearWrites returns the settings write that drops any
// rename-map entry for `name` (a re-created network), for the create
// transaction, plus the pre-value for rollback. Empty writes (ok=true,
// nil map) when there is nothing to clear. ok=false on a store read
// error — the put aborts rather than risk a stale mapping. Caller holds
// syncedSettingsMu.
func (h *Hub) computeNameClearWrites(ctx context.Context, name string) (writes, rollback map[string]string, ok bool) {
	v, present, err := h.store.SettingValue(ctx, renamesKey)
	if err != nil {
		return nil, nil, false
	}
	if !present || strings.TrimSpace(v) == "" {
		return nil, nil, true // no map: nothing to clear
	}
	m := map[string]string{}
	if !isJSONObject(v) || json.Unmarshal([]byte(v), &m) != nil {
		return nil, nil, false
	}
	if _, has := m[name]; !has {
		return nil, nil, true
	}
	delete(m, name)
	blob, err := json.Marshal(m)
	if err != nil {
		return nil, nil, false
	}
	return map[string]string{renamesKey: string(blob)}, map[string]string{renamesKey: v}, true
}

// resolveRenamed maps a possibly-stale network reference to its current
// name.
func resolveRenamed(m map[string]string, name string) string {
	if next, ok := m[name]; ok {
		return next
	}
	return name
}

// loadFilters parses the stored ignore/mute lists; corrupt or absent
// data yields empty lists (nothing filtered).
func (h *Hub) loadFilters(ctx context.Context) FiltersData {
	v, err := h.store.Setting(ctx, filtersKey)
	if err != nil {
		return FiltersData{}
	}
	return parseFilters(v)
}

// parseFilters decodes a stored filters blob; "" or corrupt yields the
// zero value.
func parseFilters(v string) FiltersData {
	var d FiltersData
	if v == "" {
		return d
	}
	if err := json.Unmarshal([]byte(v), &d); err != nil {
		return FiltersData{}
	}
	return d
}

func (s *Session) handleGetFilters(ctx context.Context, env Envelope) {
	// Non-null empties plus the Seeded marker, like get_rules: the
	// client seeds from localStorage only when never stored.
	v, present, err := s.hub.store.SettingValue(ctx, filtersKey)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "filters query failed"))
		return
	}
	d := parseFilters(v)
	d.Seeded = present
	if d.Ignores == nil {
		d.Ignores = map[string][]string{}
	}
	if d.Mutes == nil {
		d.Mutes = []string{}
	}
	s.push(envelope("filters", env.Seq, d))
}

// canonicalIgnores validates and normalizes the ignore map: nicks are
// lowercased (the client's fold, so the pusher's lookup matches
// regardless of which device wrote the list), empties dropped, and
// network keys mapped through the rename map (see renamesKey). A
// non-empty problem string reports the first violation.
func canonicalIgnores(in map[string][]string, renames map[string]string) (map[string][]string, string, bool) {
	rewrote := false
	out := make(map[string][]string, len(in))
	// Track which nicks are already present per RESOLVED network so a stale
	// old-name key merging into the renamed one (or repeats within a single
	// list) fold to a set — and the cap applies to the final unique UNION,
	// not each input list. Without this two valid 512-entry lists that
	// resolve to the same network would produce a 1024-entry list.
	seen := make(map[string]map[string]bool, len(in))
	for network, nicks := range in {
		if network == "" || len(nicks) > maxIgnoresPerNetwork {
			return nil, "bad ignore list", false
		}
		if resolved := resolveRenamed(renames, network); resolved != network {
			network = resolved
			rewrote = true
		}
		kept, problem := canonicalNicks(nicks)
		if problem != "" {
			return nil, problem, false
		}
		set := seen[network]
		if set == nil {
			set = make(map[string]bool, len(kept))
			seen[network] = set
		}
		merged := appendUnseen(out[network], set, kept)
		if len(merged) > maxIgnoresPerNetwork {
			return nil, "bad ignore list", false
		}
		// Only record a key when something survived, so a network whose
		// entire list folded away (all empty/duplicate) stays absent
		// rather than appearing as an empty entry.
		if len(merged) > 0 {
			out[network] = merged
		}
	}
	return out, "", rewrote
}

// canonicalNicks lowercases and drops empties from one network's ignore
// list; a non-empty problem string reports the first over-long nick.
func canonicalNicks(nicks []string) ([]string, string) {
	kept := make([]string, 0, len(nicks))
	for _, n := range nicks {
		if n == "" {
			continue
		}
		if len(n) > maxIgnoreNickChars {
			return nil, "ignored nick too long"
		}
		kept = append(kept, strings.ToLower(n))
	}
	return kept, ""
}

// appendUnseen appends each item of src not already present in seen to
// dst, marking it seen; returns the grown slice. Callers cap the result
// afterwards — the shared dedup step for merging ignore lists that fold
// to one network (see canonicalIgnores, rewriteFilterRefs).
func appendUnseen(dst []string, seen map[string]bool, src []string) []string {
	for _, n := range src {
		if !seen[n] {
			seen[n] = true
			dst = append(dst, n)
		}
	}
	return dst
}

// canonicalMutes validates the mute list (client bufKey form), dropping
// empties and mapping the network part through the rename map; a
// non-empty problem string reports the first violation.
func canonicalMutes(in []string, renames map[string]string) ([]string, string, bool) {
	rewrote := false
	out := make([]string, 0, len(in))
	for _, m := range in {
		if m == "" {
			continue
		}
		if len(m) > maxMuteKeyChars {
			return nil, "mute key too long", false
		}
		if network, buffer, ok := strings.Cut(m, "\n"); ok {
			if resolved := resolveRenamed(renames, network); resolved != network {
				m = resolved + "\n" + buffer
				rewrote = true
			}
		}
		out = append(out, m)
	}
	return out, "", rewrote
}

// handleSetFilters stores the ignore/mute lists and pushes them to the
// user's other sessions.
func (s *Session) handleSetFilters(ctx context.Context, env Envelope) {
	var d FiltersData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		s.push(errEnvelope(env.Seq, "bad_request", "malformed set_filters data"))
		return
	}
	if len(d.Ignores) > maxFilterNetworks || len(d.Mutes) > maxMutes {
		s.push(errEnvelope(env.Seq, "bad_request", "too many filters"))
		return
	}
	s.hub.syncedSettingsMu.Lock()
	defer s.hub.syncedSettingsMu.Unlock()
	renames, ok := s.hub.loadRenameMapChecked(ctx)
	if !ok {
		s.push(errEnvelope(env.Seq, "internal", "reading rename map failed"))
		return
	}
	ignores, problem, rewroteIg := canonicalIgnores(d.Ignores, renames)
	if problem != "" {
		s.push(errEnvelope(env.Seq, "bad_request", problem))
		return
	}
	mutes, problem, rewroteMu := canonicalMutes(d.Mutes, renames)
	if problem != "" {
		s.push(errEnvelope(env.Seq, "bad_request", problem))
		return
	}
	canonical := FiltersData{Ignores: ignores, Mutes: mutes}
	blob, err := json.Marshal(canonical)
	if err != nil {
		s.push(errEnvelope(env.Seq, "internal", "encoding filters failed"))
		return
	}
	if err := s.hub.store.SetSetting(ctx, filtersKey, string(blob)); err != nil {
		s.push(errEnvelope(env.Seq, "internal", "storing filters failed"))
		return
	}
	s.push(envelope("ok", env.Seq, nil))
	canonical.Seeded = true // broadcast only; the stored blob stays Seeded-free
	s.hub.broadcastExcept(s, envelope("filters", 0, canonical))
	// Writer echo on rename rewrites — see handleSetRules' rationale.
	if rewroteIg || rewroteMu {
		s.push(envelope("filters", 0, canonical))
	}
	s.hub.notifyPushConfigChanged()
}

// Renaming a network moves its buffers/markers/monitors with the row,
// but the synced highlight rules and ignore/mute lists reference the
// network BY NAME — left stale, an ignored harasser or muted channel
// resumes alerting (and pushing) under the new name, and scoped keywords
// silently stop. Production computes the whole settings bundle (rename
// map + rewritten rules + filters) with computeRenameWrites and commits
// it IN THE SAME transaction as the network/history move; the combined
// renameSyncedNetworkRefs below is for callers with no network move of
// their own (tests).

// renameSyncedNetworkRefs does the whole settings-side rename (map +
// rules + filters) as one locked operation. Used where there is no
// network-move transaction to fold into (tests); the production path
// threads computeRenameWrites' output through the move tx.
func (h *Hub) renameSyncedNetworkRefs(ctx context.Context, oldName, newName string) {
	h.syncedSettingsMu.Lock()
	defer h.syncedSettingsMu.Unlock()
	rw, ok := h.computeRenameWrites(ctx, oldName, newName)
	if !ok {
		log.Printf("network %q -> %q: reading synced settings failed, skipping rewrite", oldName, newName)
		return
	}
	if err := h.store.SetSettings(ctx, rw.settings); err != nil {
		log.Printf("network %q -> %q: writing synced settings: %v", oldName, newName, err)
		return
	}
	h.broadcastRenameWrites(rw)
}

// renameWrites is the full settings-side result of a network rename:
// the settings to commit (map + any changed rules/filters keys) go in
// the SAME transaction as the history move; rollback holds the
// pre-rename values of exactly those keys, to restore atomically if the
// rename is rolled back.
type renameWrites struct {
	settings map[string]string
	rollback map[string]string

	rules          RulesData
	rulesChanged   bool
	filters        FiltersData
	filtersChanged bool
}

// computeRenameWrites reads the rename map, rules, and filters with
// FAIL-CLOSED semantics (a store error OR a present-but-corrupt blob
// aborts the whole rename, ok=false — never silently drops policy) and
// computes their old->new rewrites. Caller holds syncedSettingsMu.
func (h *Hub) computeRenameWrites(ctx context.Context, oldName, newName string) (renameWrites, bool) {
	rw := renameWrites{settings: map[string]string{}, rollback: map[string]string{}}
	if !h.renameMapSetting(ctx, &rw, oldName, newName) ||
		!h.renameRuleSetting(ctx, &rw, oldName, newName) ||
		!h.renameFilterSetting(ctx, &rw, oldName, newName) {
		return rw, false
	}
	return rw, true
}

// renameMapSetting folds the rename map and records it into rw (always
// written). Reports ok=false on a store error or a corrupt present map.
func (h *Hub) renameMapSetting(ctx context.Context, rw *renameWrites, oldName, newName string) bool {
	v, present, err := h.store.SettingValue(ctx, renamesKey)
	if err != nil {
		return false
	}
	preMap := v
	if !present {
		preMap = "{}" // absent restores as empty
	}
	rw.rollback[renamesKey] = preMap
	newMap, ok := foldRenameMap(v, present, oldName, newName)
	if !ok {
		return false // present-but-corrupt: abort, don't silently reset
	}
	blob, err := json.Marshal(newMap)
	if err != nil {
		return false
	}
	rw.settings[renamesKey] = string(blob)
	return true
}

// renameRuleSetting rewrites rule scopes and records them into rw only
// if a scope changed. ok=false on a store error or a corrupt present blob.
func (h *Hub) renameRuleSetting(ctx context.Context, rw *renameWrites, oldName, newName string) bool {
	v, present, err := h.store.SettingValue(ctx, rulesKey)
	if err != nil {
		return false
	}
	if !present {
		return true
	}
	var d RulesData
	if !isJSONObject(v) || json.Unmarshal([]byte(v), &d) != nil {
		return false // corrupt: abort rather than rewrite-from-empty
	}
	if rewriteRuleRefs(d.Rules, oldName, newName) {
		blob, err := json.Marshal(d)
		if err != nil {
			return false
		}
		rw.settings[rulesKey] = string(blob)
		rw.rollback[rulesKey] = v
		rw.rules, rw.rulesChanged = d, true
	}
	return true
}

// renameFilterSetting is renameRuleSetting for the ignore/mute lists.
func (h *Hub) renameFilterSetting(ctx context.Context, rw *renameWrites, oldName, newName string) bool {
	v, present, err := h.store.SettingValue(ctx, filtersKey)
	if err != nil {
		return false
	}
	if !present {
		return true
	}
	var d FiltersData
	if !isJSONObject(v) || json.Unmarshal([]byte(v), &d) != nil {
		return false
	}
	changed, ok := rewriteFilterRefs(&d, oldName, newName)
	if !ok {
		return false // deduplicated ignore union over cap: abort the rename
	}
	if changed {
		blob, err := json.Marshal(d)
		if err != nil {
			return false
		}
		rw.settings[filtersKey] = string(blob)
		rw.rollback[filtersKey] = v
		rw.filters, rw.filtersChanged = d, true
	}
	return true
}

// broadcastRenameWrites announces the rewritten rules/filters to every
// session (after their commit) and pokes the pusher. Caller holds
// syncedSettingsMu.
func (h *Hub) broadcastRenameWrites(rw renameWrites) {
	if rw.rulesChanged {
		h.broadcast(envelope("rules", 0, RulesData{Rules: rw.rules.Rules, Seeded: true}))
	}
	if rw.filtersChanged {
		rw.filters.Seeded = true
		h.broadcast(envelope("filters", 0, rw.filters))
	}
	if rw.rulesChanged || rw.filtersChanged {
		h.notifyPushConfigChanged()
	}
}

// foldRenameMap folds old->new into the current map (existing k->old
// become k->new) and records old->new. At saturation it keeps ONLY the
// current mapping — the most important one, and old entries are
// best-effort stale-client healing anyway — so the current rename is
// never silently dropped. ok=false when the present map is corrupt
// (non-object or bad JSON), so the caller aborts rather than silently
// resetting it.
func foldRenameMap(current string, present bool, oldName, newName string) (map[string]string, bool) {
	m := map[string]string{}
	if present && strings.TrimSpace(current) != "" {
		if !isJSONObject(current) || json.Unmarshal([]byte(current), &m) != nil || m == nil {
			return nil, false
		}
	}
	for k, v := range m {
		if v == oldName {
			m[k] = newName
		}
	}
	delete(m, newName)
	if len(m) >= maxNetworkRenames {
		m = map[string]string{}
	}
	m[oldName] = newName
	return m, true
}

// rewriteRuleRefs rewrites rule scopes from oldName to newName in place;
// reports whether anything changed.
func rewriteRuleRefs(rules []Rule, oldName, newName string) bool {
	changed := false
	for i := range rules {
		if rules[i].Network == oldName {
			rules[i].Network = newName
			changed = true
		}
	}
	return changed
}

// rewriteFilterRefs rewrites the ignore map key and mute bufKey prefixes
// from oldName to newName in place; reports whether anything changed and
// whether the result is valid. ok=false means the DEDUPLICATED union of
// the old- and new-name ignore lists still exceeds maxIgnoresPerNetwork:
// the caller aborts the rename rather than TRUNCATE a privacy policy
// (silently dropping ignores is worse than a rejected rename).
func rewriteFilterRefs(d *FiltersData, oldName, newName string) (changed, ok bool) {
	if nicks, has := d.Ignores[oldName]; has {
		delete(d.Ignores, oldName)
		// Renaming onto a name that already has ignores is exotic; build the
		// full deduplicated union (never truncated). Dedup the DESTINATION
		// too, not just the incoming list — a pre-existing stored list may
		// carry duplicates, and counting those toward the cap could
		// spuriously reject a rename whose true unique union fits.
		merged := make([]string, 0, len(d.Ignores[newName])+len(nicks))
		seen := make(map[string]bool, len(d.Ignores[newName])+len(nicks))
		merged = appendUnseen(merged, seen, d.Ignores[newName])
		merged = appendUnseen(merged, seen, nicks)
		if len(merged) > maxIgnoresPerNetwork {
			return false, false // unique union over cap: abort the rename
		}
		d.Ignores[newName] = merged
		changed = true
	}
	prefix := oldName + "\n"
	for i, m := range d.Mutes {
		if strings.HasPrefix(m, prefix) {
			d.Mutes[i] = newName + "\n" + strings.TrimPrefix(m, prefix)
			changed = true
		}
	}
	return changed, true
}

// markreadTimeLayout is the server-time format used by MARKREAD
// (https://ircv3.net/specs/extensions/read-marker, fetched 2026-07-15).
const markreadTimeLayout = "2006-01-02T15:04:05.000Z"

// markerMillis maps the zero time (marker unset) to protocol 0.
func markerMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
