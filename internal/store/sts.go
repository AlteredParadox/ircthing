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
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// STS policy persistence on the settings table (key "sts:<host>", value JSON
// {"port": N, "until": <unix ms>, "duration": <ms>}), so a server's
// upgrade-to-TLS policy — and the duration needed to reschedule its expiry on
// disconnect — survives restarts. Implements irc.STSStore.
//
// until and duration are POINTERS so an absent field is distinguishable from a
// zero one: a record missing (or zero/negative) `until` is corrupt, not an
// "expired-at-epoch" policy that would silently permit a plaintext downgrade.
type stsRecord struct {
	Port     int    `json:"port"`
	Until    *int64 `json:"until"`
	Duration *int64 `json:"duration,omitempty"` // absent in pre-existing records
}

func stsKey(host string) string { return "sts:" + host }

func ptrInt64(v int64) *int64 { return &v }

// STSPolicy returns the stored policy for host; ok is false when NONE is stored
// (absent row). A present row that is empty, malformed, or missing a valid port
// or a positive `until` is CORRUPT — returned as an error so the caller fails
// closed (refuses a plaintext downgrade) rather than treating it as absent or
// as expired-at-epoch. A positive `until` in the past is a legitimately expired
// policy (ok, with the past time); the caller decides staleness. duration is 0
// when the record predates duration persistence.
func (s *Store) STSPolicy(ctx context.Context, host string) (port int, until time.Time, duration time.Duration, ok bool, err error) {
	v, present, err := s.settingValue(ctx, stsKey(host))
	if err != nil {
		return 0, time.Time{}, 0, false, err
	}
	if !present {
		return 0, time.Time{}, 0, false, nil // genuinely no policy
	}
	var rec stsRecord
	if json.Unmarshal([]byte(v), &rec) != nil ||
		rec.Port <= 0 || rec.Port > 65535 ||
		rec.Until == nil || *rec.Until <= 0 {
		return 0, time.Time{}, 0, false, fmt.Errorf("sts: unreadable policy record for %q", host)
	}
	if rec.Duration != nil && *rec.Duration > 0 {
		duration = time.Duration(*rec.Duration) * time.Millisecond
	}
	return rec.Port, time.UnixMilli(*rec.Until), duration, true, nil
}

func (s *Store) SetSTSPolicy(ctx context.Context, host string, port int, until time.Time, duration time.Duration) error {
	b, err := json.Marshal(stsRecord{
		Port:     port,
		Until:    ptrInt64(until.UnixMilli()),
		Duration: ptrInt64(duration.Milliseconds()),
	})
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, stsKey(host), string(b))
}

func (s *Store) ClearSTSPolicy(ctx context.Context, host string) error {
	return s.DeleteSetting(ctx, stsKey(host))
}
