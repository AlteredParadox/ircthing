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

package irc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// selfSignedServer starts a TLS listener with a fresh self-signed
// certificate and returns its address and the leaf DER SHA-256 (hex). The
// listener accepts one connection and completes the TLS handshake.
func selfSignedServer(t *testing.T) (addr, fingerprint string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "irc.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Drive the handshake so client-side verification actually runs.
		_ = conn.(*tls.Conn).Handshake()
		conn.Close()
	}()
	return ln.Addr().String(), hex.EncodeToString(sum[:])
}

func fpManager(t *testing.T, addr string, fingerprints []string) *Manager {
	t.Helper()
	m, err := NewManager(Config{
		Addr: addr, Nick: "AlteredParadox", TLS: true,
		TrustedFingerprints: fingerprints,
		DialTimeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDialTrustedFingerprint(t *testing.T) {
	addr, fp := selfSignedServer(t)
	// Uppercase with ':' separators exercises normalization.
	var pretty []string
	for i := 0; i < len(fp); i += 2 {
		pretty = append(pretty, strings.ToUpper(fp[i:i+2]))
	}
	m := fpManager(t, addr, []string{strings.Join(pretty, ":")})
	conn, err := m.dial(context.Background(), m.cfg.Addr, true)
	if err != nil {
		t.Fatalf("dial with matching fingerprint: %v", err)
	}
	conn.Close()
}

func TestDialUntrustedFingerprint(t *testing.T) {
	addr, _ := selfSignedServer(t)
	wrong := strings.Repeat("ab", 32)
	m := fpManager(t, addr, []string{wrong})
	if conn, err := m.dial(context.Background(), m.cfg.Addr, true); err == nil {
		conn.Close()
		t.Fatal("dial accepted a certificate with an untrusted fingerprint")
	} else if !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDialSelfSignedWithoutFingerprintFails(t *testing.T) {
	// No pins configured: standard CA verification must still apply.
	addr, _ := selfSignedServer(t)
	m := fpManager(t, addr, nil)
	if conn, err := m.dial(context.Background(), m.cfg.Addr, true); err == nil {
		conn.Close()
		t.Fatal("dial accepted a self-signed certificate without a trusted fingerprint")
	}
}

// clientCertProbeServer starts a TLS listener that requests a client
// certificate and reports, over got, how many the client actually sent.
func clientCertProbeServer(t *testing.T) (addr, fingerprint string, got chan int) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "irc.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		ClientAuth:   tls.RequestClientCert,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan int, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- -1
			return
		}
		tc := conn.(*tls.Conn)
		_ = tc.Handshake()
		got <- len(tc.ConnectionState().PeerCertificates)
		conn.Close()
	}()
	return ln.Addr().String(), hex.EncodeToString(sum[:]), got
}

// A self-signed client certificate to stand in for SASL EXTERNAL.
func selfSignedClientCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "AlteredParadox"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// When a client certificate (SASL EXTERNAL) is configured, a pin mismatch
// must abort the handshake before the certificate is transmitted, so a
// mismatched endpoint never learns our persistent identity.
func TestDialPinnedClientCertNotLeakedOnMismatch(t *testing.T) {
	addr, _, got := clientCertProbeServer(t)
	m, err := NewManager(Config{
		Addr: addr, Nick: "AlteredParadox", TLS: true,
		TrustedFingerprints: []string{strings.Repeat("ab", 32)}, // wrong pin
		DialTimeout:         5 * time.Second,
		TLSConfig:           &tls.Config{Certificates: []tls.Certificate{selfSignedClientCert(t)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := m.dial(context.Background(), m.cfg.Addr, true); err == nil {
		conn.Close()
		t.Fatal("dial accepted a mismatched pin")
	}
	if n := <-got; n != 0 {
		t.Fatalf("server received %d client certificate(s); pin mismatch must abort before the client certificate is sent", n)
	}
}

// Positive control: with a matching pin the client certificate is sent and
// the handshake completes.
func TestDialPinnedClientCertSentOnMatch(t *testing.T) {
	addr, fp, got := clientCertProbeServer(t)
	m, err := NewManager(Config{
		Addr: addr, Nick: "AlteredParadox", TLS: true,
		TrustedFingerprints: []string{fp},
		DialTimeout:         5 * time.Second,
		TLSConfig:           &tls.Config{Certificates: []tls.Certificate{selfSignedClientCert(t)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := m.dial(context.Background(), m.cfg.Addr, true)
	if err != nil {
		t.Fatalf("dial with matching pin + client cert: %v", err)
	}
	conn.Close()
	if n := <-got; n != 1 {
		t.Fatalf("server received %d client certificate(s), want 1", n)
	}
}

func TestFingerprintConfigValidation(t *testing.T) {
	if _, err := NewManager(Config{
		Addr: "irc.test:6697", Nick: "AlteredParadox", TLS: true,
		TrustedFingerprints: []string{"nothex"},
	}); err == nil {
		t.Fatal("malformed fingerprint accepted")
	}
}
