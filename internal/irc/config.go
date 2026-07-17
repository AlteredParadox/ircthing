package irc

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	ircv4 "gopkg.in/irc.v4"
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
	// TrustedFingerprints pins the server certificate: hex SHA-256 digests
	// of the leaf certificate (case-insensitive, ':' separators allowed).
	// When set, a matching fingerprint replaces CA verification — the way
	// to trust a self-signed IRC server without disabling verification.
	TrustedFingerprints []string

	// Proxy routes the connection through a proxy: "socks5://host:port"
	// (optionally user:pass@; DNS happens proxy-side) or
	// "http://host:port" (CONNECT tunnel). Empty connects directly. TLS
	// to the IRC server runs inside the tunnel as usual.
	Proxy string

	Nick     string
	Username string // defaults to Nick
	Realname string // defaults to Nick
	// Pass is the server password (PASS), sent only if non-empty.
	Pass string

	// SASL enables SASL authentication during registration. If the server
	// does not offer SASL (or the chosen mechanism), the connection
	// attempt fails (and is retried with backoff) rather than proceeding
	// unauthenticated.
	SASL *SASLConfig

	// STS optionally persists IRCv3 STS policies (see sts.go) so a
	// server's upgrade-to-TLS policy survives restarts. Nil keeps
	// policies for the process lifetime only.
	STS STSStore

	// Channels are joined after every successful registration, so they
	// come back automatically on reconnect.
	Channels []string

	// Backoff controls reconnect delays; zero values pick defaults.
	Backoff BackoffConfig

	// Timing knobs. Zero values pick the documented defaults; tests set
	// them small.
	DialTimeout      time.Duration // TCP connect + TLS handshake, default 30s
	HandshakeTimeout time.Duration // registration must finish within, default 60s
	PingInterval     time.Duration // idle time before we PING the server, default 90s
	PingTimeout      time.Duration // wait for traffic after our PING, default 30s

	// Outbound flood protection (token bucket): SendBurst messages may go
	// out back-to-back, then one message per SendInterval. Defaults: 16
	// messages, 2s. The burst must comfortably cover registration (CAP
	// LS/REQ/END, NICK, USER, SASL) plus a handful of rejoins, or every
	// reconnect crawls at one line per interval; 16 keeps sustained
	// output at the classic RFC 1459 rate while letting handshakes run
	// at full speed.
	SendBurst    int
	SendInterval time.Duration
}

// SASLConfig holds SASL authentication settings. Mechanism is one of
// "PLAIN", "EXTERNAL", "SCRAM-SHA-256", or "" to choose automatically
// (EXTERNAL when no password is set, else SCRAM-SHA-256 if the server
// offers it, else PLAIN). EXTERNAL authenticates via the TLS client
// certificate (Config.TLSConfig must carry one) and needs no password.
type SASLConfig struct {
	Mechanism string
	Login     string // authcid (account name); empty allowed for EXTERNAL
	Password  string // PLAIN / SCRAM-SHA-256
	Authzid   string // optional authorization identity, usually empty
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
	if err := c.validateSASL(); err != nil {
		return err
	}
	if _, err := fingerprintSet(c.TrustedFingerprints); err != nil {
		return err
	}
	if c.Proxy != "" {
		if _, err := parseProxyURL(c.Proxy); err != nil {
			return err
		}
	}
	if err := c.validateRegistrationLines(); err != nil {
		return err
	}
	return nil
}

// validateSASL checks the SASL configuration's internal consistency.
func (c *Config) validateSASL() error {
	if c.SASL == nil {
		return nil
	}
	mech := strings.ToUpper(c.SASL.Mechanism)
	// Resolve the mechanism newMech will actually pick: an empty mechanism
	// with no password auto-selects EXTERNAL (cert-based). Validate against
	// that effective mechanism, or an auto-EXTERNAL config escapes the TLS
	// requirement below and silently reconnect-loops (the server rejects
	// AUTHENTICATE EXTERNAL over plaintext with 904/906, forever).
	if mech == "" && c.SASL.Password == "" {
		mech = "EXTERNAL"
	}
	// Reject a mechanism we can't actually perform up front, rather than
	// letting AUTHENTICATE fail at runtime and reconnect-loop the network.
	// An empty mechanism is allowed: it auto-selects PLAIN/SCRAM by password.
	switch mech {
	case "", "PLAIN", "EXTERNAL", "SCRAM-SHA-256":
	default:
		return fmt.Errorf("irc: config: unsupported SASL mechanism %q (use PLAIN, EXTERNAL, or SCRAM-SHA-256)", c.SASL.Mechanism)
	}
	if mech != "EXTERNAL" && c.SASL.Login == "" {
		return errors.New("irc: config: SASL requires a login (except EXTERNAL)")
	}
	if mech == "EXTERNAL" && !c.TLS {
		return errors.New("irc: config: SASL EXTERNAL requires a TLS connection with a client certificate")
	}
	return nil
}

// validateRegistrationLines rejects configuration whose registration or
// autojoin messages would exceed the protocol line length. Registration
// happens before any ISUPPORT LINELEN is known, so these are always
// bounded by the 512-byte default — an oversized PASS/NICK/USER/JOIN
// would be rejected by the server on every attempt, pinning the network
// in a reconnect loop, so it is caught here (at add/edit time) instead.
func (c *Config) validateRegistrationLines() error {
	username := c.Username
	if username == "" {
		username = c.Nick
	}
	realname := c.Realname
	if realname == "" {
		realname = c.Nick
	}
	msgs := []*ircv4.Message{
		newMsg("NICK", c.Nick),
		newMsg("USER", username, "0", "*", realname),
	}
	if c.Pass != "" {
		msgs = append(msgs, newMsg("PASS", c.Pass))
	}
	for _, ch := range c.Channels {
		msgs = append(msgs, newMsg("JOIN", ch))
	}
	for _, m := range msgs {
		if err := checkLineLen(m, defaultLineLen); err != nil {
			return fmt.Errorf("irc: config: registration %s too long: %w", m.Command, err)
		}
	}
	return nil
}

// fingerprintSet normalizes the configured fingerprints (lowercase hex,
// ':' separators stripped) into a lookup set.
func fingerprintSet(fps []string) (map[string]bool, error) {
	if len(fps) == 0 {
		return nil, nil
	}
	set := make(map[string]bool, len(fps))
	for _, fp := range fps {
		norm := strings.ToLower(strings.ReplaceAll(fp, ":", ""))
		if len(norm) != sha256.Size*2 {
			return nil, fmt.Errorf("irc: config: trusted fingerprint %q is not a hex SHA-256 digest", fp)
		}
		if _, err := hex.DecodeString(norm); err != nil {
			return nil, fmt.Errorf("irc: config: trusted fingerprint %q is not a hex SHA-256 digest", fp)
		}
		set[norm] = true
	}
	return set, nil
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
		c.SendBurst = 16
	}
	if c.SendInterval <= 0 {
		c.SendInterval = 2 * time.Second
	}
}
