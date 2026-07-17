package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"ircthing/internal/netconf"
)

// Config file: JSON (stdlib, no dependency), parsed strictly — unknown
// fields are errors so typos fail loudly instead of being silently
// ignored. See config.example.json for a commented walkthrough.
type config struct {
	// Listen is the HTTP listen address. Default 127.0.0.1:8067 —
	// loopback only; put a TLS-terminating reverse proxy in front or
	// change it deliberately.
	Listen string `json:"listen"`
	// Database is the SQLite path, created on first run. Default
	// ircthing.db in the working directory.
	Database string     `json:"database"`
	User     userConfig `json:"user"`
	// Networks seeds the database on first run only: once the
	// network_configs table is non-empty (including after adding a
	// network in the web UI), the database is the source of truth and
	// this list is ignored. Manage networks from the UI thereafter.
	Networks []netconf.Network `json:"networks"`
	// RingSize overrides the per-buffer hot scrollback bound (messages
	// kept in memory per channel/query). 0 = default.
	RingSize int `json:"ring_size"`
	// RetentionDays prunes stored messages older than this many days.
	// 0 = keep forever (the default). Pruning runs hourly in the
	// background and keeps the search index in step.
	RetentionDays int `json:"retention_days"`
	// RetentionMaxMessages caps how many messages are kept per buffer
	// (channel/query); older ones beyond the newest N are pruned.
	// 0 = unlimited (the default).
	RetentionMaxMessages int `json:"retention_max_messages"`
	// SessionTTLDays is how long login cookies stay valid. 0 = 30 days.
	SessionTTLDays int `json:"session_ttl_days"`
	// SecureCookies marks the session cookie Secure (sent over HTTPS
	// only). Turn this on when a TLS-terminating reverse proxy fronts
	// the binary — i.e. any deployment beyond plain-HTTP loopback.
	SecureCookies bool `json:"secure_cookies"`
	// DisablePreviews is the initial default for the previews switch, and
	// is deliberately tri-state so the default can be privacy-first without
	// silently overriding an explicit choice:
	//   - absent (nil): previews start OFF — the server makes zero outbound
	//     fetches until they are turned on. This is the default because an
	//     auto-fetched preview is a tracking beacon (a poster learns when a
	//     buffer is viewed).
	//   - false: previews start ON (not disabled).
	//   - true:  previews start OFF (explicitly).
	// It is toggleable at runtime in the UI (Settings → Link previews &
	// media), which then wins. Preview fetches use each link's network
	// proxy, so there is no separate media proxy to configure.
	DisablePreviews *bool `json:"disable_previews"`
}

// previewsDefault resolves the initial state of the previews switch from
// the tri-state disable_previews field: off unless the config explicitly
// sets disable_previews=false (see the field doc).
func (c *config) previewsDefault() bool {
	if c.DisablePreviews == nil {
		return false
	}
	return !*c.DisablePreviews
}

type userConfig struct {
	Username string `json:"username"`
	// PasswordHash is a bcrypt hash; generate one with
	// `ircd-web -hash-password`.
	PasswordHash string `json:"password_hash"`
}

func loadConfig(path string) (*config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var cfg config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Strict means one document: trailing JSON would be silently ignored
	// otherwise, hiding merge/templating mistakes.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("%s: trailing data after the config object", path)
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8067"
	}
	if cfg.Database == "" {
		cfg.Database = "ircthing.db"
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *config) validate() error {
	if c.User.Username == "" || c.User.PasswordHash == "" {
		return errors.New("user.username and user.password_hash are required (generate the hash with -hash-password)")
	}
	seen := make(map[string]bool)
	for i, n := range c.Networks {
		if err := n.Validate(); err != nil {
			return fmt.Errorf("networks[%d]: %w", i, err)
		}
		name := n.EffectiveName()
		if seen[name] {
			return fmt.Errorf("networks[%d]: duplicate network name %q", i, name)
		}
		seen[name] = true
	}
	return nil
}

func (c *config) sessionTTL() time.Duration {
	if c.SessionTTLDays <= 0 {
		return 0 // api applies its default
	}
	return time.Duration(c.SessionTTLDays) * 24 * time.Hour
}

