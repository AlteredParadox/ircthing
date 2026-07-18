package api

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// sameOrigin (the WebSocket handshake guard) must reject a cross-HOST request
// and an http Origin when we KNOW we're https, but must NOT lock out a
// reverse-proxy WSS deployment when our own scheme is indeterminate (TLS
// terminated at the proxy, r.TLS nil). That last case is the Caddy-in-front
// regression that 403'd every WebSocket.
func TestSameOrigin(t *testing.T) {
	req := func(host string, isTLS bool, xfp string) *http.Request {
		r := httptest.NewRequest("GET", "http://"+host+"/api/ws", nil)
		if isTLS {
			r.TLS = &tls.ConnectionState{}
		}
		if xfp != "" {
			r.Header.Set("X-Forwarded-Proto", xfp)
		}
		return r
	}
	cases := []struct {
		name         string
		trustProxy   bool
		host, origin string
		isTLS        bool
		xfp          string
		want         bool
	}{
		{"direct tls, https origin", false, "h.example", "https://h.example", true, "", true},
		{"direct tls, http origin refused", false, "h.example", "http://h.example", true, "", false},
		{"proxy + xfp=https, https origin", true, "h.example", "https://h.example", false, "https", true},
		{"proxy + xfp=https, http origin refused", true, "h.example", "http://h.example", false, "https", false},
		{"proxy, no xfp -> scheme unknown, https accepted", true, "h.example", "https://h.example", false, "", true},
		{"caddy in front, behind_proxy off -> https accepted (the regression)", false, "h.example", "https://h.example", false, "", true},
		// Documented residual: when the scheme is indeterminate, a same-host http
		// Origin is also accepted (host-only). Enable behind_proxy + X-Forwarded-
		// Proto to restore strict scheme checking.
		{"indeterminate scheme -> same-host http also accepted", false, "h.example", "http://h.example", false, "", true},
		{"cross host refused", false, "h.example", "https://evil.example", true, "", false},
	}
	for _, tc := range cases {
		s := &Server{cfg: Config{TrustProxyForwarded: tc.trustProxy}}
		r := req(tc.host, tc.isTLS, tc.xfp)
		if got := s.sameOrigin(tc.origin, r); got != tc.want {
			t.Errorf("%s: sameOrigin = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// forwardedClientIP must key on the LAST X-Forwarded-For hop (the one a trusted
// single-hop proxy appends) and must NOT trust X-Real-IP, which the recommended
// proxy (Caddy) forwards client-set by default.
func TestForwardedClientIP(t *testing.T) {
	cases := []struct {
		name       string
		xff, xreal string
		want       string
	}{
		{"last XFF hop", "1.1.1.1, 2.2.2.2, 3.3.3.3", "", "3.3.3.3"},
		{"single XFF", "9.9.9.9", "", "9.9.9.9"},
		{"X-Real-IP alone is ignored", "", "6.6.6.6", ""},
		{"X-Real-IP ignored when XFF present", "3.3.3.3", "6.6.6.6", "3.3.3.3"},
		{"non-IP last entry rejected", "1.1.1.1, notanip", "", ""},
		{"ipv6 last hop", "1.1.1.1, ::1", "", "::1"},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		r, _ := http.NewRequest("GET", "/", nil)
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if tc.xreal != "" {
			r.Header.Set("X-Real-IP", tc.xreal)
		}
		if got := forwardedClientIP(r); got != tc.want {
			t.Errorf("%s: forwardedClientIP = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// issueToken must refuse a token whose caller's credential-generation snapshot
// is stale — the case where a password rotation lands between a login's verify
// and its token issuance.
func TestIssueTokenRefusesAfterRotation(t *testing.T) {
	_, srv := newTestServerWithRef(t)
	gen := srv.credGen.Load()
	srv.credGen.Add(1) // a rotation lands after the login snapshot
	if _, err := srv.issueToken(gen); !errors.Is(err, errCredRotated) {
		t.Fatalf("issueToken(staleGen) err = %v, want errCredRotated", err)
	}
	if _, err := srv.issueToken(srv.credGen.Load()); err != nil {
		t.Fatalf("issueToken(currentGen) err = %v, want nil", err)
	}
}

// loadPasswordHash must fail CLOSED on a corrupt stored override rather than
// silently reverting to the config seed (which rotation leaves untouched, so a
// fallback would resurrect the pre-rotation password). A valid override wins.
func TestLoadPasswordHashFailsClosed(t *testing.T) {
	_, srv := newTestServerWithRef(t)
	st := srv.hub.Store()
	ctx := context.Background()
	seed, _ := bcrypt.GenerateFromPassword([]byte("seedpassword"), bcrypt.MinCost)
	cfg := Config{PasswordHash: string(seed)}

	if err := st.SetSetting(ctx, passwordHashKey, "not-a-bcrypt-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPasswordHash(ctx, st, cfg); err == nil {
		t.Fatal("corrupt override: loadPasswordHash returned no error (would fall back to the seed)")
	}

	override, _ := bcrypt.GenerateFromPassword([]byte("override1"), bcrypt.MinCost)
	if err := st.SetSetting(ctx, passwordHashKey, string(override)); err != nil {
		t.Fatal(err)
	}
	h, err := loadPasswordHash(ctx, st, cfg)
	if err != nil || h != string(override) {
		t.Fatalf("valid override: got (%q, %v), want the stored override", h, err)
	}
}
