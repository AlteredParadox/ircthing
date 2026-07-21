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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// permitStream relaxes the direct stream fetcher's IP policy so tests can
// stream from loopback httptest origins (mirrors permit()).
func permitStream(s *Server) {
	s.streamFetcherFor(nil).allowIP = func(net.IP) bool { return true }
}

// mintMediaToken POSTs /api/media/token and returns the decoded response.
func mintMediaToken(t *testing.T, ts *httptest.Server, cookie *http.Cookie, target, netName string) (token string, exp int64, status int) {
	t.Helper()
	resp := mediaPost(t, ts, cookie, "/api/media/token", target, netName)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, resp.StatusCode
	}
	var body struct {
		Token string `json:"token"`
		Exp   int64  `json:"exp"`
	}
	decodeJSON(t, resp, &body)
	return body.Token, body.Exp, resp.StatusCode
}

// streamGet GETs /api/media/stream?t=token, optionally with a Range header.
func streamGet(t *testing.T, ts *httptest.Server, cookie *http.Cookie, token, rangeHdr string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/media/stream?t="+token, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// sealFor mints a token directly via the server for stream tests whose
// httptest origins live on loopback — the mint ENDPOINT (correctly) refuses
// literal non-public IPs, which is covered by TestMediaTokenEndpoint.
func sealFor(t *testing.T, srv *Server, target string) string {
	t.Helper()
	tok, err := srv.sealMediaToken(mediaToken{URL: target, Net: testNet, Exp: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestMediaTokenSealRoundtrip(t *testing.T) {
	_, srv := newTestServerWithRef(t)
	in := mediaToken{URL: "https://example.org/a.mp3", Net: testNet, Exp: time.Now().Add(time.Minute).Unix()}
	tok, err := srv.sealMediaToken(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := srv.openMediaToken(tok)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if out != in {
		t.Fatalf("roundtrip = %+v, want %+v", out, in)
	}
	// The sealed token must be opaque: the raw URL must not be recoverable
	// from the token text by simple base64 decoding (it rides in query
	// strings that reach reverse-proxy access logs).
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		t.Fatalf("token is not base64url: %v", err)
	}
	if strings.Contains(string(raw), "example.org") {
		t.Fatal("sealed token leaks the target URL in cleartext")
	}
}

func TestMediaTokenRejected(t *testing.T) {
	_, srv := newTestServerWithRef(t)
	valid, err := srv.sealMediaToken(mediaToken{URL: "https://example.org/a.mp3", Net: testNet, Exp: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}

	// Expired.
	expired, err := srv.sealMediaToken(mediaToken{URL: "https://example.org/a.mp3", Net: testNet, Exp: time.Now().Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.openMediaToken(expired); err == nil {
		t.Fatal("expired token accepted")
	}

	// Tampered: flip one ciphertext byte.
	raw, _ := base64.RawURLEncoding.DecodeString(valid)
	raw[len(raw)/2] ^= 0x01
	if _, err := srv.openMediaToken(base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered token accepted")
	}

	// Garbage.
	for _, tok := range []string{"", "not-a-token", "aGVsbG8"} {
		if _, err := srv.openMediaToken(tok); err == nil {
			t.Fatalf("garbage token %q accepted", tok)
		}
	}

	// A different server (different per-process key) must reject it.
	_, other := newTestServerWithRef(t)
	if _, err := other.openMediaToken(valid); err == nil {
		t.Fatal("token from another process's key accepted")
	}
}

func TestMediaTokenEndpoint(t *testing.T) {
	ts, _ := newTestServerWithRef(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	// Unauthenticated → 401.
	resp := mediaPost(t, ts, nil, "/api/media/token", "https://example.org/a.mp3", testNet)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d", resp.StatusCode)
	}

	token, exp, status := mintMediaToken(t, ts, cookie, "https://example.org/a.mp3", testNet)
	if status != http.StatusOK || token == "" {
		t.Fatalf("mint status = %d, token %q", status, token)
	}
	if until := exp - time.Now().Unix(); until < 60 || until > int64(mediaTokenTTL/time.Second) {
		t.Fatalf("exp %d s out is outside the expected TTL window", until)
	}

	// URL validation: non-http(s) schemes and literal non-public IPs → 400.
	for _, bad := range []string{
		"ftp://example.org/a.mp3",
		"file:///etc/passwd",
		"http://127.0.0.1/a.mp3",
		"http://169.254.169.254/a.mp3",
		"http://[::1]/a.mp3",
		"not a url",
	} {
		if _, _, status := mintMediaToken(t, ts, cookie, bad, testNet); status != http.StatusBadRequest {
			t.Fatalf("mint(%q) status = %d, want 400", bad, status)
		}
	}
}

func TestMediaStreamRequiresSessionAuth(t *testing.T) {
	ts, srv := newTestServerWithRef(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	token, _, status := mintMediaToken(t, ts, cookie, "https://example.org/a.mp3", testNet)
	if status != http.StatusOK {
		t.Fatalf("mint status = %d", status)
	}
	_ = srv

	// A valid token WITHOUT the session cookie must not stream: the token is
	// not a bearer capability on its own.
	resp := streamGet(t, ts, nil, token, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tokened-but-unauthed status = %d, want 401", resp.StatusCode)
	}
}

func TestMediaStreamBadTokens(t *testing.T) {
	ts, srv := newTestServerWithRef(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	expired, err := srv.sealMediaToken(mediaToken{URL: "https://example.org/a.mp3", Net: testNet, Exp: time.Now().Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{
		"garbage": "zzzz",
		"empty":   "",
		"expired": expired,
	} {
		resp := streamGet(t, ts, cookie, tok, "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s token status = %d, want 403", name, resp.StatusCode)
		}
	}
}

func TestMediaStreamRangePassthrough(t *testing.T) {
	payload := []byte("0123456789abcdef")
	var gotRange string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		if r.Header.Get("Cookie") != "" {
			t.Error("client cookie forwarded to origin")
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Accept-Ranges", "bytes")
		if gotRange == "bytes=4-7" {
			w.Header().Set("Content-Range", "bytes 4-7/16")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(payload[4:8])
			return
		}
		w.Write(payload)
	}))
	defer origin.Close()

	ts, srv := newTestServerWithRef(t)
	permitStream(srv)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	token := sealFor(t, srv, origin.URL+"/a.mp3")

	// Full-body 200.
	resp := streamGet(t, ts, cookie, token, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != string(payload) {
		t.Fatalf("full stream = %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}

	// Ranged 206: the Range request header must reach the origin, and the
	// 206 + Content-Range must come back through.
	resp = streamGet(t, ts, cookie, token, "bytes=4-7")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if gotRange != "bytes=4-7" {
		t.Fatalf("origin saw Range = %q", gotRange)
	}
	if resp.StatusCode != http.StatusPartialContent || string(body) != "4567" {
		t.Fatalf("ranged stream = %d %q", resp.StatusCode, body)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 4-7/16" {
		t.Fatalf("Content-Range = %q", cr)
	}
	if ar := resp.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Fatalf("Accept-Ranges = %q", ar)
	}
}

func TestMediaStreamContentTypeAllowlist(t *testing.T) {
	secret := "<html>internal dashboard</html>"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, secret)
	}))
	defer origin.Close()

	ts, srv := newTestServerWithRef(t)
	permitStream(srv)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	token := sealFor(t, srv, origin.URL+"/a.mp3")

	resp := streamGet(t, ts, cookie, token, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("html origin status = %d, want 502", resp.StatusCode)
	}
	if strings.Contains(string(body), "dashboard") {
		t.Fatal("origin body bytes leaked through a refused content type")
	}
}

func TestMediaStreamUnknownNetworkFailsClosed(t *testing.T) {
	ts, srv := newTestServerWithRef(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	// Seal directly for a network that does not exist: egress resolution must
	// fail closed (502), never fall back to a direct fetch.
	token, err := srv.sealMediaToken(mediaToken{URL: "https://example.org/a.mp3", Net: "no-such-net", Exp: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	resp := streamGet(t, ts, cookie, token, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unknown-network status = %d, want 502", resp.StatusCode)
	}
}

func TestMediaStreamDisabledWithPreviews(t *testing.T) {
	ts, srv := newTestServerWithRef(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	token, _, status := mintMediaToken(t, ts, cookie, "https://example.org/a.mp3", testNet)
	if status != http.StatusOK {
		t.Fatalf("mint status = %d", status)
	}

	if err := srv.applyPreviews(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	// Minting refuses while disabled…
	if _, _, status := mintMediaToken(t, ts, cookie, "https://example.org/a.mp3", testNet); status != http.StatusForbidden {
		t.Fatalf("mint-while-disabled status = %d, want 403", status)
	}
	// …and so does streaming with a token minted BEFORE the switch flipped.
	resp := streamGet(t, ts, cookie, token, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stream-while-disabled status = %d, want 403", resp.StatusCode)
	}
}

func TestMediaStreamConcurrencyCap(t *testing.T) {
	arrived := make(chan struct{}, streamSlots+1)
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "audio/mpeg")
		io.WriteString(w, "x")
	}))
	defer origin.Close()
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()

	ts, srv := newTestServerWithRef(t)
	permitStream(srv)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	token := sealFor(t, srv, origin.URL+"/a.mp3")

	// Occupy both stream slots (the origin holds each request open; a request
	// that reached the origin necessarily holds a slot).
	results := make(chan int, streamSlots)
	for i := 0; i < streamSlots; i++ {
		go func() {
			resp := streamGet(t, ts, cookie, token, "")
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			results <- resp.StatusCode
		}()
	}
	for i := 0; i < streamSlots; i++ {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("stream never reached the origin")
		}
	}

	// The third concurrent stream must 429 immediately — pinned behavior:
	// fail visibly, never queue.
	resp := streamGet(t, ts, cookie, token, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-cap status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}

	releaseOnce()
	for i := 0; i < streamSlots; i++ {
		if code := <-results; code != http.StatusOK {
			t.Fatalf("held stream finished with %d", code)
		}
	}
}

func TestMediaStreamIdleWatchdog(t *testing.T) {
	old := streamIdleTimeout
	streamIdleTimeout = 150 * time.Millisecond
	defer func() { streamIdleTimeout = old }()

	hung := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		io.WriteString(w, "intro")
		w.(http.Flusher).Flush()
		<-hung // origin stalls: no more bytes, connection held open
	}))
	defer origin.Close()
	defer close(hung)

	ts, srv := newTestServerWithRef(t)
	permitStream(srv)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	token := sealFor(t, srv, origin.URL+"/a.mp3")

	resp := streamGet(t, ts, cookie, token, "")
	defer resp.Body.Close()
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(resp.Body)
		done <- b
	}()
	select {
	case b := <-done:
		// The watchdog canceled the origin fetch: the stream ended (with the
		// bytes that made it through) instead of pinning a slot forever.
		if !strings.HasPrefix(string(b), "intro") {
			t.Fatalf("streamed prefix = %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hung origin pinned the stream past the idle watchdog")
	}
}

// streamIntro/streamOutro are sized WELL past net/http's response buffering
// (2 KiB): a tiny first chunk would sit in the server's bufio unflushed,
// streamGet would block on headers until the relay ended, and a "held open"
// stream test would silently degrade into a watchdog test. 8 KiB forces
// headers + bytes through to the client so the stream is genuinely in-flight
// before the test acts on it.
var (
	streamIntro = strings.Repeat("i", 8<<10)
	streamOutro = strings.Repeat("o", 8<<10)
)

// holdingOrigin serves an origin that sends streamIntro, flushes, then holds
// the connection open until `hold` closes — a stream that only session
// revocation (or the 60 s idle watchdog, far beyond these tests' deadlines)
// can end early. established is closed once the request has arrived, which
// also guarantees the stream registered its cancel func (registration
// precedes the origin fetch).
func holdingOrigin(established, hold chan struct{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		io.WriteString(w, streamIntro)
		w.(http.Flusher).Flush()
		close(established)
		<-hold
	}))
}

// startHeldStream opens a stream against a holding origin and returns a
// channel that closes when the client-side body read ends.
func startHeldStream(t *testing.T, ts *httptest.Server, cookie *http.Cookie, token string, established chan struct{}) <-chan struct{} {
	t.Helper()
	resp := streamGet(t, ts, cookie, token, "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
	}()
	select {
	case <-established:
	case <-time.After(10 * time.Second):
		t.Fatal("stream never reached the origin")
	}
	return done
}

// An in-flight stream must die with its session: logout deletes the token via
// deleteTokenLocked, which cancels registered streams alongside WebSockets.
func TestMediaStreamCanceledByLogout(t *testing.T) {
	established, hold := make(chan struct{}), make(chan struct{})
	origin := holdingOrigin(established, hold)
	defer origin.Close()
	defer close(hold)

	ts, srv := newTestServerWithRef(t)
	permitStream(srv)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	token := sealFor(t, srv, origin.URL+"/a.mp3")
	done := startHeldStream(t, ts, cookie, token, established)

	req, _ := http.NewRequest("POST", ts.URL+"/api/logout", nil)
	req.Header.Set("Origin", ts.URL)
	req.AddCookie(cookie)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d", resp.StatusCode)
	}

	select {
	case <-done:
		// Revocation tore the relay down; the origin is still holding, and
		// the idle watchdog (60 s) is far past this deadline, so only the
		// logout sweep can have ended it.
	case <-time.After(5 * time.Second):
		t.Fatal("stream survived logout")
	}
}

// Password rotation revokes EVERY session (the compromise-recovery action);
// its revoke loop runs through deleteTokenLocked too, so in-flight streams on
// any session must end with it.
func TestMediaStreamCanceledByPasswordRotation(t *testing.T) {
	established, hold := make(chan struct{}), make(chan struct{})
	origin := holdingOrigin(established, hold)
	defer origin.Close()
	defer close(hold)

	ts, srv := newTestServerWithRef(t)
	permitStream(srv)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	token := sealFor(t, srv, origin.URL+"/a.mp3")
	done := startHeldStream(t, ts, cookie, token, established)

	body := `{"current":"hunter2","new":"correct horse battery"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL)
	req.AddCookie(cookie)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("password change status = %d", resp.StatusCode)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream survived the password-rotation session revoke")
	}
}

// Pins an explicitly REJECTED behavior (M2 review decision, 2026-07-21):
// disabling previews governs NEW fetches only — token minting and fresh
// stream requests refuse (TestMediaStreamDisabledWithPreviews) — but it must
// NOT cancel an in-flight stream. Only session revocation or the idle
// watchdog ends a running stream early.
func TestMediaStreamSurvivesPreviewsToggle(t *testing.T) {
	established, proceed := make(chan struct{}), make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		io.WriteString(w, streamIntro)
		w.(http.Flusher).Flush()
		close(established)
		<-proceed
		io.WriteString(w, streamOutro)
	}))
	defer origin.Close()

	ts, srv := newTestServerWithRef(t)
	permitStream(srv)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	token := sealFor(t, srv, origin.URL+"/a.mp3")

	resp := streamGet(t, ts, cookie, token, "")
	defer resp.Body.Close()
	bodyDone := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(resp.Body)
		bodyDone <- b
	}()
	select {
	case <-established:
	case <-time.After(10 * time.Second):
		t.Fatal("stream never reached the origin")
	}

	if err := srv.applyPreviews(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	close(proceed) // origin finishes the track AFTER the toggle landed

	select {
	case b := <-bodyDone:
		if string(b) != streamIntro+streamOutro {
			t.Fatalf("streamed body = %d bytes, want the full %d-byte track (toggle must not cancel in-flight streams)", len(b), len(streamIntro)+len(streamOutro))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not complete after the previews toggle")
	}
}

// A stream that ends normally must unregister its cancel func — the registry
// must not leak entries across streams.
func TestMediaStreamRegistryCleanup(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		io.WriteString(w, "x")
	}))
	defer origin.Close()

	ts, srv := newTestServerWithRef(t)
	permitStream(srv)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	token := sealFor(t, srv, origin.URL+"/a.mp3")

	resp := streamGet(t, ts, cookie, token, "")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// The handler's deferred unregister may still be running just after the
	// client sees the body end; poll briefly rather than racing it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		srv.mu.Lock()
		n := len(srv.streamCancels)
		srv.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("streamCancels holds %d entries after the stream ended", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The stream transport must never solicit compression: with DisableCompression
// unset, Go's transport adds Accept-Encoding: gzip on the no-Range path and
// transparently decompresses, letting relayed bytes diverge from the origin's
// Content-Length.
func TestMediaStreamNoTransparentGzip(t *testing.T) {
	var acceptEnc string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptEnc = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "audio/mpeg")
		io.WriteString(w, "x")
	}))
	defer origin.Close()

	ts, srv := newTestServerWithRef(t)
	permitStream(srv)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	token := sealFor(t, srv, origin.URL+"/a.mp3")

	resp := streamGet(t, ts, cookie, token, "")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if strings.Contains(acceptEnc, "gzip") {
		t.Fatalf("stream request solicited gzip (Accept-Encoding = %q)", acceptEnc)
	}
	if acceptEnc != "identity" {
		t.Fatalf("stream request Accept-Encoding = %q, want identity", acceptEnc)
	}
}

// An origin that ignores Accept-Encoding: identity and sends an encoded body
// anyway must get a 502 with no body relayed: writeStreamHeaders forwards no
// Content-Encoding, so relaying would hand the media element mislabeled gzip
// bytes.
func TestMediaStreamRejectsEncodedResponse(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Encoding", "gzip")
		io.WriteString(w, "\x1f\x8b not really gzip")
	}))
	defer origin.Close()

	ts, srv := newTestServerWithRef(t)
	permitStream(srv)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	token := sealFor(t, srv, origin.URL+"/a.mp3")

	resp := streamGet(t, ts, cookie, token, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("encoded response relayed: status %d, want 502", resp.StatusCode)
	}
	if strings.Contains(string(body), "not really gzip") {
		t.Fatalf("encoded body bytes leaked through the 502 path")
	}
}
