package store

import (
	"fmt"
	"testing"
	"time"
)

// The global hot-ring byte budget must bound total resident ring bytes by
// evicting least-recently-used rings, keep its running total exactly in step
// with the rings, and re-warm an evicted buffer from disk on next access.
func TestHotRingByteBudgetLRU(t *testing.T) {
	s, _ := openTest(t, 50)
	s.mu.Lock()
	s.maxRingBytes = 4096 // tiny budget so a handful of small rings fill it
	s.mu.Unlock()

	const buffers = 30
	const perBuf = 8
	for b := 0; b < buffers; b++ {
		target := fmt.Sprintf("#c%d", b)
		for i := 1; i <= perBuf; i++ {
			if _, err := s.Append(ctx, "net", target, Message{
				Time: time.UnixMilli(int64(i) * 1000), Sender: "alice", Command: "PRIVMSG",
				Raw: fmt.Sprintf("m%d", i),
			}); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
	}

	s.mu.Lock()
	// Accounting invariant: ring.bytes == sum msgBytes(m) for each ring, and
	// s.ringBytes == sum of every resident ring's bytes.
	var sum int64
	for _, r := range s.rings {
		var ms int
		for _, m := range r.msgs {
			ms += msgBytes(m)
		}
		if ms != r.bytes {
			s.mu.Unlock()
			t.Fatalf("ring.bytes %d != sum msgBytes %d", r.bytes, ms)
		}
		sum += int64(r.bytes)
	}
	rb, resident, evictions, budget := s.ringBytes, len(s.rings), s.stats.ringEvictions, s.maxRingBytes
	s.mu.Unlock()

	if sum != rb {
		t.Fatalf("s.ringBytes %d != sum of resident ring bytes %d", rb, sum)
	}
	if rb > int64(budget) {
		t.Fatalf("resident ring bytes %d exceed budget %d", rb, budget)
	}
	if evictions == 0 {
		t.Fatal("expected LRU evictions under the tiny budget, got 0")
	}
	if resident >= buffers {
		t.Fatalf("no eviction: %d/%d rings still resident", resident, buffers)
	}

	// #c0 was written first and is coldest — it must have been evicted, then
	// re-warm from disk with its full history on this read.
	got, err := s.Latest(ctx, "net", "#c0", 100)
	if err != nil {
		t.Fatalf("Latest(#c0): %v", err)
	}
	if len(got) != perBuf {
		t.Fatalf("re-warmed #c0 has %d messages, want %d", len(got), perBuf)
	}
}

// Redaction and retention must decrease the tracked total (they free bytes),
// keeping s.ringBytes non-negative and in step.
func TestHotRingBytesShrinkOnRedactAndRetention(t *testing.T) {
	s, _ := openTest(t, 50)
	msgs := seed(t, s, "net", "#chan", 10)
	// Give one message a msgid, then redact it — Raw/Text are scrubbed.
	m, err := s.Append(ctx, "net", "#chan", Message{
		Time: time.UnixMilli(999_000), Sender: "me", Command: "PRIVMSG",
		MsgID: "abc", Raw: "secret body here", Text: "secret body here",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	beforeRedact := s.ringBytes
	s.mu.Unlock()
	if ok, err := s.SetRedacted(ctx, "net", "#chan", m.MsgID, "spam"); err != nil || !ok {
		t.Fatalf("SetRedacted: ok=%v err=%v", ok, err)
	}
	s.mu.Lock()
	afterRedact := s.ringBytes
	s.mu.Unlock()
	if afterRedact >= beforeRedact {
		t.Fatalf("redaction did not shrink ringBytes: %d -> %d", beforeRedact, afterRedact)
	}

	// Prune to the newest 3 messages: the ring drops the older ones and the
	// total must fall accordingly and stay consistent.
	_ = msgs
	if err := s.SetRetention(ctx, 0, 3); err != nil {
		t.Fatalf("SetRetention: %v", err)
	}
	// SetRetention prunes in a background goroutine; do a synchronous prune to
	// make the assertion deterministic.
	if _, err := s.pruneOnce(ctx, time.Now()); err != nil {
		t.Fatalf("pruneOnce: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var sum int64
	for _, r := range s.rings {
		var ms int
		for _, mm := range r.msgs {
			ms += msgBytes(mm)
		}
		if ms != r.bytes {
			t.Fatalf("post-retention ring.bytes %d != sum msgBytes %d", r.bytes, ms)
		}
		sum += int64(r.bytes)
	}
	if sum != s.ringBytes {
		t.Fatalf("post-retention s.ringBytes %d != sum %d", s.ringBytes, sum)
	}
	if s.ringBytes < 0 {
		t.Fatalf("ringBytes went negative: %d", s.ringBytes)
	}
}
