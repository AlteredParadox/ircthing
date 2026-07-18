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
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/bcrypt"

	"ircthing/internal/hub"
)

const sessionCookie = "ircthing_session"

// wsEnvelopeHeadroom is slack above the largest payload (a prefs blob)
// for the JSON envelope wrapping it (v/type/seq/data keys and quoting).
const wsEnvelopeHeadroom = 16 * 1024

// sessionRecheckInterval is how often a live WebSocket re-validates its
// session token, so logout/expiry revokes an already-open connection.
// A var so tests can shorten it.
var sessionRecheckInterval = 30 * time.Second

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
}

// Server is the http.Handler for everything: /api/* plus the embedded
// frontend. Login sessions live in memory — a server restart logs
// browsers out, which is acceptable for a personal bouncer; persisting
// them would be a store migration later.
type Server struct {
	cfg Config
	hub *hub.Hub
	mux *http.ServeMux

	// Media proxy: fetchers are per-proxy (previews use the source
	// network's proxy — proxyForNetwork), built lazily and cached by proxy
	// URL, so a handful of networks share a small pool. The result caches
	// and the request-wide semaphore (bounding the memory-heavy fetch +
	// decode + encode span) are process-wide. mediaMu guards the fetcher
	// maps and the runtime-editable previews switch.
	mediaMu       sync.RWMutex
	previewsOn    bool
	htmlByProxy   map[string]*fetcher
	imageByProxy  map[string]*fetcher
	previewCache  *ttlCache[PreviewData]
	thumbCache    *ttlCache[thumbResult]
	mediaSem      chan struct{}

	login *loginLimiter

	mu     sync.Mutex
	tokens map[string]time.Time // session token -> expiry

	// sessionTTL is the effective session-cookie lifetime in nanoseconds,
	// runtime-settable (Settings → Session). Atomic so the login path reads it
	// without the token lock.
	sessionTTL atomic.Int64

	// passwordHash is the effective bcrypt login hash: the settings-table
	// override (set via change-password) when present, else the config hash.
	// Atomic so login reads it lock-free. The config file may be a read-only
	// systemd credential, so a UI change lives in the DB, not the file.
	passwordHash atomic.Pointer[string]
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
	s := &Server{
		cfg:          cfg,
		hub:          h,
		mux:          http.NewServeMux(),
		previewsOn:   loadPreviews(context.Background(), h.Store(), cfg),
		htmlByProxy:  make(map[string]*fetcher),
		imageByProxy: make(map[string]*fetcher),
		previewCache: newTTLCache[PreviewData](30*time.Minute, 512),
		thumbCache:   newTTLCache[thumbResult](24*time.Hour, maxThumbCache),
		mediaSem:     make(chan struct{}, mediaSlots),
		login:        newLoginLimiter(),
		tokens:       make(map[string]time.Time),
	}
	s.sessionTTL.Store(int64(loadSessionTTL(context.Background(), h.Store(), cfg)))
	initHash := loadPasswordHash(context.Background(), h.Store(), cfg)
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
	s.mux.HandleFunc("GET /api/preview", s.sameSiteOnly(s.requireAuth(s.handlePreview)))
	s.mux.HandleFunc("GET /api/thumb", s.sameSiteOnly(s.requireAuth(s.handleThumb)))
	if assets != nil {
		s.mux.Handle("/", http.FileServerFS(assets))
	}
	return s, nil
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
	return s.cachedFetcher(s.htmlByProxy, maxHTMLBytes, proxy)
}

func (s *Server) imageFetcherFor(proxy *url.URL) *fetcher {
	return s.cachedFetcher(s.imageByProxy, maxImageBytes, proxy)
}

// maxProxyFetchers bounds a fetcher pool. Real deployments use a handful of
// distinct proxies (one per network, plus direct); this only bites after
// many proxy rotations over a long-lived process, at which point the pool
// is purged so obsolete fetchers — and the credential-bearing URLs keying
// them — don't accumulate. Fetchers are stateless http.Clients, so a purge
// costs only a rebuild-on-demand (in-flight requests hold their own
// fetcher pointer and are unaffected).
const maxProxyFetchers = 32

func (s *Server) cachedFetcher(pool map[string]*fetcher, maxBytes int64, proxy *url.URL) *fetcher {
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

// sameSiteOnly refuses a definitively cross-origin request via the
// Sec-Fetch-Site fetch-metadata header. SameSite=Strict cookies stop true
// cross-site requests but still treat SIBLING subdomains as same-site, so a
// hostile sibling could form-POST /api/logout or embed authenticated media
// GETs; requiring same-origin (rejecting "same-site"/"cross-site") closes
// that. When the header is ABSENT (older browsers, a header-stripping proxy)
// we fall back to an Origin check so a sibling isn't a free pass — a present
// Origin must match this request's host; an absent Origin (typical for a
// top-level GET navigation) is allowed, since the app only ever issues
// same-origin requests.
func (s *Server) sameSiteOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "cross-site", "same-site":
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		case "same-origin", "none":
			// Trusted: a same-origin fetch or a direct navigation.
		default: // absent/unknown: fall back to Origin
			if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		h(w, r)
	}
}

// sameOrigin reports whether an Origin header's host matches the request's
// own Host (scheme/port included in Host comparison via the URL authority).
func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host == host
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
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
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
	source := loginSourceKey(r)
	if wait := s.login.retryAfter(source, time.Now()); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+1)))
		http.Error(w, "too many attempts, retry later", http.StatusTooManyRequests)
		return
	}
	ok, busy := s.authenticate(r.Context(), source, req.Username, req.Password)
	if busy {
		http.Error(w, "busy, retry later", http.StatusTooManyRequests)
		return
	}
	if !ok {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := s.issueToken()
	if err != nil {
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

func (s *Server) issueToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for t, exp := range s.tokens {
		if now.After(exp) {
			delete(s.tokens, t)
		}
	}
	for len(s.tokens) >= maxSessions {
		oldest, oldestExp := "", time.Time{}
		for t, exp := range s.tokens {
			if oldest == "" || exp.Before(oldestExp) {
				oldest, oldestExp = t, exp
			}
		}
		delete(s.tokens, oldest)
	}
	s.tokens[token] = now.Add(s.sessionTTLDur())
	return token, nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.cookieName()); err == nil {
		s.mu.Lock()
		delete(s.tokens, c.Value)
		s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.tokens[c.Value]
	if ok && time.Now().After(expiry) {
		delete(s.tokens, c.Value)
		return false
	}
	return ok
}

// tokenValid reports whether a session token is still live — used to
// revoke an already-open WebSocket after logout or expiry.
func (s *Server) tokenValid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.tokens[token]
	if ok && time.Now().After(expiry) {
		delete(s.tokens, token)
		return false
	}
	return ok
}

// wsWritePump writes the session's outbound envelopes to the socket,
// periodically re-validating the session token (revoking the socket on
// logout/expiry) and closing on a slow consumer. Returns when the
// context is canceled or a write fails.
func (s *Server) wsWritePump(ctx context.Context, c *websocket.Conn, sess *hub.Session, token string) {
	revoke := time.NewTicker(sessionRecheckInterval)
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
		case env := <-sess.Outbound():
			data, err := json.Marshal(env)
			if err != nil {
				continue
			}
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err = c.Write(wctx, websocket.MessageText, data)
			wcancel()
			if err != nil {
				return
			}
		}
	}
}

// handleWS upgrades an authenticated request and bridges the connection
// to a hub.Session: one goroutine writes Outbound envelopes to the
// socket, the request goroutine reads client envelopes into Handle.
// websocket.Accept enforces same-origin for browser requests.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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

	c, err := websocket.Accept(w, r, nil)
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
