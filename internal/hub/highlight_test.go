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

import "testing"

// The case tables mirror web/test/irc.test.js ("mentionsMe",
// "stripFormatting ...") and web/test/notify.test.js ("highlightText: ...")
// so the Go port and the client matcher prove the same semantics.

func TestStripCodes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\\\x0306^\x0313.\x0305^\x0304/", `\^.^/`},
		{"\x02bold\x0f \x0304,05colored\x03", "bold colored"},
		// A bare \x03 before ",digits" is a reset + literal text.
		{"\x03,5 o'clock", ",5 o'clock"},
		{"\x0304,05red\x03", "red"},
		{"\x04ff0000red\x04", "red"},
		{"plain", "plain"},
	}
	for _, c := range cases {
		if got := stripCodes(c.in); got != c.want {
			t.Errorf("stripCodes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMentionsMe(t *testing.T) {
	cases := []struct {
		text, nick string
		want       bool
	}{
		{"AlteredParadox: hello", "AlteredParadox", true},
		{"hey AlteredParadox", "AlteredParadox", true},
		{"ALTEREDPARADOX ping", "AlteredParadox", true},
		{"unAlteredParadoxed", "AlteredParadox", false},
		{"AlteredParadox_ is someone else", "AlteredParadox", false},
		{"ping AlteredParadox[]", "AlteredParadox[]", true},
		{"nothing here", "AlteredParadox", false},
		{"anything", "", false},
		// Formatting between characters must not hide a mention.
		{"hey \x0304alice\x03!", "alice", true},
		{"al\x04ice", "alice", true},
		{"al\x03,1ice", "alice", false}, // visibly "al,1ice", not a mention
		// rfc1459 casemapping fold (RFC 2812 §2.2): {}|^ ≡ []\~.
		{"dan{m}: the deploy broke", "dan[m]", true},
		{"dan[m]: ping", "dan{m}", true},
		{"DAN{M} around?", "dan[m]", true},
		{"hey d|n^", `d\n~`, true},
		{"dan(m) hi", "dan[m]", false},    // ( is not rfc1459-equivalent to [
		{"xdan{m} nope", "dan[m]", false}, // folded {} are nick chars, not boundaries
		{"dan{m}x nope", "dan[m]", false},
	}
	for _, c := range cases {
		if got := mentionsMe(c.text, c.nick); got != c.want {
			t.Errorf("mentionsMe(%q, %q) = %v, want %v", c.text, c.nick, got, c.want)
		}
	}
}

func TestHighlightText(t *testing.T) {
	global := []Rule{{Pattern: "deploy"}}
	scoped := []Rule{{Pattern: "release", Network: "libera"}}
	cases := []struct {
		name, text, nick, network string
		rules                     []Rule
		want                      bool
	}{
		{"nick mention", "hey AlteredParadox look", "AlteredParadox", "libera", nil, true},
		{"no partial-word mention", "category AlteredParadoxx", "AlteredParadox", "libera", nil, false},
		{"case-insensitive mention", "ALTEREDPARADOX shouted", "AlteredParadox", "libera", nil, true},
		{"rfc1459 fold reaches highlight", "dan{m}: the deploy broke", "dan[m]", "libera", nil, true},
		{"global keyword", "time to deploy now", "AlteredParadox", "libera", global, true},
		{"global keyword any network", "Deploy the thing", "AlteredParadox", "oftc", global, true},
		{"no match", "nothing", "AlteredParadox", "libera", global, false},
		{"scoped keyword hits", "new release out", "AlteredParadox", "libera", scoped, true},
		{"scoped keyword scoped away", "new release out", "AlteredParadox", "oftc", scoped, false},
		{"empty pattern ignored", "hello", "AlteredParadox", "libera", []Rule{{Pattern: ""}}, false},
		{"empty text", "", "AlteredParadox", "libera", global, false},
		{"no nick no rules", "hi", "", "libera", nil, false},
		{"bold mid-keyword", "de\x02ploy now", "AlteredParadox", "libera", global, true},
		{"colour mid-keyword", "de\x0304ploy now", "AlteredParadox", "libera", global, true},
		{"bare hex byte mid-keyword", "de\x04ploy now", "AlteredParadox", "libera", global, true},
		{"substring match", "category theory", "AlteredParadox", "libera", []Rule{{Pattern: "cat"}}, true},
	}
	for _, c := range cases {
		if got := highlightText(c.text, c.nick, c.rules, c.network); got != c.want {
			t.Errorf("%s: highlightText(%q, %q, %v, %q) = %v, want %v",
				c.name, c.text, c.nick, c.rules, c.network, got, c.want)
		}
	}
}
