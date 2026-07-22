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
	maxRuleNetworkChars = 128
	maxRuleIDChars      = 64
)

// loadRules parses the stored highlight rules; corrupt or absent data
// yields an empty list (mentions-only), never an error the caller must
// distinguish.
func (h *Hub) loadRules(ctx context.Context) []Rule {
	v, err := h.store.Setting(ctx, rulesKey)
	if err != nil || v == "" {
		return nil
	}
	var d RulesData
	if err := json.Unmarshal([]byte(v), &d); err != nil {
		return nil
	}
	return d.Rules
}

func (s *Session) handleGetRules(ctx context.Context, env Envelope) {
	// Reply with rules:[] (not null) when nothing is stored, so the
	// client's "server has no rules yet → seed from localStorage" branch
	// has an unambiguous signal.
	rules := s.hub.loadRules(ctx)
	if rules == nil {
		rules = []Rule{}
	}
	s.push(envelope("rules", env.Seq, RulesData{Rules: rules}))
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
	s.hub.broadcastExcept(s, envelope("rules", 0, RulesData{Rules: kept}))
	s.hub.notifyRulesChanged()
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
