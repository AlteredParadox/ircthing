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
	"strconv"
	"testing"
	"time"
)

func TestSettings(t *testing.T) {
	s, _ := openTest(t, 10)

	// Unset key reads as empty, no error.
	v, err := s.Setting(ctx, "prefs")
	if err != nil {
		t.Fatal(err)
	}
	if v != "" {
		t.Fatalf("unset key = %q, want empty", v)
	}

	if err := s.SetSetting(ctx, "prefs", `{"theme":"dark"}`); err != nil {
		t.Fatal(err)
	}
	if v, _ = s.Setting(ctx, "prefs"); v != `{"theme":"dark"}` {
		t.Fatalf("got %q", v)
	}

	// Replace, not append.
	if err := s.SetSetting(ctx, "prefs", `{"theme":"light"}`); err != nil {
		t.Fatal(err)
	}
	if v, _ = s.Setting(ctx, "prefs"); v != `{"theme":"light"}` {
		t.Fatalf("after replace got %q", v)
	}

	// Keys are independent.
	if err := s.SetSetting(ctx, "other", "x"); err != nil {
		t.Fatal(err)
	}
	if v, _ = s.Setting(ctx, "prefs"); v != `{"theme":"light"}` {
		t.Fatalf("other key clobbered prefs: %q", v)
	}
}

func TestSettingsSurviveReopen(t *testing.T) {
	s, path := openTest(t, 10)
	if err := s.SetSetting(ctx, "prefs", `{"accent":"rose"}`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := reopen(t, path, 10)
	v, err := s2.Setting(ctx, "prefs")
	if err != nil {
		t.Fatal(err)
	}
	if v != `{"accent":"rose"}` {
		t.Fatalf("after reopen got %q", v)
	}
}

func TestSTSPolicyRoundTrip(t *testing.T) {
	s, path := openTest(t, 10)

	if _, _, _, _, _, ok, err := s.STSPolicy(ctx, "irc.example"); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}

	until := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	if _, _, err := s.SetSTSPolicy(ctx, "irc.example", 6697, until, 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := reopen(t, path, 10)
	port, got, dur, revision, policyEpoch, ok, err := s2.STSPolicy(ctx, "IRC.Example.")
	if err != nil || !ok || port != 6697 || !got.Equal(until) || dur != 3*time.Hour {
		t.Fatalf("after reopen: port=%d until=%v dur=%v ok=%v err=%v", port, got, dur, ok, err)
	}
	if revision == 0 || policyEpoch == 0 {
		t.Fatal("persisted policy has no revision/epoch")
	}

	_, clearEpoch, err := s2.ClearSTSPolicy(ctx, "irc.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, clearedRevision, gotEpoch, ok, _ := s2.STSPolicy(ctx, "irc.example"); ok || clearedRevision <= revision || gotEpoch != clearEpoch || gotEpoch == policyEpoch {
		t.Fatal("policy survived clear")
	}
}

func TestSTSPolicyMigratesLegacyHostnameAliases(t *testing.T) {
	s, _ := openTest(t, 10)
	until := time.Now().Add(2 * time.Hour).Truncate(time.Millisecond)
	legacy := `{"port":6697,"until":` + strconv.FormatInt(until.UnixMilli(), 10) + `,"duration":3600000}`
	// This is the exact raw-key shape written by pre-canonicalization releases.
	if err := s.SetSetting(ctx, "sts:IRC.Example.", legacy); err != nil {
		t.Fatal(err)
	}
	port, gotUntil, duration, revision, policyEpoch, ok, err := s.STSPolicy(ctx, "irc.example")
	if err != nil || !ok || port != 6697 || !gotUntil.Equal(until) || duration != time.Hour || revision == 0 || policyEpoch != 0 {
		t.Fatalf("legacy lookup: port=%d until=%v duration=%v revision=%d epoch=%d ok=%v err=%v", port, gotUntil, duration, revision, policyEpoch, ok, err)
	}
	if _, present, err := s.SettingValue(ctx, "sts:IRC.Example."); err != nil || present {
		t.Fatalf("legacy alias remains after migration: present=%v err=%v", present, err)
	}
	if _, present, err := s.SettingValue(ctx, "sts:irc.example"); err != nil || !present {
		t.Fatalf("canonical policy missing after migration: present=%v err=%v", present, err)
	}
}

func TestSTSPolicyAliasMergePrefersActiveAndFailsClosed(t *testing.T) {
	s, _ := openTest(t, 10)
	past := time.Now().Add(-time.Hour).UnixMilli()
	future := time.Now().Add(4 * time.Hour).Truncate(time.Millisecond)
	if err := s.SetSetting(ctx, "sts:irc.example", `{"port":6697,"until":`+strconv.FormatInt(past, 10)+`,"revision":99}`); err != nil {
		t.Fatal(err)
	}
	// Revisions on legacy aliases are independent: the lower revision must win
	// because it is the active downgrade-protection policy.
	if err := s.SetSetting(ctx, "sts:IRC.Example.", `{"port":7000,"until":`+strconv.FormatInt(future.UnixMilli(), 10)+`,"duration":7200000,"revision":1}`); err != nil {
		t.Fatal(err)
	}
	port, until, duration, revision, policyEpoch, ok, err := s.STSPolicy(ctx, "irc.example")
	if err != nil || !ok || port != 7000 || !until.Equal(future) || duration != 2*time.Hour || revision <= 99 || policyEpoch != 0 {
		t.Fatalf("merged policy: port=%d until=%v duration=%v revision=%d epoch=%d ok=%v err=%v", port, until, duration, revision, policyEpoch, ok, err)
	}

	if err := s.SetSetting(ctx, "sts:IRC.EXAMPLE.", `not json`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, err := s.STSPolicy(ctx, "irc.example"); err == nil {
		t.Fatal("corrupt canonical-equivalent alias was ignored")
	}
}

func TestSTSStaleRescheduleCannotOverwriteNewerPolicy(t *testing.T) {
	s, _ := openTest(t, 10)
	until := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	oldRevision, oldEpoch, err := s.SetSTSPolicy(ctx, "irc.example", 6697, until, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	clearRevision, clearEpoch, err := s.ClearSTSPolicy(ctx, "IRC.EXAMPLE.")
	if err != nil {
		t.Fatal(err)
	}
	if _, applied, err := s.RescheduleSTSPolicy(ctx, "irc.example", oldRevision, time.Now().Add(2*time.Hour)); err != nil || applied {
		t.Fatalf("stale reschedule: applied=%v err=%v", applied, err)
	}
	if _, _, _, revision, epoch, ok, err := s.STSPolicy(ctx, "irc.example"); err != nil || ok || revision != clearRevision || epoch != clearEpoch || epoch == oldEpoch {
		t.Fatalf("clear overwritten: revision=%d epoch=%d ok=%v err=%v", revision, epoch, ok, err)
	}

	currentRevision, currentEpoch, err := s.SetSTSPolicy(ctx, "irc.example", 7000, until, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	newUntil := time.Now().Add(3 * time.Hour).Truncate(time.Millisecond)
	newRevision, applied, err := s.RescheduleSTSPolicy(ctx, "irc.example", currentRevision, newUntil)
	if err != nil || !applied || newRevision <= currentRevision {
		t.Fatalf("current reschedule: revision=%d applied=%v err=%v", newRevision, applied, err)
	}
	port, gotUntil, duration, _, gotEpoch, ok, err := s.STSPolicy(ctx, "irc.example")
	if err != nil || !ok || port != 7000 || duration != 2*time.Hour || !gotUntil.Equal(newUntil) || gotEpoch != currentEpoch {
		t.Fatalf("rescheduled policy: port=%d until=%v duration=%v epoch=%d ok=%v err=%v", port, gotUntil, duration, gotEpoch, ok, err)
	}
}

func TestSTSPolicySetRepairsCorruptRowsWithoutGenerationABA(t *testing.T) {
	s, _ := openTest(t, 10)
	oldUntil := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	oldRevision, oldEpoch, err := s.SetSTSPolicy(ctx, "irc.example", 6697, oldUntil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, stsKey("irc.example"), `not json`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, "sts:IRC.Example.", `{"port":6697}`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, _, err := s.STSPolicy(ctx, "irc.example"); err == nil {
		t.Fatal("corrupt policy unexpectedly readable before repair")
	}

	newUntil := time.Now().Add(4 * time.Hour).Truncate(time.Millisecond)
	newRevision, newEpoch, err := s.SetSTSPolicy(ctx, "irc.example", 7443, newUntil, 4*time.Hour)
	if err != nil {
		t.Fatalf("repairing SetSTSPolicy: %v", err)
	}
	// The unreadable row may have contained any prior counters. Repair must not
	// restart at a predictable low generation, where an old manager could pass
	// a stale CAS (ABA).
	if newRevision < 1<<62 || newEpoch < 1<<62 || newRevision == oldRevision || newEpoch == oldEpoch {
		t.Fatalf("repair generations = revision %d epoch %d; want fresh opaque generations", newRevision, newEpoch)
	}
	port, gotUntil, duration, gotRevision, gotEpoch, ok, err := s.STSPolicy(ctx, "irc.example")
	if err != nil || !ok || port != 7443 || !gotUntil.Equal(newUntil) || duration != 4*time.Hour || gotRevision != newRevision || gotEpoch != newEpoch {
		t.Fatalf("repaired policy: port=%d until=%v duration=%v revision=%d epoch=%d ok=%v err=%v", port, gotUntil, duration, gotRevision, gotEpoch, ok, err)
	}
	if _, present, err := s.SettingValue(ctx, "sts:IRC.Example."); err != nil || present {
		t.Fatalf("corrupt alias survived repair: present=%v err=%v", present, err)
	}
	if got, applied, err := s.RescheduleSTSPolicy(ctx, "irc.example", oldRevision, time.Now().Add(8*time.Hour)); err != nil || applied || got != newRevision {
		t.Fatalf("pre-corruption CAS after repair: revision=%d applied=%v err=%v", got, applied, err)
	}
}

func TestSTSPolicyClearRepairsCorruptAlias(t *testing.T) {
	s, _ := openTest(t, 10)
	oldRevision, oldEpoch, err := s.SetSTSPolicy(ctx, "irc.example", 6697, time.Now().Add(time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, "sts:IRC.Example.", ``); err != nil {
		t.Fatal(err)
	}

	clearRevision, clearEpoch, err := s.ClearSTSPolicy(ctx, "irc.example")
	if err != nil {
		t.Fatalf("repairing ClearSTSPolicy: %v", err)
	}
	if clearRevision < 1<<62 || clearEpoch < 1<<62 || clearRevision == oldRevision || clearEpoch == oldEpoch {
		t.Fatalf("clear repair generations = revision %d epoch %d; want fresh opaque generations", clearRevision, clearEpoch)
	}
	if _, _, _, gotRevision, gotEpoch, ok, err := s.STSPolicy(ctx, "irc.example"); err != nil || ok || gotRevision != clearRevision || gotEpoch != clearEpoch {
		t.Fatalf("repaired clear: revision=%d epoch=%d ok=%v err=%v", gotRevision, gotEpoch, ok, err)
	}
	if _, present, err := s.SettingValue(ctx, "sts:IRC.Example."); err != nil || present {
		t.Fatalf("corrupt alias survived clear repair: present=%v err=%v", present, err)
	}
	if got, applied, err := s.RescheduleSTSPolicy(ctx, "irc.example", oldRevision, time.Now().Add(2*time.Hour)); err != nil || applied || got != clearRevision {
		t.Fatalf("pre-corruption CAS after clear: revision=%d applied=%v err=%v", got, applied, err)
	}
}

// A present-but-malformed STS record — empty value, missing/zero/negative
// `until`, bad port, or non-JSON — must FAIL CLOSED (error), not be read as an
// absent or expired policy that would permit a plaintext downgrade. A positive
// past `until` is a legitimately expired policy (ok, not an error).
func TestSTSPolicyRejectsMalformed(t *testing.T) {
	s, _ := openTest(t, 10)
	bad := []string{
		"",                          // present but empty (tampered)
		`{"port":6697}`,             // missing until
		`{"port":6697,"until":0}`,   // zero until
		`{"port":6697,"until":-5}`,  // negative until
		`{"port":0,"until":123}`,    // bad port
		`{"port":70000,"until":12}`, // port out of range
		`not json`,                  // malformed
	}
	for _, v := range bad {
		if err := s.SetSetting(ctx, stsKey("bad.example"), v); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, _, ok, err := s.STSPolicy(ctx, "bad.example"); err == nil || ok {
			t.Fatalf("STSPolicy(%q) = ok=%v err=%v, want a fail-closed error", v, ok, err)
		}
	}
	// A positive PAST until is a legitimately expired policy: ok, no error.
	past := time.Now().Add(-time.Hour).UnixMilli()
	if err := s.SetSetting(ctx, stsKey("expired.example"),
		`{"port":6697,"until":`+strconv.FormatInt(past, 10)+`}`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, ok, err := s.STSPolicy(ctx, "expired.example"); err != nil || !ok {
		t.Fatalf("expired policy: ok=%v err=%v, want ok with no error", ok, err)
	}
}

// A present-but-invalid `duration` (zero, negative, over-cap, or overflow-prone)
// must NOT fail the policy — `until` is the security field — but must be
// DISCARDED (duration 0, rescheduling lost) rather than trusted or converted
// into an int64 overflow. Only an ABSENT duration is a legacy record.
func TestSTSPolicyDurationValidation(t *testing.T) {
	s, _ := openTest(t, 10)
	future := time.Now().Add(time.Hour).UnixMilli()
	base := func(dur string) string {
		r := `{"port":6697,"until":` + strconv.FormatInt(future, 10)
		if dur != "" {
			r += `,"duration":` + dur
		}
		return r + `}`
	}
	cases := []struct {
		dur     string
		wantDur time.Duration
	}{
		{"", 0},                           // absent → legacy, no reschedule
		{"0", 0},                          // present zero → discarded
		{"-5", 0},                         // negative → discarded
		{strconv.FormatInt(1<<62, 10), 0}, // MaxInt-ish → over cap, discarded (no overflow)
		{strconv.FormatInt(maxSTSDurationMs+1, 10), 0}, // just over 100y → discarded
		{"3600000", time.Hour},                         // 1h, valid
		{strconv.FormatInt(maxSTSDurationMs, 10), time.Duration(maxSTSDurationMs) * time.Millisecond}, // max valid
	}
	for _, tc := range cases {
		if err := s.SetSetting(ctx, stsKey("d.example"), base(tc.dur)); err != nil {
			t.Fatal(err)
		}
		_, _, dur, _, _, ok, err := s.STSPolicy(ctx, "d.example")
		if err != nil || !ok {
			t.Fatalf("duration=%q: ok=%v err=%v, want a valid policy", tc.dur, ok, err)
		}
		if dur != tc.wantDur {
			t.Errorf("duration=%q: got %v, want %v", tc.dur, dur, tc.wantDur)
		}
	}
}
