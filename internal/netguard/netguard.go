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

// Package netguard holds the outbound-connection SSRF allowlist shared by
// every server-side fetcher (media proxy, Web Push sender): one policy for
// "is this address safe to dial", not one copy per caller.
package netguard

import (
	"net"
	"net/netip"
)

// specialPurposePrefixes are the IANA special-purpose registry blocks
// (plus multicast/reserved space) that net.IP's classification methods
// do not cover — "not private" is weaker than "globally routable".
// Tables: iana.org/assignments/iana-ipv4-special-registry and
// iana-ipv6-special-registry (fetched 2026-07-18).
var specialPurposePrefixes = func() []netip.Prefix {
	specs := []string{
		// IPv4
		"0.0.0.0/8",       // "this network"
		"192.0.0.0/24",    // protocol assignments (incl. 192.0.0.0/29 DS-Lite)
		"192.0.2.0/24",    // TEST-NET-1
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"192.31.196.0/24", // AS112-v4
		"192.52.193.0/24", // AMT
		"192.88.99.0/24",  // deprecated 6to4 relay anycast
		"192.175.48.0/24", // AS112 direct delegation
		"100.64.0.0/10",   // CGNAT shared address space
		"198.18.0.0/15",   // benchmarking
		"240.0.0.0/4",     // reserved (incl. 255.255.255.255 broadcast)
		// IPv6
		"::/128",        // unspecified (also caught by IsUnspecified)
		"::1/128",       // loopback (also caught by IsLoopback)
		"::ffff:0:0/96", // IPv4-mapped (unwrapped before this check)
		// Deprecated IPv4-embedding forms (RFC 5156): like NAT64, these
		// encode an IPv4 destination, and a lingering translation or
		// tunnel route would reach IPv4 space this policy blocks. An
		// SSRF allowlist has no use for deprecated transition space.
		"::/96",           // IPv4-compatible (deprecated, RFC 4291 §2.5.5.1)
		"::ffff:0:0:0/96", // IPv4-translated (SIIT, RFC 2765)
		// NAT64 translation prefixes embed an IPv4 destination: on a
		// NAT64 host, 64:ff9b::a9fe:a9fe reaches 169.254.169.254 even
		// though the direct IPv4 form is blocked. The whole well-known
		// prefix is denied (site-specific NSP prefixes cannot be known
		// statically and are the deployment's responsibility).
		"64:ff9b::/96",   // well-known NAT64 (RFC 6052)
		"64:ff9b:1::/48", // local-use IPv4/IPv6 translation (RFC 8215)
		"100::/64",       // discard-only
		"2001::/23",      // protocol assignments (TEREDO, ORCHID, benchmarking)
		"2001:db8::/32",  // documentation
		"2002::/16",      // 6to4
		"3fff::/20",      // documentation (RFC 9637)
		"5f00::/16",      // segment routing
	}
	out := make([]netip.Prefix, len(specs))
	for i, s := range specs {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}()

// IsPublicIP reports whether ip is a globally-routable public address —
// the allowlist gate for outbound fetches. It rejects loopback,
// RFC1918/ULA private ranges, link-local (incl. the 169.254.169.254
// cloud metadata endpoint), unspecified, multicast, CGNAT, and the IANA
// special-purpose blocks (documentation, benchmarking, 0/8, 240/4,
// translation/transition ranges).
func IsPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, p := range specialPurposePrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	// IPv6 allowlist backstop: require global unicast (2000::/3). The
	// denylist above is necessarily incomplete for non-global space that
	// net.IP's helpers do not classify — e.g. deprecated site-local
	// fec0::/10 (RFC 3879), which a host with a lingering internal route
	// could still reach. Everything routable on the public Internet lives
	// in 2000::/3; reject anything outside it.
	if addr.Is6() && !globalUnicastV6.Contains(addr) {
		return false
	}
	return true
}

// globalUnicastV6 is the IPv6 global-unicast block (RFC 4291 §2.4). It
// backs the IsPublicIP allowlist so non-global IPv6 space (site-local,
// ULA, link-local, multicast) is rejected regardless of the denylist.
var globalUnicastV6 = netip.MustParsePrefix("2000::/3")
