package hub

import (
	"strconv"
	"strings"
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	ircv4 "gopkg.in/irc.v4"

	"ircthing/internal/irc"
	"ircthing/internal/store"
)

// Network CRUD over the session protocol. StartNetwork dials for real,
// so definitions point at a closed local port — the manager just cycles
// connect/backoff in the background until the root context ends.
func TestNetworkManagement(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	h.UseRoot(ctx, &wg)
	defer wg.Wait() // after cancel: reap the manager goroutines
	defer cancel()
	s := h.NewSession()
	defer s.Close()
	ctxb := context.Background()

	// Seed history that a rename must carry over.
	if _, err := h.store.Append(ctxb, "alpha", "#x", store.Message{
		Time: time.Now(), Sender: "a", Command: "PRIVMSG", Raw: ":a PRIVMSG #x :hi",
	}); err != nil {
		t.Fatal(err)
	}

	cfg := func(name string) json.RawMessage {
		j, _ := json.Marshal(map[string]any{
			"name": name, "addr": "127.0.0.1:1", "allow_plaintext": true, "nick": "AlteredParadox",
		})
		return j
	}

	// Add.
	s.Handle(ctxb, request(t, "put_network", 1, PutNetworkReq{Config: cfg("alpha")}))
	recv(t, s, "ok", "networks_changed", "state")
	// Duplicate add is refused.
	s.Handle(ctxb, request(t, "put_network", 2, PutNetworkReq{Config: cfg("alpha")}))
	if env := recv(t, s, "error", "networks_changed", "state"); env.Seq != 2 {
		t.Fatalf("dup add: got seq %d", env.Seq)
	}
	// Malformed config is refused.
	s.Handle(ctxb, request(t, "put_network", 3, PutNetworkReq{Config: json.RawMessage(`{"nick":"x"}`)}))
	recv(t, s, "error", "networks_changed", "state")

	// List.
	s.Handle(ctxb, request(t, "get_networks", 4, nil))
	nets := decode[NetworksData](t, recv(t, s, "networks", "networks_changed", "state"))
	if len(nets.Networks) != 1 || nets.Networks[0].Name != "alpha" {
		t.Fatalf("networks = %+v", nets.Networks)
	}

	// Rename: history follows, the old name is announced as removed.
	s.Handle(ctxb, request(t, "put_network", 5, PutNetworkReq{OldName: "alpha", Config: cfg("beta")}))
	recv(t, s, "network_removed", "state", "networks_changed")
	recv(t, s, "ok", "state", "networks_changed")
	if msgs, err := h.store.Latest(ctxb, "beta", "#x", 5); err != nil || len(msgs) != 1 {
		t.Fatalf("history after rename: %v, %v", msgs, err)
	}

	// Delete: definition, history, and connection all go.
	s.Handle(ctxb, request(t, "delete_network", 6, NetworkRef{Network: "beta"}))
	env := recv(t, s, "ok", "state", "networks_changed", "network_removed")
	if env.Seq != 6 {
		t.Fatalf("delete reply seq = %d", env.Seq)
	}
	s.Handle(ctxb, request(t, "get_networks", 7, nil))
	nets = decode[NetworksData](t, recv(t, s, "networks", "networks_changed", "network_removed", "state"))
	if len(nets.Networks) != 0 {
		t.Fatalf("networks after delete = %+v", nets.Networks)
	}
	if msgs, _ := h.store.Latest(ctxb, "beta", "#x", 5); len(msgs) != 0 {
		t.Fatalf("history survived delete: %v", msgs)
	}
}

// An invalid edit must be rejected before anything is persisted or
// stopped: the stored definition and the running connection survive.
func TestPutNetworkValidatesFirst(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	h.UseRoot(ctx, &wg)
	defer wg.Wait() // after cancel: reap the manager goroutines
	defer cancel()
	s := h.NewSession()
	defer s.Close()
	ctxb := context.Background()

	good := json.RawMessage(`{"name":"alpha","addr":"127.0.0.1:1","allow_plaintext":true,"nick":"AlteredParadox"}`)
	s.Handle(ctxb, request(t, "put_network", 1, PutNetworkReq{Config: good}))
	recv(t, s, "ok", "networks_changed", "state")

	bad := []json.RawMessage{
		// plaintext without the explicit opt-in
		json.RawMessage(`{"name":"alpha","addr":"127.0.0.1:1","nick":"AlteredParadox"}`),
		// invalid proxy URL
		json.RawMessage(`{"name":"alpha","addr":"127.0.0.1:1","allow_plaintext":true,"nick":"AlteredParadox","proxy":"ftp://x:1"}`),
		// unreadable client certificate
		json.RawMessage(`{"name":"alpha","addr":"127.0.0.1:1","tls":true,"nick":"AlteredParadox","sasl":{"mechanism":"EXTERNAL","cert_file":"/nonexistent.pem","key_file":"/nonexistent.key"}}`),
		// realname long enough to overflow the registration line
		json.RawMessage(`{"name":"alpha","addr":"127.0.0.1:1","allow_plaintext":true,"nick":"AlteredParadox","realname":"` + strings.Repeat("z", 600) + `"}`),
	}
	for i, cfg := range bad {
		s.Handle(ctxb, request(t, "put_network", int64(10+i), PutNetworkReq{OldName: "alpha", Config: cfg}))
		if env := recv(t, s, "error", "networks_changed", "state", "network_removed"); env.Seq != int64(10+i) {
			t.Fatalf("bad edit %d: got seq %d", i, env.Seq)
		}
	}

	// Stored definition unchanged, network still running.
	s.Handle(ctxb, request(t, "get_networks", 20, nil))
	nets := decode[NetworksData](t, recv(t, s, "networks", "networks_changed", "state"))
	if len(nets.Networks) != 1 || nets.Networks[0].Name != "alpha" {
		t.Fatalf("networks = %+v", nets.Networks)
	}
	var got map[string]any
	if err := json.Unmarshal(nets.Networks[0].Config, &got); err != nil || got["proxy"] != nil {
		t.Fatalf("config mutated: %s (%v)", nets.Networks[0].Config, err)
	}
	h.mu.Lock()
	_, running := h.procs["alpha"]
	h.mu.Unlock()
	if !running {
		t.Fatal("network no longer running after rejected edits")
	}
}

// Case-variant targets resolve to one buffer: an echoed "#Go" message
// lands in the existing "#go" scrollback instead of splitting it.
func TestBufferCanonicalization(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Msg: ircv4.MustParseMessage(line), Time: time.Now()}
	}
	conn.ch <- ev(":alice!u@h PRIVMSG #go :first")
	conn.ch <- ev(":alice!u@h PRIVMSG #GO :second")
	conn.ch <- ev(":bob[x]!u@h PRIVMSG AlteredParadox :query")
	conn.ch <- ev(":bob{x}!u@h PRIVMSG AlteredParadox :query again") // rfc1459-equal sender

	// Wait for the LAST event (the rfc1459-equal query sender) to be
	// consumed before asserting counts — the event consumer works
	// through the buffered channel asynchronously, so gating on #go
	// alone would race the query buffer's creation.
	deadline := time.Now().Add(5 * time.Second)
	for {
		gm, err := h.store.Latest(ctxb, "libera", "#go", 10)
		if err != nil {
			t.Fatal(err)
		}
		qm, err := h.store.Latest(ctxb, "libera", "bob[x]", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(gm) == 2 && len(qm) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for merged messages: #go=%d bob[x]=%d, want 2 and 2", len(gm), len(qm))
		}
		time.Sleep(5 * time.Millisecond)
	}
	bufs, err := h.store.Buffers(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(bufs))
	for _, b := range bufs {
		names = append(names, b.Target)
	}
	if len(bufs) != 2 { // #go and bob[x] — no case-variant duplicates
		t.Fatalf("buffers = %v, want exactly [#go bob[x]]", names)
	}
}

// A buffer closed from the UI is not resurrected by straggler inbound
// traffic that was already in flight; after the grace window a genuinely
// new message can reopen it.
func TestCloseBufferResurrectionGuard(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Msg: ircv4.MustParseMessage(line), Time: time.Now()}
	}
	// Seed a channel buffer.
	conn.ch <- ev(":alice!u@h PRIVMSG #go :first")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#go", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed message never persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Close it (session path), then a straggler arrives with a case
	// variant of the channel — it must not re-create the buffer.
	s := h.NewSession()
	defer s.Close()
	s.Handle(ctxb, request(t, "close_buffer", 1, BufferRef{Network: "libera", Buffer: "#go"}))
	recv(t, s, "ok", "buffer_closed")
	conn.ch <- ev(":bob!u@h PRIVMSG #GO :straggler")

	// Give the event loop time to (not) recreate it.
	time.Sleep(200 * time.Millisecond)
	bufs, err := h.store.Buffers(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 0 {
		t.Fatalf("closed buffer resurrected by straggler: %+v", bufs)
	}
}

// The MONITOR presence map is bounded: a server streaming endless unique
// nicks cannot grow it without limit.
func TestPresenceMapBounded(t *testing.T) {
	h := newTestHub(t)
	for i := 0; i < maxPresenceEntries+500; i++ {
		m := ircv4.MustParseMessage(":srv 730 AlteredParadox :nick" + strconv.Itoa(i))
		h.updatePresence("libera", m, true)
	}
	h.mu.Lock()
	n := len(h.presence["libera"])
	h.mu.Unlock()
	if n > maxPresenceEntries {
		t.Fatalf("presence map = %d entries, want <= %d", n, maxPresenceEntries)
	}
}

// The WHOIS accumulator is bounded against a server streaming unique
// nicks that never terminate with 318.
func TestWhoisMapBounded(t *testing.T) {
	whois := make(map[string]*WhoisData)
	for i := 0; i < maxOpenWhois+200; i++ {
		ev := irc.Event{Network: "libera", Kind: irc.EventMessage,
			Msg: ircv4.MustParseMessage(":srv 311 AlteredParadox nick" + strconv.Itoa(i) + " u h * :real")}
		newTestHub(t).accumulateWhois(ev, whois)
	}
	if len(whois) > maxOpenWhois {
		t.Fatalf("whois map = %d entries, want <= %d", len(whois), maxOpenWhois)
	}
}

// A far-future server-time is distrusted at ingestion, so it cannot sort
// a message to the bottom of scrollback or poison the read-marker clamp.
func TestMessageTimeClampsFuture(t *testing.T) {
	now := time.Now()
	ev := func(tag string) irc.Event {
		return irc.Event{
			Network: "n", Kind: irc.EventMessage, Time: now,
			Msg: ircv4.MustParseMessage("@time=" + tag + " :a!u@h PRIVMSG #c :hi"),
		}
	}
	// A plausible (small-skew) tag is honored.
	near := now.Add(time.Minute).UTC().Format(time.RFC3339Nano)
	if got := messageTime(ev(near)); got.Sub(now.Add(time.Minute)).Abs() > time.Second {
		t.Fatalf("near-future tag not honored: %v", got)
	}
	// A far-future tag falls back to the receipt time.
	far := now.Add(100 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if got := messageTime(ev(far)); got.After(now.Add(serverTimeSkew)) {
		t.Fatalf("far-future tag not clamped: %v", got)
	}
	// A past tag (chathistory replay) is honored.
	past := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if got := messageTime(ev(past)); got.After(now) {
		t.Fatalf("past tag not honored: %v", got)
	}
}

// An empty chathistory BATCH reference must not create histBatches[""],
// which would classify all un-batched live traffic as replay (persisted
// but never broadcast).
func TestEmptyBatchRefIgnored(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")

	s := h.NewSession()
	defer s.Close()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Msg: ircv4.MustParseMessage(line), Time: time.Now()}
	}
	// Bare "+" reference — must be ignored, not stored under "".
	conn.ch <- ev(":srv BATCH + chathistory #go")
	// A normal live message must still be broadcast (not swallowed as replay).
	conn.ch <- ev(":alice!u@h PRIVMSG #go :live and visible")
	got := decode[EventData](t, recv(t, s, "event"))
	if got.Raw == "" || got.Sender != "alice" {
		t.Fatalf("live message not broadcast: %+v", got)
	}
}
