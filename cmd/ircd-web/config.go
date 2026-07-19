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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
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
	// background and keeps the search index in step. Editable at runtime in
	// Settings; this value only SEEDS the database on first run — the stored
	// value wins thereafter, so later edits to the file have no effect.
	RetentionDays int `json:"retention_days"`
	// RetentionMaxMessages caps how many messages are kept per buffer
	// (channel/query); older ones beyond the newest N are pruned.
	// 0 = unlimited (the default). Runtime-editable / DB-authoritative like
	// retention_days.
	RetentionMaxMessages int `json:"retention_max_messages"`
	// SessionTTLDays is how long login cookies stay valid. 0 = 30 days.
	// Editable in Settings; a UI change is stored and overrides this.
	SessionTTLDays int `json:"session_ttl_days"`
	// SecureCookies marks the session cookie Secure (sent over HTTPS
	// only). Turn this on when a TLS-terminating reverse proxy fronts
	// the binary — i.e. any deployment beyond plain-HTTP loopback.
	SecureCookies bool `json:"secure_cookies"`
	// BehindProxy: the binary sits behind a trusted reverse proxy that sets
	// X-Real-IP / X-Forwarded-For. The login backoff then keys on the real
	// client IP instead of the shared proxy address, so one attacker can't
	// lock out every user. Leave false for direct/loopback deployments —
	// otherwise a client could spoof those headers to evade backoff.
	BehindProxy bool `json:"behind_proxy"`
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
	// `ircd-web -hash-password`. It seeds the login password; a change made
	// in Settings → Change password is stored in the database and takes
	// precedence (the config file may be a read-only systemd credential).
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

// proxyConfigWarning flags a behind_proxy setting that disagrees with the
// listen address — the two are coupled, and either mismatch is a real security
// footgun, so we warn at startup rather than silently do the wrong thing.
// Returns "" when the pairing is sensible.
//
//   - loopback listen + behind_proxy=false: a loopback bind is almost always
//     fronted by a reverse proxy, so every request arrives from the proxy's IP
//     and the login backoff keys on ONE shared identity — one attacker locks
//     out every user. Set behind_proxy=true so it keys on X-Forwarded-For.
//   - public listen + behind_proxy=true: trusting X-Forwarded-For on a directly
//     reachable socket lets a client spoof that header to rotate identities and
//     evade the login backoff entirely. Set behind_proxy=false, or bind to
//     loopback behind a proxy that overwrites the header.
func (c *config) proxyConfigWarning() string {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return ""
	}
	ip := net.ParseIP(host)
	loopback := ip != nil && ip.IsLoopback()
	switch {
	case loopback && !c.BehindProxy:
		return "behind_proxy is false but listen is loopback (" + c.Listen + "): if a reverse proxy fronts this, all clients share the proxy's IP for login rate-limiting — one attacker can lock everyone out. Set behind_proxy=true when behind a trusted proxy."
	case !loopback && c.BehindProxy:
		return "behind_proxy is true but listen is a public address (" + c.Listen + "): a client reaching this socket directly can spoof X-Forwarded-For to evade the login backoff. Set behind_proxy=false unless a proxy overwrites that header."
	}
	return ""
}

// Conservative upper bounds on numeric knobs. These sit far below the points
// where the downstream arithmetic misbehaves — a retention_days that overflows
// the time.Duration cutoff (~106752) flips it into the FUTURE and deletes all
// history; a huge ring_size overflows the ringSize+1 query LIMIT into a
// negative that SQLite treats as unbounded. 100 years is longer than any real
// retention/session and well clear of those edges.
const (
	maxConfigDays     = 36500     // ~100 years
	maxConfigRingSize = 1_000_000 // per-buffer hot messages; far over any sane value
)

func (c *config) validate() error {
	if c.User.Username == "" || c.User.PasswordHash == "" {
		return errors.New("user.username and user.password_hash are required (generate the hash with -hash-password)")
	}
	// The store percent-encodes '?'/'#'/'%' when building its file: URI so the
	// opened file matches the secured literal path, but a database value that
	// is itself a "file:" URI (e.g. "file::memory:x") would slip past the
	// in-memory skip and create an unsecured on-disk file. Config takes a plain
	// filesystem path, so reject the URI form and control characters.
	if strings.HasPrefix(c.Database, "file:") || strings.ContainsAny(c.Database, "\n\r\x00") {
		return fmt.Errorf("database %q must be a plain filesystem path (no 'file:' URI, no control characters)", c.Database)
	}
	if c.RingSize < 0 || c.RingSize > maxConfigRingSize {
		return fmt.Errorf("ring_size %d out of range (0..%d)", c.RingSize, maxConfigRingSize)
	}
	if c.RetentionDays < 0 || c.RetentionDays > maxConfigDays {
		return fmt.Errorf("retention_days %d out of range (0..%d)", c.RetentionDays, maxConfigDays)
	}
	if c.RetentionMaxMessages < 0 {
		return fmt.Errorf("retention_max_messages %d must not be negative", c.RetentionMaxMessages)
	}
	if c.SessionTTLDays < 0 || c.SessionTTLDays > maxConfigDays {
		return fmt.Errorf("session_ttl_days %d out of range (0..%d)", c.SessionTTLDays, maxConfigDays)
	}
	return validateNetworks(c.Networks)
}

// validateNetworks checks each seed definition and rejects duplicate
// effective names (two networks would fight over one stored identity).
func validateNetworks(nets []netconf.Network) error {
	seen := make(map[string]bool)
	for i, n := range nets {
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
