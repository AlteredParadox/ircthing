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

// Package store provides SQLite persistence (WAL mode): messages,
// networks, buffers, and read markers, with a bounded in-memory hot
// scrollback ring per buffer in front of the database. Schema changes go
// through migrations/ — never mutate schema in place.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (approved; CGO-free)
)

const (
	// DefaultRingSize is the per-buffer hot scrollback bound: with the
	// 50-channel scenario from the memory target this keeps hot history
	// around 10k messages total.
	DefaultRingSize      = 200
	DefaultPageSize      = 100
	MaxPageSize          = 500
	MaxRetentionDays     = 36500
	MaxRetentionMessages = 1_000_000_000
	// maxStoredMessageBytes caps a single message's Raw/Text. Generous over
	// any legitimate IRC line (512 B content + ~8 KiB tags), it bounds the
	// pathological case of a hostile server raising LINELEN and streaming
	// huge lines into the hot rings.
	maxStoredMessageBytes = 16384
	// maxStoredFieldBytes caps a single server-controlled identifier field
	// (Sender/MsgID/redaction reason and the buffer target). Real nicks,
	// channel names and msgids are far under this; the cap plus a detaching
	// copy (see append) stops an oversized field from a hostile server
	// pinning a whole 64 KiB parsed line alive in a hot ring.
	maxStoredFieldBytes = 512
	// maxHotRingBytes is the global budget for all resident hot rings combined.
	// The per-buffer ring count (DefaultRingSize) and per-message clamps bound
	// ONE ring, but nothing bounded the total across buffers: a hostile server
	// opening many buffers of max-size messages could hold GBs resident. Above
	// this the least-recently-used rings are evicted (re-warmed from SQLite on
	// next access). Still ~16× the ~50-buffer design point (~1 MB of hot rings),
	// so it never bites legitimate use; sized to leave headroom for SQLite
	// write-churn so even an adversarial flood keeps process RSS under the 72 MB
	// target (see the adversarial memcheck scenario).
	maxHotRingBytes = 16 << 20 // 16 MiB
)

// clampUTF8 truncates s to at most max bytes, trimming a trailing partial
// rune so the result stays valid UTF-8.
func clampUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size != 1 {
			break // last rune is complete (a real U+FFFD decodes with size 3)
		}
		s = s[:len(s)-1]
	}
	return s
}

// ClampMsgID bounds a msgid to the same field limit append stores it under, so a
// LIVE redact event carries the exact id the stored (and client-displayed)
// message was indexed by — a >512-byte msgid would otherwise be broadcast in
// full and never match its truncated tombstone.
//
// A >512-byte msgid is not impossible, only abnormal: ordinary servers use short
// ids, but message-tag framing can represent longer ones, so a HOSTILE server
// could send them. Accepted residual: two distinct msgids sharing their first 512
// bytes clamp to the same key, so (a) the per-buffer msgid dedup (INSERT OR
// IGNORE) drops the LATER colliding message — a real history/display gap for
// that line — and (b) a redaction addressed to either id tombstones the one
// surviving row. Both require a server deliberately crafting near-identical
// oversized ids, and a server can already fabricate or withhold history
// outright, so we accept this rather than store unbounded ids.
func ClampMsgID(s string) string { return clampUTF8(s, maxStoredFieldBytes) }

// Message is one stored IRC message. ID and Network/Target are assigned
// by the store on append.
type Message struct {
	ID      int64
	Network string
	Target  string
	Time    time.Time
	MsgID   string // IRCv3 msgid tag, "" when absent
	Sender  string // prefix name (nick or server)
	Command string
	Raw     string // full IRC line including tags
	// Text is the searchable message body (PRIVMSG/NOTICE content, CTCP
	// ACTION unwrapped), set by the hub. Empty for lines that are not
	// indexed for search (system events, non-ACTION CTCP).
	Text string
	// Redacted marks a message deleted via draft/message-redaction; the
	// row is kept as a tombstone. RedactReason is optional.
	Redacted     bool
	RedactReason string
}

// Cursor is a position in a buffer's history: unix-millisecond timestamp
// plus row id as the tiebreaker. Pagination is exclusive of the cursor in
// both directions (matching chathistory BEFORE/AFTER semantics). For a
// pure-timestamp cursor use CursorAtTime.
type Cursor struct {
	TS int64
	ID int64
}

func (m Message) Cursor() Cursor {
	return Cursor{TS: m.Time.UnixMilli(), ID: m.ID}
}

// CursorAtTime positions before the first message at t: Before(c) returns
// only strictly older messages, After(c) includes messages stamped exactly t.
func CursorAtTime(t time.Time) Cursor {
	return Cursor{TS: t.UnixMilli()}
}

var maxCursor = Cursor{TS: math.MaxInt64, ID: math.MaxInt64}

// defaultUserID is the user seeded by the initial migration. The schema
// is fully user-scoped (users own networks; everything below follows),
// but the application runs single-user until auth lands in internal/api,
// so the store pins this id rather than threading a user through the API.
const defaultUserID = 1

// markerSkewMs bounds how far ahead of now a read marker may be set,
// tolerating clock skew while preventing a future-dated message from
// poisoning the never-regressing marker.
const markerSkewMs = 5 * 60 * 1000

// maxBuffersPerNetwork bounds buffers auto-created from inbound traffic,
// so server-controlled target/sender names cannot grow the store without
// limit (mirrors the manager-side server-fed caps).
const maxBuffersPerNetwork = 4096

var ErrMsgIDNotFound = errors.New("store: msgid not found")

type Options struct {
	// RingSize bounds the per-buffer in-memory scrollback.
	// 0 means DefaultRingSize.
	RingSize int
	// RetentionDays prunes messages older than this many days; 0 disables
	// age-based pruning. RetentionMaxMessages keeps only the newest N
	// messages per buffer; 0 disables count-based pruning. When either is
	// set, Open starts an hourly background pruner (stopped by Close).
	RetentionDays        int
	RetentionMaxMessages int
}

// Store is safe for concurrent use. One coarse mutex guards the rings and
// caches; at IRC message rates lock contention is a non-issue and this
// keeps the invariants easy to reason about.
type Store struct {
	db           *sql.DB
	ringSize     int
	maxRingBytes int // global hot-ring byte budget; resident rings are LRU-evicted above it
	retention    retentionPolicy

	mu        sync.Mutex
	networks  map[string]int64
	buffers   map[bufKey]int64
	rings     map[int64]*ring
	ringBytes int64  // running sum of every resident ring's bytes (kept in step under mu)
	accessSeq uint64 // monotonic LRU clock, stamped onto a ring by touchRing
	stats     struct {
		ringPages, dbPages, ringEvictions int // observability for tests
	}

	stopPruner   chan struct{}      // closed by Close to end the pruner
	pruneNow     chan struct{}      // buffered(1) nudge: prune promptly after a policy change
	prunerCancel context.CancelFunc // cancels an in-flight chunked prune on Close
	prunerDone   sync.WaitGroup     // waits for the pruner goroutine to exit
}

// secureDBFile ensures the database file (and any pre-existing WAL/SHM
// sidecars) are 0600 before SQLite opens them: the main file is created
// with O_EXCL if absent so we set the mode ourselves rather than the
// umask, and an existing group/world-accessible file is tightened. A
// loose file that cannot be tightened fails the open. In-memory
// databases have no file and are skipped.
// encodeDBPath percent-encodes the characters SQLite's file: URI parser
// decodes (%, ?, #) so the file it opens is exactly the literal path
// secureDBFile hardened — otherwise "a%3fb.db" would chmod that literal name
// while SQLite decoded it to "a?b.db" and created THAT one under the umask.
// '%' must be listed first so its replacement isn't itself re-encoded (the
// Replacer scans once, non-overlapping).
func encodeDBPath(path string) string {
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
}

func secureDBFile(path string) error {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file::memory:") {
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("store: creating database %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("store: stat database %s: %w", path, err)
	}
	// The main file (if it pre-existed loose) and any leftover sidecars
	// from a previous run — a WAL can hold uncheckpointed rows — before
	// SQLite reads them.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := tightenPath(p); err != nil {
			return err
		}
	}
	return nil
}

// tightenPath chmods p to 0600 when it exists and is group- or
// world-accessible, propagating stat/chmod failures rather than leaving
// a credential-bearing file readable. A missing file is not an error.
func tightenPath(p string) error {
	fi, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: stat %s: %w", p, err)
	}
	perm := fi.Mode().Perm()
	if perm&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(p, 0o600); err != nil {
		return fmt.Errorf("store: %s is group/world-accessible (%#o) and could not be tightened: %w", p, perm, err)
	}
	log.Printf("store: tightened permissions on %s from %#o to 0600", p, perm)
	return nil
}

type bufKey struct{ network, target string }

// Open opens (creating if needed) the database at path and applies any
// pending migrations. WAL mode per the architecture; NORMAL synchronous is
// the documented safe pairing with WAL.
func Open(path string, opts Options) (*Store, error) {
	if err := ValidateRetention(opts.RetentionDays, opts.RetentionMaxMessages); err != nil {
		return nil, err
	}
	if opts.RingSize <= 0 {
		opts.RingSize = DefaultRingSize
	}
	// The database holds plaintext network credentials (server, proxy,
	// SASL passwords) and private message history, so it must never be
	// group- or world-readable. Pre-create it 0600 before SQLite touches
	// it — independent of the process umask, which at the common 022
	// would otherwise leave a new file at 0644 — and tighten an existing
	// loose file.
	if err := secureDBFile(path); err != nil {
		return nil, err
	}
	dsn := "file:" + encodeDBPath(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		// secure_delete zeroes freed content on delete/update rather than
		// leaving it in free pages, so a redaction (destructive scrub) or a
		// retention delete does not leave recoverable bytes in the file.
		"&_pragma=secure_delete(on)" +
		// INCREMENTAL auto_vacuum on a fresh database; existing ones are
		// converted below. Freed pages (retention/redaction deletes) then go
		// to the freelist and are returned to the OS by incremental_vacuum
		// (run after each prune) instead of the file only ever growing.
		"&_pragma=auto_vacuum(incremental)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Single-connection limitation: this serializes ALL database access —
	// writes and any reads that miss the rings. Deliberate for now: it
	// sidesteps SQLITE_BUSY handling entirely, and at IRC message rates
	// the connection is idle almost always. If bulk reads (search, large
	// history fetches) ever contend with the write path, the successor is
	// a split pool: keep this connection as the sole writer and add a
	// small read-only pool (2–4 connections, PRAGMA query_only) — WAL
	// already lets those readers run concurrently with the writer.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	// The DSN pragma above only sets auto_vacuum on a FRESH database (before
	// any table exists). An existing database created without it keeps mode
	// 0/1 until a VACUUM rewrites the file, so convert it once here. This is
	// a one-time rewrite on first upgrade (skipped forever after).
	if err := ensureIncrementalVacuum(db); err != nil {
		db.Close()
		return nil, err
	}
	// The WAL/SHM sidecars are created during migration's first write and
	// inherit the db file's mode on unix; re-check them and fail closed
	// if any is loose and cannot be tightened — a group/world-readable
	// WAL carries the same credentials as the main file.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := tightenPath(path + suffix); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &Store{
		db:           db,
		ringSize:     opts.RingSize,
		maxRingBytes: maxHotRingBytes,
		retention:    retentionPolicy{days: opts.RetentionDays, maxPerBuffer: opts.RetentionMaxMessages},
		networks:     make(map[string]int64),
		buffers:      make(map[bufKey]int64),
		rings:        make(map[int64]*ring),
	}
	// The settings table (seeded from config on first run) is authoritative
	// for retention, so a UI change survives a restart.
	if err := s.loadRetention(context.Background(), opts); err != nil {
		db.Close()
		return nil, err
	}
	// Always run the pruner: retention is now runtime-settable, and pruneOnce
	// is a cheap no-op while both dimensions are 0.
	s.startPruner(pruneInterval)
	return s, nil
}

// ValidateRetention is shared by config, persisted-settings, and runtime API
// paths so every ingress enforces the same overflow-safe bounds.
func ValidateRetention(days, maxMessages int) error {
	if days < 0 || days > MaxRetentionDays || maxMessages < 0 || maxMessages > MaxRetentionMessages {
		return fmt.Errorf("store: retention out of range (days 0..%d, messages 0..%d)", MaxRetentionDays, MaxRetentionMessages)
	}
	return nil
}

// ensureIncrementalVacuum converts a pre-existing database to INCREMENTAL
// auto_vacuum (mode 2) when it isn't already, so freed pages can later be
// returned to the OS by incremental_vacuum. Switching the mode requires a
// full VACUUM — a one-time file rewrite on the first open after upgrade,
// skipped forever after.
func ensureIncrementalVacuum(db *sql.DB) error {
	var mode int
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return err
	}
	if mode == 2 {
		return nil
	}
	if _, err := db.Exec(`PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
		return err
	}
	if _, err := db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("store: converting to incremental auto_vacuum: %w", err)
	}
	log.Printf("store: converted database to incremental auto_vacuum")
	return nil
}

func (s *Store) Close() error {
	if s.stopPruner != nil {
		s.prunerCancel() // interrupt an in-flight chunked prune so Close doesn't block on it
		close(s.stopPruner)
		s.prunerDone.Wait()
		s.stopPruner = nil
	}
	return s.db.Close()
}

// Append persists m to the (network, target) buffer, creating network and
// buffer rows as needed, and returns the message with its assigned ID.
// A zero Time is stamped with the current time. Appends are idempotent on
// msgid: if the buffer already holds a message with the same msgid
// (chathistory backfill overlapping stored history), nothing is written
// and the zero Message (ID 0) is returned — callers use that to skip
// broadcasting.
func (s *Store) Append(ctx context.Context, network, target string, m Message) (Message, error) {
	msg, _, err := s.append(ctx, network, target, m, appendOpts{create: true, unarchive: true})
	return msg, err
}

// AppendExisting is Append minus buffer creation: the message is
// silently dropped (ID 0) when no buffer exists. Used for our own PART
// echo, which must not resurrect a buffer the user just closed — the
// close_buffer delete and the PART echo race, and both orders must end
// with the buffer gone.
func (s *Store) AppendExisting(ctx context.Context, network, target string, m Message) (Message, error) {
	msg, _, err := s.append(ctx, network, target, m, appendOpts{unarchive: true})
	return msg, err
}

// AppendGuarded is Append with an atomic guard consulted UNDER the store
// lock, together with the buffer existence check: if guardCreate(exists)
// returns true the message is dropped (ID 0). This closes the close_buffer
// resurrection race without a check-then-act split across two locks. Pass
// `!exists` to veto only buffer creation (a straggler must not re-open a
// closed buffer), or ignore exists and veto unconditionally to also DROP a
// straggler appended to a buffer not yet deleted (so it cannot broadcast a
// live event that resurrects the buffer on clients).
func (s *Store) AppendGuarded(ctx context.Context, network, target string, guardCreate func(exists bool) bool, m Message) (Message, error) {
	msg, _, err := s.append(ctx, network, target, m, appendOpts{create: true, guardCreate: guardCreate, unarchive: true})
	return msg, err
}

// AppendFoldedGuarded is AppendFolded plus the AppendGuarded append guard.
func (s *Store) AppendFoldedGuarded(ctx context.Context, network, target string, fold func(string) string, guardCreate func(exists bool) bool, m Message) (Message, error) {
	msg, _, err := s.append(ctx, network, target, m, appendOpts{create: true, fold: fold, guardCreate: guardCreate, unarchive: true})
	return msg, err
}

// AppendFoldedGuardedArchive is AppendFoldedGuarded plus explicit control
// over an archived (close_buffer purge:false) buffer. unarchive says whether
// this message is real conversation: true clears the buffer's archived flag
// so it resurfaces in Buffers/RecentBuffers; false (membership fan-out —
// JOIN/PART/QUIT/NICK/MODE) leaves the flag alone. The second result reports
// whether the buffer is STILL archived after a successful append — the hub
// then persists the line without publishing it, so a straggler (the PART
// echo racing a purge:false close) cannot resurrect the buffer on clients.
// The flag/report and the insert happen under one hold of the store lock.
func (s *Store) AppendFoldedGuardedArchive(ctx context.Context, network, target string, fold func(string) string, guardCreate func(exists bool) bool, unarchive bool, m Message) (Message, bool, error) {
	return s.append(ctx, network, target, m, appendOpts{create: true, fold: fold, guardCreate: guardCreate, unarchive: unarchive})
}

// AppendFolded resolves target to its canonical stored spelling under
// fold and appends in one locked operation, so two concurrent first
// messages for case-equivalent targets (a browser send and an incoming
// IRC event run on independent goroutines) cannot each decide no buffer
// exists and create separate rows. m.Target is set to the resolved name.
func (s *Store) AppendFolded(ctx context.Context, network, target string, fold func(string) string, m Message) (Message, error) {
	msg, _, err := s.append(ctx, network, target, m, appendOpts{create: true, fold: fold, unarchive: true})
	return msg, err
}

// nullString maps "" to a SQL NULL (an empty text/msgid must not be
// stored or indexed) and any other value to itself.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// appendVetoed reports whether the caller's guard vetoes this append. The
// guard is told whether the target buffer already exists, and the existence
// check + guard run together under s.mu (the caller holds it) so a
// concurrent DeleteBuffer cannot race the decision. A create-only guard
// returns !exists (veto only a would-be-new buffer). A straggler-drop guard
// ignores exists and vetoes whenever the buffer was just closed — dropping a
// late line even in the window before DeleteBuffer removes the buffer, so a
// straggler can never broadcast an event that resurrects it on clients.
func (s *Store) appendVetoed(ctx context.Context, network, target string, guard func(exists bool) bool) (bool, error) {
	id, err := s.bufferID(ctx, network, target, false)
	if err != nil {
		return false, err
	}
	return guard(id != 0), nil
}

// appendOpts carries append's per-call-site policy: how the target resolves,
// whether a missing buffer may be created, whether a guard may veto the write,
// and how the archived flag settles. See the exported Append* wrappers for
// what each combination means.
type appendOpts struct {
	create      bool                   // create the buffer when absent
	fold        func(string) string    // canonical-spelling resolver; nil uses target as-is
	guardCreate func(exists bool) bool // atomic veto under s.mu; nil disables the guard
	unarchive   bool                   // real conversation clears the archived flag
}

func (s *Store) append(ctx context.Context, network, target string, m Message, opts appendOpts) (Message, bool, error) {
	if network == "" || target == "" {
		return Message{}, false, errors.New("store: network and target must be non-empty")
	}
	if m.Time.IsZero() {
		m.Time = time.Now()
	}
	// Bound a single message's stored bytes AND detach every retained string
	// from its backing array. irc.v4 slices each field out of one per-line
	// buffer (up to ~64 KiB once a server raises LINELEN), so a short Sender
	// or MsgID would otherwise pin the whole line alive in the hot ring —
	// strings.Clone copies just the bounded bytes and lets the line be GC'd.
	// Legitimate IRC content is well under these caps, so real messages are
	// untouched.
	m.Raw = strings.Clone(clampUTF8(m.Raw, maxStoredMessageBytes))
	m.Text = strings.Clone(clampUTF8(m.Text, maxStoredMessageBytes))
	m.Sender = strings.Clone(clampUTF8(m.Sender, maxStoredFieldBytes))
	m.MsgID = strings.Clone(clampUTF8(m.MsgID, maxStoredFieldBytes))
	// Command is also a substring of the parsed line (irc.v4 returns the raw
	// uppercase token unchanged), so a 7-byte "PRIVMSG" would pin the whole
	// 64 KiB line — clone it too.
	m.Command = strings.Clone(clampUTF8(m.Command, maxStoredFieldBytes))
	m.RedactReason = strings.Clone(clampUTF8(m.RedactReason, maxStoredFieldBytes))
	target = strings.Clone(clampUTF8(target, maxStoredFieldBytes))
	s.mu.Lock()
	defer s.mu.Unlock()

	if opts.fold != nil {
		target, _ = s.canonicalLocked(ctx, network, target, opts.fold)
	}
	m.Network, m.Target = network, target
	if opts.guardCreate != nil {
		blocked, err := s.appendVetoed(ctx, network, target, opts.guardCreate)
		if err != nil {
			return Message{}, false, err
		}
		if blocked {
			return Message{}, false, nil
		}
	}
	bufID, r, err := s.bufferAndRing(ctx, network, target, opts.create)
	if err != nil {
		return Message{}, false, err
	}
	if bufID == 0 {
		return Message{}, false, nil // no such buffer and create is off
	}
	// Insert and archive settlement commit as one transaction: a message
	// must never become durable with its buffer's archived flag unsettled,
	// because a retry of the same msgid deduplicates before settlement and
	// would leave the buffer hidden until unrelated traffic arrived.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, false, err
	}
	msg, err := s.insertTx(ctx, tx, bufID, m)
	if err != nil || msg.ID == 0 {
		// Nothing new was stored (error, or a duplicate msgid from overlapping
		// backfill) — an already-known message must not un-archive either.
		tx.Rollback()
		return msg, false, err
	}
	stillArchived, err := s.applyArchiveTx(ctx, tx, bufID, opts.unarchive)
	if err != nil {
		tx.Rollback()
		return Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, false, err
	}
	// Hot ring only after commit: the ring must never hold a message the
	// database rolled back.
	s.ringBytes += int64(r.insert(msg))
	s.touchRing(r)
	s.evictRings(bufID)
	return msg, stillArchived, nil
}

// applyArchiveTx settles a just-appended message against the buffer's
// archived flag, inside the append transaction (caller holds s.mu). Real
// conversation (unarchive true) clears the flag so the buffer resurfaces in
// Buffers/RecentBuffers with its full history; membership fan-out (unarchive
// false) leaves it hidden and reports that, so the hub can persist the line
// without publishing an event that would resurrect the buffer on clients. An
// unarchived buffer reports false on both paths for free: the UPDATE matches
// no row, the SELECT reads 0.
func (s *Store) applyArchiveTx(ctx context.Context, tx *sql.Tx, bufID int64, unarchive bool) (bool, error) {
	if unarchive {
		_, err := tx.ExecContext(ctx,
			`UPDATE buffers SET archived = 0 WHERE id = ? AND archived = 1`, bufID)
		return false, err
	}
	var archived int64
	err := tx.QueryRowContext(ctx,
		`SELECT archived FROM buffers WHERE id = ?`, bufID).Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // buffer row gone: nothing left to resurrect
	}
	return archived != 0, err
}

// insertTx writes m into the buffer inside the append transaction (caller
// holds s.mu); the hot ring is the caller's job, after commit. A dropped
// INSERT OR IGNORE (duplicate msgid) returns the zero Message.
func (s *Store) insertTx(ctx context.Context, tx *sql.Tx, bufID int64, m Message) (Message, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO messages (buffer_id, ts, msgid, sender, command, raw, text) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		bufID, m.Time.UnixMilli(), nullString(m.MsgID), m.Sender, m.Command, m.Raw, nullString(m.Text))
	if err != nil {
		return Message{}, fmt.Errorf("store: append: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return Message{}, nil // duplicate msgid: already stored
	}
	m.ID, err = res.LastInsertId()
	if err != nil {
		return Message{}, err
	}
	return m, nil
}

// SetRedacted marks the message with the given msgid in a buffer as
// deleted (draft/message-redaction). It returns ok=false if no such
// (un-redacted) message exists, so the caller only announces real
// redactions. The hot ring is updated in step with the database.
func (s *Store) SetRedacted(ctx context.Context, network, target, msgid, reason string) (bool, error) {
	if msgid == "" {
		return false, nil
	}
	// Match the msgid the SAME way append clamped it before storing (512 bytes),
	// or a REDACT for a >512-byte msgid would search the full value and never
	// find the truncated-stored row. Real msgids are short opaque tokens; the
	// residual (two msgids sharing a 512-byte prefix redact together) needs a
	// hostile server minting such collisions.
	msgid = clampUTF8(msgid, maxStoredFieldBytes)
	s.mu.Lock()
	defer s.mu.Unlock()

	bufID, err := s.bufferID(ctx, network, target, false)
	if err != nil || bufID == 0 {
		return false, err
	}
	// Bound + detach the server-supplied reason like an append field: it is
	// written to SQLite and the hot ring, so an oversized reason would bypass
	// the append-time clamps and pin/retain the parsed line.
	reason = strings.Clone(clampUTF8(reason, maxStoredFieldBytes))
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	// Redaction is destructive at the application layer: locate the row and
	// its indexed body, purge the FTS entry, then scrub raw/text — keeping
	// only the tombstone (sender/time/command + redacted flag + reason). The
	// content is then gone from queries, search, the hot ring, and the wire.
	// This is NOT forensic erasure: freed bytes/tokens may still persist in
	// SQLite free pages, FTS segments, and WAL frames until a vacuum, and in
	// any existing backups (enable PRAGMA secure_delete / FTS5 'secure-delete'
	// if that matters for the deployment).
	var id int64
	var text sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT id, text FROM messages
		 WHERE buffer_id = ? AND msgid = ? AND redacted = 0
		 ORDER BY id DESC LIMIT 1`, bufID, msgid).Scan(&id, &text)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	// External-content FTS has no update trigger, so it must be told the
	// old text explicitly before we blank it (see 0003_fts / 0008).
	if text.Valid && text.String != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages_fts (messages_fts, rowid, text) VALUES ('delete', ?, ?)`,
			id, text.String); err != nil {
			tx.Rollback()
			return false, err
		}
	}
	// raw is NOT NULL, so blank it rather than nulling; text is nullable.
	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET raw = '', text = NULL, redacted = 1, redact_reason = ?
		 WHERE id = ?`, reasonArg, id); err != nil {
		tx.Rollback()
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if r, ok := s.rings[bufID]; ok {
		// Usually a net decrease (Raw/Text freed), but a short message
		// tombstoned with a long reason grows the ring — enforce the
		// budget either way.
		s.ringBytes += int64(r.redact(msgid, reason))
		s.touchRing(r)
		s.evictRings(bufID)
	}
	return true, nil
}

// OwnMsg identifies one of our own sent messages for chathistory adoption:
// the buffer it lives in (Network/Target), the Sender and Text to match a
// no-msgid placeholder against, the MsgID to stamp on, and the SinceMs lower
// bound on the candidate's timestamp.
type OwnMsg struct {
	Network string
	Target  string
	Sender  string
	Text    string
	MsgID   string
	SinceMs int64
}

// AdoptOwnMsgID reconciles a chathistory-replayed copy of one of our own
// messages with the local no-msgid placeholder persistOwn stored: it
// finds the OLDEST msgid-less row in the buffer with matching text (newer
// than sinceMs) and stamps the server's msgid onto it. Chathistory replays
// oldest-first, and placeholders were stored in send order, so pairing the
// earliest replay with the earliest unstamped placeholder keeps identical
// repeated messages in order — a newest-first match would stamp N identical
// sends in REVERSE, and (since redaction is destructive) a later REDACT
// would then scrub the wrong row. The caller then relies on the normal
// (buffer_id, msgid) dedup to drop the replayed insert, so a no-echo-message
// + chathistory server does not duplicate own messages after a reconnect.
// Reports whether it adopted.
//
// target is resolved to its canonical stored spelling under fold (as
// AppendFolded does), so a replay carrying different-but-case-equivalent
// casing than the buffer's stored spelling still finds the placeholder —
// otherwise the adopt misses, the insert lands with its own msgid, and the
// own message duplicates.
func (s *Store) AdoptOwnMsgID(ctx context.Context, msg OwnMsg, fold func(string) string) (bool, error) {
	if msg.MsgID == "" || msg.Text == "" || msg.Sender == "" {
		return false, nil
	}
	// Clamp+detach the server-supplied msgid exactly as append does: it is
	// written to the row AND the hot ring, so an oversized one would bypass
	// the append-time defenses (pinning the parsed line, under-counting the
	// budget) — and, stamped full-length here while a later replayed INSERT
	// clamps its own msgid to 512, would defeat the (buffer_id, msgid) dedup
	// this function exists to provide. Text is clamped to the same bound the
	// placeholder was stored under so the `text = ?` match can succeed for a
	// long (multiline, no-echo) own message; it is only a query parameter, so
	// it need not be cloned.
	msg.MsgID = strings.Clone(clampUTF8(msg.MsgID, maxStoredFieldBytes))
	msg.Text = clampUTF8(msg.Text, maxStoredMessageBytes)
	s.mu.Lock()
	defer s.mu.Unlock()

	target := msg.Target
	if fold != nil {
		target, _ = s.canonicalLocked(ctx, msg.Network, target, fold)
	}
	bufID, err := s.bufferID(ctx, msg.Network, target, false)
	if err != nil || bufID == 0 {
		return false, err
	}
	// An overlapping chathistory range can re-deliver a msgid we already
	// stamped; adopting again would hit the (buffer_id, msgid) unique index.
	// Treat that as a no-op.
	var seen int
	switch err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM messages WHERE buffer_id = ? AND msgid = ? LIMIT 1`, bufID, msg.MsgID).Scan(&seen); {
	case err == nil:
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	// Match on sender too: on a no-msgid server another user's message with
	// identical text is also a msgid-less candidate, and stamping our msgid
	// onto their row would mis-attribute (and mis-redact) it.
	var id int64
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM messages
		 WHERE buffer_id = ? AND msgid IS NULL AND sender = ? AND text = ? AND ts >= ?
		 ORDER BY ts ASC, id ASC LIMIT 1`, bufID, msg.Sender, msg.Text, msg.SinceMs).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE messages SET msgid = ? WHERE id = ?`, msg.MsgID, id); err != nil {
		return false, err
	}
	if r, ok := s.rings[bufID]; ok {
		s.ringBytes += int64(r.adoptMsgID(id, msg.MsgID))
		s.touchRing(r)
		s.evictRings(bufID)
	}
	return true, nil
}

// Latest returns the newest messages of a buffer, ascending.
func (s *Store) Latest(ctx context.Context, network, target string, limit int) ([]Message, error) {
	return s.Before(ctx, network, target, maxCursor, limit)
}

// Before returns up to limit messages strictly older than c, ascending.
// An unknown buffer yields an empty page, not an error.
func (s *Store) Before(ctx context.Context, network, target string, c Cursor, limit int) ([]Message, error) {
	limit = clampLimit(limit)
	s.mu.Lock()
	defer s.mu.Unlock()

	bufID, r, err := s.bufferAndRing(ctx, network, target, false)
	if err != nil || r == nil {
		return nil, err
	}
	if out, ok := r.pageBefore(c, limit); ok {
		s.stats.ringPages++
		return out, nil
	}
	s.stats.dbPages++
	msgs, err := s.queryPage(ctx, network, target,
		`SELECT id, ts, msgid, sender, command, raw, redacted, COALESCE(redact_reason,'') FROM messages
		 WHERE buffer_id = ? AND (ts < ? OR (ts = ? AND id < ?))
		 ORDER BY ts DESC, id DESC LIMIT ?`,
		bufID, c.TS, c.TS, c.ID, limit)
	if err != nil {
		return nil, err
	}
	reverse(msgs)
	return msgs, nil
}

// After returns up to limit messages strictly newer than c, ascending.
// An unknown buffer yields an empty page, not an error.
func (s *Store) After(ctx context.Context, network, target string, c Cursor, limit int) ([]Message, error) {
	limit = clampLimit(limit)
	s.mu.Lock()
	defer s.mu.Unlock()

	bufID, r, err := s.bufferAndRing(ctx, network, target, false)
	if err != nil || r == nil {
		return nil, err
	}
	if out, ok := r.pageAfter(c, limit); ok {
		s.stats.ringPages++
		return out, nil
	}
	s.stats.dbPages++
	return s.queryPage(ctx, network, target,
		`SELECT id, ts, msgid, sender, command, raw, redacted, COALESCE(redact_reason,'') FROM messages
		 WHERE buffer_id = ? AND (ts > ? OR (ts = ? AND id > ?))
		 ORDER BY ts ASC, id ASC LIMIT ?`,
		bufID, c.TS, c.TS, c.ID, limit)
}

// CursorForMsgID resolves an IRCv3 msgid to its position in the buffer,
// for msgid-anchored history paging.
func (s *Store) CursorForMsgID(ctx context.Context, network, target, msgid string) (Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bufID, r, err := s.bufferAndRing(ctx, network, target, false)
	if err != nil {
		return Cursor{}, err
	}
	if r == nil {
		return Cursor{}, ErrMsgIDNotFound
	}
	var c Cursor
	err = s.db.QueryRowContext(ctx,
		`SELECT id, ts FROM messages WHERE buffer_id = ? AND msgid = ? LIMIT 1`,
		bufID, msgid).Scan(&c.ID, &c.TS)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, ErrMsgIDNotFound
	}
	if err != nil {
		return Cursor{}, err
	}
	return c, nil
}

// ReadMarker returns the buffer's read marker, or the zero time when none
// has been set.
func (s *Store) ReadMarker(ctx context.Context, network, target string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bufID, err := s.bufferID(ctx, network, target, false)
	if err != nil || bufID == 0 {
		return time.Time{}, err
	}
	var ts int64
	err = s.db.QueryRowContext(ctx,
		`SELECT ts FROM read_markers WHERE buffer_id = ?`, bufID).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(ts), nil
}

// SetReadMarker advances the buffer's read marker to t. Markers only move
// forward: with several devices syncing, the newest read position wins.
func (s *Store) SetReadMarker(ctx context.Context, network, target string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// A marker never creates a buffer: it references reading state for
	// scrollback that exists. Creation here let any malformed
	// authenticated request mint phantom network/buffer rows that then
	// appeared in every sidebar (and let a closed buffer resurrect via
	// the markRead path). Unknown buffers are a silent no-op.
	bufID, err := s.bufferID(ctx, network, target, false)
	if err != nil || bufID == 0 {
		return err
	}
	// Clamp to plausibility: a marker only means "read up to here", so
	// nothing meaningfully past the present is valid. Ceilinged at
	// now + a small skew tolerance rather than at the newest stored ts —
	// a message carrying a far-future server-time would otherwise raise
	// that ceiling and let one marker suppress unread counts forever
	// (markers never regress). Legitimate clock skew stays within the
	// tolerance.
	ts := min(t.UnixMilli(), time.Now().UnixMilli()+markerSkewMs)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO read_markers (buffer_id, ts) VALUES (?, ?)
		 ON CONFLICT (buffer_id) DO UPDATE SET ts = max(ts, excluded.ts)`,
		bufID, ts)
	return err
}

// BufferInfo summarizes one buffer for the client's sidebar.
type BufferInfo struct {
	Network string
	Target  string
	LastTS  int64 // unix ms of the newest message, 0 if none
	Marker  int64 // unix ms read marker, 0 if unset
	Unread  int64 // messages newer than the marker
}

// Buffers lists every buffer with its activity and read state, ordered by
// network then target. Archived buffers (close_buffer purge:false) are
// excluded — hidden until real conversation clears the flag.
func (s *Store) Buffers(ctx context.Context) ([]BufferInfo, error) {
	// The unread count must match what the client counts as unread live, or the
	// two disagree the moment a client fetches buffers (the badge jumps). The
	// client counts only conversation lines (renderable kind != "system"), i.e.
	// PRIVMSG/NOTICE — presence traffic (JOIN/PART/QUIT/NICK/MODE/TOPIC/KICK)
	// renders as a system row and never bumps unread. Filter to the same set
	// here. (Non-ACTION CTCP is never persisted, and a redacted row keeps its
	// PRIVMSG/NOTICE command, so it counts consistently on both sides.)
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.name, b.name,
			COALESCE((SELECT MAX(ts) FROM messages WHERE buffer_id = b.id), 0),
			COALESCE((SELECT ts FROM read_markers WHERE buffer_id = b.id), 0),
			(SELECT COUNT(*) FROM messages WHERE buffer_id = b.id
				AND command IN ('PRIVMSG','NOTICE')
				AND ts > COALESCE((SELECT ts FROM read_markers WHERE buffer_id = b.id), 0))
		FROM buffers b
		JOIN networks n ON n.id = b.network_id
		WHERE b.archived = 0
		ORDER BY n.name, b.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BufferInfo
	for rows.Next() {
		var b BufferInfo
		if err := rows.Scan(&b.Network, &b.Target, &b.LastTS, &b.Marker, &b.Unread); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RecentBuffers returns at most limit buffers, newest activity first, plus a
// sentinel indicating that more rows exist. It is the bounded transport-facing
// counterpart to Buffers: a hostile server can create thousands of maximally
// sized buffer names, so the WebSocket initial snapshot must never materialize
// the entire allowed store population before queue admission.
func (s *Store) RecentBuffers(ctx context.Context, limit int) ([]BufferInfo, bool, error) {
	if limit <= 0 {
		return []BufferInfo{}, false, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.name, b.name,
			COALESCE((SELECT MAX(ts) FROM messages WHERE buffer_id = b.id), 0) AS last_ts,
			COALESCE((SELECT ts FROM read_markers WHERE buffer_id = b.id), 0),
			(SELECT COUNT(*) FROM messages WHERE buffer_id = b.id
				AND command IN ('PRIVMSG','NOTICE')
				AND ts > COALESCE((SELECT ts FROM read_markers WHERE buffer_id = b.id), 0))
		FROM buffers b
		JOIN networks n ON n.id = b.network_id
		WHERE n.user_id = ? AND b.archived = 0
		ORDER BY last_ts DESC, n.name, b.name
		LIMIT ?`, defaultUserID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]BufferInfo, 0, limit)
	for rows.Next() {
		var b BufferInfo
		if err := rows.Scan(&b.Network, &b.Target, &b.LastTS, &b.Marker, &b.Unread); err != nil {
			return nil, false, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// bufferID resolves (network, target) to a buffer row id, creating rows
// when create is set. Returns 0 for an unknown buffer when create is not.
// Caller holds s.mu.
func (s *Store) bufferID(ctx context.Context, network, target string, create bool) (int64, error) {
	key := bufKey{network, target}
	if id, ok := s.buffers[key]; ok {
		return id, nil
	}
	netID, err := s.networkID(ctx, network, create)
	if err != nil || netID == 0 {
		return 0, err
	}

	var bufID int64
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM buffers WHERE network_id = ? AND name = ?`, netID, target).Scan(&bufID)
	if errors.Is(err, sql.ErrNoRows) {
		if !create {
			return 0, nil
		}
		// Bound the number of buffers a network accrues from inbound
		// traffic: server-controlled target/sender names would otherwise
		// create buffers (and rings, and message rows) without limit —
		// the last unbounded server-fed structure. A legitimate user's
		// channels + queries stay far below this. Only ACTIVE rows count
		// toward the cap: archived=1 is set solely by an explicit
		// authenticated user action (close_buffer purge:false), never by
		// server traffic, so the server-forgeable population stays bounded
		// at the cap while a user's archived buffers do not erode it.
		// Un-archiving can push the active count past the cap, and the
		// overshoot persists until the user closes buffers again. Accepted
		// (2026-07-21): the archived reservoir grows only through explicit
		// authenticated closes — unbounded across time, but priced one user
		// action per row — and while a server can RESURFACE those rows with
		// content, it can never mint them, so the total stays bounded by
		// user intent. The alternatives — refusing an archive at some second
		// quota, or leaving a live message hidden at resurface time — both
		// punish the user to enforce a bound the server cannot abuse.
		var count int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM buffers WHERE network_id = ? AND archived = 0`, netID).Scan(&count); err != nil {
			return 0, err
		}
		if count >= maxBuffersPerNetwork {
			// At cap: drop rather than create. Logged because callers report
			// ID 0 as a benign msgid dedup — without this line quota
			// exhaustion would be silent.
			log.Printf("store[%s]: buffer cap (%d) reached, refusing new buffer %q (message dropped)",
				network, maxBuffersPerNetwork, target)
			return 0, nil
		}
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO buffers (network_id, name) VALUES (?, ?)`, netID, target)
		if err != nil {
			return 0, err
		}
		if bufID, err = res.LastInsertId(); err != nil {
			return 0, err
		}
		// Brand-new buffer: an empty ring IS its entire history.
		s.rings[bufID] = newRing(s.ringSize)
		s.rings[bufID].complete = true
	} else if err != nil {
		return 0, err
	}
	s.buffers[key] = bufID
	return bufID, nil
}

// touchRing stamps a ring as most-recently-used for the LRU clock. Caller holds s.mu.
func (s *Store) touchRing(r *ring) {
	s.accessSeq++
	r.lastUsed = s.accessSeq
}

// evictRings drops least-recently-used resident rings until the global hot-ring
// byte total is back within budget, never evicting keepID (the ring the current
// operation is using). An evicted ring re-warms from SQLite on next access, so
// eviction is transparent to correctness — it only forces a disk read. Caller
// holds s.mu.
func (s *Store) evictRings(keepID int64) {
	for s.ringBytes > int64(s.maxRingBytes) {
		var victim int64
		var seq uint64
		found := false
		for id, r := range s.rings {
			if id == keepID {
				continue
			}
			if !found || r.lastUsed < seq {
				victim, seq, found = id, r.lastUsed, true
			}
		}
		if !found {
			// Only the in-use ring remains. It can still be over budget by
			// itself (large configured ring_size × near-clamp messages), so
			// trim its oldest entries rather than let one buffer defeat the
			// whole bound.
			if r, ok := s.rings[keepID]; ok {
				s.ringBytes += int64(r.trimToBytes(s.maxRingBytes))
			}
			return
		}
		s.ringBytes -= int64(s.rings[victim].bytes)
		delete(s.rings, victim)
		s.stats.ringEvictions++
	}
}

// bufferAndRing resolves the buffer and returns its ring, warming the ring
// from disk on first touch after startup. A nil ring means the buffer does
// not exist (and create was false). Caller holds s.mu.
func (s *Store) bufferAndRing(ctx context.Context, network, target string, create bool) (int64, *ring, error) {
	bufID, err := s.bufferID(ctx, network, target, create)
	if err != nil || bufID == 0 {
		return 0, nil, err
	}
	if r, ok := s.rings[bufID]; ok {
		s.touchRing(r)
		return bufID, r, nil
	}
	// Warm with the newest ringSize+1 rows: getting fewer than requested proves
	// the ring now holds the buffer's entire history. The scan STREAMS with a
	// byte budget (warmRingScan) — it stops as soon as the accumulated rows
	// reach maxRingBytes — so a very large configured ring_size against a
	// buffer full of near-clamp (16 KiB) messages cannot transiently
	// materialize hundreds of MiB here only for evictRings/trimToBytes to
	// drop them a moment later. The row cap (byte budget over the 128-byte
	// per-message floor) additionally bounds the LIMIT for SQLite's sake.
	warmLimit := s.ringSize + 1
	if byteCap := s.maxRingBytes/msgOverhead + 1; byteCap < warmLimit {
		warmLimit = byteCap
	}
	msgs, budgetStopped, err := s.warmRingScan(ctx, network, target, bufID, warmLimit)
	if err != nil {
		return 0, nil, err
	}
	reverse(msgs)
	r := newRing(s.ringSize)
	switch {
	case len(msgs) < warmLimit && !budgetStopped:
		// Disk returned fewer than requested (and not because the byte budget
		// cut the scan short): the ring holds all history.
		r.complete = true
	case len(msgs) > s.ringSize:
		// Over-fetched the +1 sentinel (only when no byte bound was hit);
		// drop the oldest so the ring holds exactly the newest ringSize.
		msgs = msgs[1:]
	}
	// msgs is already ascending (ORDER BY ts DESC + reverse) and len <= ringSize,
	// which is exactly the ring's backing invariant — adopt it directly rather
	// than re-inserting into a second slice (avoids the transient double copy).
	r.adopt(msgs)
	s.rings[bufID] = r
	s.ringBytes += int64(r.bytes)
	s.touchRing(r)
	s.evictRings(bufID) // a full warm can add up to one ring's worth at once
	return bufID, r, nil
}

// warmRingScan reads the newest rows of a buffer for ring warm-up, newest
// first, stopping EARLY once the accumulated msgBytes reach the global ring
// byte budget — the transient allocation is bounded by ~maxRingBytes no
// matter how large the configured ring_size or the stored messages are.
// budgetStopped reports an early stop, which the caller must treat as "history
// continues past what we hold" (the ring is NOT complete).
func (s *Store) warmRingScan(ctx context.Context, network, target string, bufID int64, limit int) (msgs []Message, budgetStopped bool, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, msgid, sender, command, raw, redacted, COALESCE(redact_reason,'') FROM messages
		 WHERE buffer_id = ? ORDER BY ts DESC, id DESC LIMIT ?`,
		bufID, limit)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var bytes int
	for rows.Next() {
		var (
			m        Message
			ts       int64
			msgid    sql.NullString
			redacted int
			reason   string
		)
		if err := rows.Scan(&m.ID, &ts, &msgid, &m.Sender, &m.Command, &m.Raw, &redacted, &reason); err != nil {
			return nil, false, err
		}
		m.Time = time.UnixMilli(ts)
		m.MsgID = msgid.String
		m.Redacted = redacted != 0
		m.RedactReason = reason
		m.Network, m.Target = network, target
		msgs = append(msgs, m)
		if bytes += msgBytes(m); bytes >= s.maxRingBytes {
			return msgs, true, rows.Err()
		}
	}
	return msgs, false, rows.Err()
}

func (s *Store) queryPage(ctx context.Context, network, target, query string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var (
			m        Message
			ts       int64
			msgid    sql.NullString
			redacted int
			reason   string
		)
		if err := rows.Scan(&m.ID, &ts, &msgid, &m.Sender, &m.Command, &m.Raw, &redacted, &reason); err != nil {
			return nil, err
		}
		m.Time = time.UnixMilli(ts)
		m.MsgID = msgid.String
		m.Redacted = redacted != 0
		m.RedactReason = reason
		m.Network, m.Target = network, target
		out = append(out, m)
	}
	return out, rows.Err()
}

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultPageSize
	case limit > MaxPageSize:
		return MaxPageSize
	default:
		return limit
	}
}

func reverse(msgs []Message) {
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
}

// CanonicalBuffer resolves target to an existing buffer's stored
// spelling under fold (the network's casemapping), so one IRC entity
// never splits across case-variant buffers (#Go vs #go, or rfc1459
// pairs) — echoed messages can arrive with client-supplied casing.
// Returns target unchanged when no buffer exists yet; the fast path
// (exact spelling already cached) costs one map lookup.
//
// This is advisory (the resolve and a later Append are separate): use
// AppendFolded when the append must not race a case-variant create.
func (s *Store) CanonicalBuffer(ctx context.Context, network, target string, fold func(string) string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, _ := s.canonicalLocked(ctx, network, target, fold)
	return name
}

// canonicalLocked resolves target to an existing buffer's stored
// spelling under fold, returning (name, true) on a match or
// (target, false) when none exists. Caller holds s.mu.
func (s *Store) canonicalLocked(ctx context.Context, network, target string, fold func(string) string) (string, bool) {
	if _, ok := s.buffers[bufKey{network: network, target: target}]; ok {
		return target, true // exact spelling already known
	}
	if fold == nil {
		return target, false
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.name FROM buffers b
		JOIN networks n ON n.id = b.network_id
		WHERE n.user_id = ? AND n.name = ?`, defaultUserID, network)
	if err != nil {
		return target, false
	}
	defer rows.Close()
	want := fold(target)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return target, false
		}
		if fold(name) == want {
			return name, true
		}
	}
	return target, false
}

// FindBuffer returns the stored buffer whose name matches target under
// fold (the network's IRC casemapping — SQLite's NOCASE is ASCII-only
// and would miss the rfc1459 []\^ pairs), preserving its stored casing.
// ok is false when no such buffer exists. The hub uses it to route
// QUIT/NICK lines into an open query buffer.
//
// This scans the network's buffer names and folds each in Go rather than
// filtering in SQL: the store is deliberately casemapping-agnostic (fold is
// a per-connection parameter, not stored), so the fold cannot live in an
// index. The cost is O(buffers per network) per live QUIT/NICK, over ALL
// rows including archived — so it is bounded by the active cap
// (maxBuffersPerNetwork) PLUS the archived reservoir, which has no hard cap
// and is bounded only by the user's own deliberate closes. ACCEPTED: s.mu is
// released between calls, and a real user has tens of buffers (microseconds).
// A hostile server that first opens thousands of buffers could turn this into
// a throughput drag, but not a stall or exhaustion. If that ever matters, an
// ASCII-NOCASE indexed lookup would fast-path the common (special-char-free)
// target and fall back to this scan only for names containing []\~{}|^.
func (s *Store) FindBuffer(ctx context.Context, network, target string, fold func(string) string) (name string, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT b.name FROM buffers b
		JOIN networks n ON n.id = b.network_id
		WHERE n.user_id = ? AND n.name = ?`, defaultUserID, network)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	want := fold(target)
	for rows.Next() {
		if err := rows.Scan(&name); err != nil {
			return "", false, err
		}
		if fold(name) == want {
			return name, true, rows.Err()
		}
	}
	return "", false, rows.Err()
}
