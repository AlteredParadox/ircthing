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
	conn, err := m.dial(context.Background())
	if err != nil {
		t.Fatalf("dial with matching fingerprint: %v", err)
	}
	conn.Close()
}

func TestDialUntrustedFingerprint(t *testing.T) {
	addr, _ := selfSignedServer(t)
	wrong := strings.Repeat("ab", 32)
	m := fpManager(t, addr, []string{wrong})
	if conn, err := m.dial(context.Background()); err == nil {
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
	if conn, err := m.dial(context.Background()); err == nil {
		conn.Close()
		t.Fatal("dial accepted a self-signed certificate without a trusted fingerprint")
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
