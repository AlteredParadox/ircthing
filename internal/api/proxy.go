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

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	"ircthing/internal/proxydial"
)

// Server-side fetcher for the media proxy (link previews, image
// thumbnails). Browsers never fetch remote content directly — this is the
// single choke point. SSRF hardening differs by path:
//
//   - Direct fetches are fully hardened: the dialer re-validates the
//     *resolved* IP of every connection (and every redirect hop) against a
//     public-address policy at connect time, which defeats DNS rebinding
//     because the check is on the IP, not the hostname.
//   - Proxied fetches (through a network's proxy) can only refuse literal
//     non-public IP targets (hostAllowed). The proxy owns DNS, so a hostname
//     that resolves proxy-side to an internal/loopback/metadata address is
//     NOT caught here — such a request can reach whatever the proxy can
//     reach. This is inherent to preserving proxy-side DNS; the mitigation
//     is the proxy's own egress policy (Tor exits publicly and cannot reach
//     your LAN; a plain LAN SOCKS proxy can), or disabling previews for a
//     network whose proxy you don't trust to restrict destinations.

const proxyUserAgent = "ircthing-media-proxy/1.0 (+https://github.com/ircthing)"

// mediaSlots is the IMAGE-DECODE concurrency: each slot admits one whole
// thumbnail request (fetch through decode + encode), bounding in-flight bodies
// and decoded bitmaps together. One slot: a single worst-case decode (10 MiB
// body + ~36 MiB bitmap) fits comfortably under the unit's MemoryMax; two could
// approach it and OOM-restart the bouncer. Link previews are NOT decode-bound,
// so they use previewSlots instead — serializing them behind a slow thumbnail
// decode made previews trickle in one-at-a-time over slow (WireGuard/Tor)
// egress, some 503-ing out entirely.
const mediaSlots = 1

// previewSlots is the LINK-PREVIEW (HTML fetch + parse) concurrency. A preview
// only holds a <=512 KiB HTML body and no decoded bitmap (see maxHTMLBytes), so
// several run at once cheaply (~4 x 512 KiB) without the decode memory bound —
// they no longer wait behind image thumbnails.
const previewSlots = 4

// mediaAcquireWait bounds how long a request waits for a slot (var for
// tests).
var mediaAcquireWait = 5 * time.Second

func (s *Server) acquireMedia(ctx context.Context) bool   { return acquireSlot(ctx, s.mediaSem) }
func (s *Server) releaseMedia()                           { <-s.mediaSem }
func (s *Server) acquirePreview(ctx context.Context) bool { return acquireSlot(ctx, s.previewSem) }
func (s *Server) releasePreview()                         { <-s.previewSem }

// acquireSlot takes a slot on sem, giving up after a short bounded wait (or when
// the request dies) so waiters cannot pile up.
func acquireSlot(ctx context.Context, sem chan struct{}) bool {
	t := time.NewTimer(mediaAcquireWait)
	defer t.Stop()
	select {
	case sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

var (
	errBadURL   = errors.New("proxy: url must be an absolute http(s) URL")
	errTooLarge = errors.New("proxy: response exceeds size cap")
	errBlocked  = errors.New("proxy: refusing to connect to a non-public address")
)

type fetcher struct {
	client   *http.Client
	maxBytes int64
	// proxied is true when fetches tunnel through a configured proxy. Then
	// the connect-time Control hook can't see the (proxy-resolved) target
	// IP, so hostAllowed is applied to literal-IP targets in get and on
	// redirects instead. Direct fetches rely solely on the Control hook.
	proxied bool
	// truncate reads at most maxBytes and USES that prefix instead of failing
	// when the body is larger. Set for HTML link-preview fetches: the og/title
	// metadata lives in <head> at the top of the document, so a large page
	// (Next.js/GitBook sites routinely exceed 512 KiB) yields a good preview
	// from its head. Left false for image fetches, where a truncated body is a
	// corrupt image and must be rejected (errTooLarge).
	truncate bool
	// allowIP decides whether a resolved address may be dialed. Field so
	// tests can permit loopback (httptest listens on 127.0.0.1, which the
	// real policy blocks).
	allowIP func(net.IP) bool
}

// newFetcher builds a media fetcher. When proxy is non-nil, every fetch is
// tunneled through it (SOCKS5/HTTP), so the fetch does not reveal the
// server's real IP — matching a network's proxy for anonymity. Direct and
// proxied paths both refuse a literal non-public IP target (hostAllowed);
// the direct path additionally re-validates the resolved IP at connect
// time (rebinding-safe), while the proxied path leaves DNS to the proxy.
func newFetcher(maxBytes int64, proxy *url.URL) *fetcher {
	f := &fetcher{maxBytes: maxBytes, allowIP: isPublicIP}
	f.client = &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:                  nil, // never honor $HTTP_PROXY implicitly
			DialContext:            f.dialContext(proxy),
			TLSHandshakeTimeout:    8 * time.Second,
			ResponseHeaderTimeout:  8 * time.Second,
			MaxResponseHeaderBytes: 64 << 10, // hostile targets can otherwise stream ~10 MiB of headers (Go default) outside the body budget
			DisableKeepAlives:      true,
			MaxIdleConns:           0,
		},
		CheckRedirect: f.checkRedirect,
	}
	return f
}

// newTunnelFetcher builds a media fetcher that dials every target through the
// supplied dial function — a network's in-process WireGuard tunnel. Like the
// proxied path, the tunnel owns egress AND DNS (targets resolve in-tunnel), so
// the connect-time Control hook can't see the resolved IP: SSRF handling is the
// proxied mode (literal non-public IPs refused up front and on redirects,
// hostname resolution deferred to the tunnel — the same trust model as a
// SOCKS5 proxy). The dial func is tunnel-only: if the tunnel is down it returns
// an error and the fetch fails closed, never falling back to a direct dial.
func newTunnelFetcher(maxBytes int64, dial func(ctx context.Context, addr string) (net.Conn, error)) *fetcher {
	f := &fetcher{maxBytes: maxBytes, allowIP: isPublicIP, proxied: true}
	f.client = &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dial(ctx, addr)
			},
			TLSHandshakeTimeout:    8 * time.Second,
			ResponseHeaderTimeout:  8 * time.Second,
			MaxResponseHeaderBytes: 64 << 10, // hostile targets can otherwise stream ~10 MiB of headers (Go default) outside the body budget
			DisableKeepAlives:      true,
			MaxIdleConns:           0,
		},
		CheckRedirect: f.checkRedirect,
	}
	return f
}

// dialContext returns the transport dialer. Proxied: tunnel through the
// proxy (which owns egress + DNS); direct: a plain dialer whose Control
// hook re-validates the resolved IP at connect time (rebinding-safe).
func (f *fetcher) dialContext(proxy *url.URL) func(context.Context, string, string) (net.Conn, error) {
	if proxy != nil {
		f.proxied = true
		return func(ctx context.Context, _, addr string) (net.Conn, error) {
			return proxydial.Dial(ctx, proxy, addr, 8*time.Second)
		}
	}
	d := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: -1}
	d.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !f.allowIP(ip) {
			return fmt.Errorf("%w: %s", errBlocked, address)
		}
		return nil
	}
	return d.DialContext
}

func (f *fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("proxy: too many redirects")
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return errors.New("proxy: disallowed redirect scheme")
	}
	// The proxied path has no connect-time IP check, so re-apply the literal
	// non-public-IP block on each redirect target.
	if f.proxied && !f.hostAllowed(req.URL.Hostname()) {
		return fmt.Errorf("%w: %s", errBlocked, req.URL.Host)
	}
	// Go's client sets a Referer on redirects; strip it so the full
	// (possibly signed/private) preview URL does not leak into the
	// redirect target's or an intermediary's logs.
	req.Header.Del("Referer")
	return nil
}

// hostAllowed rejects a target whose host is a literal non-public IP. A
// hostname passes: the direct path re-checks its resolved IP at dial via
// the Control hook, and the proxied path defers resolution to the proxy.
func (f *fetcher) hostAllowed(host string) bool {
	ip := net.ParseIP(host)
	return ip == nil || f.allowIP(ip)
}

// get fetches rawURL, returning its content type, the FINAL URL after any
// redirects, and body (capped at maxBytes). Only absolute http(s) URLs are
// allowed. The final URL lets the caller resolve relative assets (og:image)
// against the page they actually came from, not the pre-redirect target.
func (f *fetcher) get(ctx context.Context, rawURL string) (contentType, finalURL string, body []byte, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", nil, errBadURL
	}
	// Proxied path: refuse a literal non-public IP up front, since the
	// Control hook can't re-check the proxy-resolved address at dial time.
	// (Direct fetches are covered by that hook.)
	if f.proxied && !f.hostAllowed(u.Hostname()) {
		return "", "", nil, errBlocked
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", nil, err
	}
	req.Header.Set("User-Agent", proxyUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", nil, fmt.Errorf("proxy: upstream status %d", resp.StatusCode)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return "", "", nil, err
	}
	if int64(len(body)) > f.maxBytes {
		if !f.truncate {
			return "", "", nil, errTooLarge
		}
		// HTML preview: keep the head-bearing prefix rather than dropping the
		// whole page. extractMeta only scans up to </head> anyway.
		body = body[:f.maxBytes]
	}
	finalURL = rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String() // the URL after redirects
	}
	return resp.Header.Get("Content-Type"), finalURL, body, nil
}

// specialPurposePrefixes are the IANA special-purpose registry blocks
// (plus multicast/reserved space) that net.IP's classification methods
// do not cover — "not private" is weaker than "globally routable".
// Tables: iana.org/assignments/iana-ipv4-special-registry and
// iana-ipv6-special-registry (fetched 2026-07-18).
var specialPurposePrefixes = func() []netip.Prefix {
	specs := []string{
		// IPv4
		"0.0.0.0/8",       // "this network"
		"192.0.0.0/24",    // protocol assignments (incl. 192.0.0.0/29 DS-Lite)
		"192.0.2.0/24",    // TEST-NET-1
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"192.31.196.0/24", // AS112-v4
		"192.52.193.0/24", // AMT
		"192.88.99.0/24",  // deprecated 6to4 relay anycast
		"192.175.48.0/24", // AS112 direct delegation
		"100.64.0.0/10",   // CGNAT shared address space
		"198.18.0.0/15",   // benchmarking
		"240.0.0.0/4",     // reserved (incl. 255.255.255.255 broadcast)
		// IPv6
		"::/128",        // unspecified (also caught by IsUnspecified)
		"::1/128",       // loopback (also caught by IsLoopback)
		"::ffff:0:0/96", // IPv4-mapped (unwrapped before this check)
		// Deprecated IPv4-embedding forms (RFC 5156): like NAT64, these
		// encode an IPv4 destination, and a lingering translation or
		// tunnel route would reach IPv4 space this policy blocks. An
		// SSRF allowlist has no use for deprecated transition space.
		"::/96",           // IPv4-compatible (deprecated, RFC 4291 §2.5.5.1)
		"::ffff:0:0:0/96", // IPv4-translated (SIIT, RFC 2765)
		// NAT64 translation prefixes embed an IPv4 destination: on a
		// NAT64 host, 64:ff9b::a9fe:a9fe reaches 169.254.169.254 even
		// though the direct IPv4 form is blocked. The whole well-known
		// prefix is denied (site-specific NSP prefixes cannot be known
		// statically and are the deployment's responsibility).
		"64:ff9b::/96",   // well-known NAT64 (RFC 6052)
		"64:ff9b:1::/48", // local-use IPv4/IPv6 translation (RFC 8215)
		"100::/64",       // discard-only
		"2001::/23",      // protocol assignments (TEREDO, ORCHID, benchmarking)
		"2001:db8::/32",  // documentation
		"2002::/16",      // 6to4
		"3fff::/20",      // documentation (RFC 9637)
		"5f00::/16",      // segment routing
	}
	out := make([]netip.Prefix, len(specs))
	for i, s := range specs {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}()

// isPublicIP reports whether ip is a globally-routable public address —
// the allowlist gate for outbound proxy fetches. It rejects loopback,
// RFC1918/ULA private ranges, link-local (incl. the 169.254.169.254
// cloud metadata endpoint), unspecified, multicast, CGNAT, and the IANA
// special-purpose blocks (documentation, benchmarking, 0/8, 240/4,
// translation/transition ranges).
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, p := range specialPurposePrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	// IPv6 allowlist backstop: require global unicast (2000::/3). The
	// denylist above is necessarily incomplete for non-global space that
	// net.IP's helpers do not classify — e.g. deprecated site-local
	// fec0::/10 (RFC 3879), which a host with a lingering internal route
	// could still reach. Everything routable on the public Internet lives
	// in 2000::/3; reject anything outside it.
	if addr.Is6() && !globalUnicastV6.Contains(addr) {
		return false
	}
	return true
}

// globalUnicastV6 is the IPv6 global-unicast block (RFC 4291 §2.4). It
// backs the isPublicIP allowlist so non-global IPv6 space (site-local,
// ULA, link-local, multicast) is rejected regardless of the denylist.
var globalUnicastV6 = netip.MustParsePrefix("2000::/3")
