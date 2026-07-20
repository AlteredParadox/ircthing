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

package proxydial

import (
	"strings"
	"testing"
)

// A rejected proxy URL must not leak its credentials in the error message.
func TestParseNeverLeaksCredentials(t *testing.T) {
	// Parse errors are fixed strings — no redaction heuristic to trust. Every
	// one of these must error WITHOUT any credential substring reaching the
	// message, including the slash-before-@ form that defeats an authority-only
	// redactor.
	cases := []string{
		"socks5://alice:sup3rSecret@host:1080/nope", // path -> rejected
		"socks5://bob:s3cr3t%zz@host:1080",          // fails url.Parse
		"carol:hunter2@host:1080",                   // scheme-less
		"socks5://u:first@SECRET@host:1080/path",    // multi-@
		"socks5://alice:secret/path@proxy:1080",     // '@' after first '/' (redactor blind spot)
	}
	secrets := []string{"sup3rSecret", "alice", "s3cr3t", "hunter2", "carol", "SECRET", "secret"}
	for _, s := range cases {
		_, err := Parse(s)
		if err == nil {
			t.Errorf("Parse(%q) = nil, want error", s)
			continue
		}
		for _, sec := range secrets {
			if strings.Contains(err.Error(), sec) {
				t.Errorf("Parse(%q) leaked %q: %v", s, sec, err)
			}
		}
	}
}

func TestParse(t *testing.T) {
	ok := []string{
		"socks5://127.0.0.1:1080",
		"socks5h://tor:9050",
		"socks5://alice:pw@10.0.0.1:1080",
		"http://user:pass@proxy.example:3128",
		"http://user:@proxy.example:3128",                       // HTTP Basic allows an empty password
		"socks5://a:" + strings.Repeat("p", 255) + "@host:1080", // 255-byte SOCKS pass is the max
	}
	for _, s := range ok {
		if _, err := Parse(s); err != nil {
			t.Errorf("Parse(%q) = %v, want ok", s, err)
		}
	}
	bad := []string{
		"socks5://noport",         // missing port
		"ftp://host:1080",         // wrong scheme
		"socks5://host:1080/path", // unexpected path
		"://bad",                  // unparseable
		"",                        // empty
		"socks5://:1080",          // empty hostname — impossible target
		"socks5://host:0",         // port below range
		"socks5://host:65536",     // port above range
		// RFC 1929: one-byte length fields, both 1-255 octets.
		"socks5://" + strings.Repeat("u", 256) + ":pw@host:1080", // username too long
		"socks5://user:" + strings.Repeat("p", 256) + "@host:1080", // password too long
		"socks5://user@host:1080",                                  // SOCKS needs a password too
		"socks5://user:@host:1080",                                 // empty SOCKS password
		"socks5://:pw@host:1080",                                   // empty SOCKS username
		// RFC 7617: HTTP Basic can't encode a control char in either field.
		// (A ':' is legal in the PASSWORD — only the user-id forbids it — so
		// that is not tested here as an error.)
		"http://a\x01b:pw@host:3128",   // control char in HTTP username
		"http://user:p\x7fw@host:3128", // DEL in HTTP password
	}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = nil, want error", s)
		}
	}
}

func TestCredsOverCleartext(t *testing.T) {
	cases := []struct {
		in   string
		warn bool
	}{
		{"socks5://user:pass@proxy.example.com:1080", true}, // remote + creds
		{"socks5://user:pass@203.0.113.9:1080", true},
		{"http://u:p@proxy.example:3128", true},
		{"socks5://user:pass@127.0.0.1:9050", false}, // loopback
		{"socks5://user:pass@localhost:9050", false},
		{"socks5://user:pass@[::1]:9050", false},
		{"socks5://proxy.example.com:1080", false}, // no creds
		{"", false},
	}
	for _, tc := range cases {
		if got := CredsOverCleartext(tc.in); got != tc.warn {
			t.Errorf("CredsOverCleartext(%q) = %v, want %v", tc.in, got, tc.warn)
		}
	}
}
