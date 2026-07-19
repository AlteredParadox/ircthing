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
	"testing"

	ircv4 "gopkg.in/irc.v4"
)

func feed005(s *isupport, tokens string) {
	s.handle(ircv4.MustParseMessage(":srv 005 AlteredParadox " + tokens + " :are supported by this server"))
}

// An implausibly large ISUPPORT value (a hostile 005 can be up to the
// 16 KiB incoming-line limit) must be ignored so it can't drive a quadratic
// MODE scan; real values are tens of bytes.
func TestISupportValueLengthBounded(t *testing.T) {
	s := newISupport()
	huge := ""
	for i := 0; i < maxISupportValue+100; i++ {
		huge += "b"
	}
	feed005(s, "CHANMODES="+huge)
	if v, ok := s.Raw("CHANMODES"); ok {
		t.Fatalf("oversized CHANMODES stored (%d bytes), want ignored", len(v))
	}
	// The default classification still applies (the huge value was dropped).
	if s.ChanModeType('b') != 'A' {
		t.Fatal("default CHANMODES classification lost after dropping oversized value")
	}
	// A normal-sized value is still accepted.
	feed005(s, "CHANMODES=eIb,k,l,imnpst")
	if v, ok := s.Raw("CHANMODES"); !ok || v != "eIb,k,l,imnpst" {
		t.Fatalf("normal CHANMODES = %q, %v; want accepted", v, ok)
	}
}

func TestISupportDefaults(t *testing.T) {
	s := newISupport()
	if !s.IsChannel("#go") || !s.IsChannel("&local") || s.IsChannel("alice") {
		t.Fatal("default CHANTYPES wrong")
	}
	if s.PrefixSymbols() != "@+" || s.SymbolForMode('o') != "@" || s.SymbolForMode('v') != "+" {
		t.Fatal("default PREFIX wrong")
	}
	if s.ChanModeType('b') != 'A' || s.ChanModeType('k') != 'B' ||
		s.ChanModeType('l') != 'C' || s.ChanModeType('i') != 'D' ||
		s.ChanModeType('o') != 'P' || s.ChanModeType('X') != 0 {
		t.Fatal("default CHANMODES classification wrong")
	}
}

func TestISupportParsing(t *testing.T) {
	cases := []struct {
		name   string
		tokens []string
		check  func(t *testing.T, s *isupport)
	}{
		{
			name:   "custom PREFIX with extended ranks",
			tokens: []string{"PREFIX=(qaohv)~&@%+"},
			check: func(t *testing.T, s *isupport) {
				if s.PrefixSymbols() != "~&@%+" {
					t.Fatalf("symbols = %q", s.PrefixSymbols())
				}
				if s.SymbolForMode('q') != "~" || s.SymbolForMode('h') != "%" {
					t.Fatal("mode->symbol mapping wrong")
				}
				if s.ChanModeType('q') != 'P' {
					t.Fatal("status mode not classified as P")
				}
			},
		},
		{
			name:   "empty PREFIX means no status prefixes",
			tokens: []string{"PREFIX="},
			check: func(t *testing.T, s *isupport) {
				if s.PrefixSymbols() != "" || s.SymbolForMode('o') != "" {
					t.Fatal("empty PREFIX not honored")
				}
			},
		},
		{
			name:   "malformed PREFIX keeps previous value",
			tokens: []string{"PREFIX=(ov@+"},
			check: func(t *testing.T, s *isupport) {
				if s.PrefixSymbols() != "@+" {
					t.Fatalf("symbols = %q", s.PrefixSymbols())
				}
			},
		},
		{
			name:   "libera-style CHANMODES",
			tokens: []string{"CHANMODES=eIbq,k,flj,CPcgimnprstuz"},
			check: func(t *testing.T, s *isupport) {
				if s.ChanModeType('q') != 'A' || s.ChanModeType('e') != 'A' ||
					s.ChanModeType('k') != 'B' || s.ChanModeType('f') != 'C' ||
					s.ChanModeType('z') != 'D' {
					t.Fatal("classification wrong")
				}
			},
		},
		{
			name:   "PREFIX wins over CHANMODES for status letters",
			tokens: []string{"CHANMODES=q,k,l,imnpst", "PREFIX=(qov)~@+"},
			check: func(t *testing.T, s *isupport) {
				// Solanum lists q as a list mode AND uses it in PREFIX on
				// some networks; PREFIX classification must win.
				if s.ChanModeType('q') != 'P' {
					t.Fatalf("q classified %c, want P", s.ChanModeType('q'))
				}
			},
		},
		{
			name:   "CHANTYPES including empty",
			tokens: []string{"CHANTYPES=#"},
			check: func(t *testing.T, s *isupport) {
				if s.IsChannel("&local") || !s.IsChannel("#go") {
					t.Fatal("CHANTYPES=# not honored")
				}
				feed005(s, "CHANTYPES=")
				if s.IsChannel("#go") {
					t.Fatal("empty CHANTYPES should mean no channels")
				}
			},
		},
		{
			name:   "negation restores the default",
			tokens: []string{"CHANTYPES=#", "-CHANTYPES"},
			check: func(t *testing.T, s *isupport) {
				if !s.IsChannel("&local") {
					t.Fatal("negation did not restore default")
				}
			},
		},
		{
			name:   "raw access and escapes",
			tokens: []string{"NETWORK=Libera.Chat", "AWAYLEN=200", `EXAMPLE=a\x20b`},
			check: func(t *testing.T, s *isupport) {
				if v, _ := s.Raw("NETWORK"); v != "Libera.Chat" {
					t.Fatalf("NETWORK = %q", v)
				}
				if v, _ := s.Raw("EXAMPLE"); v != "a b" {
					t.Fatalf("escape: %q", v)
				}
				if _, ok := s.Raw("MISSING"); ok {
					t.Fatal("missing key reported present")
				}
			},
		},
		{
			name:   "accumulates across multiple 005 lines",
			tokens: []string{"CHANTYPES=#", "PREFIX=(ov)@+"},
			check: func(t *testing.T, s *isupport) {
				feed005(s, "CASEMAPPING=ascii MODES=4")
				if v, _ := s.Raw("MODES"); v != "4" || !s.IsChannel("#x") {
					t.Fatal("second 005 line clobbered earlier state")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newISupport()
			for _, tok := range tc.tokens {
				feed005(s, tok)
			}
			tc.check(t, s)
		})
	}
}

func TestISupportFold(t *testing.T) {
	cases := []struct {
		mapping string
		a, b    string
		equal   bool
	}{
		{"rfc1459", "Nick[]\\~", "nick{}|^", true},
		{"rfc1459-strict", "Nick[]\\", "nick{}|", true},
		{"rfc1459-strict", "who~", "who^", false},
		{"ascii", "NICK", "nick", true},
		{"ascii", "nick[]", "nick{}", false},
		{"rfc1459", "#GO", "#go", true},
	}
	for _, tc := range cases {
		s := newISupport()
		feed005(s, "CASEMAPPING="+tc.mapping)
		if got := s.FoldEqual(tc.a, tc.b); got != tc.equal {
			t.Errorf("%s: FoldEqual(%q, %q) = %v, want %v", tc.mapping, tc.a, tc.b, got, tc.equal)
		}
	}
	// Unknown casemapping values are ignored.
	s := newISupport()
	feed005(s, "CASEMAPPING=unicode")
	if !s.FoldEqual("a[", "a{") {
		t.Fatal("unknown casemapping should keep rfc1459")
	}
}

func TestISupportReset(t *testing.T) {
	s := newISupport()
	feed005(s, "CHANTYPES=# CASEMAPPING=ascii NETWORK=x")
	s.reset()
	if !s.IsChannel("&local") || !s.FoldEqual("a[", "a{") {
		t.Fatal("reset did not restore defaults")
	}
	if _, ok := s.Raw("NETWORK"); ok {
		t.Fatal("raw values survived reset")
	}
}

// A CASEMAPPING that changes mid-session is ignored: folding is baked into
// existing map keys, so the first explicit mapping is pinned for the connection.
func TestISupportCasemappingLocked(t *testing.T) {
	s := newISupport()
	feed005(s, "CASEMAPPING=ascii")
	if s.FoldEqual("a[", "a{") { // ascii: [ and { are distinct
		t.Fatal("ascii mapping not applied")
	}
	feed005(s, "CASEMAPPING=rfc1459") // must be ignored
	if s.FoldEqual("a[", "a{") {
		t.Fatal("mid-session CASEMAPPING change was applied (should be locked to ascii)")
	}
	// A fresh registration (reset) accepts a new mapping again.
	s.reset()
	feed005(s, "CASEMAPPING=rfc1459")
	if !s.FoldEqual("a[", "a{") {
		t.Fatal("reset should re-open CASEMAPPING")
	}
}

// A CASEMAPPING NEGATION ("-CASEMAPPING") must honor the lock too: once a
// mapping is pinned, removing it must not reset folding to the default and
// strand already-folded map keys (the positive path is guarded; the negation
// path must be as well).
func TestISupportCasemappingNegationLocked(t *testing.T) {
	s := newISupport()
	feed005(s, "CASEMAPPING=ascii")
	if s.FoldEqual("a[", "a{") { // ascii: [ and { are distinct
		t.Fatal("ascii mapping not applied")
	}
	feed005(s, "-CASEMAPPING") // negation must be ignored once locked
	if s.FoldEqual("a[", "a{") {
		t.Fatal("-CASEMAPPING reset folding to the default (should stay locked to ascii)")
	}
}
