package store

import (
	"database/sql"
	"strconv"
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

// Buffer creation from inbound traffic is bounded per network, so a
// server (or botnet) streaming distinct target/sender names cannot grow
// the store without limit.
func TestBufferCountCap(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()

	msg := Message{Time: time.Now(), Sender: "x", Command: "PRIVMSG", Raw: ":x PRIVMSG t :hi"}
	// Fill to the cap.
	for i := 0; i < maxBuffersPerNetwork; i++ {
		if _, err := s.Append(ctx, "net", "#c"+strconv.Itoa(i), msg); err != nil {
			t.Fatal(err)
		}
	}
	// Past the cap: creation is refused (message dropped, ID 0).
	got, err := s.Append(ctx, "net", "#over", msg)
	if err != nil || got.ID != 0 {
		t.Fatalf("Append past cap = %+v, %v; want dropped", got, err)
	}
	bufs, err := s.Buffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != maxBuffersPerNetwork {
		t.Fatalf("buffers = %d, want %d", len(bufs), maxBuffersPerNetwork)
	}
	// An existing buffer still accepts messages.
	if got, err := s.Append(ctx, "net", "#c0", msg); err != nil || got.ID == 0 {
		t.Fatalf("append to existing buffer at cap = %+v, %v", got, err)
	}
}

// AdoptOwnMsgID reconciles a replayed own message with its local
// placeholder so the msgid-bearing copy dedups instead of duplicating.
func TestAdoptOwnMsgID(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()

	local := time.Now()
	// Local no-msgid placeholder (persistOwn).
	if _, err := s.Append(ctx, "net", "#c", Message{
		Time: local, Sender: "me", Command: "PRIVMSG", Raw: ":me PRIVMSG #c :hello", Text: "hello",
	}); err != nil {
		t.Fatal(err)
	}
	// The server's later-timestamped copy carries a msgid.
	adopted, err := s.AdoptOwnMsgID(ctx, "net", "#c", "hello", "srv-msgid-1", nil, local.Add(-time.Minute).UnixMilli())
	if err != nil || !adopted {
		t.Fatalf("adopt = %v, %v; want true", adopted, err)
	}
	// The replayed copy now dedups (same msgid) instead of inserting.
	got, err := s.Append(ctx, "net", "#c", Message{
		Time: local.Add(time.Second), Sender: "me", Command: "PRIVMSG",
		MsgID: "srv-msgid-1", Raw: ":me PRIVMSG #c :hello", Text: "hello",
	})
	if err != nil || got.ID != 0 {
		t.Fatalf("replayed copy = %+v, %v; want deduped (ID 0)", got, err)
	}
	msgs, err := s.Latest(ctx, "net", "#c", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("scrollback has %d messages, want 1 (no duplicate)", len(msgs))
	}
	// No matching placeholder -> no adoption.
	if adopted, _ := s.AdoptOwnMsgID(ctx, "net", "#c", "different", "x", nil, 0); adopted {
		t.Fatal("adopted a non-matching row")
	}
}

// Identical repeated own messages must adopt msgids in send order, not
// reversed. Chathistory replays oldest-first and placeholders were stored
// oldest-first, so the earliest replay must pair with the earliest
// placeholder; a newest-first match swapped them, and — redaction being
// destructive — a later REDACT then scrubbed the wrong row.
func TestAdoptOwnMsgIDPairsInOrder(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()
	base := time.UnixMilli(1_700_000_000_000)
	p1, err := s.Append(ctx, "net", "#c", Message{Time: base, Sender: "me", Command: "PRIVMSG", Raw: ":me PRIVMSG #c :ok", Text: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.Append(ctx, "net", "#c", Message{Time: base.Add(5 * time.Second), Sender: "me", Command: "PRIVMSG", Raw: ":me PRIVMSG #c :ok", Text: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	since := base.Add(-time.Minute).UnixMilli()
	// Replay oldest-first: m1 then m2.
	if ok, _ := s.AdoptOwnMsgID(ctx, "net", "#c", "ok", "m1", nil, since); !ok {
		t.Fatal("adopt m1")
	}
	if ok, _ := s.AdoptOwnMsgID(ctx, "net", "#c", "ok", "m2", nil, since); !ok {
		t.Fatal("adopt m2")
	}
	var mid1, mid2 sql.NullString
	if err := s.db.QueryRow(`SELECT msgid FROM messages WHERE id=?`, p1.ID).Scan(&mid1); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT msgid FROM messages WHERE id=?`, p2.ID).Scan(&mid2); err != nil {
		t.Fatal(err)
	}
	if mid1.String != "m1" || mid2.String != "m2" {
		t.Fatalf("msgids paired in reverse: p1(older)=%q p2(newer)=%q, want m1/m2", mid1.String, mid2.String)
	}
	// Redacting m2 must destructively scrub the NEWER message (p2), not p1.
	if ok, _ := s.SetRedacted(ctx, "net", "#c", "m2", ""); !ok {
		t.Fatal("redact m2")
	}
	var raw1, raw2 string
	s.db.QueryRow(`SELECT raw FROM messages WHERE id=?`, p1.ID).Scan(&raw1)
	s.db.QueryRow(`SELECT raw FROM messages WHERE id=?`, p2.ID).Scan(&raw2)
	if raw2 != "" {
		t.Fatalf("REDACT m2 did not scrub p2 (raw=%q)", raw2)
	}
	if raw1 == "" {
		t.Fatal("REDACT m2 wrongly scrubbed p1 (the earlier message)")
	}
	// An overlapping replay re-delivering m1 is a no-op, not a UNIQUE error.
	if ok, err := s.AdoptOwnMsgID(ctx, "net", "#c", "ok", "m1", nil, since); ok || err != nil {
		t.Fatalf("re-adopt of an already-stamped msgid = (%v, %v), want (false, nil)", ok, err)
	}
}

// A replay whose target casing differs from the buffer's stored spelling
// still adopts, because AdoptOwnMsgID canonicalizes under fold — matching
// AppendFolded, which is how the placeholder buffer was created. Without
// this the adopt would miss and the own message would duplicate.
func TestAdoptOwnMsgIDCasingMismatch(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()
	fold := strings.ToLower // stand-in for the network casemapping

	local := time.Now()
	// Placeholder filed under the user-typed spelling "Bob".
	if _, err := s.AppendFolded(ctx, "net", "Bob", fold, Message{
		Time: local, Sender: "me", Command: "PRIVMSG", Raw: ":me PRIVMSG Bob :hi", Text: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	// The server replays it with canonical casing "bob".
	adopted, err := s.AdoptOwnMsgID(ctx, "net", "bob", "hi", "srv-1", fold, local.Add(-time.Minute).UnixMilli())
	if err != nil || !adopted {
		t.Fatalf("adopt across casing = %v, %v; want true", adopted, err)
	}
	got, err := s.AppendFolded(ctx, "net", "bob", fold, Message{
		Time: local.Add(time.Second), Sender: "me", Command: "PRIVMSG",
		MsgID: "srv-1", Raw: ":me PRIVMSG bob :hi", Text: "hi",
	})
	if err != nil || got.ID != 0 {
		t.Fatalf("replayed copy = %+v, %v; want deduped (ID 0)", got, err)
	}
	if msgs, _ := s.Latest(ctx, "net", "Bob", 10); len(msgs) != 1 {
		t.Fatalf("scrollback has %d messages, want 1 (no duplicate)", len(msgs))
	}
}

// AppendGuarded consults the guard atomically with the buffer existence
// check, passing whether the buffer exists. A create-only guard (!exists)
// vetoes a missing buffer but appends to an existing one; a straggler-drop
// guard (ignores exists, returns true) drops the append even to an existing
// buffer — closing the close_buffer resurrection race in the window before
// DeleteBuffer runs.
func TestAppendGuarded(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()
	msg := func(raw string) Message {
		return Message{Sender: "a", Command: "PRIVMSG", Raw: raw, Text: raw}
	}
	createOnly := func(exists bool) bool { return !exists }
	dropAlways := func(bool) bool { return true }
	permit := func(bool) bool { return false }

	// create-only guard vetoes creating a fresh buffer -> dropped.
	got, err := s.AppendGuarded(ctx, "net", "#closed", createOnly, msg("x"))
	if err != nil || got.ID != 0 {
		t.Fatalf("vetoed create of missing buffer = %+v, %v; want no-op (ID 0)", got, err)
	}
	if m, _ := s.Latest(ctx, "net", "#closed", 10); len(m) != 0 {
		t.Fatalf("buffer resurrected despite guard: %d rows", len(m))
	}

	// Guard permits creation -> buffer created.
	if got, err := s.AppendGuarded(ctx, "net", "#open", permit, msg("y")); err != nil || got.ID == 0 {
		t.Fatalf("permitted create = %+v, %v; want created", got, err)
	}

	// create-only guard appends to an EXISTING buffer (exists -> not vetoed).
	if got, err := s.AppendGuarded(ctx, "net", "#open", createOnly, msg("z")); err != nil || got.ID == 0 {
		t.Fatalf("create-only guard on existing buffer = %+v, %v; want appended", got, err)
	}
	if m, _ := s.Latest(ctx, "net", "#open", 10); len(m) != 2 {
		t.Fatalf("existing buffer has %d rows, want 2", len(m))
	}

	// A straggler-drop guard DROPS the append to the existing buffer — this
	// is the close_buffer race fix.
	if got, err := s.AppendGuarded(ctx, "net", "#open", dropAlways, msg("w")); err != nil || got.ID != 0 {
		t.Fatalf("drop guard on existing buffer = %+v, %v; want dropped (ID 0)", got, err)
	}
	if m, _ := s.Latest(ctx, "net", "#open", 10); len(m) != 2 {
		t.Fatalf("existing buffer has %d rows after dropped straggler, want 2", len(m))
	}
}
