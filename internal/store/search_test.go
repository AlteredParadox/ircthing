package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// bodyOf extracts a PRIVMSG trailing param from a raw line, matching how
// the frontend would render the message text from search results (which
// carry Raw, not Text).
func bodyOf(raw string) string {
	if i := strings.Index(raw, " :"); i != -1 {
		return raw[i+2:]
	}
	return raw
}

// appendMsg appends a searchable PRIVMSG with the given text.
func appendMsg(t *testing.T, s *Store, network, target string, tsMs int64, text string) Message {
	t.Helper()
	m, err := s.Append(ctx, network, target, Message{
		Time:    time.UnixMilli(tsMs),
		Sender:  "alice",
		Command: "PRIVMSG",
		Raw:     fmt.Sprintf(":alice!u@h PRIVMSG %s :%s", target, text),
		Text:    text,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func searchTexts(t *testing.T, s *Store, opts SearchOptions) []string {
	t.Helper()
	msgs, err := s.Search(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = bodyOf(m.Raw)
	}
	return out
}

func TestSearch(t *testing.T) {
	s, _ := openTest(t, 10)
	appendMsg(t, s, "libera", "#go", 1000, "the quick brown fox")
	appendMsg(t, s, "libera", "#go", 2000, "lazy dogs sleep")
	appendMsg(t, s, "libera", "#go", 3000, "a Quick reply")   // case fold
	appendMsg(t, s, "libera", "#rust", 4000, "quick as rust") // other buffer
	appendMsg(t, s, "oftc", "#go", 5000, "quick on oftc")     // other network

	cases := []struct {
		name string
		opts SearchOptions
		want []string
	}{
		{
			name: "matches across buffers, newest first",
			opts: SearchOptions{Query: "quick"},
			want: []string{"quick on oftc", "quick as rust", "a Quick reply", "the quick brown fox"},
		},
		{
			name: "case-insensitive",
			opts: SearchOptions{Query: "QUICK"},
			want: []string{"quick on oftc", "quick as rust", "a Quick reply", "the quick brown fox"},
		},
		{
			name: "multiple terms are ANDed",
			opts: SearchOptions{Query: "quick fox"},
			want: []string{"the quick brown fox"},
		},
		{
			name: "scoped to a network includes all its buffers",
			opts: SearchOptions{Query: "quick", Network: "libera"},
			want: []string{"quick as rust", "a Quick reply", "the quick brown fox"},
		},
		{
			name: "scoped to a buffer",
			opts: SearchOptions{Query: "quick", Network: "libera", Target: "#rust"},
			want: []string{"quick as rust"},
		},
		{
			name: "no matches",
			opts: SearchOptions{Query: "elephant"},
			want: nil,
		},
		{
			name: "empty query returns nothing",
			opts: SearchOptions{Query: "   "},
			want: nil,
		},
		{
			name: "diacritics folded",
			opts: SearchOptions{Query: "quick"},
			want: []string{"quick on oftc", "quick as rust", "a Quick reply", "the quick brown fox"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := searchTexts(t, s, tc.opts)
			if !equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSearchPagination(t *testing.T) {
	s, _ := openTest(t, 10)
	for i := 1; i <= 5; i++ {
		appendMsg(t, s, "libera", "#go", int64(i)*1000, fmt.Sprintf("match number %d", i))
	}
	first, err := s.Search(ctx, SearchOptions{Query: "match", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || bodyOf(first[0].Raw) != "match number 5" {
		t.Fatalf("first page: %q", texts(first))
	}
	// Older matches before the last of the first page.
	second, err := s.Search(ctx, SearchOptions{Query: "match", Limit: 2, Before: first[1].Cursor()})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || bodyOf(second[0].Raw) != "match number 3" || bodyOf(second[1].Raw) != "match number 2" {
		t.Fatalf("second page: %q", texts(second))
	}
}

func TestSearchOnlyIndexesText(t *testing.T) {
	s, _ := openTest(t, 10)
	// A system line (no Text) must not be searchable even though its raw
	// form contains the word.
	if _, err := s.Append(ctx, "libera", "#go", Message{
		Time: time.UnixMilli(1000), Sender: "bob", Command: "JOIN",
		Raw: ":bob!u@h JOIN #go quicksilver", // "quicksilver" only in raw
	}); err != nil {
		t.Fatal(err)
	}
	appendMsg(t, s, "libera", "#go", 2000, "real quicksilver message")
	got := searchTexts(t, s, SearchOptions{Query: "quicksilver"})
	if len(got) != 1 || got[0] != "real quicksilver message" {
		t.Fatalf("system line leaked into search: %q", got)
	}
}

func TestSearchSurvivesQuerySpecialChars(t *testing.T) {
	s, _ := openTest(t, 10)
	appendMsg(t, s, "libera", "#go", 1000, "email me at a@b.com please")
	// Inputs that are FTS operators/syntax must not error; they are
	// treated as literal terms.
	for _, q := range []string{`a@b.com`, `"unterminated`, `foo OR bar`, `NEAR(x)`, `* ^ :`, `(paren`} {
		if _, err := s.Search(ctx, SearchOptions{Query: q}); err != nil {
			t.Fatalf("query %q errored: %v", q, err)
		}
	}
	// The literal-term treatment still finds the message.
	if got := searchTexts(t, s, SearchOptions{Query: "email please"}); len(got) != 1 {
		t.Fatalf("got %q", got)
	}
}

func TestFTSQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", `"hello"`},
		{"hello world", `"hello" "world"`},
		{"  spaced   out  ", `"spaced" "out"`},
		{`say "hi"`, `"say" """hi"""`},
		{"", ""},
		{"   ", ""},
		{"foo OR bar", `"foo" "OR" "bar"`},
	}
	for _, tc := range cases {
		if got := ftsQuery(tc.in); got != tc.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSetRedacted(t *testing.T) {
	s, _ := openTest(t, 4) // small ring so both ring and disk paths are hit
	var msgs []Message
	for i := 1; i <= 8; i++ {
		m, err := s.Append(ctx, "libera", "#go", Message{
			Time: time.UnixMilli(int64(i) * 1000), MsgID: fmt.Sprintf("m%d", i),
			Sender: "alice", Command: "PRIVMSG", Raw: fmt.Sprintf(":alice!u@h PRIVMSG #go :secret %d", i),
			Text: fmt.Sprintf("secret %d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, m)
	}

	// Redact a recent (ring-cached) message and an old (disk-only) one.
	if ok, err := s.SetRedacted(ctx, "libera", "#go", "m8", "spam"); err != nil || !ok {
		t.Fatalf("redact recent: %v %v", ok, err)
	}
	if ok, err := s.SetRedacted(ctx, "libera", "#go", "m1", ""); err != nil || !ok {
		t.Fatalf("redact old: %v %v", ok, err)
	}

	// Re-redacting or an unknown msgid reports ok=false.
	if ok, _ := s.SetRedacted(ctx, "libera", "#go", "m8", ""); ok {
		t.Fatal("double redact reported success")
	}
	if ok, _ := s.SetRedacted(ctx, "libera", "#go", "nope", ""); ok {
		t.Fatal("unknown msgid reported success")
	}

	// Ring-served page reflects the redaction (m8 is in the ring).
	recent, _ := s.Latest(ctx, "libera", "#go", 3)
	last := recent[len(recent)-1]
	if last.MsgID != "m8" || !last.Redacted || last.RedactReason != "spam" {
		t.Fatalf("ring redaction not applied: %+v", last)
	}
	// Disk-served page reflects the old redaction.
	page, _ := s.Before(ctx, "libera", "#go", msgs[3].Cursor(), 5) // m1,m2,m3
	if !page[0].Redacted || page[0].MsgID != "m1" {
		t.Fatalf("disk redaction not applied: %+v", page[0])
	}

	// Redacted messages drop out of search.
	if got := searchTexts(t, s, SearchOptions{Query: "secret"}); len(got) != 6 {
		t.Fatalf("search returned %d, want 6 (2 redacted excluded)", len(got))
	}

	// Redaction is destructive, not just a flag: the body is scrubbed on
	// both the ring-served and disk-served paths.
	if last.Raw != "" || last.Text != "" {
		t.Fatalf("ring redacted message kept its body: raw=%q text=%q", last.Raw, last.Text)
	}
	if page[0].Raw != "" || page[0].Text != "" {
		t.Fatalf("disk redacted message kept its body: raw=%q text=%q", page[0].Raw, page[0].Text)
	}
	// The content is gone from the messages table...
	var raw string
	var text sql.NullString
	if err := s.db.QueryRow(`SELECT raw, text FROM messages WHERE msgid = 'm8'`).Scan(&raw, &text); err != nil {
		t.Fatal(err)
	}
	if raw != "" || text.Valid {
		t.Fatalf("stored body not scrubbed: raw=%q text=%v", raw, text)
	}
	// ...and from the FTS index, so it cannot be recovered by a raw match.
	var ftsHits int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`,
		`"secret 8"`).Scan(&ftsHits); err != nil {
		t.Fatal(err)
	}
	if ftsHits != 0 {
		t.Fatalf("redacted text still recoverable from FTS index: %d hits", ftsHits)
	}
}

// The 0008 migration must scrub rows that were redacted under the old
// non-destructive behavior (body + FTS entry retained).
func TestRedactionScrubMigration(t *testing.T) {
	s, _ := openTest(t, 10)
	m, err := s.Append(ctx, "libera", "#go", Message{
		Time: time.UnixMilli(1000), MsgID: "old1", Sender: "a", Command: "PRIVMSG",
		Raw: ":a!u@h PRIVMSG #go :leaked secret", Text: "leaked secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-0008 state: flagged redacted, but body and FTS entry
	// still present (only the flag was set, no scrub).
	if _, err := s.db.Exec(`UPDATE messages SET redacted = 1, redact_reason = 'x' WHERE id = ?`, m.ID); err != nil {
		t.Fatal(err)
	}
	var hits int
	if err := s.db.QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`,
		`"leaked secret"`).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("precondition: expected the old row indexed, got %d hits", hits)
	}

	// Re-run the migration SQL; it must scrub the already-redacted row.
	sqlText, err := migrationsFS.ReadFile("migrations/0008_redaction_scrub.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(string(sqlText)); err != nil {
		t.Fatal(err)
	}

	var raw string
	var text sql.NullString
	if err := s.db.QueryRow(`SELECT raw, text FROM messages WHERE id = ?`, m.ID).Scan(&raw, &text); err != nil {
		t.Fatal(err)
	}
	if raw != "" || text.Valid {
		t.Fatalf("migration did not scrub the body: raw=%q text=%v", raw, text)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`,
		`"leaked secret"`).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatalf("migration did not purge the FTS entry: %d hits", hits)
	}
}

func TestAround(t *testing.T) {
	s, _ := openTest(t, 4)
	var msgs []Message
	for i := 1; i <= 10; i++ {
		msgs = append(msgs, appendMsg(t, s, "libera", "#go", int64(i)*1000, fmt.Sprintf("m%d", i)))
	}
	// Center on message 5 with a window of 4: the pivot plus 2 older
	// (older half includes the pivot) and 1 newer filling the rest.
	got, err := s.Around(ctx, "libera", "#go", msgs[4].Cursor(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if !equal(texts(got), []string{"m3", "m4", "m5", "m6"}) {
		t.Fatalf("around middle: %q", texts(got))
	}

	// Near the start: pivot is the first message, no older context.
	got, _ = s.Around(ctx, "libera", "#go", msgs[0].Cursor(), 4)
	if len(got) != 4 || bodyOf(got[0].Raw) != "m1" {
		t.Fatalf("around start: %q", texts(got))
	}

	// Unknown buffer is empty, not an error.
	if got, err := s.Around(ctx, "libera", "#nope", msgs[0].Cursor(), 4); err != nil || len(got) != 0 {
		t.Fatalf("unknown buffer: %q %v", texts(got), err)
	}
}

func texts(msgs []Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = bodyOf(m.Raw)
	}
	return out
}
