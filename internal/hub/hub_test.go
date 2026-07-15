package hub

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ircthing/internal/irc"
	"ircthing/internal/store"

	ircv4 "gopkg.in/irc.v4"
)

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
		{"nickserv notice files under nickserv", ":NickServ!s@services NOTICE AlteredParadox :identify pls", "AlteredParadox", "NickServ", true},
		{"server notice files under server name", ":irc.test NOTICE AlteredParadox :*** Looking up your hostname", "AlteredParadox", "irc.test", true},
		{"privmsg to someone else dropped", ":alice!u@h PRIVMSG bob :hi", "AlteredParadox", "", false},
		{"privmsg to us with no prefix dropped", "PRIVMSG AlteredParadox :hi", "AlteredParadox", "", false},
		{"pm before nick known dropped", ":alice!u@h PRIVMSG AlteredParadox :hi", "", "", false},
		{"join", ":alice!u@h JOIN #go", "AlteredParadox", "#go", true},
		{"part", ":alice!u@h PART #go :bye", "AlteredParadox", "#go", true},
		{"topic", ":alice!u@h TOPIC #go :new topic", "AlteredParadox", "#go", true},
		{"kick", ":op!u@h KICK #go alice :out", "AlteredParadox", "#go", true},
		{"channel mode", ":op!u@h MODE #go +o alice", "AlteredParadox", "#go", true},
		{"user mode dropped", ":AlteredParadox MODE AlteredParadox :+i", "AlteredParadox", "", false},
		{"quit dropped for now", ":alice!u@h QUIT :bye", "AlteredParadox", "", false},
		{"nick change dropped for now", ":alice!u@h NICK alicia", "AlteredParadox", "", false},
		{"numeric dropped", ":irc.test 001 AlteredParadox :Welcome", "AlteredParadox", "", false},
		{"ping dropped", "PING :x", "AlteredParadox", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ircv4.ParseMessage(tc.line)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.line, err)
			}
			got, ok := persistTarget(m, tc.ourNick)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("persistTarget = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
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

	mu      sync.Mutex
	sent    []*ircv4.Message
	sendErr error
}

func (f *fakeConn) Events() <-chan irc.Event { return f.ch }
func (f *fakeConn) Name() string             { return f.name }
func (f *fakeConn) Nick() string             { return f.nick }

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
