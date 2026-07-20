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
	"errors"
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

// A delayed 734 can refer to an add that predates a successful remove/re-add.
// Recovery must therefore establish authoritative server state with MONITOR C
// rather than deleting the rejected nick locally. The same-cap repeat at the
// end models the old autonomous B/C promotion loop and must be a no-op.
func TestMonitorRejectedDelayedNumericRebuildsAuthoritatively(t *testing.T) {
	m := newMonitorTestManager(t)
	gen := m.monGen.Load()

	if err := m.ReconcileMonitored([]string{"A", "X"}); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "+", "A,X")
	if err := m.ReconcileMonitored([]string{"X"}); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "-", "A")
	if err := m.ReconcileMonitored(nil); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "-", "X")
	if err := m.ReconcileMonitored([]string{"X"}); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "+", "X")

	// This is the delayed rejection of the original +A,X, arriving after X is
	// once again genuinely active. Clear+rebuild preserves that current truth.
	if err := m.MonitorRejected([]string{"X"}, 1, gen, []string{"X"}); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "C", "")
	wantQueuedMonitor(t, m, "+", "X")
	if err := m.ReconcileMonitored(nil); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "-", "X")

	// At cap 1, B is the sole active target. A delayed/repeated same-cap 734
	// must not forget B and promote C; otherwise B/C can reject and promote one
	// another forever while an unknown stale server entry occupies the slot.
	if err := m.ReconcileMonitored([]string{"B", "C"}); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "+", "B")
	if err := m.MonitorRejected([]string{"B"}, 1, gen, []string{"B", "C"}); err != nil {
		t.Fatal(err)
	}
	if got := len(m.out); got != 0 {
		t.Fatalf("same-cap 734 queued %d commands, want none", got)
	}
	if err := m.ReconcileMonitored(nil); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "-", "B")
}

// A lower authoritative cap is new information and gets one fresh rebuild;
// repeats at that cap are then ignored like any other stale 734.
func TestMonitorRejectedRebuildsOncePerTighterCap(t *testing.T) {
	m := newMonitorTestManager(t)
	gen := m.monGen.Load()
	if err := m.ReconcileMonitored([]string{"A", "B", "C"}); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "+", "A,B,C")

	if err := m.MonitorRejected([]string{"C"}, 2, gen, []string{"A", "B", "C"}); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "C", "")
	wantQueuedMonitor(t, m, "+", "A,B")
	if err := m.MonitorRejected([]string{"B"}, 1, gen, []string{"A", "B", "C"}); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "C", "")
	wantQueuedMonitor(t, m, "+", "A")
	if err := m.MonitorRejected([]string{"A"}, 1, gen, []string{"A", "B", "C"}); err != nil {
		t.Fatal(err)
	}
	if got := len(m.out); got != 0 {
		t.Fatalf("repeated tighter-cap 734 queued %d commands, want none", got)
	}
	if err := m.ReconcileMonitored(nil); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "-", "A")
}

// A full send queue must not partially apply a recovery or let the next
// mutation use an incremental diff against ambiguous server state. The pending
// flag makes the next normal reconcile retry the whole C+desired transaction.
func TestMonitorRejectedRetriesPendingRebuild(t *testing.T) {
	m := newMonitorTestManager(t)
	if err := m.ReconcileMonitored([]string{"alice"}); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "+", "alice")
	for len(m.out) < cap(m.out) {
		m.out <- newMsg("PING", "filler")
	}

	err := m.MonitorRejected([]string{"alice"}, 1, m.monGen.Load(), []string{"alice"})
	if !errors.Is(err, ErrSendQueueFull) {
		t.Fatalf("MonitorRejected error = %v, want ErrSendQueueFull", err)
	}
	m.monActiveMu.Lock()
	_, stillActive := m.monActive[m.isup.Fold("alice")]
	pending := m.monRebuildPending
	m.monActiveMu.Unlock()
	if !stillActive || !pending {
		t.Fatalf("failed rebuild state: active=%v pending=%v, want both true", stillActive, pending)
	}
	for len(m.out) > 0 {
		<-m.out
	}
	if err := m.ReconcileMonitored([]string{"alice"}); err != nil {
		t.Fatal(err)
	}
	wantQueuedMonitor(t, m, "C", "")
	wantQueuedMonitor(t, m, "+", "alice")
	m.monActiveMu.Lock()
	pending = m.monRebuildPending
	recovered := m.monRecovered734
	m.monActiveMu.Unlock()
	if pending || !recovered {
		t.Fatalf("successful retry state: pending=%v recovered=%v", pending, recovered)
	}
}

func newMonitorTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(testCfg("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	m.setRegistered(true)
	m.isup.applyToken("MONITOR", "")
	return m
}

func wantQueuedMonitor(t *testing.T, m *Manager, modifier, targets string) {
	t.Helper()
	select {
	case got := <-m.out:
		if got.Command != "MONITOR" || got.Param(0) != modifier ||
			(targets != "" && got.Param(1) != targets) {
			t.Fatalf("MONITOR = %v, want %s %s", got, modifier, targets)
		}
	default:
		t.Fatalf("MONITOR %s %s was not queued", modifier, targets)
	}
}
