package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var ctx = context.Background()

func openTest(t *testing.T, ringSize int) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	return reopen(t, path, ringSize), path
}

func reopen(t *testing.T, path string, ringSize int) *Store {
	t.Helper()
	s, err := Open(path, Options{RingSize: ringSize})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Stop the always-on background pruner so tests that set a retention
	// policy and call pruneOnce with a controlled `now` are deterministic:
	// the startup goroutine's wall-clock pruneOnce would otherwise race the
	// test's setup and delete its (old-timestamped) fixture rows. The startup
	// prune has already run (or will, harmlessly, against the empty DB before
	// Wait returns); no store test relies on the goroutine running.
	haltPruner(s)
	t.Cleanup(func() { s.Close() })
	return s
}

// haltPruner stops the background pruner so a test can drive pruning
// deterministically via pruneOnce. Idempotent: Close then skips it.
func haltPruner(s *Store) {
	if s.stopPruner != nil {
		s.prunerCancel() // release the pruner context (no leak) and stop it
		close(s.stopPruner)
		s.prunerDone.Wait()
		s.stopPruner = nil
	}
}

// seed appends n messages to net/#chan with ts = (i+1) seconds since
// epoch and raw "msg <i+1>", so message k (1-based) has ts k*1000.
func seed(t *testing.T, s *Store, network, target string, n int) []Message {
	t.Helper()
	out := make([]Message, 0, n)
	for i := 1; i <= n; i++ {
		m, err := s.Append(ctx, network, target, Message{
			Time:    time.UnixMilli(int64(i) * 1000),
			Sender:  "alice",
			Command: "PRIVMSG",
			Raw:     fmt.Sprintf("msg %d", i),
		})
		if err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
		out = append(out, m)
	}
	return out
}

// raws projects pages onto their Raw fields for compact comparison.
func raws(msgs []Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Raw
	}
	return out
}

func wantRaws(from, to int) []string {
	if to < from {
		return nil
	}
	out := make([]string, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("msg %d", i))
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPagination drives Before/After/Latest against a buffer of 10
// messages with a ring of 4 (holding msgs 7..10), asserting both page
// content and which side of the memory/disk boundary served it.
func TestPagination(t *testing.T) {
	const ringSize = 4
	cases := []struct {
		name     string
		query    func(s *Store, msgs []Message) ([]Message, error)
		want     []string
		fromRing bool
	}{
		{
			name:     "latest page within ring",
			query:    func(s *Store, _ []Message) ([]Message, error) { return s.Latest(ctx, "net", "#chan", 3) },
			want:     wantRaws(8, 10),
			fromRing: true,
		},
		{
			name:     "latest page larger than ring goes to disk",
			query:    func(s *Store, _ []Message) ([]Message, error) { return s.Latest(ctx, "net", "#chan", 6) },
			want:     wantRaws(5, 10),
			fromRing: false,
		},
		{
			name: "before cursor, full page inside ring",
			query: func(s *Store, msgs []Message) ([]Message, error) {
				return s.Before(ctx, "net", "#chan", msgs[8].Cursor(), 2) // before msg 9
			},
			want:     wantRaws(7, 8),
			fromRing: true,
		},
		{
			name: "before cursor straddling the ring boundary goes to disk",
			query: func(s *Store, msgs []Message) ([]Message, error) {
				return s.Before(ctx, "net", "#chan", msgs[7].Cursor(), 3) // before msg 8: 5,6,7 but ring starts at 7
			},
			want:     wantRaws(5, 7),
			fromRing: false,
		},
		{
			name: "before oldest is empty, answered by disk",
			query: func(s *Store, msgs []Message) ([]Message, error) {
				return s.Before(ctx, "net", "#chan", msgs[0].Cursor(), 5)
			},
			want:     nil,
			fromRing: false,
		},
		{
			name: "after cursor inside ring",
			query: func(s *Store, msgs []Message) ([]Message, error) {
				return s.After(ctx, "net", "#chan", msgs[7].Cursor(), 2) // after msg 8
			},
			want:     wantRaws(9, 10),
			fromRing: true,
		},
		{
			name: "after newest is empty but ring-authoritative",
			query: func(s *Store, msgs []Message) ([]Message, error) {
				return s.After(ctx, "net", "#chan", msgs[9].Cursor(), 5)
			},
			want:     nil,
			fromRing: true,
		},
		{
			name: "after cursor older than ring goes to disk",
			query: func(s *Store, msgs []Message) ([]Message, error) {
				return s.After(ctx, "net", "#chan", msgs[2].Cursor(), 2) // after msg 3
			},
			want:     wantRaws(4, 5),
			fromRing: false,
		},
		{
			name: "after with zero cursor pages from the beginning",
			query: func(s *Store, _ []Message) ([]Message, error) {
				return s.After(ctx, "net", "#chan", Cursor{}, 3)
			},
			want:     wantRaws(1, 3),
			fromRing: false,
		},
		{
			name: "pure-timestamp cursor excludes that timestamp on Before",
			query: func(s *Store, _ []Message) ([]Message, error) {
				return s.Before(ctx, "net", "#chan", CursorAtTime(time.UnixMilli(9000)), 2)
			},
			want:     wantRaws(7, 8),
			fromRing: true,
		},
		{
			name: "pure-timestamp cursor includes that timestamp on After",
			query: func(s *Store, _ []Message) ([]Message, error) {
				return s.After(ctx, "net", "#chan", CursorAtTime(time.UnixMilli(9000)), 5)
			},
			want:     wantRaws(9, 10),
			fromRing: true,
		},
		{
			name:     "unknown target yields empty page",
			query:    func(s *Store, _ []Message) ([]Message, error) { return s.Latest(ctx, "net", "#nope", 5) },
			want:     nil,
			fromRing: false,
		},
		{
			name:     "unknown network yields empty page",
			query:    func(s *Store, _ []Message) ([]Message, error) { return s.Latest(ctx, "ghost", "#chan", 5) },
			want:     nil,
			fromRing: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTest(t, ringSize)
			msgs := seed(t, s, "net", "#chan", 10)
			ringBefore, dbBefore := s.stats.ringPages, s.stats.dbPages

			got, err := tc.query(s, msgs)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if !equal(raws(got), tc.want) {
				t.Fatalf("page:\n got %q\nwant %q", raws(got), tc.want)
			}
			ringHit := s.stats.ringPages > ringBefore
			dbHit := s.stats.dbPages > dbBefore
			// Unknown-buffer queries touch neither; everything else must
			// touch exactly the expected side.
			if ringHit && dbHit {
				t.Fatal("query hit both ring and db")
			}
			if tc.fromRing && !ringHit && dbHit {
				t.Fatal("expected ring-served page, hit the database")
			}
			if !tc.fromRing && ringHit {
				t.Fatal("expected database-served page, hit the ring")
			}
		})
	}
}

func TestSmallBufferServedEntirelyFromRing(t *testing.T) {
	s, _ := openTest(t, 10)
	seed(t, s, "net", "#chan", 3)

	// Fewer messages than requested, but the ring holds the complete
	// history, so it can authoritatively return the short page.
	got, err := s.Latest(ctx, "net", "#chan", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(raws(got), wantRaws(1, 3)) {
		t.Fatalf("got %q", raws(got))
	}
	if s.stats.dbPages != 0 {
		t.Fatalf("expected pure ring service, saw %d db pages", s.stats.dbPages)
	}
	// Even a before-oldest empty page needs no disk while complete.
	if _, err := s.Before(ctx, "net", "#chan", got[0].Cursor(), 5); err != nil {
		t.Fatal(err)
	}
	if s.stats.dbPages != 0 {
		t.Fatal("complete ring should answer empty pages without disk")
	}
}

func TestSameTimestampOrdersByID(t *testing.T) {
	s, _ := openTest(t, 10)
	ts := time.UnixMilli(5000)
	var msgs []Message
	for i := 1; i <= 3; i++ {
		m, err := s.Append(ctx, "net", "#chan", Message{
			Time: ts, Sender: "a", Command: "PRIVMSG", Raw: fmt.Sprintf("dup %d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, m)
	}
	got, err := s.Latest(ctx, "net", "#chan", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(raws(got), []string{"dup 1", "dup 2", "dup 3"}) {
		t.Fatalf("got %q", raws(got))
	}
	// Cursor at the middle message: Before sees only dup 1, After only dup 3.
	before, _ := s.Before(ctx, "net", "#chan", msgs[1].Cursor(), 10)
	after, _ := s.After(ctx, "net", "#chan", msgs[1].Cursor(), 10)
	if !equal(raws(before), []string{"dup 1"}) || !equal(raws(after), []string{"dup 3"}) {
		t.Fatalf("id tiebreak: before %q after %q", raws(before), raws(after))
	}
}

func TestOutOfOrderTimestamps(t *testing.T) {
	s, _ := openTest(t, 10)
	for _, tsMs := range []int64{5000, 3000, 4000} {
		if _, err := s.Append(ctx, "net", "#chan", Message{
			Time: time.UnixMilli(tsMs), Sender: "a", Command: "PRIVMSG",
			Raw: fmt.Sprintf("ts %d", tsMs),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Latest(ctx, "net", "#chan", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(raws(got), []string{"ts 3000", "ts 4000", "ts 5000"}) {
		t.Fatalf("ring must order by timestamp, got %q", raws(got))
	}
}

func TestReopenWarmsRingAndPreservesBoundary(t *testing.T) {
	s, path := openTest(t, 4)
	seed(t, s, "net", "#chan", 10)
	s.Close()

	s2 := reopen(t, path, 4)
	// Warm ring serves the newest page from memory...
	got, err := s2.Latest(ctx, "net", "#chan", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(raws(got), wantRaws(8, 10)) {
		t.Fatalf("got %q", raws(got))
	}
	if s2.stats.ringPages != 1 || s2.stats.dbPages != 0 {
		t.Fatalf("stats after warm latest: ring=%d db=%d", s2.stats.ringPages, s2.stats.dbPages)
	}
	// ...and a page reaching past the warmed window goes to disk.
	got, err = s2.Before(ctx, "net", "#chan", got[0].Cursor(), 5) // before msg 8
	if err != nil {
		t.Fatal(err)
	}
	if !equal(raws(got), wantRaws(3, 7)) {
		t.Fatalf("got %q", raws(got))
	}
	if s2.stats.dbPages != 1 {
		t.Fatalf("expected disk page, stats: ring=%d db=%d", s2.stats.ringPages, s2.stats.dbPages)
	}
}

func TestReopenSmallBufferIsComplete(t *testing.T) {
	s, path := openTest(t, 10)
	seed(t, s, "net", "#chan", 3)
	s.Close()

	s2 := reopen(t, path, 10)
	if _, err := s2.Latest(ctx, "net", "#chan", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Before(ctx, "net", "#chan", Cursor{TS: 1}, 5); err != nil {
		t.Fatal(err)
	}
	if s2.stats.dbPages != 0 {
		t.Fatal("3 messages under a 10-slot ring must be complete after warm")
	}
}

func TestCursorForMsgID(t *testing.T) {
	s, _ := openTest(t, 4)
	seed(t, s, "net", "#chan", 5)
	anchor, err := s.Append(ctx, "net", "#chan", Message{
		Time: time.UnixMilli(6000), MsgID: "abc123", Sender: "a", Command: "PRIVMSG", Raw: "msg 6",
	})
	if err != nil {
		t.Fatal(err)
	}
	seed2 := []Message{}
	for i := 7; i <= 8; i++ {
		m, err := s.Append(ctx, "net", "#chan", Message{
			Time: time.UnixMilli(int64(i) * 1000), Sender: "a", Command: "PRIVMSG",
			Raw: fmt.Sprintf("msg %d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		seed2 = append(seed2, m)
	}
	_ = seed2

	c, err := s.CursorForMsgID(ctx, "net", "#chan", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if c != anchor.Cursor() {
		t.Fatalf("cursor = %+v, want %+v", c, anchor.Cursor())
	}
	before, _ := s.Before(ctx, "net", "#chan", c, 2)
	after, _ := s.After(ctx, "net", "#chan", c, 2)
	if !equal(raws(before), wantRaws(4, 5)) || !equal(raws(after), wantRaws(7, 8)) {
		t.Fatalf("paging around msgid: before %q after %q", raws(before), raws(after))
	}

	if _, err := s.CursorForMsgID(ctx, "net", "#chan", "nope"); !errors.Is(err, ErrMsgIDNotFound) {
		t.Fatalf("unknown msgid err = %v", err)
	}
	if _, err := s.CursorForMsgID(ctx, "net", "#ghost", "abc123"); !errors.Is(err, ErrMsgIDNotFound) {
		t.Fatalf("unknown buffer err = %v", err)
	}
}

func TestReadMarkers(t *testing.T) {
	s, path := openTest(t, 10)

	// Unset marker: zero time, no error, even for unknown buffers.
	if got, err := s.ReadMarker(ctx, "net", "#chan"); err != nil || !got.IsZero() {
		t.Fatalf("unset marker = %v, %v", got, err)
	}

	// Markers never create buffers; give the buffer a stored message.
	if _, err := s.Append(ctx, "net", "#chan", Message{
		Time: time.UnixMilli(9000), Sender: "a", Command: "PRIVMSG", Raw: ":a PRIVMSG #chan :hi",
	}); err != nil {
		t.Fatal(err)
	}
	// Setting a marker on an unknown buffer is a silent no-op.
	if err := s.SetReadMarker(ctx, "net", "#ghost", time.UnixMilli(5000)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadMarker(ctx, "net", "#ghost"); !got.IsZero() {
		t.Fatalf("marker created a buffer: %v", got)
	}

	t1 := time.UnixMilli(5000)
	t2 := time.UnixMilli(9000)
	if err := s.SetReadMarker(ctx, "net", "#chan", t1); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadMarker(ctx, "net", "#chan"); !got.Equal(t1) {
		t.Fatalf("marker = %v, want %v", got, t1)
	}
	// Markers advance...
	if err := s.SetReadMarker(ctx, "net", "#chan", t2); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadMarker(ctx, "net", "#chan"); !got.Equal(t2) {
		t.Fatalf("marker = %v, want %v", got, t2)
	}
	// ...but never move backwards (multi-device: newest read wins).
	if err := s.SetReadMarker(ctx, "net", "#chan", t1); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadMarker(ctx, "net", "#chan"); !got.Equal(t2) {
		t.Fatalf("marker regressed to %v", got)
	}

	// Buffers are independent.
	if err := s.SetReadMarker(ctx, "net", "#other", t1); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadMarker(ctx, "net", "#chan"); !got.Equal(t2) {
		t.Fatal("marker leaked across buffers")
	}

	// Markers survive a reopen.
	s.Close()
	s2 := reopen(t, path, 10)
	if got, _ := s2.ReadMarker(ctx, "net", "#chan"); !got.Equal(t2) {
		t.Fatalf("marker after reopen = %v, want %v", got, t2)
	}
}

func TestAppendStampsZeroTime(t *testing.T) {
	s, _ := openTest(t, 10)
	m, err := s.Append(ctx, "net", "#chan", Message{Sender: "a", Command: "PRIVMSG", Raw: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Time.IsZero() || time.Since(m.Time) > time.Minute {
		t.Fatalf("zero time not stamped: %v", m.Time)
	}
	if m.ID == 0 || m.Network != "net" || m.Target != "#chan" {
		t.Fatalf("append result incomplete: %+v", m)
	}
}

func TestAppendDedupesMsgID(t *testing.T) {
	s, _ := openTest(t, 10)
	first, err := s.Append(ctx, "net", "#chan", Message{
		Time: time.UnixMilli(1000), MsgID: "m1", Sender: "a", Command: "PRIVMSG", Raw: "one",
	})
	if err != nil || first.ID == 0 {
		t.Fatalf("first append: %+v, %v", first, err)
	}

	// Same msgid in the same buffer: skipped, signaled by ID 0.
	dup, err := s.Append(ctx, "net", "#chan", Message{
		Time: time.UnixMilli(2000), MsgID: "m1", Sender: "a", Command: "PRIVMSG", Raw: "one again",
	})
	if err != nil || dup.ID != 0 {
		t.Fatalf("dup append: %+v, %v", dup, err)
	}
	if msgs, _ := s.Latest(ctx, "net", "#chan", 10); len(msgs) != 1 || msgs[0].Raw != "one" {
		t.Fatalf("after dup: %v", raws(msgs))
	}

	// Same msgid in a different buffer is a different message.
	if m, err := s.Append(ctx, "net", "#other", Message{
		Time: time.UnixMilli(1000), MsgID: "m1", Sender: "a", Command: "PRIVMSG", Raw: "elsewhere",
	}); err != nil || m.ID == 0 {
		t.Fatalf("other-buffer append: %+v, %v", m, err)
	}

	// Messages without msgids never deduplicate.
	for i := 0; i < 2; i++ {
		if m, err := s.Append(ctx, "net", "#chan", Message{
			Time: time.UnixMilli(3000), Sender: "a", Command: "PRIVMSG", Raw: "no msgid",
		}); err != nil || m.ID == 0 {
			t.Fatalf("tagless append %d: %+v, %v", i, m, err)
		}
	}
	if msgs, _ := s.Latest(ctx, "net", "#chan", 10); len(msgs) != 3 {
		t.Fatalf("row count = %d, want 3", len(msgs))
	}
}

func TestAppendRejectsEmptyKeys(t *testing.T) {
	s, _ := openTest(t, 10)
	if _, err := s.Append(ctx, "", "#chan", Message{Raw: "x"}); err == nil {
		t.Fatal("empty network accepted")
	}
	if _, err := s.Append(ctx, "net", "", Message{Raw: "x"}); err == nil {
		t.Fatal("empty target accepted")
	}
}

func TestClampLimit(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, DefaultPageSize},
		{-3, DefaultPageSize},
		{1, 1},
		{MaxPageSize, MaxPageSize},
		{MaxPageSize + 1, MaxPageSize},
	}
	for _, tc := range cases {
		if got := clampLimit(tc.in); got != tc.want {
			t.Errorf("clampLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestMigrationsIdempotentAndRecorded(t *testing.T) {
	s, path := openTest(t, 10)
	s.Close()
	// Second open must be a no-op, not a re-apply.
	s2 := reopen(t, path, 10)

	var n int
	if err := s2.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 11 {
		t.Fatalf("schema_migrations rows = %d, want 11", n)
	}
	var name string
	if err := s2.db.QueryRow(`SELECT name FROM schema_migrations WHERE version = 11`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "0011_fts_recreate.sql" {
		t.Fatalf("recorded name = %q", name)
	}
	// WAL mode actually took effect.
	var mode string
	if err := s2.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func TestBuffers(t *testing.T) {
	s, _ := openTest(t, 10)

	// No buffers yet.
	infos, err := s.Buffers(ctx)
	if err != nil || len(infos) != 0 {
		t.Fatalf("empty store: %v, %v", infos, err)
	}

	seed(t, s, "net", "#chan", 5) // ts 1000..5000
	seed(t, s, "net", "#quiet", 2)
	if err := s.SetReadMarker(ctx, "net", "#chan", time.UnixMilli(3000)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetReadMarker(ctx, "net", "#quiet", time.UnixMilli(2000)); err != nil {
		t.Fatal(err)
	}
	seed(t, s, "other", "#x", 1)

	infos, err = s.Buffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []BufferInfo{
		{Network: "net", Target: "#chan", LastTS: 5000, Marker: 3000, Unread: 2},
		{Network: "net", Target: "#quiet", LastTS: 2000, Marker: 2000, Unread: 0},
		{Network: "other", Target: "#x", LastTS: 1000, Marker: 0, Unread: 1},
	}
	if len(infos) != len(want) {
		t.Fatalf("got %d buffers: %+v", len(infos), infos)
	}
	for i, w := range want {
		if infos[i] != w {
			t.Fatalf("buffer %d = %+v, want %+v", i, infos[i], w)
		}
	}
}

// Unread must count only conversation lines (PRIVMSG/NOTICE), matching what
// the client counts live — presence/system rows (JOIN/PART/QUIT/NICK/MODE/
// TOPIC/KICK) are stored but must not bump the badge, or the two disagree the
// moment a client fetches buffers.
func TestBuffersUnreadCountsOnlyMessages(t *testing.T) {
	s, _ := openTest(t, 50)
	mk := func(cmd, raw string, tsSec int64) {
		if _, err := s.Append(ctx, "net", "#c", Message{
			Time: time.UnixMilli(tsSec * 1000), Sender: "a", Command: cmd, Raw: raw,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("PRIVMSG", ":a PRIVMSG #c :hi", 1)     // counts
	mk("JOIN", ":b JOIN #c", 2)               // system, no count
	mk("PART", ":b PART #c :bye", 3)          // system, no count
	mk("NICK", ":b NICK bb", 4)               // system, no count
	mk("MODE", ":a MODE #c +o b", 5)          // system, no count
	mk("NOTICE", ":a NOTICE #c :heads up", 6) // counts
	mk("QUIT", ":b QUIT :bye", 7)             // system, no count

	infos, err := s.Buffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("got %d buffers, want 1", len(infos))
	}
	// Marker unset -> everything after 0; only the PRIVMSG and NOTICE count.
	if infos[0].Unread != 2 {
		t.Fatalf("unread = %d, want 2 (PRIVMSG+NOTICE only, not the 5 system rows)", infos[0].Unread)
	}
}

func TestDefaultUserSeededAndScoped(t *testing.T) {
	s, _ := openTest(t, 10)
	var username string
	if err := s.db.QueryRow(`SELECT username FROM users WHERE id = 1`).Scan(&username); err != nil {
		t.Fatalf("seeded user missing: %v", err)
	}
	if username != "default" {
		t.Fatalf("seeded username = %q", username)
	}

	// Networks created by the store belong to the seeded user, so the
	// whole tree below them is user-scoped transitively.
	seed(t, s, "net", "#chan", 1)
	var userID int64
	if err := s.db.QueryRow(`SELECT user_id FROM networks WHERE name = 'net'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if userID != defaultUserID {
		t.Fatalf("network user_id = %d, want %d", userID, defaultUserID)
	}

	// The same network name under another user is a distinct network:
	// the store (pinned to user 1) must not see user 2's data.
	if _, err := s.db.Exec(`INSERT INTO users (id, username) VALUES (2, 'other')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO networks (user_id, name) VALUES (2, 'net2')`); err != nil {
		t.Fatal(err)
	}
	if msgs, err := s.Latest(ctx, "net2", "#chan", 10); err != nil || len(msgs) != 0 {
		t.Fatalf("saw another user's network: %v, %v", msgs, err)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	s, path := openTest(t, 10)
	if _, err := s.db.Exec(
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (99, '0099_future.sql', 'now')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("opened a database from the future without error")
	}
}

// A database path containing a URI-special character (%, ?, #) must open — and
// secure to 0600 — exactly the literal file, not a percent-decoded one. Under
// umask 022 a mismatch leaves the real credential DB at 0644 (see the audit).
func TestOpenSecuresLiteralPathWithSpecialChars(t *testing.T) {
	if got := encodeDBPath("a%3fb?c#d"); got != "a%253fb%3Fc%23d" {
		t.Fatalf("encodeDBPath = %q", got)
	}
	dir := t.TempDir()
	for _, name := range []string{"a%3fb.db", "q?x.db", "h#y.db"} {
		path := filepath.Join(dir, name)
		s, err := Open(path, Options{})
		if err != nil {
			t.Fatalf("Open(%q): %v", name, err)
		}
		s.Close()
		fi, err := os.Stat(path) // the LITERAL path must exist and be 0600
		if err != nil {
			t.Fatalf("literal file %q not created (SQLite opened a decoded name?): %v", path, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("literal file %q mode = %o, want 0600", path, fi.Mode().Perm())
		}
	}
}
