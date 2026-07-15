package irc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	ircv4 "gopkg.in/irc.v4"
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
	cfg        Config
	events     chan Event
	out        chan *ircv4.Message
	registered atomic.Bool
	nick       atomic.Value // string: current nick once registered
	caps       atomic.Value // map[string]bool, copy-on-write: enabled capabilities
	isup       *isupport
	roster     *roster
	// joined is the set of channels to (re)join after registration:
	// the configured ones plus runtime JOINs, minus runtime PARTs (a KICK
	// deliberately does not remove the intent — bouncers rejoin). Only
	// the run goroutine touches it.
	joined map[string]string // lower(channel) -> original casing
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
		cfg:    cfg,
		events: make(chan Event, 256),
		out:    make(chan *ircv4.Message, 64),
		isup:   isup,
		roster: newRoster(isup),
		joined: make(map[string]string),
	}
	for _, ch := range cfg.Channels {
		m.joined[isup.Fold(ch)] = ch
	}
	return m, nil
}

// IsChannel reports whether target names a channel per the server's
// ISUPPORT CHANTYPES.
func (m *Manager) IsChannel(target string) bool {
	return m.isup.IsChannel(target)
}

// ChanTypes returns the server's channel prefix characters.
func (m *Manager) ChanTypes() string {
	return m.isup.ChanTypes()
}

// Channel returns the topic and member snapshot of a joined channel;
// ok is false for channels we are not in (or before registration).
func (m *Manager) Channel(name string) (topic string, members []Member, ok bool) {
	return m.roster.channel(name)
}

// CapEnabled reports whether an IRCv3 capability was negotiated on the
// current connection (kept current through cap-notify NEW/DEL).
func (m *Manager) CapEnabled(name string) bool {
	caps, _ := m.caps.Load().(map[string]bool)
	return caps[name]
}

func (m *Manager) setCaps(caps map[string]bool) {
	m.caps.Store(caps)
}

// handleCapNotify processes CAP NEW/DEL/ACK after registration
// (cap-notify, implicitly enabled by CAP LS 302). Returns messages to
// send. sasl appearing in NEW is ignored — mid-session re-auth is out of
// scope here.
func (m *Manager) handleCapNotify(in *ircv4.Message) []*ircv4.Message {
	if len(in.Params) < 3 {
		return nil
	}
	list := in.Params[len(in.Params)-1]
	switch strings.ToUpper(in.Params[1]) {
	case "NEW":
		var req []string
		for _, tok := range strings.Fields(list) {
			name, _, _ := strings.Cut(tok, "=")
			if wantedCapSet[name] && !m.CapEnabled(name) {
				req = append(req, name)
			}
		}
		if len(req) == 0 {
			return nil
		}
		sort.Strings(req)
		return []*ircv4.Message{newMsg("CAP", "REQ", strings.Join(req, " "))}
	case "ACK", "DEL":
		enable := strings.ToUpper(in.Params[1]) == "ACK"
		old, _ := m.caps.Load().(map[string]bool)
		caps := make(map[string]bool, len(old))
		for k, v := range old {
			caps[k] = v
		}
		for _, tok := range strings.Fields(list) {
			name, removed := strings.CutPrefix(tok, "-")
			if enable && !removed {
				caps[name] = true
			} else {
				delete(caps, name)
			}
		}
		m.setCaps(caps)
	}
	return nil
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
	if !m.registered.Load() {
		return ErrNotConnected
	}
	select {
	case m.out <- msg:
		return nil
	default:
		return ErrSendQueueFull
	}
}

// Run connects and reconnects until ctx is canceled.
func (m *Manager) Run(ctx context.Context) error {
	bo := newBackoff(m.cfg.Backoff)
	for {
		m.emit(ctx, Event{Kind: EventState, State: StateConnecting})
		err := m.runOnce(ctx, bo)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m.emit(ctx, Event{Kind: EventState, State: StateDisconnected, Err: err})
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bo.next()):
		}
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
func (m *Manager) runOnce(ctx context.Context, bo *backoff) error {
	// Drop messages queued for a previous connection so stale lines are
	// not written into the middle of the new registration.
drain:
	for {
		select {
		case <-m.out:
		default:
			break drain
		}
	}

	conn, err := m.dial(ctx)
	if err != nil {
		return err
	}
	// Everything below is scoped to this connection: canceling cctx
	// closes the socket, which unblocks both loops.
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		<-cctx.Done()
		conn.Close()
	}()

	r := ircv4.NewReader(conn)
	w := ircv4.NewWriter(conn)
	internal := make(chan *ircv4.Message, 16)
	go m.writeLoop(cctx, cancel, conn, w, internal)

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

	// Registration. The whole exchange must finish within
	// HandshakeTimeout.
	hs := newHandshake(&m.cfg)
	if err := send(hs.start()); err != nil {
		return err
	}
	deadline := time.Now().Add(m.cfg.HandshakeTimeout)
	for {
		conn.SetReadDeadline(deadline)
		in, err := r.ReadMessage()
		if err != nil {
			return fmt.Errorf("registration: %w", connError(cctx, err))
		}
		out, done, err := hs.handle(in)
		if err != nil {
			return fmt.Errorf("registration: %w", err)
		}
		m.emit(ctx, Event{Kind: EventMessage, Msg: in})
		if err := send(out); err != nil {
			return err
		}
		if done {
			break
		}
	}

	m.registered.Store(true)
	defer m.registered.Store(false)
	m.nick.Store(hs.nick)
	m.setCaps(hs.enabled)
	bo.reset()
	m.emit(ctx, Event{Kind: EventState, State: StateRegistered})

	m.isup.reset()   // ISUPPORT is per-connection; 005 follows shortly
	m.roster.clear() // membership is per-connection state
	defer m.roster.clear()
	rejoin := make([]string, 0, len(m.joined))
	for _, ch := range m.joined {
		rejoin = append(rejoin, ch)
	}
	sort.Strings(rejoin)
	for _, ch := range rejoin {
		if err := send([]*ircv4.Message{newMsg("JOIN", ch)}); err != nil {
			return err
		}
	}

	// Steady state. The read deadline doubles as the keepalive timer:
	// after PingInterval of silence we PING, and if the server stays
	// silent for PingTimeout more, the connection is dead.
	pinged := false
	for {
		idle := m.cfg.PingInterval
		if pinged {
			idle = m.cfg.PingTimeout
		}
		conn.SetReadDeadline(time.Now().Add(idle))
		in, err := r.ReadMessage()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if !pinged {
					pinged = true
					if err := send([]*ircv4.Message{newMsg("PING", "keepalive")}); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("ping timeout: no traffic for %s after keepalive PING", m.cfg.PingTimeout)
			}
			return connError(cctx, err)
		}
		pinged = false
		if in.Command == "PING" {
			if err := send([]*ircv4.Message{newMsg("PONG", in.Params...)}); err != nil {
				return err
			}
		}
		if in.Command == "CAP" { // cap-notify: NEW/DEL after registration
			if out := m.handleCapNotify(in); len(out) > 0 {
				if err := send(out); err != nil {
					return err
				}
			}
		}
		// Track our own nick changes, compared under the server's
		// casemapping.
		if in.Command == "NICK" && in.Prefix != nil && m.isup.FoldEqual(in.Prefix.Name, m.Nick()) {
			if n := in.Param(0); n != "" {
				m.nick.Store(n)
			}
		}
		m.isup.handle(in) // 005, ignored otherwise
		m.roster.handle(m.Nick(), in)
		m.trackJoinIntent(in)
		m.emit(ctx, Event{Kind: EventMessage, Msg: in})
	}
}

// trackJoinIntent keeps the rejoin set in step with our own JOINs and
// PARTs. Runs on the read-loop goroutine, which is the only writer of
// m.joined after construction.
func (m *Manager) trackJoinIntent(in *ircv4.Message) {
	if in.Prefix == nil || !m.isup.FoldEqual(in.Prefix.Name, m.Nick()) {
		return
	}
	switch in.Command {
	case "JOIN":
		if ch := in.Param(0); ch != "" {
			m.joined[m.isup.Fold(ch)] = ch
		}
	case "PART":
		delete(m.joined, m.isup.Fold(in.Param(0)))
	}
}

// writeLoop is the per-connection writer goroutine: it serializes
// handshake/PONG traffic and user messages onto the socket through the
// flood-protection token bucket. On write failure it cancels the
// connection context with the error, which the read loop reports.
func (m *Manager) writeLoop(ctx context.Context, cancel context.CancelCauseFunc, conn net.Conn, w *ircv4.Writer, internal <-chan *ircv4.Message) {
	tb := newTokenBucket(m.cfg.SendBurst, m.cfg.SendInterval)
	for {
		var out *ircv4.Message
		select {
		case <-ctx.Done():
			return
		case out = <-internal:
		case out = <-m.out:
		}
		if err := tb.wait(ctx); err != nil {
			return
		}
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if err := w.WriteMessage(out); err != nil {
			cancel(fmt.Errorf("write: %w", err))
			return
		}
	}
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

func (m *Manager) dial(ctx context.Context) (net.Conn, error) {
	d := &net.Dialer{Timeout: m.cfg.DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", m.cfg.Addr)
	if err != nil {
		return nil, err
	}
	if !m.cfg.TLS {
		return conn, nil
	}
	tcfg := m.cfg.TLSConfig.Clone() // Clone on nil returns nil
	if tcfg == nil {
		tcfg = &tls.Config{}
	}
	if tcfg.ServerName == "" {
		host, _, err := net.SplitHostPort(m.cfg.Addr)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("irc: cannot derive TLS server name from %q: %w", m.cfg.Addr, err)
		}
		tcfg.ServerName = host
	}
	tconn := tls.Client(conn, tcfg)
	hctx, hcancel := context.WithTimeout(ctx, m.cfg.DialTimeout)
	defer hcancel()
	if err := tconn.HandshakeContext(hctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tls handshake: %w", err)
	}
	return tconn, nil
}
