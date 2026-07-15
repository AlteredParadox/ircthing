package irc

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	ircv4 "gopkg.in/irc.v4"
)

// wire renders messages as "COMMAND param param ..." for comparison,
// ignoring serialization details like the trailing-parameter colon.
func wire(msgs []*ircv4.Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, strings.Join(append([]string{m.Command}, m.Params...), " "))
	}
	return out
}

// parseWant parses expected lines the same way so both sides normalize.
func parseWant(t *testing.T, lines []string) []string {
	t.Helper()
	var out []string
	for _, l := range lines {
		m, err := ircv4.ParseMessage(l)
		if err != nil {
			t.Fatalf("bad expected line %q: %v", l, err)
		}
		out = append(out, strings.Join(append([]string{m.Command}, m.Params...), " "))
	}
	return out
}

func b64(login, pass string) string {
	return base64.StdEncoding.EncodeToString(saslPlain("", login, pass))
}

func TestHandshake(t *testing.T) {
	baseCfg := Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true}
	saslCfg := baseCfg
	saslCfg.SASL = &SASLPlain{Login: "AlteredParadox", Password: "sesame"}
	authLine := "AUTHENTICATE " + b64("AlteredParadox", "sesame")

	type step struct {
		in     string   // line "from the server"
		want   []string // expected client responses
		done   bool
		errSub string // non-empty: handle must fail and the error contain this
	}
	cases := []struct {
		name      string
		cfg       Config
		wantStart []string
		steps     []step
		wantNick  string // checked at the end if non-empty
	}{
		{
			name:      "minimal registration without SASL",
			cfg:       baseCfg,
			wantStart: []string{"CAP LS 302", "NICK AlteredParadox", "USER AlteredParadox 0 * AlteredParadox"},
			steps: []step{
				{in: "CAP * LS :multi-prefix server-time", want: []string{"CAP END"}},
				{in: ":irc.test 001 AlteredParadox :Welcome to the test network", done: true},
			},
			wantNick: "AlteredParadox",
		},
		{
			name: "PASS and explicit username/realname",
			cfg: func() Config {
				c := baseCfg
				c.Pass = "hunter2"
				c.Username = "u"
				c.Realname = "Real Name"
				return c
			}(),
			wantStart: []string{"CAP LS 302", "PASS hunter2", "NICK AlteredParadox", "USER u 0 * :Real Name"},
			steps: []step{
				{in: "CAP * LS :sasl", want: []string{"CAP END"}},
				{in: ":irc.test 001 AlteredParadox :Welcome", done: true},
			},
		},
		{
			name:      "SASL PLAIN happy path",
			cfg:       saslCfg,
			wantStart: []string{"CAP LS 302", "NICK AlteredParadox", "USER AlteredParadox 0 * AlteredParadox"},
			steps: []step{
				{in: "CAP * LS :multi-prefix sasl=PLAIN,EXTERNAL server-time", want: []string{"CAP REQ sasl"}},
				{in: ":irc.test CAP AlteredParadox ACK :sasl", want: []string{"AUTHENTICATE PLAIN"}},
				{in: "AUTHENTICATE +", want: []string{authLine}},
				{in: ":irc.test 900 AlteredParadox AlteredParadox!u@h AlteredParadox :You are now logged in as AlteredParadox"},
				{in: ":irc.test 903 AlteredParadox :SASL authentication successful", want: []string{"CAP END"}},
				{in: ":irc.test 001 AlteredParadox :Welcome", done: true},
			},
		},
		{
			name: "SASL with multiline CAP LS",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS * :multi-prefix server-time echo-message"},
				{in: "CAP * LS * :batch labeled-response"},
				{in: "CAP * LS :sasl=PLAIN account-tag", want: []string{"CAP REQ sasl"}},
				{in: "CAP * ACK :sasl", want: []string{"AUTHENTICATE PLAIN"}},
			},
		},
		{
			name: "SASL cap without a mechanism list is attempted",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS :sasl", want: []string{"CAP REQ sasl"}},
			},
		},
		{
			name: "SASL not offered fails",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS :multi-prefix server-time", errSub: "does not offer the sasl capability"},
			},
		},
		{
			name: "SASL PLAIN not in mechanism list fails",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS :sasl=EXTERNAL,SCRAM-SHA-256", errSub: "PLAIN not offered"},
			},
		},
		{
			name: "CAP NAK for sasl fails",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS :sasl", want: []string{"CAP REQ sasl"}},
				{in: "CAP * NAK :sasl", errSub: "refused CAP REQ"},
			},
		},
		{
			name: "SASL failure numeric 904",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS :sasl", want: []string{"CAP REQ sasl"}},
				{in: "CAP * ACK :sasl", want: []string{"AUTHENTICATE PLAIN"}},
				{in: "AUTHENTICATE +", want: []string{authLine}},
				{in: ":irc.test 908 AlteredParadox PLAIN,EXTERNAL :are available mechanisms"},
				{in: ":irc.test 904 AlteredParadox :SASL authentication failed", errSub: "904"},
			},
		},
		{
			name: "SASL abort numerics fail",
			cfg:  saslCfg,
			steps: []step{
				{in: ":irc.test 906 AlteredParadox :SASL authentication aborted", errSub: "906"},
			},
		},
		{
			name: "nick locked fails",
			cfg:  saslCfg,
			steps: []step{
				{in: ":irc.test 902 AlteredParadox :You must use a nick assigned to you", errSub: "902"},
			},
		},
		{
			name: "unexpected non-empty PLAIN challenge fails",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS :sasl", want: []string{"CAP REQ sasl"}},
				{in: "CAP * ACK :sasl", want: []string{"AUTHENTICATE PLAIN"}},
				{in: "AUTHENTICATE c29tZWNoYWxsZW5nZQ==", errSub: "unexpected SASL challenge"},
			},
		},
		{
			name: "registration completing before SASL fails closed",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS :sasl", want: []string{"CAP REQ sasl"}},
				{in: ":irc.test 001 AlteredParadox :Welcome", errSub: "before SASL"},
			},
		},
		{
			name: "nick in use falls back with underscores",
			cfg:  baseCfg,
			steps: []step{
				{in: ":irc.test 433 * AlteredParadox :Nickname is already in use", want: []string{"NICK AlteredParadox_"}},
				{in: ":irc.test 433 * AlteredParadox_ :Nickname is already in use", want: []string{"NICK AlteredParadox__"}},
				{in: "CAP * LS :multi-prefix", want: []string{"CAP END"}},
				{in: ":irc.test 001 AlteredParadox__ :Welcome", done: true},
			},
			wantNick: "AlteredParadox__",
		},
		{
			name: "nick fallbacks exhausted fails",
			cfg:  baseCfg,
			steps: []step{
				{in: ":irc.test 433 * AlteredParadox :in use", want: []string{"NICK AlteredParadox_"}},
				{in: ":irc.test 433 * AlteredParadox_ :in use", want: []string{"NICK AlteredParadox__"}},
				{in: ":irc.test 433 * AlteredParadox__ :in use", want: []string{"NICK AlteredParadox___"}},
				{in: ":irc.test 433 * AlteredParadox___ :in use", errSub: "all fallbacks"},
			},
		},
		{
			name: "erroneous nickname fails",
			cfg:  baseCfg,
			steps: []step{
				{in: ":irc.test 432 * AlteredParadox :Erroneous nickname", errSub: "rejected nickname"},
			},
		},
		{
			name: "server password rejected fails",
			cfg:  baseCfg,
			steps: []step{
				{in: ":irc.test 464 * :Password incorrect", errSub: "464"},
			},
		},
		{
			name: "PING during registration is answered",
			cfg:  baseCfg,
			steps: []step{
				{in: "PING :12345", want: []string{"PONG 12345"}},
				{in: "CAP * LS :multi-prefix", want: []string{"CAP END"}},
				{in: ":irc.test 001 AlteredParadox :Welcome", done: true},
			},
		},
		{
			name: "ERROR during registration fails",
			cfg:  baseCfg,
			steps: []step{
				{in: "ERROR :Closing Link: banned", errSub: "banned"},
			},
		},
		{
			name: "server without CAP support registers directly",
			cfg:  baseCfg,
			steps: []step{
				{in: ":irc.test 421 AlteredParadox CAP :Unknown command"},
				{in: ":irc.test 001 AlteredParadox :Welcome", done: true},
			},
		},
		{
			name: "001 with truncated nick is authoritative",
			cfg:  baseCfg,
			steps: []step{
				{in: "CAP * LS :multi-prefix", want: []string{"CAP END"}},
				{in: ":irc.test 001 AlteredParadoxtruncated :Welcome", done: true},
			},
			wantNick: "AlteredParadoxtruncated",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHandshake(&tc.cfg)
			if tc.wantStart != nil {
				if got, want := wire(hs.start()), parseWant(t, tc.wantStart); !reflect.DeepEqual(got, want) {
					t.Fatalf("start():\n got %q\nwant %q", got, want)
				}
			} else {
				hs.start()
			}
			for i, st := range tc.steps {
				in, err := ircv4.ParseMessage(st.in)
				if err != nil {
					t.Fatalf("step %d: bad input line %q: %v", i, st.in, err)
				}
				out, done, err := hs.handle(in)
				if st.errSub != "" {
					if err == nil || !strings.Contains(err.Error(), st.errSub) {
						t.Fatalf("step %d (%q): err = %v, want containing %q", i, st.in, err, st.errSub)
					}
					return
				}
				if err != nil {
					t.Fatalf("step %d (%q): unexpected error: %v", i, st.in, err)
				}
				if got, want := wire(out), parseWant(t, st.want); !reflect.DeepEqual(got, want) {
					t.Fatalf("step %d (%q):\n got %q\nwant %q", i, st.in, got, want)
				}
				if done != st.done {
					t.Fatalf("step %d (%q): done = %v, want %v", i, st.in, done, st.done)
				}
			}
			if tc.wantNick != "" && hs.nick != tc.wantNick {
				t.Fatalf("final nick = %q, want %q", hs.nick, tc.wantNick)
			}
		})
	}
}

func TestParseCapList(t *testing.T) {
	cases := []struct {
		name string
		in   []string // successive CAP LS list params
		want map[string]string
	}{
		{
			name: "plain caps",
			in:   []string{"multi-prefix server-time"},
			want: map[string]string{"multi-prefix": "", "server-time": ""},
		},
		{
			name: "values",
			in:   []string{"sasl=PLAIN,EXTERNAL draft/languages=13,en"},
			want: map[string]string{"sasl": "PLAIN,EXTERNAL", "draft/languages": "13,en"},
		},
		{
			name: "accumulates across lines",
			in:   []string{"multi-prefix", "sasl=PLAIN batch"},
			want: map[string]string{"multi-prefix": "", "sasl": "PLAIN", "batch": ""},
		},
		{
			name: "later value wins",
			in:   []string{"sasl", "sasl=PLAIN"},
			want: map[string]string{"sasl": "PLAIN"},
		},
		{
			name: "empty list",
			in:   []string{""},
			want: map[string]string{},
		},
		{
			name: "surrounding whitespace",
			in:   []string{"  multi-prefix   sasl=PLAIN "},
			want: map[string]string{"multi-prefix": "", "sasl": "PLAIN"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := make(map[string]string)
			for _, line := range tc.in {
				parseCapList(line, got)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMechListed(t *testing.T) {
	cases := []struct {
		list, mech string
		want       bool
	}{
		{"PLAIN", "PLAIN", true},
		{"PLAIN,EXTERNAL", "PLAIN", true},
		{"EXTERNAL,PLAIN", "PLAIN", true},
		{"plain", "PLAIN", true}, // case-insensitive
		{"EXTERNAL,SCRAM-SHA-256", "PLAIN", false},
		{"PLAINX", "PLAIN", false},
		{"", "PLAIN", false},
	}
	for _, tc := range cases {
		if got := mechListed(tc.list, tc.mech); got != tc.want {
			t.Errorf("mechListed(%q, %q) = %v, want %v", tc.list, tc.mech, got, tc.want)
		}
	}
}
