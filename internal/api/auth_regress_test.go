package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

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
