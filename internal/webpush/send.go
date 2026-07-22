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

package webpush

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"ircthing/internal/netguard"
)

// ErrGone reports that the push service no longer knows the subscription
// (RFC 8030: 404 expired, 410 gone) — the caller should delete it.
var ErrGone = errors.New("webpush: subscription gone")

// Subscription is one browser push endpoint: the push-service URL plus
// the client keys from PushSubscription.getKey() (RFC 8291 §2/§3.1).
type Subscription struct {
	Endpoint string
	P256dh   []byte // 65-octet uncompressed P-256 point
	Auth     []byte // 16-octet auth secret
}

// ValidateEndpoint vets a subscription endpoint URL at registration time.
// The endpoint is client-supplied, so it gets the media proxy's SSRF
// posture: https only, a real hostname, a sane port, and literal IPs must
// be public (hostnames are re-validated at dial time by the Sender).
func ValidateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webpush: bad endpoint: %w", err)
	}
	if u.Scheme != "https" {
		return errors.New("webpush: endpoint must be https")
	}
	if u.Hostname() == "" {
		return errors.New("webpush: endpoint has no host")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("webpush: endpoint has a bad port")
		}
	}
	if u.User != nil {
		return errors.New("webpush: endpoint must not carry credentials")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !netguard.IsPublicIP(ip) {
		return errors.New("webpush: endpoint IP is not public")
	}
	return nil
}

// Sender delivers encrypted push messages (RFC 8030 §5). One instance is
// shared by all deliveries; it caches a VAPID token per push-service
// origin so the ECDSA sign amortizes across messages.
type Sender struct {
	client  *http.Client
	key     *ecdsa.PrivateKey
	subject string // VAPID sub claim (contact URI, RFC 8292 §2.1)

	mu     sync.Mutex
	tokens map[string]cachedToken

	// insecure permits http:// and non-public IPs so same-package tests
	// can target an httptest loopback server. Never set outside tests.
	insecure bool
}

type cachedToken struct {
	header  string
	refresh time.Time
}

// NewSender builds a Sender around the persistent VAPID key. The HTTP
// client mirrors the media fetcher's discipline (internal/api newFetcher):
// bounded timeouts everywhere, no implicit proxy, no redirects, and a
// connect-time Control hook that re-validates the RESOLVED IP so a
// hostname endpoint cannot rebind into private address space.
func NewSender(key *ecdsa.PrivateKey, subject string) *Sender {
	s := &Sender{key: key, subject: subject, tokens: make(map[string]cachedToken)}
	d := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: -1}
	d.Control = func(_, address string, _ syscall.RawConn) error {
		if s.insecure {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !netguard.IsPublicIP(ip) {
			return fmt.Errorf("webpush: refusing non-public address %s", address)
		}
		return nil
	}
	s.client = &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:                  nil, // never honor $HTTP_PROXY implicitly
			DialContext:            d.DialContext,
			TLSHandshakeTimeout:    8 * time.Second,
			ResponseHeaderTimeout:  8 * time.Second,
			MaxResponseHeaderBytes: 64 << 10,
			DisableKeepAlives:      true,
			MaxIdleConns:           0,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// A push service has no business redirecting a delivery; a
			// redirect could also escape the vetted origin.
			return errors.New("webpush: refusing redirect")
		},
	}
	return s
}

// Send POSTs one encrypted message body to a subscription (RFC 8030 §5).
// TTL is required by §5.2 (seconds the service may retain the message);
// urgency per §5.3. Returns ErrGone when the subscription should be
// deleted; other failures are transient or misconfiguration — the push
// service owns retries, we never re-send.
func (s *Sender) Send(ctx context.Context, sub Subscription, body []byte, ttl int) error {
	if err := ValidateEndpoint(sub.Endpoint); err != nil && !s.insecure {
		return err
	}
	auth, err := s.auth(sub.Endpoint)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("TTL", strconv.Itoa(ttl))
	req.Header.Set("Urgency", "high") // a highlight IS the urgent case; anything less is filtered on constrained devices
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", auth)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain a little for connection hygiene, never trust the size.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return fmt.Errorf("%w (HTTP %d)", ErrGone, resp.StatusCode)
	default:
		return fmt.Errorf("webpush: push service returned HTTP %d", resp.StatusCode)
	}
}

// auth returns the (possibly cached) VAPID Authorization value for the
// endpoint's origin. Tokens are refreshed an hour before expiry so a
// delivery never rides an about-to-expire JWT through clock skew.
func (s *Sender) auth(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	// aud must be the RFC 6454 §6.1 origin serialization (RFC 8292 §2):
	// lowercased host, default port omitted. An endpoint carrying an
	// explicit :443 would otherwise yield aud="https://host:443", which
	// strict services (Mozilla autopush) reject with 401 — an error that
	// never prunes, so that subscription would fail forever. Scheme is
	// https for every validated endpoint; http exists only under the
	// test-only insecure flag.
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // Hostname() strips a literal IPv6's brackets
	}
	if p := u.Port(); p != "" && !(p == "443" && u.Scheme == "https") && !(p == "80" && u.Scheme == "http") {
		host += ":" + p
	}
	origin := u.Scheme + "://" + host
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tokens[origin]; ok && now.Before(t.refresh) {
		return t.header, nil
	}
	header, err := vapidAuth(s.key, origin, s.subject, now)
	if err != nil {
		return "", err
	}
	// Bound the cache: origins accumulate across register/prune cycles
	// (the live-subscription cap does not retire dead origins' entries).
	// Reset-when-full beats LRU bookkeeping here — regeneration costs one
	// ECDSA sign, and real deployments see a handful of vendor origins.
	if len(s.tokens) >= maxCachedOrigins {
		s.tokens = make(map[string]cachedToken, 4)
	}
	s.tokens[origin] = cachedToken{header: header, refresh: now.Add(vapidTokenTTL - time.Hour)}
	return header, nil
}

// maxCachedOrigins bounds the VAPID token cache.
const maxCachedOrigins = 32
