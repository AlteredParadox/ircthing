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

// redactProxy masks any userinfo (user:pass@) in a proxy URL string so
// SOCKS5/HTTP proxy credentials never reach error messages or logs. It
// handles both the scheme form ("socks5://user:pass@host") and a scheme-less
// value ("user:pass@host") that bypassed the scheme check, so a malformed
// config can't echo credentials back through a validation error.
func redactProxy(s string) string {
	// Mask the userinfo (user:pass@). Go/net-url treats the LAST '@' in the
	// authority as the delimiter, so masking only through the FIRST '@' would
	// leak a password containing a literal '@'. Restrict the search to the
	// authority (before the first '/') so an '@' in a path isn't mistaken for
	// userinfo. Handles the scheme and scheme-less forms alike.
	start := 0
	if i := strings.Index(s, "://"); i != -1 {
		start = i + 3
	}
	authEnd := len(s)
	if slash := strings.IndexByte(s[start:], '/'); slash != -1 {
		authEnd = start + slash
	}
	if at := strings.LastIndexByte(s[start:authEnd], '@'); at != -1 {
		return s[:start] + "<redacted>@" + s[start+at+1:]
	}
	return s
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
func Parse(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		// Do NOT wrap the url.Parse error: *url.Error embeds the raw input,
		// which can carry proxy credentials. Report only the redacted form.
		return nil, fmt.Errorf("proxy %q: invalid proxy URL", redactProxy(s))
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http":
	default:
		return nil, fmt.Errorf("proxy %q: scheme must be socks5 or http", redactProxy(s))
	}
	if u.Host == "" || u.Port() == "" {
		return nil, fmt.Errorf("proxy %q: host:port required", redactProxy(s))
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("proxy %q: unexpected path", redactProxy(s))
	}
	return u, nil
}

// Dial connects to the proxy and tunnels to target (host:port). The whole
// exchange must finish within timeout. The proxy host itself is dialed
// directly (it is operator-configured and trusted — it may legitimately be
// loopback, e.g. a local Tor SOCKS port).
func Dial(ctx context.Context, proxy *url.URL, target string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", proxy.Host)
	if err != nil {
		return nil, fmt.Errorf("proxy %s: %w", proxy.Host, err)
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
		return errors.New("socks5: no acceptable authentication method")
	}
}

// socks5Auth is the RFC 1929 §2 username/password subnegotiation.
func socks5Auth(conn net.Conn, user *url.Userinfo) error {
	if user == nil {
		return errors.New("socks5: proxy requires authentication but none is configured")
	}
	pass, _ := user.Password()
	u, p := user.Username(), pass
	if len(u) > 255 || len(p) > 255 {
		return errors.New("socks5: username/password too long")
	}
	req := append([]byte{0x01, byte(len(u))}, u...)
	req = append(append(req, byte(len(p))), p...)
	if _, err := conn.Write(req); err != nil {
		return err
	}
	var st [2]byte
	if _, err := io.ReadFull(conn, st[:]); err != nil {
		return fmt.Errorf("socks5 auth: %w", err)
	}
	if st[1] != 0x00 {
		return errors.New("socks5: authentication rejected")
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
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("socks5: bad target port %q", portStr)
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
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: connect failed: %s", socks5Error(head[1]))
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
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if user != nil {
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
	if len(f) < 2 || !strings.HasPrefix(f[0], "HTTP/") {
		return fmt.Errorf("http proxy: malformed response %q", status)
	}
	if f[1] != "200" {
		return fmt.Errorf("http proxy: CONNECT refused: %s", status)
	}
	return nil
}
