package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"ircthing/internal/store"
)

// passwordHashKey stores the bcrypt login hash set via change-password. The
// config file seeds the initial hash but may be a read-only systemd
// credential, so a UI change is persisted here and preferred at login.
const passwordHashKey = "password_hash"

const (
	minPasswordLen = 8
	maxPasswordLen = 72 // bcrypt ignores input bytes past 72
)

// loadPasswordHash resolves the effective login hash: a valid stored override
// wins over the config hash.
func loadPasswordHash(ctx context.Context, st *store.Store, cfg Config) string {
	if v, err := st.Setting(ctx, passwordHashKey); err == nil && v != "" {
		if _, err := bcrypt.Cost([]byte(v)); err == nil { // sanity: a real bcrypt hash
			return v
		}
	}
	return cfg.PasswordHash
}

// handleChangePassword verifies the current password and stores a new bcrypt
// hash in the settings table (auth + same-origin required by the router).
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	// The current-password check is a credential comparison, so rate-limit and
	// bound the bcrypt work exactly like login.
	source := s.loginSourceKey(r)
	if wait := s.login.retryAfter(source, time.Now()); wait > 0 {
		http.Error(w, "too many attempts, retry later", http.StatusTooManyRequests)
		return
	}
	// Serialize the whole verify→store→revoke: two concurrent rotations must
	// not both verify the (same) old password and then race their writes.
	s.passwordMu.Lock()
	defer s.passwordMu.Unlock()

	if !s.login.acquire(r.Context()) {
		http.Error(w, "busy, retry later", http.StatusTooManyRequests)
		return
	}
	verifyErr := bcrypt.CompareHashAndPassword([]byte(s.effectivePasswordHash()), []byte(req.Current))
	s.login.release()
	if verifyErr != nil {
		s.login.fail(source, time.Now())
		http.Error(w, "current password is incorrect", http.StatusForbidden)
		return
	}
	s.login.ok(source)

	if len(req.New) < minPasswordLen || len(req.New) > maxPasswordLen {
		http.Error(w, "new password must be 8–72 characters", http.StatusBadRequest)
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "hashing failed", http.StatusInternalServerError)
		return
	}
	if err := s.hub.Store().SetSetting(r.Context(), passwordHashKey, string(newHash)); err != nil {
		http.Error(w, "storing failed", http.StatusInternalServerError)
		return
	}
	nh := string(newHash)
	s.passwordHash.Store(&nh)
	s.credGen.Add(1) // invalidate any login that verified the old hash

	// Revoke every OTHER session — a compromised old password must not keep
	// them alive — while the browser making the change stays signed in.
	if c, err := r.Cookie(s.cookieName()); err == nil {
		s.mu.Lock()
		for tok := range s.tokens {
			if tok != c.Value {
				delete(s.tokens, tok)
			}
		}
		s.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}
