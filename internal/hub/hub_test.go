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
	"path/filepath"
	"sort"
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
	feed(":bob!u@h JOIN #other")         // someone else -> ignored
	if ch := channels(); len(ch) != 1 || ch[0] != "#go" {
		t.Fatalf("dup/other JOIN changed channels: %v", ch)
	}
	feed(":AlteredParadox!u@h PART #go")
	if ch := channels(); len(ch) != 0 {
		t.Fatalf("after PART: channels = %v, want []", ch)
	}

	// A combined self-JOIN then a combined self-PART (comma-lists) must add and
	// then clear EVERY listed channel — not the literal "#a,#b" string.
	feed(":AlteredParadox!u@h JOIN #a,#b,#c")
	if ch := channels(); len(ch) != 3 {
		t.Fatalf("after multi-JOIN: channels = %v, want 3", ch)
	}
	feed(":AlteredParadox!u@h PART #a,#c")
	if ch := channels(); len(ch) != 1 || ch[0] != "#b" {
		t.Fatalf("after multi-PART: channels = %v, want [#b]", ch)
	}
	feed(":AlteredParadox!u@h PART #b")
	if ch := channels(); len(ch) != 0 {
		t.Fatalf("after final PART: channels = %v, want []", ch)
	}

	// An oversized channel name is NOT persisted (it would fail restart
	// validation); a framing byte likewise.
	feed(":AlteredParadox!u@h JOIN #" + strings.Repeat("x", maxPersistedChannelLen))
	if ch := channels(); len(ch) != 0 {
		t.Fatalf("oversized channel was persisted: %v", ch)
	}

	// ERR_LINKCHANNEL (470): the server refused #chat and forwarded us to
	// ##chat. The refused ORIGINAL must leave the autojoin list (join_channel
	// persisted it before the verdict), or every restart re-joins #chat,
	// follows the forward, and resurrects ##chat after the user left it. The
	// forward target itself arrives via its own JOIN echo.
	feed(":AlteredParadox!u@h JOIN #chat") // join_channel's optimistic persist
	feed(":irc.test 470 AlteredParadox #chat ##chat :Forwarding to another channel")
	if ch := channels(); len(ch) != 0 {
		t.Fatalf("after 470 forward: channels = %v, want []", ch)
	}
	feed(":AlteredParadox!u@h JOIN ##chat") // the forward target's echo
	if ch := channels(); len(ch) != 1 || ch[0] != "##chat" {
		t.Fatalf("after forwarded JOIN: channels = %v, want [##chat]", ch)
	}
	// A 470 addressed to someone else (or with folded-different casing of a
	// third party) must not touch our list.
	feed(":irc.test 470 bob ##chat #elsewhere :Forwarding to another channel")
	if ch := channels(); len(ch) != 1 || ch[0] != "##chat" {
		t.Fatalf("another user's 470 changed channels: %v", ch)
	}
	feed(":AlteredParadox!u@h PART ##chat")
	if ch := channels(); len(ch) != 0 {
		t.Fatalf("after PART ##chat: channels = %v, want []", ch)
	}
}

// End-to-end wiring for the 470 cleanup: an ERR_LINKCHANNEL flowing through
// the hub event loop must still reach persistAutojoin even though error
// numerics are consumed by the serverInfo branch (which returns before
// liveHints runs) — a placement regression here silently reintroduces the
// forwarded-channel resurrection bug.
func TestLinkChannelNumericPrunesAutojoin(t *testing.T) {
	h := newTestHub(t)
	ctx := context.Background()
	raw, err := json.Marshal(netconf.Network{
		Name: "libera", Addr: "irc.test:6697", Nick: "AlteredParadox",
		Channels: []string{"#chat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutNetworkConfig(ctx, "libera", string(raw)); err != nil {
		t.Fatal(err)
	}
	conn := &fakeConn{ch: make(chan irc.Event, 2), name: "libera", nick: "AlteredParadox"}
	rctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(rctx, conn)
	waitForNetwork(t, h, "libera")

	conn.ch <- irc.Event{
		Network: "libera", Kind: irc.EventMessage, Time: time.Now(),
		Msg: ircv4.MustParseMessage(":irc.test 470 AlteredParadox #chat ##chat :Forwarding to another channel"),
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ok, err := h.store.NetworkConfig(ctx, "libera")
		if err != nil || !ok {
			t.Fatalf("read config: ok=%v err=%v", ok, err)
		}
		var parsed netconf.Network
		if err := json.Unmarshal([]byte(got.Config), &parsed); err != nil {
			t.Fatal(err)
		}
		if len(parsed.Channels) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("channels = %v, want [] after 470", parsed.Channels)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReplayedNumericsHaveNoLiveSideEffects(t *testing.T) {
	h := newTestHub(t)
	ctx := context.Background()
	raw, _ := json.Marshal(netconf.Network{Name: "libera", Addr: "irc.test:6697", Nick: "AlteredParadox", Channels: []string{"#chat"}})
	if err := h.store.PutNetworkConfig(ctx, "libera", string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AddMonitor(ctx, "libera", "bob"); err != nil {
		t.Fatal(err)
	}
	conn := &fakeConn{ch: make(chan irc.Event, 8), name: "libera", nick: "AlteredParadox"}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go h.Run(rctx, conn)
	waitForNetwork(t, h, "libera")
	conn.ch <- irc.Event{Network: "libera", Kind: irc.EventState, State: irc.StateRegistered}
	feed := func(line string) {
		conn.ch <- irc.Event{Network: "libera", Kind: irc.EventMessage, Time: time.Now(), Msg: ircv4.MustParseMessage(line)}
	}
	feed(":srv BATCH +r chathistory #chat")
	feed("@batch=r :srv 470 AlteredParadox #chat ##chat :Forwarding")
	feed("@batch=r :srv 730 AlteredParadox :bob!u@h")
	feed("@batch=r :srv 734 AlteredParadox 1 bob :list full")
	feed("@batch=r :srv 376 AlteredParadox :End of MOTD")
	feed(":srv BATCH -r")
	time.Sleep(100 * time.Millisecond)
	nc, ok, err := h.store.NetworkConfig(ctx, "libera")
	if err != nil || !ok {
		t.Fatalf("config read: ok=%v err=%v", ok, err)
	}
	var parsed netconf.Network
	if err := json.Unmarshal([]byte(nc.Config), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Channels) != 1 || parsed.Channels[0] != "#chat" {
		t.Fatalf("replayed 470 changed autojoin: %v", parsed.Channels)
	}
	h.mu.Lock()
	presence := len(h.presence["libera"])
	h.mu.Unlock()
	if presence != 0 {
		t.Fatalf("replayed 730 changed presence: %d entries", presence)
	}
	conn.mu.Lock()
	rejected := len(conn.monRejected)
	reconciled := len(conn.monitored)
	conn.mu.Unlock()
	if rejected != 0 {
		t.Fatalf("replayed 734 reached manager: %d entries", rejected)
	}
	if reconciled != 0 {
		t.Fatalf("replayed 376 triggered registration sync: %d monitors", reconciled)
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
		name string
		line string
		// openQuery, when non-empty, is the sender nick that has an open query
		// buffer — the queryOpen predicate returns true for it (folded), false
		// for everyone else. Empty means no query is open.
		openQuery string
		ourNick   string
		want      string
		ok        bool
	}{
		{"channel privmsg", ":alice!u@h PRIVMSG #go :hi", "", "AlteredParadox", "#go", true},
		{"ampersand channel", ":alice!u@h PRIVMSG &local :hi", "", "AlteredParadox", "&local", true},
		{"channel notice", ":alice!u@h NOTICE #go :psst", "", "AlteredParadox", "#go", true},
		{"query files under sender", ":alice!u@h PRIVMSG AlteredParadox :hello", "", "AlteredParadox", "alice", true},
		{"query nick is case-insensitive", ":alice!u@h PRIVMSG ALTEREDPARADOX :hello", "", "AlteredParadox", "alice", true},
		{"nickserv notice files under the server buffer", ":NickServ!s@services NOTICE AlteredParadox :identify pls", "", "AlteredParadox", "*", true},
		// With an open query for the sender, the notice files there instead of
		// the lobby (bug fix: services/opped bots reply via NOTICE).
		{"nickserv notice with open query files under it", ":NickServ!s@services NOTICE AlteredParadox :identify pls", "NickServ", "AlteredParadox", "NickServ", true},
		{"open query match is case-insensitive", ":NickServ!s@services NOTICE AlteredParadox :hi", "nickserv", "AlteredParadox", "NickServ", true},
		{"opped bot notice with open query files under it", ":chat!bot@h NOTICE AlteredParadox :beep", "chat", "AlteredParadox", "chat", true},
		{"open query for a different nick does not redirect", ":NickServ!s@services NOTICE AlteredParadox :hi", "alice", "AlteredParadox", "*", true},
		{"server notice files under the server buffer", ":irc.test NOTICE AlteredParadox :*** Looking up your hostname", "", "AlteredParadox", "*", true},
		{"pre-registration server notice to * files under the server buffer", ":irc.test NOTICE * :*** hi", "", "", "*", true},
		{"user notice files under the server buffer", ":alice!u@h NOTICE AlteredParadox :ping", "", "AlteredParadox", "*", true},
		{"privmsg to someone else dropped", ":alice!u@h PRIVMSG bob :hi", "", "AlteredParadox", "", false},
		{"our echoed pm files under the recipient", ":AlteredParadox!u@h PRIVMSG alice :hi", "", "AlteredParadox", "alice", true},
		{"our echoed notice files under the recipient", ":AlteredParadox!u@h NOTICE alice :psst", "", "AlteredParadox", "alice", true},
		{"privmsg to us with no prefix dropped", "PRIVMSG AlteredParadox :hi", "", "AlteredParadox", "", false},
		{"pm before nick known dropped", ":alice!u@h PRIVMSG AlteredParadox :hi", "", "", "", false},
		{"join", ":alice!u@h JOIN #go", "", "AlteredParadox", "#go", true},
		{"part", ":alice!u@h PART #go :bye", "", "AlteredParadox", "#go", true},
		{"topic", ":alice!u@h TOPIC #go :new topic", "", "AlteredParadox", "#go", true},
		{"kick", ":op!u@h KICK #go alice :out", "", "AlteredParadox", "#go", true},
		{"channel mode", ":op!u@h MODE #go +o alice", "", "AlteredParadox", "#go", true},
		{"user mode dropped", ":AlteredParadox MODE AlteredParadox :+i", "", "AlteredParadox", "", false},
		// QUIT/NICK never resolve to a single target here — the hub's
		// persistMembership fans them out per shared channel instead.
		{"quit has no single target", ":alice!u@h QUIT :bye", "", "AlteredParadox", "", false},
		{"nick change has no single target", ":alice!u@h NICK alicia", "", "AlteredParadox", "", false},
		{"numeric dropped", ":irc.test 001 AlteredParadox :Welcome", "", "AlteredParadox", "", false},
		{"ping dropped", "PING :x", "", "AlteredParadox", "", false},
		{"ctcp query dropped", ":alice!u@h PRIVMSG AlteredParadox :\x01VERSION\x01", "", "AlteredParadox", "", false},
		{"ctcp reply notice dropped", ":alice!u@h NOTICE AlteredParadox :\x01VERSION theirclient\x01", "", "AlteredParadox", "", false},
		{"ctcp action persists", ":alice!u@h PRIVMSG #go :\x01ACTION waves\x01", "", "AlteredParadox", "#go", true},
		{"statusmsg op-only files under bare channel", ":op!u@h PRIVMSG @#go :ops only", "", "AlteredParadox", "#go", true},
		{"statusmsg voice-only files under bare channel", ":op!u@h PRIVMSG +#go :voiced only", "", "AlteredParadox", "#go", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ircv4.ParseMessage(tc.line)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.line, err)
			}
			// queryOpen is true only for tc.openQuery (folded), modeling one
			// open query buffer with that sender.
			queryOpen := func(nick string) bool {
				return tc.openQuery != "" && foldRFC1459(nick) == foldRFC1459(tc.openQuery)
			}
			got, ok := persistTarget(m, tc.ourNick, defaultIsChannel, foldRFC1459, "~&@%+", queryOpen)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("persistTarget = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// replayTarget must file a REPLAYED private NOTICE under the batch target,
// exactly like PRIVMSG: a chathistory TARGET query replays only messages
// belonging to that target's conversation, so every non-channel message in a
// query batch belongs to that query — regardless of the sender prefix or our
// CURRENT nick. Rerouting any of them to "*" duplicates the live copy
// (per-buffer msgid dedup) as a misfiled lobby row + phantom unread.
func TestReplayTargetNoticeRouting(t *testing.T) {
	c := &fakeConn{nick: "AlteredParadox"}
	cases := []struct {
		name        string
		line        string
		batchTarget string
		want        string
		ok          bool
	}{
		// The query we are backfilling is the sender's: file the notice there.
		{"incoming private notice -> the query", ":alice!u@h NOTICE AlteredParadox :ping", "alice", "alice", true},
		{"incoming private notice, case-variant batch -> the query", ":Alice!u@h NOTICE AlteredParadox :ping", "alice", "alice", true},
		// The correspondent renamed between the notice and the replay: the
		// batch target still names the conversation being backfilled, so the
		// notice stays in that query. (An earlier revision expected "*" here,
		// keying on fold(sender)==fold(batch target) — but the server only
		// replays messages belonging to the requested target's history, so a
		// mismatched prefix never means "different conversation"; rerouting
		// misfiled it and, msgid dedup being per-buffer, duplicated it.)
		{"incoming notice, sender renamed since -> still the query", ":alice2!u@h NOTICE AlteredParadox :ping", "alice", "alice", true},
		// Our own notice replayed under the OLD nick after a rename: still the
		// batch target — replay routing is independent of the current nick
		// (see persistBuffer's contract).
		{"own notice under old nick after rename -> the query", ":OldNick!u@h NOTICE bob :psst", "bob", "bob", true},
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

// End-to-end wiring for the notice-redirect fix: an incoming private NOTICE
// files in an OPEN query with the sender (a service or an opped bot replying
// via NOTICE), and in the lobby "*" when no such query exists. Exercises the
// queryOpen predicate (store.FindBuffer) that persistEvent builds.
func TestNoticeRoutingWithOpenQuery(t *testing.T) {
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
	// A PRIVMSG from the bot opens its query; the bot's NOTICE reply must then
	// land in that query, not the lobby. NickServ has no open query, so its
	// notice stays in "*".
	conn.ch <- ev(":chat!bot@h PRIVMSG AlteredParadox :hi there")
	conn.ch <- ev(":chat!bot@h NOTICE AlteredParadox :beep boop")
	conn.ch <- ev(":NickServ!s@services NOTICE AlteredParadox :please identify")

	deadline := time.Now().Add(5 * time.Second)
	for {
		chatMsgs, err := h.store.Latest(ctxb, "libera", "chat", 10)
		if err != nil {
			t.Fatal(err)
		}
		starMsgs, err := h.store.Latest(ctxb, "libera", serverBufferTarget, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(chatMsgs) == 2 && len(starMsgs) == 1 {
			// The bot's notice is in its query; NickServ's is in the lobby.
			if starMsgs[0].Sender != "NickServ" {
				t.Fatalf("lobby message sender = %q, want NickServ", starMsgs[0].Sender)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("routing: chat=%d (want 2), *=%d (want 1)", len(chatMsgs), len(starMsgs))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The user's exact regression: message ChanServ (PM opens), close the PM
// (archive-on-close), then receive its on-join NOTICEs. An ARCHIVED query
// is CLOSED for the notice-redirect heuristic — the notices must file in
// the lobby and the PM must stay archived, not resurface on every join.
func TestNoticeRoutingSkipsArchivedQuery(t *testing.T) {
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
	// Open the ChanServ query, then archive it (close_buffer purge:false).
	conn.ch <- ev(":ChanServ!s@services PRIVMSG AlteredParadox :done")
	waitFor(t, func() bool {
		msgs, err := h.store.Latest(ctxb, "libera", "ChanServ", 10)
		return err == nil && len(msgs) == 1
	})
	if _, err := h.store.ArchiveBufferFolded(ctxb, "libera", "ChanServ", foldRFC1459); err != nil {
		t.Fatal(err)
	}

	// The on-join services NOTICE: lobby, not the archived query.
	conn.ch <- ev(":ChanServ!s@services NOTICE AlteredParadox :you are now voiced in #chan")
	waitFor(t, func() bool {
		star, err := h.store.Latest(ctxb, "libera", serverBufferTarget, 10)
		return err == nil && len(star) == 1 && star[0].Sender == "ChanServ"
	})
	// The query kept only its original message and stayed archived.
	msgs, err := h.store.Latest(ctxb, "libera", "ChanServ", 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("archived query messages = %d (%v), want 1", len(msgs), err)
	}
	if _, archived, err := h.store.BufferState(ctxb, "libera", "ChanServ"); err != nil || !archived {
		t.Fatalf("BufferState archived = %v (%v), want true", archived, err)
	}

	// A direct PRIVMSG still resurfaces the archived PM — only the
	// notice-redirect heuristic is gated.
	conn.ch <- ev(":ChanServ!s@services PRIVMSG AlteredParadox :direct reply")
	waitFor(t, func() bool {
		_, archived, err := h.store.BufferState(ctxb, "libera", "ChanServ")
		return err == nil && !archived
	})
}

// NOTICE analog of TestReplayPMRoutesByBatchTargetAfterNickChange: replayed
// query NOTICEs must file under the batch target independently of our CURRENT
// nick. Repro this pins: we sent "/notice bob psst" as AlteredParadox (echoed
// live with msgid n1, stored in "bob"), the connection dropped, and reconnect
// fell back to AlteredParadox_. Backfilling "bob" replays the notice under the
// OLD nick; a current-nick own-check saw a foreign sender and rerouted it to
// "*" — and msgid dedup being per-buffer, the live copy in "bob" could not
// suppress that insert: a duplicate, misfiled lobby row + phantom unread.
func TestReplayNoticeRoutesByBatchTargetAfterNickChange(t *testing.T) {
	h := newTestHub(t)
	// Already renamed: the replayed history is from when we were "AlteredParadox".
	conn := &fakeConn{
		ch: make(chan irc.Event, 16), name: "libera", nick: "AlteredParadox_",
		caps: map[string]bool{"echo-message": true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx, conn)
	waitForNetwork(t, h, "libera")
	ctxb := context.Background()

	// The live echo of our notice, stored in "bob" before the rename.
	if _, err := h.store.Append(ctxb, "libera", "bob", store.Message{
		Time:    time.Date(2026, 7, 15, 0, 0, 6, 0, time.UTC),
		MsgID:   "n1",
		Sender:  "AlteredParadox",
		Command: "NOTICE",
		Raw:     ":AlteredParadox!u@h NOTICE bob :psst",
		Text:    "psst",
	}); err != nil {
		t.Fatal(err)
	}

	ev := func(line string) irc.Event {
		return irc.Event{Network: "libera", Kind: irc.EventMessage, Msg: ircv4.MustParseMessage(line), Time: time.Now()}
	}
	conn.ch <- ev(":srv BATCH +r1 chathistory bob")
	// Our own notice under the OLD nick (must dedup against the live copy in
	// "bob", not misfile into "*"), and bob's reply addressed to the OLD nick.
	conn.ch <- ev("@batch=r1;msgid=n1;time=2026-07-15T00:00:06.000Z :AlteredParadox!u@h NOTICE bob :psst")
	conn.ch <- ev("@batch=r1;msgid=n2;time=2026-07-15T00:00:07.000Z :bob!u@h NOTICE AlteredParadox :ack")
	conn.ch <- ev(":srv BATCH -r1")

	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs, err := h.store.Latest(ctxb, "libera", "bob", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) >= 2 {
			if len(msgs) != 2 {
				t.Fatalf("own notice duplicated in bob: %d rows: %+v", len(msgs), msgs)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replayed notice history not filed under bob (got %d)", len(msgs))
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Nothing leaked into the lobby or a phantom nick buffer.
	for _, phantom := range []string{serverBufferTarget, "AlteredParadox", "AlteredParadox_"} {
		if m, _ := h.store.Latest(ctxb, "libera", phantom, 10); len(m) != 0 {
			t.Fatalf("notice history misfiled under %q: %+v", phantom, m)
		}
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

	mu          sync.Mutex
	sent        []*ircv4.Message
	sendErr     error
	hist        []string // RequestChatHistory calls as "target@sinceMs"
	names       []string // EnsureNames calls
	multiline   []string // SendMultiline calls
	monitored   []string // last ReconcileMonitored desired list
	monRejected []string // MonitorRejected
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

// ChannelPage mirrors the production roster's paging contract: members in
// folded-key order, strictly after the cursor, at most limit per call.
func (f *fakeConn) ChannelPage(name string, limit int, after string) (string, []irc.Member, bool, bool) {
	all, ok := f.chans[name]
	ms := make([]irc.Member, len(all))
	copy(ms, all)
	sort.Slice(ms, func(i, j int) bool { return f.Fold(ms[i].Nick) < f.Fold(ms[j].Nick) })
	for len(ms) > 0 && after != "" && f.Fold(ms[0].Nick) <= after {
		ms = ms[1:]
	}
	truncated := len(ms) > limit
	if truncated {
		ms = ms[:limit]
	}
	return f.topic, ms, truncated, ok
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

func (f *fakeConn) SendAll(msgs []*ircv4.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr // atomic: nothing appended on failure
	}
	f.sent = append(f.sent, msgs...)
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

func (f *fakeConn) ReconcileMonitored(desired []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.monitored = append([]string(nil), desired...)
	return nil
}

func (f *fakeConn) MonitorRejected(nicks []string, _ int, _ uint64, desired []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.monRejected = append(f.monRejected, nicks...)
	// Record the desired snapshot passed to the real manager's rebuild.
	f.monitored = append([]string(nil), desired...)
	return nil
}

func (f *fakeConn) monitoredNicks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.monitored...)
}

func (f *fakeConn) rejectedNicks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.monRejected...)
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
		"PING :x", // must be dropped
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
