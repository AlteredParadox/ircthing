package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Server-side full-text search over message bodies via the FTS5 index
// (see migration 0003). Results are whole messages, newest first, with
// cursor pagination — the client renders them exactly like scrollback.

// SearchOptions parameterizes a search. Network (and then Target) narrow
// the scope; both empty searches everything. Before paginates: only
// matches older than the cursor are returned.
type SearchOptions struct {
	Query   string
	Network string
	Target  string
	Before  Cursor
	Limit   int
}

// Search returns messages matching opts.Query, newest first. An empty or
// whitespace-only query yields no results (never an error).
func (s *Store) Search(ctx context.Context, opts SearchOptions) ([]Message, error) {
	match := ftsQuery(opts.Query)
	if match == "" {
		return nil, nil
	}
	limit := clampLimit(opts.Limit)

	var (
		sb   strings.Builder
		args []any
	)
	sb.WriteString(`SELECT m.id, m.ts, m.msgid, m.sender, m.command, m.raw, n.name, b.name
		FROM messages m
		JOIN messages_fts f ON f.rowid = m.id
		JOIN buffers b ON b.id = m.buffer_id
		JOIN networks n ON n.id = b.network_id
		WHERE f.text MATCH ? AND n.user_id = ?`)
	args = append(args, match, defaultUserID)

	if opts.Network != "" {
		sb.WriteString(" AND n.name = ?")
		args = append(args, opts.Network)
		if opts.Target != "" {
			sb.WriteString(" AND b.name = ?")
			args = append(args, opts.Target)
		}
	}
	if opts.Before != (Cursor{}) {
		sb.WriteString(" AND (m.ts < ? OR (m.ts = ? AND m.id < ?))")
		args = append(args, opts.Before.TS, opts.Before.TS, opts.Before.ID)
	}
	sb.WriteString(" ORDER BY m.ts DESC, m.id DESC LIMIT ?")
	args = append(args, limit)

	return s.scanJoinedRows(ctx, sb.String(), args...)
}

// Around returns up to limit messages centered on the cursor c
// (inclusive of the message at c), ascending — used to jump to a search
// hit with surrounding context. Unknown buffers yield an empty page.
func (s *Store) Around(ctx context.Context, network, target string, c Cursor, limit int) ([]Message, error) {
	limit = clampLimit(limit)
	s.mu.Lock()
	defer s.mu.Unlock()

	bufID, err := s.bufferID(ctx, network, target, false)
	if err != nil || bufID == 0 {
		return nil, err
	}
	half := limit / 2
	// Older half plus the pivot itself (<= c), newest first.
	older, err := s.queryPage(ctx, network, target,
		`SELECT id, ts, msgid, sender, command, raw FROM messages
		 WHERE buffer_id = ? AND (ts < ? OR (ts = ? AND id <= ?))
		 ORDER BY ts DESC, id DESC LIMIT ?`,
		bufID, c.TS, c.TS, c.ID, half+1)
	if err != nil {
		return nil, err
	}
	reverse(older)
	// Strictly newer half, ascending.
	newer, err := s.queryPage(ctx, network, target,
		`SELECT id, ts, msgid, sender, command, raw FROM messages
		 WHERE buffer_id = ? AND (ts > ? OR (ts = ? AND id > ?))
		 ORDER BY ts ASC, id ASC LIMIT ?`,
		bufID, c.TS, c.TS, c.ID, limit-len(older))
	if err != nil {
		return nil, err
	}
	return append(older, newer...), nil
}

// scanJoinedRows scans the search query's rows, which carry network and
// buffer names as the last two columns.
func (s *Store) scanJoinedRows(ctx context.Context, query string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var (
			m     Message
			ts    int64
			msgid sql.NullString
		)
		if err := rows.Scan(&m.ID, &ts, &msgid, &m.Sender, &m.Command, &m.Raw, &m.Network, &m.Target); err != nil {
			return nil, err
		}
		m.Time = time.UnixMilli(ts)
		m.MsgID = msgid.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// ftsQuery turns free-form user input into a safe FTS5 MATCH expression:
// each whitespace-separated term becomes a quoted string (embedded quotes
// doubled), AND-combined. Quoting neutralizes FTS operators and syntax
// characters so arbitrary input can never error or inject operators.
func ftsQuery(input string) string {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return ""
	}
	quoted := make([]string, len(fields))
	for i, f := range fields {
		quoted[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " ")
}
