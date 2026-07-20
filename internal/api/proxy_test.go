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
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeConnectProxy runs a minimal HTTP CONNECT proxy that tunnels to the
// requested target, so a proxied fetcher can be exercised end to end. It
// returns the proxy URL.
func fakeConnectProxy(t *testing.T) *url.URL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				parts := strings.Fields(line)
				if len(parts) < 2 || parts[0] != "CONNECT" {
					return
				}
				for { // drain headers to the blank line
					h, err := br.ReadString('\n')
					if err != nil || strings.TrimSpace(h) == "" {
						break
					}
				}
				up, err := net.Dial("tcp", parts[1])
				if err != nil {
					io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				go io.Copy(up, br) // client -> target (br may hold read-ahead)
				io.Copy(c, up)     // target -> client
			}()
		}
	}()
	return &url.URL{Scheme: "http", Host: ln.Addr().String()}
}

// A fetcher configured with a proxy tunnels its fetch through it rather
// than connecting to the target directly (the anti-IP-leak guarantee).
func TestFetcherUsesProxy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("through the proxy"))
	}))
	defer target.Close()

	f := newFetcher(maxHTMLBytes, fakeConnectProxy(t))
	f.allowIP = func(net.IP) bool { return true } // permit the loopback target
	_, _, body, err := f.get(context.Background(), target.URL)
	if err != nil {
		t.Fatalf("proxied fetch: %v", err)
	}
	if string(body) != "through the proxy" {
		t.Fatalf("body = %q", body)
	}
}

// The proxied path still refuses a literal non-public IP target up front —
// the Control hook can't see the proxy-resolved address, so hostAllowed is
// the SSRF backstop.
func TestProxiedFetcherBlocksLiteralPrivateIP(t *testing.T) {
	f := newFetcher(maxHTMLBytes, fakeConnectProxy(t)) // default allowIP = isPublicIP
	for _, u := range []string{"http://10.0.0.1/x", "http://169.254.169.254/latest", "http://[::1]/"} {
		if _, _, _, err := f.get(context.Background(), u); !errors.Is(err, errBlocked) {
			t.Fatalf("get(%q) = %v, want errBlocked", u, err)
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip     string
		public bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"93.184.216.34", true}, // example.com
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"10.0.0.5", false},
		{"172.16.3.4", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // cloud metadata
		{"fe80::1", false},         // link-local
		{"fc00::1", false},         // unique-local
		{"0.0.0.0", false},
		{"::", false},
		{"224.0.0.1", false},   // multicast
		{"100.64.0.1", false},  // CGNAT
		{"100.127.0.1", false}, // CGNAT upper
		{"100.128.0.1", true},  // just outside CGNAT
		{"::ffff:127.0.0.1", false},
		{"::ffff:10.0.0.1", false},
		// IANA special-purpose blocks beyond the stdlib classifications.
		{"0.1.2.3", false},        // 0.0.0.0/8 "this network"
		{"192.0.0.8", false},      // protocol assignments
		{"192.0.2.10", false},     // TEST-NET-1
		{"198.51.100.7", false},   // TEST-NET-2
		{"203.0.113.99", false},   // TEST-NET-3
		{"198.18.0.1", false},     // benchmarking
		{"198.19.255.255", false}, // benchmarking upper
		{"192.88.99.1", false},    // 6to4 relay anycast (deprecated)
		{"240.0.0.1", false},      // reserved
		{"255.255.255.255", false},
		{"100::1", false},             // discard-only
		{"2001:db8::1", false},        // documentation
		{"2001::42", false},           // TEREDO / protocol assignments
		{"2002:808:808::1", false},    // 6to4
		{"3fff::1", false},            // documentation (RFC 9637)
		{"64:ff9b:1::1", false},       // local-use translation
		{"64:ff9b::a9fe:a9fe", false}, // NAT64-embedded 169.254.169.254
		{"64:ff9b::7f00:1", false},    // NAT64-embedded 127.0.0.1
		{"::808:808", false},          // IPv4-compatible-embedded 8.8.8.8 (deprecated form)
		{"::a9fe:a9fe", false},        // IPv4-compatible-embedded 169.254.169.254
		{"::ffff:0:808:808", false},   // IPv4-translated (SIIT)
		{"64:ff9b::808:808", false},   // NAT64-embedded 8.8.8.8: the whole prefix is out
		{"2620:fe::fe", true},         // Quad9 — ordinary global unicast
		// Non-global IPv6 the stdlib helpers do not classify: the
		// 2000::/3 allowlist backstop must reject these.
		{"fec0::1", false}, // deprecated site-local (RFC 3879)
		{"febf::1", false}, // top of fec0::/10
		{"5000::1", false}, // outside global unicast 2000::/3
		{"1000::1", false}, // below global unicast
		{"3000::1", true},  // still within 2000::/3 (2000::–3fff::)
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", tc.ip)
		}
		if got := isPublicIP(ip); got != tc.public {
			t.Errorf("isPublicIP(%s) = %v, want %v", tc.ip, got, tc.public)
		}
	}
}

func TestFetcherBlocksLoopbackByDefault(t *testing.T) {
	// httptest listens on 127.0.0.1; the real policy must refuse to dial
	// it — this proves the Control hook is wired, not just the predicate.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret internal data"))
	}))
	defer srv.Close()

	f := newFetcher(maxHTMLBytes, nil)
	_, _, _, err := f.get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("fetcher connected to loopback")
	}
	if !errors.Is(err, errBlocked) && !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want a block", err)
	}
}

// A redirect must not carry Go's auto-generated Referer, which would leak
// the full (possibly signed) preview URL into the target's logs.
func TestCheckRedirectStripsReferer(t *testing.T) {
	f := newFetcher(maxHTMLBytes, nil)
	req, err := http.NewRequest("GET", "https://example.com/next", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Referer", "https://host.internal/api/link?url=https%3A%2F%2Fsecret%2Fpage")
	if err := f.checkRedirect(req, []*http.Request{{}}); err != nil {
		t.Fatalf("checkRedirect: %v", err)
	}
	if got := req.Header.Get("Referer"); got != "" {
		t.Fatalf("Referer not stripped: %q", got)
	}
}

// permissiveFetcher allows loopback so the fetch/parse paths can be
// tested against httptest servers.
func permissiveFetcher(t *testing.T, maxBytes int64) *fetcher {
	t.Helper()
	f := newFetcher(maxBytes, nil)
	f.allowIP = func(net.IP) bool { return true }
	return f
}

func TestFetcherRejects(t *testing.T) {
	f := permissiveFetcher(t, 1024)

	t.Run("non-http scheme", func(t *testing.T) {
		if _, _, _, err := f.get(context.Background(), "file:///etc/passwd"); !errors.Is(err, errBadURL) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("relative url", func(t *testing.T) {
		if _, _, _, err := f.get(context.Background(), "/just/a/path"); !errors.Is(err, errBadURL) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("hostless and bad-port authorities", func(t *testing.T) {
		// http://:80/ has a non-empty Host (":80") but an EMPTY Hostname(): it
		// must be refused before any dial, along with out-of-range ports. A dial
		// counter proves none reach the dialer.
		for _, u := range []string{"http://:80/", "http://:0/", "https://:65536/", "http://host:0/", "http://host:99999/"} {
			f.allowIP = func(net.IP) bool {
				t.Errorf("get(%q) reached the dialer", u)
				return false
			}
			if _, _, _, err := f.get(context.Background(), u); !errors.Is(err, errBadURL) {
				t.Fatalf("get(%q) = %v, want errBadURL", u, err)
			}
		}
		f.allowIP = func(net.IP) bool { return true }
	})
	t.Run("size cap", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(make([]byte, 4096))
		}))
		defer srv.Close()
		if _, _, _, err := f.get(context.Background(), srv.URL); !errors.Is(err, errTooLarge) {
			t.Fatalf("err = %v, want too large", err)
		}
	})
	t.Run("upstream error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusForbidden)
		}))
		defer srv.Close()
		if _, _, _, err := f.get(context.Background(), srv.URL); err == nil {
			t.Fatal("expected error on 403")
		}
	})
}

// TestFetcherRequestsIdentityEncoding: the fetcher must not negotiate transparent
// gzip. With auto-gzip, the LimitReader caps DECOMPRESSED bytes — a hostile origin
// could stream unbounded wire bytes that decompress to almost nothing, burning
// bandwidth/CPU until the client timeout. Identity keeps the cap a wire-byte cap.
func TestFetcherRequestsIdentityEncoding(t *testing.T) {
	var acceptEncoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptEncoding = r.Header.Get("Accept-Encoding")
		w.Write([]byte("plain"))
	}))
	defer srv.Close()

	f := permissiveFetcher(t, 1024)
	_, _, body, err := f.get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if strings.Contains(acceptEncoding, "gzip") {
		t.Fatalf("fetcher negotiated gzip (Accept-Encoding: %q); the byte cap would measure decompressed bytes", acceptEncoding)
	}
	if string(body) != "plain" {
		t.Fatalf("body = %q", body)
	}
}

// TestWireBudget: the raw-byte budget must stop a connection that streams more
// transport bytes than the fetch could need (TLS-padding amplification — the
// plaintext LimitReader never sees those bytes), and the resulting error must
// classify as PERMANENT so the client doesn't re-download the flood.
func TestWireBudget(t *testing.T) {
	t.Run("conn stops at budget", func(t *testing.T) {
		src, sink := net.Pipe()
		defer src.Close()
		go func() { // endless writer: the "hostile origin"
			buf := make([]byte, 4096)
			for {
				if _, err := src.Write(buf); err != nil {
					return
				}
			}
		}()
		budget := new(atomic.Int64)
		budget.Store(10_000)
		bc := &budgetConn{Conn: sink, remaining: budget}
		n, err := io.Copy(io.Discard, bc)
		sink.Close()
		if !errors.Is(err, errWireBudget) {
			t.Fatalf("err = %v, want errWireBudget", err)
		}
		if n != 10_000 {
			t.Fatalf("read %d raw bytes, want exactly the 10000 budget", n)
		}
	})
	t.Run("budget exhaustion is permanent", func(t *testing.T) {
		wrapped := &net.OpError{Op: "read", Net: "tcp", Err: errWireBudget}
		if fetchErrorRetryable(wrapped) {
			t.Fatal("wire-budget exhaustion classified retryable; the client would re-download the flood")
		}
		if fetchErrorRetryable(&url.Error{Op: "Get", URL: "https://x", Err: wrapped}) {
			t.Fatal("wire-budget exhaustion not found through the url.Error chain")
		}
	})
	t.Run("normal fetch unaffected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("well under budget"))
		}))
		defer srv.Close()
		f := permissiveFetcher(t, 1024)
		_, _, body, err := f.get(context.Background(), srv.URL)
		if err != nil || string(body) != "well under budget" {
			t.Fatalf("get = %q, %v", body, err)
		}
	})
	t.Run("budget spans redirect hops", func(t *testing.T) {
		// Each redirect hop dials a fresh connection (DisableKeepAlives); the
		// budget must be REQUEST-wide, or five hops each get a fresh allowance
		// (~52 MiB of TLS padding for one nominal 10 MiB thumbnail). Two conns
		// drawing on ONE counter must drain it jointly: 600 bytes on the first
		// leaves only 400 for the second.
		budget := new(atomic.Int64)
		budget.Store(1000)
		a, b := net.Pipe()
		c, d := net.Pipe()
		defer a.Close()
		defer c.Close()
		go func() { a.Write(make([]byte, 600)); a.Close() }()
		bc1 := &budgetConn{Conn: b, remaining: budget}
		if _, err := io.ReadAll(bc1); err != nil {
			t.Fatalf("hop 1 read: %v", err)
		}
		go func() { c.Write(make([]byte, 600)) }()
		bc2 := &budgetConn{Conn: d, remaining: budget}
		if _, err := io.Copy(io.Discard, bc2); !errors.Is(err, errWireBudget) {
			t.Fatalf("hop 2 err = %v, want errWireBudget (shared budget must carry over)", err)
		}
	})
}

// TestBodyReadErrorClassification: a failure while reading the body defaults
// PERMANENT (deterministic framing breaks — e.g. a malformed chunk terminator
// after ~10 MiB — must not get the client's 4-download retry budget), while
// genuine network-level failures stay transient.
func TestBodyReadErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool // retryable
	}{
		{"malformed chunked framing", &bodyReadError{errors.New("malformed chunked encoding")}, false},
		{"bad trailer", &bodyReadError{errors.New("http: invalid trailer")}, false},
		{"socket reset mid-body", &bodyReadError{&net.OpError{Op: "read", Err: errors.New("connection reset by peer")}}, true},
		{"deadline mid-body", &bodyReadError{context.DeadlineExceeded}, true},
		{"bare connection cut", &bodyReadError{io.ErrUnexpectedEOF}, true},
		{"wire budget through body phase", &bodyReadError{&net.OpError{Op: "read", Err: errWireBudget}}, false},
		// Do-phase parser failures (malformed status/headers) — deterministic,
		// and reachable only after the endpoint burned wire budget: permanent.
		{"malformed status line", &url.Error{Op: "Get", URL: "https://x", Err: errors.New(`malformed HTTP response "x"`)}, false},
		{"malformed header", &url.Error{Op: "Get", URL: "https://x", Err: errors.New("malformed MIME header line")}, false},
		// Dial failures are the canonical transient class — a WireGuard tunnel
		// warming up returns CUSTOM error types (not net.Error), so the dial
		// tag must carry them through the url.Error chain.
		{"tunnel dial failure", &url.Error{Op: "Get", URL: "https://x", Err: &dialPhaseError{errors.New("wireguard tunnel not up")}}, true},
		{"early clean close", &url.Error{Op: "Get", URL: "https://x", Err: io.EOF}, true},
	}
	for _, tc := range cases {
		if got := fetchErrorRetryable(tc.err); got != tc.want {
			t.Errorf("%s: retryable = %v, want %v", tc.name, got, tc.want)
		}
	}
	// End to end: a server that breaks chunked framing mid-body must surface a
	// permanent (non-retryable) error through get().
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")
		buf.WriteString("5\r\nhello\r\nZZZ\r\n") // ZZZ: malformed chunk size
		buf.Flush()
	}))
	defer srv.Close()
	f := permissiveFetcher(t, 1024)
	_, _, _, err := f.get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("malformed chunking succeeded")
	}
	if fetchErrorRetryable(err) {
		t.Fatalf("malformed chunked body classified retryable: %v", err)
	}
}

// TestFetchErrorRetryable: only transient failures may be reported to the client
// as retryable (503). Permanent ones (bad/blocked URL, over-size body, upstream
// 4xx) must be non-retryable so the browser caches the failure instead of
// hammering four requests — a dead link is four tracking hits, an over-size image
// is ~40 MiB re-downloaded, to the same end.
func TestFetchErrorRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"too large", errTooLarge, false},
		{"bad url", errBadURL, false},
		{"blocked", errBlocked, false},
		{"wrapped too large", fmt.Errorf("get: %w", errTooLarge), false},
		{"upstream 403", &upstreamStatusError{403}, false},
		{"upstream 404", &upstreamStatusError{404}, false},
		{"upstream 410", &upstreamStatusError{410}, false},
		{"upstream 429", &upstreamStatusError{429}, true},
		{"upstream 500", &upstreamStatusError{500}, true},
		{"upstream 503", &upstreamStatusError{503}, true},
		// Untyped errors now default PERMANENT; real dial failures arrive as
		// dialPhaseError (the withWireBudget tag) or as a net.OpError.
		{"tagged dial failure", &dialPhaseError{errors.New("dial tcp: i/o timeout")}, true},
		{"socket op error", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"untyped unknown error", errors.New("mystery failure"), false},
		// Redirect misbehavior is a deterministic property of the target: a
		// retry of a five-hop loop re-walks every hop, so it must be permanent.
		// client.Do wraps CheckRedirect errors in *url.Error, so test the
		// wrapped form too.
		{"redirect loop", errRedirectLoop, false},
		{"wrapped redirect loop", &url.Error{Op: "Get", URL: "http://x", Err: errRedirectLoop}, false},
		{"redirect scheme", errRedirectScheme, false},
		{"wrapped redirect scheme", &url.Error{Op: "Get", URL: "http://x", Err: errRedirectScheme}, false},
		// A certificate verification failure won't heal between retries.
		{"cert verification", &url.Error{Op: "Get", URL: "https://x",
			Err: &tls.CertificateVerificationError{Err: errors.New("x509: bad")}}, false},
		// A generic TLS I/O failure (handshake cut short) stays transient.
		// A real TLS-handshake I/O failure surfaces as io.EOF (clean close),
		// io.ErrUnexpectedEOF, or a socket net.OpError — all transient. An
		// untyped string in the Do phase is the permanent default.
		{"tls handshake cut (EOF)", &url.Error{Op: "Get", URL: "https://x", Err: io.EOF}, true},
		{"tls handshake socket error", &url.Error{Op: "Get", URL: "https://x", Err: &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}}, true},
		// A malformed Location header is rejected inside net/http before
		// CheckRedirect runs and surfaces as an untyped error in *url.Error
		// (matched by message text) — deterministic, so permanent.
		{"malformed location", &url.Error{Op: "Get", URL: "http://x",
			Err: fmt.Errorf("failed to parse Location header %q: %v", "http://[bad", errors.New("parse error"))}, false},
	}
	for _, tc := range cases {
		if got := fetchErrorRetryable(tc.err); got != tc.want {
			t.Errorf("%s: fetchErrorRetryable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestFetcherTruncatesHTML: an HTML fetcher (truncate=true) must keep the
// head-bearing prefix of an over-cap page rather than fail closed — real
// preview pages (Next.js/GitBook) routinely exceed the 512 KiB HTML cap while
// the og/title tags sit in the first tens of KiB. An image fetcher (default)
// still rejects an over-cap body as a corrupt image.
func TestFetcherTruncatesHTML(t *testing.T) {
	// Body: og:title in the head, then padding well past the cap.
	head := `<html><head><meta property="og:title" content="Kept"></head><body>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(head))
		w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	t.Run("html truncates", func(t *testing.T) {
		f := permissiveFetcher(t, 1024)
		f.truncate = true
		_, _, body, err := f.get(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if int64(len(body)) != f.maxBytes {
			t.Fatalf("body len = %d, want cap %d", len(body), f.maxBytes)
		}
		var pv PreviewData
		extractMeta(string(body), srv.URL, &pv)
		if pv.Title != "Kept" {
			t.Fatalf("title = %q, want Kept", pv.Title)
		}
	})
	t.Run("image still rejects", func(t *testing.T) {
		f := permissiveFetcher(t, 1024) // truncate defaults false
		if _, _, _, err := f.get(context.Background(), srv.URL); !errors.Is(err, errTooLarge) {
			t.Fatalf("err = %v, want too large", err)
		}
	})
}

func TestFetcherRevalidatesEveryHop(t *testing.T) {
	// A public host redirecting to an internal one is the classic SSRF
	// bypass. The dialer's Control hook must run per hop, so the target
	// dial is re-checked even after the first hop was allowed. Both
	// httptest servers are 127.0.0.1, so we can't distinguish by IP;
	// instead we allow the first dial and block the second, which is
	// exactly the per-hop guarantee under test.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("internal-only data"))
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	f := newFetcher(maxHTMLBytes, nil)
	var dials int
	f.allowIP = func(net.IP) bool {
		dials++
		return dials == 1 // allow the redirector, block the target
	}
	_, _, body, err := f.get(context.Background(), redirector.URL)
	if err == nil {
		t.Fatalf("second hop was not revalidated; got body %q", body)
	}
	if dials < 2 {
		t.Fatalf("redirect target was never dialed (dials=%d)", dials)
	}
}

// get returns the FINAL URL after redirects, so the caller resolves relative
// og:image against the page they actually landed on (finding 10).
func TestFetcherReturnsFinalURL(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer final.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/landed", http.StatusFound)
	}))
	defer redirector.Close()

	f := newFetcher(maxHTMLBytes, nil)
	f.allowIP = func(net.IP) bool { return true } // allow both loopback hops
	_, finalURL, _, err := f.get(context.Background(), redirector.URL)
	if err != nil {
		t.Fatal(err)
	}
	if want := final.URL + "/landed"; finalURL != want {
		t.Fatalf("finalURL = %q, want %q (post-redirect)", finalURL, want)
	}
}
