package irc

import (
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
	s.readCmd("USER") // client sends CAP LS, [PASS,] NICK, USER
	s.send("CAP * LS :multi-prefix server-time")
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

func TestNewManagerRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		errSub string
	}{
		{"missing addr", Config{Nick: "n", AllowPlaintext: true}, "Addr"},
		{"missing nick", Config{Addr: "x:1", AllowPlaintext: true}, "Nick"},
		{"plaintext without opt-in", Config{Addr: "x:1", Nick: "n"}, "AllowPlaintext"},
		{"sasl without login", Config{Addr: "x:1", Nick: "n", TLS: true, SASL: &SASLPlain{Password: "p"}}, "login"},
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
