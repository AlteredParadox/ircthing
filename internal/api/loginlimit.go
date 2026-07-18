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
// which runs bcrypt per attempt: a small semaphore caps concurrent
// hashing (small requests cannot pin the CPU), and a per-source failure
// tracker imposes exponential backoff (1s doubling to a 60s cap).
//
// The source is the connection's remote IP. Behind the expected reverse
// proxy all attempts share the proxy's IP, so a sustained attack also
// briefly locks out the legitimate user — an accepted trade-off for a
// single-user daemon; the proxy should rate-limit /api/login as well.
type loginLimiter struct {
	sem chan struct{}

	mu      sync.Mutex
	sources map[string]*loginSource
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
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		sem:     make(chan struct{}, 2),
		sources: make(map[string]*loginSource),
	}
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

// forwardedClientIP is the client address a trusted single-hop proxy reports:
// X-Real-IP (the single immediate-peer value nginx/Caddy can set), else the
// LAST X-Forwarded-For entry — the proxy appends the address it received from
// AFTER any client-supplied values, so on a single trusted hop it is the real
// client and can't be spoofed.
func forwardedClientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	return ""
}
