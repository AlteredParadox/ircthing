package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

	f := newFetcher(maxHTMLBytes)
	_, _, err := f.get(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("fetcher connected to loopback")
	}
	if !errors.Is(err, errBlocked) && !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want a block", err)
	}
}

// permissiveFetcher allows loopback so the fetch/parse paths can be
// tested against httptest servers.
func permissiveFetcher(t *testing.T, maxBytes int64) *fetcher {
	t.Helper()
	f := newFetcher(maxBytes)
	f.allowIP = func(net.IP) bool { return true }
	return f
}

func TestFetcherRejects(t *testing.T) {
	f := permissiveFetcher(t, 1024)

	t.Run("non-http scheme", func(t *testing.T) {
		if _, _, err := f.get(context.Background(), "file:///etc/passwd"); !errors.Is(err, errBadURL) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("relative url", func(t *testing.T) {
		if _, _, err := f.get(context.Background(), "/just/a/path"); !errors.Is(err, errBadURL) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("size cap", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(make([]byte, 4096))
		}))
		defer srv.Close()
		if _, _, err := f.get(context.Background(), srv.URL); !errors.Is(err, errTooLarge) {
			t.Fatalf("err = %v, want too large", err)
		}
	})
	t.Run("upstream error status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusForbidden)
		}))
		defer srv.Close()
		if _, _, err := f.get(context.Background(), srv.URL); err == nil {
			t.Fatal("expected error on 403")
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

	f := newFetcher(maxHTMLBytes)
	var dials int
	f.allowIP = func(net.IP) bool {
		dials++
		return dials == 1 // allow the redirector, block the target
	}
	_, body, err := f.get(context.Background(), redirector.URL)
	if err == nil {
		t.Fatalf("second hop was not revalidated; got body %q", body)
	}
	if dials < 2 {
		t.Fatalf("redirect target was never dialed (dials=%d)", dials)
	}
}
