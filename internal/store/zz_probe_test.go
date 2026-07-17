package store

import (
	"context"
	"testing"
	"time"
)

func TestProbeFTSArbitraryInput(t *testing.T) {
	s, err := Open(":memory:", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	// Store one indexed message.
	if _, err := s.Append(ctx, "net", "#c", Message{Sender: "a", Command: "PRIVMSG", Raw: "x", Text: "hello world", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	inputs := []string{"!", "@#$", "* *", "NEAR(", "\"", "()", "AND", "OR", "^foo", "col:val", "a-b", "'", "-", "+"}
	for _, in := range inputs {
		_, err := s.Search(ctx, SearchOptions{Query: in})
		if err != nil {
			t.Errorf("Search(%q) errored (comment claims arbitrary input can never error): %v", in, err)
		}
	}
}

func TestProbeRedactionFTS(t *testing.T) {
	s, err := Open(":memory:", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	m, err := s.Append(ctx, "net", "#c", Message{MsgID: "m1", Sender: "a", Command: "PRIVMSG", Raw: "@msgid=m1 :a PRIVMSG #c :secret", Text: "secretword", Time: time.Now()})
	if err != nil || m.ID == 0 {
		t.Fatalf("append: %v id=%d", err, m.ID)
	}
	// Confirm searchable.
	res, _ := s.Search(ctx, SearchOptions{Query: "secretword"})
	t.Logf("before redaction, matches=%d", len(res))
	ok, err := s.SetRedacted(ctx, "net", "#c", "m1", "spam")
	if err != nil || !ok {
		t.Fatalf("redact: %v ok=%v", err, ok)
	}
	// After redaction the body must be gone from FTS.
	res, _ = s.Search(ctx, SearchOptions{Query: "secretword"})
	if len(res) != 0 {
		t.Errorf("FTS LEAK: redacted body still searchable, matches=%d", len(res))
	}
	// Now delete the buffer (cascade) and ensure no FTS corruption panic/err.
	if err := s.DeleteBuffer(ctx, "net", "#c"); err != nil {
		t.Fatalf("delete buffer: %v", err)
	}
}
