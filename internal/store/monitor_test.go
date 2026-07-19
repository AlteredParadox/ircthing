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

package store

import (
	"reflect"
	"testing"
)

func TestMonitors(t *testing.T) {
	s, _ := openTest(t, 10)

	// Unknown network yields an empty list, not an error.
	if got, err := s.Monitors(ctx, "libera"); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}

	if err := s.AddMonitor(ctx, "libera", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMonitor(ctx, "libera", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMonitor(ctx, "libera", "alice"); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := s.AddMonitor(ctx, "oftc", "carol"); err != nil { // separate network
		t.Fatal(err)
	}

	got, err := s.Monitors(ctx, "libera")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"alice", "bob"}) {
		t.Fatalf("libera monitors = %v", got)
	}
	if got, _ := s.Monitors(ctx, "oftc"); !reflect.DeepEqual(got, []string{"carol"}) {
		t.Fatalf("oftc monitors = %v", got)
	}

	// Remove is scoped and does not touch other networks.
	if err := s.RemoveMonitor(ctx, "libera", "alice"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Monitors(ctx, "libera"); !reflect.DeepEqual(got, []string{"bob"}) {
		t.Fatalf("after remove = %v", got)
	}
	if got, _ := s.Monitors(ctx, "oftc"); len(got) != 1 {
		t.Fatal("remove leaked across networks")
	}
}

func TestMonitorsSurviveReopen(t *testing.T) {
	s, path := openTest(t, 10)
	s.AddMonitor(ctx, "libera", "dave")
	s.Close()

	s2 := reopen(t, path, 10)
	if got, _ := s2.Monitors(ctx, "libera"); !reflect.DeepEqual(got, []string{"dave"}) {
		t.Fatalf("after reopen = %v", got)
	}
}

// TestMonitorNetworkSharedWithBuffers checks that AddMonitor reuses the
// network row created by message storage (no duplicate network).
func TestMonitorNetworkSharedWithBuffers(t *testing.T) {
	s, _ := openTest(t, 10)
	seed(t, s, "libera", "#go", 1) // creates the "libera" network
	if err := s.AddMonitor(ctx, "libera", "alice"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM networks WHERE name = 'libera'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("networks named libera = %d, want 1", n)
	}
}
