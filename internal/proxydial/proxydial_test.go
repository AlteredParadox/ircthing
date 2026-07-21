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
	"bytes"
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

// scriptedConn supplies a fixed peer response while accepting writes. It lets
// framing tests distinguish a complete deterministic rejection from a partial
// network read without coordinating a second net.Pipe goroutine.
type scriptedConn struct{ bytes.Reader }

func newScriptedConn(in []byte) *scriptedConn {
	return &scriptedConn{Reader: *bytes.NewReader(in)}
}

func (c *scriptedConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *scriptedConn) Close() error                     { return nil }
func (c *scriptedConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *scriptedConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *scriptedConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedConn) SetWriteDeadline(time.Time) error { return nil }

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
		"socks5://alice:secret:1080",                // extra colon: Hostname() mis-splits to "alice:secret"
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
		"socks5://" + strings.Repeat("u", 256) + ":pw@host:1080",   // username too long
		"socks5://user:" + strings.Repeat("p", 256) + "@host:1080", // password too long
		"socks5://user@host:1080",                                  // SOCKS needs a password too
		"socks5://user:@host:1080",                                 // empty SOCKS password
		"socks5://:pw@host:1080",                                   // empty SOCKS username
		// RFC 7617: HTTP Basic can't encode a control char in either field.
		// (A ':' is legal in the PASSWORD — only the user-id forbids it — so
		// that is not tested here as an error.)
		"http://a\x01b:pw@host:3128",   // control char in HTTP username
		"http://user:p\x7fw@host:3128", // DEL in HTTP password
		"socks5://host:+6667",          // signed port — strconv.Atoi would accept it
		"socks5://host:66 66",          // whitespace in port
	}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = nil, want error", s)
		}
	}
}

func TestParsePort(t *testing.T) {
	good := map[string]int{"1": 1, "6667": 6667, "65535": 65535}
	for s, want := range good {
		if got, ok := ParsePort(s); !ok || got != want {
			t.Errorf("ParsePort(%q) = %d,%v, want %d,true", s, got, ok, want)
		}
	}
	// Rejected: signed, zero, out-of-range, non-digit, unicode digit, empty.
	for _, s := range []string{"", "0", "+6667", "-1", "65536", "99999", "6a", " 66", "6 6", "६६"} {
		if _, ok := ParsePort(s); ok {
			t.Errorf("ParsePort(%q) = ok, want rejected", s)
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

// Dial must enforce Parse's structural invariants itself: a directly-
// constructed or mutated URL (unknown scheme, hostless/bad-port authority, bad
// credentials) or a malformed target must be rejected BEFORE any socket opens.
func TestDialRejectsInvalidBeforeConnecting(t *testing.T) {
	bad := []struct {
		name   string
		proxy  *url.URL
		target string
	}{
		{"unknown scheme", &url.URL{Scheme: "gopher", Host: "127.0.0.1:1"}, "example.com:6667"},
		{"empty host", &url.URL{Scheme: "socks5", Host: ":1080"}, "example.com:6667"},
		{"bad port", &url.URL{Scheme: "socks5", Host: "h:0"}, "example.com:6667"},
		{"userinfo smuggled into raw Host", &url.URL{Scheme: "socks5", Host: "alice:secret@h:1080"}, "example.com:6667"},
		{"socks creds missing password", &url.URL{Scheme: "socks5", Host: "h:1080", User: url.User("bob")}, "example.com:6667"},
		{"bad target framing", &url.URL{Scheme: "socks5", Host: "h:1080"}, "no-port"},
		{"bad target port", &url.URL{Scheme: "socks5", Host: "h:1080"}, "example.com:0"},
		{"target host with CRLF injection", &url.URL{Scheme: "http", Host: "h:1080"}, "evil\r\nHost: x:6667"},
		{"target host with space", &url.URL{Scheme: "http", Host: "h:1080"}, "a b:6667"},
		{"overlong target host", &url.URL{Scheme: "socks5", Host: "h:1080"}, strings.Repeat("x", 256) + ":6667"},
	}
	for _, tc := range bad {
		_, err := Dial(context.Background(), tc.proxy, tc.target, time.Second)
		if err == nil {
			t.Errorf("%s: Dial = nil, want a pre-dial validation error", tc.name)
			continue
		}
		// Must be a typed config error (media classifies it as PERMANENT) and
		// must never echo the smuggled secret.
		if !errors.Is(err, ErrProxyConfig) {
			t.Errorf("%s: err = %v, want ErrProxyConfig", tc.name, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: error leaked a credential: %v", tc.name, err)
		}
	}
}

func TestCompleteProxyFramingErrorsArePermanent(t *testing.T) {
	tests := []struct {
		name string
		call func() error
	}{
		{"SOCKS greeting version", func() error {
			return socks5Negotiate(newScriptedConn([]byte{4, 0}), nil)
		}},
		{"SOCKS reply address type", func() error {
			return socks5ReadReply(newScriptedConn([]byte{5, 0, 0, 9}))
		}},
		{"SOCKS reply code", func() error {
			return socks5ReadReply(newScriptedConn([]byte{5, 0xff, 0, 1}))
		}},
		{"HTTP CONNECT header cap", func() error {
			return httpConnect(newScriptedConn(bytes.Repeat([]byte{'x'}, 8200)), nil, "example.com:443")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrProxyProtocol) {
				t.Fatalf("err = %v, want ErrProxyProtocol", err)
			}
		})
	}
}

func TestPartialProxyResponsesRemainTransientIOErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"SOCKS greeting", func() error { return socks5Negotiate(newScriptedConn([]byte{5}), nil) }},
		{"SOCKS reply", func() error { return socks5ReadReply(newScriptedConn([]byte{5, 0})) }},
		{"HTTP CONNECT", func() error { return httpConnect(newScriptedConn([]byte("HTTP/1.1 200")), nil, "example.com:443") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil || errors.Is(err, ErrProxyProtocol) {
				t.Fatalf("err = %v, want an unclassified partial-I/O failure", err)
			}
		})
	}
}
