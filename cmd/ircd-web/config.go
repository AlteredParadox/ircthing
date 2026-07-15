package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"ircthing/internal/irc"
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
	Database string      `json:"database"`
	User     userConfig  `json:"user"`
	Networks []netConfig `json:"networks"`
	// RingSize overrides the per-buffer hot scrollback bound (messages
	// kept in memory per channel/query). 0 = default.
	RingSize int `json:"ring_size"`
	// SessionTTLDays is how long login cookies stay valid. 0 = 30 days.
	SessionTTLDays int `json:"session_ttl_days"`
}

type userConfig struct {
	Username string `json:"username"`
	// PasswordHash is a bcrypt hash; generate one with
	// `ircd-web -hash-password`.
	PasswordHash string `json:"password_hash"`
}

type netConfig struct {
	Name           string `json:"name"`
	Addr           string `json:"addr"`
	TLS            bool   `json:"tls"`
	AllowPlaintext bool   `json:"allow_plaintext"`
	// TrustedFingerprints pins the server certificate: hex SHA-256 of the
	// leaf cert. A match replaces CA verification (for self-signed
	// servers).
	TrustedFingerprints []string    `json:"trusted_fingerprints"`
	Nick                string      `json:"nick"`
	Username            string      `json:"username"`
	Realname            string      `json:"realname"`
	Pass                string      `json:"pass"`
	SASL                *saslConfig `json:"sasl"`
	Channels            []string    `json:"channels"`
}

type saslConfig struct {
	// Mechanism is "PLAIN", "EXTERNAL", "SCRAM-SHA-256", or "" to choose
	// automatically (EXTERNAL when no password, else SCRAM-SHA-256 if
	// offered, else PLAIN).
	Mechanism string `json:"mechanism"`
	Login     string `json:"login"`
	Password  string `json:"password"`
	// CertFile/KeyFile provide the TLS client certificate for EXTERNAL.
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
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
		if n.Addr == "" {
			return fmt.Errorf("networks[%d]: addr is required", i)
		}
		name := n.effectiveName()
		if seen[name] {
			return fmt.Errorf("networks[%d]: duplicate network name %q", i, name)
		}
		seen[name] = true
	}
	return nil
}

// effectiveName mirrors irc.Config's Name default so the duplicate check
// matches what the hub registry will see.
func (n *netConfig) effectiveName() string {
	if n.Name != "" {
		return n.Name
	}
	return n.Addr
}

func (n *netConfig) ircConfig() (irc.Config, error) {
	cfg := irc.Config{
		Name:                n.Name,
		Addr:                n.Addr,
		TLS:                 n.TLS,
		AllowPlaintext:      n.AllowPlaintext,
		TrustedFingerprints: n.TrustedFingerprints,
		Nick:                n.Nick,
		Username:            n.Username,
		Realname:            n.Realname,
		Pass:                n.Pass,
		Channels:            n.Channels,
	}
	if n.SASL != nil {
		cfg.SASL = &irc.SASLConfig{
			Mechanism: n.SASL.Mechanism,
			Login:     n.SASL.Login,
			Password:  n.SASL.Password,
		}
		// A client certificate (SASL EXTERNAL) is presented during the
		// TLS handshake.
		if n.SASL.CertFile != "" {
			cert, err := tls.LoadX509KeyPair(n.SASL.CertFile, n.SASL.KeyFile)
			if err != nil {
				return irc.Config{}, fmt.Errorf("network %q: loading client certificate: %w", n.effectiveName(), err)
			}
			cfg.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		}
	}
	return cfg, nil
}

func (c *config) sessionTTL() time.Duration {
	if c.SessionTTLDays <= 0 {
		return 0 // api applies its default
	}
	return time.Duration(c.SessionTTLDays) * 24 * time.Hour
}
