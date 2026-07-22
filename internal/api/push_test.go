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
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func pushPost(t *testing.T, ts *httptest.Server, cookie *http.Cookie, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func testSubscriptionJSON(t *testing.T, endpoint string) string {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"endpoint": endpoint,
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(auth),
		},
	})
	return string(b)
}

func TestPushSubscribeFlow(t *testing.T) {
	ts, h := newTestServer(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	ctx := context.Background()

	// Unauthenticated: refused.
	if resp := pushPost(t, ts, nil, "/api/push/subscribe", testSubscriptionJSON(t, "https://push.example/a")); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated subscribe = %d", resp.StatusCode)
	}

	// Subscribe, then re-subscribe the same endpoint (idempotent upsert).
	sub := testSubscriptionJSON(t, "https://push.example/a")
	for range 2 {
		if resp := pushPost(t, ts, cookie, "/api/push/subscribe", sub); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("subscribe = %d", resp.StatusCode)
		}
	}
	if n, _ := h.Store().CountPushSubscriptions(ctx); n != 1 {
		t.Fatalf("stored subscriptions = %d", n)
	}

	// Invalid inputs are refused before storage.
	for name, body := range map[string]string{
		"http endpoint":     testSubscriptionJSON(t, "http://push.example/a"),
		"private endpoint":  testSubscriptionJSON(t, "https://10.0.0.1/a"),
		"malformed json":    `{"endpoint":`,
		"missing keys":      `{"endpoint":"https://push.example/b","keys":{}}`,
		"short p256dh":      `{"endpoint":"https://push.example/b","keys":{"p256dh":"AAEC","auth":"AAAAAAAAAAAAAAAAAAAAAA"}}`,
		"compressed p256dh": `{"endpoint":"https://push.example/b","keys":{"p256dh":"` + base64.RawURLEncoding.EncodeToString(append([]byte{0x02}, make([]byte, 64)...)) + `","auth":"AAAAAAAAAAAAAAAAAAAAAA"}}`,
	} {
		if resp := pushPost(t, ts, cookie, "/api/push/subscribe", body); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: subscribe = %d, want 400", name, resp.StatusCode)
		}
	}
	if n, _ := h.Store().CountPushSubscriptions(ctx); n != 1 {
		t.Fatalf("subscriptions after rejects = %d", n)
	}

	// Unsubscribe is idempotent.
	for range 2 {
		if resp := pushPost(t, ts, cookie, "/api/push/unsubscribe", `{"endpoint":"https://push.example/a"}`); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("unsubscribe = %d", resp.StatusCode)
		}
	}
	if n, _ := h.Store().CountPushSubscriptions(ctx); n != 0 {
		t.Fatalf("subscriptions after unsubscribe = %d", n)
	}
}

func TestClientConfigCarriesPushKey(t *testing.T) {
	ts, _ := newTestServer(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	req, _ := http.NewRequest("GET", ts.URL+"/api/config", nil)
	req.AddCookie(cookie)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var cfg struct {
		PushPublicKey *string `json:"push_public_key"`
	}
	decodeJSON(t, resp, &cfg)
	// The pusher never ran in this test, so the key is EMPTY — but the
	// field must be present (the client keys "unavailable" off "").
	if cfg.PushPublicKey == nil || *cfg.PushPublicKey != "" {
		t.Fatalf("push_public_key = %v", cfg.PushPublicKey)
	}
}
