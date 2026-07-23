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

package api

import "testing"

// resolveSourceURL is the AGPL §13 /source decision. buildinfo VCS is
// authoritative for native builds; the stamped cfgRevision is the Docker
// fallback (a -trimpath/no-.git build has no buildinfo revision).
func TestResolveSourceURL(t *testing.T) {
	const base = sourceBaseURL
	tests := []struct {
		name        string
		biRevision  string
		dirty       bool
		cfgRevision string
		want        string
	}{
		{"native clean", "abc123", false, "", base + "/tree/abc123"},
		{"native dirty pins nothing", "abc123", true, "", base},
		{"docker stamp fallback", "", false, "def456", base + "/tree/def456"},
		{"buildinfo wins over stamp", "abc123", false, "def456", base + "/tree/abc123"},
		{"dirty wins over stamp", "", true, "def456", base},
		{"no info at all", "", false, "", base},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveSourceURL(tc.biRevision, tc.dirty, tc.cfgRevision); got != tc.want {
				t.Fatalf("resolveSourceURL(%q,%v,%q) = %q, want %q",
					tc.biRevision, tc.dirty, tc.cfgRevision, got, tc.want)
			}
		})
	}
}
