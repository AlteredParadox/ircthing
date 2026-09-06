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
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
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

// A client-certificate path is caller-controlled over the authenticated
// put_network protocol, and the resulting validation error is pushed straight
// back to that session. So the error must never disclose the process
// environment: only $CREDENTIALS_DIRECTORY expands, and no expanded path
// appears in the message. Regression test for the env-disclosure finding.
func TestIRCConfigCertPathsDoNotLeakEnvironment(t *testing.T) {
	t.Setenv("IRCTHING_TEST_SECRET", "s3cr3t-value")
	t.Setenv("CREDENTIALS_DIRECTORY", "/run/credentials/ircthing.service")

	for _, probe := range []string{
		"$IRCTHING_TEST_SECRET/probe.pem",
		"${IRCTHING_TEST_SECRET}/probe.pem",
		"$CREDENTIALS_DIRECTORY/missing.pem",
	} {
		n := &Network{Addr: "irc.x.net:6697", Nick: "me", SASL: &SASL{
			Mechanism: "EXTERNAL", CertFile: probe, KeyFile: probe,
		}}
		_, err := n.IRCConfig()
		if err == nil {
			t.Fatalf("%s: IRCConfig() = nil error, want a load failure", probe)
		}
		msg := err.Error()
		if strings.Contains(msg, "s3cr3t-value") {
			t.Errorf("%s: error discloses the environment value: %s", probe, msg)
		}
		// No filesystem path at all — not the expanded credentials
		// directory, and not the literal unexpanded reference.
		if strings.Contains(msg, "/") {
			t.Errorf("%s: error discloses a path: %s", probe, msg)
		}
	}
}

// The documented systemd LoadCredential form must still resolve, or SASL
// EXTERNAL breaks for every hardened-unit deployment.
func TestExpandCredentialsDir(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "/run/credentials/x.service")
	t.Setenv("OTHER", "leaked")
	cases := []struct{ in, want string }{
		{"$CREDENTIALS_DIRECTORY/c.pem", "/run/credentials/x.service/c.pem"},
		{"${CREDENTIALS_DIRECTORY}/c.pem", "/run/credentials/x.service/c.pem"},
		{"/etc/ircthing/c.pem", "/etc/ircthing/c.pem"},
		{"$OTHER/c.pem", "$OTHER/c.pem"},
		{"${OTHER}/c.pem", "${OTHER}/c.pem"},
	}
	for _, c := range cases {
		if got := expandCredentialsDir(c.in); got != c.want {
			t.Errorf("expandCredentialsDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The client-visible certificate error must be ONE fixed string whatever
// went wrong. The previous redaction kept the OS reason, so an
// authenticated caller could distinguish missing / unreadable / directory /
// not-PEM for any path it named — a filesystem oracle. The detail must still
// reach the server log, or the operator cannot debug a real misconfiguration.
func TestIRCConfigCertErrorIsOpaque(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(notPEM, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	probes := map[string]string{
		"missing":   filepath.Join(dir, "missing.pem"),
		"directory": dir,
		"not PEM":   notPEM,
	}
	if os.Geteuid() != 0 { // root reads a 0000 file, so the case is void there
		unreadable := filepath.Join(dir, "unreadable.pem")
		if err := os.WriteFile(unreadable, []byte("x"), 0o000); err != nil {
			t.Fatal(err)
		}
		probes["unreadable"] = unreadable
	}

	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	const want = `network "x": client certificate could not be loaded`
	for name, path := range probes {
		n := &Network{Name: "x", Addr: "irc.x.net:6697", Nick: "me", SASL: &SASL{
			Mechanism: "EXTERNAL", CertFile: path, KeyFile: path,
		}}
		_, err := n.IRCConfig()
		if err == nil {
			t.Fatalf("%s: IRCConfig() = nil error, want a load failure", name)
		}
		if !errors.Is(err, ErrClientCert) {
			t.Errorf("%s: error does not wrap ErrClientCert: %v", name, err)
		}
		if got := err.Error(); got != want {
			t.Errorf("%s: error = %q, want the fixed %q", name, got, want)
		}
		for _, leak := range []string{"no such file", "permission denied", "is a directory", "PEM", dir} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: error discloses %q: %v", name, leak, err)
			}
		}
	}
	// The operator still gets the detail, server-side only.
	if !strings.Contains(logged.String(), dir) {
		t.Errorf("server log lacks the failing path; got:\n%s", logged.String())
	}
}
