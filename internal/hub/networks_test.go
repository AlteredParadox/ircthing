package hub

import (
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

	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs, err := h.store.Latest(ctxb, "libera", "#go", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("#go has %d messages, want both casings merged", len(msgs))
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
