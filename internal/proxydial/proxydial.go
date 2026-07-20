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

// Package proxydial is a dependency-free SOCKS5 / HTTP-CONNECT dialer,
// shared by the per-network IRC connection (internal/irc) and the media
// proxy (internal/api). Both handshakes are a few dozen bytes, which beats
// adding a dependency for them (CLAUDE.md dependency policy).
//
// SOCKS5 is RFC 1928 with RFC 1929 username/password auth; the target
// hostname is passed to the proxy unresolved (ATYP domain) so DNS happens
// proxy-side — what Tor and friends expect (no local DNS leak). "socks5h"
// is accepted as an alias for that behavior. HTTP CONNECT tunnels support
// Basic proxy auth.
//
// A proxy is configured as a URL: "socks5://host:port",
// "socks5://user:pass@host:port", or "http://host:port".
package proxydial

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrProxyConfig wraps every structural proxy/target validation failure so a
// caller can classify it as PERMANENT (a misconfiguration won't fix itself on
// retry) rather than a transient dial error. All validation errors below are
// %w-wrapped with it and carry no user input (credentials never reach a log).
var ErrProxyConfig = errors.New("proxy: invalid configuration")

// ErrProxyRejected wraps a DETERMINISTIC runtime rejection by the proxy itself —
// SOCKS auth failure / no acceptable method / policy or unsupported-command
// rejection, or an HTTP CONNECT 1xx/3xx/4xx (other than 408/429). Retrying
// re-runs the same rejected handshake, so the media layer classifies it
// PERMANENT. Genuine network failures, 408/429, and 5xx stay unwrapped
// (transient).
var ErrProxyRejected = errors.New("proxy: request rejected")

// ErrProxyProtocol wraps a MALFORMED but complete proxy response — a wrong SOCKS
// version or reserved byte, or an unparseable HTTP status line. The peer is not
// speaking the protocol we expect (wrong port, not a proxy), so retrying is
// pointless: also classified PERMANENT.
var ErrProxyProtocol = errors.New("proxy: malformed response")

// ValidHostPort is the ONE strict host:port validator shared by network config,
// proxy URLs, and proxy targets: a structural split, a bounded host free of
// whitespace / control / authority-delimiter bytes (an IPv6 literal's colons
// survive net.SplitHostPort's bracket handling), and a strict decimal port. It
// returns a neutral error (no wrapping) so each caller can phrase/classify it.
func ValidHostPort(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return errors.New("must be host:port")
	}
	if len(host) > 255 || strings.ContainsAny(host, " \t\r\n/@\\?#") || hasControl(host) {
		return errors.New("malformed host")
	}
	if _, ok := ParsePort(port); !ok {
		return errors.New("port must be a decimal number in 1-65535")
	}
	return nil
}

// ParsePort strictly parses a decimal port: ASCII digits only (strconv.Atoi
// otherwise accepts a leading '+'/'-' and Unicode digits, which are not valid
// in a host:port authority) and in 1..65535. Shared with netconf's endpoint
// validation so both reject the same malformed ports.
func ParsePort(s string) (int, bool) {
	if s == "" || len(s) > 5 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// CredsOverCleartext reports whether a proxy URL carries authentication to a
// non-loopback host. SOCKS5 (RFC 1929) and HTTP Basic proxy auth are sent
// unencrypted, so credentials to a remote proxy travel in the clear unless the
// transport to it is itself protected (VPN / SSH tunnel). Loopback is exempt.
// Used only to emit an advisory warning, not to reject.
func CredsOverCleartext(proxyURL string) bool {
	u, err := url.Parse(proxyURL)
	if err != nil || u.User == nil {
		return false
	}
	host := u.Hostname()
	if host == "" || host == "localhost" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

// Parse validates a proxy configuration string and returns the parsed URL.
//
// Parse errors are FIXED strings that never echo the input: the raw value can
// carry credentials, and no redaction heuristic is airtight (a malformed value
// like "socks5://alice:secret/path@proxy:1080" puts the '@' after the first
// '/', past the authority the redactor scans). Callers that want to identify
// the proxy in a diagnostic must build it structurally from the returned URL's
// scheme+host, never from the raw string.
func Parse(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("%w: unparseable proxy URL", ErrProxyConfig)
	}
	if err := validateProxyURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

// validateProxyURL enforces every structural invariant on a proxy URL. It backs
// BOTH Parse (at persist time) and Dial (at use time), so a directly-constructed
// or post-parse-mutated URL — an unknown scheme, empty host, bad port, stray
// path/query/fragment, or malformed credentials — is rejected before any socket
// is opened, rather than falling through Dial's scheme switch and returning the
// raw proxy connection as a bogus "tunnel".
func validateProxyURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("%w: nil URL", ErrProxyConfig)
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http":
	default:
		return fmt.Errorf("%w: scheme must be socks5 or http", ErrProxyConfig)
	}
	// The RAW authority must be a bare host:port. A manually-constructed URL can
	// smuggle userinfo into Host ("alice:secret@host:1080", which ValidHostPort
	// rejects for the '@') or an extra colon ("alice:secret:1080", rejected as
	// "too many colons") — and a later dial error could echo the secret.
	// Credentials belong in u.User (validated below), never the authority.
	if err := ValidHostPort(u.Host); err != nil {
		return fmt.Errorf("%w: proxy authority %v", ErrProxyConfig, err)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: unexpected path, query, or fragment", ErrProxyConfig)
	}
	if u.User != nil {
		return validateProxyCreds(u.Scheme, u.User)
	}
	return nil
}

// validateProxyTarget vets the "host:port" a caller wants tunneled, BEFORE any
// socket opens and before either handshake writes it verbatim (HTTP CONNECT
// inserts it into a request line; SOCKS sends it as an ATYP-domain field). The
// host must be a plausible hostname or IP — no control/whitespace/authority
// bytes that could inject a header line or corrupt the SOCKS framing, and no
// over-255-byte name — and the port must be numeric and in range.
func validateProxyTarget(target string) error {
	if err := ValidHostPort(target); err != nil {
		return fmt.Errorf("%w: target %v", ErrProxyConfig, err)
	}
	return nil
}

// validateProxyCreds enforces the scheme-specific credential grammar so an
// unrepresentable or malformed value is rejected at parse/persist time, not
// after dialing the proxy. Errors are fixed strings (no credential echo).
func validateProxyCreds(scheme string, user *url.Userinfo) error {
	name := user.Username()
	pass, hasPass := user.Password()
	if scheme == "http" {
		// HTTP Basic (RFC 7617): the field is "user-id ':' password',
		// base64-encoded. A ':' in the user-id is unrepresentable, and
		// control characters would corrupt the header line.
		if strings.ContainsRune(name, ':') {
			return fmt.Errorf("%w: HTTP proxy username must not contain ':'", ErrProxyConfig)
		}
		if hasControl(name) || hasControl(pass) {
			return fmt.Errorf("%w: HTTP proxy credentials must not contain control characters", ErrProxyConfig)
		}
		if len(name) > 255 || len(pass) > 255 {
			return fmt.Errorf("%w: HTTP proxy credentials too long", ErrProxyConfig)
		}
		return nil
	}
	// SOCKS5 username/password subnegotiation (RFC 1929): BOTH fields are
	// 1-255 octets, each length-prefixed by a single byte. Userinfo with a
	// username but no password (socks5://user@host) is not a valid no-auth
	// config and cannot be encoded — reject it rather than send an empty
	// password the server may refuse.
	if !hasPass {
		return fmt.Errorf("%w: SOCKS5 credentials need both a username and a password", ErrProxyConfig)
	}
	if len(name) < 1 || len(name) > 255 || len(pass) < 1 || len(pass) > 255 {
		return fmt.Errorf("%w: SOCKS5 username and password must each be 1-255 bytes", ErrProxyConfig)
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Dial connects to the proxy and tunnels to target (host:port). The whole
// exchange must finish within timeout. The proxy host itself is dialed
// directly (it is operator-configured and trusted — it may legitimately be
// loopback, e.g. a local Tor SOCKS port).
func Dial(ctx context.Context, proxy *url.URL, target string, timeout time.Duration) (net.Conn, error) {
	// Re-validate at use time: a directly-constructed or mutated URL must not
	// reach the scheme switch below (an unknown scheme there would return the
	// raw proxy connection as a bogus tunnel), and the target framing must be a
	// real host:port before we open a socket.
	if err := validateProxyURL(proxy); err != nil {
		return nil, err
	}
	if err := validateProxyTarget(target); err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", proxy.Host)
	if err != nil {
		// Build the diagnostic from the VALIDATED structural fields, never the
		// raw proxy.Host — a malformed authority could otherwise echo a smuggled
		// secret into a log. (validateProxyURL guarantees these are clean.)
		return nil, fmt.Errorf("proxy %s: %w", net.JoinHostPort(proxy.Hostname(), proxy.Port()), err)
	}
	conn.SetDeadline(time.Now().Add(timeout))
	// Honor ctx during the handshake too (shutdown must not block until the
	// deadline): cancellation closes the socket, failing the reads.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	switch proxy.Scheme {
	case "socks5", "socks5h":
		err = socks5Connect(conn, proxy.User, target)
	case "http":
		err = httpConnect(conn, proxy.User, target)
	default:
		// Unreachable after validateProxyURL, but never return an
		// un-handshaked connection as a tunnel.
		conn.Close()
		return nil, fmt.Errorf("%w: unsupported scheme", ErrProxyConfig)
	}
	if err != nil {
		conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("proxy %s: %w", proxy.Host, err)
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

// socks5Connect performs the RFC 1928 handshake and CONNECT on an open
// proxy connection, one phase per helper. Reads are exact-size
// (io.ReadFull), so no stream bytes beyond the handshake are consumed.
func socks5Connect(conn net.Conn, user *url.Userinfo, target string) error {
	// Defensive: Dial validated the target, but a direct caller may not have.
	if err := validateProxyTarget(target); err != nil {
		return err
	}
	if err := socks5Negotiate(conn, user); err != nil {
		return err
	}
	req, err := socks5Request(target)
	if err != nil {
		return err
	}
	if _, err := conn.Write(req); err != nil {
		return err
	}
	return socks5ReadReply(conn)
}

// socks5Negotiate runs method negotiation (RFC 1928 §3) and, when the
// proxy picks it, the username/password subnegotiation (RFC 1929 §2).
func socks5Negotiate(conn net.Conn, user *url.Userinfo) error {
	// Offer no-auth, plus username/password when credentials are configured.
	methods := []byte{0x00}
	if user != nil {
		methods = append(methods, 0x02)
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return err
	}
	var sel [2]byte
	if _, err := io.ReadFull(conn, sel[:]); err != nil {
		return fmt.Errorf("socks5 greeting: %w", err)
	}
	if sel[0] != 0x05 {
		return fmt.Errorf("socks5: not a SOCKS5 proxy (version %d)", sel[0])
	}
	switch sel[1] {
	case 0x00: // no auth
		return nil
	case 0x02:
		return socks5Auth(conn, user)
	default:
		// The proxy accepts none of our offered methods: deterministic, retry
		// won't change it.
		return fmt.Errorf("%w: no acceptable authentication method", ErrProxyRejected)
	}
}

// socks5Auth is the RFC 1929 §2 username/password subnegotiation.
func socks5Auth(conn net.Conn, user *url.Userinfo) error {
	if user == nil {
		return fmt.Errorf("%w: proxy requires authentication but none is configured", ErrProxyRejected)
	}
	// Defensive re-validation before writing (Dial already validated a parsed
	// URL, but a directly-constructed one may reach here): RFC 1929 fields are
	// each 1-255 octets.
	if err := validateProxyCreds("socks5", user); err != nil {
		return err
	}
	pass, _ := user.Password()
	u, p := user.Username(), pass
	req := append([]byte{0x01, byte(len(u))}, u...)
	req = append(append(req, byte(len(p))), p...)
	if _, err := conn.Write(req); err != nil {
		return err
	}
	var st [2]byte
	if _, err := io.ReadFull(conn, st[:]); err != nil {
		return fmt.Errorf("socks5 auth: %w", err)
	}
	if st[0] != 0x01 { // RFC 1929 subnegotiation version
		return fmt.Errorf("%w: bad SOCKS5 auth version %d", ErrProxyProtocol, st[0])
	}
	if st[1] != 0x00 {
		return fmt.Errorf("%w: authentication rejected", ErrProxyRejected)
	}
	return nil
}

// socks5Request builds the CONNECT request (RFC 1928 §4). A hostname is
// sent as ATYP domain so the proxy resolves it (never us — no DNS leak).
func socks5Request(target string) ([]byte, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port, ok := ParsePort(portStr)
	if !ok {
		return nil, fmt.Errorf("%w: bad target port", ErrProxyConfig)
	}
	req := []byte{0x05, 0x01, 0x00}
	switch ip := net.ParseIP(host); {
	case ip == nil:
		if len(host) > 255 {
			return nil, errors.New("socks5: hostname too long")
		}
		req = append(append(req, 0x03, byte(len(host))), host...)
	case ip.To4() != nil:
		req = append(append(req, 0x01), ip.To4()...)
	default:
		req = append(append(req, 0x04), ip.To16()...)
	}
	return append(req, byte(port>>8), byte(port)), nil
}

// socks5ReadReply consumes the CONNECT reply (RFC 1928 §6), including the
// bound address, so the tunneled stream starts clean.
func socks5ReadReply(conn net.Conn) error {
	var head [4]byte // VER REP RSV ATYP
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fmt.Errorf("socks5 reply: %w", err)
	}
	if head[0] != 0x05 || head[2] != 0x00 { // version must be 5, reserved must be 0
		return fmt.Errorf("%w: bad SOCKS5 reply framing (ver=%d rsv=%d)", ErrProxyProtocol, head[0], head[2])
	}
	if head[1] != 0x00 {
		// Classify by RFC 1928 §6 reply code: policy / unsupported-command /
		// unsupported-address are DETERMINISTIC rejections (permanent); a
		// network/host-unreachable, connection-refused, or TTL-expired is a
		// target-side failure that may clear (transient).
		switch head[1] {
		case 0x02, 0x07, 0x08: // not-allowed-by-ruleset, command-not-supported, addr-not-supported
			return fmt.Errorf("%w: %s", ErrProxyRejected, socks5Error(head[1]))
		default:
			return fmt.Errorf("socks5: connect failed: %s", socks5Error(head[1]))
		}
	}
	var bound int
	switch head[3] {
	case 0x01:
		bound = 4
	case 0x04:
		bound = 16
	case 0x03:
		var n [1]byte
		if _, err := io.ReadFull(conn, n[:]); err != nil {
			return err
		}
		bound = int(n[0])
	default:
		return fmt.Errorf("socks5: bad address type %d in reply", head[3])
	}
	_, err := io.ReadFull(conn, make([]byte, bound+2)) // addr + port
	return err
}

// socks5Error maps RFC 1928 §6 reply codes to text.
func socks5Error(code byte) string {
	switch code {
	case 0x01:
		return "general failure"
	case 0x02:
		return "connection not allowed by ruleset"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	}
	return fmt.Sprintf("reply code %d", code)
}

// httpConnect establishes an HTTP CONNECT tunnel. The response is read
// byte-by-byte up to the blank line so nothing past the headers is
// consumed — the tunneled stream must start exactly where the proxy left
// off.
func httpConnect(conn net.Conn, user *url.Userinfo, target string) error {
	// Defensive: the target is inserted VERBATIM into the request line below, so
	// a control byte would inject a header — re-validate for a direct caller.
	if err := validateProxyTarget(target); err != nil {
		return err
	}
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if user != nil {
		// Defensive re-validation before encoding (RFC 7617: no ':' in the
		// user-id, no control chars) — a control char would otherwise inject a
		// header line, a ':' would corrupt the field split.
		if err := validateProxyCreds("http", user); err != nil {
			return err
		}
		pass, _ := user.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(user.Username() + ":" + pass))
		req += "Proxy-Authorization: Basic " + cred + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return err
	}

	var head strings.Builder
	buf := make([]byte, 1)
	for !strings.HasSuffix(head.String(), "\r\n\r\n") {
		if head.Len() > 8192 {
			return errors.New("http proxy: oversized CONNECT response")
		}
		if _, err := io.ReadFull(conn, buf); err != nil {
			return fmt.Errorf("http proxy response: %w", err)
		}
		head.WriteByte(buf[0])
	}
	status, _, _ := strings.Cut(head.String(), "\r\n")
	f := strings.Fields(status)
	// Strict status line: "HTTP/x.y NNN ...". A malformed version or a
	// non-3-digit status is a protocol error (not an HTTP proxy) — permanent.
	if len(f) < 2 || !validHTTPVersion(f[0]) {
		return fmt.Errorf("%w: bad HTTP status line %q", ErrProxyProtocol, status)
	}
	code, ok := parseHTTPStatus(f[1])
	if !ok {
		return fmt.Errorf("%w: bad HTTP status code %q", ErrProxyProtocol, f[1])
	}
	switch {
	case code >= 200 && code < 300:
		return nil // any 2xx establishes the tunnel
	case code == 408 || code == 429 || (code >= 500 && code < 600):
		// Timeout / rate-limit / server error: may clear on retry.
		return fmt.Errorf("http proxy: CONNECT refused: %s", status)
	default:
		// 1xx (no final 2xx), 3xx, and 4xx other than 408/429 are DETERMINISTIC
		// — 407 proxy-auth-required, 403 forbidden, an unexpected redirect:
		// retrying repeats the same answer.
		return fmt.Errorf("%w: CONNECT %d", ErrProxyRejected, code)
	}
}

// validHTTPVersion reports whether tok is "HTTP/<digit>.<digit>".
func validHTTPVersion(tok string) bool {
	const p = "HTTP/"
	v, ok := strings.CutPrefix(tok, p)
	if !ok || len(v) != 3 || v[1] != '.' {
		return false
	}
	return v[0] >= '0' && v[0] <= '9' && v[2] >= '0' && v[2] <= '9'
}

// parseHTTPStatus parses a strict 3-digit HTTP status in 100..599.
func parseHTTPStatus(s string) (int, bool) {
	if len(s) != 3 {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, _ := strconv.Atoi(s)
	if n < 100 || n > 599 {
		return 0, false
	}
	return n, true
}
