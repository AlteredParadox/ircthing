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
	"unicode"
	"unicode/utf8"

	"ircthing/internal/irc"
	"ircthing/internal/proxydial"
	"ircthing/internal/wgdial"
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
	Proxy string `json:"proxy,omitempty"`
	// WireGuard optionally routes this network's egress through an
	// in-process userspace WireGuard tunnel. Its presence is the config flag
	// that turns the tunnel on for this network; mutually exclusive with Proxy.
	WireGuard *WireGuard `json:"wireguard,omitempty"`
	Nick      string     `json:"nick"`
	Username  string     `json:"username,omitempty"`
	Realname  string     `json:"realname,omitempty"`
	Pass      string     `json:"pass,omitempty"`
	SASL      *SASL      `json:"sasl,omitempty"`
	Channels  []string   `json:"channels,omitempty"`
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

// WireGuard is one network's in-process WireGuard egress. Keys are
// standard base64 (as `wg`/Mullvad print them); endpoint is the peer's
// host:port, address is our address inside the tunnel, and dns is the
// in-tunnel resolver (all target lookups go there, not the local resolver).
type WireGuard struct {
	PrivateKey    string `json:"private_key"`
	PeerPublicKey string `json:"peer_public_key"`
	PresharedKey  string `json:"preshared_key,omitempty"`
	Endpoint      string `json:"endpoint"`
	Address       string `json:"address"`
	DNS           string `json:"dns"`
	MTU           int    `json:"mtu,omitempty"`
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
// maxChannels caps a network definition's channel list, matching the
// incremental autojoin cap (internal/hub.maxPersistedChannels) so the bulk
// (config/PutNetworkConfig) and incremental paths agree. maxChannelLen caps
// each name to internal/hub.maxPersistedChannelLen for the same reason: a name
// the persist path would skip must not be storable in the first place.
const (
	maxChannels   = 4096
	maxChannelLen = 200
	// maxNetworkNameBytes is mirrored by store.MaxNetworkNameBytes: sized so
	// any addr proxydial.ValidHostPort accepts (255-byte host + ":65535",
	// ~262 bytes) can serve as the EffectiveName fallback.
	maxNetworkNameBytes = 300
)

func (n *Network) Validate() error {
	if err := n.validateIdentity(); err != nil {
		return err
	}
	if err := n.validateFraming(); err != nil {
		return err
	}
	return n.validateSASLExternal()
}

// validateIdentity checks the fields that name and reach the network: addr
// shape, nick, egress exclusivity, and the reserved-name set.
func (n *Network) validateIdentity() error {
	if n.Addr == "" {
		return errors.New("addr is required")
	}
	// Nothing downstream supports a portless or malformed addr (dial, TLS SNI,
	// and STS all SplitHostPort it), so a bad shape saves fine and then wedges
	// the network in a reconnect loop with a dial-time error. Reject it here,
	// loudly, with the SAME strict host:port validator the proxy paths use:
	// host:port shape, bounded host free of whitespace / control / authority
	// delimiters, and a strict decimal port (rejects "+6667").
	if err := proxydial.ValidHostPort(n.Addr); err != nil {
		return fmt.Errorf("addr %q: %v", n.Addr, err)
	}
	if n.Nick == "" {
		return errors.New("nick is required")
	}
	name := n.EffectiveName()
	if err := validNetworkName(name); err != nil {
		if n.Name == "" {
			// The failing value is the addr standing in for a missing name
			// (EffectiveName fallback) — blame the right field.
			return fmt.Errorf("network has no name and its addr %q is not usable as one (%v); set a short \"name\"", n.Addr, err)
		}
		return err
	}
	// Egress is either a proxy or a WireGuard tunnel, never both. The full
	// WireGuard validation is deferred to wgdial.Validate via IRCConfig/NewManager;
	// this cheap mutual-exclusion check lives here too so the config layer rejects
	// the combo directly, independent of that funnel (defense in depth).
	if n.Proxy != "" && n.WireGuard != nil {
		return errors.New("proxy and wireguard are mutually exclusive")
	}
	return nil
}

// validNetworkName rejects only what is unsafe in a network identifier:
// oversized names, invalid UTF-8, control characters (NUL/CR/LF/tab
// injection), the reserved recovery prefix, and the JS Object.prototype set
// below. Spaces and other printable Unicode are deliberately legal — legacy
// configs and databases hold names like "Libera Chat". Keep in sync with
// internal/store's validateNetworkName (the packages intentionally do not
// import each other: store stays free of internal deps, and netconf must not
// pull in the persistence layer).
func validNetworkName(name string) error {
	if len(name) > maxNetworkNameBytes || !utf8.ValidString(name) {
		return fmt.Errorf("network name must be valid UTF-8 and at most %d bytes", maxNetworkNameBytes)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("network name must not contain control characters")
		}
	}
	if strings.HasPrefix(name, "__ircthing_invalid_row_") {
		return fmt.Errorf("network name %q is reserved", name)
	}
	// The frontend keys several plain JS objects by the network name (ignores,
	// monitors, per-network maps). A name that collides with an Object.prototype
	// property is either silently dropped (__proto__) or returns the inherited
	// function on lookup — truthy, and a later .includes()/array op throws. Reject
	// the full set of Object.prototype property names (plus "prototype") at this
	// single choke point rather than reshaping every keyed store client-side; the
	// set is fixed by the ECMAScript spec.
	switch name {
	case "__proto__", "constructor", "prototype",
		"hasOwnProperty", "isPrototypeOf", "propertyIsEnumerable",
		"toLocaleString", "toString", "valueOf",
		"__defineGetter__", "__defineSetter__", "__lookupGetter__", "__lookupSetter__":
		return fmt.Errorf("network name %q is reserved", name)
	}
	return nil
}

// validateFraming rejects the IRC framing characters (CR, LF, NUL) in every
// value that reaches the wire during registration, and spaces where a value
// is a non-trailing parameter: nick and username are written as NICK <nick>
// / USER <username> 0 * :realname (username defaulting to nick when unset),
// so a space there would be re-parsed by the server as a parameter boundary,
// and the writer's fatal framing guard tears the connection down — wedging
// the network in a permanent reconnect loop with a misleading error. Reject
// it at config time instead, loudly.
func (n *Network) validateFraming() error {
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
	if strings.IndexByte(n.Nick, ' ') != -1 {
		return errors.New("nick must not contain spaces")
	}
	if strings.IndexByte(n.Username, ' ') != -1 {
		return errors.New("username must not contain spaces")
	}
	// Bound the LIST length, not just each name: the incremental autojoin path
	// caps growth at maxPersistedChannels, but a bulk definition (config seed or
	// PutNetworkConfig) bypassed that, so a 100k-channel definition was accepted
	// and drove a 100k-entry rejoin map + JOIN storm on connect. Same cap here.
	if len(n.Channels) > maxChannels {
		return fmt.Errorf("too many channels (%d, max %d)", len(n.Channels), maxChannels)
	}
	for i, ch := range n.Channels {
		if ch == "" { // an empty entry would send a bare `JOIN :` on reconnect
			return fmt.Errorf("channels[%d] is empty", i)
		}
		if strings.ContainsAny(ch, " \r\n\x00") { // space too: one JOIN target
			return fmt.Errorf("channels[%d] must not contain spaces, CR, LF, or NUL", i)
		}
		// Cap each name to the persisted-channel length: a longer name can be
		// JOINed but the autojoin persist path (maxPersistedChannelLen) silently
		// skips it, so it could never be durably removed. Real names are tens of
		// bytes.
		if len(ch) > maxChannelLen {
			return fmt.Errorf("channels[%d] too long (%d bytes, max %d)", i, len(ch), maxChannelLen)
		}
	}
	return nil
}

// validateSASLExternal requires the client-certificate keypair whenever the
// EFFECTIVE mechanism is EXTERNAL: a config that selects it but omits the
// keypair would connect presenting no certificate and fail authentication
// on every attempt (a permanent backoff loop). An empty mechanism with no
// password auto-selects EXTERNAL (mirrors irc.Config.validateSASL), so
// checking only the literal string would let an auto-EXTERNAL config
// through.
func (n *Network) validateSASLExternal() error {
	if n.SASL == nil {
		return nil
	}
	mech := strings.ToUpper(n.SASL.Mechanism)
	external := mech == "EXTERNAL" || (mech == "" && n.SASL.Password == "")
	if external && (n.SASL.CertFile == "" || n.SASL.KeyFile == "") {
		return errors.New("sasl EXTERNAL requires both cert_file and key_file")
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
	if dec.Decode(new(json.RawMessage)) != io.EOF {
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
	if n.WireGuard != nil {
		cfg.WireGuard = &wgdial.Config{
			PrivateKey:    n.WireGuard.PrivateKey,
			PeerPublicKey: n.WireGuard.PeerPublicKey,
			PresharedKey:  n.WireGuard.PresharedKey,
			Endpoint:      n.WireGuard.Endpoint,
			Address:       n.WireGuard.Address,
			DNS:           n.WireGuard.DNS,
			MTU:           n.WireGuard.MTU,
		}
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
