package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNetworkConfigCRUD(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()

	// Seed only fills an empty table.
	seeded, err := s.SeedNetworkConfigs(ctx, []NetworkConfig{{Name: "libera", Config: `{"addr":"a:1"}`}})
	if err != nil || !seeded {
		t.Fatalf("seed = %v, %v; want true, nil", seeded, err)
	}
	seeded, err = s.SeedNetworkConfigs(ctx, []NetworkConfig{{Name: "other", Config: `{}`}})
	if err != nil || seeded {
		t.Fatalf("second seed = %v, %v; want false, nil", seeded, err)
	}

	if err := s.PutNetworkConfig(ctx, "oftc", `{"addr":"b:2"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.PutNetworkConfig(ctx, "oftc", `{"addr":"b:3"}`); err != nil {
		t.Fatal(err) // upsert
	}
	configs, err := s.NetworkConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 2 || configs[0].Name != "libera" || configs[1].Config != `{"addr":"b:3"}` {
		t.Fatalf("configs = %+v", configs)
	}

	if err := s.DeleteNetwork(ctx, "libera"); err != nil {
		t.Fatal(err)
	}
	configs, _ = s.NetworkConfigs(ctx)
	if len(configs) != 1 {
		t.Fatalf("after delete: %+v", configs)
	}
}

func TestDeleteAndRenameNetworkData(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()

	msg := Message{Time: time.Now(), Sender: "a", Command: "PRIVMSG", Raw: ":a PRIVMSG #x :hi"}
	if _, err := s.Append(ctx, "netA", "#x", msg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "netB", "#y", msg); err != nil {
		t.Fatal(err)
	}
	if err := s.PutNetworkConfig(ctx, "netA", `{"addr":"a:1"}`); err != nil {
		t.Fatal(err)
	}

	// Rename atomically carries history and the definition to the new
	// name.
	if err := s.ReplaceNetworkConfig(ctx, "netA", "netC", `{"addr":"c:1"}`); err != nil {
		t.Fatal(err)
	}
	got, err := s.Latest(ctx, "netC", "#x", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("after rename: %v, %v", got, err)
	}
	configs, err := s.NetworkConfigs(ctx)
	if err != nil || len(configs) != 1 || configs[0].Name != "netC" {
		t.Fatalf("configs after rename = %+v, %v", configs, err)
	}
	// Renaming onto a name with existing history is refused, and the
	// failed transaction changes nothing: netC keeps its definition.
	if err := s.ReplaceNetworkConfig(ctx, "netC", "netB", `{"addr":"b:1"}`); err == nil {
		t.Fatal("rename onto existing data: want error")
	}
	configs, _ = s.NetworkConfigs(ctx)
	if len(configs) != 1 || configs[0].Name != "netC" || configs[0].Config != `{"addr":"c:1"}` {
		t.Fatalf("configs after refused rename = %+v", configs)
	}

	// Delete removes the definition and all rows for the network; other
	// networks keep theirs.
	if err := s.DeleteNetwork(ctx, "netC"); err != nil {
		t.Fatal(err)
	}
	bufs, err := s.Buffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 1 || bufs[0].Network != "netB" {
		t.Fatalf("buffers after delete = %+v", bufs)
	}
	// The deleted network's caches are gone: a fresh append recreates
	// cleanly rather than writing into a stale ring.
	if _, err := s.Append(ctx, "netC", "#x", msg); err != nil {
		t.Fatal(err)
	}
	got, err = s.Latest(ctx, "netC", "#x", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("after re-append: %v, %v", got, err)
	}
}

func TestAppendExistingAfterDeleteBuffer(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()

	msg := Message{Time: time.Now(), Sender: "AlteredParadox", Command: "PART", Raw: ":AlteredParadox PART #x"}
	if _, err := s.Append(ctx, "net", "#x", msg); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBuffer(ctx, "net", "#x"); err != nil {
		t.Fatal(err)
	}
	// The PART echo arriving after close must not resurrect the buffer.
	got, err := s.AppendExisting(ctx, "net", "#x", msg)
	if err != nil || got.ID != 0 {
		t.Fatalf("AppendExisting after delete = %+v, %v; want dropped", got, err)
	}
	bufs, err := s.Buffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 0 {
		t.Fatalf("buffer resurrected: %+v", bufs)
	}
	// Into an existing buffer it still lands normally.
	if _, err := s.Append(ctx, "net", "#y", msg); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AppendExisting(ctx, "net", "#y", msg); err != nil || got.ID == 0 {
		t.Fatalf("AppendExisting into live buffer = %+v, %v", got, err)
	}
}

// A read marker cannot be poisoned with an implausible future
// timestamp: values past the newest message (or the present) clamp.
func TestReadMarkerClamped(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	stored, err := s.Append(ctx, "net", "#x", Message{
		Time: past, Sender: "a", Command: "PRIVMSG", Raw: ":a PRIVMSG #x :hi",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Near-MaxInt64 clamps to now (not the message: reading a quiet
	// buffer "up to now" is legitimate).
	if err := s.SetReadMarker(ctx, "net", "#x", time.UnixMilli(1<<62)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadMarker(ctx, "net", "#x")
	if err != nil {
		t.Fatal(err)
	}
	if got.After(time.Now().Add(time.Minute)) {
		t.Fatalf("marker %v not clamped", got)
	}
	// The clamp was to "now", so a message arriving later still counts
	// as unread — the poison attempt bought nothing.
	if _, err := s.Append(ctx, "net", "#x", Message{
		Time: time.Now().Add(2 * time.Minute), Sender: "a", Command: "PRIVMSG", Raw: ":a PRIVMSG #x :later",
	}); err != nil {
		t.Fatal(err)
	}
	bufs, err := s.Buffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 1 || bufs[0].Unread != 1 {
		t.Fatalf("unread = %+v, want 1 (marker not poisoned)", bufs)
	}

	// Ordinary marker at the message's own time still works.
	if err := s.SetReadMarker(ctx, "net", "#y", stored.Time); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadMarker(ctx, "net", "#y"); !got.Equal(time.UnixMilli(stored.Time.UnixMilli())) {
		t.Fatalf("normal marker = %v, want %v", got, stored.Time)
	}
}

// FindBuffer matches under the supplied IRC casemapping — SQLite NOCASE
// (ASCII-only) would miss the rfc1459 []{} pairs.
func TestFindBufferFolds(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()

	fold := func(name string) string {
		b := []byte(strings.ToLower(name))
		for i, c := range b {
			switch c {
			case '[':
				b[i] = '{'
			case ']':
				b[i] = '}'
			case '\\':
				b[i] = '|'
			}
		}
		return string(b)
	}
	msg := Message{Time: time.Now(), Sender: "x", Command: "PRIVMSG", Raw: ":x PRIVMSG a :hi"}
	if _, err := s.Append(ctx, "net", "Pal{1}", msg); err != nil {
		t.Fatal(err)
	}
	name, ok, err := s.FindBuffer(ctx, "net", "pal[1]", fold)
	if err != nil || !ok || name != "Pal{1}" {
		t.Fatalf("FindBuffer = %q, %v, %v; want Pal{1}", name, ok, err)
	}
	if _, ok, _ := s.FindBuffer(ctx, "net", "unrelated", fold); ok {
		t.Fatal("unrelated name matched")
	}
}
