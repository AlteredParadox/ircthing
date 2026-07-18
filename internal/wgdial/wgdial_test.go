package wgdial

import "testing"

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
