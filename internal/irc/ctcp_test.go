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
	"strings"
	"testing"

	ircv4 "gopkg.in/irc.v4"
)

func TestCTCPReply(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" = no reply; substring match on the reply text
	}{
		{"version", ":pal!u@h PRIVMSG AlteredParadox :\x01VERSION\x01", "\x01VERSION ircthing\x01"},
		{"version unclosed", ":pal!u@h PRIVMSG AlteredParadox :\x01VERSION", "\x01VERSION ircthing\x01"},
		{"version lowercase", ":pal!u@h PRIVMSG AlteredParadox :\x01version\x01", "\x01VERSION ircthing\x01"},
		{"ping echoes token", ":pal!u@h PRIVMSG AlteredParadox :\x01PING 12345 67\x01", "\x01PING 12345 67\x01"},
		{"ping bare", ":pal!u@h PRIVMSG AlteredParadox :\x01PING\x01", "\x01PING\x01"},
		{"clientinfo", ":pal!u@h PRIVMSG AlteredParadox :\x01CLIENTINFO\x01", "\x01CLIENTINFO ACTION CLIENTINFO PING TIME VERSION\x01"},
		{"time", ":pal!u@h PRIVMSG AlteredParadox :\x01TIME\x01", "\x01TIME "},
		{"action is a message, not a query", ":pal!u@h PRIVMSG AlteredParadox :\x01ACTION waves\x01", ""},
		{"dcc is out of scope", ":pal!u@h PRIVMSG AlteredParadox :\x01DCC SEND f 1 2 3\x01", ""},
		{"unknown query", ":pal!u@h PRIVMSG AlteredParadox :\x01FINGER\x01", ""},
		{"plain message", ":pal!u@h PRIVMSG AlteredParadox :hello", ""},
		{"no sender prefix", "PRIVMSG AlteredParadox :\x01VERSION\x01", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ctcpReply(ircv4.MustParseMessage(tc.in))
			if tc.want == "" {
				if got != nil {
					t.Fatalf("reply = %q, want none", got.String())
				}
				return
			}
			if got == nil {
				t.Fatal("no reply")
			}
			if got.Command != "NOTICE" || got.Param(0) != "pal" {
				t.Fatalf("reply = %q, want NOTICE to pal", got.String())
			}
			if !strings.Contains(got.Trailing(), strings.TrimSuffix(tc.want, "\x01")) {
				t.Fatalf("reply body = %q, want containing %q", got.Trailing(), tc.want)
			}
		})
	}
}

// An over-length CTCP PING token is truncated so the auto-reply can
// never exceed the line limit (which would fatally tear the connection
// down — a remote reconnect-loop DoS).
func TestCTCPPingTokenCapped(t *testing.T) {
	huge := strings.Repeat("A", 4000)
	reply := ctcpReply(ircv4.MustParseMessage(":pal!u@h PRIVMSG AlteredParadox :\x01PING " + huge + "\x01"))
	if reply == nil {
		t.Fatal("no reply")
	}
	// The serialized NOTICE must fit the default line limit.
	if err := checkLineLen(reply, defaultLineLen); err != nil {
		t.Fatalf("capped reply still over-length: %v", err)
	}
	body := strings.Trim(reply.Trailing(), "\x01")
	if len(body) > len("PING ")+maxCTCPPingToken {
		t.Fatalf("token not capped: %d bytes", len(body))
	}
}
