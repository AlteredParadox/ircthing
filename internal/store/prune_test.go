package store

import (
	"fmt"
	"testing"
	"time"
)

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
			Time: base.Add(time.Duration(i) * 24 * time.Hour),
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
	// The FTS index tracks the deletion (delete trigger).
	if got := searchTexts(t, s, SearchOptions{Query: "body0"}); len(got) != 0 {
		t.Fatalf("pruned message still searchable: %v", got)
	}
	if got := searchTexts(t, s, SearchOptions{Query: "body4"}); len(got) != 1 {
		t.Fatalf("retained message not searchable: %v", got)
	}
}

func TestPruneByCount(t *testing.T) {
	s, _ := openTest(t, 100)
	for i := 0; i < 10; i++ {
		if _, err := s.Append(ctx, "net", "#c", Message{
			Time: time.UnixMilli(int64(i+1) * 1000),
			Sender: "a", Command: "PRIVMSG", Raw: fmt.Sprintf("m%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A second buffer is capped independently.
	for i := 0; i < 3; i++ {
		if _, err := s.Append(ctx, "net", "#d", Message{
			Time: time.UnixMilli(int64(i+1) * 1000),
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
			Time: time.UnixMilli(int64(i+1) * 1000),
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
