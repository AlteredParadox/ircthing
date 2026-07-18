package irc

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	ircv4 "gopkg.in/irc.v4"

	"ircthing/internal/wgdial"
)

// State of a network connection, reported via Event.
type State int

const (
	StateDisconnected State = iota
	StateConnecting
	StateRegistered
)

func (s State) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateRegistered:
		return "registered"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

type EventKind int

const (
	EventState EventKind = iota
	EventMessage
)

// Event is what a Manager reports to its consumer (the hub).
type Event struct {
	Network string
	Kind    EventKind
	// Msg is set for EventMessage: one server message, including those
	// received during registration.
	Msg *ircv4.Message
	// Affected lists the channels a live QUIT/NICK touches — the
	// sender's shared channels, captured before the roster processed the
	// message and forgot them. Empty for every other message.
	Affected []string
	// State is set for EventState; Err carries the disconnect reason when
	// State is StateDisconnected.
	State State
	Err   error
	Time  time.Time
}

var (
	ErrNotConnected  = errors.New("irc: not connected")
	ErrSendQueueFull = errors.New("irc: send queue full")
)

// Manager maintains one IRC network connection: it dials (TLS unless
// plaintext is explicitly allowed), registers with optional SASL PLAIN,
// answers PING, detects dead connections with its own keepalive PING, and
// reconnects with exponential backoff + jitter. Per the concurrency model
// there is one read loop and one flood-limited writer goroutine per
// connection; Run itself is the read loop.
type Manager struct {
	cfg    Config
	events chan Event
	out    chan *ircv4.Message
	sendMu sync.Mutex // lifecycle lock: enqueue vs registered/drain
	// pendingCapVals holds values from post-registration CAP NEW,
	// published into capVals when the capability is ACKed. Read-loop
	// only.
	pendingCapVals map[string]string
	registered     atomic.Bool
	nick           atomic.Value // string: current nick once registered
	caps           atomic.Value // map[string]bool, copy-on-write: enabled capabilities
	capVals        atomic.Value // map[string]string: values of enabled caps
	isup           *isupport
	roster         *roster
	// joined is the set of channels to (re)join after registration:
	// the configured ones plus runtime JOINs, minus runtime PARTs (a KICK
	// deliberately does not remove the intent — bouncers rejoin). Only
	// the run goroutine touches it.
	// joined is the rejoin set: channels to (re)join on every
	// registration, seeded from the configured autojoin list and kept
	// in step with our own JOIN/PART. Keyed by the channel's raw
	// spelling, NOT a folded name: the server's CASEMAPPING is unknown
	// at construction (005 arrives only in the read loop), so folding
	// here would wrongly merge distinct channels like #[x] and #{x} on
	// an ascii-casemapping server. Only the run goroutine touches it.
	joined map[string]string // raw channel -> original casing (identity)

	// namesMu guards namesReq: channels for which we have already sent an
	// explicit NAMES this connection (draft/no-implicit-names). Touched by
	// EnsureNames (hub goroutine) and reset on reconnect (run goroutine).
	namesMu  sync.Mutex
	namesReq map[string]bool

	// whoxDone tracks channels already WHOX-queried this connection (see
	// maybeWHOX). Only the read loop touches it.
	whoxDone map[string]bool

	// stsMu guards the active STS policy (sts.go): connect to stsPort
	// with TLS until stsUntil (zero stsUntil with a port = session-only
	// upgrade). stsLastDur is the most recently advertised duration, used
	// to reschedule the expiry when a connection closes.
	stsMu      sync.Mutex
	stsPort    int
	stsUntil   time.Time
	stsLastDur time.Duration

	batchSeq atomic.Uint64 // outgoing multiline batch reference counter

	// wgMu guards wgTun, the lazily-built WireGuard egress tunnel. Built on
	// first dial when cfg.WireGuard != nil, reused across reconnects, torn
	// down when Run returns.
	wgMu  sync.Mutex
	wgTun *wgdial.Tunnel
}

// Name returns the configured network label.
func (m *Manager) Name() string { return m.cfg.Name }

// Nick returns the nick the server currently knows us by: empty before
// the first registration, then kept current across nick changes and
// reconnects.
func (m *Manager) Nick() string {
	n, _ := m.nick.Load().(string)
	return n
}

func NewManager(cfg Config) (*Manager, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	isup := newISupport()
	m := &Manager{
		cfg:            cfg,
		events:         make(chan Event, 256),
		out:            make(chan *ircv4.Message, 64),
		isup:           isup,
		roster:         newRoster(isup),
		joined:         make(map[string]string),
		namesReq:       make(map[string]bool),
		whoxDone:       make(map[string]bool),
		pendingCapVals: make(map[string]string),
	}
	for _, ch := range cfg.Channels {
		m.joined[ch] = ch // raw key; see the joined field comment
	}
	return m, nil
}

// IsChannel reports whether target names a channel per the server's
// ISUPPORT CHANTYPES.
func (m *Manager) IsChannel(target string) bool {
	return m.isup.IsChannel(target)
}

// ChanTypes returns the server's channel prefix characters.
// Fold lowercases a name per the connection's ISUPPORT CASEMAPPING.
func (m *Manager) Fold(name string) string { return m.isup.Fold(name) }

// defaultLineLen is the RFC 1459 line limit (message + CRLF) used until a
// server advertises ISUPPORT LINELEN, and always during registration.
const defaultLineLen = 512

// lineLen returns the server's advertised LINELEN, floored at the RFC 1459
// default. A server may legitimately RAISE the limit, but must never lower
// the effective OUTBOUND limit below 512: this value gates the writer's
// FATAL length backstop and boundedPong's budget, so a hostile 005
// LINELEN=1 would otherwise reject even a mandatory PONG and pin the network
// in a tight, backoff-reset reconnect loop. Mirrors the reader's clamp in
// linereader.setLimit.
func (m *Manager) lineLen() int {
	if v, ok := m.isup.Raw("LINELEN"); ok {
		if n, _ := strconv.Atoi(v); n > defaultLineLen {
			return n
		}
	}
	return defaultLineLen
}

// checkLineLen rejects a message whose serialized form (tags excluded —
// message tags have their own separate budget per the IRCv3
// message-tags spec) plus CRLF exceeds the server's line length.
func checkLineLen(msg *ircv4.Message, limit int) error {
	bare := *msg
	bare.Tags = nil
	if n := len(bare.String()) + 2; n > limit {
		return fmt.Errorf("irc: line is %d bytes; the server's limit is %d", n, limit)
	}
	return nil
}

// ErrUnsafeFraming reports a message carrying CR, LF, or NUL — the
// characters that frame IRC lines. Enforced centrally on every outbound
// message so a client-supplied parameter can never inject extra protocol
// lines, independent of any handler-level checks.
var ErrUnsafeFraming = errors.New("irc: message contains CR, LF, or NUL")

func hasFramingBytes(s string) bool {
	return strings.ContainsAny(s, "\r\n\x00")
}

// framingStripper removes the bytes that frame IRC lines. Used to sanitize a
// server-derived value before it goes out on the internal priority lane,
// which bypasses sendAll and hits the writer's FATAL checkFraming.
var framingStripper = strings.NewReplacer("\r", "", "\n", "", "\x00", "")

// checkFraming rejects a message whose command, parameters, prefix, or
// tags contain framing bytes.
func checkFraming(msg *ircv4.Message) error {
	if hasFramingBytes(msg.Command) || strings.IndexByte(msg.Command, ' ') != -1 {
		return ErrUnsafeFraming
	}
	if err := checkParamFraming(msg.Params); err != nil {
		return err
	}
	if msg.Prefix != nil &&
		(hasFramingBytes(msg.Prefix.Name) || hasFramingBytes(msg.Prefix.User) || hasFramingBytes(msg.Prefix.Host)) {
		return ErrUnsafeFraming
	}
	for k, v := range msg.Tags {
		if hasFramingBytes(k) || hasFramingBytes(string(v)) {
			return ErrUnsafeFraming
		}
	}
	return nil
}

func checkParamFraming(params []string) error {
	for i, p := range params {
		if hasFramingBytes(p) {
			return ErrUnsafeFraming
		}
		// A non-trailing parameter that contains a space, is empty, or begins
		// with ':' would be re-parsed by the receiver as a different boundary
		// (the serializer writes middle params verbatim, and a receiver reads
		// the first ':'-led token as the trailing arg, silently swallowing
		// the intended target and everything after it). Only the last
		// parameter is serialized as trailing and may take those forms.
		if i < len(params)-1 && (p == "" || p[0] == ':' || strings.IndexByte(p, ' ') != -1) {
			return ErrUnsafeFraming
		}
	}
	return nil
}

func (m *Manager) ChanTypes() string {
	return m.isup.ChanTypes()
}

// StatusPrefixes returns the ISUPPORT STATUSMSG prefix set (e.g. "~&@%+"),
// or "" when the server does not advertise it.
func (m *Manager) StatusPrefixes() string {
	v, _ := m.isup.Raw("STATUSMSG")
	return v
}

// Channel returns the topic and member snapshot of a joined channel;
// ok is false for channels we are not in (or before registration).
func (m *Manager) Channel(name string) (topic string, members []Member, ok bool) {
	return m.roster.channel(name)
}

// EnsureNames requests the membership of a channel we have not fetched
// this connection. Under draft/no-implicit-names
// (https://ircv3.net/specs/extensions/no-implicit-names, fetched
// 2026-07-15) the server sends no NAMES on JOIN, so member lists are
// fetched lazily — only for channels the user actually looks at — which
// is the point of the capability. Without the cap this is a no-op: the
// implicit NAMES already populated the roster. The reply (353/366)
// populates the roster and raises a members_changed hint as usual.
func (m *Manager) EnsureNames(channel string) {
	if !m.CapEnabled("no-implicit-names") || !m.registered.Load() {
		return
	}
	if len(channel) > maxChannelNameBytes {
		return
	}
	key := m.isup.Fold(channel)
	m.namesMu.Lock()
	already := m.namesReq[key]
	// Bound the set (parity with whoxDone): don't grow past the cap with
	// keys for channels we never NAMES. A new key above the cap is skipped.
	if !already && len(m.namesReq) >= maxJoinedChannels {
		m.namesMu.Unlock()
		return
	}
	m.namesMu.Unlock()
	if already {
		return
	}
	// Mark requested only after a successful send: a dropped NAMES
	// (full send queue) must be retried on the next view, not silently
	// swallowed leaving the roster empty.
	if err := m.Send(newMsg("NAMES", channel)); err != nil {
		return
	}
	m.namesMu.Lock()
	m.namesReq[key] = true
	m.namesMu.Unlock()
}

func (m *Manager) resetNames() {
	m.namesMu.Lock()
	m.namesReq = make(map[string]bool)
	m.namesMu.Unlock()
}

// SendMultiline sends a multi-line message as a draft/multiline batch.
// The whole message is validated against the server's advertised limits
// (max-lines, max-bytes, LINELEN) and enqueued atomically — it either
// goes out complete or not at all; nothing is silently truncated.
// Callers should only use this when draft/multiline is negotiated;
// otherwise send one PRIVMSG per line.
func (m *Manager) SendMultiline(target string, lines []string) error {
	lim := parseMultilineLimits(m.CapValue("draft/multiline"))
	if err := validateMultiline(target, lines, lim, m.lineLen()); err != nil {
		return err
	}
	ref := "ml" + strconv.FormatUint(m.batchSeq.Add(1), 10)
	batch := buildMultilineBatch(ref, target, lines)
	// A batch bigger than the whole send queue can never be enqueued (even
	// when idle), so sendAll's ErrSendQueueFull would be a misleading
	// "retry later". Reject it with a clear, actionable error instead.
	if len(batch) > cap(m.out) {
		return fmt.Errorf("irc: multiline message too large to send in one batch (%d lines, local limit %d)", len(lines), cap(m.out)-2)
	}
	return m.sendAll(batch)
}

// SetMonitored replaces the MONITOR list with nicks (MONITOR extension,
// https://ircv3.net/specs/extensions/monitor, fetched 2026-07-15). The
// hub drives this on every registration from the persisted buddy list, so
// the list is re-established after reconnects. Requests are clamped to the
// ISUPPORT MONITOR limit and chunked to stay within the line length; the
// server replies 730/731 with each target's current presence.
func (m *Manager) SetMonitored(nicks []string) {
	if !m.registered.Load() {
		return
	}
	if limit := m.monitorLimit(); limit > 0 && len(nicks) > limit {
		nicks = nicks[:limit]
	}
	_ = m.Send(newMsg("MONITOR", "C")) // clear any stale list on this connection
	for _, chunk := range chunkTargets(nicks, 10) {
		_ = m.Send(newMsg("MONITOR", "+", strings.Join(chunk, ",")))
	}
}

// MonitorAdd starts monitoring one nick; MonitorRemove stops. Both no-op
// before registration (the hub re-sends the whole list on registration).
func (m *Manager) MonitorAdd(nick string) {
	if m.registered.Load() {
		_ = m.Send(newMsg("MONITOR", "+", nick))
	}
}

func (m *Manager) MonitorRemove(nick string) {
	if m.registered.Load() {
		_ = m.Send(newMsg("MONITOR", "-", nick))
	}
}

// monitorLimit returns the ISUPPORT MONITOR target limit, or 0 for no
// limit / not advertised.
func (m *Manager) monitorLimit() int {
	if v, ok := m.isup.Raw("MONITOR"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// chunkTargets splits a target list into groups of at most n.
func chunkTargets(targets []string, n int) [][]string {
	var out [][]string
	for len(targets) > 0 {
		end := n
		if end > len(targets) {
			end = len(targets)
		}
		out = append(out, targets[:end])
		targets = targets[end:]
	}
	return out
}

// RequestChatHistory asks the server for messages in target newer than
// the given resume point — gap-free scrollback after reconnects
// (draft/chathistory AFTER; https://ircv3.net/specs/extensions/
// chathistory, fetched 2026-07-15). No-op unless the server offers the
// cap. The msgid selector is preferred when the newest stored message
// has one and MSGREFTYPES allows it: AFTER excludes equal timestamps, so
// two messages in the same millisecond would lose the second one under a
// timestamp selector. One page per call, clamped to the CHATHISTORY
// ISUPPORT limit; the hub keeps paging while replay batches come back
// full (paginated backfill).
func (m *Manager) RequestChatHistory(target string, sinceMs int64, msgid string) {
	if !m.CapEnabled("draft/chathistory") {
		return
	}
	limit := m.HistoryPageSize()
	sel := "timestamp=" + time.UnixMilli(sinceMs).UTC().Format("2006-01-02T15:04:05.000Z")
	if msgid != "" {
		if refs, ok := m.isup.Raw("MSGREFTYPES"); ok && mechListed(refs, "msgid") {
			sel = "msgid=" + msgid
		}
	}
	// Best-effort: a full queue just means no backfill this round.
	_ = m.Send(newMsg("CHATHISTORY", "AFTER", target, sel, strconv.Itoa(limit)))
}

// HistoryPageSize is the per-request chathistory message limit: 100,
// lowered to the server's advertised CHATHISTORY maximum.
func (m *Manager) HistoryPageSize() int {
	limit := 100
	if v, ok := m.isup.Raw("CHATHISTORY"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < limit {
			limit = n
		}
	}
	return limit
}

// CapEnabled reports whether an IRCv3 capability was negotiated on the
// current connection (kept current through cap-notify NEW/DEL).
func (m *Manager) CapEnabled(name string) bool {
	caps, _ := m.caps.Load().(map[string]bool)
	return caps[name]
}

// CapValue returns the advertised value of an enabled capability, or ""
// (e.g. draft/multiline's "max-bytes=4096,max-lines=24").
func (m *Manager) CapValue(name string) string {
	vals, _ := m.capVals.Load().(map[string]string)
	return vals[name]
}

func (m *Manager) setCaps(caps map[string]bool) {
	m.caps.Store(caps)
}

// handleCapNotify processes CAP NEW/DEL/ACK after registration
// (cap-notify, implicitly enabled by CAP LS 302). Returns messages to
// send. sasl appearing in NEW is ignored — mid-session re-auth is out of
// scope here. Values advertised by NEW (e.g. draft/multiline's
// max-bytes/max-lines) are stashed and published once the capability is
// ACKed — the ACK itself carries only names. Read-loop only, so
// pendingCapVals needs no locking.
func (m *Manager) handleCapNotify(in *ircv4.Message) []*ircv4.Message {
	if len(in.Params) < 3 {
		return nil
	}
	list := in.Params[len(in.Params)-1]
	switch strings.ToUpper(in.Params[1]) {
	case "NEW":
		var req []string
		seen := make(map[string]bool)
		for _, tok := range strings.Fields(list) {
			name, value, _ := strings.Cut(tok, "=")
			// Dedup: m.caps is only updated on the later ACK, so CapEnabled
			// can't suppress a name repeated within one CAP NEW. Without this,
			// a hostile server repeating a wanted cap hundreds of times builds
			// a multi-kilobyte CAP REQ that trips the writer's FATAL length
			// guard (the internal path has no sendAll check), looping the
			// connection.
			if !wantedCapSet[name] || m.CapEnabled(name) || seen[name] {
				continue
			}
			seen[name] = true
			req = append(req, name)
			if value != "" {
				m.pendingCapVals[name] = value
			}
		}
		if len(req) == 0 {
			return nil
		}
		sort.Strings(req)
		reqMsg := newMsg("CAP", "REQ", strings.Join(req, " "))
		// Dedup bounds this to the finite wanted set (well under LINELEN), but
		// validate anyway: this line takes the internal priority path, whose
		// writer length check is fatal — never feed it an over-length line.
		if checkLineLen(reqMsg, m.lineLen()) != nil {
			return nil
		}
		return []*ircv4.Message{reqMsg}
	case "ACK", "DEL":
		m.applyCapChange(strings.ToUpper(in.Params[1]) == "ACK", list)
	}
	return nil
}

// applyCapChange updates the enabled-capability set and its values
// (both copy-on-write) from a post-registration CAP ACK or DEL list:
// an ACK publishes the value stashed from the announcing NEW, a DEL
// drops both the capability and any stale value.
func (m *Manager) applyCapChange(enable bool, list string) {
	old, _ := m.caps.Load().(map[string]bool)
	caps := make(map[string]bool, len(old))
	for k, v := range old {
		caps[k] = v
	}
	oldVals, _ := m.capVals.Load().(map[string]string)
	vals := make(map[string]string, len(oldVals))
	for k, v := range oldVals {
		vals[k] = v
	}
	for _, tok := range strings.Fields(list) {
		name, removed := strings.CutPrefix(tok, "-")
		if enable && !removed {
			// Only accept a capability we actually want (and therefore
			// ever requested). Without this, a hostile server can ACK an
			// unbounded stream of unique, unrequested names post-
			// registration, growing this copy-on-write map without limit
			// and turning each ACK into an O(n) copy (O(n²) overall) until
			// the process dies. wantedCapSet is a small finite set.
			if !wantedCapSet[name] {
				continue
			}
			caps[name] = true
			if v, ok := m.pendingCapVals[name]; ok {
				vals[name] = v
				delete(m.pendingCapVals, name)
			}
		} else {
			delete(vals, name)
			delete(caps, name)
		}
	}
	m.capVals.Store(vals)
	m.setCaps(caps)
}

// Events delivers server messages and state changes. The channel is
// buffered; if the consumer stops reading, the read loop blocks
// (backpressure) rather than dropping events.
func (m *Manager) Events() <-chan Event { return m.events }

// Send queues msg for delivery on the current connection. It is
// best-effort: it fails fast while unregistered, and messages still queued
// when a connection dies are dropped rather than replayed into the next
// session.
func (m *Manager) Send(msg *ircv4.Message) error {
	return m.sendAll([]*ircv4.Message{msg})
}

// sendAll enqueues msgs atomically: all of them or none (a batch must
// never go out with lines missing because the queue filled partway).
// Every message is checked against the server's line-length limit first
// — the server would truncate or reject an oversized line after we had
// already acknowledged it.
//
// sendMu is the connection-generation lifecycle lock: it serializes
// every enqueue with the registered transitions and the stale-queue
// drain (see runOnce), so a message can never be enqueued for one
// connection and consumed by the next — the registered check and the
// enqueue happen atomically with respect to disconnect and drain.
func (m *Manager) sendAll(msgs []*ircv4.Message) error {
	limit := m.lineLen()
	for _, msg := range msgs {
		if err := checkFraming(msg); err != nil {
			return err
		}
		// Length-check the exact bytes the writer will emit: the
		// UTF8ONLY-scrubbed form, since invalid bytes inflate to U+FFFD (3x)
		// and the writer's post-scrub length check FATALLY tears the
		// connection down. A server-derived echo (e.g. a CTCP auto-reply
		// whose target is a hostile, invalid-UTF-8 nick) must be rejected
		// here rather than reach that fatal guard and loop the connection.
		if err := checkLineLen(m.scrubUTF8(msg), limit); err != nil {
			return err
		}
	}
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	if !m.registered.Load() {
		return ErrNotConnected
	}
	if cap(m.out)-len(m.out) < len(msgs) {
		return ErrSendQueueFull
	}
	for _, msg := range msgs {
		m.out <- msg
	}
	return nil
}

// setRegistered flips the registration flag under the lifecycle lock,
// so in-flight sendAll calls either complete before the flip or observe
// the new state — never a stale one.
func (m *Manager) setRegistered(v bool) {
	m.sendMu.Lock()
	m.registered.Store(v)
	m.sendMu.Unlock()
}

// Run connects and reconnects until ctx is canceled.
func (m *Manager) Run(ctx context.Context) error {
	defer m.wgClose()
	m.loadSTS(ctx)
	bo := newBackoff(m.cfg.Backoff)
	for {
		m.emit(ctx, Event{Kind: EventState, State: StateConnecting})
		err := m.runOnce(ctx, bo)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// An STS upgrade is a policy-driven redial, not a failure: adopt
		// a session policy and reconnect securely right away.
		var up errSTSUpgrade
		if errors.As(err, &up) {
			m.stsMu.Lock()
			m.stsPort, m.stsUntil = up.port, time.Time{}
			m.stsMu.Unlock()
			continue
		}
		m.emit(ctx, Event{Kind: EventState, State: StateDisconnected, Err: err})
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bo.next()):
		}
	}
}

// loadSTS seeds the in-memory STS policy from the persistent store.
func (m *Manager) loadSTS(ctx context.Context) {
	if m.cfg.STS == nil {
		return
	}
	host, _, err := net.SplitHostPort(m.cfg.Addr)
	if err != nil {
		return
	}
	port, until, ok, err := m.cfg.STS.STSPolicy(ctx, host)
	if err != nil || !ok || !time.Now().Before(until) {
		return
	}
	m.stsMu.Lock()
	m.stsPort, m.stsUntil = port, until
	m.stsMu.Unlock()
}

// effectiveAddr resolves where and how to connect: the configured
// address, unless an unexpired STS policy upgrades a plaintext config to
// TLS on the policy port.
func (m *Manager) effectiveAddr() (addr string, secure bool) {
	if m.cfg.TLS {
		return m.cfg.Addr, true
	}
	host, _, err := net.SplitHostPort(m.cfg.Addr)
	if err != nil {
		return m.cfg.Addr, false
	}
	m.stsMu.Lock()
	port, until := m.stsPort, m.stsUntil
	m.stsMu.Unlock()
	if port > 0 && (until.IsZero() || time.Now().Before(until)) {
		return net.JoinHostPort(host, strconv.Itoa(port)), true
	}
	return m.cfg.Addr, false
}

// applySTS handles a duration policy received on a secure connection:
// remember it (and its port — the one we are connected to), persist it,
// and keep the duration for close-time rescheduling. duration=0 clears
// the policy.
func (m *Manager) applySTS(ctx context.Context, connAddr string, d time.Duration) {
	host, _, err := net.SplitHostPort(m.cfg.Addr)
	if err != nil {
		return
	}
	_, portStr, err := net.SplitHostPort(connAddr)
	if err != nil {
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return
	}
	until := time.Now().Add(d)
	m.stsMu.Lock()
	if d == 0 {
		m.stsPort, m.stsUntil, m.stsLastDur = 0, time.Time{}, 0
	} else {
		m.stsPort, m.stsUntil, m.stsLastDur = port, until, d
	}
	m.stsMu.Unlock()
	if m.cfg.STS == nil {
		return
	}
	if d == 0 {
		_ = m.cfg.STS.ClearSTSPolicy(ctx, host)
		return
	}
	_ = m.cfg.STS.SetSTSPolicy(ctx, host, port, until)
}

// rescheduleSTS pushes the policy expiry to now + the last advertised
// duration, as the spec requires when a connection closes.
func (m *Manager) rescheduleSTS(ctx context.Context) {
	m.stsMu.Lock()
	d, port := m.stsLastDur, m.stsPort
	if d <= 0 || port == 0 {
		m.stsMu.Unlock()
		return
	}
	until := time.Now().Add(d)
	m.stsUntil = until
	m.stsMu.Unlock()
	if m.cfg.STS == nil {
		return
	}
	if host, _, err := net.SplitHostPort(m.cfg.Addr); err == nil {
		_ = m.cfg.STS.SetSTSPolicy(context.WithoutCancel(ctx), host, port, until)
	}
}

func (m *Manager) emit(ctx context.Context, ev Event) {
	ev.Network = m.cfg.Name
	ev.Time = time.Now()
	select {
	case m.events <- ev:
	case <-ctx.Done():
	}
}

// runOnce performs one connection lifecycle: dial, register, then read
// until the connection dies or ctx is canceled.
// drainSendQueue drops messages queued for a previous connection so
// stale lines are not written into the middle of the new registration.
// Under the lifecycle lock: registered is already false, so no enqueue
// can slip in after this drain (anything sent until registration
// completes is rejected with ErrNotConnected).
func (m *Manager) drainSendQueue() {
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	for {
		select {
		case <-m.out:
		default:
			return
		}
	}
}

// applyCaps records the enabled capabilities and their advertised values
// (e.g. multiline's max-bytes/max-lines) for this connection.
func (m *Manager) applyCaps(hs *handshake) {
	m.setCaps(hs.enabled)
	vals := make(map[string]string, len(hs.enabled))
	for name := range hs.enabled {
		if v := hs.caps[name]; v != "" {
			vals[name] = v
		}
	}
	m.capVals.Store(vals)
	m.pendingCapVals = make(map[string]string)
}

// rejoinList snapshots the rejoin set into a sorted slice, pruning entries
// that could never be sent (bad framing / over the line limit) from
// m.joined so one bad stored channel never wedges every reconnect. It must
// run BEFORE serveLoop starts, which is the only other writer of m.joined
// (trackJoinIntent) — the caller then sends the JOINs from the returned
// snapshot, concurrently with the read loop, touching no shared state.
func (m *Manager) rejoinList() []string {
	rejoin := make([]string, 0, len(m.joined))
	for _, ch := range m.joined {
		if !m.rejoinable(ch) {
			delete(m.joined, ch)
			continue
		}
		rejoin = append(rejoin, ch)
	}
	sort.Strings(rejoin)
	return rejoin
}

func (m *Manager) runOnce(ctx context.Context, bo *backoff) error {
	m.drainSendQueue()

	// Reset per-connection ISUPPORT/roster/lazy-NAMES/WHOX state up front,
	// BEFORE register(): the writer checks every outbound line against the
	// ISUPPORT LINELEN, and register() drives the handshake through the
	// writer, so a stale small LINELEN carried over from a previous
	// connection would fail registration lines — and, because the reset used
	// to run only after register() returned, would never clear, bricking
	// every subsequent reconnect. All four are repopulated by the
	// 005/JOIN/353 replies of the new connection, and registration is not
	// signalled until later, so a consumer still never sees the previous
	// connection's state once "registered".
	m.isup.reset()
	m.roster.clear()
	defer m.roster.clear()
	m.resetNames()
	m.whoxDone = make(map[string]bool)

	addr, secure := m.effectiveAddr()
	conn, err := m.dial(ctx, addr, secure)
	if err != nil {
		return err
	}
	// STS: whenever a secure connection closes, its policy expiry is
	// pushed to close-time + duration (a no-op without a policy).
	if secure {
		defer m.rescheduleSTS(ctx)
	}
	// Everything below is scoped to this connection: canceling cctx
	// closes the socket, which unblocks both loops.
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		<-cctx.Done()
		conn.Close()
	}()

	blr := newBoundedLineReader(conn)
	r := ircv4.NewReader(blr)
	w := ircv4.NewWriter(conn)
	if os.Getenv("IRCTHING_DEBUG_RAW") != "" {
		r.DebugCallback = func(line string) {
			log.Printf("irc[%s] << %s", m.cfg.Name, redactRaw(line))
		}
		w.DebugCallback = func(line string) {
			log.Printf("irc[%s] >> %s", m.cfg.Name, redactRaw(line))
		}
	}
	internal := make(chan *ircv4.Message, 16)
	urgent := make(chan *ircv4.Message, 8)
	go m.writeLoop(cctx, cancel, conn, w, urgent, internal)

	send := func(msgs []*ircv4.Message) error {
		for _, out := range msgs {
			select {
			case internal <- out:
			case <-cctx.Done():
				return context.Cause(cctx)
			}
		}
		return nil
	}
	lc := &liveConn{conn: conn, cctx: cctx, r: r, blr: blr, send: send, internal: internal, urgent: urgent, addr: addr, secure: secure}

	hs, err := m.register(ctx, lc)
	if err != nil {
		return err
	}

	m.nick.Store(hs.nick)
	m.applyCaps(hs)

	// Read and rejoin concurrently. rejoins are paced by the flood token
	// bucket (~1 JOIN / 2s after the burst); doing them inline before
	// serveLoop — as this used to — blocked the read loop for a many-channel
	// reconnect, freezing all input (PINGs, MOTD, NAMES, new messages) for
	// up to minutes and risking the server's SendQ limit for exactly the
	// 50-channel load this bouncer targets. Snapshot the rejoin set first
	// (serveLoop's trackJoinIntent is the only other writer of m.joined),
	// then read while the JOINs trickle out from this goroutine.
	rejoins := m.rejoinList()
	readDone := make(chan error, 1)
	go func() { readDone <- m.serveLoop(ctx, lc) }()

	// Queue the JOINs on the priority `internal` path and expose
	// send-readiness as soon as the first one is queued: the writer drains
	// `internal` ahead of `m.out`, and this loop keeps `internal` fed, so
	// every JOIN still precedes any user message or the hub's backfill/
	// monitor traffic (enqueued to m.out on StateRegistered) — without
	// waiting the full throttled drain to become send-ready.
	defer m.setRegistered(false)
	markRegistered := func() {
		m.setRegistered(true)
		bo.reset()
		m.emit(ctx, Event{Kind: EventState, State: StateRegistered})
	}
	for i, ch := range rejoins {
		if err := send([]*ircv4.Message{newMsg("JOIN", ch)}); err != nil {
			cancel(err) // writer gone; serveLoop's read fails too
			return <-readDone
		}
		if i == 0 {
			markRegistered()
		}
	}
	if len(rejoins) == 0 {
		markRegistered()
	}
	return <-readDone
}

// liveConn bundles one established connection's plumbing for the
// registration and steady-state loops.
type liveConn struct {
	conn     net.Conn
	cctx     context.Context
	r        *ircv4.Reader
	blr      *boundedLineReader
	send     func([]*ircv4.Message) error
	internal chan *ircv4.Message
	urgent   chan *ircv4.Message // top-priority, unthrottled lane (PONG)
	addr     string
	secure   bool
}

// sendUrgent queues a keepalive on the unthrottled top-priority lane so a PONG
// is never stuck behind rate-paced rejoin JOINs in the internal lane (which
// could delay it past a strict server's ping timeout). Best-effort.
func (lc *liveConn) sendUrgent(msg *ircv4.Message) {
	select {
	case lc.urgent <- msg:
	case <-lc.cctx.Done():
	}
}

// flushParting waits (briefly) for queued messages to reach the wire
// before the socket is torn down.
func (lc *liveConn) flushParting() {
	deadline := time.Now().Add(500 * time.Millisecond)
	for len(lc.internal) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// One more beat for a message the writer has dequeued but not yet
	// written.
	time.Sleep(20 * time.Millisecond)
}

// register drives the CAP/SASL/NICK/USER exchange to 001. The whole
// exchange must finish within HandshakeTimeout.
func (m *Manager) register(ctx context.Context, lc *liveConn) (*handshake, error) {
	hs := newHandshake(&m.cfg)
	hs.secure = lc.secure
	if err := lc.send(hs.start()); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(m.cfg.HandshakeTimeout)
	for {
		lc.conn.SetReadDeadline(deadline)
		in, err := lc.r.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("registration: %w", connError(lc.cctx, err))
		}
		out, done, err := hs.handle(in)
		// A secure CAP LS carried an STS duration policy: persist it.
		if hs.stsDuration != nil {
			m.applySTS(ctx, lc.addr, *hs.stsDuration)
			hs.stsDuration = nil
		}
		// A failing handshake can still have parting words (SASL abort
		// "AUTHENTICATE *", QUIT) — flush them before the deferred cancel
		// tears down the socket.
		if sendErr := lc.send(out); sendErr != nil {
			return nil, sendErr
		}
		if err != nil {
			if len(out) > 0 {
				lc.flushParting()
			}
			return nil, fmt.Errorf("registration: %w", err)
		}
		m.emit(ctx, Event{Kind: EventMessage, Msg: in})
		if done {
			return hs, nil
		}
	}
}

// serveLoop is the steady state: answer PINGs, keep ISUPPORT and the
// roster current, and emit every line to the hub. The read deadline
// doubles as the keepalive timer: after PingInterval of silence we PING,
// and if the server stays silent for PingTimeout more, the connection is
// dead.
func (m *Manager) serveLoop(ctx context.Context, lc *liveConn) error {
	// Open chathistory batches: messages tagged with these refs are
	// replayed history, not live traffic, and must not touch live state
	// (roster, nick, rejoin intent). Nested batches are not tracked —
	// servers do not nest chathistory in practice.
	histBatch := make(map[string]bool)
	ml := newMultiline() // draft/multiline reconstruction, per connection
	for {
		in, err := m.readMessage(lc)
		if err != nil {
			return err
		}
		if err := m.processLine(ctx, lc, in, histBatch, ml); err != nil {
			return err
		}
	}
}

// processLine handles one server line: protocol housekeeping, multiline
// reconstruction, chathistory-batch tracking, ISUPPORT, live-line
// processing, and the event emit. A consumed multiline batch line stops
// here (returns nil) rather than being emitted.
func (m *Manager) processLine(ctx context.Context, lc *liveConn, in *ircv4.Message, histBatch map[string]bool, ml *multiline) error {
	if err := m.serviceLine(ctx, lc, in); err != nil {
		return err
	}
	if consumed, err := m.feedMultiline(ctx, ml, in); err != nil {
		return err
	} else if consumed {
		return nil
	}
	if err := trackChathistoryBatch(in, histBatch); err != nil {
		return err
	}
	playback := in.Tags["batch"] != "" && histBatch[clampBatchRef(in.Tags["batch"])]
	m.isup.handle(in) // 005, ignored otherwise
	if in.Command == "005" {
		// A negotiated LINELEN may legally exceed the default cap; raise
		// the reader's limit (bounded by the hard ceiling) so long-but-
		// legal lines are not cut off. Add the IRCv3 message tag budget:
		// LINELEN bounds the message + CRLF only, while tags ride on top
		// and the reader counts every wire byte.
		lc.blr.setLimit(m.lineLen() + maxTagBytes)
	}
	var affected []string
	if !playback {
		var err error
		if affected, err = m.onLiveLine(in, lc.send); err != nil {
			return err
		}
	}
	m.emit(ctx, Event{Kind: EventMessage, Msg: in, Affected: affected})
	return nil
}

// readMessage reads the next server line. The read deadline doubles as
// the keepalive timer: after PingInterval of silence we PING, and if the
// server stays silent for PingTimeout more, the connection is dead.
func (m *Manager) readMessage(lc *liveConn) (*ircv4.Message, error) {
	pinged := false
	for {
		idle := m.cfg.PingInterval
		if pinged {
			idle = m.cfg.PingTimeout
		}
		lc.conn.SetReadDeadline(time.Now().Add(idle))
		in, err := lc.r.ReadMessage()
		if err == nil {
			return in, nil
		}
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			return nil, connError(lc.cctx, err)
		}
		// A timeout mid-line means the server sent a partial line and
		// stalled: the underlying bufio reader has already discarded the
		// buffered fragment, so continuing would parse the rest of the
		// line as a corrupt standalone message. Tear the connection down
		// instead — keepalive is only for a genuinely idle (at-boundary)
		// connection.
		if lc.blr.midLine() {
			return nil, fmt.Errorf("read timeout mid-line: server stalled after a partial line")
		}
		if pinged {
			return nil, fmt.Errorf("ping timeout: no traffic for %s after keepalive PING", m.cfg.PingTimeout)
		}
		pinged = true
		if err := lc.send([]*ircv4.Message{newMsg("PING", "keepalive")}); err != nil {
			return nil, err
		}
	}
}

// feedMultiline runs draft/multiline reconstruction: batch lines are
// buffered (consumed, not processed further) and the single
// reconstructed message is emitted when the batch closes.
func (m *Manager) feedMultiline(ctx context.Context, ml *multiline, in *ircv4.Message) (consumed bool, err error) {
	emit, consumed, err := ml.feed(in)
	if err != nil {
		return consumed, err
	}
	if emit != nil {
		m.emit(ctx, Event{Kind: EventMessage, Msg: emit})
	}
	return consumed, nil
}

// boundedPong builds the PONG answering a server PING, echoing just the
// token (the last param) bounded to the writer's line limit. Echoing an
// arbitrarily long, non-compliant token verbatim would build an
// over-length PONG that the writer rejects fatally — a remote
// reconnect-loop DoS (mirrors maxCTCPPingToken; the reader admits PING
// lines far larger than LINELEN, so the cap must live here).
//
// The budget divides by 3 to leave headroom for UTF8ONLY scrubbing, which
// the writer applies BEFORE its final length check: an invalid byte becomes
// U+FFFD (3 bytes), so a token of invalid bytes can inflate up to 3x. A cap
// near the full limit would let a scrubbed PONG exceed it and re-trigger the
// very teardown this guards. Real PONG tokens (a timestamp/cookie) are tiny,
// so the tighter cap never truncates a legitimate one.
func boundedPong(in *ircv4.Message, limit int) *ircv4.Message {
	token := ""
	if n := len(in.Params); n > 0 {
		token = in.Params[n-1]
	}
	// Strip framing bytes first: the PONG takes the internal priority lane,
	// which bypasses sendAll and reaches the writer's FATAL checkFraming.
	// irc.v4 trims only the trailing CRLF, so a PING token can still carry a
	// NUL or an embedded CR — unstripped, it would tear the connection down
	// and pin the network in a reconnect loop.
	token = framingStripper.Replace(token)
	budget := (limit - len("PONG :\r\n")) / 3
	if budget < 0 {
		budget = 0
	}
	if len(token) > budget {
		token = token[:budget]
	}
	return newMsg("PONG", token)
}

// serviceLine answers protocol housekeeping on a line: server PINGs and
// post-registration cap-notify (including its STS refresh).
func (m *Manager) serviceLine(ctx context.Context, lc *liveConn, in *ircv4.Message) error {
	if in.Command == "PING" {
		// Urgent lane: a PONG must not queue behind rate-paced rejoin JOINs.
		lc.sendUrgent(boundedPong(in, m.lineLen()))
	}
	if in.Command == "CAP" { // cap-notify: NEW/DEL after registration
		if out := m.handleCapNotify(in); len(out) > 0 {
			if err := lc.send(out); err != nil {
				return err
			}
		}
		if err := m.capNotifySTS(ctx, lc, in); err != nil {
			return err
		}
	}
	return nil
}

// trackChathistoryBatch follows chathistory BATCH open/close markers.
// maxOpenHistBatches bounds concurrently open chathistory replay batches
// a server can hold open, mirroring the multiline batch cap: a malicious
// server could otherwise stream unclosed "BATCH +ref chathistory" lines
// and grow this map (and the hub's) without bound.
const maxOpenHistBatches = 256

// maxBatchRefBytes bounds a batch reference used as a map key. Real refs are
// short opaque tokens; near-line-limit refs would otherwise pin whole parsed
// lines in the count-capped map. The clamp MUST be applied identically at
// store, delete, and lookup: clamping only at store would make oversized-ref
// batches miss at lookup and misclassify their replayed messages as live
// traffic (notifications, unread counts, roster churn from history).
const maxBatchRefBytes = 512

func clampBatchRef(ref string) string {
	if len(ref) > maxBatchRefBytes {
		return ref[:maxBatchRefBytes]
	}
	return ref
}

func trackChathistoryBatch(in *ircv4.Message, histBatch map[string]bool) error {
	if in.Command != "BATCH" || len(in.Params) == 0 {
		return nil
	}
	ref := in.Params[0]
	switch {
	case strings.HasPrefix(ref, "+") && strings.Contains(in.Param(1), "chathistory"):
		if len(histBatch) >= maxOpenHistBatches {
			return fmt.Errorf("irc: too many open chathistory batches (>%d)", maxOpenHistBatches)
		}
		// Clone: the ref is a substring of the parsed line and would pin it.
		histBatch[strings.Clone(clampBatchRef(ref[1:]))] = true
	case strings.HasPrefix(ref, "-"):
		delete(histBatch, clampBatchRef(ref[1:]))
	}
	return nil
}

// capNotifySTS stores/refreshes the STS policy a CAP NEW on a secure
// connection may carry (CAP DEL never disables it, per spec).
func (m *Manager) capNotifySTS(ctx context.Context, lc *liveConn, in *ircv4.Message) error {
	if len(in.Params) < 3 || strings.ToUpper(in.Params[1]) != "NEW" {
		return nil
	}
	for _, tok := range strings.Fields(in.Params[len(in.Params)-1]) {
		name, val, _ := strings.Cut(tok, "=")
		if name != "sts" {
			continue
		}
		v := parseSTS(val)
		// Per the STS spec, an upgrade policy seen over an insecure link
		// MUST trigger a secure reconnect (the same abort the CAP LS
		// path uses). Nothing is persisted until it is verified on the
		// secure connection.
		if !lc.secure && v.port > 0 {
			return errSTSUpgrade{port: v.port}
		}
		if lc.secure && v.hasDuration {
			m.applySTS(ctx, lc.addr, v.duration)
		}
	}
	return nil
}

// onLiveLine applies a live (non-replayed) line's side effects: CTCP
// auto-replies, own-nick tracking, roster and rejoin-intent bookkeeping,
// and the WHOX query after NAMES. It returns the channels a QUIT/NICK
// affects, captured before the roster forgets the sender.
func (m *Manager) onLiveLine(in *ircv4.Message, send func([]*ircv4.Message) error) ([]string, error) {
	var affected []string
	// QUIT/NICK name no channel; capture the sender's shared channels
	// before the roster processes the message and forgets them, so the
	// hub can persist a line per buffer.
	if (in.Command == "QUIT" || in.Command == "NICK") && in.Prefix != nil {
		affected = m.roster.channelsWith(in.Prefix.Name)
	}
	if err := m.autoReply(in, send); err != nil {
		return nil, err
	}
	// Track our own nick changes, compared under the server's
	// casemapping.
	if in.Command == "NICK" && in.Prefix != nil && m.isup.FoldEqual(in.Prefix.Name, m.Nick()) {
		if n := in.Param(0); n != "" {
			m.nick.Store(n)
		}
	}
	m.roster.handle(m.Nick(), in)
	if err := m.trackJoinIntent(in); err != nil {
		return affected, err
	}
	return affected, nil
}

// autoReply sends the responses a live line can solicit: CTCP replies
// to queries addressed directly to us (VERSION/PING/TIME/CLIENTINFO —
// see ctcp.go; the caller excludes replays, so old queries are never
// re-answered), and the WHOX query after a channel's end-of-NAMES (the
// roster knows who is here but not their away/account state; see
// maybeWHOX).
func (m *Manager) autoReply(in *ircv4.Message, send func([]*ircv4.Message) error) error {
	if in.Command == "PRIVMSG" && m.isup.FoldEqual(in.Param(0), m.Nick()) {
		if reply := ctcpReply(in); reply != nil {
			// Best-effort via the normal (droppable, non-priority) queue,
			// NOT the internal priority lane: CTCP replies are remotely
			// inducible in unbounded quantity, so a flood must not starve
			// user/hub output. A full queue (or any send error) just
			// drops the reply.
			_ = m.Send(reply)
		}
	}
	if in.Command == "366" {
		if out := m.maybeWHOX(in.Param(1)); out != nil {
			return send([]*ircv4.Message{out})
		}
	}
	return nil
}

// whoxToken correlates our WHOX replies (354) with the roster; the spec
// allows 1–3 digits.
const whoxToken = "152"

// maybeWHOX returns the WHOX query for a channel whose NAMES just
// completed — account and away discovery for members who were already
// there when we joined (https://ircv3.net/specs/extensions/whox, fetched
// 2026-07-16; ISUPPORT WHOX gates it). Fields: t oken, n ick, f lags
// (H/G away), a ccount. Members joining later are covered by
// extended-join; changes by away-notify/account-notify. Once per channel
// per connection.
func (m *Manager) maybeWHOX(channel string) *ircv4.Message {
	if channel == "" || len(channel) > maxChannelNameBytes {
		return nil
	}
	if _, ok := m.isup.Raw("WHOX"); !ok {
		return nil
	}
	key := m.isup.Fold(channel)
	if m.whoxDone[key] {
		return nil
	}
	// Bound the set: a hostile server can end NAMES for endlessly varying
	// channel names and grow this map for the life of the connection
	// otherwise (mirrors maxJoinedChannels). Above the cap we skip the
	// WHOX rather than track the channel.
	if len(m.whoxDone) >= maxJoinedChannels {
		return nil
	}
	// The channel name is server-supplied and the WHO goes out the internal
	// priority path, where the writer FATALLY tears the connection down on
	// an over-length or malframed line (the same trap rejoinable guards the
	// JOIN against, and boundedPong the PONG). Validate the exact bytes the
	// writer will emit — framing on the message, length on the UTF8ONLY-
	// scrubbed form — BEFORE recording the channel: a stream of oversized
	// fake 366 names must neither loop the connection nor fill whoxDone with
	// keys for WHOs that are never emitted.
	msg := newMsg("WHO", channel, "%tnfa,"+whoxToken)
	if checkFraming(msg) != nil || checkLineLen(m.scrubUTF8(msg), m.lineLen()) != nil {
		return nil
	}
	m.whoxDone[key] = true
	return msg
}

// rejoinable reports whether a JOIN for ch could be sent at rejoin time —
// so a stored rejoin intent can never fatally fail the writer. It checks
// the exact bytes the writer emits: framing on the message, and length on
// the UTF8ONLY-scrubbed form (invalid bytes inflate to U+FFFD, 3x). Without
// the scrub, an invalid-UTF-8 channel name passes here but overflows the
// writer's fatal post-scrub check under UTF8ONLY, bricking the network on
// every reconnect. Against the default limit, since isup resets before
// rejoin.
func (m *Manager) rejoinable(ch string) bool {
	msg := newMsg("JOIN", ch)
	return checkFraming(msg) == nil && checkLineLen(m.scrubUTF8(msg), defaultLineLen) == nil
}

// trackJoinIntent keeps the rejoin set in step with our own JOINs and
// PARTs. Runs on the read-loop goroutine, which is the only writer of
// m.joined after construction.
func (m *Manager) trackJoinIntent(in *ircv4.Message) error {
	if in.Prefix == nil || !m.isup.FoldEqual(in.Prefix.Name, m.Nick()) {
		return nil
	}
	switch in.Command {
	case "JOIN":
		// Clone: Param substrings alias the whole parsed line's backing
		// array, so storing even a 2-byte name in the persistent rejoin map
		// would pin the full (up to 64 KiB) line — 4096 padded self-JOINs
		// could pin ~256 MiB. rejoinable bounds the name itself; the clone
		// detaches it from the line.
		ch := strings.Clone(in.Param(0))
		// Only store a channel we could actually rejoin: a spoofed
		// self-JOIN with framing bytes or an over-length name would
		// otherwise poison the never-reset rejoin set, and since the
		// rejoin JOIN bypasses sendAll and hits the writer's FATAL
		// length/framing guard, it would brick the network on every
		// reconnect. Validate against the registration-time line limit
		// (isup is reset before rejoin, so lineLen is the default there).
		if ch == "" || !m.isup.IsChannel(ch) || !m.rejoinable(ch) {
			return nil
		}
		if _, known := m.joined[ch]; !known && len(m.joined) >= maxJoinedChannels {
			return fmt.Errorf("irc: joined-channel set exceeded %d", maxJoinedChannels)
		}
		m.joined[ch] = ch
		// A fresh self-JOIN invalidates any prior NAMES fetch: under
		// no-implicit-names the server sends no membership on JOIN, so
		// after a part/rejoin (or a forward/cycle) EnsureNames must
		// re-request when the channel is next viewed — otherwise the
		// roster is stuck with only ourselves.
		m.namesMu.Lock()
		delete(m.namesReq, m.isup.Fold(ch))
		m.namesMu.Unlock()
	case "PART":
		// Remove by the network's casemapping so a PART in any
		// equivalent casing clears the rejoin intent.
		part := in.Param(0)
		for key := range m.joined {
			if m.isup.FoldEqual(key, part) {
				delete(m.joined, key)
			}
		}
	}
	return nil
}

// maxJoinedChannels bounds the rejoin-intent set (see trackJoinIntent).
const maxJoinedChannels = 4096

// maxChannelNameBytes is a hard cap on a channel name used as a map key
// (whoxDone, namesReq). Real names are bounded by ISUPPORT CHANNELLEN (tens of
// bytes); this generous cap is independent of the negotiable LINELEN so a
// hostile server can't grow those count-capped maps with near-64 KiB keys
// (4096 × ~64 KiB ≈ 256 MiB). Matches maxRosterField, the roster's clamp.
const maxChannelNameBytes = maxRosterField

// redactRaw prepares one raw IRC line for the debug log with credentials
// removed: the PASS parameter (server password) and any non-control
// AUTHENTICATE payload (SASL PLAIN reveals the password outright, and a
// SCRAM transcript enables offline guessing). Everything else is logged
// verbatim. "+" and "*" are AUTHENTICATE control tokens and carry no
// secret.
func redactRaw(line string) string {
	trimmed := strings.TrimRight(line, "\r\n")
	msg, err := ircv4.ParseMessage(trimmed)
	if err != nil {
		return trimmed
	}
	switch strings.ToUpper(msg.Command) {
	case "PASS":
		if len(msg.Params) > 0 {
			return "PASS <redacted>"
		}
	case "AUTHENTICATE":
		if p := msg.Param(0); p != "" && p != "+" && p != "*" {
			return "AUTHENTICATE <redacted>"
		}
	}
	return trimmed
}

// writeLoop is the per-connection writer goroutine: it serializes
// handshake/PONG traffic and user messages onto the socket through the
// flood-protection token bucket. On write failure it cancels the
// connection context with the error, which the read loop reports.
func (m *Manager) writeLoop(ctx context.Context, cancel context.CancelCauseFunc, conn net.Conn, w *ircv4.Writer, urgent, internal <-chan *ircv4.Message) {
	tb := newTokenBucket(m.cfg.SendBurst, m.cfg.SendInterval)
	// writeOne emits one message and returns false on a fatal error (the loop
	// then exits). throttle applies the flood token bucket — NEVER to urgent
	// PONGs, which must go out immediately regardless of the rejoin pacing.
	writeOne := func(out *ircv4.Message, throttle bool) bool {
		if throttle {
			if err := tb.wait(ctx); err != nil {
				return false
			}
		}
		// Last-resort framing guard: no message reaches the wire with CR/LF/NUL,
		// even the internally-queued handshake/PONG lines that never pass
		// through sendAll. A tainted message tears the connection down.
		if err := checkFraming(out); err != nil {
			cancel(err)
			return false
		}
		// Line-length backstop after UTF-8 scrubbing (which can grow a line via
		// U+FFFD). Unlike framing, an over-length line is DROPPED, not fatal: a
		// mid-session 005 can lower LINELEN while a line waited in the queue,
		// and the server would reject/truncate it anyway.
		scrubbed := m.scrubUTF8(out)
		if err := checkLineLen(scrubbed, m.lineLen()); err != nil {
			log.Printf("irc[%s]: dropping over-length %s line: %v", m.cfg.Name, out.Command, err)
			return true
		}
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if err := w.WriteMessage(scrubbed); err != nil {
			cancel(fmt.Errorf("write: %w", err))
			return false
		}
		return true
	}
	// Priority: urgent (PONG) > internal (handshake/rejoins) > m.out (user/hub).
	// A PONG is never overtaken by a rate-paced JOIN, and neither is overtaken
	// by a queued user message.
	for {
		select {
		case <-ctx.Done():
			return
		case out := <-urgent:
			if !writeOne(out, false) {
				return
			}
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return
		case out := <-urgent:
			if !writeOne(out, false) {
				return
			}
		case out := <-internal:
			if !writeOne(out, true) {
				return
			}
		default:
			select {
			case <-ctx.Done():
				return
			case out := <-urgent:
				if !writeOne(out, false) {
					return
				}
			case out := <-internal:
				if !writeOne(out, true) {
					return
				}
			case out := <-m.out:
				if !writeOne(out, true) {
					return
				}
			}
		}
	}
}

// scrubUTF8 enforces ISUPPORT UTF8ONLY
// (https://ircv3.net/specs/extensions/utf8-only, fetched 2026-07-16):
// once advertised, the client "MUST NOT send non-UTF-8 data to the
// server". Invalid sequences are replaced with U+FFFD rather than the
// message dropped; in practice everything we send comes from JSON and is
// already valid. Copy-on-write: the caller's message is never mutated.
func (m *Manager) scrubUTF8(msg *ircv4.Message) *ircv4.Message {
	if _, ok := m.isup.Raw("UTF8ONLY"); !ok {
		return msg
	}
	var cleaned *ircv4.Message
	for i, p := range msg.Params {
		if utf8.ValidString(p) {
			continue
		}
		if cleaned == nil {
			cp := *msg
			cp.Params = append([]string(nil), msg.Params...)
			cleaned = &cp
		}
		cleaned.Params[i] = strings.ToValidUTF8(p, "\uFFFD")
	}
	if cleaned != nil {
		return cleaned
	}
	return msg
}

// connError prefers the cause recorded on the connection context (e.g. a
// write error that closed the socket) over the secondary read error that
// closing produced.
func connError(cctx context.Context, readErr error) error {
	if cause := context.Cause(cctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return readErr
}

// dial connects to addr, with TLS when secure — usually the configured
// address/mode, unless an STS policy upgraded them (see effectiveAddr).
// A configured proxy carries the raw TCP leg; TLS runs inside the
// tunnel.
func (m *Manager) dial(ctx context.Context, addr string, secure bool) (net.Conn, error) {
	var conn net.Conn
	var err error
	if m.cfg.WireGuard != nil {
		conn, err = m.dialWireGuard(ctx, addr)
	} else if m.cfg.Proxy != "" {
		// Validated by NewManager; a parse error here cannot happen.
		proxy, perr := parseProxyURL(m.cfg.Proxy)
		if perr != nil {
			return nil, perr
		}
		conn, err = dialProxy(ctx, proxy, addr, m.cfg.DialTimeout)
	} else {
		d := &net.Dialer{Timeout: m.cfg.DialTimeout}
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	if !secure {
		return conn, nil
	}
	return m.wrapTLS(ctx, conn, addr)
}

// dialWireGuard dials addr through this network's in-process WireGuard tunnel.
// The tunnel is expensive to stand up (userspace device + Noise
// handshake), so it is built once on first use and reused across reconnects.
// A build failure leaves wgTun nil so the next reconnect retries under backoff
// rather than caching the error forever. Target DNS resolves through the
// tunnel's in-band resolver — no local-resolver leak.
func (m *Manager) dialWireGuard(ctx context.Context, addr string) (net.Conn, error) {
	// Invariant: dialWireGuard and wgClose both run ONLY on the single Run
	// goroutine (dial <- runOnce <- Run; wgClose is Run's defer), so wgMu is
	// uncontended and a dial can never race the teardown or double-build. Keep it
	// that way — an off-Run-goroutine dial would need a guard against rebuilding a
	// tunnel after wgClose (which nothing would ever Close).
	m.wgMu.Lock()
	if m.wgTun == nil {
		// Build (incl. endpoint resolution) gets its OWN DialTimeout budget.
		bctx, cancel := m.dialCtx(ctx)
		t, err := wgdial.New(bctx, *m.cfg.WireGuard)
		cancel()
		if err != nil {
			m.wgMu.Unlock()
			return nil, err
		}
		m.wgTun = t
	} else {
		// Reconnect: best-effort endpoint refresh (DNS failover / dynamic
		// endpoint), bounded by its OWN timeout so a stalled lookup can't consume
		// the dial's budget below — on failure the existing endpoint is kept.
		rctx, cancel := m.dialCtx(ctx)
		if err := m.wgTun.Reresolve(rctx); err != nil {
			log.Printf("irc[%s]: wireguard re-resolve endpoint: %v", m.cfg.Name, err)
		}
		cancel()
	}
	tun := m.wgTun
	m.wgMu.Unlock()

	// The dial gets a FRESH DialTimeout, independent of whatever the build or
	// re-resolve above consumed — a slow endpoint lookup must never hand the dial
	// an already-expired context (which would fail every reconnect identically).
	dctx, cancel := m.dialCtx(ctx)
	defer cancel()
	return tun.DialContext(dctx, addr)
}

// dialCtx derives a DialTimeout-bounded child of ctx (or a plain cancellable
// child when no timeout is configured), so each WireGuard phase — build,
// re-resolve, dial — gets its own independent budget.
func (m *Manager) dialCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.cfg.DialTimeout > 0 {
		return context.WithTimeout(ctx, m.cfg.DialTimeout)
	}
	return context.WithCancel(ctx)
}

// wgClose tears down the WireGuard tunnel if one was built. Called when Run
// returns (context cancelled), so a removed/stopped network doesn't leak the
// userspace device goroutines or its UDP socket.
func (m *Manager) wgClose() {
	m.wgMu.Lock()
	t := m.wgTun
	m.wgTun = nil
	m.wgMu.Unlock()
	if t != nil {
		t.Close()
	}
}

// wrapTLS negotiates TLS over an already-connected raw conn for addr. When
// TrustedFingerprints are configured, pinned fingerprints replace CA
// verification: the leaf certificate's SHA-256 must be in the trusted set
// (config validation already vetted the fingerprint format).
func (m *Manager) wrapTLS(ctx context.Context, conn net.Conn, addr string) (net.Conn, error) {
	tcfg := m.cfg.TLSConfig.Clone() // Clone on nil returns nil
	if tcfg == nil {
		tcfg = &tls.Config{}
	}
	if tcfg.ServerName == "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("irc: cannot derive TLS server name from %q: %w", addr, err)
		}
		tcfg.ServerName = host
	}
	fps, _ := fingerprintSet(m.cfg.TrustedFingerprints)
	presentsClientCert := len(tcfg.Certificates) > 0 || tcfg.GetClientCertificate != nil
	if fps != nil {
		tcfg.InsecureSkipVerify = true
		if presentsClientCert {
			// SASL EXTERNAL: we will send a client certificate. Verify the
			// pin DURING the handshake via VerifyConnection (still invoked
			// under InsecureSkipVerify) — Go runs it after the server
			// certificate arrives but BEFORE our Certificate is sent, so a
			// mismatched endpoint never receives that certificate or its
			// transcript-bound proof. Without a client certificate there is
			// nothing to leak, so we verify after the handshake instead (a
			// clean close rather than a mid-handshake TLS alert).
			tcfg.VerifyConnection = func(cs tls.ConnectionState) error {
				return verifyPinned(cs.PeerCertificates, fps)
			}
		}
	}
	tconn := tls.Client(conn, tcfg)
	hctx, hcancel := context.WithTimeout(ctx, m.cfg.DialTimeout)
	defer hcancel()
	if err := tconn.HandshakeContext(hctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tls handshake: %w", err)
	}
	if fps != nil && !presentsClientCert {
		if err := verifyPinned(tconn.ConnectionState().PeerCertificates, fps); err != nil {
			tconn.Close()
			return nil, err
		}
	}
	return tconn, nil
}

// verifyPinned checks the leaf certificate's SHA-256 against the trusted
// set. It is used both as a tls.Config.VerifyConnection hook (so a
// mismatch aborts the handshake before an EXTERNAL client certificate is
// sent) and for post-handshake verification when no client certificate is
// at stake.
func verifyPinned(certs []*x509.Certificate, fps map[string]bool) error {
	if len(certs) == 0 {
		return errors.New("tls: server presented no certificate")
	}
	sum := sha256.Sum256(certs[0].Raw)
	if fps[hex.EncodeToString(sum[:])] {
		return nil
	}
	return fmt.Errorf("tls: server certificate SHA-256 %s does not match any trusted fingerprint",
		hex.EncodeToString(sum[:]))
}
