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
		cfg:    cfg,
		hub:    h,
		mux:    http.NewServeMux(),
		tokens: make(map[string]time.Time),
	}
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/ws", s.handleWS)
	if assets != nil {
		s.mux.Handle("/", http.FileServerFS(assets))
	}
	return s, nil
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
	// Evaluate both checks unconditionally so a wrong username costs the
	// same time as a wrong password.
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(s.cfg.Username)) == 1
	passErr := bcrypt.CompareHashAndPassword([]byte(s.cfg.PasswordHash), []byte(req.Password))
	if !userOK || passErr != nil {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

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
