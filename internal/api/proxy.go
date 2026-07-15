package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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

// isPublicIP reports whether ip is a globally-routable public address —
// the allowlist gate for outbound proxy fetches. It rejects loopback,
// RFC1918/ULA private ranges, link-local (incl. the 169.254.169.254 cloud
// metadata endpoint), unspecified, multicast, and CGNAT 100.64.0.0/10.
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
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false // CGNAT 100.64.0.0/10
	}
	return true
}
