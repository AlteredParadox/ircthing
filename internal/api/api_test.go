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
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/bcrypt"

	"ircthing/internal/hub"
	"ircthing/internal/irc"
	"ircthing/internal/store"

	ircv4 "gopkg.in/irc.v4"
)

func newTestServer(t *testing.T) (*httptest.Server, *hub.Hub) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := hub.New(st)

	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Username: "AlteredParadox", PasswordHash: string(hash), PreviewsDefault: true}, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, h
}

// newTestServerWithRef is like newTestServer but also returns the *Server
// so proxy tests can relax its IP policy for httptest origins.
func newTestServerWithRef(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Username: "AlteredParadox", PasswordHash: string(hash), PreviewsDefault: true}, hub.New(st), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A known, direct network so media endpoint tests can pass net=testnet
	// and clear the fail-closed proxy resolution (unresolvable networks are
	// refused, not fetched directly).
	if err := st.PutNetworkConfig(context.Background(), testNet,
		`{"name":"testnet","addr":"x:6667","allow_plaintext":true,"nick":"a"}`); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, srv
}

const testNet = "testnet"

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func login(t *testing.T, ts *httptest.Server, username, password string) *http.Response {
	t.Helper()
	body := `{"username":"` + username + `","password":"` + password + `"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL) // same-origin, as a browser sends on POST
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func sessionCookieOf(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie || c.Name == "__Host-"+sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie in response")
	return nil
}

// mediaPost sends an authenticated POST to a media endpoint (/api/preview or
// /api/thumb), which now take {url, net} in the JSON body (keeping the target
// URL out of query-string access logs). A nil cookie sends none.
func mediaPost(t *testing.T, ts *httptest.Server, cookie *http.Cookie, path, target, net string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"url": target, "net": net})
	req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL) // same-origin
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestClientConfigPreviewsEnabled(t *testing.T) {
	ts, _ := newTestServer(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	var cfg struct{ Previews bool }
	req, _ := http.NewRequest("GET", ts.URL+"/api/config", nil)
	req.AddCookie(cookie)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	decodeJSON(t, resp, &cfg)
	resp.Body.Close()
	if !cfg.Previews {
		t.Fatal("config.previews = false, want true when enabled")
	}

	// The endpoint requires auth.
	resp, err = http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth /api/config = %d, want 401", resp.StatusCode)
	}
}

// A state-changing request from a cross-site or same-site (sibling subdomain)
// context is refused via Sec-Fetch-Site; same-origin and direct navigation
// ("none") are allowed. With the header ABSENT (older browser / stripping
// proxy) we fail closed: only a same-origin Origin passes.
func TestSameSiteOnlyBlocksCrossSite(t *testing.T) {
	ts, _ := newTestServer(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	put := func(site, origin string) int {
		req, _ := http.NewRequest("PUT", ts.URL+"/api/config", strings.NewReader(`{"previews":false}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Fetch-metadata present: same-origin / direct navigation trusted.
	for _, site := range []string{"same-origin", "none"} {
		if got := put(site, ""); got == http.StatusForbidden {
			t.Fatalf("Sec-Fetch-Site=%q PUT refused: %d", site, got)
		}
	}
	for _, site := range []string{"cross-site", "same-site"} {
		if got := put(site, ""); got != http.StatusForbidden {
			t.Fatalf("Sec-Fetch-Site=%s PUT = %d, want 403", site, got)
		}
	}
	// No Sec-Fetch-Site: fail closed unless a same-origin Origin is present.
	if got := put("", ""); got != http.StatusForbidden {
		t.Fatalf("no fetch-metadata + no Origin PUT = %d, want 403 (fail closed)", got)
	}
	if got := put("", ts.URL); got == http.StatusForbidden {
		t.Fatalf("no fetch-metadata but same-origin Origin PUT refused: %d", got)
	}
	if got := put("", "https://evil.example"); got != http.StatusForbidden {
		t.Fatalf("cross-origin Origin PUT = %d, want 403", got)
	}
}

func TestPreviewsDisabledGate(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Username: "AlteredParadox", PasswordHash: string(hash), PreviewsDefault: false}, hub.New(st), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	do := func(path string) *http.Response {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.AddCookie(cookie)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	var cfg struct{ Previews bool }
	resp := do("/api/config")
	decodeJSON(t, resp, &cfg)
	resp.Body.Close()
	if cfg.Previews {
		t.Fatal("config.previews = true, want false when disabled")
	}

	resp = mediaPost(t, ts, cookie, "/api/preview", "http://example.com", "")
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("preview endpoint served while disabled: %d", resp.StatusCode)
	}
	resp = mediaPost(t, ts, cookie, "/api/thumb", "http://example.com/x.png", "")
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("thumb endpoint served while disabled: %d", resp.StatusCode)
	}
}

// The previews switch is editable at runtime via PUT /api/config, gates
// the media endpoints live, and persists to the store.
func TestPreviewsToggleRuntime(t *testing.T) {
	ts, srv := newTestServerWithRef(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	do := func(method, path, body string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.AddCookie(cookie)
		req.Header.Set("Origin", ts.URL) // same-origin
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	var cfg struct{ Previews bool }
	resp := do("GET", "/api/config", "")
	decodeJSON(t, resp, &cfg)
	resp.Body.Close()
	if !cfg.Previews {
		t.Fatal("previews should default on")
	}

	// Turn off live -> endpoints refuse.
	resp = do("PUT", "/api/config", `{"previews":false}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable PUT = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do("GET", "/api/config", "")
	decodeJSON(t, resp, &cfg)
	resp.Body.Close()
	if cfg.Previews {
		t.Fatal("previews still on after disable")
	}
	resp = mediaPost(t, ts, cookie, "/api/preview", "http://example.com", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("preview while disabled = %d, want 403", resp.StatusCode)
	}

	// Back on, and persisted (a stored "on" overrides a disabled default).
	resp = do("PUT", "/api/config", `{"previews":true}`)
	resp.Body.Close()
	if !srv.previewsEnabled() {
		t.Fatal("previews not re-enabled")
	}
	if !loadPreviews(context.Background(), srv.hub.Store(), Config{PreviewsDefault: false}) {
		t.Fatal("stored previews=on not read back over the config default")
	}
}

// Retention and session TTL are runtime-editable via PUT /api/config, reflect
// in GET, update the store / effective TTL, and reject out-of-range values.
func TestConfigRetentionAndSessionRuntime(t *testing.T) {
	ts, srv := newTestServerWithRef(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	do := func(method, path, body string) *http.Response {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.AddCookie(cookie)
		req.Header.Set("Origin", ts.URL) // same-origin
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// One logical group per request (both retention dimensions are one group):
	// the two dimensions may share a PUT, but session_ttl is a separate group.
	resp := do("PUT", "/api/config", `{"retention_days":7,"retention_max_messages":500}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("retention PUT = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do("PUT", "/api/config", `{"session_ttl_days":14}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("session PUT = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A patch spanning two groups is rejected outright (non-atomic across
	// groups), changing nothing.
	if resp = do("PUT", "/api/config", `{"retention_days":1,"session_ttl_days":1}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("cross-group PUT = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	var cfg struct {
		RetentionDays        int `json:"retention_days"`
		RetentionMaxMessages int `json:"retention_max_messages"`
		SessionTTLDays       int `json:"session_ttl_days"`
	}
	resp = do("GET", "/api/config", "")
	decodeJSON(t, resp, &cfg)
	resp.Body.Close()
	if cfg.RetentionDays != 7 || cfg.RetentionMaxMessages != 500 || cfg.SessionTTLDays != 14 {
		t.Fatalf("config = %+v, want 7/500/14", cfg)
	}
	if d, m := srv.hub.Store().Retention(); d != 7 || m != 500 {
		t.Fatalf("store retention = %d/%d, want 7/500", d, m)
	}
	if srv.sessionTTLDur() != 14*24*time.Hour {
		t.Fatalf("session TTL = %v, want 14d", srv.sessionTTLDur())
	}

	// Out-of-range values are refused (no partial write).
	if resp = do("PUT", "/api/config", `{"retention_days":-1}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative retention = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp = do("PUT", "/api/config", `{"session_ttl_days":0}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("zero session_ttl = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Validate-before-apply: a bad field must NOT let a valid one in the same
	// PUT take effect. previews starts on; disabling it alongside a bad
	// retention must 400 and leave previews on.
	if resp = do("PUT", "/api/config", `{"previews":false,"retention_days":-5}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mixed valid/invalid PUT = %d, want 400", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if !srv.previewsEnabled() {
		t.Fatal("previews were disabled by a PUT that returned 400 (not atomic)")
	}
}

// Change-password verifies the current password, stores a new bcrypt hash as
// a settings-table override, rotates which password logs in, and persists.
func TestChangePassword(t *testing.T) {
	ts, srv := newTestServerWithRef(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	var rotated *http.Cookie // session cookie the rotation response set (expect a deletion cookie)
	change := func(current, newpw string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/api/password",
			strings.NewReader(`{"current":"`+current+`","new":"`+newpw+`"}`))
		req.AddCookie(cookie)
		req.Header.Set("Origin", ts.URL)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, c := range resp.Cookies() {
			if c.Name == cookie.Name {
				rotated = c
			}
		}
		return resp.StatusCode
	}

	// Correct current but too-short new: 400 (verify passed, so no lockout).
	if code := change("hunter2", "short"); code != http.StatusBadRequest {
		t.Fatalf("short new = %d, want 400", code)
	}
	// Valid change.
	if code := change("hunter2", "newpassword1"); code != http.StatusNoContent {
		t.Fatalf("change = %d, want 204", code)
	}
	// The new password logs in; the old one no longer does. (new-first so the
	// old failure's rate-limit block doesn't shadow the success.)
	if code := login(t, ts, "AlteredParadox", "newpassword1").StatusCode; code != http.StatusNoContent {
		t.Fatalf("login with new password = %d, want 204", code)
	}
	if code := login(t, ts, "AlteredParadox", "hunter2").StatusCode; code != http.StatusUnauthorized {
		t.Fatalf("login with old password = %d, want 401", code)
	}
	// Rotation revokes EVERY session including the requester's (a stolen copy
	// must not survive the recovery action) and mints NO replacement — the
	// response carries a deletion cookie, and the requester logs in again.
	// This removes the mint/logout ordering race entirely.
	authProbe := func(c *http.Cookie) int {
		req, _ := http.NewRequest("GET", ts.URL+"/api/ws", nil)
		req.AddCookie(c)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := authProbe(cookie); code != http.StatusUnauthorized {
		t.Fatalf("old token after rotation = %d, want 401 (stolen copies must die)", code)
	}
	if rotated == nil {
		t.Fatal("rotation response carried no cookie at all (expected a deletion cookie)")
	}
	if rotated.Value != "" || rotated.MaxAge >= 0 {
		t.Fatalf("rotation minted a replacement session (value=%q maxAge=%d); want a deletion cookie", rotated.Value, rotated.MaxAge)
	}

	// The override is persisted and read back over the config hash.
	h, err := loadPasswordHash(context.Background(), srv.hub.Store(), Config{PasswordHash: "seed-ignored"})
	if err != nil {
		t.Fatalf("loadPasswordHash: %v", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(h), []byte("newpassword1")) != nil {
		t.Fatal("stored override does not verify the new password")
	}
}

// A wrong current password is refused (403) without changing anything.
func TestChangePasswordWrongCurrent(t *testing.T) {
	ts, srv := newTestServerWithRef(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	req, _ := http.NewRequest("POST", ts.URL+"/api/password",
		strings.NewReader(`{"current":"wrongpass","new":"newpassword1"}`))
	req.AddCookie(cookie)
	req.Header.Set("Origin", ts.URL)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong current = %d, want 403", resp.StatusCode)
	}
	// Nothing was stored: no override hash was persisted.
	if v, _ := srv.hub.Store().Setting(context.Background(), passwordHashKey); v != "" {
		t.Fatalf("wrong-current attempt stored an override: %q", v)
	}
}

// Previews use the source network's egress: egressForNetwork resolves
// direct/proxy/tunnel from the stored config, the per-proxy and per-network
// fetcher pools build + reuse one fetcher each, and a WireGuard network's
// fetch fails closed (never falls back to direct) when its tunnel is down.
func TestEgressForNetwork(t *testing.T) {
	_, srv := newTestServerWithRef(t)
	ctx := context.Background()
	st := srv.hub.Store()
	if err := st.PutNetworkConfig(ctx, "tornet",
		`{"name":"tornet","addr":"x:6697","tls":true,"nick":"a","proxy":"socks5://user:pass@127.0.0.1:9050"}`); err != nil {
		t.Fatal(err)
	}
	if err := st.PutNetworkConfig(ctx, "direct",
		`{"name":"direct","addr":"y:6667","allow_plaintext":true,"nick":"a"}`); err != nil {
		t.Fatal(err)
	}
	if err := st.PutNetworkConfig(ctx, "malformed", `{not valid json`); err != nil {
		t.Fatal(err)
	}
	if err := st.PutNetworkConfig(ctx, "badproxy",
		`{"name":"badproxy","addr":"z:6667","allow_plaintext":true,"nick":"a","proxy":"ftp://x:1"}`); err != nil {
		t.Fatal(err)
	}
	if err := st.PutNetworkConfig(ctx, "wgnet",
		`{"name":"wgnet","addr":"w:6697","tls":true,"nick":"a","wireguard":{"private_key":"AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","peer_public_key":"AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","endpoint":"203.0.113.7:51820","address":"10.64.0.2","dns":"10.64.0.1"}}`); err != nil {
		t.Fatal(err)
	}

	if e := srv.egressForNetwork(ctx, "tornet"); !e.ok || e.proxy == nil || e.proxy.Host != "127.0.0.1:9050" || e.tunnel {
		t.Fatalf("tornet = %+v; want proxy 127.0.0.1:9050", e)
	}
	// Known direct network: ok, no proxy, no tunnel.
	if e := srv.egressForNetwork(ctx, "direct"); !e.ok || e.proxy != nil || e.tunnel {
		t.Fatalf("direct = %+v; want direct (ok, no proxy/tunnel)", e)
	}
	// A WireGuard network resolves to TUNNEL egress — the fetch dials through
	// the tunnel, never directly, so the real IP is never leaked.
	if e := srv.egressForNetwork(ctx, "wgnet"); !e.ok || !e.tunnel || e.network != "wgnet" || e.proxy != nil {
		t.Fatalf("wgnet = %+v; want tunnel egress for wgnet", e)
	}
	// Everything unresolvable must FAIL CLOSED (ok=false).
	for _, name := range []string{"nonexistent", "", "malformed", "badproxy"} {
		if e := srv.egressForNetwork(ctx, name); e.ok {
			t.Fatalf("egressForNetwork(%q) = %+v; want fail-closed (ok=false)", name, e)
		}
	}
	// Fetcher selection by egress.
	if f := srv.htmlFetcherForNetwork(ctx, "tornet"); f == nil || !f.proxied {
		t.Fatalf("tornet html fetcher = %v; want proxied", f)
	}
	if f := srv.htmlFetcherForNetwork(ctx, "tornet"); f != srv.htmlFetcherForNetwork(ctx, "tornet") {
		t.Fatal("per-proxy fetcher not cached/reused")
	}
	if f := srv.htmlFetcherForNetwork(ctx, "direct"); f == nil || f.proxied {
		t.Fatal("direct fetcher must not be proxied")
	}
	for _, name := range []string{"nonexistent", "malformed", "badproxy"} {
		if f := srv.htmlFetcherForNetwork(ctx, name); f != nil {
			t.Fatalf("fetcher for %q = %v; want nil (fail closed)", name, f)
		}
	}
	// The WireGuard network's fetcher exists (tunnel egress) and is cached,
	// but with no network actually running its dial fails closed — it must
	// NEVER fall back to a direct fetch (which would leak the real IP).
	wgf := srv.htmlFetcherForNetwork(ctx, "wgnet")
	if wgf == nil || !wgf.proxied {
		t.Fatalf("wgnet fetcher = %v; want a proxied-mode tunnel fetcher", wgf)
	}
	if wgf != srv.htmlFetcherForNetwork(ctx, "wgnet") {
		t.Fatal("per-network tunnel fetcher not cached/reused")
	}
	if _, _, _, err := wgf.get(ctx, "https://example.com/"); err == nil {
		t.Fatal("wgnet fetch with no running tunnel must fail closed, not fall back to direct")
	}
}

// The per-proxy fetcher pool stays bounded across many distinct proxies
// (proxy rotations over a long-lived process), rather than retaining every
// obsolete credential-bearing fetcher forever.
func TestFetcherPoolBounded(t *testing.T) {
	_, srv := newTestServerWithRef(t)
	for i := 0; i < maxProxyFetchers+10; i++ {
		srv.htmlFetcherFor(&url.URL{Scheme: "socks5", Host: "127.0.0.1:" + strconv.Itoa(1000+i)})
	}
	srv.mediaMu.RLock()
	n := len(srv.htmlByProxy)
	srv.mediaMu.RUnlock()
	if n > maxProxyFetchers {
		t.Fatalf("htmlByProxy = %d, want <= %d", n, maxProxyFetchers)
	}
}

// A preview/thumb request tagged with a network that can't be resolved to a
// direct-or-proxied decision is refused, not fetched directly (which would
// leak the egress IP for a link that belongs to a proxied network).
func TestMediaFailsClosedOnUnknownNetwork(t *testing.T) {
	ts, srvObj := newTestServerWithRef(t)
	permit(srvObj)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	cases := []struct{ path, target, net string }{
		{"/api/preview", "http://example.com", "ghostnet"},
		{"/api/preview", "http://example.com", ""}, // no net at all
		{"/api/thumb", "http://example.com/x.png", "ghostnet"},
	}
	for _, tc := range cases {
		resp := mediaPost(t, ts, cookie, tc.path, tc.target, tc.net)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s net=%q = %d, want 502 (fail closed)", tc.path, tc.net, resp.StatusCode)
		}
	}
}

func TestLogin(t *testing.T) {
	ts, srv := newTestServerWithRef(t)
	cases := []struct {
		name, user, pass string
		wantStatus       int
	}{
		{"valid credentials", "AlteredParadox", "hunter2", http.StatusNoContent},
		{"wrong password", "AlteredParadox", "wrong", http.StatusUnauthorized},
		{"wrong username", "nobody", "hunter2", http.StatusUnauthorized},
		{"empty", "", "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each case starts with a clean slate; failure backoff is
			// exercised separately in TestLoginBackoff.
			srv.login.mu.Lock()
			clear(srv.login.sources)
			srv.login.mu.Unlock()
			resp := login(t, ts, tc.user, tc.pass)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusNoContent {
				c := sessionCookieOf(t, resp)
				if !c.HttpOnly || c.Value == "" {
					t.Fatalf("cookie not HttpOnly or empty: %+v", c)
				}
			}
		})
	}
}

func TestWSRequiresAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/ws")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	// A bogus cookie is rejected too.
	req, _ := http.NewRequest("GET", ts.URL+"/api/ws", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "forged"})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged cookie status = %d, want 401", resp.StatusCode)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	ts, _ := newTestServer(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	req, _ := http.NewRequest("POST", ts.URL+"/api/logout", nil)
	req.Header.Set("Origin", ts.URL)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", ts.URL+"/api/ws", nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ws after logout status = %d, want 401", resp.StatusCode)
	}
}

// fakeConn implements hub.Conn so tests can inject IRC events through the
// real fan-out path.
type fakeConn struct {
	ch   chan irc.Event
	name string
	nick string
}

func (f *fakeConn) Events() <-chan irc.Event  { return f.ch }
func (f *fakeConn) Name() string              { return f.name }
func (f *fakeConn) Nick() string              { return f.nick }
func (f *fakeConn) Send(*ircv4.Message) error      { return nil }
func (f *fakeConn) SendAll([]*ircv4.Message) error { return nil }

func (f *fakeConn) Channel(string) (string, []irc.Member, bool) {
	return "", nil, false
}

func (f *fakeConn) CapEnabled(string) bool { return false }

func (f *fakeConn) IsChannel(t string) bool {
	return t != "" && (t[0] == '#' || t[0] == '&')
}

func (f *fakeConn) ChanTypes() string       { return "#&" }
func (f *fakeConn) StatusPrefixes() string  { return "~&@%+" }
func (f *fakeConn) Fold(name string) string { return strings.ToLower(name) }

func (f *fakeConn) RequestChatHistory(string, int64, string) {}
func (f *fakeConn) HistoryPageSize() int                     { return 100 }

func (f *fakeConn) EnsureNames(string) {}

func (f *fakeConn) SendMultiline(string, []string) error { return nil }
func (f *fakeConn) ReconcileMonitored([]string) error    { return nil }
func (f *fakeConn) MonitorRejected([]string, int, uint64, []string) error { return nil }

func (f *fakeConn) privmsg(line string) irc.Event {
	return irc.Event{
		Network: f.name,
		Kind:    irc.EventMessage,
		Msg:     ircv4.MustParseMessage(line),
		Time:    time.Now(),
	}
}

// TestWSEndToEnd drives the full path: login, upgrade, receive a live
// push fanned out from an IRC event, page history over the socket, and
// survive an unknown envelope type.
func TestWSEndToEnd(t *testing.T) {
	ts, h := newTestServer(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := &fakeConn{ch: make(chan irc.Event, 4), name: "libera", nick: "AlteredParadox"}
	go h.Run(ctx, conn)

	header := http.Header{}
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	header.Set("Origin", ts.URL) // same-origin: handleWS requires a matching Origin
	c, _, err := websocket.Dial(ctx, ts.URL+"/api/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	var env hub.Envelope
	readEnv := func() {
		t.Helper()
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("bad envelope: %v", err)
		}
	}

	// A message arriving from IRC is pushed live over the socket.
	conn.ch <- conn.privmsg(":alice!u@h PRIVMSG #go :hello")
	readEnv()
	if env.Type != "event" {
		t.Fatalf("expected event push, got %+v", env)
	}
	var ev hub.EventData
	if err := json.Unmarshal(env.Data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Raw == "" || ev.Sender != "alice" || ev.Buffer != "#go" {
		t.Fatalf("event = %+v", ev)
	}

	// The same message comes back from a history request.
	req, _ := json.Marshal(map[string]any{
		"v": 1, "type": "get_history", "seq": 42,
		"data": map[string]any{"network": "libera", "buffer": "#go", "limit": 10},
	})
	if err := c.Write(ctx, websocket.MessageText, req); err != nil {
		t.Fatal(err)
	}
	readEnv()
	if env.Type != "history" || env.Seq != 42 {
		t.Fatalf("envelope = %+v", env)
	}
	var page hub.HistoryData
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Sender != "alice" {
		t.Fatalf("history = %+v", page)
	}

	// An unknown envelope type is ignored, and the connection lives on to
	// deliver the next push.
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"v":1,"type":"future_thing"}`)); err != nil {
		t.Fatal(err)
	}
	conn.ch <- conn.privmsg(":bob!u@h PRIVMSG #go :live push")
	readEnv()
	if env.Type != "event" {
		t.Fatalf("expected event push after unknown type, got %+v", env)
	}
	if err := json.Unmarshal(env.Data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Sender != "bob" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestLoginBackoff(t *testing.T) {
	ts, srv := newTestServerWithRef(t)

	// First failure blocks the source; an immediate retry is refused
	// without reaching bcrypt.
	if resp := login(t, ts, "AlteredParadox", "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first failure status = %d", resp.StatusCode)
	}
	resp := login(t, ts, "AlteredParadox", "hunter2")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("during backoff status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}

	// Once the block expires, a good login succeeds and clears the slate.
	srv.login.mu.Lock()
	for _, s := range srv.login.sources {
		s.blockedUntil = time.Now().Add(-time.Second)
	}
	srv.login.mu.Unlock()
	if resp := login(t, ts, "AlteredParadox", "hunter2"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("after backoff status = %d", resp.StatusCode)
	}
	srv.login.mu.Lock()
	n := len(srv.login.sources)
	srv.login.mu.Unlock()
	if n != 0 {
		t.Fatalf("success did not clear the source (%d left)", n)
	}
}

func TestLoginBackoffGrows(t *testing.T) {
	l := newLoginLimiter()
	now := time.Now()
	var waits []time.Duration
	for i := 0; i < 8; i++ {
		l.fail("src", now)
		waits = append(waits, l.retryAfter("src", now))
	}
	if waits[0] != time.Second || waits[1] != 2*time.Second || waits[2] != 4*time.Second {
		t.Fatalf("early backoff = %v", waits[:3])
	}
	if waits[7] != time.Minute {
		t.Fatalf("backoff cap = %v, want 1m", waits[7])
	}
	if l.retryAfter("other", now) != 0 {
		t.Fatal("unrelated source blocked")
	}
}

// The global bucket caps total attempt rate regardless of source: rotating
// source addresses (each getting a free first attempt from the per-source
// tracker) must not buy unlimited bcrypt work.
func TestLoginGlobalBucket(t *testing.T) {
	l := newLoginLimiter()
	now := time.Now()
	// The full burst passes, then the bucket is dry.
	for i := 0; i < int(loginGlobalBurst); i++ {
		if wait := l.globalAllow(now); wait != 0 {
			t.Fatalf("burst attempt %d blocked (wait %v)", i, wait)
		}
	}
	wait := l.globalAllow(now)
	if wait <= 0 || wait > time.Second*2 {
		t.Fatalf("post-burst wait = %v, want ~1 token interval", wait)
	}
	// Refill: after one token interval a single attempt passes again…
	later := now.Add(time.Duration(float64(time.Second) / loginGlobalRate))
	if wait := l.globalAllow(later); wait != 0 {
		t.Fatalf("refilled attempt blocked (wait %v)", wait)
	}
	// …but only one — the very next is dry again.
	if wait := l.globalAllow(later); wait <= 0 {
		t.Fatal("second attempt after single refill should be blocked")
	}
	// A long idle period refills at most to the burst cap.
	idle := later.Add(time.Hour)
	for i := 0; i < int(loginGlobalBurst); i++ {
		if wait := l.globalAllow(idle); wait != 0 {
			t.Fatalf("post-idle attempt %d blocked (wait %v)", i, wait)
		}
	}
	if wait := l.globalAllow(idle); wait <= 0 {
		t.Fatal("burst cap not enforced after idle refill")
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := ts.Client().Get(ts.URL + "/api/ws") // any route gets them
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
		"X-DNS-Prefetch-Control": "off", // rendered links are attacker-controlled; no per-hostname DNS leak
	} {
		if got := resp.Header.Get(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "connect-src 'self'", "img-src 'self' data:"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q missing %q", csp, want)
		}
	}
}

func TestSecureCookieFlag(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, _ := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	srv, err := New(Config{Username: "AlteredParadox", PasswordHash: string(hash), SecureCookies: true}, hub.New(st), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	c := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	if !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v, want Secure + SameSite=Strict", c)
	}
	// Under SecureCookies the cookie carries the __Host- prefix (defeats
	// sibling-subdomain cookie-tossing).
	if c.Name != "__Host-"+sessionCookie {
		t.Fatalf("session cookie name = %q, want __Host- prefix", c.Name)
	}
	// Logout's deletion cookie carries matching attributes.
	req, _ := http.NewRequest("POST", ts.URL+"/api/logout", nil)
	req.Header.Set("Origin", ts.URL)
	req.AddCookie(c)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	del := sessionCookieOf(t, resp)
	if !del.Secure || del.SameSite != http.SameSiteStrictMode || del.MaxAge >= 0 {
		t.Fatalf("deletion cookie = %+v, want Secure + Strict + expired", del)
	}
}

// Session tokens are pruned at issue time: expired entries go, and the
// live set is capped by evicting the oldest.
func TestSessionPruning(t *testing.T) {
	ts, srv := newTestServerWithRef(t)

	srv.mu.Lock()
	srv.tokens["expired"] = time.Now().Add(-time.Hour)
	for i := 0; i < maxSessions; i++ {
		srv.tokens[strconv.Itoa(i)] = time.Now().Add(time.Duration(i+1) * time.Minute)
	}
	srv.mu.Unlock()

	sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.tokens) > maxSessions {
		t.Fatalf("tokens = %d, want <= %d", len(srv.tokens), maxSessions)
	}
	if _, ok := srv.tokens["expired"]; ok {
		t.Fatal("expired token survived a login")
	}
	if _, ok := srv.tokens["0"]; ok {
		t.Fatal("oldest token not evicted at the cap")
	}
	if _, ok := srv.tokens[strconv.Itoa(maxSessions-1)]; !ok {
		t.Fatal("newest pre-existing token wrongly evicted")
	}
}

// A large set_prefs (custom CSS between the 32 KiB default read limit
// and the 64 KiB prefs cap) must round-trip: the WS read limit is sized
// to admit it rather than closing the connection as oversized.
func TestWSLargePrefs(t *testing.T) {
	ts, h := newTestServer(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := &fakeConn{ch: make(chan irc.Event, 4), name: "libera", nick: "AlteredParadox"}
	go h.Run(ctx, conn)

	header := http.Header{}
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	header.Set("Origin", ts.URL) // same-origin: handleWS requires a matching Origin
	c, _, err := websocket.Dial(ctx, ts.URL+"/api/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	c.SetReadLimit(hub.MaxPrefsBytes + 32*1024) // client must accept the echo too

	// ~48 KiB of CSS: over the 32 KiB default, under the 64 KiB cap.
	css := strings.Repeat("a", 48*1024)
	prefs, err := json.Marshal(map[string]string{"css": css})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(hub.PrefsData{Prefs: prefs})
	if err != nil {
		t.Fatal(err)
	}
	env := hub.Envelope{V: hub.ProtocolVersion, Type: "set_prefs", Seq: 7, Data: data}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 32*1024 {
		t.Fatalf("test payload %d bytes, must exceed the 32 KiB default", len(raw))
	}
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("ws write: %v", err)
	}

	// The handler acknowledges rather than the connection closing.
	_, respRaw, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws read (connection closed as oversized?): %v", err)
	}
	var resp hub.Envelope
	if err := json.Unmarshal(respRaw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Type != "ok" || resp.Seq != 7 {
		t.Fatalf("response = %+v, want ok/seq 7", resp)
	}

	// And it round-trips: get_prefs returns the large blob verbatim
	// (also proving the read limit admits the reply, not just the send).
	getEnv := hub.Envelope{V: hub.ProtocolVersion, Type: "get_prefs", Seq: 8}
	getRaw, err := json.Marshal(getEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, getRaw); err != nil {
		t.Fatal(err)
	}
	_, gotRaw, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("get_prefs read: %v", err)
	}
	var got hub.Envelope
	if err := json.Unmarshal(gotRaw, &got); err != nil {
		t.Fatal(err)
	}
	var pd hub.PrefsData
	if err := json.Unmarshal(got.Data, &pd); err != nil {
		t.Fatal(err)
	}
	if string(pd.Prefs) != string(prefs) {
		t.Fatalf("round-tripped prefs length %d, want %d", len(pd.Prefs), len(prefs))
	}
}

// A live WebSocket is revoked when its session token is invalidated
// (logout or expiry), not left working until the socket happens to drop.
func TestWSRevokedOnLogout(t *testing.T) {
	old := sessionRecheckInterval.Load()
	sessionRecheckInterval.Store(int64(50 * time.Millisecond))
	defer sessionRecheckInterval.Store(old)

	ts, h := newTestServer(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := &fakeConn{ch: make(chan irc.Event, 4), name: "libera", nick: "AlteredParadox"}
	go h.Run(ctx, conn)

	header := http.Header{}
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	header.Set("Origin", ts.URL) // same-origin: handleWS requires a matching Origin
	c, _, err := websocket.Dial(ctx, ts.URL+"/api/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// Invalidate the token (as logout does).
	req, _ := http.NewRequest("POST", ts.URL+"/api/logout", nil)
	req.Header.Set("Origin", ts.URL)
	req.AddCookie(cookie)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// The socket must close on its own within a few recheck intervals.
	rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
	defer rcancel()
	if _, _, err := c.Read(rctx); err == nil {
		t.Fatal("read succeeded after logout; socket not revoked")
	} else if rctx.Err() != nil {
		t.Fatal("socket still open 3s after logout")
	}
}

// Logout must tear the token's live sockets down IMMEDIATELY, not on the next
// revalidation tick — a stolen already-open socket must not keep receiving IRC
// traffic for up to 30 s. The ticker is set absurdly long so only the immediate
// cancel path can close the socket within the deadline.
func TestWSRevokedImmediatelyOnLogout(t *testing.T) {
	old := sessionRecheckInterval.Load()
	sessionRecheckInterval.Store(int64(time.Hour))
	defer sessionRecheckInterval.Store(old)

	ts, h := newTestServer(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := &fakeConn{ch: make(chan irc.Event, 4), name: "libera", nick: "AlteredParadox"}
	go h.Run(ctx, conn)

	header := http.Header{}
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	header.Set("Origin", ts.URL)
	c, _, err := websocket.Dial(ctx, ts.URL+"/api/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	req, _ := http.NewRequest("POST", ts.URL+"/api/logout", nil)
	req.Header.Set("Origin", ts.URL)
	req.AddCookie(cookie)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// With the ticker out of the picture, only the logout-path cancel can close
	// this socket. 3 s is generous for a context cancel + close.
	rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
	defer rcancel()
	if _, _, err := c.Read(rctx); err == nil {
		t.Fatal("read succeeded after logout; socket not revoked")
	} else if rctx.Err() != nil {
		t.Fatal("socket still open 3s after logout — immediate revocation not wired")
	}
}

// The login backoff keys on the forwarded client IP only when TrustProxyForwarded
// is set, so one attacker behind a proxy can't lock out every user.
func TestLoginSourceKeyForwarded(t *testing.T) {
	srv := &Server{cfg: Config{TrustProxyForwarded: true}}
	req := httptest.NewRequest("POST", "/api/login", nil)
	req.RemoteAddr = "10.0.0.1:5555" // the proxy

	// X-Real-IP is NOT trusted: the recommended proxy (Caddy) forwards a
	// client-set X-Real-IP unchanged, so it must be ignored — fall back to the
	// socket peer (RemoteAddr) when it's the only forwarded header present.
	req.Header.Set("X-Real-IP", "203.0.113.7")
	if got := srv.loginSourceKey(req); got != "10.0.0.1" {
		t.Fatalf("X-Real-IP must be ignored, source = %q, want 10.0.0.1 (socket peer)", got)
	}
	req.Header.Del("X-Real-IP")
	// The last X-Forwarded-For hop (appended by the trusted proxy) is the real
	// client and is used even when earlier, client-forged entries precede it.
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	if got := srv.loginSourceKey(req); got != "203.0.113.9" {
		t.Fatalf("XFF source = %q, want 203.0.113.9", got)
	}
	// A garbage last hop is rejected, falling back to the socket peer.
	req.Header.Set("X-Forwarded-For", "1.2.3.4, notanip")
	if got := srv.loginSourceKey(req); got != "10.0.0.1" {
		t.Fatalf("non-IP XFF hop source = %q, want 10.0.0.1 (socket peer)", got)
	}
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.9")
	// Untrusted: client-settable headers are ignored, fall back to RemoteAddr.
	srv.cfg.TrustProxyForwarded = false
	if got := srv.loginSourceKey(req); got != "10.0.0.1" {
		t.Fatalf("untrusted source = %q, want 10.0.0.1", got)
	}
}
