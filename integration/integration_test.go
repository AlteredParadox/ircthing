//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ircthing/internal/hub"
	"ircthing/internal/irc"
	"ircthing/internal/store"

	ircv4 "gopkg.in/irc.v4"
)

// TestConnectJoinMessage: the CLAUDE.md "connect, join" scenario — our
// stack connects, autojoins, exchanges messages with another user, and
// everything lands in the store and fans out to sessions.
func TestConnectJoinMessage(t *testing.T) {
	addr := startErgo(t)
	st, h := newStoreAndHub(t)
	s := startStack(t, st, h, irc.Config{
		Name: "ergo", Addr: addr, Nick: "webuser", Channels: []string{"#it"},
	})
	s.waitRegistered()
	s.waitJoined("webuser", "#it")

	buddy := dialRaw(t, addr, "buddy")
	buddy.send("JOIN #it")
	buddy.waitFor(func(m *ircv4.Message) bool { return m.Command == "JOIN" })
	// Wait until we see buddy's join before buddy speaks — otherwise the
	// message can race our own JOIN completing.
	s.waitEnvelope("event", func(d json.RawMessage) bool {
		var ev hub.EventData
		return json.Unmarshal(d, &ev) == nil && ev.Command == "JOIN" && ev.Sender == "buddy"
	})
	buddy.send("PRIVMSG #it :hello from buddy")

	// The message reaches sessions live and is persisted with ergo's
	// msgid + server-time.
	s.waitEnvelope("event", func(d json.RawMessage) bool {
		var ev hub.EventData
		return json.Unmarshal(d, &ev) == nil && strings.Contains(ev.Raw, "hello from buddy")
	})
	msgs := s.waitStored("ergo", "#it", func(m []store.Message) bool {
		return countContaining(m, "hello from buddy") == 1
	})
	for _, m := range msgs {
		if strings.Contains(m.Raw, "hello from buddy") && m.MsgID == "" {
			t.Fatalf("no msgid on persisted message: %q", m.Raw)
		}
	}

	// Sending from a hub session reaches buddy and persists exactly once
	// (echo-message: ergo reflects it; no local double).
	s.sess.Handle(context.Background(), envelope(t, "send", 1, hub.SendData{
		Network: "ergo", Target: "#it", Text: "hi buddy",
	}))
	buddy.waitFor(func(m *ircv4.Message) bool {
		return m.Command == "PRIVMSG" && strings.Contains(m.Trailing(), "hi buddy")
	})
	s.waitStored("ergo", "#it", func(m []store.Message) bool {
		return countContaining(m, "hi buddy") == 1
	})
	time.Sleep(300 * time.Millisecond) // a duplicate would need a beat to appear
	final := s.waitStored("ergo", "#it", func([]store.Message) bool { return true })
	if n := countContaining(final, "hi buddy"); n != 1 {
		t.Fatalf("own message persisted %d times", n)
	}
}

// TestSASL: register an account with services, then authenticate with
// SASL PLAIN. Registration completing at all proves SASL succeeded — the
// handshake fails closed when SASL is configured but not completed.
func TestSASL(t *testing.T) {
	addr := startErgo(t)

	reg := dialRaw(t, addr, "sasluser")
	reg.send("PRIVMSG NickServ :REGISTER sekrit-pw")
	reg.waitFor(func(m *ircv4.Message) bool { // ergo: "Account created"
		return m.Command == "NOTICE" && strings.Contains(strings.ToLower(m.Trailing()), "created")
	})
	reg.send("QUIT :done")

	st, h := newStoreAndHub(t)
	s := startStack(t, st, h, irc.Config{
		Name: "ergo", Addr: addr, Nick: "sasluser",
		SASL: &irc.SASLPlain{Login: "sasluser", Password: "sekrit-pw"},
	})
	s.waitRegistered()
}

// TestReadMarkerMultiDevice: two ircthing instances ("devices") logged
// into the same account; reading on one device moves the marker on the
// other via draft/read-marker.
func TestReadMarkerMultiDevice(t *testing.T) {
	addr := startErgo(t)

	reg := dialRaw(t, addr, "shared")
	reg.send("PRIVMSG NickServ :REGISTER sekrit-pw")
	reg.waitFor(func(m *ircv4.Message) bool {
		return m.Command == "NOTICE" && strings.Contains(strings.ToLower(m.Trailing()), "created")
	})
	reg.send("QUIT :done")

	sasl := &irc.SASLPlain{Login: "shared", Password: "sekrit-pw"}
	stA, hA := newStoreAndHub(t)
	devA := startStack(t, stA, hA, irc.Config{
		Name: "ergo", Addr: addr, Nick: "shared", SASL: sasl, Channels: []string{"#rm"},
	})
	devA.waitRegistered()
	stB, hB := newStoreAndHub(t)
	devB := startStack(t, stB, hB, irc.Config{
		Name: "ergo", Addr: addr, Nick: "shared", SASL: sasl, Channels: []string{"#rm"},
	})
	devB.waitRegistered()

	// Device A marks #rm read; device B's session learns the position.
	markTime := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	devA.sess.Handle(context.Background(), envelope(t, "set_read_marker", 1, hub.SetMarkerData{
		Network: "ergo", Buffer: "#rm", Time: markTime.UnixMilli(),
	}))
	got := devB.waitEnvelope("read_marker", func(d json.RawMessage) bool {
		var md hub.MarkerData
		return json.Unmarshal(d, &md) == nil && md.Buffer == "#rm" && md.Time == markTime.UnixMilli()
	})
	_ = got

	// And device B's own store agrees.
	deadline := time.Now().Add(testTimeout)
	for {
		m, err := stB.ReadMarker(context.Background(), "ergo", "#rm")
		if err != nil {
			t.Fatal(err)
		}
		if m.UnixMilli() == markTime.UnixMilli() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("device B marker = %v, want %v", m, markTime)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestChathistoryReconnectReplay: the CLAUDE.md "chathistory,
// reconnect-replay" scenario — messages sent while we are offline are
// backfilled into the store on reconnect, without duplicating what we
// already had.
func TestChathistoryReconnectReplay(t *testing.T) {
	addr := startErgo(t)
	st, h := newStoreAndHub(t)

	s1 := startStack(t, st, h, irc.Config{
		Name: "ergo", Addr: addr, Nick: "webuser", Channels: []string{"#hist"},
	})
	s1.waitRegistered()
	s1.waitJoined("webuser", "#hist")

	buddy := dialRaw(t, addr, "buddy")
	buddy.send("JOIN #hist")
	s1.waitEnvelope("event", func(d json.RawMessage) bool {
		var ev hub.EventData
		return json.Unmarshal(d, &ev) == nil && ev.Command == "JOIN" && ev.Sender == "buddy"
	})
	buddy.send("PRIVMSG #hist :m1 before disconnect")
	s1.waitStored("ergo", "#hist", func(m []store.Message) bool {
		return countContaining(m, "m1 before disconnect") == 1
	})
	s1.stop()

	// While we are gone, history accumulates server-side.
	buddy.send("PRIVMSG #hist :m2 while offline")
	buddy.send("PRIVMSG #hist :m3 while offline")
	time.Sleep(200 * time.Millisecond)

	// Reconnect: JOIN triggers CHATHISTORY AFTER from the newest stored
	// timestamp, and the replay fills the gap.
	s2 := startStack(t, st, h, irc.Config{
		Name: "ergo", Addr: addr, Nick: "webuser", Channels: []string{"#hist"},
	})
	s2.waitRegistered()
	msgs := s2.waitStored("ergo", "#hist", func(m []store.Message) bool {
		return countContaining(m, "m2 while offline") == 1 && countContaining(m, "m3 while offline") == 1
	})

	// No duplicates, and chronological order survived.
	if n := countContaining(msgs, "m1 before disconnect"); n != 1 {
		t.Fatalf("m1 stored %d times", n)
	}
	idx := func(sub string) int {
		for i, m := range msgs {
			if strings.Contains(m.Raw, sub) {
				return i
			}
		}
		return -1
	}
	if !(idx("m1 before") < idx("m2 while") && idx("m2 while") < idx("m3 while")) {
		t.Fatalf("order wrong: %v", rawsOf(msgs))
	}

	// Idempotence under a second reconnect: the same window replays and
	// deduplicates by msgid.
	s2.stop()
	s3 := startStack(t, st, h, irc.Config{
		Name: "ergo", Addr: addr, Nick: "webuser", Channels: []string{"#hist"},
	})
	s3.waitRegistered()
	s3.waitStored("ergo", "#hist", func(m []store.Message) bool {
		// Four JOINs: s1, buddy, s2, s3 — proves s3's flow completed.
		return countContaining(m, "JOIN #hist") >= 4
	})
	final := s3.waitStored("ergo", "#hist", func([]store.Message) bool { return true })
	for _, sub := range []string{"m1 before", "m2 while", "m3 while"} {
		if n := countContaining(final, sub); n != 1 {
			t.Fatalf("%q stored %d times after re-replay", sub, n)
		}
	}
}
