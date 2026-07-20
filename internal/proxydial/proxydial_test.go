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
func TestParseRedactsCredentials(t *testing.T) {
	// Password chosen so it is unambiguous in the error text.
	_, err := Parse("socks5://alice:sup3rSecret@host:1080/nope") // path -> rejected
	if err == nil {
		t.Fatal("expected an error for a proxy URL with a path")
	}
	if strings.Contains(err.Error(), "sup3rSecret") || strings.Contains(err.Error(), "alice") {
		t.Fatalf("error leaked credentials: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>@host:1080") {
		t.Fatalf("error should retain the redacted host: %v", err)
	}

	// A value that fails url.Parse must not leak the raw credential via a
	// wrapped *url.Error (which embeds the original string).
	if _, err := Parse("socks5://bob:s3cr3t%zz@host:1080"); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("invalid-escape URL leaked or did not error: %v", err)
	}

	// A scheme-less credential form (bypasses the scheme masker's "://" split)
	// must still be redacted in the error.
	if _, err := Parse("carol:hunter2@host:1080"); err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "carol") {
		t.Fatalf("scheme-less URL leaked credentials: %v", err)
	}

	// A password containing '@' (multiple '@' in the authority): Go uses the
	// LAST '@' as the userinfo delimiter, so masking must reach it.
	if _, err := Parse("socks5://u:first@SECRET@host:1080/path"); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("multi-@ URL leaked credentials: %v", err)
	}
}

func TestParse(t *testing.T) {
	ok := []string{
		"socks5://127.0.0.1:1080",
		"socks5h://tor:9050",
		"socks5://alice:pw@10.0.0.1:1080",
		"http://user:pass@proxy.example:3128",
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
		// RFC 1929 length fields are one byte: >255-byte SOCKS creds cannot
		// be encoded and previously failed only after dialing the proxy.
		"socks5://" + strings.Repeat("u", 256) + ":pw@host:1080",
		"socks5://user:" + strings.Repeat("p", 256) + "@host:1080",
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
