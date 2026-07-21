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
		// Interior spaces are legal: legacy configs/databases hold names
		// like "Libera Chat" and must keep working.
		{"network name with spaces", `{"name": "My Network", "addr": "a:1", "nick": "me"}`, ""},
		// An unnamed network is keyed by its addr; when that fallback hits
		// the name rule, the error must blame the missing name, not a
		// "name" field the user never set.
		{"reserved-prefix addr fallback", `{"addr": "__ircthing_invalid_row_1:6667", "nick": "me"}`, "no name and its addr"},
		{"network name control", `{"name": "bad\u001bname", "addr": "a:1", "nick": "me"}`, "control"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := Parse([]byte(tc.in))
			if tc.errSub == "" {
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if n.Name == "" && n.EffectiveName() != n.Addr {
					t.Fatalf("EffectiveName = %q, want addr", n.EffectiveName())
				}
				if n.Name != "" && n.EffectiveName() != n.Name {
					t.Fatalf("EffectiveName = %q, want name", n.EffectiveName())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("err = %v, want containing %q", err, tc.errSub)
			}
		})
	}
	long := &Network{Name: strings.Repeat("n", maxNetworkNameBytes+1), Addr: "a:1", Nick: "me"}
	if err := long.Validate(); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("long network name validation = %v", err)
	}
	// An unnamed network whose addr is a long-but-legal hostname must
	// validate: the name cap is sized to cover every addr ValidHostPort
	// accepts, so the fallback can never fail on length alone.
	unnamed := &Network{Addr: strings.Repeat("h", 130) + ":6667", Nick: "me"}
	if err := unnamed.Validate(); err != nil {
		t.Fatalf("unnamed network with 130-byte host = %v, want valid", err)
	}
	// A >300-byte addr never reaches the name fallback — ValidHostPort caps
	// hosts at 255 bytes and rejects it first, blaming the addr.
	huge := &Network{Addr: strings.Repeat("h", 301) + ":6667", Nick: "me"}
	if err := huge.Validate(); err == nil || !strings.Contains(err.Error(), "addr") {
		t.Fatalf("oversized addr = %v, want addr error", err)
	}
}
