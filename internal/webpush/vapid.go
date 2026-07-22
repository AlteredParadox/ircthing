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

// Package webpush implements the application-server side of Web Push:
// message encryption (RFC 8291), voluntary server identification (VAPID,
// RFC 8292), and delivery to a push service (RFC 8030). Specs fetched
// 2026-07-21. Protocol code only — no store or hub knowledge lives here.
package webpush

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// GenerateKey creates the server's long-lived VAPID signing key. It is a
// P-256 key (RFC 8292 §2: ES256 is the only permitted algorithm) that
// subscriptions bind to via applicationServerKey, so it must be generated
// once and persisted — rotating it silently orphans every subscription.
func GenerateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// MarshalKey serializes the VAPID key for the settings table: PKCS#8 DER,
// base64 (std) encoded.
func MarshalKey(priv *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// ParseKey is the inverse of MarshalKey.
func ParseKey(s string) (*ecdsa.PrivateKey, error) {
	der, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("vapid key: %w", err)
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("vapid key: %w", err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, fmt.Errorf("vapid key: not a P-256 ECDSA key")
	}
	return ec, nil
}

// PublicKeyBytes returns the 65-octet uncompressed point form of the
// public key (0x04 ‖ X ‖ Y) — the encoding RFC 8292 §3.2 requires for the
// "k" parameter and the browser expects for applicationServerKey.
func PublicKeyBytes(pub *ecdsa.PublicKey) ([]byte, error) {
	e, err := pub.ECDH()
	if err != nil {
		return nil, err
	}
	return e.Bytes(), nil
}

// PublicKeyB64 is PublicKeyBytes as unpadded base64url — the wire form
// for both the VAPID "k" parameter and GET /api/config's push_public_key.
func PublicKeyB64(pub *ecdsa.PublicKey) (string, error) {
	b, err := PublicKeyBytes(pub)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// vapidTokenTTL is how long issued JWTs are valid. RFC 8292 §2 caps exp at
// 24 hours from the request; 12 hours keeps a wide margin for clock skew
// on either side while still amortizing the ECDSA sign across many pushes.
const vapidTokenTTL = 12 * time.Hour

// vapidAuth builds the Authorization header value for one push-service
// origin (RFC 8292 §3: `vapid t=<jwt>, k=<pubkey>`). aud is the ORIGIN of
// the push resource, not the full URL (§2); sub is a contact URI (§2.1).
func vapidAuth(priv *ecdsa.PrivateKey, origin, subject string, now time.Time) (string, error) {
	claims, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub,omitempty"`
	}{Aud: origin, Exp: now.Add(vapidTokenTTL).Unix(), Sub: subject})
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding
	// Header is constant: ES256 is the only algorithm VAPID admits.
	signing := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) +
		"." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	// JWS ES256 (RFC 7518 §3.4) signatures are the raw 64-octet R ‖ S
	// concatenation, each value left-padded to 32 octets — NOT the ASN.1
	// DER form ecdsa.SignASN1 produces.
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	pub, err := PublicKeyB64(&priv.PublicKey)
	if err != nil {
		return "", err
	}
	return "vapid t=" + signing + "." + b64.EncodeToString(sig) + ", k=" + pub, nil
}
