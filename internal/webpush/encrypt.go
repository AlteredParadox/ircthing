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
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// Message encryption per RFC 8291 (one HKDF chain combining the ECDH
// shared secret with the subscription's auth secret, then the aes128gcm
// content coding of RFC 8188). The whole message must fit ONE record: a
// push service accepts at most 4096 octets of content-coded payload
// (RFC 8030 §7.2), so multi-record support is dead weight we skip.

const (
	// saltLen and authLen per RFC 8291 §2/§3.2; keyLen is the uncompressed
	// P-256 point both sides exchange.
	saltLen = 16
	authLen = 16
	keyLen  = 65

	// MaxPlaintext keeps header (86) + plaintext + pad delimiter (1) +
	// GCM tag (16) within the 4096-octet push-service ceiling. Exported
	// so callers can bound payloads BEFORE encrypting per subscription.
	MaxPlaintext = 4096 - 86 - 1 - 16
)

// ErrBadKeys marks failures caused by the subscription's OWN keys (a
// malformed or off-curve p256dh, a short auth secret). These are the
// only Encrypt errors that justify deleting a subscription — anything
// else (an oversized payload above all) is the CALLER's problem, and
// pruning on it would let one bad payload wipe every registration.
var ErrBadKeys = errors.New("webpush: bad subscription keys")

// Encrypt seals plaintext for a subscription identified by its p256dh
// public key (65-octet uncompressed point) and 16-octet auth secret,
// using a fresh ephemeral key and salt per message as RFC 8291 §3.1
// requires. The result is the complete aes128gcm request body.
func Encrypt(plaintext, p256dh, auth []byte) ([]byte, error) {
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encrypt(plaintext, p256dh, auth, asPriv, salt)
}

// encrypt is Encrypt with the ephemeral key and salt injected so the
// RFC 8291 §5 fixed-value test vector can drive the full path.
func encrypt(plaintext, p256dh, auth []byte, asPriv *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > MaxPlaintext {
		return nil, fmt.Errorf("webpush: plaintext %d bytes exceeds single-record limit %d", len(plaintext), MaxPlaintext)
	}
	if len(auth) != authLen {
		return nil, fmt.Errorf("%w: auth secret must be %d bytes, got %d", ErrBadKeys, authLen, len(auth))
	}
	uaPub, err := ecdh.P256().NewPublicKey(p256dh)
	if err != nil {
		return nil, fmt.Errorf("%w: bad p256dh point: %v", ErrBadKeys, err)
	}
	ecdhSecret, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPub := asPriv.PublicKey().Bytes()

	// RFC 8291 §3.3-§3.4: two HKDF-SHA-256 stages. First combine the ECDH
	// secret with the auth secret (salt=auth, info binds both public keys);
	// then derive the content key and nonce from that IKM (salt=salt).
	keyInfo := append(append([]byte("WebPush: info\x00"), p256dh...), asPub...)
	prkKey, err := hkdf.Extract(sha256.New, ecdhSecret, auth)
	if err != nil {
		return nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}

	// RFC 8188 §2.1 header: salt ‖ rs (uint32) ‖ idlen ‖ keyid, with the
	// application-server public key as keyid (RFC 8291 §4). rs stays at
	// 4096 like the spec example — irrelevant for a single record.
	body := make([]byte, 0, saltLen+4+1+keyLen+len(plaintext)+1+16)
	body = append(body, salt...)
	body = binary.BigEndian.AppendUint32(body, 4096)
	body = append(body, keyLen)
	body = append(body, asPub...)

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// Last (only) record: plaintext ‖ 0x02 pad delimiter (RFC 8188 §2,
	// RFC 8291 §4 — "a push message MUST include exactly one record").
	padded := append(append(make([]byte, 0, len(plaintext)+1), plaintext...), 0x02)
	return gcm.Seal(body, nonce, padded, nil), nil
}
