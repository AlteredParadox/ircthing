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
)

// Server-side fetcher for the media proxy (link previews, image
// thumbnails). Browsers never fetch remote content directly — this is the
// single choke point, and it is hardened against SSRF: the dialer
// re-validates the *resolved* IP of every connection (and every redirect
// hop) against a public-address policy, which defeats DNS rebinding
// because the check happens at connect time, not on the hostname.

const proxyUserAgent = "ircthing-media-proxy/1.0 (+https://github.com/ircthing)"

// mediaSlots is the media-path concurrency: each slot admits one whole
// preview or thumbnail request (fetch through decode/parse and encode),
// bounding in-flight bodies and decoded bitmaps together.
const mediaSlots = 2

// mediaAcquireWait bounds how long a request waits for a slot (var for
// tests).
var mediaAcquireWait = 5 * time.Second

// acquireMedia takes a media slot, giving up after a short bounded wait
// (or when the request dies) so waiters cannot pile up.
func (s *Server) acquireMedia(ctx context.Context) bool {
	t := time.NewTimer(mediaAcquireWait)
	defer t.Stop()
	select {
	case s.mediaSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

func (s *Server) releaseMedia() {
	<-s.mediaSem
}

var (
	errBadURL   = errors.New("proxy: url must be an absolute http(s) URL")
	errTooLarge = errors.New("proxy: response exceeds size cap")
	errBlocked  = errors.New("proxy: refusing to connect to a non-public address")
)

type fetcher struct {
	client   *http.Client
	maxBytes int64
	// allowIP decides whether a resolved address may be dialed. Field so
	// tests can permit loopback (httptest listens on 127.0.0.1, which the
	// real policy blocks).
	allowIP func(net.IP) bool
}

func newFetcher(maxBytes int64) *fetcher {
	f := &fetcher{maxBytes: maxBytes, allowIP: isPublicIP}
	d := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: -1}
	// Control runs after DNS resolution with the concrete IP:port, before
	// the socket connects — the correct, rebinding-safe hook.
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
	f.client = &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil, // never use an outbound proxy
			DialContext:           d.DialContext,
			TLSHandshakeTimeout:   8 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
			DisableKeepAlives:     true,
			MaxIdleConns:          0,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("proxy: too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("proxy: disallowed redirect scheme")
			}
			return nil
		},
	}
	return f
}

// get fetches rawURL, returning its content type and body (capped at
// maxBytes). Only absolute http(s) URLs are allowed.
func (f *fetcher) get(ctx context.Context, rawURL string) (contentType string, body []byte, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", nil, errBadURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("User-Agent", proxyUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("proxy: upstream status %d", resp.StatusCode)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return "", nil, err
	}
	if int64(len(body)) > f.maxBytes {
		return "", nil, errTooLarge
	}
	return resp.Header.Get("Content-Type"), body, nil
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
	return true
}
