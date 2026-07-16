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

type Config struct {
	Username     string
	PasswordHash string        // bcrypt hash of the user's password
	SessionTTL   time.Duration // default 30 days
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
	// caches, and a semaphore bounding concurrent image decodes.
	htmlFetcher  *fetcher
	imageFetcher *fetcher
	previewCache *ttlCache[PreviewData]
	thumbCache   *ttlCache[thumbResult]
	thumbSem     chan struct{}

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
		thumbSem:     make(chan struct{}, 4),
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
	s.tokens[token] = time.Now().Add(s.cfg.SessionTTL)
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure is not forced: TLS termination is expected at a reverse
		// proxy for now. Revisit when built-in TLS serving lands.
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.tokens, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
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
