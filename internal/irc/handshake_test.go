package irc

import (
	"crypto/sha256"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/pbkdf2"
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

// b64 builds the expected PLAIN payload; the authzid defaults to the
// login (see newMech).
func b64(login, pass string) string {
	return base64.StdEncoding.EncodeToString(saslPlain(login, login, pass))
}

func TestHandshake(t *testing.T) {
	baseCfg := Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true}
	saslCfg := baseCfg
	saslCfg.SASL = &SASLConfig{Mechanism: "PLAIN", Login: "AlteredParadox", Password: "sesame"}
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
				{in: "CAP * LS :multi-prefix server-time", want: []string{"CAP REQ :multi-prefix server-time"}},
				{in: "CAP AlteredParadox ACK :multi-prefix server-time", want: []string{"CAP END"}},
				{in: ":irc.test 001 AlteredParadox :Welcome to the test network", done: true},
			},
			wantNick: "AlteredParadox",
		},
		{
			name: "no wanted caps offered goes straight to CAP END",
			cfg:  baseCfg,
			steps: []step{
				{in: "CAP * LS :example/vendor-thing", want: []string{"CAP END"}},
				{in: ":irc.test 001 AlteredParadox :Welcome", done: true},
			},
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
			wantStart: []string{"CAP LS 302", "NICK AlteredParadox", "USER u 0 * :Real Name"},
			steps: []step{
				// PASS is deferred until the CAP LS reply (post-STS-check).
				{in: "CAP * LS :sasl", want: []string{"PASS hunter2", "CAP END"}},
				{in: ":irc.test 001 AlteredParadox :Welcome", done: true},
			},
		},
		{
			name:      "SASL PLAIN happy path",
			cfg:       saslCfg,
			wantStart: []string{"CAP LS 302", "NICK AlteredParadox", "USER AlteredParadox 0 * AlteredParadox"},
			steps: []step{
				{in: "CAP * LS :multi-prefix sasl=PLAIN,EXTERNAL server-time", want: []string{"CAP REQ :multi-prefix sasl server-time"}},
				{in: ":irc.test CAP AlteredParadox ACK :multi-prefix sasl server-time", want: []string{"AUTHENTICATE PLAIN"}},
				{in: "AUTHENTICATE +", want: []string{authLine}},
				{in: ":irc.test 900 AlteredParadox AlteredParadox!u@h AlteredParadox :You are now logged in as AlteredParadox"},
				{in: ":irc.test 903 AlteredParadox :SASL authentication successful", want: []string{"CAP END"}},
				{in: ":irc.test 001 AlteredParadox :Welcome", done: true},
			},
		},
		{
			name: "SASL EXTERNAL sends an empty response",
			cfg: func() Config {
				c := baseCfg
				c.SASL = &SASLConfig{Mechanism: "EXTERNAL"}
				return c
			}(),
			steps: []step{
				{in: "CAP * LS :sasl=EXTERNAL", want: []string{"CAP REQ sasl"}},
				{in: "CAP * ACK :sasl", want: []string{"AUTHENTICATE EXTERNAL"}},
				// EXTERNAL's authorization identity is empty -> "+".
				{in: "AUTHENTICATE +", want: []string{"AUTHENTICATE +"}},
				{in: ":irc.test 903 AlteredParadox :SASL successful", want: []string{"CAP END"}},
				{in: ":irc.test 001 AlteredParadox :Welcome", done: true},
			},
		},
		{
			name: "SASL with multiline CAP LS",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS * :multi-prefix server-time echo-message"},
				{in: "CAP * LS * :batch labeled-response"},
				{
					in:   "CAP * LS :sasl=PLAIN account-tag",
					want: []string{"CAP REQ :account-tag batch echo-message labeled-response multi-prefix sasl server-time"},
				},
				// A partial ACK still enables what it names.
				{in: "CAP * ACK :sasl", want: []string{"AUTHENTICATE PLAIN"}},
			},
		},
		{
			name: "NAK of the full set falls back to sasl alone",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS :batch sasl", want: []string{"CAP REQ :batch sasl"}},
				{in: "CAP * NAK :batch sasl", want: []string{"CAP REQ sasl"}},
				{in: "CAP * ACK :sasl", want: []string{"AUTHENTICATE PLAIN"}},
			},
		},
		{
			name: "NAK without sasl configured proceeds bare",
			cfg:  baseCfg,
			steps: []step{
				{in: "CAP * LS :batch", want: []string{"CAP REQ batch"}},
				{in: "CAP * NAK :batch", want: []string{"CAP END"}},
				{in: ":irc.test 001 AlteredParadox :Welcome", done: true},
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
			// The mechanism mismatch is only fatal after the ACK: we still
			// REQ sasl (the conventional client flow, asserted by irctest),
			// then part with a QUIT instead of authenticating.
			name: "SASL PLAIN not in mechanism list quits after ACK",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS :sasl=EXTERNAL,SCRAM-SHA-256", want: []string{"CAP REQ sasl"}},
				{in: "CAP * ACK :sasl", want: []string{"QUIT :SASL mechanism unavailable"}, errSub: "PLAIN not offered"},
			},
		},
		{
			name: "CAP NAK for sasl fails",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS :sasl", want: []string{"CAP REQ sasl"}},
				{in: "CAP * NAK :sasl", errSub: "refused the capability request"},
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
			// PLAIN is single-round; any server challenge (even a spurious
			// non-empty one) is answered with the credential payload.
			name: "PLAIN ignores the challenge content",
			cfg:  saslCfg,
			steps: []step{
				{in: "CAP * LS :sasl", want: []string{"CAP REQ sasl"}},
				{in: "CAP * ACK :sasl", want: []string{"AUTHENTICATE PLAIN"}},
				{in: "AUTHENTICATE c29tZWNoYWxsZW5nZQ==", want: []string{authLine}},
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
				{in: "CAP * LS :example/none", want: []string{"CAP END"}},
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
				{in: "CAP * LS :example/none", want: []string{"CAP END"}},
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
				{in: "CAP * LS :example/none", want: []string{"CAP END"}},
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
					// A failing step may still have parting words (QUIT,
					// AUTHENTICATE *) that must go to the wire.
					if st.want != nil {
						if got, want := wire(out), parseWant(t, st.want); !reflect.DeepEqual(got, want) {
							t.Fatalf("step %d (%q):\n got %q\nwant %q", i, st.in, got, want)
						}
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

// TestHandshakeSCRAMFlow drives a full SCRAM-SHA-256 exchange through the
// handshake, playing a cooperative server that computes a valid
// server-first and server-final from the client's actual nonce.
func TestHandshakeSCRAMFlow(t *testing.T) {
	cfg := Config{
		Addr: "irc.test:6697", Nick: "AlteredParadox", TLS: true,
		SASL: &SASLConfig{Mechanism: "SCRAM-SHA-256", Login: "AlteredParadox", Password: "pencil"},
	}
	hs := newHandshake(&cfg)
	hs.start()

	// respond feeds a line and returns the single AUTHENTICATE argument
	// the client sends back (decoded from base64).
	respond := func(line string) []byte {
		t.Helper()
		out, _, err := hs.handle(ircv4.MustParseMessage(line))
		if err != nil {
			t.Fatalf("handle %q: %v", line, err)
		}
		if len(out) != 1 || out[0].Command != "AUTHENTICATE" {
			t.Fatalf("handle %q -> %v", line, wire(out))
		}
		arg := out[0].Param(0)
		if arg == "+" {
			return nil
		}
		b, err := base64.StdEncoding.DecodeString(arg)
		if err != nil {
			t.Fatalf("client sent non-base64 %q", arg)
		}
		return b
	}

	// CAP negotiation up to the AUTHENTICATE SCRAM-SHA-256.
	if _, _, err := hs.handle(ircv4.MustParseMessage("CAP * LS :sasl=SCRAM-SHA-256")); err != nil {
		t.Fatal(err)
	}
	out, _, _ := hs.handle(ircv4.MustParseMessage("CAP AlteredParadox ACK :sasl"))
	if len(out) != 1 || out[0].String() != "AUTHENTICATE SCRAM-SHA-256" {
		t.Fatalf("ack -> %v", wire(out))
	}

	// Client-first.
	clientFirst := string(respond("AUTHENTICATE +"))
	bare := strings.TrimPrefix(clientFirst, "n,,")
	attrs, _ := parseScramAttrs(bare)
	cnonce := attrs["r"]
	if cnonce == "" || attrs["n"] != "AlteredParadox" {
		t.Fatalf("client-first = %q", clientFirst)
	}

	// Server-first: extend the nonce, pick a salt and iteration count.
	snonce := cnonce + "serverpart"
	salt := []byte("0123456789abcdef")
	iters := 4096
	serverFirst := "r=" + snonce + ",s=" + base64.StdEncoding.EncodeToString(salt) + ",i=4096"

	clientFinal := string(respond("AUTHENTICATE " + base64Encode(serverFirst)))
	// clientFinal is "c=biws,r=<snonce>,p=<proof>".
	if !strings.HasPrefix(clientFinal, "c=biws,r="+snonce+",p=") {
		t.Fatalf("client-final = %q", clientFinal)
	}

	// Compute the expected server signature and complete the exchange.
	saltedPassword := pbkdf2.Key([]byte("pencil"), salt, iters, sha256.Size, sha256.New)
	serverKey := scramHMAC(saltedPassword, []byte("Server Key"))
	clientFinalNoProof := "c=biws,r=" + snonce
	authMessage := bare + "," + serverFirst + "," + clientFinalNoProof
	serverSig := scramHMAC(serverKey, []byte(authMessage))
	serverFinal := "v=" + base64.StdEncoding.EncodeToString(serverSig)

	ack := respond("AUTHENTICATE " + base64Encode(serverFinal))
	if len(ack) != 0 {
		t.Fatalf("final client ack = %q, want empty", ack)
	}

	// 903 completes SASL; the handshake sends CAP END.
	end, _, _ := hs.handle(ircv4.MustParseMessage(":irc.test 903 AlteredParadox :ok"))
	if len(end) != 1 || end[0].String() != "CAP END" {
		t.Fatalf("after 903 -> %v", wire(end))
	}
}

func base64Encode(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestHandshakeRecordsEnabledCaps(t *testing.T) {
	cfg := Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true}
	hs := newHandshake(&cfg)
	hs.start()
	feed := func(line string) {
		t.Helper()
		if _, _, err := hs.handle(ircv4.MustParseMessage(line)); err != nil {
			t.Fatal(err)
		}
	}
	feed("CAP * LS :multi-prefix server-time away-notify example/none")
	feed("CAP AlteredParadox ACK :multi-prefix server-time away-notify")
	feed(":irc.test 001 AlteredParadox :Welcome")
	for _, c := range []string{"multi-prefix", "server-time", "away-notify"} {
		if !hs.enabled[c] {
			t.Errorf("cap %s not recorded as enabled", c)
		}
	}
	if hs.enabled["example/none"] {
		t.Error("unrequested cap recorded")
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

// On a secure link PASS is sent up front (no eavesdropper), which also
// reaches servers that never reply to CAP LS; on an insecure link it is
// deferred to the CAP LS reply (STS protection).
func TestHandshakePassSecureVsInsecure(t *testing.T) {
	cfg := Config{Addr: "x:1", Nick: "AlteredParadox", Pass: "s3cret", TLS: true}

	secure := newHandshake(&cfg)
	secure.secure = true
	got := wire(secure.start())
	found := false
	for _, l := range got {
		if l == "PASS s3cret" {
			found = true
		}
	}
	if !found {
		t.Fatalf("secure start() = %q, want PASS included", got)
	}

	insecure := newHandshake(&cfg)
	insecure.secure = false
	for _, l := range wire(insecure.start()) {
		if l == "PASS s3cret" {
			t.Fatal("insecure start() leaked PASS before the STS decision")
		}
	}
	// The deferred PASS comes with the CAP LS reply (no STS).
	out, _, err := insecure.handle(ircv4.MustParseMessage("CAP * LS :multi-prefix"))
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, l := range wire(out) {
		if l == "PASS s3cret" {
			found = true
		}
	}
	if !found {
		t.Fatalf("insecure CAP LS reply = %q, want deferred PASS", wire(out))
	}
}
