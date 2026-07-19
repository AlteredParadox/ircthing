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

// Package wgdial is an in-process WireGuard egress dialer. It brings up a
// userspace WireGuard tunnel via wireguard-go + gVisor netstack (no TUN device,
// no root, no host routing changes) and dials TCP targets THROUGH the tunnel,
// resolving their DNS via the tunnel-internal resolver so no name lookup leaks
// to the local resolver — the same no-local-resolution rule the SOCKS5 dialer
// follows (proxy-side DNS).
//
// The ONE intentional exception to that rule is the WireGuard peer endpoint
// itself: it is the tunnel's UDP entry point, dialed over plain UDP before the
// tunnel exists, so a hostname endpoint cannot be resolved through the tunnel
// (chicken-and-egg) and is resolved via the local resolver. See resolveEndpoint.
package wgdial

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"

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
	Endpoint      string // host:port of the WireGuard peer (hostname or IP; dialed locally over UDP)
	Address       string // our address inside the tunnel, e.g. "10.64.0.2"
	DNS           string // in-tunnel resolver: ip or ip:port (default :53), e.g. "10.64.0.1" / "10.64.0.1:5353"
	MTU           int    // 0 -> 1420
}

// Tunnel is a live userspace WireGuard device. Build once per network and
// reuse across reconnects; DialContext is safe for concurrent use.
type Tunnel struct {
	dev  *device.Device
	tnet *netstack.Net

	cfg        Config
	peerPubHex string // for Reresolve's endpoint update

	// dnsPort is the in-tunnel DNS port. When 53 we let netstack's built-in
	// resolver handle target lookups (the validated path); otherwise resolver
	// dials dnsAddrPort through the tunnel, since netstack's resolver is
	// hardwired to :53.
	dnsPort     int
	dnsAddrPort string
	resolver    *net.Resolver
}

// New brings up the tunnel: allocates a netstack interface with our in-tunnel
// address and DNS, resolves the peer endpoint (locally, pre-tunnel), starts the
// WireGuard device, and configures the peer. ctx bounds the endpoint lookup.
func New(ctx context.Context, cfg Config) (*Tunnel, error) {
	addr, err := netip.ParseAddr(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("wgdial: address %q: %w", cfg.Address, err)
	}
	dnsIP, dnsPort, dnsAddrPort, err := parseDNS(cfg.DNS)
	if err != nil {
		return nil, err
	}
	// Resolve the endpoint BEFORE allocating the netstack device: the lookup can
	// fail (hostname endpoint + DNS flap), and CreateNetTUN spins up a gVisor
	// stack + goroutines that only device.Close releases — resolving first avoids
	// leaking one per failed (re)connect attempt.
	resolvedEndpoint, err := resolveEndpoint(ctx, cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	// Clamp defensively: Validate rejects an out-of-range MTU, but a config
	// predating the validator must never reach netstack with a value it would
	// int32-truncate into a panic (see Validate). 0 or anything out of the sane
	// range falls back to the 1420 default.
	mtu := cfg.MTU
	if mtu < 1280 || mtu > 1500 {
		mtu = 1420
	}
	tun, tnet, err := netstack.CreateNetTUN([]netip.Addr{addr}, []netip.Addr{dnsIP}, mtu)
	if err != nil {
		return nil, fmt.Errorf("wgdial: create netstack tun: %w", err)
	}

	dev := device.NewDevice(tun, chooseBind(resolvedEndpoint), device.NewLogger(device.LogLevelError, "wg "))
	uapi, err := uapiConfig(cfg, resolvedEndpoint)
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

	peerPubHex, _ := keyHex(cfg.PeerPublicKey) // already validated by uapiConfig
	t := &Tunnel{
		dev: dev, tnet: tnet, cfg: cfg, peerPubHex: peerPubHex,
		dnsPort: dnsPort, dnsAddrPort: dnsAddrPort,
	}
	// A resolver that always queries the in-tunnel DNS server (at its configured
	// host:port) THROUGH the tunnel — the OS resolv.conf server passed in is
	// ignored. Used only for the non-53 DNS-port path; the lookup never touches
	// the local resolver.
	t.resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return t.tnet.DialContext(ctx, network, t.dnsAddrPort)
		},
	}
	return t, nil
}

// Validate checks a Config statically — keys decode to 32-byte WireGuard keys,
// the tunnel address parses, DNS is ip[:port], and the endpoint is host:port —
// WITHOUT bringing up a device (no goroutines, no UDP socket, no name lookup).
// The manager calls it during config validation on a throwaway manager, so it
// must have no side effects.
func Validate(cfg Config) error {
	addr, err := netip.ParseAddr(cfg.Address)
	if err != nil {
		return fmt.Errorf("wgdial: address %q: %w", cfg.Address, err)
	}
	dnsIP, _, _, err := parseDNS(cfg.DNS)
	if err != nil {
		return err
	}
	// The netstack interface has the single local address above; DNS
	// traffic of the other family would be unroutable and every in-tunnel
	// lookup would fail — fail-closed but invisible. Reject at config time.
	if addr.Unmap().Is4() != dnsIP.Unmap().Is4() {
		return fmt.Errorf("wgdial: address %q and dns %q must be the same IP family", cfg.Address, cfg.DNS)
	}
	host, portStr, err := net.SplitHostPort(cfg.Endpoint)
	if err != nil {
		return fmt.Errorf("wgdial: endpoint %q: not host:port: %w", cfg.Endpoint, err)
	}
	if host == "" {
		return fmt.Errorf("wgdial: endpoint %q: host is required", cfg.Endpoint)
	}
	if _, err := endpointPort(portStr); err != nil {
		return fmt.Errorf(errEndpointFmt, cfg.Endpoint, err)
	}
	// Bound the MTU. 0 means the 1420 default; otherwise require a sane
	// WireGuard range. An unbounded value reaches netstack, which int32-truncates
	// it in device setup — a negative or >MaxInt32 MTU then yields a negative
	// padding length and panics an encryption worker (crashing the whole process)
	// on the first packet. New clamps too, as belt-and-suspenders.
	if cfg.MTU != 0 && (cfg.MTU < 1280 || cfg.MTU > 1500) {
		return fmt.Errorf("wgdial: mtu %d out of range (1280–1500, or 0 for the 1420 default)", cfg.MTU)
	}
	if _, err = uapiConfig(cfg, cfg.Endpoint); err != nil { // validates the base64 keys
		return err
	}
	// An all-zero key is never a real key — it is the classic placeholder
	// mistake — yet it passes every syntactic check and then fails the
	// Noise handshake forever, visible only as a reconnect loop. Reject it
	// at config time. (An all-zero preshared key is protocol-equivalent to
	// omitting it, so it is not checked.)
	if isZeroKey(cfg.PrivateKey) {
		return fmt.Errorf("wgdial: private_key is all zeros (placeholder, not a key)")
	}
	if isZeroKey(cfg.PeerPublicKey) {
		return fmt.Errorf("wgdial: peer_public_key is all zeros (placeholder, not a key)")
	}
	return nil
}

// isZeroKey reports whether a base64 key (already validated to decode to 32
// bytes) is all zero bytes.
func isZeroKey(b64 string) bool {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return false
	}
	for _, b := range raw {
		if b != 0 {
			return false
		}
	}
	return true
}

// DialContext dials a TCP target through the tunnel. With the default DNS port
// (53) a hostname is resolved by netstack's built-in resolver against the
// in-tunnel DNS server (the validated path). With a non-53 in-tunnel DNS port
// we resolve the hostname ourselves against that server (through the tunnel)
// and dial the resulting IP. Either way no lookup reaches the local resolver.
func (t *Tunnel) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	if t.dnsPort == 53 {
		return t.tnet.DialContext(ctx, "tcp", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("wgdial: dial %q: %w", addr, err)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return t.tnet.DialContext(ctx, "tcp", addr) // literal IP: no DNS
	}
	ips, err := t.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("wgdial: resolve %q in-tunnel: %w", host, err)
	}
	var lastErr error
	for _, ip := range ips {
		c, derr := t.tnet.DialContext(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
		if derr == nil {
			return c, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("wgdial: %q resolved to no addresses", host)
	}
	return nil, lastErr
}

// Reresolve re-resolves a hostname endpoint (locally — the pre-tunnel
// exception) and updates the peer's endpoint on the live device, so a DNS
// failover or a dynamic endpoint IP is picked up on reconnect. A literal-IP
// endpoint resolves to itself, making this a cheap no-op update. Best-effort:
// on lookup failure the caller keeps the existing endpoint.
func (t *Tunnel) Reresolve(ctx context.Context) error {
	resolved, err := resolveEndpoint(ctx, t.cfg.Endpoint)
	if err != nil {
		return err
	}
	// update_only: refresh the existing peer's endpoint without recreating it,
	// leaving keys/allowed-ips/keepalive untouched.
	return t.dev.IpcSet("public_key=" + t.peerPubHex + "\nupdate_only=true\nendpoint=" + resolved + "\n")
}

// Close tears down the tunnel.
func (t *Tunnel) Close() {
	if t.dev != nil {
		t.dev.Close()
	}
}

// errEndpointFmt wraps every endpoint-shaped failure identically:
// "wgdial: endpoint %q: <cause>".
const errEndpointFmt = "wgdial: endpoint %q: %w"

// resolveEndpoint turns the peer endpoint host:port into ip:port. A literal IP
// is returned unchanged. A hostname is resolved via the LOCAL resolver — the
// ONE intentional exception to the no-local-DNS rule (see the package doc):
// the endpoint is the tunnel's own UDP entry point and cannot be resolved
// through the tunnel that does not yet exist. Prefers an IPv4 result so a
// v4-only host stays on v4 (see chooseBind).
func resolveEndpoint(ctx context.Context, endpoint string) (string, error) {
	host, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf(errEndpointFmt, endpoint, err)
	}
	port, err := endpointPort(portStr)
	if err != nil {
		return "", fmt.Errorf(errEndpointFmt, endpoint, err)
	}
	// Always reconstruct the returned endpoint from a parsed netip.AddrPort, for
	// BOTH the literal-IP and resolved paths: it yields a canonical ip:port with
	// no room for the raw config string (whose port SplitHostPort would accept
	// with an embedded newline) to inject extra UAPI directives, and it unmaps a
	// 4-in-6 literal so it agrees with endpointIsV4 / the v4-only bind.
	if ip, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(ip.Unmap(), port).String(), nil // literal IP: no lookup
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return "", fmt.Errorf("wgdial: resolve endpoint %q: %w", host, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("wgdial: endpoint %q resolved to no addresses", host)
	}
	pick := ips[0]
	for _, ip := range ips {
		if ip.Is4() || ip.Is4In6() {
			pick = ip
			break
		}
	}
	return netip.AddrPortFrom(pick.Unmap(), port).String(), nil
}

// endpointPort parses and bounds a host:port port field to 1..65535. A
// non-numeric value — e.g. one carrying a newline that SplitHostPort otherwise
// accepts — is rejected; this is what stops the endpoint from injecting extra
// UAPI directives (a second peer, allowed_ip, ...) into the device config.
func endpointPort(portStr string) (uint16, error) {
	n, err := strconv.Atoi(portStr)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("bad port %q", portStr)
	}
	return uint16(n), nil
}

// parseDNS splits the configured DNS value into the netstack DNS server IP, its
// port (default 53), and the host:port the in-tunnel resolver dials. A bare IP
// keeps port 53; "ip:port" honors a non-standard port (netstack's own resolver
// is hardwired to :53, so a custom port is served by Tunnel.resolver).
func parseDNS(s string) (ip netip.Addr, port int, addrPort string, err error) {
	host, portStr := s, "53"
	if h, p, e := net.SplitHostPort(s); e == nil {
		host, portStr = h, p
	}
	ip, err = netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, 0, "", fmt.Errorf("wgdial: dns %q: %w", s, err)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return netip.Addr{}, 0, "", fmt.Errorf("wgdial: dns %q: bad port %q", s, portStr)
	}
	return ip, port, net.JoinHostPort(host, portStr), nil
}

// uapiConfig renders the wireguard-go IPC (UAPI) config. Keys are converted
// from base64 to the hex the UAPI expects, and endpoint is the (already
// resolved) ip:port the UAPI requires. allowed_ip=0.0.0.0/0,::/0 routes all
// target traffic through the peer.
func uapiConfig(cfg Config, endpoint string) (string, error) {
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
		"endpoint=" + endpoint + "\n" +
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
