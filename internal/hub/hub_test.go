package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ircthing/internal/irc"
	"ircthing/internal/netconf"
	"ircthing/internal/store"

	ircv4 "gopkg.in/irc.v4"
)

// TestPersistAutojoinMirrorsMembership checks that our own live JOIN/PART is
// mirrored into the stored network definition's channel list (so a restart
// rejoins it / stops), that a repeat or a different-casing JOIN is a no-op,
// and that another user's membership change is ignored.
func TestPersistAutojoinMirrorsMembership(t *testing.T) {
	h := newTestHub(t)
	ctx := context.Background()
	raw, err := json.Marshal(netconf.Network{Name: "libera", Addr: "irc.test:6697", Nick: "AlteredParadox"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutNetworkConfig(ctx, "libera", string(raw)); err != nil {
		t.Fatal(err)
	}
	c := &fakeConn{name: "libera", nick: "AlteredParadox"}
	channels := func() []string {
		got, ok, err := h.store.NetworkConfig(ctx, "libera")
		if err != nil || !ok {
			t.Fatalf("read config: ok=%v err=%v", ok, err)
		}
		var parsed netconf.Network
		if err := json.Unmarshal([]byte(got.Config), &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed.Channels
	}
	feed := func(line string) {
		h.persistAutojoin(ctx, c, irc.Event{Network: "libera", Msg: ircv4.MustParseMessage(line)})
	}

	feed(":AlteredParadox!u@h JOIN #go")
	if ch := channels(); len(ch) != 1 || ch[0] != "#go" {
		t.Fatalf("after JOIN: channels = %v, want [#go]", ch)
	}
	feed(":ALTEREDPARADOX!u@h JOIN #GO") // same channel, different casing -> no-op
	feed(":bob!u@h JOIN #other") // someone else -> ignored
	if ch := channels(); len(ch) != 1 || ch[0] != "#go" {
		t.Fatalf("dup/other JOIN changed channels: %v", ch)
	}
	feed(":AlteredParadox!u@h PART #go")
	if ch := channels(); len(ch) != 0 {
		t.Fatalf("after PART: channels = %v, want []", ch)
	}

	// An oversized channel name is NOT persisted (it would fail restart
	// validation); a framing byte likewise.
	feed(":AlteredParadox!u@h JOIN #" + strings.Repeat("x", maxPersistedChannelLen))
	if ch := channels(); len(ch) != 0 {
		t.Fatalf("oversized channel was persisted: %v", ch)
	}
}

// persistAutojoin runs on the Hub event goroutine, which StopNetwork waits to
// exit while holding netOps; it must therefore never BLOCK on netOps (that is
// the deadlock finding). It uses TryLock and skips while a network op holds it.
func TestPersistAutojoinDoesNotBlockUnderNetOps(t *testing.T) {
	h := newTestHub(t)
	ctx := context.Background()
	raw, err := json.Marshal(netconf.Network{Name: "libera", Addr: "irc.test:6697", Nick: "AlteredParadox"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutNetworkConfig(ctx, "libera", string(raw)); err != nil {
		t.Fatal(err)
	}
	c := &fakeConn{name: "libera", nick: "AlteredParadox"}
	join := irc.Event{Network: "libera", Msg: ircv4.MustParseMessage(":AlteredParadox!u@h JOIN #go")}

	h.netOps.Lock() // simulate an in-progress network edit/delete
	done := make(chan struct{})
	go func() {
		h.persistAutojoin(ctx, c, join)
		close(done)
	}()
	select {
	case <-done: // TryLock skipped, returned promptly — no deadlock
	case <-time.After(3 * time.Second):
		h.netOps.Unlock()
		t.Fatal("persistAutojoin blocked while netOps was held")
	}
	h.netOps.Unlock()

	// With netOps free, persistence works normally.
	h.persistAutojoin(ctx, c, join)
	got, _, _ := h.store.NetworkConfig(ctx, "libera")
	var parsed netconf.Network
	if err := json.Unmarshal([]byte(got.Config), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Channels) != 1 || parsed.Channels[0] != "#go" {
		t.Fatalf("channels = %v, want [#go]", parsed.Channels)
	}
}

func TestPersistTarget(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		ourNick string
		want    string
		ok      bool
	}{
		{"channel privmsg", ":alice!u@h PRIVMSG #go :hi", "AlteredParadox", "#go", true},
		{"ampersand channel", ":alice!u@h PRIVMSG &local :hi", "AlteredParadox", "&local", true},
		{"channel notice", ":alice!u@h NOTICE #go :psst", "AlteredParadox", "#go", true},
		{"query files under sender", ":alice!u@h PRIVMSG AlteredParadox :hello", "AlteredParadox", "alice", true},
		{"query nick is case-insensitive", ":alice!u@h PRIVMSG ALTEREDPARADOX :hello", "AlteredParadox", "alice", true},
		{"nickserv notice files under the server buffer", ":NickServ!s@services NOTICE AlteredParadox :identify pls", "AlteredParadox", "*", true},
		{"server notice files under the server buffer", ":irc.test NOTICE AlteredParadox :*** Looking up your hostname", "AlteredParadox", "*", true},
		{"pre-registration server notice to * files under the server buffer", ":irc.test NOTICE * :*** hi", "", "*", true},
		{"user notice files under the server buffer", ":alice!u@h NOTICE AlteredParadox :ping", "AlteredParadox", "*", true},
		{"privmsg to someone else dropped", ":alice!u@h PRIVMSG bob :hi", "AlteredParadox", "", false},
		{"our echoed pm files under the recipient", ":AlteredParadox!u@h PRIVMSG alice :hi", "AlteredParadox", "alice", true},
		{"our echoed notice files under the recipient", ":AlteredParadox!u@h NOTICE alice :psst", "AlteredParadox", "alice", true},
		{"privmsg to us with no prefix dropped", "PRIVMSG AlteredParadox :hi", "AlteredParadox", "", false},
		{"pm before nick known dropped", ":alice!u@h PRIVMSG AlteredParadox :hi", "", "", false},
		{"join", ":alice!u@h JOIN #go", "AlteredParadox", "#go", true},
		{"part", ":alice!u@h PART #go :bye", "AlteredParadox", "#go", true},
		{"topic", ":alice!u@h TOPIC #go :new topic", "AlteredParadox", "#go", true},
		{"kick", ":op!u@h KICK #go alice :out", "AlteredParadox", "#go", true},
		{"channel mode", ":op!u@h MODE #go +o alice", "AlteredParadox", "#go", true},
		{"user mode dropped", ":AlteredParadox MODE AlteredParadox :+i", "AlteredParadox", "", false},
		// QUIT/NICK never resolve to a single target here — the hub's
		// persistMembership fans them out per shared channel instead.
		{"quit has no single target", ":alice!u@h QUIT :bye", "AlteredParadox", "", false},
		{"nick change has no single target", ":alice!u@h NICK alicia", "AlteredParadox", "", false},
		{"numeric dropped", ":irc.test 001 AlteredParadox :Welcome", "AlteredParadox", "", false},
		{"ping dropped", "PING :x", "AlteredParadox", "", false},
		{"ctcp query dropped", ":alice!u@h PRIVMSG AlteredParadox :\x01VERSION\x01", "AlteredParadox", "", false},
		{"ctcp reply notice dropped", ":alice!u@h NOTICE AlteredParadox :\x01VERSION theirclient\x01", "AlteredParadox", "", false},
		{"ctcp action persists", ":alice!u@h PRIVMSG #go :\x01ACTION waves\x01", "AlteredParadox", "#go", true},
		{"statusmsg op-only files under bare channel", ":op!u@h PRIVMSG @#go :ops only", "AlteredParadox", "#go", true},
		{"statusmsg voice-only files under bare channel", ":op!u@h PRIVMSG +#go :voiced only", "AlteredParadox", "#go", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ircv4.ParseMessage(tc.line)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.line, err)
			}
			got, ok := persistTarget(m, tc.ourNick, defaultIsChannel, foldRFC1459, "~&@%+")
			if got != tc.want || ok != tc.ok {
				t.Fatalf("persistTarget = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// replayTarget must file a REPLAYED private NOTICE from a correspondent in the
// server buffer "*" — the same place live noticeTarget files it — so a
// reconnect chathistory replay does not duplicate it into the query buffer
// (per-buffer msgid dedup would let both persist: a duplicate row + phantom
// unread). Channel notices and our own echoed notice keep the batch target.
func TestReplayTargetNoticeRouting(t *testing.T) {
	c := &fakeConn{nick: "AlteredParadox"}
	cases := []struct {
		name        string
		line        string
		batchTarget string
		want        string
		ok          bool
	}{
		{"incoming private notice -> server buffer", ":alice!u@h NOTICE AlteredParadox :ping", "alice", "*", true},
		{"incoming private notice, case-variant batch", ":Alice!u@h NOTICE AlteredParadox :ping", "alice", "*", true},
		{"our own echoed notice -> recipient query", ":AlteredParadox!u@h NOTICE alice :psst", "alice", "alice", true},
		{"channel notice -> the channel", ":alice!u@h NOTICE #go :psst", "#go", "#go", true},
		{"private message -> the query", ":alice!u@h PRIVMSG AlteredParadox :hi", "alice", "alice", true},
		{"channel message -> the channel", ":alice!u@h PRIVMSG #go :hi", "#go", "#go", true},
		{"statusmsg channel notice -> bare channel", ":alice!u@h NOTICE @#go :hi", "@#go", "#go", true},
		{"non-action ctcp dropped", ":alice!u@h NOTICE AlteredParadox :\x01VERSION x\x01", "alice", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := ircv4.MustParseMessage(tc.line)
			got, ok := replayTarget(m, tc.batchTarget, c)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("replayTarget = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestStoreMessage(t *testing.T) {
	recv := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		line       string
		wantTime   time.Time
		wantMsgID  string
		wantSender string
	}{
		{
			name:       "server-time and msgid tags win",
			line:       "@time=2026-07-15T09:30:00.123Z;msgid=abc123 :alice!u@h PRIVMSG #go :hi",
			wantTime:   time.Date(2026, 7, 15, 9, 30, 0, 123_000_000, time.UTC),
			wantMsgID:  "abc123",
			wantSender: "alice",
		},
		{
			name:       "no tags falls back to receive time",
			line:       ":alice!u@h PRIVMSG #go :hi",
			wantTime:   recv,
			wantSender: "alice",
		},
		{
			name:       "malformed time tag falls back to receive time",
			line:       "@time=yesterday :alice!u@h PRIVMSG #go :hi",
			wantTime:   recv,
			wantSender: "alice",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := ircv4.ParseMessage(tc.line)
			if err != nil {
				t.Fatal(err)
			}
			got := storeMessage(irc.Event{Kind: irc.EventMessage, Msg: msg, Time: recv})
			if !got.Time.Equal(tc.wantTime) {
				t.Fatalf("time = %v, want %v", got.Time, tc.wantTime)
			}
			if got.MsgID != tc.wantMsgID || got.Sender != tc.wantSender {
				t.Fatalf("msgid/sender = %q/%q", got.MsgID, got.Sender)
			}
			if got.Command != "PRIVMSG" || got.Raw == "" {
				t.Fatalf("command/raw = %q/%q", got.Command, got.Raw)
			}
		})
	}
}

type fakeConn struct {
	ch    chan irc.Event
	name  string
	nick  string
	topic string
	chans map[string][]irc.Member
	caps  map[string]bool
	// pageSize is HistoryPageSize; 0 means the default 100.
	pageSize int

	mu        sync.Mutex
	sent      []*ircv4.Message
	sendErr   error
	hist      []string // RequestChatHistory calls as "target@sinceMs"
	names     []string // EnsureNames calls
	multiline []string // SendMultiline calls
	monitored []string // SetMonitored
	monAdd    []string // MonitorAdd
	monRemove []string // MonitorRemove
}

func (f *fakeConn) Events() <-chan irc.Event     { return f.ch }
func (f *fakeConn) Name() string                 { return f.name }
func (f *fakeConn) Nick() string                 { return f.nick }
func (f *fakeConn) CapEnabled(name string) bool  { return f.caps[name] }
func (f *fakeConn) IsChannel(target string) bool { return defaultIsChannel(target) }
func (f *fakeConn) ChanTypes() string            { return "#&" }
func (f *fakeConn) StatusPrefixes() string       { return "~&@%+" }
func (f *fakeConn) Fold(name string) string      { return foldRFC1459(name) }

// foldRFC1459 mirrors the default IRC casemapping for test fakes.
func foldRFC1459(name string) string {
	b := []byte(strings.ToLower(name))
	for i, c := range b {
		switch c {
		case '[':
			b[i] = '{'
		case ']':
			b[i] = '}'
		case '\\':
			b[i] = '|'
		case '~':
			b[i] = '^'
		}
	}
	return string(b)
}

func (f *fakeConn) HistoryPageSize() int {
	if f.pageSize > 0 {
		return f.pageSize
	}
	return 100
}

func (f *fakeConn) Channel(name string) (string, []irc.Member, bool) {
	ms, ok := f.chans[name]
	return f.topic, ms, ok
}

func (f *fakeConn) Send(m *ircv4.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeConn) sentMsgs() []*ircv4.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*ircv4.Message(nil), f.sent...)
}

func (f *fakeConn) RequestChatHistory(target string, sinceMs int64, msgid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hist = append(f.hist, fmt.Sprintf("%s@%d@%s", target, sinceMs, msgid))
}

func (f *fakeConn) histReqs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hist...)
}

func (f *fakeConn) EnsureNames(channel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, channel)
}

func (f *fakeConn) namesReqs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...)
}

func (f *fakeConn) SendMultiline(target string, lines []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.multiline = append(f.multiline, target+"|"+strings.Join(lines, "\\n"))
	return nil
}

func (f *fakeConn) multilineSends() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.multiline...)
}

func (f *fakeConn) SetMonitored(nicks []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.monitored = append([]string(nil), nicks...)
}

func (f *fakeConn) MonitorAdd(nick string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.monAdd = append(f.monAdd, nick)
}

func (f *fakeConn) MonitorRemove(nick string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.monRemove = append(f.monRemove, nick)
}

func (f *fakeConn) monitoredNicks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.monitored...)
}

func (f *fakeConn) monAdds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.monAdd...)
}

func (f *fakeConn) monRemoves() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.monRemove...)
}

func TestHubPersistsEvents(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	conn := &fakeConn{ch: make(chan irc.Event, 16), name: "libera", nick: "AlteredParadox"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		New(st).Run(ctx, conn)
	}()

	lines := []string{
		":alice!u@h JOIN #go",
		"@msgid=m1 :alice!u@h PRIVMSG #go :hello channel",
		":bob!u@h PRIVMSG AlteredParadox :hello query",
		":irc.test 372 AlteredParadox :- motd line", // must be dropped
		"PING :x",                        // must be dropped
	}
	for _, l := range lines {
		conn.ch <- irc.Event{
			Network: "libera",
			Kind:    irc.EventMessage,
			Msg:     ircv4.MustParseMessage(l),
			Time:    time.Now(),
		}
	}
	// State events must not disturb persistence.
	conn.ch <- irc.Event{Network: "libera", Kind: irc.EventState, State: irc.StateRegistered}

	waitFor := func(network, target string, want int) []store.Message {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			msgs, err := st.Latest(context.Background(), network, target, 50)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) >= want {
				return msgs
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s/%s has %d messages, want %d", network, target, len(msgs), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	chanMsgs := waitFor("libera", "#go", 2)
	if chanMsgs[0].Command != "JOIN" || chanMsgs[1].Command != "PRIVMSG" {
		t.Fatalf("#go commands: %s, %s", chanMsgs[0].Command, chanMsgs[1].Command)
	}
	if chanMsgs[1].MsgID != "m1" {
		t.Fatalf("msgid = %q, want m1", chanMsgs[1].MsgID)
	}
	query := waitFor("libera", "bob", 1)
	if query[0].Sender != "bob" {
		t.Fatalf("query sender = %q", query[0].Sender)
	}

	// The dropped lines must not have created buffers.
	if msgs, _ := st.Latest(context.Background(), "libera", "irc.test", 10); len(msgs) != 0 {
		t.Fatalf("numeric was persisted: %v", msgs)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hub did not stop on cancel")
	}
}
