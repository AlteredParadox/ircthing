package irc

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestSCRAMVector checks the client against the RFC 7677 §3 worked
// example for SCRAM-SHA-256 (user "user", password "pencil").
func TestSCRAMVector(t *testing.T) {
	c := newSCRAM("", "user", "pencil")
	c.nonce = func() string { return "rOprNGfwEbeRWgbNEkqO" }

	first, err := c.respond(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(first); got != "n,,n=user,r=rOprNGfwEbeRWgbNEkqO" {
		t.Fatalf("client-first = %q", got)
	}

	serverFirst := "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	final, err := c.respond([]byte(serverFirst))
	if err != nil {
		t.Fatal(err)
	}
	want := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0," +
		"p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if got := string(final); got != want {
		t.Fatalf("client-final:\n got %q\nwant %q", got, want)
	}

	// The server signature from the RFC verifies, completing the exchange.
	serverFinal := "v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
	ack, err := c.respond([]byte(serverFinal))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(ack) != 0 {
		t.Fatalf("final ack = %q, want empty", ack)
	}
}

func TestSCRAMRejectsBadServer(t *testing.T) {
	mk := func() *scramClient {
		c := newSCRAM("", "user", "pencil")
		c.nonce = func() string { return "rOprNGfwEbeRWgbNEkqO" }
		c.respond(nil) // client-first
		return c
	}

	// Server nonce that doesn't extend the client nonce is rejected.
	c := mk()
	if _, err := c.respond([]byte("r=different,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096")); err == nil ||
		!strings.Contains(err.Error(), "nonce") {
		t.Fatalf("bad nonce err = %v", err)
	}

	// A wrong server signature (MITM) fails verification.
	c = mk()
	c.respond([]byte("r=rOprNGfwEbeRWgbNEkqO-srv,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"))
	if _, err := c.respond([]byte("v=" + base64.StdEncoding.EncodeToString([]byte("wrong-signature-32-bytes-padded!")))); err == nil ||
		!strings.Contains(err.Error(), "signature mismatch") {
		t.Fatalf("bad sig err = %v", err)
	}

	// A server error attribute surfaces.
	c = mk()
	c.respond([]byte("r=rOprNGfwEbeRWgbNEkqO-x,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"))
	if _, err := c.respond([]byte("e=invalid-proof")); err == nil || !strings.Contains(err.Error(), "invalid-proof") {
		t.Fatalf("server error not surfaced: %v", err)
	}
}

func TestSCRAMEscaping(t *testing.T) {
	c := newSCRAM("", "a,b=c", "pw")
	c.nonce = func() string { return "NONCE" }
	first, _ := c.respond(nil)
	if got := string(first); got != "n,,n=a=2Cb=3Dc,r=NONCE" {
		t.Fatalf("username escaping = %q", got)
	}
}

func TestNewMech(t *testing.T) {
	cases := []struct {
		name     string
		cfg      SASLConfig
		offered  string
		wantName string
		wantErr  bool
	}{
		{"explicit plain", SASLConfig{Mechanism: "PLAIN", Login: "u", Password: "p"}, "PLAIN,EXTERNAL", "PLAIN", false},
		{"explicit scram", SASLConfig{Mechanism: "SCRAM-SHA-256", Login: "u", Password: "p"}, "SCRAM-SHA-256", "SCRAM-SHA-256", false},
		{"explicit external", SASLConfig{Mechanism: "EXTERNAL"}, "EXTERNAL", "EXTERNAL", false},
		{"auto prefers scram when offered", SASLConfig{Login: "u", Password: "p"}, "PLAIN,SCRAM-SHA-256", "SCRAM-SHA-256", false},
		{"auto falls back to plain", SASLConfig{Login: "u", Password: "p"}, "PLAIN", "PLAIN", false},
		{"auto external without password", SASLConfig{}, "EXTERNAL,PLAIN", "EXTERNAL", false},
		{"unoffered mechanism errors", SASLConfig{Mechanism: "SCRAM-SHA-256", Login: "u", Password: "p"}, "PLAIN,EXTERNAL", "", true},
		{"empty offered list trusts config", SASLConfig{Mechanism: "PLAIN", Login: "u", Password: "p"}, "", "PLAIN", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := newMech(&tc.cfg, tc.offered)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got mech %v", m)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.Name() != tc.wantName {
				t.Fatalf("mech = %q, want %q", m.Name(), tc.wantName)
			}
		})
	}
}
