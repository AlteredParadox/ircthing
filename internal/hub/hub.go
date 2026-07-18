// Package hub owns fan-out: IRC events flow in from the per-network
// connection managers, get persisted to the store, and are broadcast to
// every connected WebSocket session. Sessions send client requests
// (history pages, read markers, outgoing messages) back through it.
// The hub is transport-agnostic: internal/api owns the actual WebSocket
// connections and moves Envelopes in and out of Sessions.
package hub

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
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
	SetMonitored(nicks []string)
	MonitorAdd(nick string)
	MonitorRemove(nick string)
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

	mu         sync.Mutex
	networks   map[string]Conn
	states     map[string]string          // last known connection state per network
	presence   map[string]map[string]bool // network -> monitored nick -> online (MONITOR)
	motdWanted map[string]bool            // networks with an explicit /motd pending
	procs      map[string]*netProc        // running network lifecycles
	sessions   map[*Session]struct{}
	// recentClose bounds the buffer-resurrection race: a buffer closed
	// from the UI (close_buffer) must not be re-created by straggler
	// inbound traffic that was already in flight. Keyed by
	// network+"\x00"+foldedBuffer -> unix-ms of the close.
	recentClose map[string]int64
}

// Store exposes the shared store so the HTTP layer can read/write its own
// settings keys (e.g. the runtime media-proxy config) without a second
// handle to the database.
func (h *Hub) Store() *store.Store { return h.store }

func New(st *store.Store) *Hub {
	return &Hub{
		store:       st,
		recentClose: make(map[string]int64),
		networks:    make(map[string]Conn),
		states:      make(map[string]string),
		presence:    make(map[string]map[string]bool),
		motdWanted:  make(map[string]bool),
		procs:       make(map[string]*netProc),
		sessions:    make(map[*Session]struct{}),
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

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-c.Events():
			switch ev.Kind {
			case irc.EventState:
				if ev.State == irc.StateRegistered {
					// Fresh connection: stale batch refs are gone and
					// every target gets a full pagination budget again.
					histBatches = make(map[string]*histBatch)
					backfillPages = make(map[string]int)
					whois = make(map[string]*WhoisData)
				}
				h.onState(ctx, c, ev)
			case irc.EventMessage:
				if err := h.onMessage(ctx, c, ev, histBatches, backfillPages, whois); err != nil {
					return err
				}
			}
		}
	}
}

// onState records and broadcasts a connection state change, kicking off
// backfill and MONITOR re-establishment on registration.
func (h *Hub) onState(ctx context.Context, c Conn, ev irc.Event) {
	h.mu.Lock()
	h.states[ev.Network] = ev.State.String()
	h.mu.Unlock()
	errStr := ""
	if ev.Err != nil {
		errStr = ev.Err.Error()
		log.Printf("irc[%s]: %s: %v", ev.Network, ev.State, ev.Err)
	} else {
		log.Printf("irc[%s]: %s", ev.Network, ev.State)
	}
	h.broadcast(envelope("state", 0, StateData{
		Network: ev.Network,
		State:   ev.State.String(),
		Error:   errStr,
	}))
	if ev.State == irc.StateRegistered {
		h.backfill(ctx, c)
		h.startMonitor(ctx, c)
	}
	if ev.State == irc.StateDisconnected {
		h.clearPresence(ev.Network)
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
			log.Printf("irc[%s]: server failure: %s", ev.Network, ev.Msg.String())
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
		log.Printf("irc[%s]: MONITOR list full: %s", ev.Network, ev.Msg.Trailing())
		return true
	case "732", "733": // RPL_MONLIST / end — the store is our source of truth
		return true
	}
	return false
}

func (h *Hub) onMessage(ctx context.Context, c Conn, ev irc.Event, histBatches map[string]*histBatch, backfillPages map[string]int, whois map[string]*WhoisData) error {
	// Standard-replies and MONITOR numerics are self-contained.
	if h.handleControlNumeric(ctx, c, ev) {
		return nil
	}
	// WHOIS replies accumulate into one card (consumed here).
	if h.accumulateWhois(ev, whois) {
		return nil
	}
	// Other command replies (WHOWAS/WHO/LIST/AWAY/...) and error numerics
	// are pushed as ephemeral "server_info" lines.
	if info, ok := h.serverInfo(ev); ok {
		info.Network = ev.Network
		h.broadcast(envelope("server_info", 0, info))
		return nil
	}
	if ev.Msg.Command == "BATCH" {
		h.trackHistoryBatch(ctx, ev, c, histBatches, backfillPages)
		return nil
	}
	// An empty batch tag must never match: histBatches[""] could exist
	// if a server opened a chathistory batch with a bare "+" reference
	// (guarded below in trackHistoryBatch), and would then classify all
	// un-batched live traffic as replay — persisted but never
	// broadcast.
	batchRef := ev.Msg.Tags["batch"]
	replay := batchRef != "" && histBatches[batchRef] != nil
	batch := histBatches[batchRef]
	if replay {
		// Track the batch's own newest message as the anchor for the
		// next page: live traffic may already have stored rows newer
		// than the gap, so the store's latest row would skip past it.
		batch.count++
		if t := messageTime(ev); !t.IsZero() {
			batch.lastTS = t.UnixMilli()
		}
		batch.lastID = ev.Msg.Tags["msgid"]
	} else {
		h.liveHints(ctx, c, ev)
	}
	switch ev.Msg.Command {
	case "TAGMSG": // ephemeral, never persisted
		if !replay {
			h.relayTyping(ev, c)
		}
		return nil
	case "MARKREAD": // marker state, never persisted
		h.applyUpstreamMarker(ctx, c, ev)
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

// liveHints handles the side effects only live (non-replayed) messages
// have: members_changed refetch hints, surfacing INVITEs, and the
// backfill request our own JOIN triggers.
func (h *Hub) liveHints(ctx context.Context, c Conn, ev irc.Event) {
	if hint, affected := membersHint(ev.Msg); affected {
		// Canonical spelling: clients key buffers by it (see history_changed).
		h.broadcast(envelope("members_changed", 0, MembersChangedData{
			Network: ev.Network, Buffer: h.store.CanonicalBuffer(ctx, ev.Network, hint, c.Fold),
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
	if ev.Msg.Prefix == nil || c.Nick() == "" || c.Fold(ev.Msg.Prefix.Name) != c.Fold(c.Nick()) {
		return // not our own membership change
	}
	var add bool
	switch ev.Msg.Command {
	case "JOIN":
		add = true
	case "PART":
		add = false
	default:
		return
	}
	ch := ev.Msg.Param(0)
	if ch == "" || !c.IsChannel(ch) {
		return
	}
	// Never persist a channel we could not validly rejoin: a spoofed self-JOIN
	// for an over-long name or one carrying framing bytes would make
	// netconf.Validate fail on every restart (bricking the network). A PART
	// (remove) is always allowed, so an already-stored bad entry can be cleared.
	if add && (len(ch) > maxPersistedChannelLen || strings.ContainsAny(ch, " \r\n\x00")) {
		return
	}
	// TryLock, never Lock: this runs on the Hub event goroutine, and a network
	// edit/delete holds netOps while StopNetwork waits for THIS goroutine to
	// exit — a blocking Lock here would deadlock. Best-effort: skip persisting
	// this one membership change while a network operation is in progress.
	if !h.netOps.TryLock() {
		return
	}
	defer h.netOps.Unlock()
	if err := h.updateAutojoinLocked(ctx, ev.Network, ch, add, c.Fold); err != nil {
		log.Printf("irc[%s]: persist autojoin %s %q: %v", ev.Network, ev.Msg.Command, ch, err)
	}
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
	return t, t != ""
}

// persistEvent stores a message in its buffer and, when live, broadcasts
// it. Duplicate msgids (overlapping backfill) are silently dropped.
func (h *Hub) persistEvent(ctx context.Context, c Conn, ev irc.Event, replay bool, batch *histBatch) error {
	// Routing: during replay the buffer is the batch target, which is
	// authoritative and independent of the nick we used when these messages
	// were sent. Re-deriving from the CURRENT nick (persistTarget → directTarget)
	// would misfile PM history after a /nick or a reconnect collision-fallback
	// changed our nick since. Live traffic routes by the current nick, correct
	// in the moment.
	var target string
	var ok bool
	if replay && batch != nil && batch.target != "" &&
		(ev.Msg.Command == "PRIVMSG" || ev.Msg.Command == "NOTICE") {
		target, ok = replayTarget(ev.Msg, batch.target, c)
	} else {
		target, ok = persistTarget(ev.Msg, c.Nick(), c.IsChannel, c.Fold, c.StatusPrefixes())
	}
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
	stored, err := h.store.AppendFoldedGuarded(ctx, ev.Network, target, c.Fold,
		h.graceGuard(ev, c, target, selfPart, replay), storeMessage(ev))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("irc[%s]: persist %s to %q: %v", ev.Network, ev.Msg.Command, target, err)
		return nil
	}
	if stored.ID == 0 {
		return nil // duplicate msgid: already stored and announced
	}
	if !replay {
		h.broadcast(envelope("event", 0, eventData(stored)))
	}
	return nil
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
	nicks, err := h.store.Monitors(ctx, c.Name())
	if err != nil {
		log.Printf("irc[%s]: monitor load: %v", c.Name(), err)
		return
	}
	if len(nicks) > 0 {
		c.SetMonitored(nicks)
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
	maxOpenWhois       = 64               // concurrent WHOIS accumulations
	maxOpenHistBatches = 256              // concurrent chathistory replay batches
	maxBackfillTargets = 4096             // distinct targets tracked for paginated backfill
	closeGraceMs       = 10_000           // buffer stays closed against stragglers this long
	ownDedupWindow     = 10 * time.Minute // reconcile a replayed own message with its local placeholder
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
}

// histBatch is the replay progress of one open chathistory batch.
type histBatch struct {
	target string
	count  int
	lastTS int64  // unix ms of the batch's newest message
	lastID string // its msgid, "" when it has none
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

// accumulateWhois collects the WHOIS reply numerics into one card,
// keyed by the queried nick (param 1), and flushes it as a "whois" push
// on 318 (end of /WHOIS). It returns true when it consumed the message
// — so whois numerics never reach serverInfo. 301 (away) is folded in
// only while a whois for that nick is in progress; a standalone away
// notice falls through to serverInfo. Whois state is per connection
// (reset on registration in Run).
func (h *Hub) accumulateWhois(ev irc.Event, whois map[string]*WhoisData) bool {
	m := ev.Msg
	switch m.Command {
	case "311", "312", "313", "317", "319", "330", "335", "338", "378", "379", "671":
		nick := m.Param(1)
		if nick == "" {
			return true
		}
		w := whois[nick]
		if w == nil {
			if len(whois) >= maxOpenWhois {
				return true // consumed but not tracked: bound the map
			}
			w = &WhoisData{Nick: nick}
			whois[nick] = w
		}
		applyWhois(w, m)
		return true
	case "301": // away — part of a whois only if one is open
		if w := whois[m.Param(1)]; w != nil {
			w.Away = m.Trailing()
			return true
		}
		return false
	case "318": // end of /WHOIS: flush the card
		nick := m.Param(1)
		if w := whois[nick]; w != nil {
			delete(whois, nick)
			w.Network = ev.Network
			clampWhois(w) // bound server-controlled fields the client then retains
			h.broadcast(envelope("whois", 0, w))
		}
		return true
	}
	return false
}

// clampWhois bounds every server-controlled string field of a whois card
// before it is broadcast — a hostile server can pack ~64 KiB into each, and
// the client keeps completed cards in scrollback.
func clampWhois(w *WhoisData) {
	w.Nick = clampServerInfo(w.Nick)
	w.User = clampServerInfo(w.User)
	w.Host = clampServerInfo(w.Host)
	w.Realname = clampServerInfo(w.Realname)
	w.Server = clampServerInfo(w.Server)
	w.Channels = clampServerInfo(w.Channels)
	w.Account = clampServerInfo(w.Account)
	w.Actual = clampServerInfo(w.Actual)
	w.Away = clampServerInfo(w.Away)
}

// applyWhois sets the field one WHOIS numeric carries.
func applyWhois(w *WhoisData, m *ircv4.Message) {
	switch m.Command {
	case "311": // <me> <nick> <user> <host> * :<realname>
		w.User, w.Host, w.Realname = m.Param(2), m.Param(3), m.Trailing()
	case "312": // <me> <nick> <server> :<info>
		w.Server = m.Param(2)
	case "313": // is an IRC operator
		w.Operator = true
	case "317": // <me> <nick> <idle> [<signon>] :seconds idle, signon time
		w.Idle = atoiOr0(m.Param(2))
		w.Signon = atoiOr0(m.Param(3))
	case "319": // :<prefixed channels>
		w.Channels = m.Trailing()
	case "330": // <me> <nick> <account> :is logged in as
		w.Account = m.Param(2)
	case "335": // is a bot
		w.Bot = true
	case "338": // <me> <nick> <host/ip> [...] :actually using host
		// The IP/host is in the middle params; the trailing is a label.
		if mid := strings.Join(midParams(m, 2), " "); mid != "" && w.Actual == "" {
			w.Actual = mid
		}
	case "378": // <me> <nick> :is connecting from <user@host> <ip>
		if w.Actual == "" {
			w.Actual = strings.TrimPrefix(m.Trailing(), "is connecting from ")
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
// user-requested and should be shown. motdExpected consumes the gate at
// the end-of-MOTD numerics.
func (h *Hub) expectMOTD(network string) {
	h.mu.Lock()
	h.motdWanted[network] = true
	h.mu.Unlock()
}

func (h *Hub) motdExpected(network, cmd string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	want := h.motdWanted[network]
	if cmd == "376" || cmd == "422" { // RPL_ENDOFMOTD / ERR_NOMOTD
		delete(h.motdWanted, network)
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
	case strings.HasPrefix(ref, "+") && strings.Contains(ev.Msg.Param(1), "chathistory"):
		if ref[1:] == "" {
			return // empty reference: ignore, or it becomes histBatches[""]
		}
		if len(batches) >= maxOpenHistBatches {
			return // bound the map; the manager tears the connection down at this cap
		}
		batches[ref[1:]] = &histBatch{target: ev.Msg.Param(2)}
	case strings.HasPrefix(ref, "-"):
		b := batches[ref[1:]]
		if b == nil {
			return
		}
		delete(batches, ref[1:])
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
		// Announce the CANONICAL stored spelling: clients key buffers by it,
		// so a wire-spelling hint whose casing differs would miss the client's
		// page-invalidation and leave stale scrollback.
		h.broadcast(envelope("history_changed", 0, HistoryChangedData{
			Network: ev.Network, Buffer: h.store.CanonicalBuffer(ctx, ev.Network, b.target, c.Fold),
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
			delete(h.recentClose, k)
		}
	}
	h.recentClose[network+"\x00"+foldedBuffer] = nowMs
}

// unmarkClosed clears a buffer's close grace (e.g. on a self-JOIN
// reopen) so subsequent traffic re-creates it normally.
func (h *Hub) unmarkClosed(network, foldedBuffer string) {
	h.mu.Lock()
	delete(h.recentClose, network+"\x00"+foldedBuffer)
	h.mu.Unlock()
}

// recentlyClosed reports whether the folded buffer was closed within the
// grace window.
func (h *Hub) recentlyClosed(network, foldedBuffer string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.recentClose[network+"\x00"+foldedBuffer]
	return ok && time.Now().UnixMilli()-t <= closeGraceMs
}

func (h *Hub) network(name string) Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.networks[name]
}

func (h *Hub) broadcast(env Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.sessions {
		s.push(env)
	}
}

// broadcastExcept sends to every session but the originator, which gets
// its own seq-tagged response instead.
func (h *Hub) broadcastExcept(except *Session, env Envelope) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.sessions {
		if s != except {
			s.push(env)
		}
	}
}

// relayTyping turns an incoming TAGMSG carrying the +typing client tag
// into a "typing" push. Our own echoed TAGMSGs are ignored — the local
// client knows what it is typing.
func (h *Hub) relayTyping(ev irc.Event, c Conn) {
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
	buffer := ev.Msg.Param(0)
	if !c.IsChannel(buffer) {
		// A typing notice addressed to us belongs in the sender's query.
		if c.Fold(buffer) != c.Fold(nick) {
			return
		}
		buffer = sender
	}
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
	// Resolve the buffer. During replay the batch target is authoritative and
	// nick-independent (a replayed redaction of a PM would otherwise miss after
	// a since-then nick change). Live: a redaction addressed to our nick files
	// under the other party's buffer, mirroring persistTarget.
	var buffer string
	addressedToUs := false
	if replay && batch != nil && batch.target != "" {
		buffer = batch.target
	} else {
		addressedToUs = !c.IsChannel(target) && ev.Msg.Prefix != nil && c.Fold(target) == c.Fold(c.Nick())
		buffer = target
		if addressedToUs {
			buffer = ev.Msg.Prefix.Name
		}
	}
	buffer = h.store.CanonicalBuffer(ctx, ev.Network, buffer, c.Fold)
	ok, err := h.store.SetRedacted(ctx, ev.Network, buffer, msgid, reason)
	if err != nil {
		log.Printf("irc[%s]: redact %s in %q: %v", ev.Network, msgid, buffer, err)
		return
	}
	if !ok && addressedToUs {
		// A private NOTICE addressed to us is filed in the server buffer (see
		// noticeTarget), not the sender's query — retry there so a service's
		// redaction of a stored private NOTICE actually scrubs it.
		buffer = serverBufferTarget
		if ok, err = h.store.SetRedacted(ctx, ev.Network, buffer, msgid, reason); err != nil {
			log.Printf("irc[%s]: redact %s in %q: %v", ev.Network, msgid, buffer, err)
			return
		}
	}
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
		Network: ev.Network, Buffer: buffer, MsgID: msgid,
		By: clampServerInfo(by), Reason: clampServerInfo(reason),
	}))
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
		stored, err := h.store.AppendFoldedGuarded(ctx, ev.Network, target, c.Fold,
			func(bool) bool { return h.recentlyClosed(ev.Network, c.Fold(target)) },
			storeMessage(ev))
		if err != nil {
			log.Printf("irc[%s]: persist %s to %q: %v", ev.Network, ev.Msg.Command, target, err)
			continue
		}
		if stored.ID == 0 {
			continue // duplicate msgid: already stored and announced
		}
		if !replay {
			h.broadcast(envelope("event", 0, eventData(stored)))
		}
	}
}

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
func persistTarget(m *ircv4.Message, ourNick string, isChan func(string) bool, fold func(string) string, statusPrefixes string) (string, bool) {
	switch m.Command {
	case "PRIVMSG", "NOTICE":
		return directTarget(m, ourNick, isChan, fold, statusPrefixes)
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
func directTarget(m *ircv4.Message, ourNick string, isChan func(string) bool, fold func(string) string, statusPrefixes string) (string, bool) {
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
		return noticeTarget(m, t, ourNick, fold)
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

// noticeTarget files a NOTICE that is not addressed to a channel. Server
// "***" notices and service messages (NickServ/ChanServ/SaslServ) — anything
// addressed to us or the pre-registration "*" — collect in the network server
// buffer (The Lounge lobby) rather than spawning a per-sender query that would
// clutter the sidebar. Our own echoed notice goes to its recipient's buffer.
func noticeTarget(m *ircv4.Message, t, ourNick string, fold func(string) string) (string, bool) {
	own := m.Prefix != nil && ourNick != "" && fold(m.Prefix.Name) == fold(ourNick)
	if own {
		if t != "" && fold(t) != fold(ourNick) {
			return t, true // our own echoed notice -> the recipient's buffer
		}
		return "", false
	}
	// Addressed to us (t == our nick) or the pre-registration "*".
	if t == serverBufferTarget || ourNick == "" || fold(t) == fold(ourNick) {
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
