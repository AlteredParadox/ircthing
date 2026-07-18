package wgdial

import (
	"context"
	"testing"
)

// A valid base64-encoded 32-byte WireGuard key (all zero bytes). Enough to
// exercise decode/length checks without standing up a device.
const zeroKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func goodConfig() Config {
	return Config{
		PrivateKey:    zeroKey,
		PeerPublicKey: zeroKey,
		Endpoint:      "203.0.113.7:51820",
		Address:       "10.64.0.2",
		DNS:           "10.64.0.1",
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(goodConfig()); err != nil {
		t.Fatalf("good config rejected: %v", err)
	}

	bad := []struct {
		name string
		mut  func(c *Config)
	}{
		{"empty private key", func(c *Config) { c.PrivateKey = "" }},
		{"non-base64 key", func(c *Config) { c.PrivateKey = "not!base64!" }},
		{"short key", func(c *Config) { c.PeerPublicKey = "AAAA" }}, // decodes to 3 bytes
		{"bad address", func(c *Config) { c.Address = "not-an-ip" }},
		{"bad dns", func(c *Config) { c.DNS = "1.2.3" }},
		{"endpoint without port", func(c *Config) { c.Endpoint = "203.0.113.7" }},
		{"empty endpoint", func(c *Config) { c.Endpoint = "" }},
		{"endpoint non-numeric port", func(c *Config) { c.Endpoint = "203.0.113.7:https" }},
		{"endpoint port out of range", func(c *Config) { c.Endpoint = "203.0.113.7:99999" }},
		{"endpoint newline-injected port", func(c *Config) { c.Endpoint = "203.0.113.7:51820\npublic_key=x" }},
		{"endpoint empty host", func(c *Config) { c.Endpoint = ":51820" }},
		{"mtu negative", func(c *Config) { c.MTU = -1 }},
		{"mtu absurdly large", func(c *Config) { c.MTU = 3000000000 }},
		{"mtu too small", func(c *Config) { c.MTU = 500 }},
	}
	for _, tc := range bad {
		c := goodConfig()
		tc.mut(&c)
		if err := Validate(c); err == nil {
			t.Errorf("%s: Validate accepted an invalid config", tc.name)
		}
	}
}

func TestKeyHex(t *testing.T) {
	h, err := keyHex(zeroKey)
	if err != nil {
		t.Fatalf("keyHex(zeroKey): %v", err)
	}
	want := "0000000000000000000000000000000000000000000000000000000000000000"
	if h != want {
		t.Fatalf("keyHex(zeroKey) = %q, want %q", h, want)
	}
	if _, err := keyHex("AAAA"); err == nil { // 3 bytes, not 32
		t.Error("keyHex accepted a non-32-byte key")
	}
	if _, err := keyHex("%%%"); err == nil {
		t.Error("keyHex accepted non-base64")
	}
}

func TestParseDNS(t *testing.T) {
	// bare IP -> port 53
	ip, port, ap, err := parseDNS("10.64.0.1")
	if err != nil || port != 53 || ap != "10.64.0.1:53" || ip.String() != "10.64.0.1" {
		t.Fatalf("parseDNS(bare) = %v,%d,%q,%v", ip, port, ap, err)
	}
	// ip:port -> honored
	_, port, ap, err = parseDNS("10.64.0.1:5353")
	if err != nil || port != 5353 || ap != "10.64.0.1:5353" {
		t.Fatalf("parseDNS(port) = %d,%q,%v", port, ap, err)
	}
	// IPv6 bare and with port
	if _, port, _, err := parseDNS("fd00::1"); err != nil || port != 53 {
		t.Fatalf("parseDNS(v6 bare) = %d,%v", port, err)
	}
	if _, port, _, err := parseDNS("[fd00::1]:5353"); err != nil || port != 5353 {
		t.Fatalf("parseDNS(v6 port) = %d,%v", port, err)
	}
	for _, bad := range []string{"not-an-ip", "10.0.0.1:0", "10.0.0.1:99999", "10.0.0.1:abc", ""} {
		if _, _, _, err := parseDNS(bad); err == nil {
			t.Errorf("parseDNS(%q) accepted an invalid value", bad)
		}
	}
}

func TestResolveEndpoint(t *testing.T) {
	ctx := context.Background()
	// A literal IP passes through unchanged (no lookup).
	if got, err := resolveEndpoint(ctx, "203.0.113.7:51820"); err != nil || got != "203.0.113.7:51820" {
		t.Fatalf("resolveEndpoint(literal) = %q, %v", got, err)
	}
	if got, err := resolveEndpoint(ctx, "[2001:db8::1]:51820"); err != nil || got != "[2001:db8::1]:51820" {
		t.Fatalf("resolveEndpoint(literal v6) = %q, %v", got, err)
	}
	// A hostname is resolved via the local resolver; localhost is in /etc/hosts.
	// v4 is preferred, so expect the loopback v4 with the port preserved.
	if got, err := resolveEndpoint(ctx, "localhost:51820"); err != nil || got != "127.0.0.1:51820" {
		t.Fatalf("resolveEndpoint(localhost) = %q, %v; want 127.0.0.1:51820", got, err)
	}
	// A 4-in-6 literal is unmapped so it agrees with endpointIsV4 / the v4 bind.
	if got, err := resolveEndpoint(ctx, "[::ffff:203.0.113.7]:51820"); err != nil || got != "203.0.113.7:51820" {
		t.Fatalf("resolveEndpoint(4-in-6) = %q, %v; want 203.0.113.7:51820", got, err)
	}
	// A non-numeric / newline-injected port is rejected (no UAPI injection).
	for _, bad := range []string{"203.0.113.7:https", "203.0.113.7:99999", "203.0.113.7:51820\npublic_key=x"} {
		if _, err := resolveEndpoint(ctx, bad); err == nil {
			t.Errorf("resolveEndpoint(%q) accepted a bad port", bad)
		}
	}
	// Missing port is rejected.
	if _, err := resolveEndpoint(ctx, "203.0.113.7"); err == nil {
		t.Fatal("resolveEndpoint accepted an endpoint without a port")
	}
}

// The optional preshared key must be validated when present and ignored when
// absent (its absence is not an error).
func TestValidatePresharedKey(t *testing.T) {
	c := goodConfig()
	c.PresharedKey = "bogus"
	if err := Validate(c); err == nil {
		t.Error("Validate accepted a malformed preshared key")
	}
	c.PresharedKey = zeroKey
	if err := Validate(c); err != nil {
		t.Errorf("Validate rejected a valid preshared key: %v", err)
	}
}
