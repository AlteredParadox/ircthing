package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A message older than a byte-trimmed (incomplete) ring's oldest entry must
// NOT punch a hole the ring then serves over: after trimToBytes shrinks the
// in-use ring below capacity, a backward (old server-time) insert used to
// front-insert without eviction, and pageAfter's "cursor >= oldest" authority
// then served the ring alone — dropping the on-disk rows between the
// backfilled entry and the trim boundary. The row must instead come from disk.
func TestTrimmedRingNoHoleOnBackwardInsert(t *testing.T) {
	s, _ := openTest(t, 10_000) // large ring_size: the single ring can outgrow the budget
	s.mu.Lock()
	s.maxRingBytes = 4096
	s.mu.Unlock()
	// Fill with ascending server-time so the ring holds a contiguous suffix,
	// then blows the tiny budget and gets trimmed (complete=false).
	for i := 1; i <= 60; i++ {
		if _, err := s.Append(ctx, "net", "#c", Message{
			Time: time.UnixMilli(int64(i) * 1000), Sender: "a", Command: "PRIVMSG",
			Raw: strings.Repeat("x", 200),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A new-to-disk message with an OLD server-time (a delayed relay / hostile
	// clock) whose cursor predates the trimmed ring's oldest entry.
	if _, err := s.Append(ctx, "net", "#c", Message{
		Time: time.UnixMilli(5_000), Sender: "a", Command: "PRIVMSG", Raw: "late",
	}); err != nil {
		t.Fatal(err)
	}
	// All 61 rows must be served (from disk where the ring can't), in order —
	// nothing silently missing.
	got, err := s.Latest(ctx, "net", "#c", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 61 {
		t.Fatalf("Latest served %d messages, want 61 (no hole)", len(got))
	}
	for i := 1; i < len(got); i++ {
		if cursorLess(got[i].Cursor(), got[i-1].Cursor()) {
			t.Fatalf("served rows out of order at %d", i)
		}
	}
	// An After page anchored before the old insert must also see everything
	// after it (this is the exact heuristic that used to serve the hole).
	after, err := s.After(ctx, "net", "#c", Cursor{TS: 1, ID: 0}, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 61 {
		t.Fatalf("After served %d messages, want 61 (no hole)", len(after))
	}
}

// applyRetention must clear `complete` whenever it drops entries: pruning is
// no longer atomic with the disk delete (pruneOnce releases s.mu between
// chunks), so a mid-prune append could make the in-memory filter drop a row
// the disk kept — and a still-`complete` ring would serve that hole
// authoritatively. Clearing forces a disk fallback.
func TestApplyRetentionClearsComplete(t *testing.T) {
	mk := func(i int) Message {
		return Message{ID: int64(i), Time: time.UnixMilli(int64(i) * 1000), Raw: "x"}
	}
	// Age drop clears complete.
	r := newRing(20)
	for i := 1; i <= 10; i++ {
		r.insert(mk(i))
	}
	r.complete = true
	r.applyRetention(5_000, 0) // drops ts < 5s
	if r.complete {
		t.Fatal("complete not cleared after age drop")
	}
	// A no-op retention (nothing matches) leaves complete untouched.
	r2 := newRing(20)
	for i := 1; i <= 5; i++ {
		r2.insert(mk(i))
	}
	r2.complete = true
	r2.applyRetention(0, 100) // maxPerBuffer > len: no drop
	if !r2.complete {
		t.Fatal("complete wrongly cleared when nothing was dropped")
	}
	// Count drop clears complete.
	r2.applyRetention(0, 3)
	if r2.complete {
		t.Fatal("complete not cleared after count drop")
	}
}

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

// Retention pruning must CLEAR the vacated backing-array slots, not just
// truncate the slice: a full age-out otherwise leaves every dropped message's
// strings GC-reachable while bytes reports them freed — and a pruned-then-
// quiet buffer would retain them indefinitely.
func TestRetentionClearsVacatedRingSlots(t *testing.T) {
	mk := func(i int) Message {
		return Message{ID: int64(i), Time: time.UnixMilli(int64(i) * 1000), Raw: "body", Text: "body"}
	}
	// Partial drop: 7 of 10 pruned — slots [3:10] of the backing array must
	// be zeroed.
	r := newRing(20)
	for i := 1; i <= 10; i++ {
		r.insert(mk(i))
	}
	r.applyRetention(8_000, 0) // drops ts < 8s: messages 1..7
	if len(r.msgs) != 3 {
		t.Fatalf("kept %d messages, want 3", len(r.msgs))
	}
	for i, m := range r.msgs[3:10] { // beyond len, within the old length
		if m.Raw != "" || m.Text != "" || m.ID != 0 {
			t.Fatalf("stale backing slot %d not cleared: %+v", 3+i, m)
		}
	}
	// Full age-out (the common dormant-buffer case): everything cleared.
	r2 := newRing(20)
	for i := 1; i <= 10; i++ {
		r2.insert(mk(i))
	}
	r2.applyRetention(999_000, 0)
	if len(r2.msgs) != 0 || r2.bytes != 0 {
		t.Fatalf("full prune left len=%d bytes=%d", len(r2.msgs), r2.bytes)
	}
	for i, m := range r2.msgs[:10] {
		if m.Raw != "" {
			t.Fatalf("stale backing slot %d not cleared after full prune", i)
		}
	}
	// The count-based path (maxPerBuffer) clears too.
	r3 := newRing(20)
	for i := 1; i <= 10; i++ {
		r3.insert(mk(i))
	}
	r3.applyRetention(0, 4)
	for i, m := range r3.msgs[4:10] {
		if m.Raw != "" {
			t.Fatalf("stale backing slot %d not cleared after count prune", 4+i)
		}
	}
}

// A redaction can GROW a ring (short message, long tombstone reason); the
// budget must be enforced on that path, not only on insert/warm.
func TestRedactGrowthTriggersEviction(t *testing.T) {
	s, _ := openTest(t, 50)
	seed(t, s, "net", "#cold", 5) // the eviction victim
	if _, err := s.Append(ctx, "net", "#hot", Message{
		Time: time.UnixMilli(1000), Sender: "a", Command: "PRIVMSG", MsgID: "m1", Raw: "x", Text: "x",
	}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.maxRingBytes = int(s.ringBytes) + 100 // growth headroom well under the reason size
	s.mu.Unlock()
	if ok, err := s.SetRedacted(ctx, "net", "#hot", "m1", strings.Repeat("r", 500)); err != nil || !ok {
		t.Fatalf("SetRedacted: ok=%v err=%v", ok, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ringBytes > int64(s.maxRingBytes) {
		t.Fatalf("ringBytes %d over budget %d after growing redaction", s.ringBytes, s.maxRingBytes)
	}
	if _, cold := s.rings[s.buffers[bufKey{network: "net", target: "#cold"}]]; cold {
		t.Fatal("cold ring not evicted after growing redaction")
	}
}

// Removing rings via buffer delete / network delete must subtract their bytes
// from the global accounting, or the effective budget shrinks forever and the
// cache eventually thrashes.
func TestRingRemovalAccounting(t *testing.T) {
	s, _ := openTest(t, 50)
	seed(t, s, "net", "#a", 5)
	seed(t, s, "net", "#b", 5)
	seed(t, s, "net2", "#c", 5)

	check := func(step string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var sum int64
		for _, r := range s.rings {
			sum += int64(r.bytes)
		}
		if sum != s.ringBytes {
			t.Fatalf("%s: s.ringBytes %d != resident sum %d", step, s.ringBytes, sum)
		}
	}
	if err := s.DeleteBuffer(ctx, "net", "#a"); err != nil {
		t.Fatal(err)
	}
	check("after DeleteBuffer")
	if err := s.DeleteNetwork(ctx, "net2"); err != nil {
		t.Fatal(err)
	}
	check("after DeleteNetwork")
	s.mu.Lock()
	left, rb := len(s.rings), s.ringBytes
	s.mu.Unlock()
	if left != 1 || rb <= 0 {
		t.Fatalf("want 1 resident ring with positive bytes, got %d rings / %d bytes", left, rb)
	}
}

// A single in-use ring larger than the whole budget cannot be LRU-evicted
// (evictRings never evicts keepID) — it must be trimmed from the oldest end
// instead, so one buffer with a huge configured ring_size cannot defeat the
// global bound.
func TestSingleRingTrimmedToBudget(t *testing.T) {
	s, _ := openTest(t, 10_000)
	s.mu.Lock()
	s.maxRingBytes = 4096
	s.mu.Unlock()
	for i := 1; i <= 50; i++ {
		if _, err := s.Append(ctx, "net", "#only", Message{
			Time: time.UnixMilli(int64(i) * 1000), Sender: "a", Command: "PRIVMSG",
			Raw: strings.Repeat("x", 200),
		}); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	r := s.rings[s.buffers[bufKey{network: "net", target: "#only"}]]
	rb, budget := s.ringBytes, s.maxRingBytes
	kept, complete := len(r.msgs), r.complete
	s.mu.Unlock()
	if rb > int64(budget) {
		t.Fatalf("single ring bytes %d exceed budget %d", rb, budget)
	}
	if kept == 0 || kept >= 50 {
		t.Fatalf("expected a trimmed-but-nonempty ring, kept %d/50", kept)
	}
	if complete {
		t.Fatal("trimmed ring still claims complete history")
	}
	// The newest messages survive and are served; older pages come from disk.
	got, err := s.Latest(ctx, "net", "#only", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Fatalf("Latest after trim = %d messages, want 50 (disk fallback)", len(got))
	}
}
