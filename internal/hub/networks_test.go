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
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ircv4 "gopkg.in/irc.v4"

	"ircthing/internal/irc"
	"ircthing/internal/store"
)

// editChannelList is the autojoin read-modify-write core. It applies the WHOLE
// wanted slice in one pass (each name folded once), so a hostile comma-list
// can't turn one JOIN/PART line into a per-token fold/store storm (F1/F2).
func TestEditChannelList(t *testing.T) {
	fold := strings.ToLower
	tests := []struct {
		name    string
		chans   []string
		wanted  []string
		add     bool
		want    []string
		changed bool
	}{
		{"add new", []string{"#a"}, []string{"#b"}, true, []string{"#a", "#b"}, true},
		{"add dedups against stored fold", []string{"#Chan"}, []string{"#chan"}, true, []string{"#Chan"}, false},
		{"add batch keeps order, skips existing", []string{"#a"}, []string{"#a", "#b", "#c"}, true, []string{"#a", "#b", "#c"}, true},
		{"add batch dedups within wanted", nil, []string{"#x", "#X", "#y"}, true, []string{"#x", "#y"}, true},
		{"remove one", []string{"#a", "#b"}, []string{"#a"}, false, []string{"#b"}, true},
		{"remove folds", []string{"#Chan", "#b"}, []string{"#chan"}, false, []string{"#b"}, true},
		{"remove batch (combined PART)", []string{"#a", "#b", "#c"}, []string{"#a", "#c"}, false, []string{"#b"}, true},
		{"remove absent is a no-op", []string{"#a"}, []string{"#zzz"}, false, []string{"#a"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := editChannelList(tt.chans, tt.wanted, tt.add, fold)
			if changed != tt.changed {
				t.Fatalf("changed = %v, want %v", changed, tt.changed)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}

	// The count cap holds: a full list rejects further additions without change.
	full := make([]string, maxPersistedChannels)
	for i := range full {
		full[i] = "#c" + strconv.Itoa(i)
	}
	if got, changed := editChannelList(full, []string{"#new"}, true, fold); changed || len(got) != maxPersistedChannels {
		t.Fatalf("count cap not enforced: changed=%v len=%d", changed, len(got))
	}
}

// The hub keys histBatches by a rule that must match internal/irc's
// clampBatchRef exactly: a normal ref passes through; an over-512 ref is HASHED
// to a bounded key (not truncated — that aliases distinct refs — nor rejected —
// that turned a long batch into live traffic). Distinct over-limit refs must map
// to distinct keys so a long batch still correlates. Locks the hub side.
func TestClampBatchRef(t *testing.T) {
	if maxBatchRefBytes != 512 {
		t.Fatalf("maxBatchRefBytes = %d, must stay 512 to match internal/irc", maxBatchRefBytes)
	}
	if got := clampBatchRef(strings.Repeat("a", 100)); got != strings.Repeat("a", 100) {
		t.Fatalf("short ref altered: %q", got)
	}
	if got := clampBatchRef(strings.Repeat("a", 512)); len(got) != 512 {
		t.Fatalf("at-limit ref must pass through: len=%d", len(got))
	}
	// Over-limit refs are hashed: bounded, non-empty, and distinct refs stay
	// distinct (two refs sharing a 512-byte prefix must NOT collide).
	k1 := clampBatchRef(strings.Repeat("a", 600) + "X")
	k2 := clampBatchRef(strings.Repeat("a", 600) + "Y")
	if k1 == "" || len(k1) > 128 {
		t.Fatalf("over-limit ref key = %q (want bounded, non-empty)", k1)
	}
	if k1 == k2 {
		t.Fatal("distinct over-limit refs collided to one key")
	}
}

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
	s.Handle(ctxb, request(t, "get_network", 40, GetNetworkReq{Network: "alpha"}))
	one := decode[NetworkData](t, recv(t, s, "network", "state"))
	if one.Network.Name != "alpha" || len(one.Network.Config) == 0 {
		t.Fatalf("exact network = %+v", one.Network)
	}

	// Rename: history follows, the old name is announced as removed and
	// the rename itself broadcast (clients rewrite their local synced
	// rule/filter references).
	s.Handle(ctxb, request(t, "put_network", 5, PutNetworkReq{OldName: "alpha", Config: cfg("beta")}))
	recv(t, s, "network_removed", "state", "networks_changed")
	recv(t, s, "network_renamed", "state", "networks_changed")
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

func TestGetNetworksIsByteBoundedAndPaged(t *testing.T) {
	h := newTestHub(t)
	s := h.NewSession()
	defer s.Close()
	ctx := context.Background()
	padding := strings.Repeat("x", 60<<10)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("net-%02d", i)
		cfg, err := json.Marshal(map[string]string{"padding": padding})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.store.PutNetworkConfig(ctx, name, string(cfg)); err != nil {
			t.Fatal(err)
		}
	}
	// A malformed legacy row must be surfaced as recovery metadata instead of
	// making RawMessage marshal fail and silently dropping the whole reply.
	if err := h.store.PutNetworkConfig(ctx, "invalid", `{`); err != nil {
		t.Fatal(err)
	}

	var after int64
	seen := make(map[string]bool)
	for page := 0; page < 10; page++ {
		s.Handle(ctx, request(t, "get_networks", int64(page+1), GetNetworksReq{After: after}))
		env := recv(t, s, "networks")
		if len(env.Data) > maxHistoryPayloadBytes {
			t.Fatalf("page payload = %d, cap = %d", len(env.Data), maxHistoryPayloadBytes)
		}
		data := decode[NetworksData](t, env)
		if len(data.Networks) == 0 {
			t.Fatal("paged listing made no progress")
		}
		for _, n := range data.Networks {
			seen[n.Name] = true
			if n.Name == "invalid" && (!n.Invalid || len(n.Config) != 0) {
				t.Fatalf("invalid row = %#v", n)
			}
		}
		if !data.HasMore {
			break
		}
		if data.Next <= after {
			t.Fatalf("cursor did not advance: after=%d next=%d", after, data.Next)
		}
		after = data.Next
	}
	if len(seen) != 21 || !seen["invalid"] {
		t.Fatalf("saw %d definitions, want all 21", len(seen))
	}
}

func TestStopNetworkClearsStoppedPlaceholder(t *testing.T) {
	h := newTestHub(t)
	h.NoteStoppedNetwork("broken")
	h.StopNetwork("broken")
	h.mu.Lock()
	_, exists := h.states["broken"]
	h.mu.Unlock()
	if exists {
		t.Fatal("stopped-network placeholder survived deletion/repair stop")
	}
}

func TestRestartNetworkKeepsFailedDefinitionVisible(t *testing.T) {
	h := newTestHub(t)
	h.restartNetwork(&store.NetworkConfig{Name: "broken", Config: `{`})
	h.mu.Lock()
	state, exists := h.states["broken"]
	h.mu.Unlock()
	if !exists || state != irc.StateDisconnected.String() {
		t.Fatalf("failed restart state = %+v, exists=%v; want disconnected placeholder", state, exists)
	}
}

func TestLegacyNetworkEditRequiresRecreate(t *testing.T) {
	for _, row := range []store.NetworkConfig{
		{Oversized: true},
		{Config: ""},
	} {
		if !legacyNetworkRequiresRecreate(true, row, true) {
			t.Fatalf("legacy row %#v was considered safely editable", row)
		}
	}
	if legacyNetworkRequiresRecreate(true, store.NetworkConfig{Config: `{}`}, true) {
		t.Fatal("bounded non-empty row unexpectedly requires recreation")
	}
	if legacyNetworkRequiresRecreate(true, store.NetworkConfig{}, false) {
		t.Fatal("add path unexpectedly classified as a legacy edit")
	}
}

func TestRawInvalidLegacyNetworkMutationsAreRejected(t *testing.T) {
	h := newTestHub(t)
	s := h.NewSession()
	defer s.Close()
	ctx := context.Background()
	cfg := json.RawMessage(`{"name":"replacement","addr":"127.0.0.1:1","allow_plaintext":true,"nick":"AlteredParadox"}`)

	// Spaced names are deliberately absent here: they are ordinary legal
	// names since the rule was relaxed to grandfather legacy databases.
	for i, name := range []string{"bad\x1bname", store.ReservedRecoveryNetworkPrefix + "7", strings.Repeat("n", store.MaxNetworkNameBytes+1)} {
		s.Handle(ctx, request(t, "put_network", int64(i*2+1), PutNetworkReq{OldName: name, Config: cfg}))
		if got := decode[ErrorData](t, recv(t, s, "error")); got.Code != "bad_request" {
			t.Fatalf("put with raw legacy name %q: code=%q", name, got.Code)
		}
		s.Handle(ctx, request(t, "delete_network", int64(i*2+2), NetworkRef{Network: name}))
		if got := decode[ErrorData](t, recv(t, s, "error")); got.Code != "bad_request" {
			t.Fatalf("delete with raw legacy name %q: code=%q", name, got.Code)
		}
	}
}

// A printable spaced name ("Libera Chat") is an ordinary name: creatable,
// editable under its own name, and deletable through the normal paths.
// Legacy databases hold such names; they must never be forced through the
// destructive delete-only recovery path.
func TestPutNetworkAcceptsSpacedName(t *testing.T) {
	h := newTestHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	h.UseRoot(ctx, &wg)
	defer wg.Wait() // after cancel: reap the manager goroutines
	defer cancel()
	s := h.NewSession()
	defer s.Close()
	ctxb := context.Background()

	cfg := json.RawMessage(`{"name":"Libera Chat","addr":"127.0.0.1:1","allow_plaintext":true,"nick":"AlteredParadox"}`)
	s.Handle(ctxb, request(t, "put_network", 1, PutNetworkReq{Config: cfg}))
	recv(t, s, "ok", "networks_changed", "state")
	edited := json.RawMessage(`{"name":"Libera Chat","addr":"127.0.0.1:2","allow_plaintext":true,"nick":"AlteredParadox"}`)
	s.Handle(ctxb, request(t, "put_network", 2, PutNetworkReq{OldName: "Libera Chat", Config: edited}))
	recv(t, s, "ok", "networks_changed", "state", "network_removed")
	s.Handle(ctxb, request(t, "delete_network", 3, NetworkRef{Network: "Libera Chat"}))
	recv(t, s, "ok", "networks_changed", "state", "network_removed")
	if count, err := h.store.NetworkConfigCount(ctxb); err != nil || count != 0 {
		t.Fatalf("count after delete = %d err=%v, want 0", count, err)
	}
}

func TestDeleteSyntheticRecoveryNetwork(t *testing.T) {
	h := newTestHub(t)
	ctx := context.Background()
	if err := h.store.PutNetworkConfig(ctx, "legacy", `{}`); err != nil {
		t.Fatal(err)
	}
	rows, _, err := h.store.NetworkConfigsPage(ctx, 0, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("page = %#v err=%v", rows, err)
	}
	label := h.NoteInvalidNetwork(rows[0].PageID)
	s := h.NewSession()
	defer s.Close()
	s.Handle(ctx, request(t, "delete_network", 1, NetworkRef{Network: label}))
	if env := recv(t, s, "ok", "network_removed", "networks_changed"); env.Seq != 1 {
		t.Fatalf("delete reply seq = %d", env.Seq)
	}
	if count, err := h.store.NetworkConfigCount(ctx); err != nil || count != 0 {
		t.Fatalf("recovery row survived: count=%d err=%v", count, err)
	}
	h.mu.Lock()
	_, stateExists := h.states[label]
	_, mappingExists := h.recoveryRows[label]
	h.mu.Unlock()
	if stateExists || mappingExists {
		t.Fatal("synthetic recovery placeholder survived successful delete")
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
	recv(t, s, "ok", "event") // a racing seed event may arrive
	// A bare close (no purge field) resolves to the destructive default and
	// the broadcast says so: purge=true, always present.
	if closed := decode[BufferRef](t, recv(t, s, "buffer_closed", "event")); closed.Purge == nil || !*closed.Purge {
		t.Fatalf("bare close_buffer broadcast = %+v, want resolved purge=true", closed)
	}
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

func TestCloseSerializesCommittedEventThroughPublication(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{name: "libera", nick: "me", ch: make(chan irc.Event)}
	s := h.NewSession()
	defer s.Close()
	ctx := context.Background()
	ev := irc.Event{
		Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
		Msg: ircv4.MustParseMessage(":alice!u@h PRIVMSG me :hello"),
	}

	// Stop the live path only at publication. A private message's grace guard
	// short-circuits without h.mu, so its store append completes first and its
	// broadcast then waits here.
	h.mu.Lock()
	locked := true
	defer func() {
		if locked {
			h.mu.Unlock()
		}
	}()
	eventDone := make(chan struct{})
	go func() {
		_ = h.persistEvent(ctx, conn, ev, false, nil)
		close(eventDone)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		bufs, err := h.store.Buffers(ctx)
		if err != nil {
			h.mu.Unlock()
			locked = false
			t.Fatal(err)
		}
		if len(bufs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			h.mu.Unlock()
			locked = false
			t.Fatal("live append did not commit before publication")
		}
		time.Sleep(time.Millisecond)
	}

	closeDone := make(chan error, 1)
	go func() {
		h.bufferMutationMu.Lock()
		defer h.bufferMutationMu.Unlock()
		_, err := h.store.DeleteBufferFolded(ctx, "libera", "alice", conn.Fold, func(name string) {
			h.markClosed("libera", conn.Fold(name), time.Now().UnixMilli())
		})
		if err == nil {
			h.broadcast(envelope("buffer_closed", 0, BufferRef{Network: "libera", Buffer: "alice"}))
		}
		closeDone <- err
	}()

	// The close must still be waiting at the wider mutation boundary; without
	// it the row is already deleted here while the old event has not published.
	time.Sleep(25 * time.Millisecond)
	bufs, err := h.store.Buffers(ctx)
	if err != nil || len(bufs) != 1 {
		h.mu.Unlock()
		locked = false
		t.Fatalf("close crossed append/publication boundary: buffers=%v err=%v", bufs, err)
	}
	h.mu.Unlock()
	locked = false
	<-eventDone
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if got := recv(t, s, "event"); got.Type != "event" {
		t.Fatalf("first publication = %q, want event", got.Type)
	}
	if got := recv(t, s, "buffer_closed"); got.Type != "buffer_closed" {
		t.Fatalf("second publication = %q, want buffer_closed", got.Type)
	}
	if bufs, err := h.store.Buffers(ctx); err != nil || len(bufs) != 0 {
		t.Fatalf("closed row remains: buffers=%v err=%v", bufs, err)
	}
}

func TestRecentCloseCountAndByteCaps(t *testing.T) {
	h := newTestHub(t)
	now := time.Now().UnixMilli()
	for i := 0; i < maxRecentCloses*2; i++ {
		h.markClosed("net", fmt.Sprintf("#%04d-%s", i, strings.Repeat("x", 500)), now+int64(i))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.recentClose) > maxRecentCloses {
		t.Fatalf("recentClose count = %d, cap %d", len(h.recentClose), maxRecentCloses)
	}
	if h.recentCloseBytes > maxRecentCloseBytes {
		t.Fatalf("recentClose bytes = %d, cap %d", h.recentCloseBytes, maxRecentCloseBytes)
	}
}

// MONITOR presence is stored only for configured buddies (so the map is
// bounded by the monitor list), and the server-reported nick casing is
// folded back to the configured spelling — otherwise an online buddy shows
// perpetually offline.
func TestPresenceCasemapping(t *testing.T) {
	h := newTestHub(t)
	ctx := context.Background()
	c := &fakeConn{ch: make(chan irc.Event, 1), name: "libera", nick: "AlteredParadox"}
	if err := h.store.AddMonitor(ctx, "libera", "bob"); err != nil {
		t.Fatal(err)
	}
	// Server reports it online with different casing.
	h.updatePresence(ctx, c, "libera", ircv4.MustParseMessage(":srv 730 AlteredParadox :Bob!u@h"), true)
	entries, err := h.monitorList(ctx, "libera")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Nick != "bob" || !entries[0].Online {
		t.Fatalf("monitor entries = %+v, want [{bob online}]", entries)
	}
	// A nick we don't monitor is ignored, so the map stays bounded.
	h.updatePresence(ctx, c, "libera", ircv4.MustParseMessage(":srv 730 AlteredParadox :stranger!u@h"), true)
	h.mu.Lock()
	n := len(h.presence["libera"])
	h.mu.Unlock()
	if n != 1 {
		t.Fatalf("presence map = %d entries, want 1 (only monitored buddies)", n)
	}
}

// The WHOIS accumulator is bounded against a server streaming unique
// nicks that never terminate with 318.
func TestWhoisMapBounded(t *testing.T) {
	whois := make(map[string]*WhoisData)
	conn := &fakeConn{name: "libera", nick: "AlteredParadox"}
	for i := 0; i < maxOpenWhois+200; i++ {
		ev := irc.Event{Network: "libera", Kind: irc.EventMessage,
			Msg: ircv4.MustParseMessage(":srv 311 AlteredParadox nick" + strconv.Itoa(i) + " u h * :real")}
		newTestHub(t).accumulateWhois(conn, ev, whois)
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

// QUIT/NICK fan-out honors the recentClose grace: a straggler QUIT for a
// just-closed channel does not resurrect its buffer.
func TestQuitDoesNotResurrectClosedBuffer(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox",
		chans: map[string][]irc.Member{"#foo": {{Nick: "AlteredParadox"}, {Nick: "bob"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()

	ev := func(line string, affected ...string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
			Msg: ircv4.MustParseMessage(line), Affected: affected}
	}
	// Seed #foo.
	conn.ch <- ev(":bob!u@h PRIVMSG #foo :hi")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#foo", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Close it, then a straggler QUIT touching #foo arrives.
	s := h.NewSession()
	defer s.Close()
	s.Handle(ctxb, request(t, "close_buffer", 1, BufferRef{Network: "libera", Buffer: "#foo"}))
	recv(t, s, "ok", "buffer_closed", "event") // a racing seed event may arrive
	conn.ch <- ev(":bob!u@h QUIT :bye", "#foo")

	time.Sleep(200 * time.Millisecond)
	bufs, err := h.store.Buffers(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 0 {
		t.Fatalf("QUIT resurrected the closed buffer: %+v", bufs)
	}
}

// A QUIT/NICK replayed inside an event-playback batch must not resurrect a
// buffer the user just closed either — the close grace is replay-agnostic
// (mirrors persistEvent). Before the fix the replay path skipped the grace
// and re-created the buffer with orphan system lines.
func TestReplayedQuitDoesNotResurrectClosedBuffer(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox",
		chans: map[string][]irc.Member{"#foo": {{Nick: "AlteredParadox"}, {Nick: "bob"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
			Msg: ircv4.MustParseMessage(line)}
	}
	// Seed #foo.
	conn.ch <- ev(":bob!u@h PRIVMSG #foo :hi")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#foo", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Close it, then an event-playback batch for #foo replays a QUIT.
	s := h.NewSession()
	defer s.Close()
	s.Handle(ctxb, request(t, "close_buffer", 1, BufferRef{Network: "libera", Buffer: "#foo"}))
	recv(t, s, "ok", "buffer_closed", "event")
	conn.ch <- ev(":srv BATCH +r1 chathistory #foo")
	conn.ch <- ev("@batch=r1;time=2026-07-15T00:00:06.000Z :bob!u@h QUIT :bye")
	conn.ch <- ev(":srv BATCH -r1")

	time.Sleep(200 * time.Millisecond)
	bufs, err := h.store.Buffers(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 0 {
		t.Fatalf("replayed QUIT resurrected the closed buffer: %+v", bufs)
	}
}

// A REPLAYED (event-playback) self-JOIN must NOT clear the close grace or
// resurrect a just-closed channel — only a live rejoin carries that intent.
func TestReplayedSelfJoinDoesNotResurrectClosedChannel(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox",
		chans: map[string][]irc.Member{"#foo": {{Nick: "AlteredParadox"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()
	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
			Msg: ircv4.MustParseMessage(line)}
	}
	conn.ch <- ev(":bob!u@h PRIVMSG #foo :hi")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#foo", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	s := h.NewSession()
	defer s.Close()
	s.Handle(ctxb, request(t, "close_buffer", 1, BufferRef{Network: "libera", Buffer: "#foo"}))
	recv(t, s, "ok", "buffer_closed", "event")

	conn.ch <- ev(":srv BATCH +r1 chathistory #foo")
	conn.ch <- ev("@batch=r1;time=2026-07-15T00:00:06.000Z :AlteredParadox!u@h JOIN #foo")
	conn.ch <- ev(":srv BATCH -r1")
	time.Sleep(200 * time.Millisecond)
	if bufs, _ := h.store.Buffers(ctxb); len(bufs) != 0 {
		t.Fatalf("replayed self-JOIN resurrected the closed channel: %+v", bufs)
	}
}

// A REPLAYED PM to a just-closed query must be dropped too: backfill is a
// straggler regardless of target type. (A LIVE PM still reopens the query —
// see TestInboundPMReopensClosedQuery.)
func TestReplayedPMToClosedQueryDropped(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()
	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
			Msg: ircv4.MustParseMessage(line)}
	}
	conn.ch <- ev(":bob!u@h PRIVMSG AlteredParadox :hi")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "bob", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("query seed not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	s := h.NewSession()
	defer s.Close()
	s.Handle(ctxb, request(t, "close_buffer", 1, BufferRef{Network: "libera", Buffer: "bob"}))
	recv(t, s, "ok", "buffer_closed", "event")

	conn.ch <- ev(":srv BATCH +r1 chathistory bob")
	conn.ch <- ev("@batch=r1;time=2026-07-15T00:00:06.000Z :bob!u@h PRIVMSG AlteredParadox :old")
	conn.ch <- ev(":srv BATCH -r1")
	time.Sleep(200 * time.Millisecond)
	if bufs, _ := h.store.Buffers(ctxb); len(bufs) != 0 {
		t.Fatalf("replayed PM resurrected the closed query: %+v", bufs)
	}
}

// A QUIT/NICK fan-out target comes from the roster's raw wire spelling,
// which can differ in case from the stored buffer. It must be canonicalized
// so the system line lands in the existing buffer instead of splitting off
// a case-variant duplicate.
func TestQuitFoldsToExistingChannelBuffer(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox",
		chans: map[string][]irc.Member{"#foo": {{Nick: "AlteredParadox"}, {Nick: "bob"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()

	ev := func(line string, affected ...string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
			Msg: ircv4.MustParseMessage(line), Affected: affected}
	}
	// Seed the stored buffer under lowercase '#foo'.
	conn.ch <- ev(":bob!u@h PRIVMSG #foo :hi")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#foo", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A QUIT fan-out arrives with the roster's uppercase wire spelling.
	conn.ch <- ev(":bob!u@h QUIT :bye", "#FOO")
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#foo", 5); len(b) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("QUIT not persisted to #foo")
		}
		time.Sleep(5 * time.Millisecond)
	}
	bufs, err := h.store.Buffers(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 1 {
		t.Fatalf("QUIT split into a case-variant buffer: %+v", bufs)
	}
}

// Our own PART echo whose channel casing differs from the stored buffer
// spelling must still be recorded in that buffer (folded lookup), not
// dropped — while never creating a new buffer.
func TestSelfPartFoldsToExistingChannelBuffer(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox",
		chans: map[string][]irc.Member{"#foo": {{Nick: "AlteredParadox"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
			Msg: ircv4.MustParseMessage(line)}
	}
	// Seed the stored buffer under lowercase '#foo'.
	conn.ch <- ev(":bob!u@h PRIVMSG #foo :hi")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#foo", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Our own PART echo arrives with a case-variant channel name.
	conn.ch <- ev(":AlteredParadox!u@h PART #FOO :bye")
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#foo", 5); len(b) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("self-PART not persisted to #foo (case-folded lookup missed)")
		}
		time.Sleep(5 * time.Millisecond)
	}
	bufs, err := h.store.Buffers(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 1 {
		t.Fatalf("self-PART created a case-variant buffer: %+v", bufs)
	}
}

// A straggler for a channel in its close grace but NOT yet deleted (the
// window between markClosed and DeleteBuffer) must be dropped, not appended
// and broadcast — otherwise it resurrects the buffer on clients. The guard
// must veto the append even though the buffer still exists.
func TestStragglerToClosedChannelDropped(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox",
		chans: map[string][]irc.Member{"#go": {{Nick: "AlteredParadox"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
			Msg: ircv4.MustParseMessage(line)}
	}
	// Seed the #go buffer.
	conn.ch <- ev(":bob!u@h PRIVMSG #go :hi")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#go", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Enter the race window: marked closed, buffer NOT yet deleted.
	h.markClosed("libera", conn.Fold("#go"), time.Now().UnixMilli())

	// The straggler, then a message to a different open buffer as an ordering
	// barrier (events are processed in order on the Run goroutine).
	conn.ch <- ev(":bob!u@h PRIVMSG #go :straggler")
	conn.ch <- ev(":bob!u@h PRIVMSG #other :hello")
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#other", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("barrier message not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The straggler must have been dropped: #go still holds only the seed.
	if b, _ := h.store.Latest(ctxb, "libera", "#go", 5); len(b) != 1 {
		t.Fatalf("straggler to a closed channel was appended: #go has %d rows, want 1", len(b))
	}
}

// Our own PART echo is a straggler too: landing in the close grace (marked
// closed, buffer not yet deleted) it must be dropped, not appended and
// broadcast as a live event that resurrects the buffer.
func TestSelfPartStragglerToClosedChannelDropped(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox",
		chans: map[string][]irc.Member{"#go": {{Nick: "AlteredParadox"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
			Msg: ircv4.MustParseMessage(line)}
	}
	conn.ch <- ev(":bob!u@h PRIVMSG #go :hi")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#go", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	h.markClosed("libera", conn.Fold("#go"), time.Now().UnixMilli())

	conn.ch <- ev(":AlteredParadox!u@h PART #go :bye") // our own PART echo, in the grace
	conn.ch <- ev(":bob!u@h PRIVMSG #other :hello")
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#other", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("barrier message not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if b, _ := h.store.Latest(ctxb, "libera", "#go", 5); len(b) != 1 {
		t.Fatalf("self-PART echo to a closed channel was appended: #go has %d rows, want 1", len(b))
	}
}

// The close grace is channel-only: an inbound PM to a just-closed query
// must reopen the conversation, not be dropped as a straggler.
func TestInboundPMReopensClosedQuery(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
			Msg: ircv4.MustParseMessage(line)}
	}
	// Open the query 'bob' with an inbound PM.
	conn.ch <- ev(":bob!u@h PRIVMSG AlteredParadox :hi")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "bob", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed PM not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Close the query, then bob PMs again within the grace window.
	s := h.NewSession()
	defer s.Close()
	s.Handle(ctxb, request(t, "close_buffer", 1, BufferRef{Network: "libera", Buffer: "bob"}))
	recv(t, s, "ok", "buffer_closed", "event")
	conn.ch <- ev(":bob!u@h PRIVMSG AlteredParadox :you there?")

	// The new PM reopens the query and is delivered + persisted.
	got := decode[EventData](t, recv(t, s, "event", "buffer_closed"))
	if got.Buffer != "bob" {
		t.Fatalf("reopened PM buffer = %q, want bob", got.Buffer)
	}
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "bob", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reopened PM not persisted (dropped by the close grace)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// monitor_add for a network that is neither configured nor connected is
// rejected, so it cannot mint phantom network/monitor rows.
func TestMonitorRejectsUnknownNetwork(t *testing.T) {
	h := newTestHub(t)
	s := h.NewSession()
	defer s.Close()
	ctx := context.Background()

	s.Handle(ctx, request(t, "monitor_add", 1, MonitorReq{Network: "ghostnet", Nick: "someone"}))
	if env := recv(t, s, "error"); env.Seq != 1 {
		t.Fatalf("unknown-network monitor: got seq %d", env.Seq)
	}
	// No networks row was created.
	bufs, err := h.store.Buffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 0 {
		t.Fatalf("phantom rows created: %+v", bufs)
	}
}

// A self-JOIN within the close grace reopens the buffer (live traffic
// flows again) instead of being dropped.
func TestSelfJoinReopensClosedBuffer(t *testing.T) {
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
	conn.ch <- ev(":bob!u@h PRIVMSG #go :hi")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#go", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	s := h.NewSession()
	defer s.Close()
	s.Handle(ctxb, request(t, "close_buffer", 1, BufferRef{Network: "libera", Buffer: "#go"}))
	recv(t, s, "ok", "buffer_closed", "event")

	// Rejoin within the grace window, then a channel message.
	conn.ch <- ev(":AlteredParadox!u@h JOIN #go")
	conn.ch <- ev(":carol!u@h PRIVMSG #go :after rejoin")
	deadline = time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#go", 5); len(b) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("message after rejoin was dropped (buffer not reopened)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// persistOwn does not create a buffer for a network that is neither
// configured nor connected (e.g. deleted mid-send), so a racing session
// write cannot resurrect an orphan.
func TestPersistOwnSkipsUnknownNetwork(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 4), name: "ghostnet", nick: "AlteredParadox"}
	s := h.NewSession()
	defer s.Close()
	ctx := context.Background()

	// ghostnet has no config and is not connected (not run via h.Run).
	s.persistOwn(ctx, conn, "ghostnet", "#x", "hi")
	bufs, err := h.store.Buffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 0 {
		t.Fatalf("persistOwn created a buffer for an unknown network: %+v", bufs)
	}
}

// ---- close_buffer purge:false (archive) ----

func boolPtr(b bool) *bool { return &b }

// expectNoEvent drains the session queue for a moment and fails on any
// "event" push — the envelope that would grow a sidebar row on clients.
// Roster hints (members_changed) are tolerated: the client only uses them
// to refresh the ACTIVE buffer's member list, never to create a buffer.
func expectNoEvent(t *testing.T, s *Session) {
	t.Helper()
	timeout := time.After(100 * time.Millisecond)
	for {
		select {
		case queued := <-s.Outbound():
			var env Envelope
			_ = json.Unmarshal(queued.Data, &env)
			queued.Release()
			if env.Type == "event" {
				t.Fatalf("unexpected event push: %s", env.Data)
			}
		case <-timeout:
			return
		}
	}
}

// archiveTestSetup runs a hub with one fake network, seeds #foo with one
// PRIVMSG, and archives it via close_buffer purge:false, returning the
// session with its ok/buffer_closed already consumed.
func archiveTestSetup(t *testing.T) (*Hub, *fakeConn, *Session, func(string, ...string) irc.Event) {
	t.Helper()
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()

	ev := func(line string, affected ...string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
			Msg: ircv4.MustParseMessage(line), Affected: affected}
	}
	conn.ch <- ev(":alice!u@h PRIVMSG #foo :first")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#foo", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed message never persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}

	s := h.NewSession()
	t.Cleanup(s.Close)
	s.Handle(ctxb, request(t, "close_buffer", 1,
		BufferRef{Network: "libera", Buffer: "#FOO", Purge: boolPtr(false)}))
	env := recv(t, s, "ok", "event") // a racing seed event may arrive
	if ref := decode[BufferRef](t, env); ref.Buffer != "#foo" || ref.Network != "libera" ||
		ref.Purge == nil || *ref.Purge {
		t.Fatalf("ok payload = %+v, want canonical libera/#foo with resolved purge=false", ref)
	}
	if closed := decode[BufferRef](t, recv(t, s, "buffer_closed", "event")); closed.Purge == nil || *closed.Purge {
		t.Fatalf("buffer_closed payload = %+v, want resolved purge=false (archive)", closed)
	}
	return h, conn, s, ev
}

// close_buffer purge:false archives: the ok response is canonicalized,
// buffer_closed is broadcast (asserted in archiveTestSetup), history stays
// intact, and the buffer is absent from a fresh get_buffers.
func TestCloseBufferArchiveKeepsHistory(t *testing.T) {
	h, _, s, _ := archiveTestSetup(t)
	ctxb := context.Background()

	if msgs, err := h.store.Latest(ctxb, "libera", "#foo", 5); err != nil || len(msgs) != 1 {
		t.Fatalf("history after archive = %d msgs, %v; want 1 intact", len(msgs), err)
	}
	s.Handle(ctxb, request(t, "get_buffers", 2, nil))
	data := decode[BuffersData](t, recv(t, s, "buffers"))
	if len(data.Buffers) != 0 {
		t.Fatalf("get_buffers after archive = %+v, want empty", data.Buffers)
	}
}

// An explicit purge:true keeps the original destructive behavior (the
// missing-purge case is covered by the pre-existing close_buffer tests).
func TestCloseBufferPurgeTrueStillDeletes(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()

	conn.ch <- irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
		Msg: ircv4.MustParseMessage(":alice!u@h PRIVMSG #foo :first")}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := h.store.Latest(ctxb, "libera", "#foo", 5); len(b) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed message never persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s := h.NewSession()
	defer s.Close()
	s.Handle(ctxb, request(t, "close_buffer", 1,
		BufferRef{Network: "libera", Buffer: "#foo", Purge: boolPtr(true)}))
	if ref := decode[BufferRef](t, recv(t, s, "ok", "event")); ref.Purge == nil || !*ref.Purge {
		t.Fatalf("ok payload = %+v, want resolved purge=true", ref)
	}
	if closed := decode[BufferRef](t, recv(t, s, "buffer_closed", "event")); closed.Purge == nil || !*closed.Purge {
		t.Fatalf("buffer_closed payload = %+v, want resolved purge=true (destructive)", closed)
	}
	if msgs, _ := h.store.Latest(ctxb, "libera", "#foo", 5); len(msgs) != 0 {
		t.Fatalf("purge:true left %d history rows", len(msgs))
	}
}

// The PART echo that races a purge:false close lands after the archive: it
// is persisted into the hidden buffer but publishes no event and does not
// resurface the buffer — the archive path's equivalent of the delete path's
// close tombstone.
func TestArchivedPartEchoStaysHidden(t *testing.T) {
	h, conn, s, ev := archiveTestSetup(t)
	ctxb := context.Background()

	conn.ch <- ev(":AlteredParadox!u@h PART #foo")
	time.Sleep(200 * time.Millisecond)
	expectNoEvent(t, s) // no event broadcast may resurrect the buffer on clients
	if bufs, _ := h.store.Buffers(ctxb); len(bufs) != 0 {
		t.Fatalf("PART echo resurfaced the archived buffer: %+v", bufs)
	}
	if msgs, _ := h.store.Latest(ctxb, "libera", "#foo", 5); len(msgs) != 2 {
		t.Fatalf("history = %d msgs, want seed + PART kept", len(msgs))
	}
}

// Foreign membership fan-out (someone else's JOIN, a QUIT touching the
// channel) must not resurface an archived buffer either.
func TestArchivedForeignMembershipStaysHidden(t *testing.T) {
	h, conn, s, ev := archiveTestSetup(t)
	ctxb := context.Background()

	conn.ch <- ev(":bob!u@h JOIN #foo")
	conn.ch <- ev(":bob!u@h QUIT :bye", "#foo")
	time.Sleep(200 * time.Millisecond)
	expectNoEvent(t, s)
	if bufs, _ := h.store.Buffers(ctxb); len(bufs) != 0 {
		t.Fatalf("foreign membership resurfaced the archived buffer: %+v", bufs)
	}
}

// A KICK touching the channel is presence traffic like PART/QUIT (it
// renders as a system row and never bumps unread) and must not resurface
// an archived buffer either.
func TestArchivedKickStaysHidden(t *testing.T) {
	h, conn, s, ev := archiveTestSetup(t)
	ctxb := context.Background()

	conn.ch <- ev(":op!u@h KICK #foo bob :bye")
	time.Sleep(200 * time.Millisecond)
	expectNoEvent(t, s)
	if bufs, _ := h.store.Buffers(ctxb); len(bufs) != 0 {
		t.Fatalf("KICK resurfaced the archived buffer: %+v", bufs)
	}
}

// unarchivePolicy pins the resurface rules directly: live conversation
// resurfaces an archived buffer; membership fan-out (including KICK)
// never does; REPLAYED content never does (a chathistory page in flight
// when the user archives is backfill, not intent); our own live JOIN is a
// deliberate rejoin, a replayed one is not.
func TestUnarchivePolicy(t *testing.T) {
	cases := []struct {
		command     string
		own, replay bool
		want        bool
	}{
		{"PRIVMSG", false, false, true}, // live conversation resurfaces
		{"PRIVMSG", false, true, false}, // replayed content is backfill
		{"NOTICE", false, true, false},
		{"TOPIC", false, false, true},
		{"KICK", false, false, false}, // live KICK is presence traffic
		{"KICK", false, true, false},
		{"PART", false, false, false},
		{"QUIT", false, false, false},
		{"JOIN", true, false, true}, // own live JOIN = deliberate rejoin
		{"JOIN", true, true, false}, // replayed self-JOIN = backfill
		{"JOIN", false, false, false},
	}
	for _, c := range cases {
		if got := unarchivePolicy(c.command, c.own, c.replay); got != c.want {
			t.Errorf("unarchivePolicy(%q, own=%v, replay=%v) = %v, want %v",
				c.command, c.own, c.replay, got, c.want)
		}
	}
}

// Real conversation resurfaces an archived buffer: the event is broadcast
// live and a fresh get_buffers lists the buffer again with its prior
// history plus the new message.
func TestArchivedBufferResurfacesOnContent(t *testing.T) {
	h, conn, s, ev := archiveTestSetup(t)
	ctxb := context.Background()

	conn.ch <- ev(":bob!u@h PRIVMSG #foo :are you there?")
	got := decode[EventData](t, recv(t, s, "event"))
	if got.Buffer != "#foo" || got.Command != "PRIVMSG" {
		t.Fatalf("resurfacing event = %+v, want PRIVMSG to #foo", got)
	}
	s.Handle(ctxb, request(t, "get_buffers", 2, nil))
	data := decode[BuffersData](t, recv(t, s, "buffers"))
	if len(data.Buffers) != 1 || data.Buffers[0].Buffer != "#foo" {
		t.Fatalf("get_buffers after content = %+v, want [#foo]", data.Buffers)
	}
	if msgs, _ := h.store.Latest(ctxb, "libera", "#foo", 5); len(msgs) != 2 {
		t.Fatalf("history = %d msgs, want prior 1 + new 1", len(msgs))
	}
}

// Our own LIVE JOIN of an archived channel is a deliberate rejoin and
// resurfaces it (a REPLAYED self-JOIN must not — see
// TestReplayedSelfJoinDoesNotResurrectClosedChannel for the deleted-path
// twin of that rule).
func TestArchivedBufferResurfacesOnOwnLiveJoin(t *testing.T) {
	h, conn, s, ev := archiveTestSetup(t)
	ctxb := context.Background()

	conn.ch <- ev(":AlteredParadox!u@h JOIN #foo")
	got := decode[EventData](t, recv(t, s, "event", "members_changed"))
	if got.Buffer != "#foo" || got.Command != "JOIN" {
		t.Fatalf("resurfacing event = %+v, want own JOIN of #foo", got)
	}
	if bufs, _ := h.store.Buffers(ctxb); len(bufs) != 1 || bufs[0].Target != "#foo" {
		t.Fatalf("Buffers after own live JOIN = %+v, want [#foo]", bufs)
	}
}
