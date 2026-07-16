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
	"math"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (approved; CGO-free)
)

const (
	// DefaultRingSize is the per-buffer hot scrollback bound: with the
	// 50-channel scenario from the memory target this keeps hot history
	// around 10k messages total.
	DefaultRingSize = 200
	DefaultPageSize = 100
	MaxPageSize     = 500
)

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

var ErrMsgIDNotFound = errors.New("store: msgid not found")

type Options struct {
	// RingSize bounds the per-buffer in-memory scrollback.
	// 0 means DefaultRingSize.
	RingSize int
}

// Store is safe for concurrent use. One coarse mutex guards the rings and
// caches; at IRC message rates lock contention is a non-issue and this
// keeps the invariants easy to reason about.
type Store struct {
	db       *sql.DB
	ringSize int

	mu       sync.Mutex
	networks map[string]int64
	buffers  map[bufKey]int64
	rings    map[int64]*ring
	stats    struct{ ringPages, dbPages int } // observability for tests
}

type bufKey struct{ network, target string }

// Open opens (creating if needed) the database at path and applies any
// pending migrations. WAL mode per the architecture; NORMAL synchronous is
// the documented safe pairing with WAL.
func Open(path string, opts Options) (*Store, error) {
	if opts.RingSize <= 0 {
		opts.RingSize = DefaultRingSize
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"
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
	return &Store{
		db:       db,
		ringSize: opts.RingSize,
		networks: make(map[string]int64),
		buffers:  make(map[bufKey]int64),
		rings:    make(map[int64]*ring),
	}, nil
}

func (s *Store) Close() error {
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
	return s.append(ctx, network, target, m, true)
}

// AppendExisting is Append minus buffer creation: the message is
// silently dropped (ID 0) when no buffer exists. Used for our own PART
// echo, which must not resurrect a buffer the user just closed — the
// close_buffer delete and the PART echo race, and both orders must end
// with the buffer gone.
func (s *Store) AppendExisting(ctx context.Context, network, target string, m Message) (Message, error) {
	return s.append(ctx, network, target, m, false)
}

func (s *Store) append(ctx context.Context, network, target string, m Message, create bool) (Message, error) {
	if network == "" || target == "" {
		return Message{}, errors.New("store: network and target must be non-empty")
	}
	if m.Time.IsZero() {
		m.Time = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	bufID, r, err := s.bufferAndRing(ctx, network, target, create)
	if err != nil {
		return Message{}, err
	}
	if bufID == 0 {
		return Message{}, nil // no such buffer and create is off
	}
	var msgid, text any
	if m.MsgID != "" {
		msgid = m.MsgID
	}
	if m.Text != "" {
		text = m.Text // NULL otherwise: not indexed for search
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO messages (buffer_id, ts, msgid, sender, command, raw, text) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		bufID, m.Time.UnixMilli(), msgid, m.Sender, m.Command, m.Raw, text)
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
	m.Network, m.Target = network, target
	r.insert(m)
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
	s.mu.Lock()
	defer s.mu.Unlock()

	bufID, err := s.bufferID(ctx, network, target, false)
	if err != nil || bufID == 0 {
		return false, err
	}
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET redacted = 1, redact_reason = ?
		 WHERE buffer_id = ? AND msgid = ? AND redacted = 0`,
		reasonArg, bufID, msgid)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if r, ok := s.rings[bufID]; ok {
		r.redact(msgid, reason)
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
	// nothing past the newest stored message (or the present, for a
	// quiet buffer) is meaningful. Without the clamp one buggy or
	// malicious timestamp near MaxInt64 would suppress unread counts
	// forever, because markers never regress.
	var latest int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(ts), 0) FROM messages WHERE buffer_id = ?`,
		bufID).Scan(&latest); err != nil {
		return err
	}
	ts := min(t.UnixMilli(), max(latest, time.Now().UnixMilli()))
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
// network then target.
func (s *Store) Buffers(ctx context.Context) ([]BufferInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.name, b.name,
			COALESCE((SELECT MAX(ts) FROM messages WHERE buffer_id = b.id), 0),
			COALESCE((SELECT ts FROM read_markers WHERE buffer_id = b.id), 0),
			(SELECT COUNT(*) FROM messages WHERE buffer_id = b.id
				AND ts > COALESCE((SELECT ts FROM read_markers WHERE buffer_id = b.id), 0))
		FROM buffers b
		JOIN networks n ON n.id = b.network_id
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

// bufferAndRing resolves the buffer and returns its ring, warming the ring
// from disk on first touch after startup. A nil ring means the buffer does
// not exist (and create was false). Caller holds s.mu.
func (s *Store) bufferAndRing(ctx context.Context, network, target string, create bool) (int64, *ring, error) {
	bufID, err := s.bufferID(ctx, network, target, create)
	if err != nil || bufID == 0 {
		return 0, nil, err
	}
	if r, ok := s.rings[bufID]; ok {
		return bufID, r, nil
	}
	// Warm with the newest ringSize+1 rows: getting fewer proves the ring
	// now holds the buffer's entire history.
	msgs, err := s.queryPage(ctx, network, target,
		`SELECT id, ts, msgid, sender, command, raw, redacted, COALESCE(redact_reason,'') FROM messages
		 WHERE buffer_id = ? ORDER BY ts DESC, id DESC LIMIT ?`,
		bufID, s.ringSize+1)
	if err != nil {
		return 0, nil, err
	}
	reverse(msgs)
	r := newRing(s.ringSize)
	if len(msgs) <= s.ringSize {
		r.complete = true
	} else {
		msgs = msgs[1:]
	}
	for _, m := range msgs {
		r.insert(m)
	}
	s.rings[bufID] = r
	return bufID, r, nil
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
func (s *Store) CanonicalBuffer(ctx context.Context, network, target string, fold func(string) string) string {
	s.mu.Lock()
	if _, ok := s.buffers[bufKey{network: network, target: target}]; ok {
		s.mu.Unlock()
		return target
	}
	s.mu.Unlock()
	if name, ok, err := s.FindBuffer(ctx, network, target, fold); err == nil && ok {
		return name
	}
	return target
}

// FindBuffer returns the stored buffer whose name matches target under
// fold (the network's IRC casemapping — SQLite's NOCASE is ASCII-only
// and would miss the rfc1459 []\^ pairs), preserving its stored casing.
// ok is false when no such buffer exists. The hub uses it to route
// QUIT/NICK lines into an open query buffer.
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
