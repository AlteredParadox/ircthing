package irc

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"ircthing/internal/proxydial"
)

// Per-network proxy support (SOCKS5 with RFC 1929 auth, or HTTP CONNECT)
// lives in internal/proxydial, shared with the media proxy. These thin
// wrappers keep the irc-local call sites and error prefixes stable.

// parseProxyURL validates a per-network proxy configuration string.
func parseProxyURL(s string) (*url.URL, error) {
	u, err := proxydial.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("irc: config: %w", err)
	}
	return u, nil
}

// dialProxy connects to the proxy and tunnels to target (host:port),
// finishing within timeout.
func dialProxy(ctx context.Context, proxy *url.URL, target string, timeout time.Duration) (net.Conn, error) {
	return proxydial.Dial(ctx, proxy, target, timeout)
}
