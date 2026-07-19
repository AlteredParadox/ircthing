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

package netconf

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		errSub string // empty = must parse
	}{
		{"minimal", `{"addr": "irc.x.net:6697", "tls": true, "nick": "me"}`, ""},
		{"missing addr", `{"nick": "me"}`, "addr is required"},
		{"missing nick", `{"addr": "a:1"}`, "nick is required"},
		{"unknown field", `{"addr": "a:1", "nick": "me", "nickk": "typo"}`, "nickk"},
		{"malformed", `{`, "unexpected"},
		{"trailing document", `{"addr": "a:1", "nick": "me"} {"oops": 1}`, "trailing data"},
		{"CRLF in pass", `{"addr": "a:1", "nick": "me", "pass": "x\r\nOPER a b"}`, "CR, LF, or NUL"},
		{"newline in realname", `{"addr": "a:1", "nick": "me", "realname": "a\nb"}`, "CR, LF, or NUL"},
		{"NUL in username", `{"addr": "a:1", "nick": "me", "username": "a\u0000b"}`, "CR, LF, or NUL"},
		{"space in channel", `{"addr": "a:1", "nick": "me", "channels": ["#a b"]}`, "spaces, CR, LF"},
		{"space in nick", `{"addr": "a:1", "nick": "John Doe"}`, "nick must not contain spaces"},
		{"space in username", `{"addr": "a:1", "nick": "me", "username": "John Doe"}`, "username must not contain spaces"},
		{"CRLF in sasl password", `{"addr": "a:1", "nick": "me", "sasl": {"login": "u", "password": "p\r\nx"}}`, "CR, LF, or NUL"},
		{"EXTERNAL without keypair", `{"addr": "a:1", "nick": "me", "sasl": {"mechanism": "EXTERNAL"}}`, "cert_file and key_file"},
		{"EXTERNAL missing key", `{"addr": "a:1", "nick": "me", "sasl": {"mechanism": "EXTERNAL", "cert_file": "/c.pem"}}`, "cert_file and key_file"},
		{"EXTERNAL with keypair", `{"addr": "a:1", "nick": "me", "sasl": {"mechanism": "EXTERNAL", "cert_file": "/c.pem", "key_file": "/k.pem"}}`, ""},
		// Empty mechanism + no password auto-selects EXTERNAL, so it needs a keypair too.
		{"auto-EXTERNAL without keypair", `{"addr": "a:1", "nick": "me", "sasl": {}}`, "cert_file and key_file"},
		{"reserved network name", `{"name": "__proto__", "addr": "a:1", "nick": "me"}`, "reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := Parse([]byte(tc.in))
			if tc.errSub == "" {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if n.EffectiveName() != n.Addr {
					t.Fatalf("EffectiveName = %q, want addr", n.EffectiveName())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("err = %v, want containing %q", err, tc.errSub)
			}
		})
	}
}
