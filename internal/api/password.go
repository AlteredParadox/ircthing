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
	"log"
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
	v, present, err := st.SettingValue(ctx, passwordHashKey)
	if err != nil {
		return "", fmt.Errorf("reading stored password override: %w", err)
	}
	if !present {
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
		http.Error(w, msgTooManyAttempts, http.StatusTooManyRequests)
		return
	}
	if wait := s.login.globalAllow(time.Now()); wait > 0 {
		http.Error(w, msgTooManyAttempts, http.StatusTooManyRequests)
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
		http.Error(w, msgTooManyAttempts, http.StatusTooManyRequests)
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
		// Same brute-force class as a failed login (a stolen session
		// guessing the current password to rotate it): logged with the
		// proxy-aware source so the same fail2ban filter catches it.
		log.Printf("login: failed password-change verification from %s", source)
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
	// Hold the push-mutation barrier across BOTH the subscription wipe
	// and the token revocation below: a concurrent subscribe takes the
	// same barrier, so it cannot slip an insert into the window between
	// them (which would survive recovery). Released after the revoke.
	s.pushMutationMu.Lock()
	defer s.pushMutationMu.Unlock()

	// Store the new hash AND wipe every push subscription in ONE
	// transaction: rotation is the compromise-recovery lever, so the
	// hash change and the revocation of push grants (a stolen session
	// may have planted an attacker endpoint) must not be separable — a
	// deletion failure previously only logged and still returned 204,
	// leaving the planted endpoint live under the new password.
	if err := s.hub.Store().SetSettingAndWipePushSubscriptions(r.Context(), passwordHashKey, string(newHash)); err != nil {
		http.Error(w, "storing failed", http.StatusInternalServerError)
		return
	}
	nh := string(newHash)
	s.passwordHash.Store(&nh)
	// Invalidate any in-flight delivery synchronously with the wipe (and
	// under the barrier): a worker that loaded its endpoint slice before
	// this wipe must not keep sending to a pre-rotation endpoint.
	s.hub.BumpPushEpoch()

	// Bump the generation and revoke EVERY session ATOMICALLY under the same
	// s.mu that issueToken takes. This closes the login race: a concurrent
	// old-password login either loses this lock (issueToken then observes the
	// new generation and refuses to mint) or wins it (and this revoke deletes
	// the session it just inserted). The requester's own session is revoked
	// too — rotation is the compromise-recovery action, so a stolen copy of
	// that exact cookie must not survive it.
	//
	// NO replacement token is minted: rotation revokes all and sends a
	// deletion cookie, and the requester logs in again with the new password.
	// This is strictly simpler than rotating onto a fresh cookie and removes
	// the mint/logout ordering race entirely — a concurrent logout and this
	// rotation both end in "no valid token, deletion cookie", regardless of
	// which HTTP response the browser processes last (RFC 6265 §4.1.1 leaves
	// concurrent Set-Cookie ordering undefined, so there must be nothing to
	// reorder).
	s.mu.Lock()
	s.credGen.Add(1) // invalidate any login that verified the old hash
	var cancels []context.CancelFunc
	for tok := range s.tokens {
		// deleteTokenLocked drops the revoked session's live sockets
		// immediately, same as logout — not on the 30 s ticker.
		cancels = append(cancels, s.deleteTokenLocked(tok)...)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	// The subscription wipe committed with the hash above; refresh the
	// pusher's cached count off the request path so a client abort can't
	// strand it. (Push subscriptions are the OTHER credential-shaped
	// grant a stolen session can plant; legitimate devices re-register
	// via the client's on-load resync after logging back in.)
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.hub.RefreshPushCount(ctx)
	}()
	// Audit the rotation (a security-relevant event: it revokes every other
	// session). Distinct wording from the login lines and NOT matched by the
	// fail2ban failregex — a successful change is not an attack.
	log.Printf("login: password changed from %s", source)
	// Deletion cookie: same attributes as the session cookie so the browser
	// treats it as the same cookie and drops it.
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.cfg.SecureCookies,
	})
	w.WriteHeader(http.StatusNoContent)
}
