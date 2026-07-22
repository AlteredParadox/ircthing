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

package netguard

import (
	"net"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip     string
		public bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"93.184.216.34", true}, // example.com
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"10.0.0.5", false},
		{"172.16.3.4", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // cloud metadata
		{"fe80::1", false},         // link-local
		{"fc00::1", false},         // unique-local
		{"0.0.0.0", false},
		{"::", false},
		{"224.0.0.1", false},   // multicast
		{"100.64.0.1", false},  // CGNAT
		{"100.127.0.1", false}, // CGNAT upper
		{"100.128.0.1", true},  // just outside CGNAT
		{"::ffff:127.0.0.1", false},
		{"::ffff:10.0.0.1", false},
		// IANA special-purpose blocks beyond the stdlib classifications.
		{"0.1.2.3", false},        // 0.0.0.0/8 "this network"
		{"192.0.0.8", false},      // protocol assignments
		{"192.0.2.10", false},     // TEST-NET-1
		{"198.51.100.7", false},   // TEST-NET-2
		{"203.0.113.99", false},   // TEST-NET-3
		{"198.18.0.1", false},     // benchmarking
		{"198.19.255.255", false}, // benchmarking upper
		{"192.88.99.1", false},    // 6to4 relay anycast (deprecated)
		{"240.0.0.1", false},      // reserved
		{"255.255.255.255", false},
		{"100::1", false},             // discard-only
		{"2001:db8::1", false},        // documentation
		{"2001::42", false},           // TEREDO / protocol assignments
		{"2002:808:808::1", false},    // 6to4
		{"3fff::1", false},            // documentation (RFC 9637)
		{"64:ff9b:1::1", false},       // local-use translation
		{"64:ff9b::a9fe:a9fe", false}, // NAT64-embedded 169.254.169.254
		{"64:ff9b::7f00:1", false},    // NAT64-embedded 127.0.0.1
		{"::808:808", false},          // IPv4-compatible-embedded 8.8.8.8 (deprecated form)
		{"::a9fe:a9fe", false},        // IPv4-compatible-embedded 169.254.169.254
		{"::ffff:0:808:808", false},   // IPv4-translated (SIIT)
		{"64:ff9b::808:808", false},   // NAT64-embedded 8.8.8.8: the whole prefix is out
		{"2620:fe::fe", true},         // Quad9 — ordinary global unicast
		// Non-global IPv6 the stdlib helpers do not classify: the
		// 2000::/3 allowlist backstop must reject these.
		{"fec0::1", false}, // deprecated site-local (RFC 3879)
		{"febf::1", false}, // top of fec0::/10
		{"5000::1", false}, // outside global unicast 2000::/3
		{"1000::1", false}, // below global unicast
		{"3000::1", true},  // still within 2000::/3 (2000::–3fff::)
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", tc.ip)
		}
		if got := IsPublicIP(ip); got != tc.public {
			t.Errorf("IsPublicIP(%s) = %v, want %v", tc.ip, got, tc.public)
		}
	}
}
