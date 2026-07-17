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
	body := strings.NewReader(`{"username":"` + username + `","password":"` + password + `"}`)
	resp, err := http.Post(ts.URL+"/api/login", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func sessionCookieOf(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie in response")
	return nil
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

// With previews disabled the media endpoints are not served (zero outbound
// fetch) and /api/config advertises it so the UI stops requesting them.
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

	resp = do("/api/preview?url=http://example.com")
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("preview endpoint served while disabled: %d", resp.StatusCode)
	}
	resp = do("/api/thumb?url=http://example.com/x.png")
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
	resp = do("GET", "/api/preview?url=http://example.com", "")
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

// Previews use the source network's proxy: proxyForNetwork resolves it from
// the stored config, and the per-proxy fetcher pool builds + reuses one
// fetcher per distinct proxy.
func TestProxyForNetwork(t *testing.T) {
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

	if p, ok := srv.proxyForNetwork(ctx, "tornet"); !ok || p == nil || p.Host != "127.0.0.1:9050" {
		t.Fatalf("tornet = %v,%v; want (127.0.0.1:9050, true)", p, ok)
	}
	// Known direct network: (nil, true) — a direct fetch is intended.
	if p, ok := srv.proxyForNetwork(ctx, "direct"); !ok || p != nil {
		t.Fatalf("direct = %v,%v; want (nil, true)", p, ok)
	}
	// Everything unresolvable must FAIL CLOSED: (nil, false).
	for _, name := range []string{"nonexistent", "", "malformed", "badproxy"} {
		if p, ok := srv.proxyForNetwork(ctx, name); ok || p != nil {
			t.Fatalf("proxyForNetwork(%q) = %v,%v; want (nil, false)", name, p, ok)
		}
	}
	p, _ := srv.proxyForNetwork(ctx, "tornet")
	f1 := srv.htmlFetcherFor(p)
	if !f1.proxied {
		t.Fatal("tornet fetcher not proxied")
	}
	if f2 := srv.htmlFetcherFor(p); f2 != f1 {
		t.Fatal("per-proxy fetcher not cached/reused")
	}
	if fd := srv.htmlFetcherFor(nil); fd.proxied {
		t.Fatal("direct fetcher must not be proxied")
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
	for _, path := range []string{
		"/api/preview?url=http://example.com&net=ghostnet",
		"/api/preview?url=http://example.com", // no net at all
		"/api/thumb?url=http://example.com/x.png&net=ghostnet",
	} {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.AddCookie(cookie)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("%s = %d, want 502 (fail closed)", path, resp.StatusCode)
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
func (f *fakeConn) Send(*ircv4.Message) error { return nil }

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
func (f *fakeConn) SetMonitored([]string)                {}
func (f *fakeConn) MonitorAdd(string)                    {}
func (f *fakeConn) MonitorRemove(string)                 {}

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
	// Logout's deletion cookie carries matching attributes.
	req, _ := http.NewRequest("POST", ts.URL+"/api/logout", nil)
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
	old := sessionRecheckInterval
	sessionRecheckInterval = 50 * time.Millisecond
	defer func() { sessionRecheckInterval = old }()

	ts, h := newTestServer(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := &fakeConn{ch: make(chan irc.Event, 4), name: "libera", nick: "AlteredParadox"}
	go h.Run(ctx, conn)

	header := http.Header{}
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	c, _, err := websocket.Dial(ctx, ts.URL+"/api/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// Invalidate the token (as logout does).
	req, _ := http.NewRequest("POST", ts.URL+"/api/logout", nil)
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
