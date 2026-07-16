package irc

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

// Per-network proxy support: SOCKS5 (RFC 1928, with RFC 1929
// username/password auth) and HTTP CONNECT tunnels. Hand-rolled on
// purpose — both handshakes are a few dozen bytes, which beats adding a
// dependency for them (CLAUDE.md dependency policy).
//
// The proxy is configured as a URL: "socks5://host:port",
// "socks5://user:pass@host:port" or "http://host:port". For SOCKS5 the
// target hostname is passed to the proxy unresolved (ATYP domain), so
// DNS happens proxy-side — what Tor and friends expect; "socks5h" is
// accepted as an alias for the same behavior.

// parseProxyURL validates a proxy configuration string.
func parseProxyURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("irc: config: proxy %q: %w", s, err)
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http":
	default:
		return nil, fmt.Errorf("irc: config: proxy %q: scheme must be socks5 or http", s)
	}
	if u.Host == "" || u.Port() == "" {
		return nil, fmt.Errorf("irc: config: proxy %q: host:port required", s)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("irc: config: proxy %q: unexpected path", s)
	}
	return u, nil
}

// dialProxy connects to the proxy and tunnels to target (host:port).
// The whole exchange must finish within timeout.
func dialProxy(ctx context.Context, proxy *url.URL, target string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", proxy.Host)
	if err != nil {
		return nil, fmt.Errorf("proxy %s: %w", proxy.Host, err)
	}
	conn.SetDeadline(time.Now().Add(timeout))
	// Honor ctx during the handshake too (shutdown must not block until
	// the deadline): cancellation closes the socket, failing the reads.
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
// proxy connection. Reads are exact-size (io.ReadFull), so no stream
// bytes beyond the handshake are consumed.
func socks5Connect(conn net.Conn, user *url.Userinfo, target string) error {
	// Method negotiation (RFC 1928 §3): no-auth, plus username/password
	// when credentials are configured.
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
	case 0x02: // username/password subnegotiation (RFC 1929 §2)
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
	default:
		return errors.New("socks5: no acceptable authentication method")
	}

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("socks5: bad target port %q", portStr)
	}
	req := []byte{0x05, 0x01, 0x00} // CONNECT request (RFC 1928 §4)
	switch ip := net.ParseIP(host); {
	case ip == nil: // hostname: resolved by the proxy (ATYP 0x03, domain)
		if len(host) > 255 {
			return errors.New("socks5: hostname too long")
		}
		req = append(append(req, 0x03, byte(len(host))), host...)
	case ip.To4() != nil:
		req = append(append(req, 0x01), ip.To4()...)
	default:
		req = append(append(req, 0x04), ip.To16()...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return err
	}

	var head [4]byte // reply VER REP RSV ATYP (RFC 1928 §6)
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fmt.Errorf("socks5 reply: %w", err)
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: connect failed: %s", socks5Error(head[1]))
	}
	// Consume the bound address so the stream starts clean.
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
	_, err = io.ReadFull(conn, make([]byte, bound+2)) // addr + port
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
// consumed — the tunneled stream must start exactly where the proxy
// left off.
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
