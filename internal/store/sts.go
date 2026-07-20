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

// STS policy persistence on the settings table (key "sts:<host>", value
// JSON {"port": N, "until": <unix ms>}), so a server's upgrade-to-TLS
// policy survives restarts. Implements irc.STSStore.

type stsRecord struct {
	Port  int   `json:"port"`
	Until int64 `json:"until"`
}

func stsKey(host string) string { return "sts:" + host }

// STSPolicy returns the stored policy for host; ok is false when none is
// stored. Expiry is not checked here — callers decide what stale means.
func (s *Store) STSPolicy(ctx context.Context, host string) (port int, until time.Time, ok bool, err error) {
	v, err := s.Setting(ctx, stsKey(host))
	if err != nil || v == "" {
		return 0, time.Time{}, false, err
	}
	var rec stsRecord
	if json.Unmarshal([]byte(v), &rec) != nil || rec.Port <= 0 || rec.Port > 65535 {
		// A NONEMPTY but unusable record is NOT "no policy": a policy was set
		// (the host had STS) and we can't honor it. Returning an error lets the
		// caller fail closed (refuse a plaintext downgrade) per the STS spec,
		// rather than silently treating a corrupt record as absent.
		return 0, time.Time{}, false, fmt.Errorf("sts: unreadable policy record for %q", host)
	}
	return rec.Port, time.UnixMilli(rec.Until), true, nil
}

func (s *Store) SetSTSPolicy(ctx context.Context, host string, port int, until time.Time) error {
	b, err := json.Marshal(stsRecord{Port: port, Until: until.UnixMilli()})
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, stsKey(host), string(b))
}

func (s *Store) ClearSTSPolicy(ctx context.Context, host string) error {
	return s.DeleteSetting(ctx, stsKey(host))
}
