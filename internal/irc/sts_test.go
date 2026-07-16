package irc

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	ircv4 "gopkg.in/irc.v4"
)

func TestParseSTS(t *testing.T) {
	cases := []struct {
		in   string
		want stsValue
	}{
		{"port=6697", stsValue{port: 6697}},
		{"duration=300", stsValue{hasDuration: true, duration: 300 * time.Second}},
		{"duration=0", stsValue{hasDuration: true}},
		{"port=6697,duration=60", stsValue{port: 6697, hasDuration: true, duration: time.Minute}},
		// Unknown keys ignored; malformed values dropped.
		{"preload,port=6697,future=x", stsValue{port: 6697}},
		{"port=notaport", stsValue{}},
		{"port=0", stsValue{}},
		{"port=70000", stsValue{}},
		{"duration=-1", stsValue{}},
		{"", stsValue{}},
	}
	for _, tc := range cases {
		if got := parseSTS(tc.in); got != tc.want {
			t.Errorf("parseSTS(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestHandshakeSTS(t *testing.T) {
	cfg := Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true}
	ls := func(caps string) *ircv4.Message {
		return ircv4.MustParseMessage("CAP * LS :" + caps)
	}

	t.Run("insecure with port upgrades", func(t *testing.T) {
		hs := newHandshake(&cfg)
		hs.start()
		_, _, err := hs.handle(ls("sts=duration=300,port=6697 multi-prefix"))
		var up errSTSUpgrade
		if !errors.As(err, &up) || up.port != 6697 {
			t.Fatalf("err = %v, want errSTSUpgrade{6697}", err)
		}
	})

	t.Run("insecure without port is ignored", func(t *testing.T) {
		hs := newHandshake(&cfg)
		hs.start()
		out, _, err := hs.handle(ls("sts=duration=300 multi-prefix"))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(out) == 0 {
			t.Fatal("handshake did not continue")
		}
	})

	t.Run("secure records the duration and continues", func(t *testing.T) {
		hs := newHandshake(&cfg)
		hs.secure = true
		hs.start()
		out, _, err := hs.handle(ls("sts=duration=300,port=12345 multi-prefix"))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if hs.stsDuration == nil || *hs.stsDuration != 300*time.Second {
			t.Fatalf("stsDuration = %v, want 300s", hs.stsDuration)
		}
		// The port key is ignored on secure connections: no upgrade error,
		// registration proceeds.
		if len(out) == 0 {
			t.Fatal("handshake did not continue")
		}
	})

	t.Run("sts is never requested", func(t *testing.T) {
		hs := newHandshake(&cfg)
		hs.secure = true
		hs.start()
		out, _, err := hs.handle(ls("sts=duration=300 multi-prefix"))
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range out {
			if msg.Command == "CAP" && len(msg.Params) >= 2 && msg.Params[0] == "REQ" {
				if slices.Contains(strings.Fields(msg.Params[len(msg.Params)-1]), "sts") {
					t.Fatalf("client requested sts: %v", msg)
				}
			}
		}
	})
}

func TestManagerSTSPolicyState(t *testing.T) {
	m, err := NewManager(Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}

	// No policy: plaintext to the configured address.
	if addr, secure := m.effectiveAddr(); addr != "irc.test:6667" || secure {
		t.Fatalf("effectiveAddr = %q/%v", addr, secure)
	}

	// A duration policy on a secure connection sets port + expiry.
	m.applySTS(t.Context(), "irc.test:6697", 5*time.Minute)
	if addr, secure := m.effectiveAddr(); addr != "irc.test:6697" || !secure {
		t.Fatalf("after applySTS: effectiveAddr = %q/%v", addr, secure)
	}

	// duration=0 clears it.
	m.applySTS(t.Context(), "irc.test:6697", 0)
	if addr, secure := m.effectiveAddr(); addr != "irc.test:6667" || secure {
		t.Fatalf("after clear: effectiveAddr = %q/%v", addr, secure)
	}

	// An expired policy no longer upgrades.
	m.applySTS(t.Context(), "irc.test:6697", time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if _, secure := m.effectiveAddr(); secure {
		t.Fatal("expired policy still upgrades")
	}
}

// A post-registration CAP NEW carrying an STS upgrade port over an
// insecure link triggers the same secure-reconnect abort as CAP LS.
func TestCapNotifySTSUpgrade(t *testing.T) {
	m, err := NewManager(Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}

	insecure := &liveConn{secure: false, addr: "irc.test:6667"}
	msg := ircv4.MustParseMessage(":srv CAP AlteredParadox NEW :sts=port=6697,duration=100")
	err = m.capNotifySTS(context.Background(), insecure, msg)
	var up errSTSUpgrade
	if !errors.As(err, &up) || up.port != 6697 {
		t.Fatalf("insecure CAP NEW sts: err = %v, want errSTSUpgrade{6697}", err)
	}

	// On a secure link the same NEW persists the duration, no upgrade.
	secure := &liveConn{secure: true, addr: "irc.test:6697"}
	if err := m.capNotifySTS(context.Background(), secure, msg); err != nil {
		t.Fatalf("secure CAP NEW sts: %v", err)
	}
}
