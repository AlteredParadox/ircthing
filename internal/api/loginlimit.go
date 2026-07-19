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
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginLimiter bounds the cost of the unauthenticated login endpoint,
// which runs bcrypt per attempt, in three layers: a global token bucket
// caps total attempt rate (rotating source addresses cannot buy
// unlimited bcrypt work), a small semaphore caps concurrent hashing
// (small requests cannot pin the CPU), and a per-source failure tracker
// imposes exponential backoff (1s doubling to a 60s cap).
//
// The source is the connection's remote IP. Behind the expected reverse
// proxy all attempts share the proxy's IP, so a sustained attack also
// briefly locks out the legitimate user — an accepted trade-off for a
// single-user daemon; the proxy should rate-limit /api/login as well.
type loginLimiter struct {
	sem chan struct{}

	mu      sync.Mutex
	sources map[string]*loginSource

	// Global attempt bucket. Refilled lazily on each globalAllow call.
	tokens     float64
	lastRefill time.Time
}

type loginSource struct {
	failures     int
	blockedUntil time.Time
}

const (
	loginBackoffBase = time.Second
	loginBackoffMax  = time.Minute
	loginAcquireWait = 2 * time.Second
	loginSourcesMax  = 1024

	// Global attempt budget. Per-source backoff alone cannot stop an
	// attacker rotating source addresses — every fresh source gets a free
	// first attempt, enough to keep both bcrypt slots pinned on the 1-vCPU
	// target. One token per second (burst 5) is far above any human login
	// cadence and bounds worst-case bcrypt CPU to a few percent of a core.
	loginGlobalRate  = 1.0 // tokens per second
	loginGlobalBurst = 5.0
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		sem:        make(chan struct{}, 2),
		sources:    make(map[string]*loginSource),
		tokens:     loginGlobalBurst,
		lastRefill: time.Now(),
	}
}

// globalAllow consumes one attempt from the global bucket, reporting 0 when
// the attempt may proceed and otherwise how long until a token is available.
// Unlike the per-source tracker it charges every attempt before hashing, so
// rotating source addresses cannot buy unlimited bcrypt work.
func (l *loginLimiter) globalAllow(now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if elapsed := now.Sub(l.lastRefill).Seconds(); elapsed > 0 {
		l.tokens = min(l.tokens+elapsed*loginGlobalRate, loginGlobalBurst)
		l.lastRefill = now
	}
	if l.tokens >= 1 {
		l.tokens--
		return 0
	}
	return time.Duration((1 - l.tokens) / loginGlobalRate * float64(time.Second))
}

// retryAfter reports how long the source is still blocked (0 = allowed).
func (l *loginLimiter) retryAfter(source string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.sources[source]; s != nil && now.Before(s.blockedUntil) {
		return s.blockedUntil.Sub(now)
	}
	return 0
}

// fail records a failed attempt: the next one is allowed only after an
// exponentially growing delay.
func (l *loginLimiter) fail(source string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sources[source]
	if s == nil {
		// A full table only prunes expired entries — an attacker
		// spreading across sources cannot evict active blocks.
		if len(l.sources) >= loginSourcesMax {
			for k, v := range l.sources {
				if now.After(v.blockedUntil) {
					delete(l.sources, k)
				}
			}
		}
		if len(l.sources) >= loginSourcesMax {
			return
		}
		s = &loginSource{}
		l.sources[source] = s
	}
	shift := min(s.failures, 6) // 1s .. 64s, clamped below
	s.failures++
	s.blockedUntil = now.Add(min(loginBackoffBase<<shift, loginBackoffMax))
}

// ok clears a source after a successful login.
func (l *loginLimiter) ok(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.sources, source)
}

// acquire takes a bcrypt slot, giving up after a short bounded wait so
// waiting requests cannot pile up indefinitely.
func (l *loginLimiter) acquire(ctx context.Context) bool {
	t := time.NewTimer(loginAcquireWait)
	defer t.Stop()
	select {
	case l.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

func (l *loginLimiter) release() {
	<-l.sem
}

// loginSourceKey identifies the attempt source for backoff. Behind a reverse
// proxy every attempt would otherwise share the proxy's IP, so one attacker's
// sustained failures lock out the real user (and even authenticated password
// changes) — a public denial of service. When TrustProxyForwarded is set (the
// deployment is behind a trusted single-hop proxy) it uses the forwarded
// client IP instead, giving each real client its own backoff bucket. These
// headers are client-settable, so they are consulted ONLY under that flag.
func (s *Server) loginSourceKey(r *http.Request) string {
	if s.cfg.TrustProxyForwarded {
		if ip := forwardedClientIP(r); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// forwardedClientIP is the client address a trusted single-hop reverse proxy
// reports in the LAST X-Forwarded-For entry. On a single trusted hop the proxy
// appends the address it accepted the connection from AFTER any client-supplied
// XFF values, so the last entry is the real client and cannot be spoofed;
// earlier entries are attacker-controlled and ignored. Returns "" when there is
// no XFF or the last entry is not a valid IP (fall back to the socket peer).
//
// X-Real-IP is deliberately NOT consulted: the recommended reverse proxy (Caddy)
// forwards a client-supplied X-Real-IP UNCHANGED by default (it sanitizes only
// X-Forwarded-*), so trusting it would let an attacker rotate the header to
// escape login backoff or spoof a victim's IP to poison their bucket. Deploy
// behind_proxy only with a proxy that appends the client to X-Forwarded-For
// (Caddy does by default; nginx: proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for).
func forwardedClientIP(r *http.Request) string {
	// Take the last entry of the LAST X-Forwarded-For header line. Get returns
	// only the first line, so a proxy that appends the client as a SEPARATE
	// header line (HAProxy's default) would otherwise leave the attacker's
	// own first line as the key — letting them charge a victim's backoff
	// bucket. On a single trusted hop the last hop the proxy adds is the real
	// client, whichever style it uses.
	vals := r.Header.Values("X-Forwarded-For")
	if len(vals) == 0 {
		return ""
	}
	parts := strings.Split(vals[len(vals)-1], ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if net.ParseIP(last) == nil {
		return ""
	}
	return last
}
