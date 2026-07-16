package hub

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

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
