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
		{"CRLF in sasl password", `{"addr": "a:1", "nick": "me", "sasl": {"login": "u", "password": "p\r\nx"}}`, "CR, LF, or NUL"},
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
