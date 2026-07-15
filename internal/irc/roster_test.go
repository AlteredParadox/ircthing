package irc

import (
	"reflect"
	"testing"

	ircv4 "gopkg.in/irc.v4"
)

// feed parses and applies lines as the user "AlteredParadox".
func feed(t *testing.T, r *roster, lines ...string) {
	t.Helper()
	for _, l := range lines {
		r.handle("AlteredParadox", ircv4.MustParseMessage(l))
	}
}

func members(t *testing.T, r *roster, ch string) []Member {
	t.Helper()
	_, ms, _ := r.channel(ch)
	return ms
}

// join sets up our own membership of #go.
var joinGo = ":AlteredParadox!u@h JOIN #go"

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
			r := newRoster()
			feed(t, r, tc.lines...)
			tc.check(t, r)
		})
	}
}

func TestRosterClear(t *testing.T) {
	r := newRoster()
	feed(t, r, joinGo, ":alice!u@h JOIN #go")
	r.clear()
	if _, _, ok := r.channel("#go"); ok {
		t.Fatal("state survived clear")
	}
}
