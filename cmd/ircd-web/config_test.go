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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validConfig = `{
	"user": {"username": "AlteredParadox", "password_hash": "$2a$10$abcdefghijklmnopqrstuv"},
	"networks": [
		{
			"name": "libera",
			"addr": "irc.libera.chat:6697",
			"tls": true,
			"nick": "AlteredParadox",
			"sasl": {"login": "AlteredParadox", "password": "pw"},
			"channels": ["#go", "#linux"]
		},
		{"addr": "irc.other.net:6667", "allow_plaintext": true, "nick": "AlteredParadox"}
	]
}`

func TestLoadConfig(t *testing.T) {
	cases := []struct {
		name    string
		content string
		errSub  string // empty = must load
	}{
		{"valid", validConfig, ""},
		{
			name:    "unknown field rejected",
			content: `{"user": {"username": "a", "password_hash": "h"}, "listne": "x"}`,
			errSub:  "listne",
		},
		{
			name:    "missing user",
			content: `{"networks": []}`,
			errSub:  "user.username",
		},
		{
			name: "network without addr",
			content: `{"user": {"username": "a", "password_hash": "h"},
				"networks": [{"nick": "x"}]}`,
			errSub: "addr is required",
		},
		{
			name: "duplicate network names",
			content: `{"user": {"username": "a", "password_hash": "h"},
				"networks": [
					{"name": "n", "addr": "a:1", "nick": "x"},
					{"name": "n", "addr": "b:1", "nick": "x"}
				]}`,
			errSub: "duplicate network name",
		},
		{
			name: "duplicate via defaulted name",
			content: `{"user": {"username": "a", "password_hash": "h"},
				"networks": [
					{"addr": "a:1", "nick": "x"},
					{"addr": "a:1", "nick": "y"}
				]}`,
			errSub: "duplicate network name",
		},
		{"malformed json", `{`, "unexpected"},
		{
			name:    "trailing document rejected",
			content: `{"user": {"username": "a", "password_hash": "h"}} {"oops": 1}`,
			errSub:  "trailing data",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(writeConfig(t, tc.content))
			if tc.errSub == "" {
				if err != nil {
					t.Fatalf("loadConfig: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("err = %v, want containing %q", err, tc.errSub)
			}
		})
	}
}

func TestConfigDefaultsAndMapping(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8067" {
		t.Fatalf("Listen default = %q", cfg.Listen)
	}
	if cfg.Database != "ircthing.db" {
		t.Fatalf("Database default = %q", cfg.Database)
	}
	if cfg.sessionTTL() != 0 {
		t.Fatalf("sessionTTL with no config = %v, want 0 (api default)", cfg.sessionTTL())
	}

	ic, err := cfg.Networks[0].IRCConfig()
	if err != nil {
		t.Fatal(err)
	}
	if ic.Name != "libera" || ic.Addr != "irc.libera.chat:6697" || !ic.TLS {
		t.Fatalf("ircConfig = %+v", ic)
	}
	if ic.SASL == nil || ic.SASL.Login != "AlteredParadox" || ic.SASL.Password != "pw" {
		t.Fatalf("SASL mapping = %+v", ic.SASL)
	}
	if len(ic.Channels) != 2 || ic.Channels[0] != "#go" {
		t.Fatalf("Channels = %v", ic.Channels)
	}

	second := cfg.Networks[1]
	if second.EffectiveName() != "irc.other.net:6667" {
		t.Fatalf("effectiveName = %q", second.EffectiveName())
	}
	sc, err := second.IRCConfig()
	if err != nil {
		t.Fatal(err)
	}
	if sc.SASL != nil || !sc.AllowPlaintext {
		t.Fatalf("second ircConfig = %+v", sc)
	}
}

func TestPreviewsDefaultTriState(t *testing.T) {
	base := `{"user": {"username": "a", "password_hash": "h"}`
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"absent defaults off (privacy-first)", base + "}", false},
		{"explicit false enables", base + `, "disable_previews": false}`, true},
		{"explicit true disables", base + `, "disable_previews": true}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(writeConfig(t, tc.content))
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.previewsDefault(); got != tc.want {
				t.Fatalf("previewsDefault() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExampleConfigParses(t *testing.T) {
	// The committed example must always stay loadable.
	if _, err := loadConfig("../../config.example.json"); err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
	// And its behind_proxy must agree with its loopback listen (no warning), or
	// a copy-paste operator inherits a rate-limit footgun.
	cfg, err := loadConfig("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.proxyConfigWarning(); w != "" {
		t.Fatalf("example config proxy-warns: %q", w)
	}
	if w := cfg.cookieConfigWarning(); w != "" {
		t.Fatalf("example config cookie-warns: %q", w)
	}
}

func TestCookieConfigWarning(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		behind bool
		secure bool
		warn   bool
	}{
		{"proxy-fronted, secure (example)", "127.0.0.1:8067", true, true, false},
		{"proxy-fronted but insecure cookie", "127.0.0.1:8067", true, false, true},
		{"plain-http loopback, secure cookie bounces", "127.0.0.1:8067", false, true, true},
		{"plain-http loopback, insecure cookie ok", "127.0.0.1:8067", false, false, false},
		{"public direct plaintext warns (credentials over HTTP)", "0.0.0.0:8067", false, false, true},
		// A public direct listen is plaintext regardless of secure_cookies —
		// secure=true doesn't encrypt the login POST and additionally breaks the
		// cookie. It must still warn.
		{"public direct plaintext, secure cookie still warns", "0.0.0.0:8067", false, true, true},
		{"public behind TLS proxy, secure", "0.0.0.0:8067", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &config{Listen: tc.listen, BehindProxy: tc.behind, SecureCookies: tc.secure}
			if got := c.cookieConfigWarning() != ""; got != tc.warn {
				t.Fatalf("warn = %v, want %v (msg: %q)", got, tc.warn, c.cookieConfigWarning())
			}
		})
	}
}

func TestProxyConfigWarning(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		behind bool
		warn   bool
	}{
		{"loopback fronted by proxy", "127.0.0.1:8067", true, false},
		{"loopback but not behind proxy (shared-lockout risk)", "127.0.0.1:8067", false, true},
		{"ipv6 loopback direct", "[::1]:8067", false, true},
		{"public direct", "0.0.0.0:8067", false, false},
		{"public trusting XFF (spoofable)", "0.0.0.0:8067", true, true},
		{"public host trusting XFF", "203.0.113.9:8067", true, true},
		{"malformed listen is not flagged", "not-an-addr", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &config{Listen: tc.listen, BehindProxy: tc.behind}
			got := c.proxyConfigWarning() != ""
			if got != tc.warn {
				t.Fatalf("warn = %v, want %v (msg: %q)", got, tc.warn, c.proxyConfigWarning())
			}
		})
	}
}

func TestResolveConfigPath(t *testing.T) {
	cases := []struct {
		name    string
		flagVal string
		flagSet bool
		credDir string
		want    string
	}{
		{"no credential falls back to flag default", "config.json", false, "", "config.json"},
		{"credential used when flag unset", "config.json", false, "/run/creds/ircthing.service", "/run/creds/ircthing.service/config.json"},
		{"explicit flag wins over credential", "/etc/custom.json", true, "/run/creds/ircthing.service", "/etc/custom.json"},
		{"explicit flag with no credential", "/etc/custom.json", true, "", "/etc/custom.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveConfigPath(tc.flagVal, tc.flagSet, tc.credDir); got != tc.want {
				t.Fatalf("resolveConfigPath(%q, %v, %q) = %q, want %q", tc.flagVal, tc.flagSet, tc.credDir, got, tc.want)
			}
		})
	}
}
