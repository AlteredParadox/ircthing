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
	"strings"
	"testing"
)

// ValidMonitorTarget gates what may be persisted as a MONITOR buddy and
// what SetMonitored will place on the wire: one bad stored value would
// otherwise fail its whole ten-nick chunk on every reconnect.
func TestValidMonitorTarget(t *testing.T) {
	valid := []string{"alice", "a", strings.Repeat("n", maxMonitorNickLen)}
	invalid := []string{
		"", "a b", "a,b", "a\rb", "a\nb", "a\x00b",
		strings.Repeat("n", maxMonitorNickLen+1),
	}
	for _, n := range valid {
		if !ValidMonitorTarget(n) {
			t.Errorf("ValidMonitorTarget(%q) = false, want true", n)
		}
	}
	for _, n := range invalid {
		if ValidMonitorTarget(n) {
			t.Errorf("ValidMonitorTarget(%.20q) = true, want false", n)
		}
	}
}

// ReconcileMonitored must drop invalid PERSISTED entries (added before
// validation tightened, or by an older version) instead of letting one poison
// its ten-nick chunk at send time. On a fresh connection the delta is pure
// additions of the valid entries only.
func TestReconcileMonitoredDropsInvalidEntries(t *testing.T) {
	m, err := NewManager(testCfg("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	m.setRegistered(true)
	// ReconcileMonitored refuses a server that doesn't advertise MONITOR; this
	// unit test drives the token in directly.
	m.isup.applyToken("MONITOR", "")
	if err := m.ReconcileMonitored([]string{"alice", "bad nick", "b\x00b", strings.Repeat("n", maxMonitorNickLen+1), "bob"}); err != nil {
		t.Fatal(err)
	}
	add := <-m.out
	if add.Command != "MONITOR" || add.Param(0) != "+" || add.Param(1) != "alice,bob" {
		t.Fatalf("MONITOR add = %v, want + alice,bob (invalid entries dropped)", add)
	}
}
