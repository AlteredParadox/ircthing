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
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Retention is runtime-settable and the stored value (settings table) wins
// over the config Options across restarts: config only seeds it on first run.
func TestRetentionRuntimeSettableAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ret.db")

	// First run seeds retention from the config Options.
	s, err := Open(path, Options{RetentionDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if d, m := s.Retention(); d != 30 || m != 0 {
		t.Fatalf("seeded retention = %d/%d, want 30/0", d, m)
	}
	// A runtime change persists.
	if err := s.SetRetention(context.Background(), 7, 500); err != nil {
		t.Fatal(err)
	}
	if d, m := s.Retention(); d != 7 || m != 500 {
		t.Fatalf("after SetRetention = %d/%d, want 7/500", d, m)
	}
	s.Close()

	// Reopen with a DIFFERENT config: the stored runtime value wins.
	s2, err := Open(path, Options{RetentionDays: 999, RetentionMaxMessages: 999})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if d, m := s2.Retention(); d != 7 || m != 500 {
		t.Fatalf("after reopen = %d/%d, want 7/500 (stored wins over config)", d, m)
	}
}

func TestSetRetentionRejectsInvalidPolicyAtBoundary(t *testing.T) {
	s, _ := openTest(t, 10)
	beforeDays, beforeMax := s.Retention()
	if err := s.SetRetention(context.Background(), MaxRetentionDays+1, 1); err == nil {
		t.Fatal("SetRetention accepted an out-of-range policy")
	}
	afterDays, afterMax := s.Retention()
	if afterDays != beforeDays || afterMax != beforeMax {
		t.Fatalf("invalid policy changed live state: before %d/%d, after %d/%d",
			beforeDays, beforeMax, afterDays, afterMax)
	}
}

func TestRetentionSettingsCorruptionFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name, days, max string
		deleteMax       bool
	}{
		{name: "empty", days: "", max: "10"},
		{name: "malformed", days: "x", max: "10"},
		{name: "negative", days: "-1", max: "10"},
		{name: "overflow", days: "999999999999999999999", max: "10"},
		{name: "partial", days: "10", max: "10", deleteMax: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "retention.db")
			s, err := Open(path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetSettings(context.Background(), map[string]string{retentionDaysKey: tc.days, retentionMaxKey: tc.max}); err != nil {
				t.Fatal(err)
			}
			if tc.deleteMax {
				_ = s.DeleteSetting(context.Background(), retentionMaxKey)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if reopened, err := Open(path, Options{}); err == nil {
				reopened.Close()
				t.Fatal("corrupt retention settings did not fail startup")
			}
		})
	}
}

// A fresh database uses INCREMENTAL auto_vacuum so retention deletes can be
// reclaimed by incremental_vacuum.
func TestAutoVacuumIncremental(t *testing.T) {
	s, _ := openTest(t, 10)
	var mode int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (incremental)", mode)
	}
}

// An existing database created without auto_vacuum is converted on open.
func TestAutoVacuumConvertsExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE t(x)`); err != nil {
		t.Fatal(err)
	}
	var m int
	if err := raw.QueryRow(`PRAGMA auto_vacuum`).Scan(&m); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if m == 2 {
		t.Skip("baseline already incremental; nothing to convert")
	}
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mode int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 2 {
		t.Fatalf("auto_vacuum after convert = %d, want 2", mode)
	}
}

func dbCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPruneByAge(t *testing.T) {
	s, _ := openTest(t, 100)
	base := time.UnixMilli(1_700_000_000_000)
	for i := 0; i < 5; i++ { // one message per day, days 0..4
		if _, err := s.Append(ctx, "net", "#c", Message{
			Time:   base.Add(time.Duration(i) * 24 * time.Hour),
			Sender: "a", Command: "PRIVMSG",
			Raw:  fmt.Sprintf(":a PRIVMSG #c :m%d", i),
			Text: fmt.Sprintf("body%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// now = day 4 + 1h, retain 2 days -> cutoff = day 2 + 1h; days 0,1,2 go.
	s.retention = retentionPolicy{days: 2}
	now := base.Add(4*24*time.Hour + time.Hour)
	n, err := s.pruneOnce(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("pruned %d, want 3", n)
	}
	if got := dbCount(t, s); got != 2 {
		t.Fatalf("db has %d rows, want 2", got)
	}
	// The (complete) ring is reconciled too, so ring-served reads do not
	// return the pruned messages from memory.
	if got, _ := s.Latest(ctx, "net", "#c", 10); len(got) != 2 {
		t.Fatalf("ring served %d messages, want 2 after age prune", len(got))
	}
	// The FTS index tracks the deletion (delete trigger).
	if got := searchTexts(t, s, SearchOptions{Query: "body0"}); len(got) != 0 {
		t.Fatalf("pruned message still searchable: %v", got)
	}
	if got := searchTexts(t, s, SearchOptions{Query: "body4"}); len(got) != 1 {
		t.Fatalf("retained message not searchable: %v", got)
	}
}

// Regression: an age-only policy that deletes every row of a dormant
// buffer whose history fits in the ring must not keep serving those rows
// from the (complete) ring.
func TestPruneReconcilesCompleteRing(t *testing.T) {
	s, _ := openTest(t, 100) // ring larger than history -> stays complete
	base := time.UnixMilli(1_700_000_000_000)
	for i := 0; i < 5; i++ {
		if _, err := s.Append(ctx, "net", "#c", Message{
			Time:   base.Add(time.Duration(i) * 24 * time.Hour),
			Sender: "a", Command: "PRIVMSG", Raw: fmt.Sprintf("m%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.Latest(ctx, "net", "#c", 10); len(got) != 5 {
		t.Fatalf("pre-prune Latest = %d, want 5", len(got))
	}

	s.retention = retentionPolicy{days: 1} // age only, maxPerBuffer = 0
	n, err := s.pruneOnce(ctx, base.Add(10*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || dbCount(t, s) != 0 {
		t.Fatalf("prune deleted %d (db=%d), want 5 (db=0)", n, dbCount(t, s))
	}
	// The bug: the ring kept serving all 5 from memory. Must be 0 now.
	if got, _ := s.Latest(ctx, "net", "#c", 10); len(got) != 0 {
		t.Fatalf("ring served %d pruned messages from memory, want 0", len(got))
	}
}

func TestPruneByCount(t *testing.T) {
	s, _ := openTest(t, 100)
	for i := 0; i < 10; i++ {
		if _, err := s.Append(ctx, "net", "#c", Message{
			Time:   time.UnixMilli(int64(i+1) * 1000),
			Sender: "a", Command: "PRIVMSG", Raw: fmt.Sprintf("m%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A second buffer is capped independently.
	for i := 0; i < 3; i++ {
		if _, err := s.Append(ctx, "net", "#d", Message{
			Time:   time.UnixMilli(int64(i+1) * 1000),
			Sender: "a", Command: "PRIVMSG", Raw: fmt.Sprintf("d%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	s.retention = retentionPolicy{maxPerBuffer: 4}
	n, err := s.pruneOnce(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 { // #c: 10 -> 4 (drop 6); #d: 3 -> 3 (drop 0)
		t.Fatalf("pruned %d, want 6", n)
	}
	if got := dbCount(t, s); got != 7 { // 4 + 3
		t.Fatalf("db has %d rows, want 7", got)
	}
}

func TestPruneDisabledIsNoop(t *testing.T) {
	s, _ := openTest(t, 100)
	for i := 0; i < 3; i++ {
		if _, err := s.Append(ctx, "net", "#c", Message{
			Time:   time.UnixMilli(int64(i+1) * 1000),
			Sender: "a", Command: "PRIVMSG", Raw: fmt.Sprintf("m%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Default policy (both zero) deletes nothing.
	n, err := s.pruneOnce(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || dbCount(t, s) != 3 {
		t.Fatalf("no-op prune deleted rows: n=%d count=%d", n, dbCount(t, s))
	}
}

// Chunked deletion must remove EVERY qualifying row across multiple batches,
// for both the age and per-buffer-count dimensions, with a chunk far smaller
// than the dataset.
func TestPruneChunkedMultiBatch(t *testing.T) {
	defer func(c int) { pruneChunk = c }(pruneChunk)
	pruneChunk = 3 // force several batches over the row counts below

	s, _ := openTest(t, 500)
	base := time.UnixMilli(1_700_000_000_000)
	// #c: 20 messages, one per hour.
	for i := 0; i < 20; i++ {
		if _, err := s.Append(ctx, "net", "#c", Message{
			Time:   base.Add(time.Duration(i) * time.Hour),
			Sender: "a", Command: "PRIVMSG", Raw: fmt.Sprintf("c%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// #d: 14 messages, so the per-buffer cap prunes it independently.
	for i := 0; i < 14; i++ {
		if _, err := s.Append(ctx, "net", "#d", Message{
			Time:   base.Add(time.Duration(i) * time.Hour),
			Sender: "a", Command: "PRIVMSG", Raw: fmt.Sprintf("d%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Age prune: cutoff at hour 8 drops #c[0..7] and #d[0..7] = 16 rows,
	// across many 3-row chunks.
	s.retention = retentionPolicy{days: 0}
	cutoff := base.Add(8 * time.Hour)
	// Drive age pruning directly via a controlled now: retain "0 days" won't
	// prune, so use maxPerBuffer to also exercise the count path below. First
	// the age path:
	s.retention = retentionPolicy{days: 1}
	n, err := s.pruneOnce(ctx, cutoff.Add(24*time.Hour)) // now = cutoff + 1 day
	if err != nil {
		t.Fatal(err)
	}
	if n != 16 {
		t.Fatalf("age prune deleted %d, want 16 (8 from each buffer)", n)
	}
	if got := dbCount(t, s); got != 18 { // 12 + 6
		t.Fatalf("db has %d rows after age prune, want 18", got)
	}

	// Now a per-buffer count cap of 4 across the survivors: #c 12->4 (drop 8),
	// #d 6->4 (drop 2) = 10 rows, again across many chunks.
	s.retention = retentionPolicy{maxPerBuffer: 4}
	n, err = s.pruneOnce(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("count prune deleted %d, want 10", n)
	}
	if got := dbCount(t, s); got != 8 { // 4 + 4
		t.Fatalf("db has %d rows after count prune, want 8", got)
	}
	// The survivors are the NEWEST 4 of each buffer, in order.
	got, _ := s.Latest(ctx, "net", "#c", 10)
	if len(got) != 4 || got[3].Raw != "c19" || got[0].Raw != "c16" {
		t.Fatalf("#c survivors = %v, want c16..c19", rawsOf(got))
	}
}

func rawsOf(ms []Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Raw
	}
	return out
}

// Open with retention configured must start a pruner that Close stops
// cleanly (no hang, no leak).
func TestPrunerLifecycle(t *testing.T) {
	path := t.TempDir() + "/r.db"
	s, err := Open(path, Options{RetentionMaxMessages: 5})
	if err != nil {
		t.Fatal(err)
	}
	if s.stopPruner == nil {
		t.Fatal("retention configured but pruner not started")
	}
	// Close must return promptly (the WaitGroup joins the goroutine).
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung: pruner goroutine did not exit")
	}
}
