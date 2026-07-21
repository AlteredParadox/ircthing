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

package irc

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	ircv4 "gopkg.in/irc.v4"
)

// feed parses and applies lines as the user "AlteredParadox", routing 005 through
// the isupport tracker the same way the manager's read loop does.
func feed(t *testing.T, r *roster, lines ...string) {
	t.Helper()
	for _, l := range lines {
		m := ircv4.MustParseMessage(l)
		r.isup.handle(m)
		r.handle("AlteredParadox", m)
	}
}

func testRoster() *roster {
	return newRoster(newISupport())
}

func members(t *testing.T, r *roster, ch string) []Member {
	t.Helper()
	_, ms, _ := r.channel(ch)
	// User/Host (from userhost-in-names / JOIN / CHGHOST) are asserted
	// separately in TestRosterUserHost; zero them here so the many
	// prefix/account/away/mode tables stay focused on their subject. channel()
	// returns fresh Member copies, so this doesn't touch roster state.
	for i := range ms {
		ms[i].User, ms[i].Host = "", ""
	}
	return ms
}

// TestRosterUserHost checks that ident/host are captured from the JOIN prefix,
// userhost-in-names 353 entries, and CHGHOST, and surface on channel().
func TestRosterUserHost(t *testing.T) {
	r := testRoster()
	feed(t, r,
		joinGo, // :AlteredParadox!u@h JOIN #go
		":srv 353 AlteredParadox = #go :@alice!auser@ahost bob!buser@bhost AlteredParadox",
		":srv 366 AlteredParadox #go :end",
		":bob!buser@bhost CHGHOST newuser newhost",
	)
	_, ms, ok := r.channel("#go")
	if !ok {
		t.Fatal("channel #go not found")
	}
	got := map[string]string{}
	for _, m := range ms {
		got[m.Nick] = m.User + "@" + m.Host
	}
	want := map[string]string{
		"AlteredParadox": "u@h",             // from the JOIN prefix
		"alice":          "auser@ahost",     // from userhost-in-names 353 (prefix stripped)
		"bob":            "newuser@newhost", // CHGHOST overrides the 353 user@host
	}
	for nick, uh := range want {
		if got[nick] != uh {
			t.Errorf("%s user@host = %q, want %q", nick, got[nick], uh)
		}
	}
}

func TestRosterChannelPageReturnsSortedBoundedPrefix(t *testing.T) {
	r := testRoster()
	feed(t, r,
		joinGo,
		":srv 353 AlteredParadox = #go :n4 n1 n3 AlteredParadox n0 n2",
		":srv 366 AlteredParadox #go :end",
	)
	topic, got, truncated, ok := r.channelPage("#go", 3, "")
	if !ok || topic != "" || !truncated {
		t.Fatalf("page metadata = ok=%v topic=%q truncated=%v", ok, topic, truncated)
	}
	want := []string{"AlteredParadox", "n0", "n1"}
	if len(got) != len(want) {
		t.Fatalf("members = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Nick != want[i] {
			t.Fatalf("member %d = %q, want %q", i, got[i].Nick, want[i])
		}
	}
}

// TestRosterChannelPageCursorWalk pages through a large channel with the
// `after` cursor and checks the walk yields every member exactly once, in
// folded-key order, across page-size boundaries (strictly-after: no
// duplicate or skip at page joins).
func TestRosterChannelPageCursorWalk(t *testing.T) {
	r := testRoster()
	feed(t, r, joinGo)
	// 1,000 members via NAMES bursts (plus ourselves), under the roster bounds.
	lines := []string{":srv 353 AlteredParadox = #go :AlteredParadox"}
	for i := 0; i < 1000; i += 100 {
		nicks := make([]string, 0, 100)
		for j := i; j < i+100; j++ {
			nicks = append(nicks, fmt.Sprintf("User%04d", j))
		}
		lines = append(lines, ":srv 353 AlteredParadox = #go :"+strings.Join(nicks, " "))
	}
	lines = append(lines, ":srv 366 AlteredParadox #go :end")
	feed(t, r, lines...)

	var walked []string
	after := ""
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("cursor walk did not terminate")
		}
		_, page, truncated, ok := r.channelPage("#go", 37, after)
		if !ok {
			t.Fatal("channel lost mid-walk")
		}
		for _, m := range page {
			walked = append(walked, m.Nick)
		}
		if !truncated {
			break
		}
		if len(page) == 0 {
			t.Fatal("truncated page with no members")
		}
		after = r.isup.Fold(page[len(page)-1].Nick)
	}
	if len(walked) != 1001 { // 1,000 + ourselves
		t.Fatalf("walked %d members, want 1001", len(walked))
	}
	seen := map[string]bool{}
	for i, nick := range walked {
		if seen[nick] {
			t.Fatalf("duplicate member %q at position %d", nick, i)
		}
		seen[nick] = true
		if i > 0 && r.isup.Fold(walked[i-1]) >= r.isup.Fold(nick) {
			t.Fatalf("out of order at %d: %q then %q", i, walked[i-1], nick)
		}
	}
	for j := 0; j < 1000; j++ {
		if nick := fmt.Sprintf("User%04d", j); !seen[nick] {
			t.Fatalf("member %q missing from walk", nick)
		}
	}
}

// A cursor that matches no stored key still means "strictly after": the walk
// resumes at the next key without duplicating or skipping, and an empty
// cursor is the first page.
func TestRosterChannelPageCursorBoundaries(t *testing.T) {
	r := testRoster()
	feed(t, r,
		joinGo,
		":srv 353 AlteredParadox = #go :AlteredParadox n1 n3 n5",
		":srv 366 AlteredParadox #go :end",
	)
	nicks := func(ms []Member) []string {
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = m.Nick
		}
		return out
	}
	// Empty cursor = first page (identical to the pre-cursor behavior).
	_, first, _, _ := r.channelPage("#go", 2, "")
	if want := []string{"alteredparadox", "n1"}; !reflect.DeepEqual(nicks(first), []string{"AlteredParadox", "n1"}) {
		t.Fatalf("first page = %v, want AlteredParadox,n1 (fold order %v)", nicks(first), want)
	}
	// Between-keys cursor: "n2" is not a member; the page starts at n3.
	_, page, truncated, _ := r.channelPage("#go", 10, "n2")
	if !reflect.DeepEqual(nicks(page), []string{"n3", "n5"}) || truncated {
		t.Fatalf("after n2 = %v truncated=%v, want [n3 n5] false", nicks(page), truncated)
	}
	// Cursor past every key: empty final page, not truncated.
	_, page, truncated, _ = r.channelPage("#go", 10, "zzz")
	if len(page) != 0 || truncated {
		t.Fatalf("after zzz = %v truncated=%v, want empty false", nicks(page), truncated)
	}
}

// Membership churn between pages must not panic, duplicate within a page, or
// derail the cursor. Cross-page consistency under churn is best-effort by
// design (see channelPage): a member joining behind the cursor is missed
// until the next refresh — members_changed triggers that refetch.
func TestRosterChannelPageCursorChurn(t *testing.T) {
	r := testRoster()
	feed(t, r,
		joinGo,
		":srv 353 AlteredParadox = #go :AlteredParadox n1 n2 n3 n4 n5 n6",
		":srv 366 AlteredParadox #go :end",
	)
	_, page1, truncated, _ := r.channelPage("#go", 3, "")
	if !truncated || len(page1) != 3 {
		t.Fatalf("page1 = %d members truncated=%v", len(page1), truncated)
	}
	after := r.isup.Fold(page1[len(page1)-1].Nick) // "n2"
	// Churn: a page-1 member and a not-yet-walked member leave; a new member
	// joins behind the cursor (missed — acceptable) and one ahead of it.
	feed(t, r,
		":n1!u@h PART #go",
		":n4!u@h QUIT :bye",
		":a0!u@h JOIN #go", // behind the cursor: missed by this walk
		":n45!u@h JOIN #go",
	)
	_, page2, _, ok := r.channelPage("#go", 10, after)
	if !ok {
		t.Fatal("channel lost after churn")
	}
	seen := map[string]bool{}
	for _, m := range page2 {
		if seen[m.Nick] {
			t.Fatalf("duplicate %q within a page", m.Nick)
		}
		seen[m.Nick] = true
		if key := r.isup.Fold(m.Nick); key <= after {
			t.Fatalf("member %q at key %q not strictly after cursor %q", m.Nick, key, after)
		}
	}
	want := []string{"n3", "n45", "n5", "n6"}
	for _, nick := range want {
		if !seen[nick] {
			t.Fatalf("member %q missing after churn: page2 = %v", nick, page2)
		}
	}
	if len(page2) != len(want) {
		t.Fatalf("page2 has %d members, want %d (%v)", len(page2), len(want), page2)
	}
}

// join sets up our own membership of #go.
var joinGo = ":AlteredParadox!u@h JOIN #go"

// The connection-wide member budget bounds total roster memory even when a
// hostile server fills many channels — a new member is refused once the
// aggregate is spent, regardless of per-channel room.
func TestRosterAggregateBudget(t *testing.T) {
	defer func(ch, ag int) { maxChannelMembers, maxRosterMembers = ch, ag }(maxChannelMembers, maxRosterMembers)
	maxChannelMembers = 10 // large, so the aggregate budget is the binding limit
	maxRosterMembers = 3

	r := testRoster()
	feed(t, r, ":AlteredParadox!u@h JOIN #a")        // our own join: 1 member
	feed(t, r, ":u1!u@h JOIN #a", ":u2!u@h JOIN #a") // -> 3 members (aggregate cap)
	if got := r.totalMembers(); got != 3 {
		t.Fatalf("totalMembers = %d, want 3", got)
	}
	// Further members are refused once the budget is spent — in this channel
	// (room under its per-channel cap) and in a new one.
	feed(t, r, ":u3!u@h JOIN #a")
	feed(t, r, ":AlteredParadox!u@h JOIN #b", ":x!u@h JOIN #b")
	if got := r.totalMembers(); got > maxRosterMembers {
		t.Fatalf("totalMembers = %d, want <= %d (budget must hold)", got, maxRosterMembers)
	}
	if n := len(members(t, r, "#a")); n != 3 {
		t.Fatalf("#a has %d members, want 3", n)
	}
}

// One hostile near-64-KiB modestring must not be processed letter by letter:
// each status letter used to cost an O(channels) budget scan under the roster
// lock (~268M map visits at the 4,096-channel cap). Letters past the cap are
// dropped; a normal-sized MODE line still applies fully.
func TestRosterModeLetterCap(t *testing.T) {
	r := testRoster()
	feed(t, r,
		joinGo,
		":srv 353 AlteredParadox = #go :alice bob",
		":srv 366 AlteredParadox #go :end",
	)
	// A hostile line: cap+ letters of a status mode aimed at one nick.
	// Only maxModeLettersPerLine letters are examined, so this returns
	// quickly; the grants inside the cap still apply.
	feed(t, r, ":srv MODE #go +"+strings.Repeat("o", maxModeLettersPerLine+1000)+" alice")
	for _, m := range members(t, r, "#go") {
		if m.Nick == "alice" && m.Prefix != "@" {
			t.Fatalf("alice prefix = %q, want @ (in-cap grant must apply)", m.Prefix)
		}
	}
	// Grants past the cap are ignored.
	modes := strings.Repeat("i", maxModeLettersPerLine) + "o" // 'i' is type D: no argument
	feed(t, r, ":srv MODE #go +"+modes+" bob")
	for _, m := range members(t, r, "#go") {
		if m.Nick == "bob" && m.Prefix != "" {
			t.Fatalf("bob prefix = %q, want none (past-cap letters must be dropped)", m.Prefix)
		}
	}
}

// The BYTE budget must bind independently of the count caps: clamped fields
// are up to maxRosterField each, so counts alone would admit ~20x the memory
// the ~150 B/member estimate suggests. Also checks the running per-channel
// byte accounting stays exact across joins, NAMES swaps, updates, and
// departures.
func TestRosterByteBudget(t *testing.T) {
	defer func(b int) { maxRosterBytes = b }(maxRosterBytes)
	maxRosterBytes = 2 * memberOverhead // room for ~2 small members

	r := testRoster()
	feed(t, r, ":AlteredParadox!u@h JOIN #a", ":u1!u@h JOIN #a")
	if got := r.totalBytes(); got > maxRosterBytes+2*memberOverhead {
		t.Fatalf("totalBytes = %d, budget %d wildly exceeded", got, maxRosterBytes)
	}
	// Over budget now: further members are refused.
	feed(t, r, ":u2!u@h JOIN #a", ":u3!u@h JOIN #a")
	if n := len(members(t, r, "#a")); n != 2 {
		t.Fatalf("#a has %d members, want 2 (byte budget must bind)", n)
	}
	// Over budget, growing field updates are refused; shrinking ones apply.
	feed(t, r, ":u1!u@h ACCOUNT bigaccountname")
	for _, m := range members(t, r, "#a") {
		if m.Account != "" {
			t.Fatalf("growing ACCOUNT applied over budget: %+v", m)
		}
	}
	// Over budget, a NICK to a much longer name must NOT inflate the entry
	// (same guard as field updates) — else NICK is an unbounded growth path.
	longNick := strings.Repeat("z", 300)
	feed(t, r, ":u1!u@h NICK "+longNick)
	for _, m := range members(t, r, "#a") {
		if len(m.Nick) > 50 {
			t.Fatalf("growing NICK applied over budget: nick len %d", len(m.Nick))
		}
	}

	// Accounting stays exact through the mutation paths. Takes the roster
	// under test explicitly — it must inspect the SAME roster being mutated
	// (r2 below), not the earlier small-budget r.
	checkExact := func(rr *roster, step string) {
		t.Helper()
		rr.mu.Lock()
		defer rr.mu.Unlock()
		for _, st := range rr.chans {
			want := len(st.topic)
			for k, m := range st.members {
				want += memberBytes(k, m)
			}
			for k, m := range st.pending {
				want += memberBytes(k, m)
			}
			if st.bytes != want {
				t.Fatalf("%s: channel %q bytes %d != recomputed %d", step, st.name, st.bytes, want)
			}
		}
	}
	r2 := testRoster()
	maxRosterBytes = 8 << 20 // the outer defer restores the real value
	feed(t, r2, ":AlteredParadox!u@h JOIN #x",
		":irc 353 AlteredParadox = #x :@op +voice plain AlteredParadox",
		":irc 366 AlteredParadox #x :End of /NAMES list")
	checkExact(r2, "after NAMES")
	feed(t, r2, ":plain!u@h ACCOUNT services-acct")
	checkExact(r2, "after ACCOUNT")
	feed(t, r2, ":op!u@h NICK operator")
	checkExact(r2, "after NICK")
	feed(t, r2, ":irc TOPIC #x :a channel topic")
	checkExact(r2, "after TOPIC")
	feed(t, r2, ":voice!u@h QUIT :bye")
	checkExact(r2, "after QUIT")
	feed(t, r2, ":plain!u@h PART #x")
	checkExact(r2, "after PART")
	feed(t, r2, ":irc MODE #x +o operator")
	checkExact(r2, "after MODE")
}

func TestRoster(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		check func(t *testing.T, r *roster)
	}{
		{
			name: "NAMES accumulates across lines and swaps on 366",
			lines: []string{
				joinGo,
				":srv 353 AlteredParadox = #go :@op +voiced AlteredParadox",
				":srv 353 AlteredParadox = #go :plain",
				":srv 366 AlteredParadox #go :End of /NAMES list",
			},
			check: func(t *testing.T, r *roster) {
				want := []Member{{Nick: "AlteredParadox"}, {Nick: "op", Prefix: "@"}, {Nick: "plain"}, {Nick: "voiced", Prefix: "+"}}
				if got := members(t, r, "#go"); !reflect.DeepEqual(got, want) {
					t.Fatalf("members = %v, want %v", got, want)
				}
			},
		},
		{
			name: "multi-prefix NAMES keeps all prefixes ordered",
			lines: []string{
				// Extended prefixes exist only when 005 advertises them.
				":srv 005 AlteredParadox PREFIX=(qaohv)~&@%+ :are supported by this server",
				joinGo,
				":srv 353 AlteredParadox = #go :~owner &admin %half @+multi",
				":srv 366 AlteredParadox #go :end",
			},
			check: func(t *testing.T, r *roster) {
				want := []Member{
					{Nick: "admin", Prefix: "&"}, {Nick: "half", Prefix: "%"},
					{Nick: "multi", Prefix: "@+"}, {Nick: "owner", Prefix: "~"},
				}
				if got := members(t, r, "#go"); !reflect.DeepEqual(got, want) {
					t.Fatalf("members = %v, want %v", got, want)
				}
			},
		},
		{
			name: "userhost-in-names hostmasks are stripped to nicks",
			lines: []string{
				joinGo,
				":srv 353 AlteredParadox = #go :@+alice!a@host.example bob!b@2001:db8::1 AlteredParadox!u@h",
				":srv 366 AlteredParadox #go :end",
			},
			check: func(t *testing.T, r *roster) {
				want := []Member{
					{Nick: "alice", Prefix: "@+"}, {Nick: "AlteredParadox"}, {Nick: "bob"},
				}
				if got := members(t, r, "#go"); !reflect.DeepEqual(got, want) {
					t.Fatalf("members = %v, want %v", got, want)
				}
			},
		},
		{
			name: "away-notify toggles away state",
			lines: []string{
				joinGo, ":alice!u@h JOIN #go",
				":alice!u@h AWAY :gone fishing",
			},
			check: func(t *testing.T, r *roster) {
				if got := members(t, r, "#go"); !got[0].Away {
					t.Fatalf("alice not away: %v", got)
				}
				feed(t, r, ":alice!u@h AWAY")
				if got := members(t, r, "#go"); got[0].Away {
					t.Fatalf("alice still away: %v", got)
				}
			},
		},
		{
			name: "mode revocation on stacked prefixes keeps the rest",
			lines: []string{
				joinGo,
				":srv 353 AlteredParadox = #go :@+alice AlteredParadox",
				":srv 366 AlteredParadox #go :end",
				":op!u@h MODE #go -o alice",
			},
			check: func(t *testing.T, r *roster) {
				if got := members(t, r, "#go"); got[0].Prefix != "+" {
					t.Fatalf("alice prefix = %q, want +", got[0].Prefix)
				}
				// A re-grant inserts in rank order.
				feed(t, r, ":op!u@h MODE #go +o alice")
				if got := members(t, r, "#go"); got[0].Prefix != "@+" {
					t.Fatalf("alice prefix = %q, want @+", got[0].Prefix)
				}
			},
		},
		{
			name:  "join and part",
			lines: []string{joinGo, ":alice!u@h JOIN #go", ":alice!u@h PART #go :bye"},
			check: func(t *testing.T, r *roster) {
				want := []Member{{Nick: "AlteredParadox"}}
				if got := members(t, r, "#go"); !reflect.DeepEqual(got, want) {
					t.Fatalf("members = %v, want %v", got, want)
				}
			},
		},
		{
			name:  "our part drops the channel",
			lines: []string{joinGo, ":AlteredParadox!u@h PART #go"},
			check: func(t *testing.T, r *roster) {
				if _, _, ok := r.channel("#go"); ok {
					t.Fatal("channel still tracked after our PART")
				}
			},
		},
		{
			name:  "kick removes the victim, our kick drops the channel",
			lines: []string{joinGo, ":alice!u@h JOIN #go", ":op!u@h KICK #go alice :out"},
			check: func(t *testing.T, r *roster) {
				if got := members(t, r, "#go"); len(got) != 1 || got[0].Nick != "AlteredParadox" {
					t.Fatalf("members = %v", got)
				}
				feed(t, r, ":op!u@h KICK #go AlteredParadox :you too")
				if _, _, ok := r.channel("#go"); ok {
					t.Fatal("channel still tracked after being kicked")
				}
			},
		},
		{
			name: "quit removes from every channel",
			lines: []string{
				joinGo, ":AlteredParadox!u@h JOIN #two",
				":alice!u@h JOIN #go", ":alice!u@h JOIN #two",
				":alice!u@h QUIT :gone",
			},
			check: func(t *testing.T, r *roster) {
				if got := members(t, r, "#go"); len(got) != 1 {
					t.Fatalf("#go members = %v", got)
				}
				if got := members(t, r, "#two"); len(got) != 1 {
					t.Fatalf("#two members = %v", got)
				}
			},
		},
		{
			name: "nick change preserves the prefix",
			lines: []string{
				joinGo,
				":srv 353 AlteredParadox = #go :@alice AlteredParadox",
				":srv 366 AlteredParadox #go :end",
				":alice!u@h NICK alicia",
			},
			check: func(t *testing.T, r *roster) {
				want := []Member{{Nick: "alicia", Prefix: "@"}, {Nick: "AlteredParadox"}}
				if got := members(t, r, "#go"); !reflect.DeepEqual(got, want) {
					t.Fatalf("members = %v, want %v", got, want)
				}
			},
		},
		{
			name: "mode grants and revocations",
			lines: []string{
				joinGo,
				":srv 353 AlteredParadox = #go :alice bob AlteredParadox",
				":srv 366 AlteredParadox #go :end",
				":op!u@h MODE #go +ov alice bob",
				":op!u@h MODE #go -o alice",
			},
			check: func(t *testing.T, r *roster) {
				want := []Member{{Nick: "alice"}, {Nick: "AlteredParadox"}, {Nick: "bob", Prefix: "+"}}
				if got := members(t, r, "#go"); !reflect.DeepEqual(got, want) {
					t.Fatalf("members = %v, want %v", got, want)
				}
			},
		},
		{
			name: "mode argument consumption skips non-status args",
			lines: []string{
				joinGo,
				":srv 353 AlteredParadox = #go :alice AlteredParadox",
				":srv 366 AlteredParadox #go :end",
				// +b and +k consume args, +l consumes when setting, im do
				// not; the op grant must land on alice, not a mode arg.
				":op!u@h MODE #go +bklimo *!*@spam sekrit 42 alice",
			},
			check: func(t *testing.T, r *roster) {
				want := []Member{{Nick: "alice", Prefix: "@"}, {Nick: "AlteredParadox"}}
				if got := members(t, r, "#go"); !reflect.DeepEqual(got, want) {
					t.Fatalf("members = %v, want %v", got, want)
				}
			},
		},
		{
			name: "unsetting a list mode still consumes its arg",
			lines: []string{
				joinGo,
				":srv 353 AlteredParadox = #go :alice AlteredParadox",
				":srv 366 AlteredParadox #go :end",
				":op!u@h MODE #go -b+o *!*@spam alice",
			},
			check: func(t *testing.T, r *roster) {
				if got := members(t, r, "#go"); got[0].Prefix != "@" {
					t.Fatalf("members = %v", got)
				}
			},
		},
		{
			name: "topic from 332, TOPIC, and 331",
			lines: []string{
				joinGo,
				":srv 332 AlteredParadox #go :welcome to go",
			},
			check: func(t *testing.T, r *roster) {
				if topic, _, _ := r.channel("#go"); topic != "welcome to go" {
					t.Fatalf("topic = %q", topic)
				}
				feed(t, r, ":alice!u@h TOPIC #go :new topic")
				if topic, _, _ := r.channel("#go"); topic != "new topic" {
					t.Fatalf("topic = %q", topic)
				}
				feed(t, r, ":srv 331 AlteredParadox #go :No topic is set")
				if topic, _, _ := r.channel("#go"); topic != "" {
					t.Fatalf("topic = %q", topic)
				}
			},
		},
		{
			name:  "case-insensitive channel and nick handling",
			lines: []string{joinGo, ":Alice!u@h JOIN #GO", ":ALICE!u@h PART #Go"},
			check: func(t *testing.T, r *roster) {
				want := []Member{{Nick: "AlteredParadox"}}
				if got := members(t, r, "#gO"); !reflect.DeepEqual(got, want) {
					t.Fatalf("members = %v, want %v", got, want)
				}
			},
		},
		{
			name: "ISUPPORT-driven mode consumption with custom CHANMODES",
			lines: []string{
				// libera-style: q is a list mode (quiet), f takes an arg
				// only when set, j likewise.
				":srv 005 AlteredParadox CHANMODES=eIbq,k,flj,CPcgimnprstuz :are supported by this server",
				joinGo,
				":srv 353 AlteredParadox = #go :alice AlteredParadox",
				":srv 366 AlteredParadox #go :end",
				// +q consumes the mask (list mode, NOT owner status here),
				// +f consumes, then +o grants alice.
				":op!u@h MODE #go +qfo *!*@spam 30:5 alice",
			},
			check: func(t *testing.T, r *roster) {
				want := []Member{{Nick: "alice", Prefix: "@"}, {Nick: "AlteredParadox"}}
				if got := members(t, r, "#go"); !reflect.DeepEqual(got, want) {
					t.Fatalf("members = %v, want %v", got, want)
				}
			},
		},
		{
			name: "unsetting a C-type mode consumes no argument",
			lines: []string{
				":srv 005 AlteredParadox CHANMODES=b,k,fl,imnpst :are supported by this server",
				joinGo,
				":srv 353 AlteredParadox = #go :alice AlteredParadox",
				":srv 366 AlteredParadox #go :end",
				// -f takes no arg when unsetting, so alice is +o's target.
				":op!u@h MODE #go -f+o alice",
			},
			check: func(t *testing.T, r *roster) {
				if got := members(t, r, "#go"); got[0].Prefix != "@" {
					t.Fatalf("members = %v", got)
				}
			},
		},
		{
			name: "ascii casemapping distinguishes bracket nicks",
			lines: []string{
				":srv 005 AlteredParadox CASEMAPPING=ascii :are supported by this server",
				joinGo,
				":alice[]!u@h JOIN #go",
				// Under ascii, alice{} is a DIFFERENT user; this PART must
				// not remove alice[].
				":alice{}!u@h PART #go",
			},
			check: func(t *testing.T, r *roster) {
				if got := members(t, r, "#go"); len(got) != 2 {
					t.Fatalf("ascii casemapping folded brackets: %v", got)
				}
			},
		},
		{
			name: "rfc1459 casemapping folds bracket nicks",
			lines: []string{
				joinGo,
				":alice[]!u@h JOIN #go",
				":alice{}!u@h PART #go", // same user under rfc1459
			},
			check: func(t *testing.T, r *roster) {
				if got := members(t, r, "#go"); len(got) != 1 {
					t.Fatalf("rfc1459 casemapping missed brackets: %v", got)
				}
			},
		},
		{
			name:  "unknown channel messages are ignored",
			lines: []string{":srv 353 AlteredParadox = #ghost :@op", ":srv 366 AlteredParadox #ghost :end", ":alice!u@h JOIN #ghost"},
			check: func(t *testing.T, r *roster) {
				if _, _, ok := r.channel("#ghost"); ok {
					t.Fatal("tracked a channel we never joined")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testRoster()
			feed(t, r, tc.lines...)
			tc.check(t, r)
		})
	}
}

// A member/channel whose name exceeds maxRosterField is stored under a
// clamped key; the removal/update lookups must clamp too, or the entry
// becomes a ghost that only reconnect clears. Regression for the unclamped
// fold-lookup sites (PART/KICK/MODE/AWAY/channel).
func TestRosterOversizedNameNoGhost(t *testing.T) {
	bigNick := strings.Repeat("n", maxRosterField+50)
	bigChan := "#" + strings.Repeat("c", maxRosterField+50)

	// Oversized nick joins #go, then parts: it must actually leave.
	r := testRoster()
	feed(t, r, joinGo, ":"+bigNick+"!u@h JOIN #go")
	if _, ms, _ := r.channel("#go"); len(ms) != 2 {
		t.Fatalf("after join: %d members, want 2", len(ms))
	}
	feed(t, r, ":"+bigNick+"!u@h PART #go :bye")
	if _, ms, _ := r.channel("#go"); len(ms) != 1 {
		t.Fatalf("oversized nick ghosted after PART: %d members, want 1", len(ms))
	}

	// A self-JOIN to an oversized channel, then self-PART: the channel state
	// must be deleted, not stranded.
	feed(t, r, ":AlteredParadox!u@h JOIN "+bigChan)
	if _, _, ok := r.channel(bigChan); !ok {
		t.Fatal("oversized channel not created on self-JOIN")
	}
	feed(t, r, ":AlteredParadox!u@h PART "+bigChan)
	if _, _, ok := r.channel(bigChan); ok {
		t.Fatal("oversized channel ghosted after self-PART")
	}
}

// Multi-target PART/KICK (comma-lists, RFC 2812 §3.2.2/§3.2.8) must remove
// the member from every named channel — a server that doesn't split them
// would otherwise leave ghosts.
func TestRosterMultiTargetPartKick(t *testing.T) {
	r := testRoster()
	feed(t, r,
		":AlteredParadox!u@h JOIN #a", ":AlteredParadox!u@h JOIN #b", ":AlteredParadox!u@h JOIN #c",
		":bob!u@h JOIN #a", ":bob!u@h JOIN #b",
		":carol!u@h JOIN #c", ":dave!u@h JOIN #c")

	// bob parts #a and #b in one line.
	feed(t, r, ":bob!u@h PART #a,#b :bye")
	for _, ch := range []string{"#a", "#b"} {
		for _, m := range members(t, r, ch) {
			if m.Nick == "bob" {
				t.Fatalf("bob ghosted in %s after multi-PART", ch)
			}
		}
	}

	// One channel, two victims kicked in one line.
	feed(t, r, ":op!u@h KICK #c carol,dave :out")
	if ms := members(t, r, "#c"); len(ms) != 1 || ms[0].Nick != "AlteredParadox" {
		t.Fatalf("#c after multi-KICK = %v, want just AlteredParadox", rosterNicks(ms))
	}
}

func rosterNicks(ms []Member) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Nick
	}
	return out
}

func TestRosterClear(t *testing.T) {
	r := testRoster()
	feed(t, r, joinGo, ":alice!u@h JOIN #go")
	r.clear()
	if _, _, ok := r.channel("#go"); ok {
		t.Fatal("state survived clear")
	}
}

func TestRosterAccountsAndWHOX(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		check func(t *testing.T, r *roster)
	}{
		{
			name: "WHOX 354 sets away and account for existing members",
			lines: []string{
				joinGo,
				":srv 353 AlteredParadox = #go :alice bob AlteredParadox",
				":srv 366 AlteredParadox #go :End of /NAMES list",
				":srv 354 AlteredParadox 152 alice G alicerella",
				":srv 354 AlteredParadox 152 bob H 0", // logged out
			},
			check: func(t *testing.T, r *roster) {
				want := []Member{
					{Nick: "alice", Away: true, Account: "alicerella"},
					{Nick: "AlteredParadox"},
					{Nick: "bob"},
				}
				if got := members(t, r, "#go"); !reflect.DeepEqual(got, want) {
					t.Fatalf("members = %v, want %v", got, want)
				}
			},
		},
		{
			name: "354 with a foreign token is ignored",
			lines: []string{
				joinGo,
				":srv 353 AlteredParadox = #go :alice",
				":srv 366 AlteredParadox #go :x",
				":srv 354 AlteredParadox 999 alice G someacct",
			},
			check: func(t *testing.T, r *roster) {
				if got := members(t, r, "#go"); got[0].Away || got[0].Account != "" {
					t.Fatalf("foreign-token 354 applied: %v", got[0])
				}
			},
		},
		{
			name: "extended-join carries the account; * means logged out",
			lines: []string{
				joinGo,
				":srv 366 AlteredParadox #go :x",
				":carol!u@h JOIN #go carolacct :Carol C.",
				":dave!u@h JOIN #go * :Dave D.",
			},
			check: func(t *testing.T, r *roster) {
				got := members(t, r, "#go")
				if got[1].Nick != "carol" || got[1].Account != "carolacct" {
					t.Fatalf("carol = %v", got[1])
				}
				if got[2].Nick != "dave" || got[2].Account != "" {
					t.Fatalf("dave = %v", got[2])
				}
			},
		},
		{
			name: "account-notify updates and clears",
			lines: []string{
				joinGo,
				":srv 353 AlteredParadox = #go :alice",
				":srv 366 AlteredParadox #go :x",
				":alice!u@h ACCOUNT alicerella",
			},
			check: func(t *testing.T, r *roster) {
				if got := members(t, r, "#go"); got[0].Account != "alicerella" {
					t.Fatalf("after ACCOUNT: %v", got[0])
				}
				feed(t, r, ":alice!u@h ACCOUNT *")
				if got := members(t, r, "#go"); got[0].Account != "" {
					t.Fatalf("after logout: %v", got[0])
				}
			},
		},
		{
			name: "a NAMES refresh keeps learned away/account state",
			lines: []string{
				joinGo,
				":srv 353 AlteredParadox = #go :alice AlteredParadox",
				":srv 366 AlteredParadox #go :x",
				":srv 354 AlteredParadox 152 alice G alicerella",
				":srv 353 AlteredParadox = #go :@alice AlteredParadox", // refresh, alice opped meanwhile
				":srv 366 AlteredParadox #go :x",
			},
			check: func(t *testing.T, r *roster) {
				got := members(t, r, "#go")
				if got[0].Prefix != "@" || !got[0].Away || got[0].Account != "alicerella" {
					t.Fatalf("after refresh: %v", got[0])
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testRoster()
			feed(t, r, tc.lines...)
			tc.check(t, r)
		})
	}
}

func TestRosterBotFlag(t *testing.T) {
	r := testRoster()
	feed(t, r,
		":srv 005 AlteredParadox BOT=B :are supported by this server",
		joinGo,
		":srv 353 AlteredParadox = #go :guard alice AlteredParadox",
		":srv 366 AlteredParadox #go :x",
		":srv 354 AlteredParadox 152 guard H*B botacct",
		":srv 354 AlteredParadox 152 alice H 0",
	)
	got := members(t, r, "#go") // nick-sorted: alice, AlteredParadox, guard
	if !got[2].Bot || got[2].Account != "botacct" {
		t.Fatalf("guard = %+v, want bot with account", got[2])
	}
	if got[0].Bot {
		t.Fatalf("alice flagged as bot: %+v", got[0])
	}
	// Without the ISUPPORT BOT letter, flags are not misread.
	r2 := testRoster()
	feed(t, r2,
		joinGo,
		":srv 353 AlteredParadox = #go :guard AlteredParadox",
		":srv 366 AlteredParadox #go :x",
		":srv 354 AlteredParadox 152 guard HB 0",
	)
	if got := members(t, r2, "#go"); got[1].Bot {
		t.Fatalf("bot flagged without ISUPPORT BOT: %+v", got[1])
	}
}

func TestChannelsWith(t *testing.T) {
	r := testRoster()
	feed(t, r,
		joinGo,
		":srv 353 AlteredParadox = #go :alice AlteredParadox",
		":srv 366 AlteredParadox #go :x",
		":AlteredParadox!u@h JOIN #rust",
		":srv 353 AlteredParadox = #rust :Alice bob AlteredParadox",
		":srv 366 AlteredParadox #rust :x",
	)
	if got := r.channelsWith("ALICE"); !reflect.DeepEqual(got, []string{"#go", "#rust"}) {
		t.Fatalf("channelsWith(ALICE) = %v (casemapped lookup)", got)
	}
	if got := r.channelsWith("bob"); !reflect.DeepEqual(got, []string{"#rust"}) {
		t.Fatalf("channelsWith(bob) = %v", got)
	}
	if got := r.channelsWith("ghost"); got != nil {
		t.Fatalf("channelsWith(ghost) = %v", got)
	}
}

// A repeat self-JOIN for a channel we are already in preserves the
// accumulated members and topic instead of wiping them.
func TestRosterDuplicateSelfJoinPreserves(t *testing.T) {
	r := testRoster()
	feed(t, r,
		":AlteredParadox JOIN #chan",
		":srv 353 AlteredParadox = #chan :AlteredParadox alice bob",
		":srv 366 AlteredParadox #chan :end",
		":srv 332 AlteredParadox #chan :the topic",
	)
	if got := len(members(t, r, "#chan")); got != 3 {
		t.Fatalf("setup members = %d, want 3", got)
	}
	// Duplicate self-JOIN must not reset the channel.
	feed(t, r, ":AlteredParadox JOIN #chan")
	if got := len(members(t, r, "#chan")); got != 3 {
		t.Fatalf("members after dup self-JOIN = %d, want 3 (state preserved)", got)
	}
	if topic, _, _ := r.channel("#chan"); topic != "the topic" {
		t.Fatalf("topic lost on dup self-JOIN: %q", topic)
	}
}

// A member departing mid-NAMES (between 353 and 366) is not resurrected
// by the 366 swap.
func TestRosterLiveDepartureDuringNames(t *testing.T) {
	r := testRoster()
	feed(t, r, ":AlteredParadox JOIN #chan") // channel exists, empty
	// NAMES burst begins, listing bob.
	feed(t, r, ":srv 353 AlteredParadox = #chan :AlteredParadox alice bob")
	// bob QUITs before 366.
	feed(t, r, ":bob QUIT :gone")
	// Also a live JOIN of carol before 366.
	feed(t, r, ":carol JOIN #chan")
	feed(t, r, ":srv 366 AlteredParadox #chan :end")

	names := map[string]bool{}
	for _, m := range members(t, r, "#chan") {
		names[m.Nick] = true
	}
	if names["bob"] {
		t.Fatal("bob (quit mid-NAMES) resurrected by the 366 swap")
	}
	if !names["carol"] {
		t.Fatal("carol (joined mid-NAMES) lost by the 366 swap")
	}
	if !names["alice"] || !names["AlteredParadox"] {
		t.Fatalf("NAMES members missing: %v", names)
	}
}

// A MODE prefix change arriving mid-NAMES survives the 366 swap.
func TestRosterModeDuringNames(t *testing.T) {
	r := testRoster()
	feed(t, r,
		"@nick=AlteredParadox :srv 005 AlteredParadox PREFIX=(ov)@+ :are supported",
		":AlteredParadox JOIN #chan",
		":srv 353 AlteredParadox = #chan :AlteredParadox alice",
	)
	// Op alice mid-NAMES, before 366.
	feed(t, r, ":srv MODE #chan +o alice")
	feed(t, r, ":srv 366 AlteredParadox #chan :end")

	for _, m := range members(t, r, "#chan") {
		if m.Nick == "alice" {
			if m.Prefix != "@" {
				t.Fatalf("alice prefix = %q, want @ (MODE survived the swap)", m.Prefix)
			}
			return
		}
	}
	t.Fatal("alice missing")
}
