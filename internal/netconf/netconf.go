// Package netconf defines the JSON shape of a network definition. It is
// shared by the config file (bootstrap seeds) and the store (the
// database is the source of truth once seeded), and maps to the irc
// package's runtime Config.
package netconf

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"ircthing/internal/irc"
)

// Network is one IRC network definition. Stored as JSON both in the
// config file's networks[] and in the network_configs table; secrets
// (server password, SASL password) live at the same trust level as the
// config file itself.
type Network struct {
	Name           string `json:"name"`
	Addr           string `json:"addr"`
	TLS            bool   `json:"tls"`
	AllowPlaintext bool   `json:"allow_plaintext"`
	// TrustedFingerprints pins the server certificate: hex SHA-256 of the
	// leaf cert. A match replaces CA verification (for self-signed
	// servers).
	TrustedFingerprints []string `json:"trusted_fingerprints,omitempty"`
	// Proxy routes this network through "socks5://[user:pass@]host:port"
	// (DNS resolved proxy-side, Tor-friendly) or "http://host:port"
	// (CONNECT tunnel). Empty connects directly.
	Proxy    string   `json:"proxy,omitempty"`
	Nick     string   `json:"nick"`
	Username string   `json:"username,omitempty"`
	Realname string   `json:"realname,omitempty"`
	Pass     string   `json:"pass,omitempty"`
	SASL     *SASL    `json:"sasl,omitempty"`
	Channels []string `json:"channels,omitempty"`
}

type SASL struct {
	// Mechanism is "PLAIN", "EXTERNAL", "SCRAM-SHA-256", or "" to choose
	// automatically (EXTERNAL when no password, else SCRAM-SHA-256 if
	// offered, else PLAIN).
	Mechanism string `json:"mechanism,omitempty"`
	Login     string `json:"login,omitempty"`
	Password  string `json:"password,omitempty"`
	// CertFile/KeyFile provide the TLS client certificate for EXTERNAL.
	CertFile string `json:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`
}

// EffectiveName mirrors irc.Config's Name default so name-keyed lookups
// (hub registry, store) agree on what an unnamed network is called.
func (n *Network) EffectiveName() string {
	if n.Name != "" {
		return n.Name
	}
	return n.Addr
}

// Validate checks the fields a broken value of which would only surface
// as a confusing connect-time failure, and rejects the IRC framing
// characters (CR, LF, NUL) in every value that reaches the wire during
// registration — these go out via PASS/NICK/USER/JOIN and would
// otherwise inject extra protocol lines on every (re)connect.
func (n *Network) Validate() error {
	if n.Addr == "" {
		return errors.New("addr is required")
	}
	if n.Nick == "" {
		return errors.New("nick is required")
	}
	fields := map[string]string{
		"addr": n.Addr, "nick": n.Nick, "username": n.Username,
		"realname": n.Realname, "pass": n.Pass, "proxy": n.Proxy,
	}
	if n.SASL != nil {
		fields["sasl.login"] = n.SASL.Login
		fields["sasl.password"] = n.SASL.Password
	}
	for name, v := range fields {
		if strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("%s must not contain CR, LF, or NUL", name)
		}
	}
	// nick and username are written as NON-trailing parameters of the
	// registration lines (NICK <nick>; USER <username> 0 * :realname, with
	// username defaulting to nick when unset). A space there would be
	// re-parsed by the server as a parameter boundary, and the writer's
	// fatal framing guard tears the connection down — wedging the network
	// in a permanent reconnect loop with a misleading error. Reject it at
	// config time instead, loudly.
	if strings.IndexByte(n.Nick, ' ') != -1 {
		return errors.New("nick must not contain spaces")
	}
	if strings.IndexByte(n.Username, ' ') != -1 {
		return errors.New("username must not contain spaces")
	}
	for i, ch := range n.Channels {
		if strings.ContainsAny(ch, " \r\n\x00") { // space too: one JOIN target
			return fmt.Errorf("channels[%d] must not contain spaces, CR, LF, or NUL", i)
		}
	}
	// SASL EXTERNAL authenticates purely with a client certificate, so a
	// config that selects it but omits the keypair would connect presenting
	// no certificate and fail authentication on every attempt (a permanent
	// backoff loop). Require both files explicitly — against the EFFECTIVE
	// mechanism: an empty mechanism with no password auto-selects EXTERNAL
	// (mirrors irc.Config.validateSASL), so checking only the literal string
	// would let an auto-EXTERNAL config through.
	if n.SASL != nil {
		mech := strings.ToUpper(n.SASL.Mechanism)
		external := mech == "EXTERNAL" || (mech == "" && n.SASL.Password == "")
		if external && (n.SASL.CertFile == "" || n.SASL.KeyFile == "") {
			return errors.New("sasl EXTERNAL requires both cert_file and key_file")
		}
	}
	return nil
}

// Parse decodes and validates a JSON network definition, rejecting
// unknown fields so client typos fail loudly.
func Parse(raw []byte) (*Network, error) {
	var n Network
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&n); err != nil {
		return nil, err
	}
	// Strict means one document; see loadConfig.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, errors.New("trailing data after the network object")
	}
	if err := n.Validate(); err != nil {
		return nil, err
	}
	return &n, nil
}

// IRCConfig maps the definition onto the irc package's runtime config,
// loading the SASL EXTERNAL client certificate if one is configured.
func (n *Network) IRCConfig() (irc.Config, error) {
	cfg := irc.Config{
		Name:                n.Name,
		Addr:                n.Addr,
		TLS:                 n.TLS,
		AllowPlaintext:      n.AllowPlaintext,
		TrustedFingerprints: n.TrustedFingerprints,
		Proxy:               n.Proxy,
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
		// TLS handshake. Expand env references so a systemd unit can point
		// cert_file/key_file at "$CREDENTIALS_DIRECTORY/..." (LoadCredential),
		// as the README documents, rather than the literal string failing to
		// open on every connect.
		if n.SASL.CertFile != "" {
			cert, err := tls.LoadX509KeyPair(os.ExpandEnv(n.SASL.CertFile), os.ExpandEnv(n.SASL.KeyFile))
			if err != nil {
				return irc.Config{}, fmt.Errorf("network %q: loading client certificate: %w", n.EffectiveName(), err)
			}
			cfg.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		}
	}
	return cfg, nil
}
