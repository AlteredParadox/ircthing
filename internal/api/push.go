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
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"ircthing/internal/hub"
	"ircthing/internal/store"
	"ircthing/internal/webpush"
)

// Web Push subscription registration. HTTP (not WebSocket types) because
// the service worker's pushsubscriptionchange handler must be able to
// re-register with no page and no socket — a plain same-origin fetch
// carries the session cookie.

// pushSubscribeBody is the standard PushSubscription.toJSON() shape.
type pushSubscribeBody struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// decodeKey accepts base64url with or without padding (browsers emit
// unpadded; be liberal in what we accept) and requires an exact length.
func decodeKey(s string, wantLen int) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		if b, err = base64.URLEncoding.DecodeString(s); err != nil {
			return nil, err
		}
	}
	if len(b) != wantLen {
		return nil, errors.New("bad key length")
	}
	return b, nil
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	var body pushSubscribeBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "malformed subscription", http.StatusBadRequest)
		return
	}
	if err := webpush.ValidateEndpoint(body.Endpoint); err != nil {
		http.Error(w, "bad endpoint", http.StatusBadRequest)
		return
	}
	// p256dh: 65-octet uncompressed P-256 point; auth: 16-octet secret
	// (RFC 8291 §2/§3.1). Stored unpadded so the pusher decodes uniformly.
	p256dh, err := decodeKey(body.Keys.P256dh, 65)
	if err != nil || p256dh[0] != 0x04 {
		http.Error(w, "bad p256dh key", http.StatusBadRequest)
		return
	}
	auth, err := decodeKey(body.Keys.Auth, 16)
	if err != nil {
		http.Error(w, "bad auth secret", http.StatusBadRequest)
		return
	}
	// Re-check auth AFTER the (attacker-pausable) body read, UNDER the
	// push-mutation barrier: requireAuth validated the cookie before the
	// body arrived, so a stolen session could pause its upload past a
	// password rotation — which wipes subscriptions and revokes tokens —
	// then finish and re-plant its endpoint. Holding pushMutationMu
	// across the recheck+insert serializes against rotation's wipe+revoke
	// (which takes the same barrier), so this runs strictly before (its
	// insert is wiped) or strictly after (authed() fails, token revoked).
	//
	// TryLock, not Lock: rotation blocks-acquires the barrier, so a stolen
	// session cannot pile up serialized inserts ahead of the recovery —
	// concurrent mutations bounce with 429 while rotation holds it.
	if !s.pushMutationMu.TryLock() {
		http.Error(w, "busy, retry later", http.StatusTooManyRequests)
		return
	}
	defer s.pushMutationMu.Unlock()
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	err = s.hub.Store().UpsertPushSubscription(r.Context(), store.PushSubscription{
		Endpoint: body.Endpoint,
		P256dh:   base64.RawURLEncoding.EncodeToString(p256dh),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
	})
	if errors.Is(err, store.ErrPushSubscriptionCap) {
		http.Error(w, "too many push subscriptions", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "storing subscription failed", http.StatusInternalServerError)
		return
	}
	refreshPushCountDetached(s.hub)
	w.WriteHeader(http.StatusNoContent)
}

// refreshPushCountDetached refreshes the pusher's cached subscription
// count on a context that survives the request: the row is already
// committed, and a client abort (a service worker killed mid-fetch)
// cancelling r.Context() here would strand the cache — at 0, that
// silently disables every push despite a valid stored subscription.
func refreshPushCountDetached(h *hub.Hub) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.RefreshPushCount(ctx)
}

func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Endpoint == "" {
		http.Error(w, "malformed unsubscribe", http.StatusBadRequest)
		return
	}
	// Same barrier + auth-recheck as subscribe (TryLock so rotation is
	// never queued behind a stale-token flood).
	if !s.pushMutationMu.TryLock() {
		http.Error(w, "busy, retry later", http.StatusTooManyRequests)
		return
	}
	defer s.pushMutationMu.Unlock()
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Idempotent: unsubscribing an unknown endpoint is fine — the browser
	// may retry after we already pruned it on a 410.
	if err := s.hub.Store().DeletePushSubscription(r.Context(), body.Endpoint); err != nil {
		http.Error(w, "removing subscription failed", http.StatusInternalServerError)
		return
	}
	refreshPushCountDetached(s.hub)
	w.WriteHeader(http.StatusNoContent)
}
