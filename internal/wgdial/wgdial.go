// Package wgdial is an in-process WireGuard egress dialer (SPIKE, phase-4
// candidate — measurement only, not wired into main). It brings up a userspace
// WireGuard tunnel via wireguard-go + gVisor netstack (no TUN device, no root,
// no host routing changes) and dials TCP targets THROUGH the tunnel, resolving
// their DNS via the tunnel-internal resolver so no name lookup leaks to the
// local resolver — the same no-local-resolution rule the SOCKS5 dialer follows
// (proxy-side DNS).
package wgdial

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// Config is one network's WireGuard egress. Keys are standard WireGuard
// base64 (as `wg` / Mullvad print them); Endpoint/Address/DNS are the tunnel
// peer, our address inside the tunnel, and the in-tunnel resolver.
type Config struct {
	PrivateKey    string // base64
	PeerPublicKey string // base64
	PresharedKey  string // base64, optional
	Endpoint      string // host:port of the WireGuard peer (dialed locally over UDP)
	Address       string // our address inside the tunnel, e.g. "10.64.0.2"
	DNS           string // in-tunnel DNS resolver, e.g. "10.64.0.1"
	MTU           int    // 0 -> 1420
}

// Tunnel is a live userspace WireGuard device. Build once per network and
// reuse across reconnects; DialContext is safe for concurrent use.
type Tunnel struct {
	dev  *device.Device
	tnet *netstack.Net
}

// New brings up the tunnel: allocates a netstack interface with our in-tunnel
// address and DNS, starts the WireGuard device, and configures the peer.
func New(cfg Config) (*Tunnel, error) {
	addr, err := netip.ParseAddr(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("wgdial: address %q: %w", cfg.Address, err)
	}
	dns, err := netip.ParseAddr(cfg.DNS)
	if err != nil {
		return nil, fmt.Errorf("wgdial: dns %q: %w", cfg.DNS, err)
	}
	mtu := cfg.MTU
	if mtu == 0 {
		mtu = 1420
	}
	tun, tnet, err := netstack.CreateNetTUN([]netip.Addr{addr}, []netip.Addr{dns}, mtu)
	if err != nil {
		return nil, fmt.Errorf("wgdial: create netstack tun: %w", err)
	}
	dev := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "wg "))

	uapi, err := uapiConfig(cfg)
	if err != nil {
		dev.Close()
		return nil, err
	}
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wgdial: configure device: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wgdial: bring up device: %w", err)
	}
	return &Tunnel{dev: dev, tnet: tnet}, nil
}

// Validate checks a Config statically — keys decode to 32-byte WireGuard keys,
// the tunnel address and in-tunnel DNS parse as IPs, and the endpoint is
// host:port — WITHOUT bringing up a device (no goroutines, no UDP socket). The
// manager calls it during config validation, which runs on a throwaway manager,
// so it must have no side effects.
func Validate(cfg Config) error {
	if _, err := netip.ParseAddr(cfg.Address); err != nil {
		return fmt.Errorf("wgdial: address %q: %w", cfg.Address, err)
	}
	if _, err := netip.ParseAddr(cfg.DNS); err != nil {
		return fmt.Errorf("wgdial: dns %q: %w", cfg.DNS, err)
	}
	if _, _, err := net.SplitHostPort(cfg.Endpoint); err != nil {
		return fmt.Errorf("wgdial: endpoint %q: not host:port: %w", cfg.Endpoint, err)
	}
	_, err := uapiConfig(cfg) // validates the base64 keys
	return err
}

// DialContext dials a TCP target through the tunnel. A hostname is resolved by
// the netstack resolver against the in-tunnel DNS server, so no lookup reaches
// the local resolver (matching the SOCKS5 dialer's proxy-side-DNS rule).
func (t *Tunnel) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	return t.tnet.DialContext(ctx, "tcp", addr)
}

// Close tears down the tunnel.
func (t *Tunnel) Close() {
	if t.dev != nil {
		t.dev.Close()
	}
}

// uapiConfig renders the wireguard-go IPC (UAPI) config. Keys are converted
// from base64 to the hex the UAPI expects. allowed_ip=0.0.0.0/0,::/0 routes
// all target traffic through the peer.
func uapiConfig(cfg Config) (string, error) {
	priv, err := keyHex(cfg.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("wgdial: private key: %w", err)
	}
	pub, err := keyHex(cfg.PeerPublicKey)
	if err != nil {
		return "", fmt.Errorf("wgdial: peer public key: %w", err)
	}
	s := "private_key=" + priv + "\n" +
		"public_key=" + pub + "\n" +
		"endpoint=" + cfg.Endpoint + "\n" +
		"allowed_ip=0.0.0.0/0\n" +
		"allowed_ip=::/0\n" +
		"persistent_keepalive_interval=25\n"
	if cfg.PresharedKey != "" {
		psk, err := keyHex(cfg.PresharedKey)
		if err != nil {
			return "", fmt.Errorf("wgdial: preshared key: %w", err)
		}
		s += "preshared_key=" + psk + "\n"
	}
	return s, nil
}

// keyHex decodes a base64 WireGuard key (32 bytes) to lowercase hex.
func keyHex(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("key is %d bytes, want 32", len(raw))
	}
	return hex.EncodeToString(raw), nil
}
