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

// Package hub owns fan-out: IRC events flow in from the per-network
// connection managers, get persisted to the store, and are broadcast to
// every connected WebSocket session. Sessions send client requests
// (history pages, read markers, outgoing messages) back through it.
// The hub is transport-agnostic: internal/api owns the actual WebSocket
// connections and moves Envelopes in and out of Sessions.
package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"ircthing/internal/irc"
	"ircthing/internal/store"

	ircv4 "gopkg.in/irc.v4"
)

// serverBufferTarget is the per-network "server buffer" (The Lounge lobby):
// server/service NOTICEs and server-info lines collect here instead of
// scattering into per-sender query buffers. "*" is the IRC placeholder the
// server itself uses for our nick before registration, and is never a valid
// channel or real nick, so it can't collide with a conversation buffer.
const serverBufferTarget = "*"

// isServerBuffer reports whether target is the synthetic per-network server
// buffer. It collects service/server NOTICEs but is NOT a real IRC target, so
// outbound conversation operations (PRIVMSG/TAGMSG typing) and upstream
// history sync (CHATHISTORY/MARKREAD) must never address it.
func isServerBuffer(target string) bool { return target == serverBufferTarget }

// Conn is the slice of *irc.Manager the hub consumes.
type Conn interface {
	Name() string
	Nick() string
	Events() <-chan irc.Event
	Send(*ircv4.Message) error
	// SendAll queues msgs atomically — all or none — so a multi-message user
	// action can't deliver a partial prefix when a later line fails.
	SendAll([]*ircv4.Message) error
	Channel(name string) (topic string, members []irc.Member, ok bool)
	CapEnabled(name string) bool
	IsChannel(target string) bool // per the network's ISUPPORT CHANTYPES
	ChanTypes() string
	// StatusPrefixes is the ISUPPORT STATUSMSG set (e.g. "~&@%+"): a
	// PRIVMSG/NOTICE to "@#chan" targets ops of #chan. Empty when
	// unadvertised.
	StatusPrefixes() string
	// Fold lowercases a name per the network's ISUPPORT CASEMAPPING
	// (rfc1459 folds []\^ to {}|~ pairs); every nick/channel comparison
	// must use it — ASCII-only folding misroutes on rfc1459 networks.
	Fold(name string) string
	// RequestChatHistory backfills target with messages newer than the
	// resume point (msgid preferred over sinceMs when non-empty); a
	// no-op on networks without draft/chathistory.
	RequestChatHistory(target string, sinceMs int64, msgid string)
	// HistoryPageSize is the per-request message limit chathistory
	// requests are clamped to (ISUPPORT CHATHISTORY); a replay batch of
	// this size means the gap may extend past it.
	HistoryPageSize() int
	// EnsureNames lazily fetches a channel's membership under
	// draft/no-implicit-names; a no-op otherwise.
	EnsureNames(channel string)
	// SendMultiline sends the lines as one draft/multiline batch.
	SendMultiline(target string, lines []string) error
	// SetMonitored replaces the MONITOR list; MonitorAdd/MonitorRemove
	// adjust it by one nick (MONITOR extension).
	// ReconcileMonitored drives the server MONITOR list toward the persisted
	// buddy list `desired`, sending only the incremental delta atomically. The
	// hub calls it on registration and after each add/remove; it returns an
	// error if the delta couldn't be enqueued (retried on the next call).
	ReconcileMonitored(desired []string) error
	// MonitorRejected handles a 734 (list full): records the server's real
	// capacity and authoritatively clears+rebuilds the MONITOR list. Repeated
	// stale numerics at the same cap are ignored so delayed replies cannot drive
	// an add/reject loop. limit is the AUTHORITATIVE cap the numeric carries
	// (0 => the manager infers it); gen is the event's connection generation,
	// so a 734 from a superseded connection is ignored.
	MonitorRejected(nicks []string, limit int, gen uint64, desired []string) error
}

type Hub struct {
	store *store.Store

	// Root lifetime for dynamically started networks (see UseRoot).
	rootCtx context.Context
	rootWG  *sync.WaitGroup
	// netOps serializes network start/stop/config operations.
	netOps sync.Mutex
	// lifecycleGate guards create-on-demand session writes (persistOwn,
	// AddMonitor) against network teardown: writers take RLock and
	// re-check the network is still known; delete/rename take Lock, so a
	// racing write either completes before the delete (and is
	// cascade-removed) or sees the network gone and skips — never
	// resurrecting an orphan networks row.
	lifecycleGate sync.RWMutex
	// bufferMutationMu linearizes live store append+broadcast operations with
	// close_buffer's delete+tombstone+buffer_closed broadcast. Store-level
	// atomic guards prevent database resurrection, but without this wider
	// boundary an append completed just before a delete could broadcast its
	// event just after buffer_closed and resurrect the client-only buffer.
	// Lock order: lifecycleGate (when needed), bufferMutationMu, store, h.mu.
	bufferMutationMu sync.Mutex

	// monMu serializes the monitor-add folded-duplicate check with its
	// insert: SQLite uniqueness is byte-exact, so without this two
	// concurrent adds of Alice and alice each pass the check and both
	// persist — the casemapped-duplicate state the check exists to prevent.
	monMu sync.Mutex

	mu                sync.Mutex
	networks          map[string]Conn
	states            map[string]string          // last known connection state per network
	presence          map[string]map[string]bool // network -> monitored nick -> online (MONITOR)
	motdWanted        map[string][]string        // queued explicit /motd targets per network ("" = untargeted)
	procs             map[string]*netProc        // running network lifecycles
	recoveryRows      map[string]int64           // synthetic stopped label -> invalid DB rowid
	sessions          map[*Session]struct{}
	queuedBytes       atomic.Int64
	sessionQueueBytes int64
	hubQueueBytes     int64
	largeResponseSem  chan struct{}
	// recentClose bounds the buffer-resurrection race: a buffer closed
	// from the UI (close_buffer) must not be re-created by straggler
	// inbound traffic that was already in flight. Keyed by
	// network+"\x00"+foldedBuffer -> unix-ms of the close.
	recentClose      map[string]int64
	recentCloseBytes int

	// Web Push scheduler plumbing (see push.go). The channels feed the
	// single runPusher goroutine; sends are always non-blocking. pushSubs
	// caches the subscription count so the per-message fast path is one
	// atomic load; pushPubKey (under mu) is the VAPID public key the
	// HTTP layer serves to clients.
	pushCandidates chan pushCandidate
	pushMarkers    chan markerAdvance
	pushRulesDirty chan struct{}
	pushSubs       atomic.Int64
	pushPubKey     string
}

// Store exposes the shared store so the HTTP layer can read/write its own
// settings keys (e.g. the runtime media-proxy config) without a second
// handle to the database.
func (h *Hub) Store() *store.Store { return h.store }

func New(st *store.Store) *Hub {
	return &Hub{
		store:             st,
		recentClose:       make(map[string]int64),
		networks:          make(map[string]Conn),
		states:            make(map[string]string),
		presence:          make(map[string]map[string]bool),
		motdWanted:        make(map[string][]string),
		procs:             make(map[string]*netProc),
		recoveryRows:      make(map[string]int64),
		sessions:          make(map[*Session]struct{}),
		sessionQueueBytes: defaultSessionQueueBytes,
		hubQueueBytes:     defaultHubQueueBytes,
		largeResponseSem:  make(chan struct{}, 4),
		pushCandidates:    make(chan pushCandidate, 256),
		pushMarkers:       make(chan markerAdvance, 64),
		pushRulesDirty:    make(chan struct{}, 1),
	}
}

// Run consumes one network's events until ctx is canceled: message
// traffic is persisted and broadcast, state changes are broadcast. A
// failed append is logged, not fatal: losing one line of scrollback beats
// dropping the connection's event stream.
func (h *Hub) Run(ctx context.Context, c Conn) error {
	name := c.Name()
	h.mu.Lock()
	h.networks[name] = c
	if _, ok := h.states[name]; !ok {
		h.states[name] = irc.StateDisconnected.String()
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.networks, name)
		h.mu.Unlock()
	}()

	// Open chathistory batches on this connection: ref -> replay
	// progress. Messages inside are replayed history — persisted, but
	// not pushed as live events; a history_changed hint follows when the
	// batch ends, and a full page triggers the next backfill request.
	histBatches := make(map[string]*histBatch)
	// backfillPages counts follow-up pages per target on the current
	// connection, bounding paginated backfill (see maxBackfillPages).
	backfillPages := make(map[string]int)
	// whois accumulates WHOIS reply numerics into a card, keyed by the
	// queried nick, until 318 flushes it (see accumulateWhois).
	whois := make(map[string]*WhoisData)
	// reg gates the post-registration synchronization for the current
	// connection (see regSync).
	reg := &regSync{}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-c.Events():
			switch ev.Kind {
			case irc.EventState:
				if ev.State == irc.StateConnecting {
					// Reset connection-scoped state on a NEW connection. Doing it on
					// StateConnecting — and ONLY there — is what closes the
					// pre-registration window: the manager emits StateConnecting
					// before it starts the read loop, and events arrive over one
					// ordered channel, so stale batch refs and pagination budgets are
					// cleared BEFORE any message — even a pre-registration BATCH —
					// from the new connection is processed against them. There is
					// deliberately NO second reset at StateRegistered: the read loop
					// runs before that event is emitted, so a batch legitimately
					// opened between the two events would be erased by it.
					histBatches = make(map[string]*histBatch)
					backfillPages = make(map[string]int)
					whois = make(map[string]*WhoisData)
					*reg = regSync{}
					// Presence and the MOTD gate are connection-scoped too, and an
					// STS upgrade redials via StateConnecting WITHOUT ever emitting
					// StateDisconnected — so clearing them only there let stale
					// presence and an armed MOTD gate survive into the secure
					// connection. StateConnecting is the true generation boundary;
					// the Disconnected clears below are now a harmless backstop.
					h.clearPresence(ev.Network)
					h.clearMOTD(ev.Network)
				}
				h.onState(ctx, c, ev, reg)
			case irc.EventMessage:
				// End of the welcome burst (RPL_ENDOFMOTD/ERR_NOMOTD): the
				// second of the two signals post-registration synchronization
				// waits for. Checked here — not inside onMessage — because it
				// must fire regardless of how the numeric is otherwise routed.
				if !historyReplay(ev, histBatches) && !reg.burstEnded &&
					(ev.Msg.Command == "376" || ev.Msg.Command == "422") {
					reg.burstEnded = true
					h.maybeStartSync(ctx, c, reg)
				}
				if err := h.onMessage(ctx, c, ev, histBatches, backfillPages, whois); err != nil {
					return err
				}
			}
		}
	}
}

// regSync tracks when one connection's post-registration synchronization
// (query-buffer backfill, MONITOR re-establishment) may start: only after
// BOTH StateRegistered (sends are accepted) AND the end of the welcome
// burst (376/422, which standard registration ordering — see
// https://modern.ircdocs.horse — places after the 005 ISUPPORT burst).
// Keying off StateRegistered alone raced the asynchronously-applied 005:
// the hub could send msgid= selectors before MSGREFTYPES arrived, request
// 100 messages before a smaller CHATHISTORY limit was known, send an
// oversized MONITOR list, and classify buffers with default CHANTYPES.
// The two flags accept either arrival order — the read loop starts before
// StateRegistered is emitted, so a fast server's 376 can precede it on the
// event channel. A server that never ends its MOTD (an RFC violation; 376
// or 422 is mandatory) simply gets no query backfill/MONITOR on that
// connection — accepted, since such a server controls the history anyway.
type regSync struct {
	registered bool
	burstEnded bool
	synced     bool
}

// maybeStartSync runs the one-shot post-registration synchronization once
// both regSync signals are in.
func (h *Hub) maybeStartSync(ctx context.Context, c Conn, reg *regSync) {
	if reg.synced || !reg.registered || !reg.burstEnded {
		return
	}
	reg.synced = true
	h.backfill(ctx, c)
	h.startMonitor(ctx, c)
}

// onState records and broadcasts a connection state change. Registration
// arms the first of regSync's two gates; backfill and MONITOR
// re-establishment start only once the welcome burst has also ended (see
// regSync/maybeStartSync).
func (h *Hub) onState(ctx context.Context, c Conn, ev irc.Event, reg *regSync) {
	h.mu.Lock()
	h.states[ev.Network] = ev.State.String()
	h.mu.Unlock()
	errStr := ""
	if ev.Err != nil {
		// A registration error embeds the server's raw ERROR line (handshake.go),
		// so clamp+quote it like FAIL/MONITOR — otherwise a hostile server drives
		// control bytes and ~64 KiB lines into the log and the client broadcast.
		errStr = clampServerInfo(ev.Err.Error())
		log.Printf("irc[%s]: %s: %q", ev.Network, ev.State, errStr)
	} else {
		log.Printf("irc[%s]: %s", ev.Network, ev.State)
	}
	h.broadcast(envelope("state", 0, StateData{
		Network: ev.Network,
		State:   ev.State.String(),
		Error:   errStr,
	}))
	if ev.State == irc.StateRegistered {
		reg.registered = true
		h.maybeStartSync(ctx, c, reg)
	}
	if ev.State == irc.StateDisconnected {
		// Backstop: the authoritative clear is on StateConnecting (the
		// connection-generation boundary, which the STS redial also crosses).
		// Clearing here too is harmless and covers a terminal disconnect that
		// is never followed by a reconnect.
		h.clearPresence(ev.Network)
		h.clearMOTD(ev.Network)
	}
}

// onMessage routes one server message: protocol-internal traffic is
// consumed, ephemeral kinds are relayed, everything else persists (and,
// when live rather than replayed, broadcasts). The only error is a
// canceled context surfacing through a failed persist.
// handleControlNumeric handles the self-contained standard-reply
// (FAIL/WARN/NOTE) and MONITOR numerics, returning true when it consumed
// the message.
func (h *Hub) handleControlNumeric(ctx context.Context, c Conn, ev irc.Event) bool {
	switch ev.Msg.Command {
	case "FAIL", "WARN", "NOTE":
		if ev.Msg.Command == "FAIL" {
			// %q + clamp: the line is server-controlled — raw %s would let a
			// hostile server drive terminal escape sequences and ~64 KiB lines
			// into the log (the client-facing broadcast below is already
			// clamped; the log must be too).
			log.Printf("irc[%s]: server failure: %q", ev.Network, clampServerInfo(ev.Msg.String()))
		}
		if txt := strings.TrimSpace(strings.Join(ev.Msg.Params, " ")); txt != "" {
			h.broadcast(envelope("server_info", 0, ServerInfoData{
				Network: ev.Network,
				Text:    clampServerInfo(strings.ToLower(ev.Msg.Command) + ": " + txt),
			}))
		}
		return true
	case "730": // RPL_MONONLINE
		h.updatePresence(ctx, c, ev.Network, ev.Msg, true)
		return true
	case "731": // RPL_MONOFFLINE
		h.updatePresence(ctx, c, ev.Network, ev.Msg, false)
		return true
	case "734": // ERR_MONLISTFULL
		// Format: <nick> <limit> <rejected,targets> :message. Under monMu (the
		// same lock as mutations), pass the persisted desired list to the manager.
		// A 734 has no request correlation, so the manager cannot safely delete
		// only the named targets: a delayed rejection may predate a successful
		// remove/re-add. It instead performs one atomic MONITOR C + cap-clamped
		// rebuild, then ignores stale same-cap repeats so they cannot alternate
		// additions forever.
		if len(ev.Msg.Params) > 2 {
			limit, _ := strconv.Atoi(ev.Msg.Params[1])
			h.monMu.Lock()
			if desired, lerr := h.store.Monitors(ctx, ev.Network); lerr == nil {
				// One atomic, gen-checked call: learn the authoritative limit
				// and clear+rebuild if this connection has not recovered at it.
				if rerr := c.MonitorRejected(strings.Split(ev.Msg.Params[2], ","), limit, ev.Gen, desired); rerr != nil {
					log.Printf("irc[%s]: monitor rebuild after 734: %v", ev.Network, rerr)
				}
			} else {
				log.Printf("irc[%s]: monitor 734 skipped (list read failed): %v", ev.Network, lerr)
			}
			h.monMu.Unlock()
		}
		log.Printf("irc[%s]: MONITOR list full: %q", ev.Network, clampServerInfo(ev.Msg.Trailing()))
		return true
	case "732", "733": // RPL_MONLIST / end — the store is our source of truth
		return true
	}
	return false
}

func (h *Hub) onMessage(ctx context.Context, c Conn, ev irc.Event, histBatches map[string]*histBatch, backfillPages map[string]int, whois map[string]*WhoisData) error {
	if ev.Msg.Command == "BATCH" {
		h.trackHistoryBatch(ctx, ev, c, histBatches, backfillPages)
		return nil
	}
	// An empty batch tag must never match: histBatches[""] could exist
	// if a server opened a chathistory batch with a bare "+" reference
	// (guarded below in trackHistoryBatch), and would then classify all
	// un-batched live traffic as replay — persisted but never
	// broadcast.
	//
	// Clamp the lookup EXACTLY like the stored key (trackHistoryBatch
	// clamps at store): an oversized ref clamped only at store would miss
	// here and misclassify the batch's replayed messages as live traffic —
	// notifications, unread counts, and duplicate own messages from
	// history.
	batchRef := clampBatchRef(ev.Msg.Tags["batch"])
	replay := historyReplay(ev, histBatches)
	batch := histBatches[batchRef]
	if replay {
		batch.noteReplayed(ev)
	} else if h.liveControlConsumed(ctx, c, ev, whois) {
		return nil
	}
	switch ev.Msg.Command {
	case "TAGMSG": // ephemeral, never persisted
		if !replay {
			h.relayTyping(ctx, ev, c)
		}
		return nil
	case "MARKREAD": // marker state, never persisted
		if !replay {
			h.applyUpstreamMarker(ctx, c, ev)
		}
		return nil
	case "REDACT": // updates an existing row, not a new one
		h.applyRedaction(ctx, ev, c, replay, batch)
		return nil
	case "QUIT", "NICK": // fan out per shared channel
		h.persistMembership(ctx, c, ev, replay, batch)
		return nil
	}
	return h.persistEvent(ctx, c, ev, replay, batch)
}

func historyReplay(ev irc.Event, batches map[string]*histBatch) bool {
	ref := clampBatchRef(ev.Msg.Tags["batch"])
	return ref != "" && batches[ref] != nil
}

// noteReplayed records one replayed message's progress in its batch.
// It tracks the batch's own newest message as the anchor for the next
// page: live traffic may already have stored rows newer than the gap, so
// the store's latest row would skip past it.
func (b *histBatch) noteReplayed(ev irc.Event) {
	b.count++
	if t := messageTime(ev); !t.IsZero() {
		b.lastTS = t.UnixMilli()
	}
	// Clamp+detach: tag values are parser-detached but unbounded, so an
	// oversized msgid retained across 256 open batches is real memory —
	// and it would be interpolated into the next CHATHISTORY request,
	// which the writer drops as over-length anyway.
	b.lastID = clampDetach(ev.Msg.Tags["msgid"])
}

// liveControlConsumed runs the side effects only LIVE (non-replayed)
// messages have and reports whether the message was fully consumed by
// them. Every live-only numeric/control effect runs only AFTER replay
// classification (the caller's replay check). Replayed 470/MONITOR/WHOIS/
// MOTD numerics are history, not current connection state, and must
// neither mutate state nor emit ephemeral cards.
func (h *Hub) liveControlConsumed(ctx context.Context, c Conn, ev irc.Event, whois map[string]*WhoisData) bool {
	if h.handleControlNumeric(ctx, c, ev) {
		return true
	}
	if h.accumulateWhois(c, ev, whois) {
		return true
	}
	if ev.Msg.Command == "470" {
		h.persistAutojoin(ctx, c, ev)
	}
	if info, ok := h.serverInfo(ev); ok {
		info.Network = ev.Network
		h.broadcast(envelope("server_info", 0, info))
		return true
	}
	h.liveHints(ctx, c, ev)
	return false
}

// liveHints handles the side effects only live (non-replayed) messages
// have: members_changed refetch hints, surfacing INVITEs, and the
// backfill request our own JOIN triggers.
func (h *Hub) liveHints(ctx context.Context, c Conn, ev irc.Event) {
	if hint, affected := membersHint(ev.Msg); affected {
		// Canonical spelling: clients key buffers by it (see history_changed).
		// An empty hint (the channel-spanning QUIT/NICK/AWAY/ACCOUNT/CHGHOST)
		// stays empty — skip CanonicalBuffer, which would otherwise run a full
		// O(buffers) name scan under the store lock for every one of these
		// high-volume lines (a netsplit QUIT burst) only to return "".
		buffer := ""
		if hint != "" {
			buffer = h.store.CanonicalBuffer(ctx, ev.Network, hint, c.Fold)
		}
		h.broadcast(envelope("members_changed", 0, MembersChangedData{
			Network: ev.Network, Buffer: buffer,
		}))
	}
	// An INVITE has no reply numeric to forward — surface it directly
	// (both direct invites and invite-notify's third-party ones).
	if ev.Msg.Command == "INVITE" && ev.Msg.Prefix != nil && len(ev.Msg.Params) >= 2 {
		who := ev.Msg.Param(0)
		if c.Fold(who) == c.Fold(c.Nick()) {
			who = "you"
		}
		h.broadcast(envelope("server_info", 0, ServerInfoData{
			Network: ev.Network,
			Text:    clampServerInfo(ev.Msg.Prefix.Name + " invited " + who + " to " + ev.Msg.Param(1)),
		}))
	}
	// Our own JOIN is the moment to backfill that channel: requesting at
	// registration would race the JOIN, and servers may refuse history
	// for channels we are not in yet.
	if ev.Msg.Command == "JOIN" && ev.Msg.Prefix != nil &&
		c.Nick() != "" && c.Fold(ev.Msg.Prefix.Name) == c.Fold(c.Nick()) {
		// Resolve the resume point against the CANONICAL stored spelling: the
		// JOIN echo's casing can differ from how the buffer was stored (a
		// UI-typed name, a recreated channel, an rfc1459 fold pair), and an
		// exact-spelling lastStored would miss and silently skip backfill,
		// leaving a permanent scrollback gap. RequestChatHistory keeps the
		// wire spelling (a server-valid name).
		canon := h.store.CanonicalBuffer(ctx, ev.Network, ev.Msg.Param(0), c.Fold)
		if ts, msgid := h.lastStored(ctx, ev.Network, canon); ts > 0 {
			c.RequestChatHistory(ev.Msg.Param(0), ts, msgid)
		}
	}
	h.persistAutojoin(ctx, c, ev)
}

// maxPersistedChannelLen bounds a channel name written to the stored config —
// generous over any real CHANNELLEN, it rejects a hostile oversized name that
// would fail registration-line validation on restart.
const maxPersistedChannelLen = 200

// persistAutojoin keeps the stored network definition's channel list in step
// with our own live JOIN/PART, so a channel joined at runtime — via the UI, a
// typed /join, or a server forward — is rejoined after a restart, and a PART
// stops it. A KICK is intentionally NOT removed: it stays in the rejoin intent
// (matching the manager's `joined` set, which rejoins kicked channels).
// Best-effort — a failure leaves the stored list stale but never drops the
// event; editChannelList makes a repeat (initial rejoin echo) a no-op.
func (h *Hub) persistAutojoin(ctx context.Context, c Conn, ev irc.Event) {
	list, add := autojoinChange(c, ev)
	if list == "" {
		return
	}
	// TryLock, never Lock: this runs on the Hub event goroutine, and a network
	// edit/delete holds netOps while StopNetwork waits for THIS goroutine to
	// exit — a blocking Lock here would deadlock. Best-effort: skip persisting
	// this membership change while a network operation is in progress.
	if !h.netOps.TryLock() {
		return
	}
	defer h.netOps.Unlock()
	// JOIN/PART carry a comma-list of channels (RFC 2812); collect the whole list
	// and apply it in ONE read-modify-write, or a combined self-PART
	// ("PART #a,#b") would leave the stored list with #a,#b still present and
	// rejoin them after a restart. Batching also caps a hostile comma-list flood:
	// a "PART #x,#y,…" with thousands of tokens is one store round-trip, not one
	// per token (F2), and each token is length-clamped BEFORE it reaches c.Fold —
	// a token longer than maxPersistedChannelLen can neither be validly persisted
	// (add) nor fold-match any stored ≤200-byte name (remove), so oversized junk
	// is dropped before the fold, defusing the self-PART fold amplifier (F1).
	var chans []string
	for _, ch := range strings.Split(list, ",") {
		if ch == "" || !c.IsChannel(ch) || len(ch) > maxPersistedChannelLen {
			continue
		}
		// A spoofed self-JOIN for a name carrying framing bytes would make
		// netconf.Validate fail on every restart (bricking the network); never
		// persist one. Invalid UTF-8 is just as poisonous, differently:
		// json.Marshal coerces it to U+FFFD (3 bytes per bad byte), so the
		// STORED spelling silently diverges from the live one — restart joins
		// a mojibake channel, dedup misses forever (one duplicate entry per
		// echo), and a near-cap name can inflate past the registration-line
		// validator, putting the network into skip-on-startup. A PART (remove)
		// is always allowed, so an already-stored bad entry can still be
		// cleared.
		if add && (strings.ContainsAny(ch, " \r\n\x00") || !utf8.ValidString(ch)) {
			continue
		}
		chans = append(chans, ch)
	}
	if len(chans) == 0 {
		return
	}
	if err := h.updateAutojoinLocked(ctx, ev.Network, chans, add, c.Fold); err != nil {
		log.Printf("irc[%s]: persist autojoin %s %d chans: %v", ev.Network, ev.Msg.Command, len(chans), err)
	}
}

// autojoinChange maps one live event to the comma-list of channels whose
// autojoin entries it changes and whether they are added ("" = no change).
// Our own JOIN adds, our own PART removes — and ERR_LINKCHANNEL (470,
// "<nick> <original> <target>": the server refused <original> and forwarded
// the join) removes the ORIGINAL name. join_channel persists the requested
// name before the server's verdict, and only the forward TARGET ever gets a
// JOIN echo — so without this the refused original stays stored forever, and
// every restart re-joins it, follows the forward, and resurrects a channel
// the user has since left (join #chat -> land in ##chat, part ##chat, restart
// brings ##chat back).
func autojoinChange(c Conn, ev irc.Event) (list string, add bool) {
	switch ev.Msg.Command {
	case "JOIN", "PART":
		if ev.Msg.Prefix == nil || c.Nick() == "" || c.Fold(ev.Msg.Prefix.Name) != c.Fold(c.Nick()) {
			return "", false // not our own membership change
		}
		return ev.Msg.Param(0), ev.Msg.Command == "JOIN"
	case "470": // ERR_LINKCHANNEL — addressed to our nick in param 0
		if c.Nick() == "" || c.Fold(ev.Msg.Param(0)) != c.Fold(c.Nick()) {
			return "", false
		}
		return ev.Msg.Param(1), false
	}
	return "", false
}

// adoptReplayedOwn handles a chathistory replay of one of our own messages
// (no echo-message, so persistOwn stored a no-msgid placeholder): it stamps
// the server's msgid onto the placeholder so the subsequent insert dedups
// against it instead of duplicating. It is called for every replayed
// PRIVMSG/NOTICE and identifies OUR messages purely by the placeholder match
// (buffer+sender+text) — never the current nick — so a since-then nick change
// can't defeat it; it is a no-op for anyone else's messages.
func (h *Hub) adoptReplayedOwn(ctx context.Context, c Conn, ev irc.Event, target string) {
	if ev.Msg.Command != "PRIVMSG" && ev.Msg.Command != "NOTICE" || ev.Msg.Prefix == nil {
		return
	}
	mid := ev.Msg.Tags["msgid"]
	if mid == "" {
		return
	}
	since := messageTime(ev).Add(-ownDedupWindow).UnixMilli()
	// The replay's prefix is the nick we sent under — the same sender persistOwn
	// stored on the placeholder — so the match holds across a later nick change.
	sender := ev.Msg.Prefix.Name
	own := store.OwnMsg{
		Network: ev.Network, Target: target, Sender: sender,
		Text: searchText(ev.Msg), MsgID: mid, SinceMs: since,
	}
	if _, aerr := h.store.AdoptOwnMsgID(ctx, own, c.Fold); aerr != nil {
		log.Printf("irc[%s]: own-message dedup for %q: %v", ev.Network, target, aerr)
	}
}

// replayTarget files a REPLAYED PRIVMSG/NOTICE under its batch target — the
// buffer the chathistory/event-playback batch is for — regardless of the nick
// we used when the message was sent. Non-ACTION CTCP is still dropped so a
// hostile replay can't spam buffers.
func replayTarget(m *ircv4.Message, batchTarget string, c Conn) (string, bool) {
	if body := m.Trailing(); strings.HasPrefix(body, "\x01") && !strings.HasPrefix(body, "\x01ACTION") {
		return "", false
	}
	t := stripStatusPrefix(batchTarget, c.StatusPrefixes(), c.IsChannel)
	if t == "" {
		return "", false
	}
	// NOTICEs route by the batch target exactly like PRIVMSG — no re-derivation
	// from the sender or our current nick. Per draft/chathistory, a TARGET
	// query replays only messages belonging to that target's conversation, so
	// every non-channel message in a query batch belongs to that query. Keying
	// on the sender vs our CURRENT nick (as an earlier revision did, mirroring
	// live noticeTarget) violated persistBuffer's nick-independence contract: a
	// replay of our own notice after a /nick or collision-fallback rename saw
	// fold(sender) != fold(current nick), rerouted to "*", and — msgid dedup
	// being per-buffer — duplicated the live copy as a misfiled lobby row.
	// Live routing is untouched; it never reaches here (persistBuffer only
	// calls replayTarget inside a chathistory batch).
	return t, true
}

// persistBuffer resolves the buffer an event persists into. During replay
// the buffer is the batch target, which is authoritative and independent of
// the nick we used when these messages were sent — re-deriving from the
// CURRENT nick (persistTarget → directTarget) would misfile PM history after
// a /nick or a reconnect collision-fallback changed our nick since. Live
// traffic routes by the current nick, correct in the moment.
func persistBuffer(ev irc.Event, c Conn, replay bool, batch *histBatch, queryOpen func(string) bool) (string, bool) {
	if replay && batch != nil && batch.target != "" &&
		(ev.Msg.Command == "PRIVMSG" || ev.Msg.Command == "NOTICE") {
		return replayTarget(ev.Msg, batch.target, c)
	}
	return persistTarget(ev.Msg, c.Nick(), c.IsChannel, c.Fold, c.StatusPrefixes(), queryOpen)
}

// persistEvent stores a message in its buffer and, when live, broadcasts
// it. Duplicate msgids (overlapping backfill) are silently dropped.
func (h *Hub) persistEvent(ctx context.Context, c Conn, ev irc.Event, replay bool, batch *histBatch) error {
	// A live append and its publication are one observable mutation. Serialize
	// them with close_buffer so the UI cannot see buffer_closed followed by an
	// older, already-committed event. Replay writes publish only a later
	// history_changed hint and remain protected by the store's atomic guard.
	if !replay {
		h.bufferMutationMu.Lock()
		defer h.bufferMutationMu.Unlock()
	}
	target, ok := persistBuffer(ev, c, replay, batch, h.queryOpenFn(ctx, ev.Network, c))
	if !ok {
		return nil
	}
	// A buffer just closed from the UI must not be re-created by traffic
	// that was already in flight (our own PART echo, or any straggler
	// line for the channel): within the grace window, append without
	// creation so a deleted buffer stays deleted. Otherwise resolve to
	// the canonical stored spelling and append atomically (AppendFolded)
	// — an echoed message can carry client-supplied casing, and #Go/#go
	// must not split into two buffers even under a concurrent send.
	own := ev.Msg.Prefix != nil && c.Nick() != "" && c.Fold(ev.Msg.Prefix.Name) == c.Fold(c.Nick())
	selfPart := own && ev.Msg.Command == "PART"
	// A LIVE self-JOIN reopens a channel: clear any close grace so its
	// buffer is re-created and live traffic flows again (otherwise a
	// rejoin within the 10s window would silently drop messages). A
	// REPLAYED (event-playback) self-JOIN carries no rejoin intent — it is
	// a straggler like any other backfill line — so it must NOT clear the
	// grace, or it would resurrect a buffer the user just closed.
	if own && !replay && ev.Msg.Command == "JOIN" {
		h.unmarkClosed(ev.Network, c.Fold(target))
	}
	// Dedup our own REPLAYED messages against the no-msgid placeholders
	// persistOwn stored: adoptReplayedOwn matches by (buffer, sender, text) —
	// the sender being the nick we sent under, recorded on both the placeholder
	// and the replay — so it survives a since-then nick change, unlike a
	// current-nick `own` check. Only meaningful on a no-echo-message server;
	// with echo-message the replay dedups by msgid instead. It is a no-op for
	// anyone else's messages (no placeholder matches their sender).
	if replay && !c.CapEnabled("echo-message") &&
		(ev.Msg.Command == "PRIVMSG" || ev.Msg.Command == "NOTICE") {
		h.adoptReplayedOwn(ctx, c, ev, target)
	}
	// graceGuard resolves this event's close-grace policy; AppendFoldedGuarded
	// evaluates it inside the store, atomically with the buffer-existence
	// check, so a straggler in flight when the user closes a buffer cannot
	// resurrect it (the recentlyClosed check and the append used to be a
	// check-then-act split across h.mu and store.mu — a two-lock TOCTOU).
	stored, stillArchived, err := h.store.AppendFoldedGuardedArchive(ctx, ev.Network, target, c.Fold,
		h.graceGuard(ev, c, target, selfPart, replay), unarchivePolicy(ev.Msg.Command, own, replay), storeMessage(ev))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("irc[%s]: persist %s to %q: %v", ev.Network, ev.Msg.Command, target, err)
		return nil
	}
	if stored.ID == 0 {
		// Duplicate msgid (already stored and announced) or an at-cap
		// buffer-quota refusal (logged by the store).
		return nil
	}
	if stillArchived {
		// Persisted into a buffer that stays archived: publish nothing — an
		// event or history hint would re-grow a sidebar row that get_buffers
		// hides. The line is in history for whenever the buffer resurfaces.
		return nil
	}
	h.publishPersisted(stored, target, replay, batch)
	h.maybePushCandidate(c, ev, stored, target, replay, own)
	return nil
}

// queryOpenFn builds the lazy queryOpen predicate persistBuffer needs: it
// reports whether a NON-channel query buffer with the given nick is already
// open, so an incoming private NOTICE from a service or bot files under that
// conversation instead of the lobby (see noticeTarget). Evaluated lazily —
// only noticeTarget's addressed-to-us branch calls it, so channel traffic and
// PMs never pay for the buffer scan. A lookup error yields false (fall back
// to the lobby), never a misroute.
func (h *Hub) queryOpenFn(ctx context.Context, network string, c Conn) func(string) bool {
	return func(nick string) bool {
		name, found, err := h.store.FindBuffer(ctx, network, nick, c.Fold)
		return err == nil && found && !c.IsChannel(name)
	}
}

// unarchivePolicy decides whether persisting this event resurfaces an
// archived (close_buffer purge:false) buffer: real LIVE conversation
// (PRIVMSG/NOTICE/TOPIC/...) resurfaces a hidden buffer so its history
// returns on new activity; membership fan-out must not — the PART echo
// that races a purge:false close lands after the archive and would
// otherwise resurrect the buffer (the mirror of the deleted path's close
// tombstone). KICK is presence traffic like the rest (it renders as a
// system row and never bumps unread — see Buffers' classification) and
// stays hidden too. REPLAYED content never unarchives: a chathistory page
// still in flight when the user archives is backfill, not new
// conversation. Our own LIVE JOIN is a deliberate rejoin and does
// resurface it — the same intent rule as persistEvent's unmarkClosed; a
// REPLAYED self-JOIN is backfill, not intent.
func unarchivePolicy(command string, own, replay bool) bool {
	switch command {
	case "JOIN":
		return own && !replay
	case "PART", "QUIT", "NICK", "MODE", "KICK":
		return false
	}
	return !replay
}

// publishPersisted announces a freshly stored, non-archived event: live
// events broadcast to every session; a replayed event rerouted to "*" (a
// correspondent's private NOTICE) instead flags its batch — the batch-close
// hint names only batch.target, so "*" needs the flag for its own
// history_changed or the client's cached "*" list never refreshes.
func (h *Hub) publishPersisted(stored store.Message, target string, replay bool, batch *histBatch) {
	if !replay {
		h.broadcast(envelope("event", 0, eventData(stored)))
		return
	}
	if batch != nil && isServerBuffer(target) {
		batch.starTouched = true
	}
}

// graceGuard returns the buffer-creation/close-grace predicate for one event.
// The guard IGNORES the exists flag so it also DROPS a straggler landing in
// the window after markClosed but before DeleteBuffer — otherwise that
// straggler appends and broadcasts a live event that re-creates the buffer on
// clients (and races the buffer_closed push).
func (h *Hub) graceGuard(ev irc.Event, c Conn, target string, selfPart, replay bool) func(bool) bool {
	folded := c.Fold(target)
	if selfPart {
		// Our own PART echo must never create a buffer, but must still find the
		// existing one under casemapping (target is already folded by the
		// store). A PART echo is a straggler too: drop it within the close
		// grace so it cannot broadcast a live event that resurrects the buffer.
		return func(exists bool) bool { return !exists || h.recentlyClosed(ev.Network, folded) }
	}
	// For LIVE traffic the grace is CHANNEL-only: channels have stragglers
	// (QUIT/NICK, our own PART echo, late lines) that must not reopen a
	// just-closed buffer, but a live PM is a NEW conversation and must reopen
	// its query rather than be dropped for the 10s window. REPLAYED
	// (event-playback / chathistory) traffic is always a straggler, including a
	// backfilled PM: it must never undo a close, so the grace applies then too.
	return func(bool) bool {
		return (replay || c.IsChannel(target)) && h.recentlyClosed(ev.Network, folded)
	}
}

// backfill requests missed history for the network's non-channel (query)
// buffers after (re)registration; channels backfill on their own JOIN
// echo instead. The store's newest timestamp per buffer is the resume
// point; buffers with no stored history have nothing to resume from.
// startMonitor re-establishes the persisted MONITOR buddy list on the
// network after (re)registration; the server's 730/731 replies then seed
// presence.
func (h *Hub) startMonitor(ctx context.Context, c Conn) {
	// Snapshot + send under monMu, the same lock handleMonitor's mutations
	// hold across their store-write + wire-send. Unserialized, a concurrent
	// add/remove could interleave with the restoration's MONITOR C + list
	// (e.g. "+bob, C, +stale-list"), leaving SQLite and the server's monitor
	// set disagreeing until the next reconnect.
	h.monMu.Lock()
	defer h.monMu.Unlock()
	nicks, err := h.store.Monitors(ctx, c.Name())
	if err != nil {
		log.Printf("irc[%s]: monitor load: %v", c.Name(), err)
		return
	}
	// Reconcile even for an empty list — the manager's active set is empty on a
	// fresh connection, so an empty desired list is simply a no-op, and a
	// non-empty one establishes the buddies. (No MONITOR C needed: the server's
	// list starts empty on every new connection.)
	if err := c.ReconcileMonitored(nicks); err != nil {
		log.Printf("irc[%s]: monitor restore: %v", c.Name(), err)
	}
}

// updatePresence applies a 730/731 reply (comma-separated targets, which
// may carry nick!user@host masks) and pushes each changed nick.
// Bounds on server-fed hub maps: a malicious server (or a plaintext
// MITM) can stream numerics with endlessly varying nicks/refs, so these
// per-connection maps are capped the way the manager caps its own
// server-fed structures (multiline batches, line length). Legitimate
// use stays far below these.
const (
	maxOpenWhois        = 64               // concurrent WHOIS accumulations
	maxOpenHistBatches  = 256              // concurrent chathistory replay batches
	maxBackfillTargets  = 4096             // distinct targets tracked for paginated backfill
	closeGraceMs        = 10_000           // buffer stays closed against stragglers this long
	maxRecentCloses     = 1024             // authenticated close flood bound
	maxRecentCloseBytes = 1 << 20          // keys retained by recentClose
	ownDedupWindow      = 10 * time.Minute // reconcile a replayed own message with its local placeholder
)

func (h *Hub) updatePresence(ctx context.Context, c Conn, network string, m *ircv4.Message, online bool) {
	// 730/731 report the target's CURRENT-nick casing, which can differ from
	// the configured spelling. Resolve each back to the configured nick via
	// CASEMAPPING so presence keys match the buddy list monitorList/the client
	// render (an unfolded key made an online buddy show perpetually offline).
	configured, err := h.store.Monitors(ctx, network)
	if err != nil {
		return
	}
	byFold := make(map[string]string, len(configured))
	for _, nk := range configured {
		byFold[c.Fold(nk)] = nk
	}
	for _, target := range strings.Split(m.Trailing(), ",") {
		nick := strings.TrimSpace(target)
		if i := strings.IndexByte(nick, '!'); i != -1 {
			nick = nick[:i]
		}
		key, ok := byFold[c.Fold(nick)]
		if nick == "" || !ok {
			continue // empty, or a nick we don't monitor — nothing to update
		}
		h.mu.Lock()
		p := h.presence[network]
		if p == nil {
			p = make(map[string]bool)
			h.presence[network] = p
		}
		p[key] = online
		h.mu.Unlock()
		h.broadcast(envelope("presence", 0, PresenceData{Network: network, Nick: key, Online: online}))
	}
}

func (h *Hub) clearPresence(network string) {
	h.mu.Lock()
	delete(h.presence, network)
	h.mu.Unlock()
}

// removePresence forgets one nick's cached presence — on monitor removal, so
// a re-add doesn't resurface the stale state via monitorList before a fresh
// 730/731 arrives.
func (h *Hub) removePresence(network, nick string) {
	h.mu.Lock()
	if p := h.presence[network]; p != nil {
		delete(p, nick)
	}
	h.mu.Unlock()
}

// monitorList returns the persisted buddy list for a network with each
// nick's last-known presence (false when unknown).
func (h *Hub) monitorList(ctx context.Context, network string) ([]MonitorEntry, error) {
	nicks, err := h.store.Monitors(ctx, network)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	pres := h.presence[network]
	out := make([]MonitorEntry, len(nicks))
	for i, nick := range nicks {
		out[i] = MonitorEntry{Nick: nick, Online: pres[nick]}
	}
	h.mu.Unlock()
	return out, nil
}

func (h *Hub) backfill(ctx context.Context, c Conn) {
	infos, err := h.store.Buffers(ctx)
	if err != nil {
		log.Printf("irc[%s]: backfill: %v", c.Name(), err)
		return
	}
	markers := c.CapEnabled("draft/read-marker")
	for _, b := range infos {
		if b.Network != c.Name() || c.IsChannel(b.Target) || isServerBuffer(b.Target) {
			// Channels resume on their own JOIN echo (+ MARKREAD from the
			// server); the synthetic server buffer is local-only and must never
			// emit CHATHISTORY */MARKREAD * upstream.
			continue
		}
		if b.LastTS > 0 {
			_, msgid := h.lastStored(ctx, b.Network, b.Target)
			c.RequestChatHistory(b.Target, b.LastTS, msgid)
		}
		if markers {
			// Fetch the account's read position for the query buffer;
			// the reply flows back through applyUpstreamMarker.
			_ = c.Send(&ircv4.Message{Command: "MARKREAD", Params: []string{b.Target}})
		}
	}
}

// lastStored returns the newest stored message's timestamp (unix ms) and
// msgid for one buffer; (0, "") when the buffer has no history.
func (h *Hub) lastStored(ctx context.Context, network, target string) (int64, string) {
	msgs, err := h.store.Latest(ctx, network, target, 1)
	if err != nil || len(msgs) == 0 {
		return 0, ""
	}
	return msgs[0].Time.UnixMilli(), msgs[0].MsgID
}

// applyUpstreamMarker folds a draft/read-marker MARKREAD line (another
// client of our account read something, or a reply to our get/set) into
// the store and tells sessions. The store clamps regressions, so the
// pushed value is always the newest known position.
func (h *Hub) applyUpstreamMarker(ctx context.Context, c Conn, ev irc.Event) {
	target := ev.Msg.Param(0)
	sel := ev.Msg.Param(1)
	v, ok := strings.CutPrefix(sel, "timestamp=")
	if target == "" || !ok { // includes the "*" no-marker reply
		return
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return
	}
	target = h.store.CanonicalBuffer(ctx, ev.Network, target, c.Fold)
	if err := h.store.SetReadMarker(ctx, ev.Network, target, t); err != nil {
		log.Printf("irc[%s]: upstream read marker for %q: %v", ev.Network, target, err)
		return
	}
	authoritative, err := h.store.ReadMarker(ctx, ev.Network, target)
	if err != nil {
		return
	}
	h.broadcast(envelope("read_marker", 0, MarkerData{
		Network: ev.Network, Buffer: target, Time: markerMillis(authoritative),
	}))
	h.notifyMarkerAdvance(ev.Network, target, authoritative)
}

// histBatch is the replay progress of one open chathistory batch.
type histBatch struct {
	target string
	count  int
	lastTS int64  // unix ms of the batch's newest message
	lastID string // its msgid, "" when it has none
	// starTouched records that a replayed event in this batch was rerouted to
	// the server buffer "*" (a correspondent's private NOTICE, or a redaction
	// of one). The batch-close hint names only `target`, so "*" needs its own
	// history_changed or the client's cached "*" list is never invalidated.
	starTouched bool
}

// maxBackfillPages bounds paginated backfill per target per connection:
// at the default 100-message pages a reconnect catches up on 1000
// messages, the hot-scrollback scale. Older remainders stay reachable by
// explicit history paging.
const maxBackfillPages = 10

// serverInfoNumerics are the reply numerics of user-issued informational
// commands (WHOIS/WHOWAS/WHO/LIST/AWAY/INVITE/MODE queries), forwarded
// to clients as ephemeral "server_info" lines. Connect-time floods
// (ISUPPORT, LUSERS) stay out; MOTD numerics are gated separately since
// servers also send them unsolicited at every registration.
var serverInfoNumerics = map[string]bool{
	"301": true, "305": true, "306": true, "307": true, // away status
	"314": true, "369": true, // WHOWAS
	"352": true,                           // WHO (354/315 are our own WHOX traffic — roster data)
	"321": true, "322": true, "323": true, // LIST
	"324": true, "329": true, "367": true, "368": true, // MODE queries
	"341": true, // INVITE ack
}

// serverInfo decides whether an event should be pushed as an ephemeral
// "server_info" line, and formats it: the leading nick parameter is
// dropped, the rest joined. Error numerics (4xx/5xx) always qualify —
// "401 no such nick" after a /whois would otherwise vanish. WHOIS
// replies are handled separately (accumulateWhois -> a "whois" card).
func (h *Hub) serverInfo(ev irc.Event) (ServerInfoData, bool) {
	cmd := ev.Msg.Command
	n, err := strconv.Atoi(cmd)
	if err != nil {
		return ServerInfoData{}, false
	}
	switch {
	case cmd == "375" || cmd == "372" || cmd == "376" || cmd == "422":
		if !h.motdExpected(ev.Network, cmd) {
			return ServerInfoData{}, false
		}
	case cmd == "402":
		// ERR_NOSUCHSERVER is the TERMINAL reply of a targeted "MOTD <server>"
		// naming a nonexistent server — no 376/422 will follow, so it must
		// consume that request's gate or it stays armed until disconnect,
		// forwarding a later unsolicited MOTD as if requested. Consumption is
		// CORRELATED by the named server (params: <nick> <server> <text>): a
		// 402 from an unrelated command (/whois via a bad server) matches no
		// pending target and consumes nothing. It then falls through to the
		// generic error-numeric forwarding below.
		if len(ev.Msg.Params) > 1 {
			h.consumeMOTDTarget(ev.Network, ev.Msg.Params[1])
		}
	case serverInfoNumerics[cmd]:
	case n >= 400 && n < 600:
	default:
		return ServerInfoData{}, false
	}
	params := ev.Msg.Params
	if len(params) > 1 {
		params = params[1:] // our own nick
	}
	txt := strings.TrimSpace(strings.Join(params, " "))
	if txt == "" {
		return ServerInfoData{}, false
	}
	return ServerInfoData{Text: clampServerInfo(txt)}, true
}

// maxServerInfoBytes bounds an ephemeral server_info line. These are NOT
// stored (so they skip the store's clamp), but the browser keeps thousands of
// them in a buffer, so a hostile server streaming near-LINELEN 4xx/5xx
// numerics could bloat client memory. Real MOTD/error/notice lines are far
// under this.
const maxServerInfoBytes = 2048

// clampServerInfo truncates s to maxServerInfoBytes, trimming a trailing
// partial rune so the result stays valid UTF-8.
func clampServerInfo(s string) string {
	if len(s) <= maxServerInfoBytes {
		return s
	}
	s = s[:maxServerInfoBytes]
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size != 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// maxBatchRefBytes bounds a BATCH reference used as a histBatches key. It MUST
// stay equal to internal/irc's clampBatchRef limit (also 512) AND reject the
// SAME way: the manager keys its own replay-suppression map by that rule, and
// any disagreement lets a ref be replay to one layer but live to the other —
// splitting hub vs manager state. Real refs are short.
const maxBatchRefBytes = 512

// clampBatchRef returns the histBatches key for a batch reference: a short ref
// (≤512) verbatim, an over-limit ref HASHED to a bounded "\x00"+sha256 key
// (never truncated — that aliases distinct refs — nor rejected — that turned a
// long-ref batch into live traffic). See internal/irc.clampBatchRef; must match
// it byte-for-byte so the two layers agree.
func clampBatchRef(s string) string {
	if len(s) <= maxBatchRefBytes {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return "\x00" + hex.EncodeToString(sum[:])
}

// accumulateWhois collects the WHOIS reply numerics into one card, keyed
// by the CONNECTION-FOLDED queried nick (param 1), and flushes it as a
// "whois" push on 318 (end of /WHOIS). The fold matters: real servers
// (Ergo among them) send the detail numerics with the target's CANONICAL
// spelling but echo the CLIENT'S requested spelling in 318 — /whois ALICE
// for canonical Alice would otherwise accumulate whois["Alice"] and try
// to flush whois["ALICE"], stranding the card until re-registration.
// Returns true when it consumed the message — so whois numerics never
// reach serverInfo. 301 (away) is folded in only while a whois for that
// nick is in progress; a standalone away notice falls through to
// serverInfo. Whois state is per connection (reset on registration in
// Run).
func (h *Hub) accumulateWhois(c Conn, ev irc.Event, whois map[string]*WhoisData) bool {
	m := ev.Msg
	switch m.Command {
	case "311", "312", "313", "317", "319", "330", "335", "338", "378", "379", "671":
		// Clamp+detach BEFORE folding: the fold's output becomes a retained
		// map key, and the card's Nick keeps the wire spelling — neither may
		// pin a hostile ~64 KiB parsed line for the card's life.
		nick := clampDetach(m.Param(1))
		if nick == "" {
			return true
		}
		key := c.Fold(nick)
		w := whois[key]
		if w == nil {
			if len(whois) >= maxOpenWhois {
				return true // consumed but not tracked: bound the map
			}
			w = &WhoisData{Nick: nick}
			whois[key] = w
		}
		applyWhois(w, m) // detaches each field it sets — see the note there
		return true
	case "301": // away — part of a whois only if one is open
		if w := whois[c.Fold(clampServerInfo(m.Param(1)))]; w != nil {
			w.Away = clampDetach(m.Trailing())
			return true
		}
		return false
	case "318": // end of /WHOIS: flush the card
		key := c.Fold(clampServerInfo(m.Param(1))) // folded like the stored key
		if w := whois[key]; w != nil {
			delete(whois, key)
			w.Network = ev.Network
			clampWhois(w) // idempotent re-clamp before broadcast
			h.broadcast(envelope("whois", 0, w))
		}
		return true
	}
	return false
}

// clampDetach bounds s to maxServerInfoBytes AND copies it into a fresh
// allocation, so a short result no longer pins the ~64 KiB parsed-line buffer it
// was sliced from. Use it wherever a server-controlled string is RETAINED (map
// keys, in-progress cards); clampServerInfo alone shares the backing array,
// which is fine only for values broadcast and immediately dropped.
func clampDetach(s string) string {
	return strings.Clone(clampServerInfo(s))
}

// clampWhois bounds AND detaches every server-controlled field of a whois card.
// applyWhois already clamps+detaches each field as it sets it (so a resident,
// never-flushed card pins nothing), making this a cheap once-per-WHOIS safety
// net at flush that also re-bounds the appended 319 channel list.
func clampWhois(w *WhoisData) {
	w.Nick = clampDetach(w.Nick)
	w.User = clampDetach(w.User)
	w.Host = clampDetach(w.Host)
	w.Realname = clampDetach(w.Realname)
	w.Server = clampDetach(w.Server)
	w.Channels = clampDetach(w.Channels)
	w.Account = clampDetach(w.Account)
	w.Actual = clampDetach(w.Actual)
	w.Away = clampDetach(w.Away)
}

// applyWhois sets the field(s) one WHOIS numeric carries, bounding AND detaching
// each server-controlled string as it stores it (clampDetach) so a resident card
// pins no ~64 KiB parsed line even if the server never sends the 318 terminator.
// Integers and bools need no detach.
func applyWhois(w *WhoisData, m *ircv4.Message) {
	switch m.Command {
	case "311": // <me> <nick> <user> <host> * :<realname>
		w.User, w.Host, w.Realname = clampDetach(m.Param(2)), clampDetach(m.Param(3)), clampDetach(m.Trailing())
	case "312": // <me> <nick> <server> :<info>
		w.Server = clampDetach(m.Param(2))
	case "313": // is an IRC operator
		w.Operator = true
	case "317": // <me> <nick> <idle> [<signon>] :seconds idle, signon time
		w.Idle = atoiOr0(m.Param(2))
		w.Signon = atoiOr0(m.Param(3))
	case "319": // :<prefixed channels> — a heavily-joined user's list spans
		// MULTIPLE 319 replies; APPEND (bounded) rather than overwrite, or only
		// the last chunk survives. clampDetach, not clampServerInfo: when the
		// clamp truncates, a plain slice would share the oversized concat's
		// backing array — up to ~66 KiB pinned per card (64 cards max) until
		// the 318 flush a hostile server can simply withhold.
		w.Channels = clampDetach(strings.TrimSpace(w.Channels + " " + m.Trailing()))
	case "330": // <me> <nick> <account> :is logged in as
		w.Account = clampDetach(m.Param(2))
	case "335": // is a bot
		w.Bot = true
	case "338": // <me> <nick> <host/ip> [...] :actually using host
		// The IP/host is in the middle params; the trailing is a label.
		if mid := strings.Join(midParams(m, 2), " "); mid != "" && w.Actual == "" {
			w.Actual = clampDetach(mid)
		}
	case "378": // <me> <nick> :is connecting from <user@host> <ip>
		if w.Actual == "" {
			w.Actual = clampDetach(strings.TrimPrefix(m.Trailing(), "is connecting from "))
		}
	case "671": // is using a secure connection
		w.Secure = true
	}
}

func atoiOr0(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// midParams returns the parameters from index `from` up to (excluding)
// the trailing parameter — the fixed args of a numeric, minus its
// human-readable label.
func midParams(m *ircv4.Message, from int) []string {
	end := len(m.Params) - 1 // exclude trailing
	if end <= from {
		return nil
	}
	return m.Params[from:end]
}

// expectMOTD opens the MOTD gate for a network: the next MOTD reply is
// user-requested and should be shown. The gate is a QUEUE of outstanding
// request targets ("" for a plain /motd) — two concurrent requests each arm
// it, one's rollback must not retract the other's, and a 402 terminal error
// consumes only an entry whose target it actually names. motdExpected
// consumes one entry at each end-of-MOTD numeric, and a disconnect clears the
// queue outright (clearMOTD): the owning connection can no longer reply.
//
// Correlation is deliberately BEST-EFFORT: MOTD numerics carry no request
// association (no label), so a plain-/motd reply racing the tail of a
// registration burst can still be mis-attributed. Full correlation needs
// labeled-response; this queue just keeps the common cases (unrelated 402s,
// concurrent requests) from consuming the wrong gate.
// maxPendingMOTD caps the queue: a server that never terminates its replies
// (no 376/422/402) would otherwise let repeated /motd grow it forever. At the
// cap the OLDEST entry is dropped — it belongs to the request most likely
// already orphaned. (The complete fix for correlation AND leakage here is
// labeled-response on the outgoing MOTD; until then this stays best-effort.)
const maxPendingMOTD = 8

func (h *Hub) expectMOTD(network, target string) {
	h.mu.Lock()
	q := append(h.motdWanted[network], target)
	if len(q) > maxPendingMOTD {
		q = q[len(q)-maxPendingMOTD:]
	}
	h.motdWanted[network] = q
	h.mu.Unlock()
}

// retractMOTD rolls back ONE expectMOTD whose command never reached the send
// queue (the send failed) — the most recently armed entry for that target.
// Without the rollback, the stale gate would expose the next UNSOLICITED MOTD
// burst — e.g. a reconnect's — as if requested.
func (h *Hub) retractMOTD(network, target string) {
	h.mu.Lock()
	q := h.motdWanted[network]
	for i := len(q) - 1; i >= 0; i-- {
		if q[i] == target {
			q = append(q[:i], q[i+1:]...)
			break
		}
	}
	if len(q) == 0 {
		delete(h.motdWanted, network)
	} else {
		h.motdWanted[network] = q
	}
	h.mu.Unlock()
}

// clearMOTD drops every armed gate for the network — called on disconnect,
// which orphans all outstanding requests at once.
func (h *Hub) clearMOTD(network string) {
	h.mu.Lock()
	delete(h.motdWanted, network)
	h.mu.Unlock()
}

// consumeMOTDTarget consumes an armed gate whose target matches a 402's
// named server, case-insensitively (servers case-fold names). It does NOT
// touch untargeted ("" ) entries: an unrelated 402 (a /whois against a bad
// server, say) must not eat a pending plain /motd's gate.
func (h *Hub) consumeMOTDTarget(network, target string) {
	if target == "" {
		return
	}
	h.mu.Lock()
	q := h.motdWanted[network]
	for i, tgt := range q {
		if tgt != "" && strings.EqualFold(tgt, target) {
			q = append(q[:i], q[i+1:]...)
			break
		}
	}
	if len(q) == 0 {
		delete(h.motdWanted, network)
	} else {
		h.motdWanted[network] = q
	}
	h.mu.Unlock()
}

func (h *Hub) motdExpected(network, cmd string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	q := h.motdWanted[network]
	want := len(q) > 0
	if cmd == "376" || cmd == "422" { // RPL_ENDOFMOTD / ERR_NOMOTD
		if len(q) > 1 {
			h.motdWanted[network] = q[1:]
		} else {
			delete(h.motdWanted, network)
		}
	}
	return want
}

// trackHistoryBatch follows BATCH open/close for chathistory replays.
// When a replay finishes it announces the affected buffer (so clients
// drop stale pages and refetch) and — if the batch filled a whole page,
// meaning the gap may extend past it — requests the next page anchored
// at the batch's own newest message.
func (h *Hub) trackHistoryBatch(ctx context.Context, ev irc.Event, c Conn, batches map[string]*histBatch, pages map[string]int) {
	if len(ev.Msg.Params) == 0 {
		return
	}
	ref := ev.Msg.Params[0]
	switch {
	case strings.HasPrefix(ref, "+") && ev.Msg.Param(1) == "chathistory":
		openHistoryBatch(ev, ref[1:], batches)
	case strings.HasPrefix(ref, "-"):
		h.closeHistoryBatch(ctx, ev, c, clampBatchRef(ref[1:]), batches, pages)
	}
}

// openHistoryBatch records an opened chathistory BATCH so its replayed lines
// are classified as history, not live. The ref KEY is clamped with clampBatchRef
// (512, matching the manager's replay-suppression key so both layers agree) and
// cloned to detach it from the ~64 KiB BATCH line; the target is clamped+detached
// separately (2048 — it seeds the folded backfillPages key, bounded by count not
// length). The open map is bounded.
func openHistoryBatch(ev irc.Event, ref string, batches map[string]*histBatch) {
	key := clampBatchRef(ref)
	if key == "" {
		return // empty OR over-limit reference: ignore (never store "" as a key)
	}
	if len(batches) >= maxOpenHistBatches {
		return // bound the map at maxOpenHistBatches
	}
	batches[strings.Clone(key)] = &histBatch{target: clampDetach(ev.Msg.Param(2))}
}

// closeHistoryBatch finishes a chathistory replay: it announces the affected
// buffer (so clients drop stale pages and refetch) and requests the next page
// when the batch filled a whole page (the gap may extend past it).
func (h *Hub) closeHistoryBatch(ctx context.Context, ev irc.Event, c Conn, id string, batches map[string]*histBatch, pages map[string]int) {
	b := batches[id]
	if b == nil {
		return
	}
	delete(batches, id)
	key := c.Fold(b.target)
	// Cap the number of distinct targets tracked, not just the pages per
	// target: a hostile server can close a chathistory batch for an
	// endlessly varying target and grow this map for the life of the
	// connection otherwise (mirrors the presence/whois key caps above).
	_, known := pages[key]
	if !known && len(pages) >= maxBackfillTargets {
		// Drop the follow-up rather than track a new target.
	} else if b.count >= c.HistoryPageSize() && b.lastTS > 0 && pages[key] < maxBackfillPages {
		pages[key]++
		c.RequestChatHistory(b.target, b.lastTS, b.lastID)
	}
	// Announce the CANONICAL stored spelling: clients key buffers by it, so a
	// wire-spelling hint whose casing differs would miss the client's page-
	// invalidation and leave stale scrollback.
	h.broadcast(envelope("history_changed", 0, HistoryChangedData{
		Network: ev.Network, Buffer: h.store.CanonicalBuffer(ctx, ev.Network, b.target, c.Fold),
	}))
	// If a replayed event in this batch was rerouted to the server buffer (a
	// correspondent's private NOTICE or its redaction), "*" changed too but the
	// hint above named only b.target — invalidate "*" as well.
	if b.starTouched {
		h.broadcast(envelope("history_changed", 0, HistoryChangedData{
			Network: ev.Network, Buffer: serverBufferTarget,
		}))
	}
}

// markClosed records that a buffer was just closed, so in-flight inbound
// traffic cannot resurrect it for closeGraceMs. Expired entries are
// pruned here (closes are rare user actions).
func (h *Hub) markClosed(network, foldedBuffer string, nowMs int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, t := range h.recentClose {
		if nowMs-t > closeGraceMs {
			h.deleteRecentCloseLocked(k)
		}
	}
	key := network + "\x00" + foldedBuffer
	if _, exists := h.recentClose[key]; !exists {
		h.recentCloseBytes += len(key)
	}
	h.recentClose[key] = nowMs
	for len(h.recentClose) > maxRecentCloses || h.recentCloseBytes > maxRecentCloseBytes {
		var oldestKey string
		var oldest int64
		for k, t := range h.recentClose {
			if oldestKey == "" || t < oldest {
				oldestKey, oldest = k, t
			}
		}
		if oldestKey == "" {
			break
		}
		h.deleteRecentCloseLocked(oldestKey)
	}
}

func (h *Hub) deleteRecentCloseLocked(key string) {
	if _, ok := h.recentClose[key]; ok {
		delete(h.recentClose, key)
		h.recentCloseBytes -= len(key)
	}
}

// unmarkClosed clears a buffer's close grace (e.g. on a self-JOIN
// reopen) so subsequent traffic re-creates it normally.
func (h *Hub) unmarkClosed(network, foldedBuffer string) {
	h.mu.Lock()
	h.deleteRecentCloseLocked(network + "\x00" + foldedBuffer)
	h.mu.Unlock()
}

// recentlyClosed reports whether the folded buffer was closed within the
// grace window.
func (h *Hub) recentlyClosed(network, foldedBuffer string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.recentClose[network+"\x00"+foldedBuffer]
	if ok && time.Now().UnixMilli()-t > closeGraceMs {
		h.deleteRecentCloseLocked(network + "\x00" + foldedBuffer)
		return false
	}
	return ok
}

func (h *Hub) network(name string) Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.networks[name]
}

func (h *Hub) broadcast(env Envelope) {
	frame, err := json.Marshal(env)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.sessions {
		s.pushFrame(frame)
	}
}

// broadcastExcept sends to every session but the originator, which gets
// its own seq-tagged response instead.
func (h *Hub) broadcastExcept(except *Session, env Envelope) {
	frame, err := json.Marshal(env)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.sessions {
		if s != except {
			s.pushFrame(frame)
		}
	}
}

// relayTyping turns an incoming TAGMSG carrying the +typing client tag
// into a "typing" push. Our own echoed TAGMSGs are ignored — the local
// client knows what it is typing.
func (h *Hub) relayTyping(ctx context.Context, ev irc.Event, c Conn) {
	state := ev.Msg.Tags["+typing"]
	if state != "active" && state != "paused" && state != "done" {
		return
	}
	sender := ""
	if ev.Msg.Prefix != nil {
		sender = ev.Msg.Prefix.Name
	}
	nick := c.Nick()
	if sender == "" || nick == "" || c.Fold(sender) == c.Fold(nick) {
		return
	}
	// Strip any STATUSMSG prefix before the channel test so a TAGMSG to
	// "@#chan" is shown in "#chan" (where its messages are filed), not dropped.
	buffer := stripStatusPrefix(ev.Msg.Param(0), c.StatusPrefixes(), c.IsChannel)
	if !c.IsChannel(buffer) {
		// A typing notice addressed to us belongs in the sender's query.
		if c.Fold(buffer) != c.Fold(nick) {
			return
		}
		buffer = sender
	}
	// Broadcast the CANONICAL stored spelling — clients key typing state by the
	// exact buffer string (no fold), so the wire spelling of a case-variant
	// query ("Bob" vs stored "bob") would never match and the indicator would
	// silently never render.
	buffer = h.store.CanonicalBuffer(ctx, ev.Network, buffer, c.Fold)
	h.broadcast(envelope("typing", 0, TypingData{
		Network: ev.Network, Buffer: buffer, Nick: sender, State: state,
	}))
}

// membersHint reports whether a message changes channel state clients may
// be displaying, and which buffer it affects ("" = anywhere on the
// network: QUIT and NICK span channels the hub doesn't track).
func membersHint(m *ircv4.Message) (buffer string, affected bool) {
	switch m.Command {
	case "JOIN", "PART", "KICK", "TOPIC", "MODE":
		return m.Param(0), true
	case "366": // end of NAMES: <me> <channel>
		return m.Param(1), true
	case "315": // end of WHO: away/account discovery finished (WHOX)
		return m.Param(1), true
	case "QUIT", "NICK", "AWAY", "ACCOUNT", "CHGHOST": // span channels the hub doesn't track
		return "", true
	}
	return "", false
}

func newPrivmsg(target, text string) *ircv4.Message {
	return &ircv4.Message{Command: "PRIVMSG", Params: []string{target, text}}
}

// redactionBuffer resolves the buffer a REDACT applies to. During replay the
// batch target is authoritative and nick-independent (a replayed redaction
// of a PM would otherwise miss after a since-then nick change). Live: a
// redaction addressed to our nick files under the other party's buffer,
// mirroring persistTarget.
func redactionBuffer(ev irc.Event, c Conn, target string, replay bool, batch *histBatch) (buffer string, addressedToUs bool) {
	if replay && batch != nil && batch.target != "" {
		return batch.target, false
	}
	addressedToUs = !c.IsChannel(target) && ev.Msg.Prefix != nil && c.Fold(target) == c.Fold(c.Nick())
	if addressedToUs {
		return ev.Msg.Prefix.Name, true
	}
	return target, false
}

// applyRedaction handles an incoming REDACT (draft/message-redaction):
// ":nick REDACT <target> <msgid> [:reason]". It marks the referenced
// message deleted in the store and, unless this is history replay,
// announces it so loaded clients tombstone the message live.
func (h *Hub) applyRedaction(ctx context.Context, ev irc.Event, c Conn, replay bool, batch *histBatch) {
	target := ev.Msg.Param(0)
	msgid := ev.Msg.Param(1)
	if target == "" || msgid == "" {
		return
	}
	reason := ""
	if len(ev.Msg.Params) > 2 {
		reason = ev.Msg.Trailing()
	}
	buffer, addressedToUs := redactionBuffer(ev, c, target, replay, batch)
	buffer = h.store.CanonicalBuffer(ctx, ev.Network, buffer, c.Fold)
	// A private NOTICE addressed to us is filed in the server buffer (see
	// noticeTarget / replayTarget), not the sender's query — so a redaction
	// that misses should retry "*". Live: only when addressed to us. Replay:
	// whenever a non-channel (query) target missed, since replayTarget files
	// the correspondent's notice in "*" and the REDACT's batch target is the
	// query buffer.
	retryStar := addressedToUs || (replay && !c.IsChannel(target))
	// starBatch is the batch to flag when a REPLAYED scrub lands in "*" (nil on
	// the live path — a live scrub never touches batch state).
	var starBatch *histBatch
	if replay {
		starBatch = batch
	}
	ok, buffer := h.scrubRedaction(ctx, ev, buffer, msgid, reason, retryStar, starBatch)
	if !ok || replay {
		return
	}
	by := ""
	if ev.Msg.Prefix != nil {
		by = ev.Msg.Prefix.Name
	}
	// Clamp what goes on the wire: the stored copy is bounded inside SetRedacted,
	// but this LIVE push carries the server-controlled reason (and redactor name)
	// verbatim, which the client retains as a tombstone. Match the replay path,
	// which serves the already-clamped stored reason.
	h.broadcast(envelope("redact", 0, RedactData{
		// Clamp the msgid to the stored form (store.ClampMsgID): the message was
		// indexed under the truncated id, so an unclamped >512-byte id here would
		// never match its tombstone on the client.
		Network: ev.Network, Buffer: buffer, MsgID: store.ClampMsgID(msgid),
		By: clampServerInfo(by), Reason: clampServerInfo(reason),
	}))
}

// scrubRedaction marks the message deleted in the store, retrying the server
// buffer "*" when the first attempt misses and retryStar is set (a private
// NOTICE's redaction). Returns whether a row was scrubbed and the buffer it
// landed in; when starBatch is non-nil (a replay), flags its starTouched if
// the scrub landed in "*".
func (h *Hub) scrubRedaction(ctx context.Context, ev irc.Event, buffer, msgid, reason string, retryStar bool, starBatch *histBatch) (bool, string) {
	// %.100q, not %s: msgid is server-controlled and IRC tag escapes can
	// smuggle control characters into it — clamp and quote it like every
	// other server-derived log field.
	ok, err := h.store.SetRedacted(ctx, ev.Network, buffer, msgid, reason)
	if err != nil {
		log.Printf("irc[%s]: redact %.100q in %q: %v", ev.Network, msgid, buffer, err)
		return false, buffer
	}
	if ok || !retryStar {
		return ok, buffer
	}
	buffer = serverBufferTarget
	ok, err = h.store.SetRedacted(ctx, ev.Network, buffer, msgid, reason)
	if err != nil {
		log.Printf("irc[%s]: redact %.100q in %q: %v", ev.Network, msgid, buffer, err)
		return false, buffer
	}
	// A replayed scrub that landed in "*": flag it so the batch close
	// invalidates the client's cached "*" list (see histBatch.starTouched).
	if ok && starBatch != nil {
		starBatch.starTouched = true
	}
	return ok, buffer
}

func eventData(m store.Message) EventData {
	// The store scrubs a redacted message's body on redaction, so m.Raw is
	// already empty here; blank it defensively regardless, so a redacted
	// message's content is never sent to a client (the UI renders the
	// tombstone from the Redacted flag + reason alone).
	raw := m.Raw
	if m.Redacted {
		raw = ""
	}
	return EventData{
		Network:      m.Network,
		Buffer:       m.Target,
		ID:           m.ID,
		Time:         m.Time.UnixMilli(),
		MsgID:        m.MsgID,
		Sender:       m.Sender,
		Command:      m.Command,
		Raw:          raw,
		Redacted:     m.Redacted,
		RedactReason: m.RedactReason,
	}
}

// persistMembership writes QUIT/NICK system lines into scrollback.
// Neither message names a channel: a live event carries the sender's
// shared channels (Event.Affected, captured by the manager before its
// roster forgot them) plus any open query buffer with that nick; a
// replayed one belongs to its chathistory batch's target. The per-buffer
// msgid dedup makes the fan-out idempotent under overlapping backfill.
func (h *Hub) persistMembership(ctx context.Context, c Conn, ev irc.Event, replay bool, batch *histBatch) {
	for _, target := range h.membershipTargets(ctx, ev, replay, batch, c.Fold) {
		h.persistMembershipLine(ctx, c, ev, target, replay)
	}
}

// persistMembershipLine stores one QUIT/NICK system line into one buffer
// and, when live and unarchived, broadcasts it. Live writes hold
// bufferMutationMu for the append + broadcast, like persistEvent.
func (h *Hub) persistMembershipLine(ctx context.Context, c Conn, ev irc.Event, target string, replay bool) {
	if !replay {
		h.bufferMutationMu.Lock()
		defer h.bufferMutationMu.Unlock()
	}
	// Canonicalize under the network casemapping (like persistEvent) so
	// a QUIT/NICK for #FOO lands in the existing #foo buffer instead of
	// splitting off a case-variant duplicate: the channel targets come
	// from the roster's raw wire spelling, which can differ in case from
	// the stored buffer.
	//
	// A just-closed buffer must not be resurrected by QUIT/NICK fan-out
	// either (mirrors persistEvent's grace): the guard, applied
	// atomically with buffer creation in the store, drops a straggler
	// for a buffer closed concurrently. This is replay-agnostic to match
	// persistEvent — a replayed (event-playback) QUIT/NICK for a buffer
	// the user just closed must not re-create it either.
	// QUIT/NICK are membership fan-out and never un-archive a
	// close_buffer purge:false buffer (channel or query): the line is
	// persisted into the hidden history but not published, mirroring
	// persistEvent's archived-buffer policy.
	stored, stillArchived, err := h.store.AppendFoldedGuardedArchive(ctx, ev.Network, target, c.Fold,
		func(bool) bool { return h.recentlyClosed(ev.Network, c.Fold(target)) },
		false, storeMessage(ev))
	if err != nil {
		log.Printf("irc[%s]: persist %s to %q: %v", ev.Network, ev.Msg.Command, target, err)
		return
	}
	if stored.ID == 0 {
		return // duplicate msgid: already stored and announced
	}
	if !replay && !stillArchived {
		h.broadcast(envelope("event", 0, eventData(stored)))
	}
}

// maxMembershipFanout caps how many buffers one QUIT/NICK line is persisted
// and broadcast into. The roster tolerates up to 4096 channels per nick, so a
// hostile server that first stuffs our roster can otherwise turn every 20-byte
// NICK line into that many serialized DB inserts + client pushes, repeatably
// (alternating NICK a<->b preserves membership). The manager already truncates
// Event.Affected to maxAffectedChannels (== this) at capture, so this is a
// belt-and-braces bound; past it the roster still updates every channel — only
// the scrollback system lines in the excess channels are skipped (logged, never
// silent). 128 is far beyond any real shared-channel count.
const maxMembershipFanout = 128

// membershipTargets collects the buffers a QUIT/NICK line lands in: the
// replay batch's channel, or — live — the shared channels the manager
// captured plus any open query buffer with that nick.
func (h *Hub) membershipTargets(ctx context.Context, ev irc.Event, replay bool, batch *histBatch, fold func(string) string) []string {
	if replay {
		if batch != nil && batch.target != "" {
			return []string{batch.target}
		}
		return nil
	}
	targets := ev.Affected
	if len(targets) > maxMembershipFanout {
		log.Printf("irc[%s]: %s affects %d channels; persisting the system line to the first %d only",
			ev.Network, ev.Msg.Command, len(targets), maxMembershipFanout)
		// Full-slice expression: the append below must reallocate rather than
		// write into ev.Affected's spare capacity.
		targets = targets[:maxMembershipFanout:maxMembershipFanout]
	}
	if ev.Msg.Prefix != nil {
		if name, ok, err := h.store.FindBuffer(ctx, ev.Network, ev.Msg.Prefix.Name, fold); err == nil && ok {
			targets = append(targets, name)
		}
	}
	return targets
}

// persistTarget decides which buffer a message lands in, or none.
// isChan and fold are the network's ISUPPORT-driven channel detection
// and casemapping.
func persistTarget(m *ircv4.Message, ourNick string, isChan func(string) bool, fold func(string) string, statusPrefixes string, queryOpen func(string) bool) (string, bool) {
	switch m.Command {
	case "PRIVMSG", "NOTICE":
		return directTarget(m, ourNick, isChan, fold, statusPrefixes, queryOpen)
	case "JOIN", "PART", "TOPIC", "KICK", "MODE":
		if t := m.Param(0); isChan(t) {
			return t, true
		}
		return "", false
	}
	return "", false
}

// stripStatusPrefix removes a single leading STATUSMSG prefix (e.g. '@' in
// "@#chan", '+' in "+#chan") when the remainder is a channel, so a message
// to a channel-member subset is filed under the bare channel buffer. Any
// other target (a plain channel, a nick) is returned unchanged.
func stripStatusPrefix(target, statusPrefixes string, isChan func(string) bool) string {
	if len(target) > 1 && strings.IndexByte(statusPrefixes, target[0]) != -1 && isChan(target[1:]) {
		return target[1:]
	}
	return target
}

// directTarget files a PRIVMSG/NOTICE: channels under themselves,
// messages addressed to us under the sender (queries, NickServ, server
// notices), and our own echoes under the recipient.
func directTarget(m *ircv4.Message, ourNick string, isChan func(string) bool, fold func(string) string, statusPrefixes string, queryOpen func(string) bool) (string, bool) {
	// Non-ACTION CTCP (VERSION probes, PING, and their reply NOTICEs) is
	// protocol chatter, not conversation: persisting it would open
	// unread query buffers for every probe.
	if body := m.Trailing(); strings.HasPrefix(body, "\x01") &&
		!strings.HasPrefix(body, "\x01ACTION") {
		return "", false
	}
	t := stripStatusPrefix(m.Param(0), statusPrefixes, isChan)
	if isChan(t) {
		return t, true
	}
	if m.Command == "NOTICE" {
		return noticeTarget(m, t, ourNick, fold, queryOpen)
	}
	if m.Prefix == nil || m.Prefix.Name == "" || ourNick == "" {
		return "", false
	}
	if fold(t) == fold(ourNick) {
		return m.Prefix.Name, true
	}
	// echo-message: our own message reflected back — file under the
	// recipient so sent PMs land in the query buffer.
	if fold(m.Prefix.Name) == fold(ourNick) && t != "" {
		return t, true
	}
	return "", false
}

// noticeTarget files a NOTICE that is not addressed to a channel. An
// incoming private NOTICE addressed to us collects in the network server
// buffer (The Lounge lobby) — server "***" notices and service messages
// (NickServ/ChanServ/SaslServ) — UNLESS a query buffer with the sender is
// already open, in which case it files there: this keeps a live conversation
// with a service or an opped bot (which reply via NOTICE, not PRIVMSG) in the
// buffer the user opened, rather than scattering the replies to the lobby. We
// only redirect to an EXISTING query (queryOpen), never spawn one, so an
// unsolicited service notice still lands in the lobby and does not clutter the
// sidebar. Our own echoed notice goes to its recipient's buffer.
func noticeTarget(m *ircv4.Message, t, ourNick string, fold func(string) string, queryOpen func(string) bool) (string, bool) {
	own := m.Prefix != nil && ourNick != "" && fold(m.Prefix.Name) == fold(ourNick)
	if own {
		if t != "" && fold(t) != fold(ourNick) {
			return t, true // our own echoed notice -> the recipient's buffer
		}
		return "", false
	}
	// Addressed to us (t == our nick) or the pre-registration "*".
	if t == serverBufferTarget || ourNick == "" || fold(t) == fold(ourNick) {
		// An open query with the sender wins over the lobby: file the notice
		// where the user is talking to them.
		if m.Prefix != nil && m.Prefix.Name != "" && queryOpen != nil && queryOpen(m.Prefix.Name) {
			return m.Prefix.Name, true
		}
		return serverBufferTarget, true
	}
	return "", false
}

// defaultIsChannel is the RFC 1459 CHANTYPES fallback, used by tests.
func defaultIsChannel(target string) bool {
	return target != "" && (target[0] == '#' || target[0] == '&')
}

// messageTime is the single place the stored-timestamp source rule
// lives. Precedence:
//
//  1. the message's IRCv3 server-time tag, when present and valid
//     (RFC3339 UTC with millisecond precision) — the server's clock is
//     authoritative for ordering across reconnects and future history
//     replay;
//  2. otherwise the local receive time carried on the event;
//  3. as a last resort (zero event time), store.Append stamps the
//     current time on insert.
func messageTime(ev irc.Event) time.Time {
	if v, ok := ev.Msg.Tags["time"]; ok {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			// Reject an implausible far-future server-time (clock skew or
			// a malicious server): it would sort the message to the
			// bottom of scrollback permanently and poison the
			// read-marker clamp (which ceilings on the newest ts). Fall
			// back to our receipt time.
			ref := ev.Time
			if ref.IsZero() {
				ref = time.Now()
			}
			if !t.After(ref.Add(serverTimeSkew)) {
				return t
			}
		}
	}
	return ev.Time
}

// serverTimeSkew is how far ahead of receipt a server-time tag may be
// before we distrust it (clock skew allowance).
const serverTimeSkew = 5 * time.Minute

// storeMessage converts an event to its stored form.
func storeMessage(ev irc.Event) store.Message {
	sender := ""
	if ev.Msg.Prefix != nil {
		sender = ev.Msg.Prefix.Name
	}
	return store.Message{
		Time:    messageTime(ev),
		MsgID:   ev.Msg.Tags["msgid"],
		Sender:  sender,
		Command: ev.Msg.Command,
		Raw:     ev.Msg.String(),
		Text:    searchText(ev.Msg),
	}
}

// searchText extracts the body indexed for full-text search: PRIVMSG and
// NOTICE content, with CTCP ACTION unwrapped to its text. Non-ACTION CTCP
// and every other command index nothing (empty), so search returns only
// real messages.
func searchText(m *ircv4.Message) string {
	switch m.Command {
	case "PRIVMSG", "NOTICE":
		body := m.Trailing()
		if a, ok := ctcpAction(body); ok {
			return a
		}
		if len(body) > 0 && body[0] == '\x01' {
			return "" // other CTCP (VERSION, PING, ...): not searchable text
		}
		return body
	}
	return ""
}

// ctcpAction returns the action text of a CTCP ACTION body
// (\x01ACTION <text>\x01), or ok=false if the body is not an action.
func ctcpAction(body string) (string, bool) {
	const prefix = "\x01ACTION "
	if !strings.HasPrefix(body, prefix) {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(body, prefix), "\x01"), true
}
