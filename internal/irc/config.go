package irc

import (
	"crypto/tls"
	"errors"
	"time"
)

// Config describes one IRC network connection.
type Config struct {
	// Name is the network label echoed in every Event. Defaults to Addr.
	Name string

	// Addr is the server address as host:port.
	Addr string
	// TLS enables a TLS connection. Plaintext requires the explicit
	// AllowPlaintext opt-in.
	TLS bool
	// AllowPlaintext permits connecting without TLS.
	AllowPlaintext bool
	// TLSConfig optionally overrides the TLS client configuration
	// (e.g. client certificates). ServerName is derived from Addr when
	// empty. Nil gets sane defaults.
	TLSConfig *tls.Config

	Nick     string
	Username string // defaults to Nick
	Realname string // defaults to Nick
	// Pass is the server password (PASS), sent only if non-empty.
	Pass string

	// SASL enables SASL PLAIN authentication during registration.
	// If the server does not offer it, the connection attempt fails
	// (and is retried with backoff) rather than proceeding
	// unauthenticated.
	SASL *SASLPlain

	// Backoff controls reconnect delays; zero values pick defaults.
	Backoff BackoffConfig

	// Timing knobs. Zero values pick the documented defaults; tests set
	// them small.
	DialTimeout      time.Duration // TCP connect + TLS handshake, default 30s
	HandshakeTimeout time.Duration // registration must finish within, default 60s
	PingInterval     time.Duration // idle time before we PING the server, default 90s
	PingTimeout      time.Duration // wait for traffic after our PING, default 30s

	// Outbound flood protection (token bucket): SendBurst messages may go
	// out back-to-back, then one message per SendInterval. Defaults: 4
	// messages, 2s (the classic RFC 1459 client penalty model).
	SendBurst    int
	SendInterval time.Duration
}

// SASLPlain holds credentials for the SASL PLAIN mechanism (RFC 4616).
type SASLPlain struct {
	Login    string
	Password string
}

func (c *Config) validate() error {
	if c.Addr == "" {
		return errors.New("irc: config: Addr is required")
	}
	if c.Nick == "" {
		return errors.New("irc: config: Nick is required")
	}
	if !c.TLS && !c.AllowPlaintext {
		return errors.New("irc: config: plaintext connection requires the explicit AllowPlaintext opt-in")
	}
	if c.SASL != nil && c.SASL.Login == "" {
		return errors.New("irc: config: SASL requires a login")
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.Name == "" {
		c.Name = c.Addr
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 30 * time.Second
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = time.Minute
	}
	if c.PingInterval <= 0 {
		c.PingInterval = 90 * time.Second
	}
	if c.PingTimeout <= 0 {
		c.PingTimeout = 30 * time.Second
	}
	if c.SendBurst <= 0 {
		c.SendBurst = 4
	}
	if c.SendInterval <= 0 {
		c.SendInterval = 2 * time.Second
	}
}
