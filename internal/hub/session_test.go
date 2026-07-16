package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
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

func TestSessionSearch(t *testing.T) {
	h := newTestHub(t)
	ctxb := context.Background()
	seed := func(network, target string, tsMs int64, text string) {
		if _, err := h.store.Append(ctxb, network, target, store.Message{
			Time:    time.UnixMilli(tsMs),
			Sender:  "alice",
			Command: "PRIVMSG",
			Raw:     "@msgid=x" + text + " :alice!u@h PRIVMSG " + target + " :" + text,
			Text:    text,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("libera", "#go", 1000, "hello world")
	seed("libera", "#go", 2000, "goodbye world")
	seed("libera", "#rust", 3000, "world of rust")

	s := h.NewSession()
	defer s.Close()

	// Broad search across buffers, newest first.
	s.Handle(ctxb, request(t, "search", 4, SearchReq{Query: "world"}))
	env := recv(t, s, "search_results")
	if env.Seq != 4 {
		t.Fatalf("seq = %d", env.Seq)
	}
	data := decode[SearchData](t, env)
	if data.Query != "world" || len(data.Messages) != 3 {
		t.Fatalf("results = %+v", data)
	}
	if data.Messages[0].Buffer != "#rust" || data.Messages[0].Network != "libera" {
		t.Fatalf("first result misrouted: %+v", data.Messages[0])
	}

	// Scoped + multi-term.
	s.Handle(ctxb, request(t, "search", 5, SearchReq{Query: "goodbye world", Network: "libera", Buffer: "#go"}))
	data = decode[SearchData](t, recv(t, s, "search_results"))
	if len(data.Messages) != 1 || data.Messages[0].Raw == "" {
		t.Fatalf("scoped search = %+v", data.Messages)
	}

	// Empty query is a bad request, not a crash.
	s.Handle(ctxb, request(t, "search", 6, SearchReq{Query: "   "}))
	if got := decode[ErrorData](t, recv(t, s, "error")); got.Code != "bad_request" {
		t.Fatalf("empty query code = %q", got.Code)
	}
}

func TestSessionGetHistoryAround(t *testing.T) {
	h := newTestHub(t)
	seedHub(t, h, 10) // msgs 1..10 at ts 1000..10000
	s := h.NewSession()
	defer s.Close()

	s.Handle(context.Background(), request(t, "get_history", 8, HistoryReq{
		Network: "libera", Buffer: "#go", Around: &Cursor{TS: 5000, ID: 5}, Limit: 4,
	}))
	page := decode[HistoryData](t, recv(t, s, "history"))
	if len(page.Messages) != 4 {
		t.Fatalf("around page size = %d", len(page.Messages))
	}
	// Centered on msg 5: 3,4,5,6.
	if page.Messages[0].Raw != "msg 3" || page.Messages[3].Raw != "msg 6" {
		t.Fatalf("around window = %v", func() []string {
			out := make([]string, len(page.Messages))
			for i, m := range page.Messages {
				out[i] = m.Raw
			}
			return out
		}())
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

func TestSessionSendWithEchoMessage(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{
		ch: make(chan irc.Event, 4), name: "libera", nick: "AlteredParadox",
		caps: map[string]bool{"echo-message": true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")

	s := h.NewSession()
	defer s.Close()

	// With echo-message the send is acked but NOT persisted or broadcast
	// locally — the server's echo is the source of truth.
	s.Handle(ctx, request(t, "send", 8, SendData{Network: "libera", Target: "#go", Text: "hello"}))
	recv(t, s, "ok")
	if msgs, _ := h.store.Latest(ctx, "libera", "#go", 10); len(msgs) != 0 {
		t.Fatalf("self-persisted despite echo-message: %v", msgs)
	}

	// The echo arrives as a normal event and persists once.
	conn.ch <- irc.Event{
		Network: "libera", Kind: irc.EventMessage,
		Msg: ircv4.MustParseMessage(":AlteredParadox!u@h PRIVMSG #go :hello"), Time: time.Now(),
	}
	ev := decode[EventData](t, recv(t, s, "event"))
	if ev.Sender != "AlteredParadox" || ev.Buffer != "#go" {
		t.Fatalf("echo event = %+v", ev)
	}
	msgs, _ := h.store.Latest(ctx, "libera", "#go", 10)
	if len(msgs) != 1 {
		t.Fatalf("persisted %d messages, want 1", len(msgs))
	}
}

func TestRedactionInbound(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)

	s := h.NewSession()
	defer s.Close()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Msg: ircv4.MustParseMessage(line), Time: time.Now()}
	}

	// A message arrives and is stored...
	conn.ch <- ev("@msgid=bad1 :alice!u@h PRIVMSG #go :something regrettable")
	got := decode[EventData](t, recv(t, s, "event"))
	if got.MsgID != "bad1" {
		t.Fatalf("event = %+v", got)
	}

	// ...then an op redacts it: a redact push fires and the store row is
	// tombstoned.
	conn.ch <- ev(":op!o@h REDACT #go bad1 :off-topic")
	rd := decode[RedactData](t, recv(t, s, "redact"))
	if rd.Buffer != "#go" || rd.MsgID != "bad1" || rd.By != "op" || rd.Reason != "off-topic" {
		t.Fatalf("redact push = %+v", rd)
	}
	msgs, _ := h.store.Latest(ctx, "libera", "#go", 10)
	if len(msgs) != 1 || !msgs[0].Redacted || msgs[0].RedactReason != "off-topic" {
		t.Fatalf("store not tombstoned: %+v", msgs)
	}

	// Redacting an unknown msgid announces nothing.
	conn.ch <- ev(":op!o@h REDACT #go ghost :x")
	// Follow with a normal message; if a stray redact were queued we'd see
	// it first.
	conn.ch <- ev("@msgid=ok1 :alice!u@h PRIVMSG #go :fine message")
	if got := recv(t, s, "event"); got.Type != "event" {
		t.Fatalf("unknown-msgid redact leaked: %+v", got)
	}
}

func TestRedactionOutbound(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{
		ch: make(chan irc.Event), name: "libera", nick: "AlteredParadox",
		caps: map[string]bool{"draft/message-redaction": true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")

	s := h.NewSession()
	defer s.Close()

	s.Handle(ctx, request(t, "redact", 3, RedactReq{Network: "libera", Buffer: "#go", MsgID: "m1", Reason: "my mistake"}))
	recv(t, s, "ok")
	deadline := time.Now().Add(5 * time.Second)
	for len(conn.sentMsgs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("REDACT never sent")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := conn.sentMsgs()[0].String(); got != "REDACT #go m1 :my mistake" {
		t.Fatalf("wire = %q", got)
	}

	// Without the cap, a redact request is refused, not sent.
	conn.mu.Lock()
	conn.caps = nil
	conn.mu.Unlock()
	before := len(conn.sentMsgs())
	s.Handle(ctx, request(t, "redact", 4, RedactReq{Network: "libera", Buffer: "#go", MsgID: "m2"}))
	if got := decode[ErrorData](t, recv(t, s, "error")); got.Code != "unsupported" {
		t.Fatalf("error code = %q", got.Code)
	}
	if len(conn.sentMsgs()) != before {
		t.Fatal("REDACT sent despite missing cap")
	}
}

func TestMultilineSend(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{
		ch: make(chan irc.Event), name: "libera", nick: "AlteredParadox",
		caps: map[string]bool{"draft/multiline": true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")

	s := h.NewSession()
	defer s.Close()

	// Multi-line text with the cap goes out as one multiline batch and,
	// without echo, persists as a single message with embedded newlines.
	s.Handle(ctx, request(t, "send", 1, SendData{Network: "libera", Target: "#go", Text: "line one\nline two\nline three"}))
	recv(t, s, "ok", "event")
	if got := conn.multilineSends(); len(got) != 1 || got[0] != "#go|line one\\nline two\\nline three" {
		t.Fatalf("multiline sends = %v", got)
	}
	msgs, _ := h.store.Latest(ctx, "libera", "#go", 10)
	if len(msgs) != 1 || msgs[0].Text != "line one\nline two\nline three" {
		t.Fatalf("stored = %+v", msgs)
	}

	// A single-line message uses a plain PRIVMSG, not a batch.
	s.Handle(ctx, request(t, "send", 2, SendData{Network: "libera", Target: "#go", Text: "just one line"}))
	recv(t, s, "ok", "event")
	if len(conn.multilineSends()) != 1 {
		t.Fatal("single line used a multiline batch")
	}
	if sent := conn.sentMsgs(); len(sent) == 0 || sent[len(sent)-1].Trailing() != "just one line" {
		t.Fatalf("single-line PRIVMSG missing: %v", sent)
	}
}

func TestMultilineSendFallbackWithoutCap(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event), name: "libera", nick: "AlteredParadox"} // no cap
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")

	s := h.NewSession()
	defer s.Close()

	// Without the cap, newlines fall back to one PRIVMSG per line.
	s.Handle(ctx, request(t, "send", 1, SendData{Network: "libera", Target: "#go", Text: "a\nb"}))
	recv(t, s, "ok", "event")
	if len(conn.multilineSends()) != 0 {
		t.Fatal("used multiline batch without the cap")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(conn.sentMsgs()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("sent %d PRIVMSGs, want 2", len(conn.sentMsgs()))
		}
		time.Sleep(2 * time.Millisecond)
	}
	sent := conn.sentMsgs()
	if sent[0].Trailing() != "a" || sent[1].Trailing() != "b" {
		t.Fatalf("fallback lines = %q, %q", sent[0].Trailing(), sent[1].Trailing())
	}
}

func TestMonitorFlow(t *testing.T) {
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

	// Add two buddies: persisted, and forwarded to the connection.
	s.Handle(ctx, request(t, "monitor_add", 1, MonitorReq{Network: "libera", Nick: "alice"}))
	recv(t, s, "ok")
	s.Handle(ctx, request(t, "monitor_add", 2, MonitorReq{Network: "libera", Nick: "bob"}))
	recv(t, s, "ok")
	deadline := time.Now().Add(5 * time.Second)
	for len(conn.monAdds()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("MonitorAdd calls = %v", conn.monAdds())
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The server reports alice online, bob offline (730/731); both push.
	conn.ch <- ev(":srv 730 AlteredParadox :alice!u@h")
	if p := decode[PresenceData](t, recv(t, s, "presence")); p.Nick != "alice" || !p.Online {
		t.Fatalf("presence = %+v", p)
	}
	conn.ch <- ev(":srv 731 AlteredParadox :bob")
	if p := decode[PresenceData](t, recv(t, s, "presence")); p.Nick != "bob" || p.Online {
		t.Fatalf("presence = %+v", p)
	}

	// get_monitors reflects the persisted list with current presence.
	s.Handle(ctx, request(t, "get_monitors", 3, map[string]string{"network": "libera"}))
	data := decode[MonitorsData](t, recv(t, s, "monitors"))
	want := map[string]bool{"alice": true, "bob": false}
	if len(data.Monitors) != 2 {
		t.Fatalf("monitors = %+v", data.Monitors)
	}
	for _, m := range data.Monitors {
		if want[m.Nick] != m.Online {
			t.Fatalf("%s online = %v, want %v", m.Nick, m.Online, want[m.Nick])
		}
	}

	// Removing forwards to the connection and pushes an offline presence.
	s.Handle(ctx, request(t, "monitor_remove", 4, MonitorReq{Network: "libera", Nick: "alice"}))
	recv(t, s, "ok", "presence")
	if got := conn.monRemoves(); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("MonitorRemove = %v", got)
	}
	if list, _ := h.store.Monitors(ctx, "libera"); len(list) != 1 || list[0] != "bob" {
		t.Fatalf("stored monitors after remove = %v", list)
	}

	// A bad nick is rejected without touching the store.
	s.Handle(ctx, request(t, "monitor_add", 5, MonitorReq{Network: "libera", Nick: "bad nick"}))
	if got := decode[ErrorData](t, recv(t, s, "error")); got.Code != "bad_request" {
		t.Fatalf("bad nick code = %q", got.Code)
	}
}

func TestMonitorReestablishedOnRegistration(t *testing.T) {
	h := newTestHub(t)
	if err := h.store.AddMonitor(context.Background(), "libera", "alice"); err != nil {
		t.Fatal(err)
	}
	conn := &fakeConn{ch: make(chan irc.Event, 4), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)

	conn.ch <- irc.Event{Network: "libera", Kind: irc.EventState, State: irc.StateRegistered}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := conn.monitoredNicks(); len(got) == 1 && got[0] == "alice" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("SetMonitored not called with the persisted list; got %v", conn.monitoredNicks())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSessionCommand(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")

	s := h.NewSession()
	defer s.Close()

	cases := []struct {
		name    string
		data    CommandData
		wantErr string // "" = ok expected
		sent    string // expected wire form when ok
	}{
		{
			name: "join",
			data: CommandData{Network: "libera", Command: "join", Params: []string{"#go"}},
			sent: "JOIN #go",
		},
		{
			name: "join with key",
			data: CommandData{Network: "libera", Command: "JOIN", Params: []string{"#priv", "sekrit"}},
			sent: "JOIN #priv sekrit",
		},
		{
			name: "part with reason",
			data: CommandData{Network: "libera", Command: "PART", Params: []string{"#go", "later folks"}},
			sent: "PART #go :later folks",
		},
		{
			name: "topic",
			data: CommandData{Network: "libera", Command: "TOPIC", Params: []string{"#go", "new topic here"}},
			sent: "TOPIC #go :new topic here",
		},
		{
			name: "nick",
			data: CommandData{Network: "libera", Command: "NICK", Params: []string{"AlteredParadox2"}},
			sent: "NICK AlteredParadox2",
		},
		{
			name:    "disallowed command",
			data:    CommandData{Network: "libera", Command: "OPER", Params: []string{"x", "y"}},
			wantErr: "bad_request",
		},
		{
			name:    "too many params",
			data:    CommandData{Network: "libera", Command: "NICK", Params: []string{"a", "b"}},
			wantErr: "bad_request",
		},
		{
			name:    "missing params",
			data:    CommandData{Network: "libera", Command: "JOIN"},
			wantErr: "bad_request",
		},
		{
			name:    "crlf injection rejected",
			data:    CommandData{Network: "libera", Command: "JOIN", Params: []string{"#go\r\nQUIT"}},
			wantErr: "bad_request",
		},
		{
			name:    "space in non-final param rejected",
			data:    CommandData{Network: "libera", Command: "JOIN", Params: []string{"#a #b", "key"}},
			wantErr: "bad_request",
		},
		{
			name:    "unknown network",
			data:    CommandData{Network: "ghost", Command: "JOIN", Params: []string{"#go"}},
			wantErr: "unknown_network",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(conn.sentMsgs())
			s.Handle(ctx, request(t, "command", 4, tc.data))
			if tc.wantErr != "" {
				if got := decode[ErrorData](t, recv(t, s, "error")); got.Code != tc.wantErr {
					t.Fatalf("error code = %q, want %q", got.Code, tc.wantErr)
				}
				if len(conn.sentMsgs()) != before {
					t.Fatal("rejected command still hit the wire")
				}
				return
			}
			recv(t, s, "ok")
			sent := conn.sentMsgs()
			if got := sent[len(sent)-1].String(); got != tc.sent {
				t.Fatalf("wire = %q, want %q", got, tc.sent)
			}
		})
	}
}

func TestSessionGetChannel(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{
		ch: make(chan irc.Event), name: "libera", nick: "AlteredParadox",
		topic: "welcome",
		chans: map[string][]irc.Member{
			"#go": {{Nick: "alice", Prefix: "@+"}, {Nick: "AlteredParadox"}, {Nick: "bob", Prefix: "+", Away: true}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")

	s := h.NewSession()
	defer s.Close()

	s.Handle(ctx, request(t, "get_channel", 5, ChannelReq{Network: "libera", Buffer: "#go"}))
	got := decode[ChannelData](t, recv(t, s, "channel"))
	if !got.Joined || got.Topic != "welcome" || len(got.Members) != 3 {
		t.Fatalf("channel = %+v", got)
	}
	// get_channel lazily requests membership (no-implicit-names path).
	if names := conn.namesReqs(); len(names) == 0 || names[len(names)-1] != "#go" {
		t.Fatalf("EnsureNames not called: %v", names)
	}
	if got.Members[0].Nick != "alice" || got.Members[0].Prefix != "@+" {
		t.Fatalf("members = %+v", got.Members)
	}
	if !got.Members[2].Away {
		t.Fatalf("away flag lost: %+v", got.Members[2])
	}

	// Unjoined buffer (a PM) answers joined=false, not an error.
	s.Handle(ctx, request(t, "get_channel", 6, ChannelReq{Network: "libera", Buffer: "alice"}))
	if got := decode[ChannelData](t, recv(t, s, "channel")); got.Joined || len(got.Members) != 0 {
		t.Fatalf("pm channel = %+v", got)
	}
	// Unknown network likewise.
	s.Handle(ctx, request(t, "get_channel", 7, ChannelReq{Network: "ghost", Buffer: "#go"}))
	if got := decode[ChannelData](t, recv(t, s, "channel")); got.Joined {
		t.Fatalf("ghost network = %+v", got)
	}
}

func TestHubBroadcastsMembersChanged(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)

	s := h.NewSession()
	defer s.Close()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Msg: ircv4.MustParseMessage(line), Time: time.Now()}
	}

	// JOIN: hint names the channel (and the JOIN also persists as event).
	conn.ch <- ev(":alice!u@h JOIN #go")
	if got := decode[MembersChangedData](t, recv(t, s, "members_changed")); got.Buffer != "#go" {
		t.Fatalf("hint = %+v", got)
	}
	recv(t, s, "event")

	// QUIT is not persisted but still hints, network-wide.
	conn.ch <- ev(":alice!u@h QUIT :bye")
	if got := decode[MembersChangedData](t, recv(t, s, "members_changed")); got.Buffer != "" {
		t.Fatalf("quit hint = %+v", got)
	}

	// End of NAMES hints its channel.
	conn.ch <- ev(":srv 366 AlteredParadox #go :End of /NAMES list")
	if got := decode[MembersChangedData](t, recv(t, s, "members_changed")); got.Buffer != "#go" {
		t.Fatalf("366 hint = %+v", got)
	}

	// End of WHO (WHOX away/account discovery) hints its channel too,
	// and the 354 data lines themselves stay silent.
	conn.ch <- ev(":srv 354 AlteredParadox 152 alice G aliceacct")
	conn.ch <- ev(":srv 315 AlteredParadox #go :End of WHO list")
	if got := decode[MembersChangedData](t, recv(t, s, "members_changed")); got.Buffer != "#go" {
		t.Fatalf("315 hint = %+v", got)
	}
	expectSilence(t, s)

	// account-notify hints network-wide.
	conn.ch <- ev(":alice!u@h ACCOUNT aliceacct")
	if got := decode[MembersChangedData](t, recv(t, s, "members_changed")); got.Buffer != "" {
		t.Fatalf("ACCOUNT hint = %+v", got)
	}

	// Ordinary PRIVMSG does not hint.
	conn.ch <- ev(":alice!u@h PRIVMSG #go :hi")
	if env := recv(t, s, "event"); env.Type != "event" {
		t.Fatalf("got %+v", env)
	}
}

func TestTypingOutbound(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{
		ch: make(chan irc.Event), name: "libera", nick: "AlteredParadox",
		caps: map[string]bool{"message-tags": true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")

	s := h.NewSession()
	defer s.Close()

	// Fire-and-forget (no seq): TAGMSG goes out, nothing comes back.
	s.Handle(ctx, request(t, "typing", 0, TypingData{Network: "libera", Buffer: "#go", State: "active"}))
	deadline := time.Now().Add(5 * time.Second)
	for len(conn.sentMsgs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("TAGMSG never sent")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := conn.sentMsgs()[0].String(); got != "@+typing=active TAGMSG #go" {
		t.Fatalf("wire = %q", got)
	}
	expectSilence(t, s)

	// With a seq, it is acked.
	s.Handle(ctx, request(t, "typing", 9, TypingData{Network: "libera", Buffer: "#go", State: "done"}))
	recv(t, s, "ok")

	// Invalid state: dropped, not relayed.
	before := len(conn.sentMsgs())
	s.Handle(ctx, request(t, "typing", 0, TypingData{Network: "libera", Buffer: "#go", State: "typing!"}))
	expectSilence(t, s)
	if len(conn.sentMsgs()) != before {
		t.Fatal("invalid state hit the wire")
	}

	// Without message-tags the notification is silently suppressed.
	conn.mu.Lock()
	conn.caps = nil
	conn.mu.Unlock()
	s.Handle(ctx, request(t, "typing", 10, TypingData{Network: "libera", Buffer: "#go", State: "active"}))
	recv(t, s, "ok")
	if len(conn.sentMsgs()) != before {
		t.Fatal("TAGMSG sent without message-tags")
	}
}

func TestTypingInbound(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)

	s := h.NewSession()
	defer s.Close()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Msg: ircv4.MustParseMessage(line), Time: time.Now()}
	}

	// Channel typing is pushed with the channel buffer.
	conn.ch <- ev("@+typing=active :alice!u@h TAGMSG #go")
	got := decode[TypingData](t, recv(t, s, "typing"))
	if got.Buffer != "#go" || got.Nick != "alice" || got.State != "active" {
		t.Fatalf("typing = %+v", got)
	}

	// PM typing files under the sender's query buffer.
	conn.ch <- ev("@+typing=paused :bob!u@h TAGMSG AlteredParadox")
	got = decode[TypingData](t, recv(t, s, "typing"))
	if got.Buffer != "bob" || got.State != "paused" {
		t.Fatalf("pm typing = %+v", got)
	}

	// Our own echo, foreign-target TAGMSG, and tagless TAGMSG: silence.
	conn.ch <- ev("@+typing=active :AlteredParadox!u@h TAGMSG #go")
	conn.ch <- ev("@+typing=active :carol!u@h TAGMSG dave")
	conn.ch <- ev("@example/other=1 :alice!u@h TAGMSG #go")
	expectSilence(t, s)

	// TAGMSG is never persisted.
	if msgs, _ := h.store.Latest(ctx, "libera", "#go", 10); len(msgs) != 0 {
		t.Fatalf("TAGMSG persisted: %v", msgs)
	}
}

func TestChatHistoryBackfillFlow(t *testing.T) {
	h := newTestHub(t)
	// Existing history: the resume point for backfill.
	if _, err := h.store.Append(context.Background(), "libera", "#go", store.Message{
		Time: time.UnixMilli(5000), MsgID: "seed", Sender: "alice", Command: "PRIVMSG", Raw: "seed msg",
	}); err != nil {
		t.Fatal(err)
	}

	// A query buffer backfills at registration; the channel on JOIN.
	if _, err := h.store.Append(context.Background(), "libera", "bob", store.Message{
		Time: time.UnixMilli(4000), MsgID: "pm", Sender: "bob", Command: "PRIVMSG", Raw: "pm msg",
	}); err != nil {
		t.Fatal(err)
	}

	conn := &fakeConn{ch: make(chan irc.Event, 16), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)

	s := h.NewSession()
	defer s.Close()

	// Registration backfills the query buffer only.
	conn.ch <- irc.Event{Network: "libera", Kind: irc.EventState, State: irc.StateRegistered}
	recv(t, s, "state")
	deadline := time.Now().Add(5 * time.Second)
	for len(conn.histReqs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("query backfill never requested")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := conn.histReqs(); len(got) != 1 || got[0] != "bob@4000@pm" {
		t.Fatalf("registration backfill = %v", got)
	}

	// Our JOIN echo triggers the channel backfill.
	conn.ch <- irc.Event{
		Network: "libera", Kind: irc.EventMessage,
		Msg: ircv4.MustParseMessage(":AlteredParadox!u@h JOIN #go"), Time: time.Now(),
	}
	for len(conn.histReqs()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("channel backfill never requested")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := conn.histReqs()[1]; got != "#go@5000@seed" {
		t.Fatalf("channel backfill request = %q", got)
	}
	recv(t, s, "members_changed", "event") // JOIN side effects

	// The replay batch: persisted, but no live event/members pushes; a
	// history_changed hint follows the batch close.
	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Msg: ircv4.MustParseMessage(line), Time: time.Now()}
	}
	conn.ch <- ev(":srv BATCH +r1 chathistory #go")
	conn.ch <- ev("@batch=r1;msgid=h1;time=2026-07-15T00:00:06.000Z :bob!u@h PRIVMSG #go :missed one")
	conn.ch <- ev("@batch=r1;msgid=h2;time=2026-07-15T00:00:07.000Z :bob!u@h JOIN #go")
	conn.ch <- ev(":srv BATCH -r1")
	hint := decode[HistoryChangedData](t, recv(t, s, "history_changed", "event", "members_changed"))
	if hint.Network != "libera" || hint.Buffer != "#go" {
		t.Fatalf("hint = %+v", hint)
	}

	msgs, err := h.store.Latest(ctx, "libera", "#go", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 || msgs[1].Raw == "" || msgs[1].MsgID != "h1" {
		t.Fatalf("persisted: %+v", msgs)
	}
	if !msgs[1].Time.Equal(time.Date(2026, 7, 15, 0, 0, 6, 0, time.UTC)) {
		t.Fatalf("server-time not applied: %v", msgs[1].Time)
	}

	// Re-delivery of an already-stored msgid (overlapping backfill) is
	// dropped silently, even outside a batch.
	conn.ch <- ev("@msgid=h1;time=2026-07-15T00:00:06.000Z :bob!u@h PRIVMSG #go :missed one")
	expectSilence(t, s)
	if msgs, _ := h.store.Latest(ctx, "libera", "#go", 10); len(msgs) != 4 {
		t.Fatalf("duplicate persisted: %d rows", len(msgs))
	}

	// Live traffic still pushes normally.
	conn.ch <- ev("@msgid=live1 :bob!u@h PRIVMSG #go :fresh")
	if got := decode[EventData](t, recv(t, s, "event")); got.Raw == "" || got.MsgID != "live1" {
		t.Fatalf("live event = %+v", got)
	}

	// No further backfills beyond the query and joined channel.
	if got := conn.histReqs(); len(got) != 2 {
		t.Fatalf("backfill requests = %v", got)
	}
}

func TestReadMarkerBridgeOutbound(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{
		ch: make(chan irc.Event), name: "libera", nick: "AlteredParadox",
		caps: map[string]bool{"draft/read-marker": true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")

	s := h.NewSession()
	defer s.Close()

	s.Handle(ctx, request(t, "set_read_marker", 2, SetMarkerData{Network: "libera", Buffer: "#go", Time: 1752570000000}))
	recv(t, s, "read_marker")
	deadline := time.Now().Add(5 * time.Second)
	for len(conn.sentMsgs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("MARKREAD never sent upstream")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := conn.sentMsgs()[0].String(); got != "MARKREAD #go timestamp=2025-07-15T09:00:00.000Z" {
		t.Fatalf("wire = %q", got)
	}

	// A regressing set sends the authoritative (newer) value upstream,
	// never the regression.
	s.Handle(ctx, request(t, "set_read_marker", 3, SetMarkerData{Network: "libera", Buffer: "#go", Time: 1000}))
	recv(t, s, "read_marker")
	for len(conn.sentMsgs()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("second MARKREAD never sent")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := conn.sentMsgs()[1].String(); got != "MARKREAD #go timestamp=2025-07-15T09:00:00.000Z" {
		t.Fatalf("regression leaked upstream: %q", got)
	}
}

func TestReadMarkerBridgeInbound(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)

	s := h.NewSession()
	defer s.Close()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Msg: ircv4.MustParseMessage(line), Time: time.Now()}
	}

	// Another device of ours read #go: store updates and sessions learn.
	conn.ch <- ev(":irc.test MARKREAD #go timestamp=2025-07-15T09:00:00.000Z")
	got := decode[MarkerData](t, recv(t, s, "read_marker"))
	if got.Buffer != "#go" || got.Time != 1752570000000 {
		t.Fatalf("marker push = %+v", got)
	}
	stored, err := h.store.ReadMarker(ctx, "libera", "#go")
	if err != nil || stored.UnixMilli() != 1752570000000 {
		t.Fatalf("stored = %v, %v", stored, err)
	}

	// An older upstream value never regresses the marker; the push
	// carries the authoritative newer one.
	conn.ch <- ev(":irc.test MARKREAD #go timestamp=2020-01-01T00:00:00.000Z")
	got = decode[MarkerData](t, recv(t, s, "read_marker"))
	if got.Time != 1752570000000 {
		t.Fatalf("regressed to %d", got.Time)
	}

	// The "*" no-marker reply and garbage are ignored, not persisted.
	conn.ch <- ev(":irc.test MARKREAD alice *")
	conn.ch <- ev(":irc.test MARKREAD alice timestamp=yesterday")
	expectSilence(t, s)
	if m, _ := h.store.ReadMarker(ctx, "libera", "alice"); !m.IsZero() {
		t.Fatalf("garbage set a marker: %v", m)
	}
}

func TestReadMarkerFetchedForQueriesOnRegistration(t *testing.T) {
	h := newTestHub(t)
	if _, err := h.store.Append(context.Background(), "libera", "bob", store.Message{
		Time: time.UnixMilli(4000), Sender: "bob", Command: "PRIVMSG", Raw: "pm",
	}); err != nil {
		t.Fatal(err)
	}
	conn := &fakeConn{
		ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox",
		caps: map[string]bool{"draft/read-marker": true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)

	conn.ch <- irc.Event{Network: "libera", Kind: irc.EventState, State: irc.StateRegistered}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var get bool
		for _, m := range conn.sentMsgs() {
			if m.String() == "MARKREAD bob" {
				get = true
			}
		}
		if get {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("MARKREAD get never sent; sent: %v", conn.sentMsgs())
		}
		time.Sleep(2 * time.Millisecond)
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

func TestPrefsFlow(t *testing.T) {
	h := newTestHub(t)
	ctx := context.Background()
	a := h.NewSession()
	defer a.Close()
	b := h.NewSession()
	defer b.Close()

	// Nothing stored yet: prefs is null/absent.
	a.Handle(ctx, request(t, "get_prefs", 1, nil))
	if d := decode[PrefsData](t, recv(t, a, "prefs")); len(d.Prefs) != 0 {
		t.Fatalf("initial prefs = %s", d.Prefs)
	}

	// Setting from session A acks A and pushes the blob to B, not A.
	blob := `{"theme":"dark","accent":"rose"}`
	a.Handle(ctx, request(t, "set_prefs", 2, PrefsData{Prefs: json.RawMessage(blob)}))
	recv(t, a, "ok")
	if d := decode[PrefsData](t, recv(t, b, "prefs")); string(d.Prefs) != blob {
		t.Fatalf("pushed prefs = %s", d.Prefs)
	}

	// get_prefs returns the stored blob.
	b.Handle(ctx, request(t, "get_prefs", 3, nil))
	if d := decode[PrefsData](t, recv(t, b, "prefs")); string(d.Prefs) != blob {
		t.Fatalf("stored prefs = %s", d.Prefs)
	}

	// Empty and oversized blobs are rejected.
	a.Handle(ctx, request(t, "set_prefs", 4, PrefsData{}))
	if e := decode[ErrorData](t, recv(t, a, "error")); e.Code != "bad_request" {
		t.Fatalf("empty set_prefs error = %+v", e)
	}
	big := make([]byte, maxPrefsBytes+16)
	for i := range big {
		big[i] = 'x'
	}
	big[0], big[len(big)-1] = '"', '"'
	a.Handle(ctx, request(t, "set_prefs", 5, PrefsData{Prefs: big}))
	if e := decode[ErrorData](t, recv(t, a, "error")); e.Code != "bad_request" {
		t.Fatalf("oversized set_prefs error = %+v", e)
	}

	// The rejected writes did not clobber the stored value.
	a.Handle(ctx, request(t, "get_prefs", 6, nil))
	if d := decode[PrefsData](t, recv(t, a, "prefs")); string(d.Prefs) != blob {
		t.Fatalf("prefs after rejected writes = %s", d.Prefs)
	}
}

func TestPaginatedBackfill(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox", pageSize: 2}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	s := h.NewSession()
	defer s.Close()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Msg: ircv4.MustParseMessage(line), Time: time.Now()}
	}
	fullBatch := func(ref string, sec int) {
		conn.ch <- ev(fmt.Sprintf(":srv BATCH +%s chathistory #go", ref))
		conn.ch <- ev(fmt.Sprintf("@batch=%s;msgid=%sa;time=2026-07-15T00:00:%02d.000Z :bob!u@h PRIVMSG #go :a", ref, ref, sec))
		conn.ch <- ev(fmt.Sprintf("@batch=%s;msgid=%sb;time=2026-07-15T00:00:%02d.500Z :bob!u@h PRIVMSG #go :b", ref, ref, sec))
		conn.ch <- ev(fmt.Sprintf(":srv BATCH -%s", ref))
		recv(t, s, "history_changed", "event", "state")
	}
	waitReqs := func(n int, what string) []string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for len(conn.histReqs()) < n && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		got := conn.histReqs()
		if len(got) != n {
			t.Fatalf("%s: requests = %v, want %d", what, got, n)
		}
		return got
	}

	// A full replay page means the gap may extend past it: closing the
	// batch requests the next page, anchored at the batch's own newest
	// message (time + msgid), not at whatever the store holds.
	fullBatch("r1", 2)
	want := fmt.Sprintf("#go@%d@r1b", time.Date(2026, 7, 15, 0, 0, 2, 500e6, time.UTC).UnixMilli())
	if got := waitReqs(1, "after full page"); got[0] != want {
		t.Fatalf("follow-up = %q, want %q", got[0], want)
	}

	// A partial page ends pagination.
	conn.ch <- ev(":srv BATCH +r2 chathistory #go")
	conn.ch <- ev("@batch=r2;msgid=r2a;time=2026-07-15T00:00:03.000Z :bob!u@h PRIVMSG #go :three")
	conn.ch <- ev(":srv BATCH -r2")
	recv(t, s, "history_changed", "event")
	waitReqs(1, "after partial page")

	// Follow-ups are bounded per target and connection.
	for i := 0; i < 15; i++ {
		fullBatch(fmt.Sprintf("b%d", i), 10+i)
	}
	waitReqs(maxBackfillPages, "at the page cap")

	// A new registration restores the pagination budget.
	conn.ch <- irc.Event{Network: "libera", Kind: irc.EventState, State: irc.StateRegistered, Time: time.Now()}
	fullBatch("fresh", 50)
	waitReqs(maxBackfillPages+1, "after re-registration")
}

func TestServerInfoFlow(t *testing.T) {
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

	// WHOIS replies are forwarded, with our nick stripped.
	conn.ch <- ev(":srv 311 AlteredParadox alice ~u example.org * :Alice A.")
	info := decode[ServerInfoData](t, recv(t, s, "server_info"))
	if info.Network != "libera" || info.Text != "alice ~u example.org * Alice A." {
		t.Fatalf("info = %+v", info)
	}

	// Error numerics too.
	conn.ch <- ev(":srv 401 AlteredParadox ghost :No such nick/channel")
	if info := decode[ServerInfoData](t, recv(t, s, "server_info")); info.Text != "ghost No such nick/channel" {
		t.Fatalf("error info = %+v", info)
	}

	// Connect-time noise (LUSERS, ISUPPORT) is not forwarded, and MOTD is
	// gated until a client actually asks for it.
	conn.ch <- ev(":srv 251 AlteredParadox :There are 5 users")
	conn.ch <- ev(":srv 375 AlteredParadox :- message of the day -")
	conn.ch <- ev(":srv 372 AlteredParadox :- welcome")
	conn.ch <- ev(":srv 376 AlteredParadox :End of /MOTD")
	expectSilence(t, s)

	// /motd opens the gate; the end numeric closes it again.
	s.Handle(ctx, request(t, "command", 1, CommandData{Network: "libera", Command: "MOTD", Params: nil}))
	recv(t, s, "ok")
	conn.ch <- ev(":srv 375 AlteredParadox :- message of the day -")
	conn.ch <- ev(":srv 372 AlteredParadox :- welcome")
	conn.ch <- ev(":srv 376 AlteredParadox :End of /MOTD")
	if info := decode[ServerInfoData](t, recv(t, s, "server_info")); info.Text != "- message of the day -" {
		t.Fatalf("motd line 1 = %+v", info)
	}
	recv(t, s, "server_info")
	recv(t, s, "server_info")
	conn.ch <- ev(":srv 372 AlteredParadox :- late line after the gate closed")
	expectSilence(t, s)
}

func TestCommandAllowlistAdditions(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	s := h.NewSession()
	defer s.Close()

	ok := []CommandData{
		{Network: "libera", Command: "WHOIS", Params: []string{"alice"}},
		{Network: "libera", Command: "AWAY", Params: nil}, // zero params: back from away
		{Network: "libera", Command: "AWAY", Params: []string{"gone fishing"}},
		{Network: "libera", Command: "MODE", Params: []string{"#go", "+o", "alice"}},
		{Network: "libera", Command: "KICK", Params: []string{"#go", "alice", "spamming the channel"}},
		{Network: "libera", Command: "NOTICE", Params: []string{"alice", "psst over here"}},
		{Network: "libera", Command: "LIST", Params: nil},
	}
	for i, d := range ok {
		s.Handle(ctx, request(t, "command", int64(i+1), d))
		if env := recv(t, s, "ok", "server_info"); env.Seq != int64(i+1) {
			t.Fatalf("%s: seq = %d", d.Command, env.Seq)
		}
	}

	bad := []CommandData{
		{Network: "libera", Command: "QUIT", Params: nil},                          // never allowed
		{Network: "libera", Command: "PRIVMSG", Params: []string{"#go", "x"}},      // use "send"
		{Network: "libera", Command: "KICK", Params: []string{"#go"}},              // too few
		{Network: "libera", Command: "MODE", Params: []string{"#go", "+o o", "x"}}, // space mid-param
		{Network: "libera", Command: "WHOIS", Params: []string{"a", "b", "c"}},     // too many
	}
	for i, d := range bad {
		s.Handle(ctx, request(t, "command", int64(100+i), d))
		if e := decode[ErrorData](t, recv(t, s, "error")); e.Code != "bad_request" {
			t.Fatalf("%s: error = %+v", d.Command, e)
		}
	}
}

func TestInviteSurfaced(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 4), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	s := h.NewSession()
	defer s.Close()

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Msg: ircv4.MustParseMessage(line), Time: time.Now()}
	}

	// A direct invite names "you".
	conn.ch <- ev(":alice!u@h INVITE AlteredParadox #secret")
	if info := decode[ServerInfoData](t, recv(t, s, "server_info")); info.Text != "alice invited you to #secret" {
		t.Fatalf("direct invite = %+v", info)
	}

	// invite-notify: someone else being invited is shown by nick.
	conn.ch <- ev(":alice!u@h INVITE bob #secret")
	if info := decode[ServerInfoData](t, recv(t, s, "server_info")); info.Text != "alice invited bob to #secret" {
		t.Fatalf("third-party invite = %+v", info)
	}
}

func TestStandardRepliesSurfaced(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 4), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	s := h.NewSession()
	defer s.Close()

	conn.ch <- irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
		Msg: ircv4.MustParseMessage(":srv FAIL REHASH CONFIG_BAD :Invalid config")}
	info := decode[ServerInfoData](t, recv(t, s, "server_info"))
	if info.Text != "fail: REHASH CONFIG_BAD Invalid config" {
		t.Fatalf("FAIL = %+v", info)
	}
	conn.ch <- irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
		Msg: ircv4.MustParseMessage(":srv WARN * INVALID_UTF8 :Message dropped")}
	if info := decode[ServerInfoData](t, recv(t, s, "server_info")); info.Text != "warn: * INVALID_UTF8 Message dropped" {
		t.Fatalf("WARN = %+v", info)
	}
}

func TestQuitNickScrollback(t *testing.T) {
	h := newTestHub(t)
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	s := h.NewSession()
	defer s.Close()

	ev := func(line string, affected ...string) irc.Event {
		return irc.Event{
			Network: "libera", Kind: irc.EventMessage,
			Msg: ircv4.MustParseMessage(line), Affected: affected, Time: time.Now(),
		}
	}

	// A PM from alice opens a query buffer under her nick's casing.
	conn.ch <- ev(":Alice!u@h PRIVMSG AlteredParadox :psst")
	recv(t, s, "event")

	// Live QUIT: a line lands in each shared channel AND the open query
	// (found case-insensitively), each pushed live.
	conn.ch <- ev(":alice!u@h QUIT :gone fishing", "#go", "#rust")
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		got := decode[EventData](t, recv(t, s, "event", "members_changed"))
		if got.Command != "QUIT" {
			t.Fatalf("push %d = %+v", i, got)
		}
		seen[got.Buffer] = true
	}
	if !seen["#go"] || !seen["#rust"] || !seen["Alice"] {
		t.Fatalf("QUIT buffers = %v", seen)
	}
	if msgs, _ := h.store.Latest(ctx, "libera", "#go", 5); countRaw(msgs, "QUIT") != 1 {
		t.Fatalf("#go store: %+v", msgs)
	}

	// Live NICK renders per channel too.
	conn.ch <- ev(":bob!u@h NICK bobby", "#go")
	if got := decode[EventData](t, recv(t, s, "event", "members_changed")); got.Command != "NICK" || got.Buffer != "#go" {
		t.Fatalf("NICK push = %+v", got)
	}

	// Replayed QUIT (chathistory + event-playback): persisted to the
	// batch's target only, no live push, deduplicated by msgid.
	conn.ch <- ev(":srv BATCH +r1 chathistory #go")
	conn.ch <- ev("@batch=r1;msgid=q1;time=2026-07-16T00:00:01.000Z :carol!u@h QUIT :netsplit")
	conn.ch <- ev("@batch=r1;msgid=q1;time=2026-07-16T00:00:01.000Z :carol!u@h QUIT :netsplit") // overlap
	conn.ch <- ev(":srv BATCH -r1")
	recv(t, s, "history_changed", "event", "members_changed")
	msgs, _ := h.store.Latest(ctx, "libera", "#go", 10)
	if countRaw(msgs, "carol") != 1 {
		t.Fatalf("replayed QUIT rows = %d, want 1 (deduped): %+v", countRaw(msgs, "carol"), msgs)
	}
	expectSilence(t, s)
}

func countRaw(msgs []store.Message, sub string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Raw, sub) {
			n++
		}
	}
	return n
}
