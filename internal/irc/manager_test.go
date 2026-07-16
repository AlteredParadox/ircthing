package irc

import (
	"strconv"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	ircv4 "gopkg.in/irc.v4"
)

// testCfg returns a Config with timings scaled down for tests.
func testCfg(addr string) Config {
	return Config{
		Name:             "testnet",
		Addr:             addr,
		AllowPlaintext:   true,
		Nick:             "AlteredParadox",
		Backoff:          BackoffConfig{Min: 10 * time.Millisecond, Max: 50 * time.Millisecond},
		DialTimeout:      5 * time.Second,
		HandshakeTimeout: 5 * time.Second,
		PingInterval:     5 * time.Second,
		PingTimeout:      time.Second,
		SendBurst:        100, // keep flood protection out of the way
		SendInterval:     time.Millisecond,
	}
}

// srvConn wraps one accepted server-side connection. All helpers run on
// the test goroutine and fail the test on error or 5s of silence.
type srvConn struct {
	t *testing.T
	c net.Conn
	r *ircv4.Reader
	w *ircv4.Writer
}

// listen starts a listener whose accepted connections arrive on the
// returned channel, so tests can script consecutive connections.
func listen(t *testing.T, ln net.Listener) <-chan *srvConn {
	t.Helper()
	conns := make(chan *srvConn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(conns)
				return
			}
			conns <- &srvConn{t: t, c: c, r: ircv4.NewReader(c), w: ircv4.NewWriter(c)}
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return conns
}

func accept(t *testing.T, conns <-chan *srvConn) *srvConn {
	t.Helper()
	select {
	case s, ok := <-conns:
		if !ok {
			t.Fatal("listener closed")
		}
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the client to connect")
		return nil
	}
}

func (s *srvConn) read() *ircv4.Message {
	s.t.Helper()
	s.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err := s.r.ReadMessage()
	if err != nil {
		s.t.Fatalf("server read: %v", err)
	}
	return m
}

// readCmd reads messages, skipping others, until one with the given
// command arrives.
func (s *srvConn) readCmd(cmd string) *ircv4.Message {
	s.t.Helper()
	for {
		if m := s.read(); m.Command == cmd {
			return m
		}
	}
}

func (s *srvConn) send(line string) {
	s.t.Helper()
	s.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := s.w.Write(line); err != nil {
		s.t.Fatalf("server write %q: %v", line, err)
	}
}

// register plays the server side of a minimal CAP + registration
// exchange, ACKing whatever the client requests.
func (s *srvConn) register(nick string) {
	s.t.Helper()
	s.registerCaps(nick, "multi-prefix server-time")
}

func (s *srvConn) registerCaps(nick, lsCaps string) {
	s.t.Helper()
	s.readCmd("USER") // client sends CAP LS, [PASS,] NICK, USER
	s.send("CAP * LS :" + lsCaps)
	for {
		m := s.readCmd("CAP")
		switch m.Param(0) {
		case "REQ":
			s.send("CAP " + nick + " ACK :" + m.Trailing())
		case "END":
			s.send(":irc.test 001 " + nick + " :Welcome to the test network " + nick)
			return
		}
	}
}

// registerExternal plays the server side of a SASL EXTERNAL exchange.
func (s *srvConn) registerExternal(nick string) {
	s.t.Helper()
	s.readCmd("USER")
	s.send("CAP * LS :sasl=EXTERNAL")
	req := s.readCmd("CAP") // CAP REQ :sasl
	s.send("CAP " + nick + " ACK :" + req.Trailing())
	if m := s.readCmd("AUTHENTICATE"); m.Param(0) != "EXTERNAL" {
		s.t.Fatalf("AUTHENTICATE %q, want EXTERNAL", m.Param(0))
	}
	s.send("AUTHENTICATE +")
	if m := s.readCmd("AUTHENTICATE"); m.Param(0) != "+" {
		s.t.Fatalf("EXTERNAL response = %q, want + (empty)", m.Param(0))
	}
	s.send(":irc.test 903 " + nick + " :SASL successful")
	s.readCmd("CAP") // CAP END
	s.send(":irc.test 001 " + nick + " :Welcome " + nick)
}

// startManager runs the manager in the background for the whole test.
func startManager(t *testing.T, cfg Config) *Manager {
	t.Helper()
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return m
}

// waitState drains events until the wanted state change arrives.
func waitState(t *testing.T, m *Manager, want State) Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-m.Events():
			if ev.Kind == EventState && ev.State == want {
				return ev
			}
		case <-timeout:
			t.Fatalf("timed out waiting for state %v", want)
		}
	}
}

func TestManagerRegistersRespondsAndReconnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))

	// First connection: registration completes.
	s := accept(t, conns)
	s.register("AlteredParadox")
	ev := waitState(t, m, StateRegistered)
	if ev.Network != "testnet" {
		t.Fatalf("event network = %q, want testnet", ev.Network)
	}

	// Server PING is answered with a matching PONG.
	s.send("PING :abc123")
	if pong := s.readCmd("PONG"); pong.Param(0) != "abc123" {
		t.Fatalf("PONG param = %q, want abc123", pong.Param(0))
	}

	// Server messages surface as events.
	s.send(":nick!u@h PRIVMSG #chan :hello there")
	waitPrivmsg := func() *ircv4.Message {
		timeout := time.After(5 * time.Second)
		for {
			select {
			case ev := <-m.Events():
				if ev.Kind == EventMessage && ev.Msg.Command == "PRIVMSG" {
					return ev.Msg
				}
			case <-timeout:
				t.Fatal("timed out waiting for PRIVMSG event")
			}
		}
	}
	if got := waitPrivmsg(); got.Trailing() != "hello there" {
		t.Fatalf("PRIVMSG trailing = %q", got.Trailing())
	}

	// Dropping the connection triggers a reconnect that registers again.
	s.c.Close()
	waitState(t, m, StateDisconnected)
	s2 := accept(t, conns)
	s2.register("AlteredParadox")
	waitState(t, m, StateRegistered)
}

func TestManagerSend(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))

	if err := m.Send(newMsg("PRIVMSG", "#chan", "too early")); err != ErrNotConnected {
		t.Fatalf("Send before registration = %v, want ErrNotConnected", err)
	}

	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	if err := m.Send(newMsg("PRIVMSG", "#chan", "hello world")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := s.readCmd("PRIVMSG")
	if got.Param(0) != "#chan" || got.Trailing() != "hello world" {
		t.Fatalf("server saw %q", got.String())
	}
}

func TestManagerKeepaliveTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)

	cfg := testCfg(ln.Addr().String())
	cfg.PingInterval = 100 * time.Millisecond
	cfg.PingTimeout = 100 * time.Millisecond
	m := startManager(t, cfg)

	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	// Silence: the client must probe with PING, and when the server does
	// not answer, declare the connection dead.
	if ping := s.readCmd("PING"); ping.Param(0) == "" {
		t.Fatal("keepalive PING has no token")
	}
	ev := waitState(t, m, StateDisconnected)
	if ev.Err == nil || !strings.Contains(ev.Err.Error(), "ping timeout") {
		t.Fatalf("disconnect error = %v, want ping timeout", ev.Err)
	}
}

func TestManagerKeepaliveAnswered(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)

	cfg := testCfg(ln.Addr().String())
	cfg.PingInterval = 100 * time.Millisecond
	cfg.PingTimeout = 5 * time.Second
	m := startManager(t, cfg)

	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	// Answering the keepalive keeps the connection alive across several
	// idle periods.
	for i := 0; i < 3; i++ {
		ping := s.readCmd("PING")
		s.send("PONG :" + ping.Param(0))
	}
	select {
	case ev := <-m.Events():
		if ev.Kind == EventState && ev.State == StateDisconnected {
			t.Fatalf("unexpected disconnect: %v", ev.Err)
		}
	default:
	}
}

func TestManagerAutojoinsAfterRegistration(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	cfg := testCfg(ln.Addr().String())
	cfg.Channels = []string{"#go", "#linux"}
	m := startManager(t, cfg)

	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)
	for _, want := range cfg.Channels {
		if got := s.readCmd("JOIN").Param(0); got != want {
			t.Fatalf("JOIN %q, want %q", got, want)
		}
	}

	// Channels are re-joined after a reconnect.
	s.c.Close()
	s2 := accept(t, conns)
	s2.register("AlteredParadox")
	if got := s2.readCmd("JOIN").Param(0); got != "#go" {
		t.Fatalf("JOIN after reconnect = %q, want #go", got)
	}
}

func TestManagerRejoinsRuntimeChannels(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	cfg := testCfg(ln.Addr().String())
	cfg.Channels = []string{"#cfg"}
	m := startManager(t, cfg)

	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)
	if got := s.readCmd("JOIN").Param(0); got != "#cfg" {
		t.Fatalf("configured join = %q", got)
	}

	// Server echoes our runtime JOIN and a later PART of the configured
	// channel; NAMES fills the roster.
	s.send(":AlteredParadox!u@h JOIN #dyn")
	s.send(":srv 353 AlteredParadox = #dyn :@AlteredParadox alice")
	s.send(":srv 366 AlteredParadox #dyn :end")
	s.send(":AlteredParadox!u@h PART #cfg")

	// Roster reflects membership.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, members, ok := m.Channel("#dyn"); ok && len(members) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("roster never saw #dyn members")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Reconnect: the runtime join persists, the parted channel does not,
	// and stale roster state is gone.
	s.c.Close()
	waitState(t, m, StateDisconnected)
	if _, _, ok := m.Channel("#dyn"); ok {
		t.Fatal("roster kept state across disconnect")
	}
	s2 := accept(t, conns)
	s2.register("AlteredParadox")
	if got := s2.readCmd("JOIN").Param(0); got != "#dyn" {
		t.Fatalf("rejoin = %q, want #dyn", got)
	}
	s2.c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if msg, err := s2.r.ReadMessage(); err == nil && msg.Command == "JOIN" {
		t.Fatalf("unexpected second join: %s", msg.String())
	}
}

func TestManagerCapsAndNotify(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))

	s := accept(t, conns)
	s.register("AlteredParadox") // offers + ACKs multi-prefix, server-time
	waitState(t, m, StateRegistered)
	if !m.CapEnabled("multi-prefix") || !m.CapEnabled("server-time") {
		t.Fatal("negotiated caps not recorded")
	}
	if m.CapEnabled("away-notify") {
		t.Fatal("unoffered cap reported enabled")
	}

	// cap-notify NEW: the client requests the wanted new cap; ACK
	// enables it. DEL disables.
	s.send("CAP AlteredParadox NEW :away-notify example/unwanted")
	req := s.readCmd("CAP")
	if req.Param(0) != "REQ" || req.Trailing() != "away-notify" {
		t.Fatalf("REQ after NEW = %q", req.String())
	}
	s.send("CAP AlteredParadox ACK :away-notify")
	deadline := time.Now().Add(5 * time.Second)
	for !m.CapEnabled("away-notify") {
		if time.Now().After(deadline) {
			t.Fatal("away-notify never enabled after ACK")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.send("CAP AlteredParadox DEL :server-time")
	for m.CapEnabled("server-time") {
		if time.Now().After(deadline) {
			t.Fatal("server-time never disabled after DEL")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A valued NEW: the value from the announcement is published once
	// the capability is ACKed (the ACK itself carries only the name),
	// and a DEL drops it again.
	s.send(`CAP AlteredParadox NEW :draft/multiline=max-bytes=4096,max-lines=4`)
	if req := s.readCmd("CAP"); req.Trailing() != "draft/multiline" {
		t.Fatalf("REQ after valued NEW = %q", req.String())
	}
	s.send("CAP AlteredParadox ACK :draft/multiline")
	for m.CapValue("draft/multiline") != "max-bytes=4096,max-lines=4" {
		if time.Now().After(deadline) {
			t.Fatalf("value never published after ACK (got %q)", m.CapValue("draft/multiline"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.send("CAP AlteredParadox DEL :draft/multiline")
	for m.CapValue("draft/multiline") != "" || m.CapEnabled("draft/multiline") {
		if time.Now().After(deadline) {
			t.Fatal("value/cap never dropped after DEL")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestManagerMonitor(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))
	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)
	s.send(":irc.test 005 AlteredParadox MONITOR=3 :are supported by this server")
	deadline := time.Now().Add(5 * time.Second)
	for m.monitorLimit() != 3 {
		if time.Now().After(deadline) {
			t.Fatal("005 MONITOR never applied")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// SetMonitored clears then adds, clamped to the ISUPPORT limit of 3.
	m.SetMonitored([]string{"a", "b", "c", "d"})
	if got := s.readCmd("MONITOR"); got.Param(0) != "C" {
		t.Fatalf("first MONITOR = %q, want C", got.String())
	}
	add := s.readCmd("MONITOR")
	if add.Param(0) != "+" || add.Param(1) != "a,b,c" {
		t.Fatalf("MONITOR + = %q, want a,b,c (clamped)", add.String())
	}

	m.MonitorAdd("e")
	if got := s.readCmd("MONITOR"); got.Param(0) != "+" || got.Param(1) != "e" {
		t.Fatalf("MonitorAdd = %q", got.String())
	}
	m.MonitorRemove("a")
	if got := s.readCmd("MONITOR"); got.Param(0) != "-" || got.Param(1) != "a" {
		t.Fatalf("MonitorRemove = %q", got.String())
	}
}

func TestManagerLazyNames(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))

	s := accept(t, conns)
	s.registerCaps("AlteredParadox", "no-implicit-names")
	waitState(t, m, StateRegistered)

	// First request for a channel sends NAMES; a repeat is deduped.
	m.EnsureNames("#go")
	if got := s.readCmd("NAMES").Param(0); got != "#go" {
		t.Fatalf("NAMES target = %q", got)
	}
	m.EnsureNames("#go")
	m.EnsureNames("#other")
	if got := s.readCmd("NAMES").Param(0); got != "#other" {
		t.Fatalf("second NAMES = %q (dedup failed?)", got)
	}

	// A reconnect clears the requested set, so NAMES is re-sent.
	s.c.Close()
	waitState(t, m, StateDisconnected)
	s2 := accept(t, conns)
	s2.registerCaps("AlteredParadox", "no-implicit-names")
	waitState(t, m, StateRegistered)
	m.EnsureNames("#go")
	if got := s2.readCmd("NAMES").Param(0); got != "#go" {
		t.Fatalf("NAMES after reconnect = %q", got)
	}
}

func TestManagerEnsureNamesNoopWithoutCap(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))
	s := accept(t, conns)
	s.register("AlteredParadox") // does not offer no-implicit-names
	waitState(t, m, StateRegistered)

	m.EnsureNames("#go")
	// A marker must be next on the wire — no NAMES was sent.
	if err := m.Send(newMsg("PRIVMSG", "#go", "marker")); err != nil {
		t.Fatal(err)
	}
	if got := s.read(); got.Command != "PRIVMSG" {
		t.Fatalf("expected marker, got %q", got.String())
	}
}

func TestManagerRequestsChatHistory(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))

	s := accept(t, conns)
	s.registerCaps("AlteredParadox", "batch server-time message-tags draft/chathistory draft/event-playback")
	waitState(t, m, StateRegistered)
	s.send(":irc.test 005 AlteredParadox CHATHISTORY=50 MSGREFTYPES=msgid,timestamp :are supported by this server")

	// The limit clamps to the server's advertised maximum once 005 lands.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v, ok := m.isup.Raw("CHATHISTORY"); ok && v == "50" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("005 never applied")
		}
		time.Sleep(2 * time.Millisecond)
	}
	since := int64(1752570000000)

	// With a msgid and MSGREFTYPES support, the precise selector wins
	// (a timestamp selector loses same-millisecond messages).
	m.RequestChatHistory("#go", since, "abc123")
	got := s.readCmd("CHATHISTORY")
	if got.Param(0) != "AFTER" || got.Param(1) != "#go" ||
		got.Param(2) != "msgid=abc123" || got.Param(3) != "50" {
		t.Fatalf("wire = %q", got.String())
	}

	// Without a msgid the timestamp selector is used.
	m.RequestChatHistory("#go", since, "")
	got = s.readCmd("CHATHISTORY")
	wantTS := time.UnixMilli(since).UTC().Format("2006-01-02T15:04:05.000Z")
	if got.Param(2) != "timestamp="+wantTS {
		t.Fatalf("wire = %q", got.String())
	}
}

func TestManagerChatHistoryWithoutCapIsNoop(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))
	s := accept(t, conns)
	s.register("AlteredParadox") // no chathistory in LS
	waitState(t, m, StateRegistered)

	m.RequestChatHistory("#go", 1000, "x")
	// A marker message must be the next thing on the wire.
	if err := m.Send(newMsg("PRIVMSG", "#go", "marker")); err != nil {
		t.Fatal(err)
	}
	if got := s.read(); got.Command != "PRIVMSG" {
		t.Fatalf("expected marker PRIVMSG, got %q", got.String())
	}
}

func TestManagerPlaybackDoesNotTouchLiveState(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))
	s := accept(t, conns)
	s.registerCaps("AlteredParadox", "batch draft/chathistory draft/event-playback")
	waitState(t, m, StateRegistered)

	s.send(":AlteredParadox!u@h JOIN #go")
	s.send(":srv 353 AlteredParadox = #go :AlteredParadox")
	s.send(":srv 366 AlteredParadox #go :end")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, members, ok := m.Channel("#go"); ok && len(members) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never joined #go")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Replayed JOIN/NICK inside a chathistory batch must not alter the
	// roster or our own nick.
	s.send(":irc.test BATCH +r1 chathistory #go")
	s.send("@batch=r1 :ghost!u@h JOIN #go")
	s.send("@batch=r1 :AlteredParadox!u@h NICK ghost2")
	s.send(":irc.test BATCH -r1")
	// A live JOIN after the batch still lands (and proves processing).
	s.send(":carol!u@h JOIN #go")
	for {
		_, members, _ := m.Channel("#go")
		if len(members) == 2 {
			if members[0].Nick != "AlteredParadox" || members[1].Nick != "carol" {
				t.Fatalf("members = %v", members)
			}
			break
		}
		if len(members) > 2 {
			t.Fatalf("playback polluted the roster: %v", members)
		}
		if time.Now().After(deadline) {
			t.Fatal("live JOIN never applied")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if m.Nick() != "AlteredParadox" {
		t.Fatalf("playback NICK changed our nick to %q", m.Nick())
	}
}

func TestManagerAppliesISupport(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))

	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	// Defaults before 005.
	if !m.IsChannel("&local") || m.ChanTypes() != "#&" {
		t.Fatal("defaults wrong before 005")
	}
	s.send(":irc.test 005 AlteredParadox CHANTYPES=# PREFIX=(qaohv)~&@%+ CASEMAPPING=ascii :are supported by this server")
	deadline := time.Now().Add(5 * time.Second)
	for m.IsChannel("&local") {
		if time.Now().After(deadline) {
			t.Fatal("005 CHANTYPES never applied")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if m.ChanTypes() != "#" {
		t.Fatalf("ChanTypes = %q", m.ChanTypes())
	}

	// The roster consumes the advertised PREFIX for NAMES parsing.
	s.send(":AlteredParadox!u@h JOIN #x")
	s.send(":srv 353 AlteredParadox = #x :~boss AlteredParadox")
	s.send(":srv 366 AlteredParadox #x :end")
	for {
		if _, members, ok := m.Channel("#x"); ok && len(members) == 2 {
			if members[1].Nick != "boss" || members[1].Prefix != "~" {
				t.Fatalf("members = %v", members)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("roster never applied 005 PREFIX")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A reconnect resets ISUPPORT to defaults until the new 005.
	s.c.Close()
	waitState(t, m, StateDisconnected)
	s2 := accept(t, conns)
	s2.register("AlteredParadox")
	waitState(t, m, StateRegistered)
	if !m.IsChannel("&local") {
		t.Fatal("ISUPPORT not reset on reconnect")
	}
}

func TestManagerTracksNick(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))

	if m.Nick() != "" {
		t.Fatalf("Nick before registration = %q", m.Nick())
	}
	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)
	if m.Nick() != "AlteredParadox" {
		t.Fatalf("Nick = %q, want AlteredParadox", m.Nick())
	}

	// Our own nick change (as echoed by the server) is tracked;
	// someone else's is not.
	s.send(":carol!u@h NICK :carola")
	s.send(":AlteredParadox!u@h NICK :AlteredParadox2")
	deadline := time.Now().Add(5 * time.Second)
	for m.Nick() != "AlteredParadox2" {
		if time.Now().After(deadline) {
			t.Fatalf("Nick = %q, want AlteredParadox2", m.Nick())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestManagerTLS(t *testing.T) {
	cert, pool := selfSignedCert(t)
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(tcpLn, &tls.Config{Certificates: []tls.Certificate{cert}})
	conns := listen(t, ln)

	cfg := testCfg(tcpLn.Addr().String())
	cfg.AllowPlaintext = false
	cfg.TLS = true
	cfg.TLSConfig = &tls.Config{RootCAs: pool}
	m := startManager(t, cfg)

	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)
}

// TestManagerPresentsClientCert verifies the SASL EXTERNAL prerequisite:
// a configured TLS client certificate is presented to the server during
// the handshake (the server then matches its fingerprint to an account).
func TestManagerPresentsClientCert(t *testing.T) {
	serverCert, pool := selfSignedCert(t)
	clientCert, _ := selfSignedCert(t)

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gotCert := make(chan bool, 1)
	ln := tls.NewListener(tcpLn, &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequestClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			select {
			case gotCert <- len(cs.PeerCertificates) > 0:
			default:
			}
			return nil
		},
	})
	conns := listen(t, ln)

	cfg := testCfg(tcpLn.Addr().String())
	cfg.AllowPlaintext = false
	cfg.TLS = true
	cfg.TLSConfig = &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{clientCert}}
	cfg.SASL = &SASLConfig{Mechanism: "EXTERNAL"}
	m := startManager(t, cfg)

	s := accept(t, conns)
	// Server offers EXTERNAL; the client authenticates with an empty
	// response proven by the presented certificate.
	s.registerExternal("AlteredParadox")
	waitState(t, m, StateRegistered)

	select {
	case ok := <-gotCert:
		if !ok {
			t.Fatal("server did not receive a client certificate")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TLS never completed")
	}
}

func TestNewManagerRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		errSub string
	}{
		{"missing addr", Config{Nick: "n", AllowPlaintext: true}, "Addr"},
		{"missing nick", Config{Addr: "x:1", AllowPlaintext: true}, "Nick"},
		{"plaintext without opt-in", Config{Addr: "x:1", Nick: "n"}, "AllowPlaintext"},
		{"sasl without login", Config{Addr: "x:1", Nick: "n", TLS: true, SASL: &SASLConfig{Password: "p"}}, "login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewManager(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("err = %v, want containing %q", err, tc.errSub)
			}
		})
	}
}

// selfSignedCert generates a throwaway server certificate for 127.0.0.1
// and a pool trusting it.
func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ircthing test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(crand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func TestManagerCTCPAutoReply(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))
	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	s.send(":pal!u@h PRIVMSG AlteredParadox :\x01VERSION\x01")
	if got := s.readCmd("NOTICE"); got.Param(0) != "pal" || !strings.Contains(got.Trailing(), "VERSION ircthing") {
		t.Fatalf("CTCP reply = %q", got.String())
	}

	// Channel-wide CTCP is ignored; the next NOTICE must answer the
	// direct PING, not the channel VERSION.
	s.send(":pal!u@h PRIVMSG #go :\x01VERSION\x01")
	s.send(":pal!u@h PRIVMSG AlteredParadox :\x01PING xyz\x01")
	if got := s.readCmd("NOTICE"); !strings.Contains(got.Trailing(), "PING xyz") {
		t.Fatalf("reply = %q, want the PING answer (channel CTCP ignored)", got.String())
	}
}

func TestManagerWHOXOnJoin(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))
	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	// Without WHOX in ISUPPORT: no query after NAMES. The next command we
	// read must be the WHO that follows once WHOX IS advertised.
	s.send(":AlteredParadox!u@h JOIN #go")
	s.send(":srv 353 AlteredParadox = #go :alice AlteredParadox")
	s.send(":srv 366 AlteredParadox #go :End of /NAMES list")

	s.send(":srv 005 AlteredParadox WHOX :are supported by this server")
	s.send(":AlteredParadox!u@h JOIN #rust")
	s.send(":srv 353 AlteredParadox = #rust :bob AlteredParadox")
	s.send(":srv 366 AlteredParadox #rust :End of /NAMES list")
	who := s.readCmd("WHO")
	if who.Param(0) != "#rust" || who.Param(1) != "%tnfa,"+whoxToken {
		t.Fatalf("WHO = %q, want #rust %%tnfa,%s", who.String(), whoxToken)
	}

	// A NAMES refresh does not re-query; the 354 replies land in the
	// roster (away + account).
	s.send(":srv 354 AlteredParadox " + whoxToken + " bob G bobacct")
	s.send(":srv 366 AlteredParadox #rust :End of /NAMES list")
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, ms, ok := m.Channel("#rust")
		if ok && len(ms) == 2 && ms[1].Nick == "bob" && ms[1].Away && ms[1].Account == "bobacct" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("354 never applied: %v", ms)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Prove no second WHO was sent for either channel: solicit MONITOR
	// output and assert nothing WHO-shaped precedes it on the wire.
	m.MonitorAdd("x")
	for {
		got := s.read()
		if got.Command == "MONITOR" {
			break
		}
		if got.Command == "WHO" {
			t.Fatalf("unexpected extra WHO before MONITOR: %q", got.String())
		}
	}
}

func TestManagerUTF8Only(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))
	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	// Latin-1 é: invalid as UTF-8. Without UTF8ONLY it passes through
	// untouched (IRC is bytes, historically).
	if err := m.Send(newMsg("PRIVMSG", "#go", "caf\xe9 one")); err != nil {
		t.Fatal(err)
	}
	if got := s.readCmd("PRIVMSG"); got.Trailing() != "caf\xe9 one" {
		t.Fatalf("pre-UTF8ONLY trailing = %q", got.Trailing())
	}

	// Once the server advertises UTF8ONLY, invalid sequences are
	// replaced (the spec forbids sending non-UTF-8 at all).
	s.send(":srv 005 AlteredParadox UTF8ONLY :are supported by this server")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := m.isup.Raw("UTF8ONLY"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("005 UTF8ONLY never applied")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := m.Send(newMsg("PRIVMSG", "#go", "caf\xe9 two")); err != nil {
		t.Fatal(err)
	}
	if got := s.readCmd("PRIVMSG"); got.Trailing() != "caf\uFFFD two" {
		t.Fatalf("post-UTF8ONLY trailing = %q, want caf\uFFFD two", got.Trailing())
	}
}

func TestManagerQuitNickAffected(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))
	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	s.send(":AlteredParadox!u@h JOIN #a")
	s.send(":srv 353 AlteredParadox = #a :alice AlteredParadox")
	s.send(":srv 366 AlteredParadox #a :x")
	s.send(":AlteredParadox!u@h JOIN #b")
	s.send(":srv 353 AlteredParadox = #b :alice bob AlteredParadox")
	s.send(":srv 366 AlteredParadox #b :x")
	s.send(":alice!u@h QUIT :gone fishing")

	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case ev := <-m.Events():
			if ev.Kind == EventMessage && ev.Msg.Command == "QUIT" {
				if len(ev.Affected) != 2 || ev.Affected[0] != "#a" || ev.Affected[1] != "#b" {
					t.Fatalf("QUIT Affected = %v, want [#a #b]", ev.Affected)
				}
				// The roster processed it after the capture.
				if _, ms, _ := m.Channel("#b"); len(ms) != 2 {
					t.Fatalf("#b members after quit = %v", ms)
				}
				return
			}
			if ev.Kind == EventMessage && ev.Msg.Command != "QUIT" && len(ev.Affected) != 0 {
				t.Fatalf("%s carries Affected %v", ev.Msg.Command, ev.Affected)
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("QUIT event never arrived")
		}
	}
}

// The writer boundary drops any message carrying framing bytes, even a
// handshake line built from a tainted config that slipped past
// validation — the connection is torn down and the injected line never
// reaches the wire.
func TestWriterRejectsFramingInHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)

	cfg := testCfg(ln.Addr().String())
	cfg.Realname = "evil\r\nOPER admin secret" // injected extra command
	m := startManager(t, cfg)
	_ = m

	s := accept(t, conns)
	// CAP LS and NICK precede USER and are clean; the USER line is the
	// tainted one, so the connection dies at or before it. The server
	// must never see an OPER line. Read until the connection closes or a
	// short window elapses, asserting no injected command appears.
	s.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	r := ircv4.NewReader(s.c)
	for {
		msg, err := r.ReadMessage()
		if err != nil {
			break // connection torn down (expected) or deadline
		}
		if msg.Command == "OPER" {
			t.Fatalf("injected OPER reached the wire: %q", msg.String())
		}
		if msg.Command == "USER" {
			t.Fatalf("tainted USER line was written: %q", msg.String())
		}
	}
}

// Distinct channels that rfc1459 would merge but ascii keeps separate
// are both joined: the configured list must not be folded before the
// server's CASEMAPPING is known.
func TestManagerAutojoinPreservesDistinctChannels(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	cfg := testCfg(ln.Addr().String())
	cfg.Channels = []string{"#[x]", "#{x}"} // rfc1459-equal, ascii-distinct
	m := startManager(t, cfg)

	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	got := map[string]bool{}
	got[s.readCmd("JOIN").Param(0)] = true
	got[s.readCmd("JOIN").Param(0)] = true
	if !got["#[x]"] || !got["#{x}"] {
		t.Fatalf("joined = %v, want both #[x] and #{x}", got)
	}
}

// A user message sent immediately after registration never overtakes
// the autojoin JOIN on the wire: rejoins are queued (to the priority
// internal path) before the connection is exposed as send-ready.
func TestManagerJoinPrecedesUserMessage(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	cfg := testCfg(ln.Addr().String())
	cfg.Channels = []string{"#go"}
	m := startManager(t, cfg)

	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	// Queue a user PRIVMSG the instant we are send-ready.
	for {
		if err := m.Send(newMsg("PRIVMSG", "#go", "hi")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// The JOIN must reach the server before the PRIVMSG.
	sawJoin := false
	for i := 0; i < 10; i++ {
		msg := s.read()
		switch msg.Command {
		case "JOIN":
			sawJoin = true
		case "PRIVMSG":
			if !sawJoin {
				t.Fatal("PRIVMSG reached the server before its JOIN")
			}
			return
		}
	}
	t.Fatal("never saw the PRIVMSG")
}

// A server streaming an oversized incoming multiline batch is
// disconnected rather than allowed to grow memory without bound.
func TestManagerDisconnectsOversizedBatch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))

	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	s.send(":srv BATCH +b draft/multiline #go")
	line := "@batch=b;draft/multiline-concat :a!u@h PRIVMSG #go :" + strings.Repeat("z", 500)
	for i := 0; i < maxMLBatchBytes/500+4; i++ {
		s.send(line)
	}

	// The manager tears the connection down and reconnects (testCfg
	// backoff is short), so a second connection arrives.
	s2 := accept(t, conns)
	s2.register("AlteredParadox")
	waitState(t, m, StateRegistered)
}

func TestRedactRaw(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"PASS hunter2", "PASS <redacted>"},
		{"PASS s3cr3t\r\n", "PASS <redacted>"},
		{"AUTHENTICATE AGF0YgBwYXNzd29yZA==", "AUTHENTICATE <redacted>"},
		{"AUTHENTICATE PLAIN", "AUTHENTICATE <redacted>"}, // mechanism over-redacted, safe
		{":srv AUTHENTICATE Zm9vYmFy", "AUTHENTICATE <redacted>"},
		{"AUTHENTICATE +", "AUTHENTICATE +"}, // control token, not a secret
		{"AUTHENTICATE *", "AUTHENTICATE *"},
		{"NICK AlteredParadox", "NICK AlteredParadox"},
		{"PRIVMSG #go :hello world", "PRIVMSG #go :hello world"},
	}
	for _, tc := range cases {
		if got := redactRaw(tc.in); got != tc.want {
			t.Errorf("redactRaw(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// No credential material survives redaction of a PLAIN payload.
	for _, secret := range []string{"hunter2", "AGF0YgBwYXNzd29yZA=="} {
		if strings.Contains(redactRaw("PASS "+secret), secret) ||
			strings.Contains(redactRaw("AUTHENTICATE "+secret), secret) {
			t.Fatalf("secret %q survived redaction", secret)
		}
	}
}

func TestConfigRejectsOversizedRegistration(t *testing.T) {
	long := strings.Repeat("z", 600)
	base := func() Config { return Config{Addr: "x:1", Nick: "n", AllowPlaintext: true} }
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"realname", func(c *Config) { c.Realname = long }},
		{"username", func(c *Config) { c.Username = long }},
		{"pass", func(c *Config) { c.Pass = long }},
		{"nick", func(c *Config) { c.Nick = long }},
		{"channel", func(c *Config) { c.Channels = []string{"#" + long} }},
	}
	for _, tc := range cases {
		cfg := base()
		tc.mut(&cfg)
		if _, err := NewManager(cfg); err == nil || !strings.Contains(err.Error(), "too long") {
			t.Fatalf("%s: err = %v, want registration-too-long", tc.name, err)
		}
	}
	if _, err := NewManager(base()); err != nil {
		t.Fatalf("normal config rejected: %v", err)
	}
}

// An internal line that would exceed the line limit after serialization
// (here a PONG echoing an over-long server PING) tears the connection
// down at the writer rather than emitting a line the server rejects.
func TestManagerDisconnectsOnOversizedInternalLine(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m := startManager(t, testCfg(ln.Addr().String()))

	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	s.send("PING :" + strings.Repeat("x", 600)) // PONG echo > 512

	s2 := accept(t, conns) // torn down, reconnects
	s2.register("AlteredParadox")
	waitState(t, m, StateRegistered)
}

// A server spoofing an unbounded stream of self-JOINs is torn down
// rather than growing the never-reset rejoin set without bound.
func TestManagerBoundsJoinedSet(t *testing.T) {
	m, err := NewManager(Config{Addr: "x:1", Nick: "AlteredParadox", AllowPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}
	m.nick.Store("AlteredParadox")
	var terr error
	for i := 0; i <= maxJoinedChannels+1; i++ {
		terr = m.trackJoinIntent(ircv4.MustParseMessage(":AlteredParadox!u@h JOIN #c" + strconv.Itoa(i)))
		if terr != nil {
			break
		}
	}
	if terr == nil {
		t.Fatal("no error after exceeding the joined-channel cap")
	}
	if len(m.joined) > maxJoinedChannels {
		t.Fatalf("joined = %d, want <= %d", len(m.joined), maxJoinedChannels)
	}
}

// A spoofed self-JOIN with a framing or over-length channel name is not
// stored as rejoin intent, so it cannot brick the network on reconnect.
func TestTrackJoinRejectsPoisonedChannel(t *testing.T) {
	m, err := NewManager(Config{Addr: "x:1", Nick: "AlteredParadox", AllowPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}
	m.nick.Store("AlteredParadox")

	bad := []string{
		"#" + strings.Repeat("A", 600),    // over the 512 line limit at rejoin
		"#chan\r\nOPER a b",               // framing injection
		"#nul\x00byte",                    // NUL
		"notachannel",                     // not a channel name
	}
	for _, ch := range bad {
		if err := m.trackJoinIntent(ircv4.MustParseMessage(":AlteredParadox!u@h JOIN " + ch)); err != nil {
			t.Fatalf("trackJoinIntent(%q) errored: %v", ch, err)
		}
	}
	if len(m.joined) != 0 {
		t.Fatalf("poisoned channels stored: %v", m.joined)
	}
	// A normal channel is still tracked.
	if err := m.trackJoinIntent(ircv4.MustParseMessage(":AlteredParadox!u@h JOIN #good")); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.joined["#good"]; !ok {
		t.Fatalf("valid channel not tracked: %v", m.joined)
	}
}

func TestISupportKeyCap(t *testing.T) {
	s := newISupport()
	for i := 0; i < maxISupportKeys+100; i++ {
		s.handle(ircv4.MustParseMessage(":srv 005 me K" + strconv.Itoa(i) + "=x :are supported"))
	}
	s.mu.Lock()
	n := len(s.raw)
	s.mu.Unlock()
	if n > maxISupportKeys {
		t.Fatalf("ISUPPORT raw map = %d keys, want <= %d", n, maxISupportKeys)
	}
}

func TestCapListCap(t *testing.T) {
	dst := map[string]string{}
	var fields []string
	for i := 0; i < maxAdvertisedCaps+100; i++ {
		fields = append(fields, "cap"+strconv.Itoa(i))
	}
	parseCapList(strings.Join(fields, " "), dst)
	if len(dst) > maxAdvertisedCaps {
		t.Fatalf("cap map = %d, want <= %d", len(dst), maxAdvertisedCaps)
	}
}

// Under no-implicit-names, a fresh self-JOIN must re-arm EnsureNames so
// the roster reloads after a part/rejoin (or channel forward/cycle).
func TestEnsureNamesReArmedOnRejoin(t *testing.T) {
	m, err := NewManager(Config{Addr: "x:1", Nick: "AlteredParadox", AllowPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}
	m.nick.Store("AlteredParadox")
	m.registered.Store(true)
	m.setCaps(map[string]bool{"no-implicit-names": true})

	drainNames := func() int {
		n := 0
		for {
			select {
			case msg := <-m.out:
				if msg.Command == "NAMES" {
					n++
				}
			default:
				return n
			}
		}
	}

	m.EnsureNames("#c")
	if drainNames() != 1 {
		t.Fatal("first EnsureNames did not send NAMES")
	}
	m.EnsureNames("#c") // already requested -> no send
	if drainNames() != 0 {
		t.Fatal("EnsureNames re-sent NAMES without a rejoin")
	}
	// A self-JOIN (part/rejoin cycle) re-arms it.
	if err := m.trackJoinIntent(ircv4.MustParseMessage(":AlteredParadox!u@h JOIN #c")); err != nil {
		t.Fatal(err)
	}
	m.EnsureNames("#c")
	if drainNames() != 1 {
		t.Fatal("EnsureNames did not re-fetch NAMES after a rejoin")
	}
}

// A dropped NAMES (full send queue) is retried on the next view rather
// than leaving the roster permanently empty.
func TestEnsureNamesRetriesOnDroppedSend(t *testing.T) {
	m, err := NewManager(Config{Addr: "x:1", Nick: "AlteredParadox", AllowPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}
	m.registered.Store(true)
	m.setCaps(map[string]bool{"no-implicit-names": true})

	// Fill the send queue so the NAMES send is dropped.
	for len(m.out) < cap(m.out) {
		m.out <- newMsg("PING", "x")
	}
	m.EnsureNames("#c") // dropped; must NOT mark requested
	// Drain and retry with room available.
	for len(m.out) > 0 {
		<-m.out
	}
	m.EnsureNames("#c")
	got := 0
	for len(m.out) > 0 {
		if (<-m.out).Command == "NAMES" {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("NAMES sends after retry = %d, want 1 (dropped request retried)", got)
	}
}
