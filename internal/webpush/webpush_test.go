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

package webpush

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("bad base64url fixture: %v", err)
	}
	return b
}

// TestEncryptRFC8291Vector drives the full encryption path with the fixed
// keys and salt of RFC 8291 §5 and requires the byte-exact request body
// the spec publishes.
func TestEncryptRFC8291Vector(t *testing.T) {
	plaintext := []byte("When I grow up, I want to be a watermelon")
	asPrivate := b64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")
	asPublic := b64(t, "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	uaPublic := b64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	auth := b64(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := b64(t, "DGv6ra1nlYgDCS1FRnbzlw")
	want := b64(t, "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27ml"+
		"mlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPT"+
		"pK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN")

	priv, err := ecdh.P256().NewPrivateKey(asPrivate)
	if err != nil {
		t.Fatalf("as_private: %v", err)
	}
	if !bytes.Equal(priv.PublicKey().Bytes(), asPublic) {
		t.Fatal("fixture mismatch: as_private does not yield as_public")
	}
	got, err := encrypt(plaintext, uaPublic, auth, priv, salt)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ciphertext mismatch\n got %x\nwant %x", got, want)
	}
}

func TestEncryptRejectsBadInputs(t *testing.T) {
	priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := PublicKeyBytes(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, authLen)
	if _, err := Encrypt(make([]byte, maxPlaintext+1), pub, auth); err == nil {
		t.Error("oversized plaintext accepted")
	}
	if _, err := Encrypt([]byte("hi"), pub, auth[:8]); err == nil {
		t.Error("short auth secret accepted")
	}
	if _, err := Encrypt([]byte("hi"), pub[:33], auth); err == nil {
		t.Error("compressed/truncated p256dh accepted")
	}
}

func TestVapidAuth(t *testing.T) {
	priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	const origin = "https://push.example.net"
	const subject = "https://github.com/AlteredParadox/ircthing"
	header, err := vapidAuth(priv, origin, subject, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(header, "vapid t=") {
		t.Fatalf("header = %q, want vapid scheme", header)
	}
	rest := strings.TrimPrefix(header, "vapid t=")
	jwt, k, ok := strings.Cut(rest, ", k=")
	if !ok {
		t.Fatalf("no k parameter in %q", header)
	}
	wantK, err := PublicKeyB64(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if k != wantK {
		t.Errorf("k = %q, want %q", k, wantK)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d segments", len(parts))
	}
	var hdr struct{ Typ, Alg string }
	if err := json.Unmarshal(b64(t, parts[0]), &hdr); err != nil {
		t.Fatalf("JWT header: %v", err)
	}
	if hdr.Typ != "JWT" || hdr.Alg != "ES256" {
		t.Errorf("JWT header = %+v", hdr)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(b64(t, parts[1]), &claims); err != nil {
		t.Fatalf("JWT claims: %v", err)
	}
	if claims.Aud != origin || claims.Sub != subject {
		t.Errorf("claims = %+v", claims)
	}
	// RFC 8292 §2: exp at most 24h out. Ours is 12h.
	wantExp := now.Add(vapidTokenTTL).Unix()
	if claims.Exp != wantExp {
		t.Errorf("exp = %d, want %d", claims.Exp, wantExp)
	}
	// The signature must be raw R ‖ S (RFC 7518 §3.4), not ASN.1.
	sig := b64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want 64", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&priv.PublicKey, digest[:], r, s) {
		t.Error("signature does not verify")
	}
}

func TestMarshalParseKeyRoundTrip(t *testing.T) {
	priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := MarshalKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseKey(s)
	if err != nil {
		t.Fatal(err)
	}
	if !priv.Equal(back) {
		t.Error("round-tripped key differs")
	}
	if _, err := ParseKey("not base64!"); err == nil {
		t.Error("garbage accepted")
	}
}

func TestValidateEndpoint(t *testing.T) {
	cases := []struct {
		url string
		ok  bool
	}{
		{"https://fcm.googleapis.com/fcm/send/abc123", true},
		{"https://web.push.apple.com/QOsabc", true},
		{"https://updates.push.services.mozilla.com:443/wpush/v2/x", true},
		{"http://fcm.googleapis.com/fcm/send/abc", false}, // not https
		{"https://127.0.0.1/push", false},                 // loopback literal
		{"https://10.0.0.5/push", false},                  // private literal
		{"https://[fe80::1]/push", false},                 // link-local literal
		{"https://169.254.169.254/latest/meta-data", false},
		{"https://example.com:99999/push", false}, // bad port
		{"https://user:pw@example.com/push", false},
		{"https:///push", false}, // no host
		{"", false},
	}
	for _, tc := range cases {
		err := ValidateEndpoint(tc.url)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateEndpoint(%q) = %v, want ok=%v", tc.url, err, tc.ok)
		}
	}
}

func TestSenderHeadersAndStatus(t *testing.T) {
	priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	status := http.StatusCreated
	var got http.Header
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	defer srv.Close()

	s := NewSender(priv, "https://github.com/AlteredParadox/ircthing")
	s.insecure = true
	sub := Subscription{Endpoint: srv.URL, P256dh: make([]byte, keyLen), Auth: make([]byte, authLen)}

	if err := s.Send(context.Background(), sub, []byte("payload"), 3600); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ttl := got.Get("TTL"); ttl != "3600" {
		t.Errorf("TTL = %q", ttl)
	}
	if u := got.Get("Urgency"); u != "high" {
		t.Errorf("Urgency = %q", u)
	}
	if ce := got.Get("Content-Encoding"); ce != "aes128gcm" {
		t.Errorf("Content-Encoding = %q", ce)
	}
	if a := got.Get("Authorization"); !strings.HasPrefix(a, "vapid t=") {
		t.Errorf("Authorization = %q", a)
	}
	if string(gotBody) != "payload" {
		t.Errorf("body = %q", gotBody)
	}

	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		status = code
		if err := s.Send(context.Background(), sub, nil, 60); !errors.Is(err, ErrGone) {
			t.Errorf("HTTP %d: err = %v, want ErrGone", code, err)
		}
	}
	status = http.StatusInternalServerError
	if err := s.Send(context.Background(), sub, nil, 60); err == nil || errors.Is(err, ErrGone) {
		t.Errorf("HTTP 500: err = %v, want transient error", err)
	}
}

func TestSenderRefusesLoopbackByDefault(t *testing.T) {
	priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("sender reached loopback")
	}))
	defer srv.Close()
	s := NewSender(priv, "")
	sub := Subscription{Endpoint: srv.URL, P256dh: make([]byte, keyLen), Auth: make([]byte, authLen)}
	if err := s.Send(context.Background(), sub, nil, 60); err == nil {
		t.Fatal("Send to http loopback succeeded")
	}
}
