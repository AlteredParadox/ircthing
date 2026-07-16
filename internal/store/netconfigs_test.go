package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

	// Near-MaxInt64 clamps to now + a small skew tolerance, not to the
	// far-future value — so it cannot suppress real traffic forever.
	if err := s.SetReadMarker(ctx, "net", "#x", time.UnixMilli(1<<62)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadMarker(ctx, "net", "#x")
	if err != nil {
		t.Fatal(err)
	}
	if got.After(time.Now().Add(6 * time.Minute)) {
		t.Fatalf("marker %v not clamped to ~now", got)
	}
	// A message arriving well beyond the skew window is still unread —
	// the poison attempt could not mark future traffic as read.
	if _, err := s.Append(ctx, "net", "#x", Message{
		Time: time.Now().Add(30 * time.Minute), Sender: "a", Command: "PRIVMSG", Raw: ":a PRIVMSG #x :later",
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
	if _, err := s.Append(ctx, "net", "#y", Message{
		Time: stored.Time, Sender: "a", Command: "PRIVMSG", Raw: ":a PRIVMSG #y :hi",
	}); err != nil {
		t.Fatal(err)
	}
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

// AppendFolded resolves case-variant targets to one buffer even under
// concurrent first messages — the fold+create+append is one locked op.
func TestAppendFoldedConcurrent(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()
	fold := func(x string) string { return strings.ToLower(x) }

	var wg sync.WaitGroup
	for _, target := range []string{"#Go", "#go", "#GO", "#gO"} {
		wg.Add(1)
		go func(tg string) {
			defer wg.Done()
			_, _ = s.AppendFolded(ctx, "net", tg, fold, Message{
				Time: time.Now(), Sender: "a", Command: "PRIVMSG", Raw: ":a PRIVMSG " + tg + " :hi",
			})
		}(target)
	}
	wg.Wait()

	bufs, err := s.Buffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 1 {
		names := make([]string, len(bufs))
		for i, b := range bufs {
			names[i] = b.Target
		}
		t.Fatalf("concurrent case-variant appends made %d buffers: %v", len(bufs), names)
	}
}

// The database holds plaintext credentials, so a freshly created file
// must be 0600 regardless of umask, and an existing loose file is
// tightened on open.
func TestDatabasePermissions(t *testing.T) {
	old := syscall.Umask(0o022) // the common umask that would yield 0644
	defer syscall.Umask(old)

	dir := t.TempDir()
	path := filepath.Join(dir, "perm.db")

	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertMode := func(p string, want os.FileMode) {
		fi, err := os.Stat(p)
		if err != nil {
			return // sidecar may not exist; only assert when present
		}
		if got := fi.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %#o, want %#o", p, got, want)
		}
	}
	// Force the WAL sidecars into existence, then check everything.
	ctx := context.Background()
	if _, err := s.Append(ctx, "n", "#c", Message{Time: time.Now(), Sender: "a", Command: "PRIVMSG", Raw: ":a PRIVMSG #c :hi"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("new db mode = %#o, want 0600", got)
	}
	assertMode(path+"-wal", 0o600)
	assertMode(path+"-shm", 0o600)
	s.Close()

	// An existing world/group-readable db is tightened on open.
	loose := filepath.Join(dir, "loose.db")
	if f, err := os.OpenFile(loose, os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	s2, err := Open(loose, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	fi, _ = os.Stat(loose)
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("existing loose db mode after open = %#o, want 0600", got)
	}

	// A pre-existing loose WAL sidecar is tightened before SQLite opens
	// it (it can hold uncheckpointed credential rows).
	waldb := filepath.Join(dir, "wal.db")
	for _, p := range []string{waldb, waldb + "-wal", waldb + "-shm"} {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	s3, err := Open(waldb, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	for _, p := range []string{waldb, waldb + "-wal", waldb + "-shm"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode after open = %#o, want 0600", p, fi.Mode().Perm())
		}
	}
}

// tightenPath propagates a stat failure (rather than silently
// continuing) and treats a missing file as a no-op.
func TestTightenPathErrors(t *testing.T) {
	dir := t.TempDir()

	// Missing file: no-op.
	if err := tightenPath(filepath.Join(dir, "absent-wal")); err != nil {
		t.Fatalf("missing sidecar should be a no-op, got %v", err)
	}

	// Stat failure: a path whose "parent" is a regular file yields
	// ENOTDIR, which must propagate, not be swallowed.
	blocker := filepath.Join(dir, "blocker")
	if f, err := os.OpenFile(blocker, os.O_CREATE|os.O_WRONLY, 0o600); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	if err := tightenPath(filepath.Join(blocker, "child-wal")); err == nil {
		t.Fatal("stat error was swallowed")
	}
}
