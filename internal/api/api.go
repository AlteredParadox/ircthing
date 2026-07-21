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

// Package api provides the HTTP layer: session-cookie auth, the
// WebSocket sync endpoint (bridging connections to hub.Sessions), and the
// embedded frontend. HTTP fallbacks (media proxy, search) come later.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/bcrypt"

	"ircthing"
	"ircthing/internal/hub"
)

const sessionCookie = "ircthing_session"

// wsEnvelopeHeadroom is slack above the largest payload (a prefs blob)
// for the JSON envelope wrapping it (v/type/seq/data keys and quoting).
const wsEnvelopeHeadroom = 16 * 1024

// sessionRecheckInterval is how often a live WebSocket re-validates its
// session token — the EXPIRY backstop (logout and password rotation revoke
// live sockets immediately via wsCancels). Atomic (nanoseconds) so tests can
// adjust it while socket goroutines that read it are still running.
var sessionRecheckInterval atomic.Int64

func init() { sessionRecheckInterval.Store(int64(30 * time.Second)) }

// maxSessions caps concurrently valid login sessions; the oldest is
// evicted at issue time. Generous for one user across devices/tabs.
const maxSessions = 128

type Config struct {
	Username     string
	PasswordHash string        // bcrypt hash of the user's password
	SessionTTL   time.Duration // default 30 days
	// SecureCookies marks session cookies Secure (HTTPS-only). Enable
	// whenever TLS terminates in front of the binary; off by default
	// because the default deployment is plain HTTP on loopback.
	SecureCookies bool
	// PreviewsDefault is the initial state of the previews switch (true =
	// previews start on). It defaults to false (privacy-first: zero outbound
	// media fetches) unless the config explicitly enables them. Editable at
	// runtime via /api/config, which then wins.
	PreviewsDefault bool
	// TrustProxyForwarded makes the login backoff key on the X-Real-IP /
	// X-Forwarded-For client address instead of the (shared) proxy RemoteAddr.
	// Enable ONLY behind a trusted reverse proxy that sets those headers — a
	// direct-facing deployment must not honor client-settable headers.
	TrustProxyForwarded bool
}

// Server is the http.Handler for everything: /api/* plus the embedded
// frontend. Login sessions live in memory — a server restart logs
// browsers out, which is acceptable for a personal bouncer; persisting
// them would be a store migration later.
type Server struct {
	cfg Config
	hub *hub.Hub
	mux *http.ServeMux

	// Media proxy: fetchers match the source network's egress (see
	// egressForNetwork) — direct/proxy fetchers cached by proxy URL, WireGuard
	// tunnel fetchers cached by network — built lazily, so a handful of
	// networks share a small pool. The result caches
	// and the request-wide semaphore (bounding the memory-heavy fetch +
	// decode + encode span) are process-wide. mediaMu guards the fetcher
	// maps and the runtime-editable previews switch.
	mediaMu      sync.RWMutex
	previewsOn   bool
	htmlByProxy  map[string]*fetcher
	imageByProxy map[string]*fetcher
	// Tunnel fetchers for WireGuard networks, keyed by network name (their
	// dial func resolves the network's LIVE tunnel per dial, so they survive
	// reconnects). Separate from the proxy pools since they key by network,
	// not proxy URL.
	tunnelHTMLByNet  map[string]*fetcher
	tunnelImageByNet map[string]*fetcher
	previewCache     *ttlCache[PreviewData]
	thumbCache       *ttlCache[thumbResult]
	mediaSem         chan struct{} // image-decode concurrency (thumbnails)
	previewSem       chan struct{} // link-preview (HTML) concurrency, decoupled from decodes

	login *loginLimiter

	mu     sync.Mutex
	tokens map[string]time.Time // session token -> expiry
	// wsCancels tracks the cancel func of every live WebSocket by session
	// token, so revoking a token (logout, password rotation) tears its
	// sockets down IMMEDIATELY instead of waiting out the 30 s revalidation
	// ticker — a stolen already-open socket must not keep receiving IRC
	// traffic for that window. Guarded by mu (paired with tokens). The
	// ticker in wsWritePump stays as the expiry backstop.
	wsCancels map[string]map[uint64]context.CancelFunc
	wsConns   map[uint64]*websocket.Conn
	wsNextID  uint64

	// sessionTTL is the effective session-cookie lifetime in nanoseconds,
	// runtime-settable (Settings → Session). Atomic so the login path reads it
	// without the token lock.
	sessionTTL atomic.Int64

	// passwordHash is the effective bcrypt login hash: the settings-table
	// override (set via change-password) when present, else the config hash.
	// Atomic so login reads it lock-free. The config file may be a read-only
	// systemd credential, so a UI change lives in the DB, not the file.
	passwordHash atomic.Pointer[string]
	// passwordMu serializes change-password so two rotations can't both verify
	// the old password and clobber each other (leaving DB and runtime hashes
	// disagreeing). credGen bumps on every rotation; a login rechecks it before
	// issuing a token so a login that verified the just-superseded password
	// doesn't slip a session through the rotation's revoke.
	passwordMu sync.Mutex
	credGen    atomic.Uint64

	// settingsMu serializes the runtime settings writes in handleSetConfig
	// (retention, session TTL). Retention is a read-modify-write — read the
	// current pair, overlay the changed dimension, store both — so two
	// concurrent PUTs would otherwise lose one dimension (last write wins), and
	// SetRetention's persist-then-install could interleave into disagreeing DB
	// and live state. One writer at a time closes both.
	settingsMu sync.Mutex

	// wsDrainMu makes closing WebSocket admission atomic with adding a handler
	// to wsWG. Without it, DrainSessions could observe a zero count and return
	// while a request concurrently called Add, letting that handler outlive the
	// store. Once wsDraining is set, WebSocket admission never reopens.
	wsDrainMu  sync.Mutex
	wsDraining bool
	// wsWG tracks live WebSocket handler goroutines so shutdown can drain them
	// before the store is closed (a hijacked WS is not tracked by http.Server
	// and its store reads/writes would otherwise race Store.Close).
	wsWG sync.WaitGroup
}

// admitWS registers a WebSocket handler unless shutdown has closed admission.
// The Add and draining transition share wsDrainMu so Add can never race the
// first Wait in DrainSessions.
func (s *Server) admitWS() bool {
	s.wsDrainMu.Lock()
	defer s.wsDrainMu.Unlock()
	if s.wsDraining {
		return false
	}
	s.wsWG.Add(1)
	return true
}

// WaitSessions blocks until every live WebSocket handler has returned, or the
// timeout elapses. Call it during shutdown AFTER the base context is canceled
// (which unblocks the read loops) and BEFORE closing the store, so no session
// goroutine touches the store after it is closed.
func (s *Server) WaitSessions(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { s.wsWG.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// DrainSessions permits graceful cancellation first, then force-closes every
// still-tracked hijacked connection and waits definitively. Store.Close may
// run only after this returns.
func (s *Server) DrainSessions(grace time.Duration) {
	// Close admission before the first Wait. Any handler that won the lock is
	// already represented in wsWG; every later request receives a 503.
	s.wsDrainMu.Lock()
	s.wsDraining = true
	s.wsDrainMu.Unlock()

	if s.WaitSessions(grace) {
		return
	}
	s.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(s.wsConns))
	for _, c := range s.wsConns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.CloseNow()
	}
	s.wsWG.Wait()
}

func New(cfg Config, h *hub.Hub, assets fs.FS) (*Server, error) {
	if cfg.Username == "" || cfg.PasswordHash == "" {
		return nil, errors.New("api: Username and PasswordHash are required")
	}
	if _, err := bcrypt.Cost([]byte(cfg.PasswordHash)); err != nil {
		return nil, fmt.Errorf("api: PasswordHash is not a bcrypt hash: %w", err)
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	previewsOn, err := loadPreviews(context.Background(), h.Store(), cfg)
	if err != nil {
		return nil, err
	}
	sessionTTL, err := loadSessionTTL(context.Background(), h.Store(), cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:              cfg,
		hub:              h,
		mux:              http.NewServeMux(),
		previewsOn:       previewsOn,
		htmlByProxy:      make(map[string]*fetcher),
		imageByProxy:     make(map[string]*fetcher),
		tunnelHTMLByNet:  make(map[string]*fetcher),
		tunnelImageByNet: make(map[string]*fetcher),
		previewCache:     newTTLCache[PreviewData](30*time.Minute, 512),
		// 30 min, matching the preview cache: a thumbnail is the longest-lived
		// server-side copy of a message's media, and redaction doesn't purge it
		// (keys are (net,url), shared across messages), so a shorter TTL bounds
		// how long a redacted image's thumbnail can linger. Thumbnails are cheap
		// to refetch; the previous 24 h only saved re-decode work.
		thumbCache: newTTLCache[thumbResult](30*time.Minute, maxThumbCache),
		mediaSem:   make(chan struct{}, mediaSlots),
		previewSem: make(chan struct{}, previewSlots),
		login:      newLoginLimiter(),
		tokens:     make(map[string]time.Time),
		wsCancels:  make(map[string]map[uint64]context.CancelFunc),
		wsConns:    make(map[uint64]*websocket.Conn),
	}
	s.sessionTTL.Store(int64(sessionTTL))
	initHash, err := loadPasswordHash(context.Background(), h.Store(), cfg)
	if err != nil {
		return nil, err
	}
	s.passwordHash.Store(&initHash)
	// State-changing and media endpoints require a same-origin request (the
	// WebSocket does its own Origin check in handleWS). GET /api/config is
	// read-only and needs no CSRF guard.
	s.mux.HandleFunc("POST /api/login", s.sameSiteOnly(s.handleLogin))
	s.mux.HandleFunc("POST /api/logout", s.sameSiteOnly(s.handleLogout))
	s.mux.HandleFunc("GET /api/ws", s.handleWS)
	s.mux.HandleFunc("GET /api/config", s.requireAuth(s.handleClientConfig))
	s.mux.HandleFunc("PUT /api/config", s.sameSiteOnly(s.requireAuth(s.handleSetConfig)))
	s.mux.HandleFunc("POST /api/password", s.sameSiteOnly(s.requireAuth(s.handleChangePassword)))
	// The media endpoints are always registered; they refuse (403) at
	// runtime when previews are disabled, so the switch is editable live.
	// POST, not GET: the target URL travels in the request body so it never
	// reaches a reverse-proxy access log's query string (may carry userinfo /
	// signed params). sameSiteOnly still guards them.
	s.mux.HandleFunc("POST /api/preview", s.sameSiteOnly(s.requireAuth(s.handlePreview)))
	s.mux.HandleFunc("POST /api/thumb", s.sameSiteOnly(s.requireAuth(s.handleThumb)))
	// AGPL §13 requires OFFERING THE SOURCE to every network user. These three
	// are deliberately UNauthenticated so anyone using a deployment can reach
	// them: /license and /third-party-licenses serve the embedded license TEXTS;
	// /source is the actual source offer — it redirects to the Corresponding
	// Source, pinned to the exact built commit when the build stamped one.
	s.mux.HandleFunc("GET /license", serveText(ircthing.License))
	s.mux.HandleFunc("GET /third-party-licenses", serveText(ircthing.ThirdPartyLicenses))
	s.mux.HandleFunc("GET /source", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sourceURL(), http.StatusFound)
	})
	if assets != nil {
		s.mux.Handle("/", http.FileServerFS(assets))
	}
	return s, nil
}

// sourceBaseURL is where the Corresponding Source is published. A downstream
// fork that changes this (and rebuilds) makes /source point at ITS source, as
// AGPL §13 requires; the vcs.revision pin means users get the exact revision
// running, not a moving branch tip.
const sourceBaseURL = "https://github.com/AlteredParadox/ircthing"

// sourceURL returns the Corresponding Source location, pinned to the built
// commit ONLY for a clean, VCS-stamped build. A dirty build (vcs.modified=true)
// runs code that no commit reflects, and a build with no stamp (-buildvcs=false)
// has no commit to point at — in both cases pinning would MISLEAD, so fall back
// to the repository root. Release builds should be clean and VCS-stamped so
// users get the exact revision; the fallback keeps §13 honest otherwise.
func sourceURL() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return sourceBaseURL
	}
	var revision string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				return sourceBaseURL // dirty tree: no commit reflects what's running
			}
		}
	}
	if revision != "" {
		return sourceBaseURL + "/tree/" + revision
	}
	return sourceBaseURL
}

// serveText serves a static embedded text (license notices) as plain UTF-8.
func serveText(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(body)
	}
}

// handleClientConfig returns the server-set switches the frontend needs at
// startup (currently just whether link/media previews are enabled), so the
// UI doesn't request previews the server has turned off.
// previewsEnabled reports the current (runtime-editable) previews switch.
func (s *Server) previewsEnabled() bool {
	s.mediaMu.RLock()
	defer s.mediaMu.RUnlock()
	return s.previewsOn
}

// htmlFetcherFor / imageFetcherFor return a fetcher bound to proxy (nil =
// direct), building and caching one per distinct proxy. The pool is small
// (one entry per network proxy plus direct).
func (s *Server) htmlFetcherFor(proxy *url.URL) *fetcher {
	return s.cachedFetcher(s.htmlByProxy, maxHTMLBytes, proxy, true)
}

func (s *Server) imageFetcherFor(proxy *url.URL) *fetcher {
	return s.cachedFetcher(s.imageByProxy, maxImageBytes, proxy, false)
}

// htmlFetcherForNetwork / imageFetcherForNetwork resolve how a media fetch for
// a link seen on `network` must egress and return the matching fetcher: direct,
// through the network's proxy, or through its WireGuard tunnel. They return nil
// to FAIL CLOSED (the caller sends 502) when the egress can't be safely
// determined — never a direct fetch that would leak a proxied/tunneled
// network's real IP.
func (s *Server) htmlFetcherForNetwork(ctx context.Context, network string) *fetcher {
	e := s.egressForNetwork(ctx, network)
	if !e.ok {
		return nil
	}
	if e.tunnel {
		return s.cachedTunnelFetcher(s.tunnelHTMLByNet, maxHTMLBytes, e.network, true)
	}
	return s.htmlFetcherFor(e.proxy) // nil proxy => direct
}

func (s *Server) imageFetcherForNetwork(ctx context.Context, network string) *fetcher {
	e := s.egressForNetwork(ctx, network)
	if !e.ok {
		return nil
	}
	if e.tunnel {
		return s.cachedTunnelFetcher(s.tunnelImageByNet, maxImageBytes, e.network, false)
	}
	return s.imageFetcherFor(e.proxy)
}

// cachedTunnelFetcher returns a fetcher (per network, per size) that dials
// through the network's live WireGuard tunnel. The dial func resolves the
// tunnel fresh on every dial via the hub, so the cached fetcher transparently
// follows the network across reconnects and fails closed when it is down.
func (s *Server) cachedTunnelFetcher(pool map[string]*fetcher, maxBytes int64, network string, truncate bool) *fetcher {
	s.mediaMu.RLock()
	f := pool[network]
	s.mediaMu.RUnlock()
	if f != nil {
		return f
	}
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	if f = pool[network]; f == nil {
		if len(pool) >= maxProxyFetchers {
			clear(pool)
		}
		f = newTunnelFetcher(maxBytes, func(ctx context.Context, addr string) (net.Conn, error) {
			return s.hub.NetworkTunnelDial(ctx, network, addr)
		})
		f.truncate = truncate // set before publish (under lock) — race-free
		pool[network] = f
	}
	return f
}

// maxProxyFetchers bounds a fetcher pool. Real deployments use a handful of
// distinct proxies (one per network, plus direct); this only bites after
// many proxy rotations over a long-lived process, at which point the pool
// is purged so obsolete fetchers — and the credential-bearing URLs keying
// them — don't accumulate. Fetchers are stateless http.Clients, so a purge
// costs only a rebuild-on-demand (in-flight requests hold their own
// fetcher pointer and are unaffected).
const maxProxyFetchers = 32

func (s *Server) cachedFetcher(pool map[string]*fetcher, maxBytes int64, proxy *url.URL, truncate bool) *fetcher {
	key := proxyString(proxy)
	s.mediaMu.RLock()
	f := pool[key]
	s.mediaMu.RUnlock()
	if f != nil {
		return f
	}
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	if f = pool[key]; f == nil {
		if len(pool) >= maxProxyFetchers {
			clear(pool)
		}
		f = newFetcher(maxBytes, proxy)
		f.truncate = truncate // set before publish (under lock) — race-free
		pool[key] = f
	}
	return f
}

// requireAuth wraps a handler so only authenticated sessions reach it —
// the media proxy must not become an open relay for the whole internet.
func (s *Server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// cookieName is the session cookie's name. Under SecureCookies (the
// TLS-fronted deployment) it carries the __Host- prefix, which the browser
// enforces to require Secure + Path=/ + NO Domain — so a sibling subdomain
// cannot inject a parent-domain cookie of the same name to shadow the
// victim's session (a login/session denial of service). The plain-loopback
// deferral (Secure off) can't use the prefix, so it keeps the bare name.
func (s *Server) cookieName() string {
	if s.cfg.SecureCookies {
		return "__Host-" + sessionCookie
	}
	return sessionCookie
}

// sameSiteOnly refuses a cross-origin request to a state-changing / media
// endpoint (all guarded routes are POST/PUT). SameSite=Strict cookies stop
// true cross-site requests but still treat SIBLING subdomains as same-site, so
// a hostile sibling could form-POST /api/logout or trigger authenticated media
// fetches. The Sec-Fetch-Site fetch-metadata header distinguishes these:
// same-origin/none are trusted, same-site/cross-site refused.
//
// When Sec-Fetch-Site is ABSENT (an older browser or a header-stripping proxy)
// we FAIL CLOSED via Origin: browsers send Origin on every POST/PUT, even
// same-origin, so a legitimate request always carries one — a missing or
// cross-origin Origin is refused rather than waved through (which a sibling
// could otherwise exploit). A non-browser API client can supply Origin or
// Sec-Fetch-Site: same-origin. Deployment note: a fronting reverse proxy MUST
// preserve Origin (or Sec-Fetch-Site), or these endpoints refuse everything.
func (s *Server) sameSiteOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "same-origin", "none":
			// Trusted: a same-origin fetch or a direct navigation.
		case "cross-site", "same-site":
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		default: // absent/unknown: require a same-origin Origin (fail closed)
			if origin := r.Header.Get("Origin"); origin == "" || !s.sameOrigin(origin, r) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		h(w, r)
	}
}

// sameOrigin reports whether an Origin names the same origin as the request. It
// backs BOTH the WebSocket handshake gate and sameSiteOnly's absent-Sec-Fetch-
// Site fallback. Host must always match; the scheme is compared only when it is
// reliably known (see the body) — an indeterminate scheme falls back to host-
// only so a reverse-proxy deployment isn't locked out. The residual is that on
// such a deployment a same-HOST cross-scheme Origin is accepted; the host match
// plus the authenticated SameSite=Strict cookie remain the guard.
func (s *Server) sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host != r.Host {
		return false
	}
	// Verify the scheme ONLY when ours is reliably known: a direct TLS listener
	// (r.TLS), or X-Forwarded-Proto from a trusted proxy. Behind a TLS-terminating
	// proxy that doesn't set that header — or when behind_proxy is off — we cannot
	// know our external scheme (r.TLS is nil even though the browser reached us
	// over https), so we do NOT reject on it: host match plus the authenticated
	// SameSite=Strict session cookie remain the guard. Otherwise a normal Caddy /
	// nginx WSS deployment would be locked out of the WebSocket entirely. When the
	// scheme IS known, an http Origin on an https deployment is still refused.
	scheme := ""
	if r.TLS != nil {
		scheme = "https"
	} else if s.cfg.TrustProxyForwarded {
		scheme = r.Header.Get("X-Forwarded-Proto")
	}
	return scheme == "" || u.Scheme == scheme
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	// Browser hardening on every response. The CSP locks everything to
	// same-origin; connect-src 'self' covers the same-origin WebSocket
	// in every current browser, and nothing request-controlled (Host)
	// is interpolated into the header. style-src allows inline styles:
	// the user-CSS override feature injects a <style> element, and
	// style attributes are harmless next to a locked-down script-src
	// ('self' via default-src).
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	// Message links are attacker-controlled remote anchors; hyperlink DNS
	// prefetch would leak a DNS query for every rendered hostname without a
	// click (a viewing tracker). Chromium already disables prefetch for
	// pages served over HTTPS, so this protects the residual cases: plain-
	// HTTP access, altered browser settings, and other engines.
	// chromium.org/developers/design-documents/dns-prefetching/
	h.Set("X-DNS-Prefetch-Control", "off")
	// img-src includes blob: for thumbnails: they are fetched from the media
	// proxy as blobs and rendered from object URLs (preview.jsx), which
	// Firefox blocks under 'self' unless blob: is listed explicitly.
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "malformed login request", http.StatusBadRequest)
		return
	}
	// Login is the one unauthenticated endpoint that burns CPU (bcrypt):
	// per-source failure backoff plus a bounded hashing semaphore keep it
	// from being a cheap exhaustion vector.
	source := s.loginSourceKey(r)
	if wait := s.login.retryAfter(source, time.Now()); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+1)))
		http.Error(w, "too many attempts, retry later", http.StatusTooManyRequests)
		return
	}
	// The global bucket is charged after the per-source check so a source
	// already in backoff cannot drain tokens from everyone else.
	if wait := s.login.globalAllow(time.Now()); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+1)))
		http.Error(w, "too many attempts, retry later", http.StatusTooManyRequests)
		return
	}
	// Snapshot the credential generation BEFORE the (slow) bcrypt verify; if a
	// password change lands during it, the verified hash is stale — refuse to
	// mint a session that would survive the rotation's session revoke.
	gen := s.credGen.Load()
	ok, busy := s.authenticate(r.Context(), source, req.Username, req.Password)
	if busy {
		http.Error(w, "busy, retry later", http.StatusTooManyRequests)
		return
	}
	if !ok || s.credGen.Load() != gen {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := s.issueToken(gen)
	if err != nil {
		// A rotation that landed between the recheck and issuance is not an
		// internal fault — treat it as a failed (stale) credential.
		if errors.Is(err, errCredRotated) {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.sessionTTLDur().Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure follows config: on when TLS terminates in front of the
		// binary (the recommended deployment), off for plain loopback.
		Secure: s.cfg.SecureCookies,
	})
	w.WriteHeader(http.StatusNoContent)
}

// authenticate runs the bounded, constant-time credential check for one
// login attempt and records success/failure for the source's backoff.
// busy is true when no hashing slot was available.
func (s *Server) authenticate(ctx context.Context, source, username, password string) (ok, busy bool) {
	if !s.login.acquire(ctx) {
		return false, true
	}
	// Evaluate both checks unconditionally so a wrong username costs the
	// same time as a wrong password.
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(s.cfg.Username)) == 1
	passErr := bcrypt.CompareHashAndPassword([]byte(s.effectivePasswordHash()), []byte(password))
	s.login.release()
	if !userOK || passErr != nil {
		s.login.fail(source, time.Now())
		return false, false
	}
	s.login.ok(source)
	return true, false
}

// issueToken mints a session token, pruning expired sessions and
// evicting the oldest once the live set is at capacity, so repeated
// logins cannot grow the map without bound.
// sessionTTLDur is the effective (runtime-settable) session-cookie lifetime.
func (s *Server) sessionTTLDur() time.Duration { return time.Duration(s.sessionTTL.Load()) }

// effectivePasswordHash is the current bcrypt login hash (override or config).
func (s *Server) effectivePasswordHash() string { return *s.passwordHash.Load() }

// errCredRotated reports that the login credentials changed between a login's
// pre-verify snapshot and token issuance, so the just-verified session must not
// be minted.
var errCredRotated = errors.New("api: credentials rotated during login")

// randomToken mints a fresh 256-bit session token.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (s *Server) issueToken(gen uint64) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	var cancels []context.CancelFunc
	s.mu.Lock()
	// Close the rotation race: handleChangePassword bumps credGen and revokes
	// every session under this same s.mu. If it committed after the caller's
	// snapshot, refuse — otherwise a login that verified the OLD hash could
	// insert a token the revoke loop has already passed.
	if s.credGen.Load() != gen {
		s.mu.Unlock()
		return "", errCredRotated
	}
	for t, exp := range s.tokens {
		if now.After(exp) {
			cancels = append(cancels, s.deleteTokenLocked(t)...)
		}
	}
	for len(s.tokens) >= maxSessions {
		oldest, oldestExp := "", time.Time{}
		for t, exp := range s.tokens {
			if oldest == "" || exp.Before(oldestExp) {
				oldest, oldestExp = t, exp
			}
		}
		cancels = append(cancels, s.deleteTokenLocked(oldest)...)
	}
	s.tokens[token] = now.Add(s.sessionTTLDur())
	s.mu.Unlock()
	// Expired/evicted sessions may still have open sockets; drop them now
	// rather than leaving them to the revalidation ticker.
	for _, cancel := range cancels {
		cancel()
	}
	return token, nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.cookieName()); err == nil {
		// deleteTokenLocked tears down this token's live WebSockets NOW — the
		// write pump's 30 s revalidation ticker would otherwise let an
		// already-open (possibly stolen) socket keep receiving and sending
		// IRC traffic after logout.
		s.mu.Lock()
		cancels := s.deleteTokenLocked(c.Value)
		s.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
	}
	// The deletion cookie carries the same attributes as the session
	// cookie so every browser treats it as the same cookie.
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.cfg.SecureCookies,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(s.cookieName())
	if err != nil {
		return false
	}
	return s.tokenValid(c.Value)
}

// tokenValid reports whether a session token is still live — used on every
// authenticated request and to revoke an already-open WebSocket after
// logout or expiry. An expired token is deleted AND its live sockets are
// canceled (deleteTokenLocked), so expiry mid-session doesn't leave a
// socket running until the ticker.
func (s *Server) tokenValid(token string) bool {
	var cancels []context.CancelFunc
	s.mu.Lock()
	expiry, ok := s.tokens[token]
	if ok && time.Now().After(expiry) {
		cancels = s.deleteTokenLocked(token)
		ok = false
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return ok
}

// registerWSCancel records a live socket's cancel func under its session
// token and returns the matching unregister. It VALIDATES the token under the
// same lock: authentication happened before the WebSocket upgrade, and a
// logout/rotation landing in that window has already deleted the token — its
// cancel sweep ran before this registration, so registering blind would leave
// the socket live until the ticker. ok=false means the token is gone/expired
// and the handler must refuse the socket.
func (s *Server) registerWSCancel(token string, cancel context.CancelFunc, conn *websocket.Conn) (unregister func(), ok bool) {
	if token == "" {
		return func() {}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, live := s.tokens[token]
	if !live || time.Now().After(expiry) {
		return func() {}, false
	}
	id := s.wsNextID
	s.wsNextID++
	m := s.wsCancels[token]
	if m == nil {
		m = make(map[uint64]context.CancelFunc)
		s.wsCancels[token] = m
	}
	m[id] = cancel
	s.wsConns[id] = conn
	return func() {
		s.mu.Lock()
		delete(s.wsConns, id)
		if m := s.wsCancels[token]; m != nil {
			delete(m, id)
			if len(m) == 0 {
				delete(s.wsCancels, token)
			}
		}
		s.mu.Unlock()
	}, true
}

// deleteTokenLocked removes a session token and collects its live sockets'
// cancel funcs. EVERY token deletion must go through here (logout, rotation,
// expiry, capacity eviction) — a deletion that skips the sweep leaves the
// token's sockets running until the revalidation ticker. Caller holds s.mu
// and must invoke the returned funcs AFTER releasing it.
func (s *Server) deleteTokenLocked(token string) []context.CancelFunc {
	delete(s.tokens, token)
	return s.cancelSocketsLocked(token)
}

// cancelSocketsLocked collects the cancel funcs of every live socket on
// `token` and removes them from the registry. Caller holds s.mu; the returned
// funcs must be called AFTER the lock is released (a cancel wakes the socket
// goroutines, which may immediately try to unregister and take s.mu).
func (s *Server) cancelSocketsLocked(token string) []context.CancelFunc {
	m := s.wsCancels[token]
	if m == nil {
		return nil
	}
	delete(s.wsCancels, token)
	out := make([]context.CancelFunc, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	return out
}

// wsWritePump writes the session's outbound envelopes to the socket,
// periodically re-validating the session token (revoking the socket on
// logout/expiry) and closing on a slow consumer. Returns when the
// context is canceled or a write fails.
func (s *Server) wsWritePump(ctx context.Context, c *websocket.Conn, sess *hub.Session, token string) {
	revoke := time.NewTicker(time.Duration(sessionRecheckInterval.Load()))
	defer revoke.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-revoke.C:
			if !s.tokenValid(token) {
				c.Close(websocket.StatusPolicyViolation, "session ended")
				return
			}
		case <-sess.Done():
			c.Close(websocket.StatusPolicyViolation, "too slow, reconnect and refetch")
			return
		case queued := <-sess.Outbound():
			// Frames are encoded before queue admission, so the reservation is
			// exact and the write pump never holds a second full-frame copy.
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Write(wctx, websocket.MessageText, queued.Data)
			wcancel()
			queued.Release()
			if err != nil {
				return
			}
		}
	}
}

// handleWS upgrades an authenticated request and bridges the connection
// to a hub.Session: one goroutine writes Outbound envelopes to the
// socket, the request goroutine reads client envelopes into Handle.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.admitWS() {
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.wsWG.Done()

	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Same-origin check (host always, scheme when determinable — see sameOrigin).
	// The coder/websocket default (Accept with nil opts) allows an ABSENT Origin,
	// which a browser never sends on a WS handshake, so we require it and run our
	// own check, disabling the library's weaker one below.
	if origin := r.Header.Get("Origin"); origin == "" || !s.sameOrigin(origin, r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	// Register the hub session BEFORE the upgrade: once Accept returns
	// on the wire, the client considers itself subscribed, so events
	// arriving during the handshake must already land in this session's
	// outbound queue rather than being broadcast past it.
	sess := s.hub.NewSession()
	if sess == nil {
		http.Error(w, "too many active sessions", http.StatusServiceUnavailable)
		return
	}
	defer sess.Close()

	// InsecureSkipVerify disables the library's weaker host-only Origin check;
	// we already enforced a stricter scheme+host same-origin check above.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return // Accept has already written the HTTP error
	}
	defer c.CloseNow()
	// The default read limit (32 KiB) is below the prefs cap, so a valid
	// set_prefs (custom CSS up to MaxPrefsBytes) plus its JSON envelope
	// would be rejected as oversized before reaching the handler. Admit
	// the largest legitimate message with envelope headroom.
	c.SetReadLimit(hub.MaxPrefsBytes + wsEnvelopeHeadroom)

	// The session's token, so the live socket can be revoked when the
	// user logs out or the token expires (auth is otherwise only checked
	// once, at upgrade).
	var token string
	if ck, err := r.Cookie(s.cookieName()); err == nil {
		token = ck.Value
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Register so logout/password-rotation revokes this socket immediately;
	// the write pump's ticker remains the expiry backstop. Registration
	// re-validates the token atomically — a logout that landed between the
	// auth check and this point already ran its cancel sweep, so a blind
	// registration would leave this socket alive until the ticker.
	unregister, live := s.registerWSCancel(token, cancel, c)
	if !live {
		c.Close(websocket.StatusPolicyViolation, "session ended")
		return
	}
	defer unregister()

	go func() {
		defer cancel()
		s.wsWritePump(ctx, c, sess, token)
	}()

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var env hub.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue // undecodable frames are ignored, never fatal
		}
		sess.Handle(ctx, env)
	}
}
