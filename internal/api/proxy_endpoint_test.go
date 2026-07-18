package api

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// permit rewires a Server's direct (no-proxy) fetchers to allow loopback so
// tests can use httptest origins. Preview/thumb requests without a `net`
// param resolve to the direct fetcher, which this builds and relaxes.
func permit(s *Server) {
	s.htmlFetcherFor(nil).allowIP = func(net.IP) bool { return true }
	s.imageFetcherFor(nil).allowIP = func(net.IP) bool { return true }
}

func TestPreviewEndpoint(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<head>
			<meta property="og:title" content="Hello Preview">
			<meta property="og:description" content="from the origin">
		</head>`))
	}))
	defer origin.Close()

	ts, srvObj := newTestServerWithRef(t)
	permit(srvObj)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	// Unauthenticated is rejected.
	resp := mediaPost(t, ts, nil, "/api/preview", origin.URL, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d", resp.StatusCode)
	}

	resp = mediaPost(t, ts, cookie, "/api/preview", origin.URL, testNet)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var pv PreviewData
	decodeJSON(t, resp, &pv)
	if pv.Kind != "link" || pv.Title != "Hello Preview" || pv.Description != "from the origin" {
		t.Fatalf("preview = %+v", pv)
	}
}

func TestThumbEndpoint(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 800, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 800; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 200, 255})
		}
	}
	var raw bytes.Buffer
	png.Encode(&raw, img)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(raw.Bytes())
	}))
	defer origin.Close()

	ts, srvObj := newTestServerWithRef(t)
	permit(srvObj)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	resp := mediaPost(t, ts, cookie, "/api/thumb", origin.URL, testNet)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q", ct)
	}
	cfg, _, err := image.DecodeConfig(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != thumbMaxDim || cfg.Height != thumbMaxDim {
		t.Fatalf("thumb dims = %dx%d, want %d square", cfg.Width, cfg.Height, thumbMaxDim)
	}
}

func TestThumbRejectsNonImage(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>not an image</html>"))
	}))
	defer origin.Close()

	ts, srvObj := newTestServerWithRef(t)
	permit(srvObj)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	resp := mediaPost(t, ts, cookie, "/api/thumb", origin.URL, testNet)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
}

// A PNG declaring huge dimensions is rejected at the header check, not
// decoded. 100000^2 is accepted by Go's DecodeConfig (its own overflow
// guard only trips much higher) but far exceeds the decode budget; the
// dimension cap rejects it before the area math.
func TestThumbRejectsHugeDimensions(t *testing.T) {
	// A valid 1x1 RGBA PNG, then patch its IHDR to claim 100000 x 100000 and
	// fix the IHDR CRC.
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	binary.BigEndian.PutUint32(b[16:], 100000) // IHDR width
	binary.BigEndian.PutUint32(b[20:], 100000) // IHDR height
	binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(b)
	}))
	defer origin.Close()

	ts, srvObj := newTestServerWithRef(t)
	permit(srvObj)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	resp := mediaPost(t, ts, cookie, "/api/thumb", origin.URL, testNet)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 (rejected before decode)", resp.StatusCode)
	}
}

func TestProxyRejectsBadURL(t *testing.T) {
	ts, _ := newTestServerWithRef(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	cases := []struct{ path, target string }{
		{"/api/preview", ""},        // empty url -> 400
		{"/api/preview", "notaurl"}, // unfetchable -> 502
		{"/api/thumb", ""},
		{"/api/thumb", "notaurl"},
	}
	for _, tc := range cases {
		resp := mediaPost(t, ts, cookie, tc.path, tc.target, "")
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatalf("%s url=%q returned 200 for bad input", tc.path, tc.target)
		}
	}
}
