package store

import (
	"context"
	"encoding/json"
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
	if json.Unmarshal([]byte(v), &rec) != nil || rec.Port <= 0 {
		return 0, time.Time{}, false, nil // corrupt entry: treat as unset
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
