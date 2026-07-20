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
	"encoding/json"
	"fmt"
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
// wins over the config seed. It fails CLOSED — a store read error or a corrupt
// override returns an error rather than silently falling back to the config
// seed. Rotation deliberately leaves the seed untouched, so a silent fallback
// would resurrect the pre-rotation password on the next restart. Only a
// genuinely-absent override (empty value, no error) uses the seed, so first
// boot still works.
func loadPasswordHash(ctx context.Context, st *store.Store, cfg Config) (string, error) {
	v, err := st.Setting(ctx, passwordHashKey)
	if err != nil {
		return "", fmt.Errorf("reading stored password override: %w", err)
	}
	if v == "" {
		return cfg.PasswordHash, nil // no override set yet (first boot)
	}
	if _, err := bcrypt.Cost([]byte(v)); err != nil {
		return "", fmt.Errorf("stored password override is not a valid bcrypt hash (refusing to fall back to the config seed)")
	}
	return v, nil
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
	// bound the bcrypt work exactly like login — including login's GLOBAL
	// bucket, which this endpoint previously skipped.
	source := s.loginSourceKey(r)
	if wait := s.login.retryAfter(source, time.Now()); wait > 0 {
		http.Error(w, "too many attempts, retry later", http.StatusTooManyRequests)
		return
	}
	if wait := s.login.globalAllow(time.Now()); wait > 0 {
		http.Error(w, "too many attempts, retry later", http.StatusTooManyRequests)
		return
	}
	// Serialize the whole verify→store→revoke: two concurrent rotations must
	// not both verify the (same) old password and then race their writes.
	// TryLock, not Lock: rotations are rare and human-initiated, so a burst
	// piling up on an unbounded mutex wait is only ever an attack shape
	// (goroutine pressure from a stolen session delaying compromise
	// recovery) — bounce concurrent attempts instead of queueing them.
	if !s.passwordMu.TryLock() {
		http.Error(w, "busy, retry later", http.StatusTooManyRequests)
		return
	}
	defer s.passwordMu.Unlock()
	// RECHECK backoff after admission: a burst that arrived before the first
	// failure installed backoff has already passed the pre-lock check, and
	// serializing it through the lock would otherwise let every queued
	// request burn a bcrypt verify with no further gate.
	if wait := s.login.retryAfter(source, time.Now()); wait > 0 {
		http.Error(w, "too many attempts, retry later", http.StatusTooManyRequests)
		return
	}
	// Recheck auth too: a logout/rotation that landed while this request
	// waited must invalidate it — the session it rode in on is gone.
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

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
		http.Error(w, "new password must be 8–72 bytes", http.StatusBadRequest)
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

	// Bump the generation and revoke EVERY session ATOMICALLY under the same
	// s.mu that issueToken takes. This closes the login race: a concurrent
	// old-password login either loses this lock (issueToken then observes the
	// new generation and refuses to mint) or wins it (and this revoke deletes
	// the session it just inserted). The requester's own token is revoked too
	// — password rotation is the compromise-recovery action, and if that
	// exact token was stolen, keeping it valid would keep the thief logged in
	// through the rotation. The requester is rotated onto a FRESH token via
	// Set-Cookie below; their open WebSocket drops and reconnects with it.
	// Whether the requester's own token is STILL LIVE is decided atomically
	// with the revoke: a logout racing this rotation deletes the token first,
	// and minting a replacement regardless would re-authenticate a browser
	// that already signed out (whichever response lands last would win).
	requester := ""
	if c, err := r.Cookie(s.cookieName()); err == nil {
		requester = c.Value
	}
	// Generate the replacement token BEFORE the lock (crypto/rand can block);
	// whether it is actually installed is decided inside the critical section.
	replacement, tokErr := randomToken()
	s.mu.Lock()
	s.credGen.Add(1) // invalidate any login that verified the old hash
	_, requesterLive := s.tokens[requester]
	var cancels []context.CancelFunc
	for tok := range s.tokens {
		// deleteTokenLocked drops the revoked session's live sockets
		// immediately, same as logout — not on the 30 s ticker.
		cancels = append(cancels, s.deleteTokenLocked(tok)...)
	}
	// Mint INSIDE the same critical section as the revoke. Minting after the
	// unlock left an ordering where a concurrent logout processed the (just-
	// revoked, hence missing) old token as a no-op between revoke and mint —
	// and the mint then resurrected a session the user had explicitly ended.
	// Atomically, a logout is either wholly before (requester token gone → no
	// mint) or wholly after (it deletes the OLD token, which no longer
	// exists; the browser's cookie jar is governed by whichever response it
	// processes last, which no server can reorder).
	minted := false
	if requesterLive && tokErr == nil {
		s.tokens[replacement] = time.Now().Add(s.sessionTTLDur())
		minted = true
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	if !minted {
		// Already logged out/expired (no replacement for a token that was
		// gone), or token generation failed (rotation still succeeded; the
		// requester logs in again with the new password).
		w.WriteHeader(http.StatusNoContent)
		return
	}
	token := replacement
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.sessionTTLDur().Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.cfg.SecureCookies,
	})
	w.WriteHeader(http.StatusNoContent)
}
