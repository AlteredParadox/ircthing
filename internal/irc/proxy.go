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
