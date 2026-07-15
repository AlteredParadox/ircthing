package api

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// permit rewires a Server's proxy fetchers to allow loopback so tests can
// use httptest origins.
func permit(s *Server) {
	s.htmlFetcher.allowIP = func(net.IP) bool { return true }
	s.imageFetcher.allowIP = func(net.IP) bool { return true }
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
	resp, err := http.Get(ts.URL + "/api/preview?url=" + url.QueryEscape(origin.URL))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/preview?url="+url.QueryEscape(origin.URL), nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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

	req, _ := http.NewRequest("GET", ts.URL+"/api/thumb?url="+url.QueryEscape(origin.URL), nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
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

	req, _ := http.NewRequest("GET", ts.URL+"/api/thumb?url="+url.QueryEscape(origin.URL), nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
}

func TestProxyRejectsBadURL(t *testing.T) {
	ts, _ := newTestServerWithRef(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))
	for _, path := range []string{"/api/preview", "/api/preview?url=", "/api/thumb?url=notaurl"} {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatalf("%s returned 200 for bad input", path)
		}
	}
}
