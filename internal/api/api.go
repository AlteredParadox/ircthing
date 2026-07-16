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
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/bcrypt"

	"ircthing/internal/hub"
)

const sessionCookie = "ircthing_session"

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
}

// Server is the http.Handler for everything: /api/* plus the embedded
// frontend. Login sessions live in memory — a server restart logs
// browsers out, which is acceptable for a personal bouncer; persisting
// them would be a store migration later.
type Server struct {
	cfg Config
	hub *hub.Hub
	mux *http.ServeMux

	// Media proxy: separate fetchers (different size caps), result
	// caches, and a request-wide semaphore bounding the memory-heavy
	// span (fetch + decode + encode) of concurrent media requests.
	htmlFetcher  *fetcher
	imageFetcher *fetcher
	previewCache *ttlCache[PreviewData]
	thumbCache   *ttlCache[thumbResult]
	mediaSem     chan struct{}

	login *loginLimiter

	mu     sync.Mutex
	tokens map[string]time.Time // session token -> expiry
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
		htmlFetcher:  newFetcher(maxHTMLBytes),
		imageFetcher: newFetcher(maxImageBytes),
		previewCache: newTTLCache[PreviewData](30*time.Minute, 512),
		thumbCache:   newTTLCache[thumbResult](24*time.Hour, maxThumbCache),
		mediaSem:     make(chan struct{}, mediaSlots),
		login:        newLoginLimiter(),
		tokens:       make(map[string]time.Time),
	}
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/ws", s.handleWS)
	s.mux.HandleFunc("GET /api/preview", s.requireAuth(s.handlePreview))
	s.mux.HandleFunc("GET /api/thumb", s.requireAuth(s.handleThumb))
	if assets != nil {
		s.mux.Handle("/", http.FileServerFS(assets))
	}
	return s, nil
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
	if !s.login.acquire(r.Context()) {
		http.Error(w, "busy, retry later", http.StatusTooManyRequests)
		return
	}
	// Evaluate both checks unconditionally so a wrong username costs the
	// same time as a wrong password.
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(s.cfg.Username)) == 1
	passErr := bcrypt.CompareHashAndPassword([]byte(s.cfg.PasswordHash), []byte(req.Password))
	s.login.release()
	if !userOK || passErr != nil {
		s.login.fail(source, time.Now())
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	s.login.ok(source)

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	// Housekeeping at issue time: drop expired sessions (otherwise they
	// only leave when their exact token is presented again), and cap the
	// live set by evicting the oldest — repeated logins must not grow
	// the map forever.
	now := time.Now()
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
	s.tokens[token] = now.Add(s.cfg.SessionTTL)
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure follows config: on when TLS terminates in front of the
		// binary (the recommended deployment), off for plain loopback.
		Secure: s.cfg.SecureCookies,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.tokens, c.Value)
		s.mu.Unlock()
	}
	// The deletion cookie carries the same attributes as the session
	// cookie so every browser treats it as the same cookie.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.cfg.SecureCookies,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
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

// handleWS upgrades an authenticated request and bridges the connection
// to a hub.Session: one goroutine writes Outbound envelopes to the
// socket, the request goroutine reads client envelopes into Handle.
// websocket.Accept enforces same-origin for browser requests.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has already written the HTTP error
	}
	defer c.CloseNow()

	sess := s.hub.NewSession()
	defer sess.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
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
