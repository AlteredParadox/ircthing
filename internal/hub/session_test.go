package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"ircthing/internal/irc"
	"ircthing/internal/store"

	ircv4 "gopkg.in/irc.v4"
)

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st)
}

// recv waits for the next envelope of the given type, failing on anything
// else (except when skip lists types to pass over).
func recv(t *testing.T, s *Session, wantType string, skip ...string) Envelope {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case env := <-s.Outbound():
			if env.Type == wantType {
				return env
			}
			skipped := false
			for _, sk := range skip {
				if env.Type == sk {
					skipped = true
				}
			}
			if !skipped {
				t.Fatalf("got envelope type %q, want %q", env.Type, wantType)
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %q envelope", wantType)
		}
	}
}

// expectSilence asserts no envelope arrives (for ignored inputs).
func expectSilence(t *testing.T, s *Session) {
	t.Helper()
	select {
	case env := <-s.Outbound():
		t.Fatalf("unexpected envelope: %+v", env)
	case <-time.After(50 * time.Millisecond):
	}
}

func decode[T any](t *testing.T, env Envelope) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(env.Data, &v); err != nil {
		t.Fatalf("decode %q data: %v", env.Type, err)
	}
	return v
}

func request(t *testing.T, typ string, seq int64, data any) Envelope {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return Envelope{V: ProtocolVersion, Type: typ, Seq: seq, Data: raw}
}

func seedHub(t *testing.T, h *Hub, n int) []store.Message {
	t.Helper()
	msgs := make([]store.Message, 0, n)
	for i := 1; i <= n; i++ {
		m, err := h.store.Append(context.Background(), "libera", "#go", store.Message{
			Time:    time.UnixMilli(int64(i) * 1000),
			MsgID:   fmt.Sprintf("id%d", i),
			Sender:  "alice",
			Command: "PRIVMSG",
			Raw:     fmt.Sprintf("msg %d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func TestSessionHistoryPaging(t *testing.T) {
	cases := []struct {
		name    string
		req     HistoryReq
		want    []string // expected Raw fields, ascending
		errCode string
	}{
		{
			name: "latest by default",
			req:  HistoryReq{Network: "libera", Buffer: "#go", Limit: 3},
			want: []string{"msg 8", "msg 9", "msg 10"},
		},
		{
			name: "before cursor",
			req:  HistoryReq{Network: "libera", Buffer: "#go", Before: &Cursor{TS: 5000, ID: 5}, Limit: 2},
			want: []string{"msg 3", "msg 4"},
		},
		{
			name: "after cursor",
			req:  HistoryReq{Network: "libera", Buffer: "#go", After: &Cursor{TS: 8000, ID: 8}, Limit: 5},
			want: []string{"msg 9", "msg 10"},
		},
		{
			name: "before msgid",
			req:  HistoryReq{Network: "libera", Buffer: "#go", BeforeMsgID: "id4", Limit: 2},
			want: []string{"msg 2", "msg 3"},
		},
		{
			name: "after msgid",
			req:  HistoryReq{Network: "libera", Buffer: "#go", AfterMsgID: "id9", Limit: 5},
			want: []string{"msg 10"},
		},
		{
			name: "empty buffer yields empty page",
			req:  HistoryReq{Network: "libera", Buffer: "#empty", Limit: 5},
			want: []string{},
		},
		{
			name:    "unknown msgid errors",
			req:     HistoryReq{Network: "libera", Buffer: "#go", BeforeMsgID: "nope"},
			errCode: "unknown_msgid",
		},
		{
			name:    "missing buffer errors",
			req:     HistoryReq{Network: "libera"},
			errCode: "bad_request",
		},
	}

	h := newTestHub(t)
	seedHub(t, h, 10)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := h.NewSession()
			defer s.Close()
			s.Handle(context.Background(), request(t, "get_history", 7, tc.req))

			if tc.errCode != "" {
				env := recv(t, s, "error")
				if env.Seq != 7 {
					t.Fatalf("error seq = %d, want 7", env.Seq)
				}
				if got := decode[ErrorData](t, env); got.Code != tc.errCode {
					t.Fatalf("error code = %q, want %q", got.Code, tc.errCode)
				}
				return
			}
			env := recv(t, s, "history")
			if env.Seq != 7 {
				t.Fatalf("history seq = %d, want 7", env.Seq)
			}
			page := decode[HistoryData](t, env)
			if len(page.Messages) != len(tc.want) {
				t.Fatalf("page size = %d, want %d (%+v)", len(page.Messages), len(tc.want), page.Messages)
			}
			for i, m := range page.Messages {
				if m.Raw != tc.want[i] {
					t.Fatalf("message %d = %q, want %q", i, m.Raw, tc.want[i])
				}
				if m.Network != "libera" || m.Buffer != "#go" {
					t.Fatalf("message %d misrouted: %+v", i, m)
				}
			}
		})
	}
}

func TestSessionReadMarkers(t *testing.T) {
	h := newTestHub(t)
	a := h.NewSession()
	defer a.Close()
	b := h.NewSession()
	defer b.Close()
	ctx := context.Background()

	// Unset marker reads as 0.
	a.Handle(ctx, request(t, "get_read_marker", 1, MarkerRef{Network: "libera", Buffer: "#go"}))
	if got := decode[MarkerData](t, recv(t, a, "read_marker")); got.Time != 0 {
		t.Fatalf("unset marker = %d, want 0", got.Time)
	}

	// Setting responds to the requester (seq-tagged) and pushes to the
	// other session (no seq).
	a.Handle(ctx, request(t, "set_read_marker", 2, SetMarkerData{Network: "libera", Buffer: "#go", Time: 9000}))
	resp := recv(t, a, "read_marker")
	if resp.Seq != 2 {
		t.Fatalf("response seq = %d, want 2", resp.Seq)
	}
	if got := decode[MarkerData](t, resp); got.Time != 9000 {
		t.Fatalf("marker = %d, want 9000", got.Time)
	}
	push := recv(t, b, "read_marker")
	if push.Seq != 0 {
		t.Fatalf("push seq = %d, want 0", push.Seq)
	}
	if got := decode[MarkerData](t, push); got.Time != 9000 || got.Buffer != "#go" {
		t.Fatalf("push data = %+v", got)
	}

	// A regressing set returns the authoritative (newer) value.
	b.Handle(ctx, request(t, "set_read_marker", 3, SetMarkerData{Network: "libera", Buffer: "#go", Time: 5000}))
	if got := decode[MarkerData](t, recv(t, b, "read_marker")); got.Time != 9000 {
		t.Fatalf("regressed marker reported %d, want authoritative 9000", got.Time)
	}
}

func TestSessionSend(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")

	a := h.NewSession()
	defer a.Close()
	b := h.NewSession()
	defer b.Close()

	a.Handle(ctx, request(t, "send", 5, SendData{Network: "libera", Target: "#go", Text: "one\ntwo"}))

	// Both lines hit the IRC connection...
	deadline := time.Now().Add(5 * time.Second)
	for len(conn.sentMsgs()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("sent %d messages, want 2", len(conn.sentMsgs()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	sent := conn.sentMsgs()
	if sent[0].Command != "PRIVMSG" || sent[0].Param(0) != "#go" || sent[0].Trailing() != "one" {
		t.Fatalf("first sent = %q", sent[0].String())
	}

	// ...the sender gets events plus the ack, the other session events.
	ev := decode[EventData](t, recv(t, a, "event"))
	if ev.Sender != "AlteredParadox" || ev.Buffer != "#go" {
		t.Fatalf("own message event = %+v", ev)
	}
	recv(t, a, "ok", "event")
	recv(t, b, "event")

	// Both messages were persisted.
	msgs, err := h.store.Latest(ctx, "libera", "#go", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Sender != "AlteredParadox" {
		t.Fatalf("persisted: %+v", msgs)
	}

	// Unknown network fails cleanly.
	a.Handle(ctx, request(t, "send", 6, SendData{Network: "ghost", Target: "#go", Text: "x"}))
	if got := decode[ErrorData](t, recv(t, a, "error")); got.Code != "unknown_network" {
		t.Fatalf("error code = %q", got.Code)
	}
}

func waitForNetwork(t *testing.T, h *Hub, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.network(name) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("network %q never registered", name)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSessionGetBuffers(t *testing.T) {
	h := newTestHub(t)
	seedHub(t, h, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn := &fakeConn{ch: make(chan irc.Event, 4), name: "libera", nick: "AlteredParadox"}
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	conn.ch <- irc.Event{Network: "libera", Kind: irc.EventState, State: irc.StateRegistered}

	s := h.NewSession()
	defer s.Close()
	recv(t, s, "state") // wait until the state event has been processed

	s.Handle(ctx, request(t, "get_buffers", 3, nil))
	env := recv(t, s, "buffers")
	if env.Seq != 3 {
		t.Fatalf("seq = %d", env.Seq)
	}
	data := decode[BuffersData](t, env)
	if len(data.Networks) != 1 || data.Networks[0].Name != "libera" ||
		data.Networks[0].State != "registered" || data.Networks[0].Nick != "AlteredParadox" {
		t.Fatalf("networks = %+v", data.Networks)
	}
	if len(data.Buffers) != 1 || data.Buffers[0].Buffer != "#go" ||
		data.Buffers[0].Unread != 3 || data.Buffers[0].LastTime != 3000 {
		t.Fatalf("buffers = %+v", data.Buffers)
	}
}

func TestSessionIgnoresUnknownInput(t *testing.T) {
	h := newTestHub(t)
	s := h.NewSession()
	defer s.Close()
	ctx := context.Background()

	// Unknown type: ignored (forward-compat rule).
	s.Handle(ctx, Envelope{V: ProtocolVersion, Type: "frobnicate", Seq: 9})
	expectSilence(t, s)
	// Unknown protocol version: ignored entirely.
	s.Handle(ctx, request(t, "get_history", 9, HistoryReq{Network: "n", Buffer: "#b"}))
	env := recv(t, s, "history") // sanity: v1 works...
	_ = env
	s.Handle(ctx, Envelope{V: 99, Type: "get_history", Seq: 10})
	expectSilence(t, s)
	// Malformed data on a known type: error, session survives.
	s.Handle(ctx, Envelope{V: ProtocolVersion, Type: "send", Seq: 11, Data: json.RawMessage(`"not an object"`)})
	if got := decode[ErrorData](t, recv(t, s, "error")); got.Code != "bad_request" {
		t.Fatalf("error code = %q", got.Code)
	}
}

func TestSlowSessionEvicted(t *testing.T) {
	h := newTestHub(t)
	s := h.NewSession()
	defer s.Close()

	// Never drain: the buffer fills, then the next broadcast evicts.
	for i := 0; i < sessionBuffer+1; i++ {
		h.broadcast(envelope("event", 0, EventData{Raw: "x"}))
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("slow session was not evicted")
	}

	// A healthy session created afterwards still receives broadcasts.
	s2 := h.NewSession()
	defer s2.Close()
	h.broadcast(envelope("event", 0, EventData{Raw: "y"}))
	recv(t, s2, "event")
}

func TestHubBroadcastsStateAndEvents(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 4), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)

	s := h.NewSession()
	defer s.Close()

	conn.ch <- irc.Event{Network: "libera", Kind: irc.EventState, State: irc.StateRegistered}
	st := decode[StateData](t, recv(t, s, "state"))
	if st.Network != "libera" || st.State != "registered" {
		t.Fatalf("state = %+v", st)
	}

	conn.ch <- irc.Event{
		Network: "libera", Kind: irc.EventMessage,
		Msg: ircv4.MustParseMessage("@msgid=z1 :alice!u@h PRIVMSG #go :hi"), Time: time.Now(),
	}
	ev := decode[EventData](t, recv(t, s, "event"))
	if ev.Buffer != "#go" || ev.Sender != "alice" || ev.MsgID != "z1" || ev.ID == 0 {
		t.Fatalf("event = %+v", ev)
	}
}
