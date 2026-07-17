package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	"ircthing/internal/netconf"
	"ircthing/internal/proxydial"
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
	// SessionTTLDays is how long login cookies stay valid. 0 = 30 days.
	SessionTTLDays int `json:"session_ttl_days"`
	// SecureCookies marks the session cookie Secure (sent over HTTPS
	// only). Turn this on when a TLS-terminating reverse proxy fronts
	// the binary — i.e. any deployment beyond plain-HTTP loopback.
	SecureCookies bool `json:"secure_cookies"`
	// MediaProxy routes the server-side media proxy (link previews, image
	// thumbnails) through a proxy, so those fetches don't leak the server's
	// real IP when the IRC connections use one. Same URL form as a network
	// proxy: socks5://[user:pass@]host:port (SOCKS5 with RFC 1929 auth),
	// socks5h://…, or http://[user:pass@]host:port. Empty = direct.
	MediaProxy string `json:"media_proxy"`
	// DisablePreviews turns the media proxy off entirely: /api/preview and
	// /api/thumb are disabled and the UI stops requesting them, so the
	// server makes zero outbound fetches for links/images.
	DisablePreviews bool `json:"disable_previews"`
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
	if c.MediaProxy != "" {
		if _, err := proxydial.Parse(c.MediaProxy); err != nil {
			return fmt.Errorf("media_proxy: %w", err)
		}
	}
	return nil
}

func (c *config) sessionTTL() time.Duration {
	if c.SessionTTLDays <= 0 {
		return 0 // api applies its default
	}
	return time.Duration(c.SessionTTLDays) * 24 * time.Hour
}

// mediaProxyURL parses the media-proxy setting (nil when unset). validate()
// has already checked it parses, so this is expected to succeed.
func (c *config) mediaProxyURL() (*url.URL, error) {
	if c.MediaProxy == "" {
		return nil, nil
	}
	return proxydial.Parse(c.MediaProxy)
}
