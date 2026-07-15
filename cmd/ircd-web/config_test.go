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

	ic, err := cfg.Networks[0].ircConfig()
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
	if second.effectiveName() != "irc.other.net:6667" {
		t.Fatalf("effectiveName = %q", second.effectiveName())
	}
	sc, err := second.ircConfig()
	if err != nil {
		t.Fatal(err)
	}
	if sc.SASL != nil || !sc.AllowPlaintext {
		t.Fatalf("second ircConfig = %+v", sc)
	}
}

func TestExampleConfigParses(t *testing.T) {
	// The committed example must always stay loadable.
	if _, err := loadConfig("../../config.example.json"); err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
}
