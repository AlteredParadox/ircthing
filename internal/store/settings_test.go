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

	if _, _, _, ok, err := s.STSPolicy(ctx, "irc.example"); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}

	until := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	if err := s.SetSTSPolicy(ctx, "irc.example", 6697, until, 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := reopen(t, path, 10)
	port, got, dur, ok, err := s2.STSPolicy(ctx, "irc.example")
	if err != nil || !ok || port != 6697 || !got.Equal(until) || dur != 3*time.Hour {
		t.Fatalf("after reopen: port=%d until=%v dur=%v ok=%v err=%v", port, got, dur, ok, err)
	}

	if err := s2.ClearSTSPolicy(ctx, "irc.example"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, _ := s2.STSPolicy(ctx, "irc.example"); ok {
		t.Fatal("policy survived clear")
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
		if _, _, _, ok, err := s.STSPolicy(ctx, "bad.example"); err == nil || ok {
			t.Fatalf("STSPolicy(%q) = ok=%v err=%v, want a fail-closed error", v, ok, err)
		}
	}
	// A positive PAST until is a legitimately expired policy: ok, no error.
	past := time.Now().Add(-time.Hour).UnixMilli()
	if err := s.SetSetting(ctx, stsKey("expired.example"),
		`{"port":6697,"until":`+strconv.FormatInt(past, 10)+`}`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok, err := s.STSPolicy(ctx, "expired.example"); err != nil || !ok {
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
		{"", 0},                                                     // absent → legacy, no reschedule
		{"0", 0},                                                    // present zero → discarded
		{"-5", 0},                                                   // negative → discarded
		{strconv.FormatInt(1<<62, 10), 0},                           // MaxInt-ish → over cap, discarded (no overflow)
		{strconv.FormatInt(maxSTSDurationMs+1, 10), 0},              // just over 100y → discarded
		{"3600000", time.Hour},                                      // 1h, valid
		{strconv.FormatInt(maxSTSDurationMs, 10), time.Duration(maxSTSDurationMs) * time.Millisecond}, // max valid
	}
	for _, tc := range cases {
		if err := s.SetSetting(ctx, stsKey("d.example"), base(tc.dur)); err != nil {
			t.Fatal(err)
		}
		_, _, dur, ok, err := s.STSPolicy(ctx, "d.example")
		if err != nil || !ok {
			t.Fatalf("duration=%q: ok=%v err=%v, want a valid policy", tc.dur, ok, err)
		}
		if dur != tc.wantDur {
			t.Errorf("duration=%q: got %v, want %v", tc.dur, dur, tc.wantDur)
		}
	}
}
