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
