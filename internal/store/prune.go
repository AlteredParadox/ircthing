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

package store

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strconv"
	"time"
)

// Settings-table keys for the runtime-editable retention policy. The config
// file seeds these on first run; the stored value (set via the UI) is
// authoritative thereafter, so a restart never reverts a runtime change.
const (
	retentionDaysKey = "retention_days"
	retentionMaxKey  = "retention_max_messages"
)

// Retention returns the current policy (days, max-per-buffer).
func (s *Store) Retention() (days, maxPerBuffer int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retention.days, s.retention.maxPerBuffer
}

// SetRetention updates the policy, persists it, and prunes once promptly so a
// tighter policy applies without waiting for the next hourly tick.
//
// NOTE: persist and live-state install happen under separate lock acquisitions
// (SetSettings takes s.mu itself), so this method is NOT internally
// linearizable — concurrent callers could leave the persisted and in-memory
// policies disagreeing. Callers MUST serialize it; the only caller (the API's
// handleSetConfig) does, holding settingsMu across the whole read-modify-write.
func (s *Store) SetRetention(ctx context.Context, days, maxPerBuffer int) error {
	// Both keys in one transaction: a partial write would leave the two
	// dimensions inconsistent across a restart.
	if err := s.SetSettings(ctx, map[string]string{
		retentionDaysKey: strconv.Itoa(days),
		retentionMaxKey:  strconv.Itoa(maxPerBuffer),
	}); err != nil {
		return err
	}
	s.mu.Lock()
	s.retention = retentionPolicy{days: days, maxPerBuffer: maxPerBuffer}
	s.mu.Unlock()
	// Nudge the (Close-tracked) background pruner instead of spawning an
	// untracked goroutine that could outlive Close and log against a closed
	// database. Non-blocking: a nudge already queued will prune once with the
	// latest policy, which is all we need.
	select {
	case s.pruneNow <- struct{}{}:
	default:
	}
	return nil
}

// loadRetention makes the settings table authoritative for retention. On first
// run (no stored keys) the config-seeded Options — already in s.retention — are
// written through; otherwise the stored value overrides them.
func (s *Store) loadRetention(ctx context.Context, opts Options) error {
	dv, err := s.Setting(ctx, retentionDaysKey)
	if err != nil {
		return err
	}
	mv, err := s.Setting(ctx, retentionMaxKey)
	if err != nil {
		return err
	}
	if dv == "" && mv == "" { // first run: seed from config, both keys atomically
		return s.SetSettings(ctx, map[string]string{
			retentionDaysKey: strconv.Itoa(opts.RetentionDays),
			retentionMaxKey:  strconv.Itoa(opts.RetentionMaxMessages),
		})
	}
	days, _ := strconv.Atoi(dv)
	maxPer, _ := strconv.Atoi(mv)
	s.mu.Lock()
	s.retention = retentionPolicy{days: days, maxPerBuffer: maxPer}
	s.mu.Unlock()
	return nil
}

// pruneInterval is how often the background pruner runs when retention is
// configured. History bounds are coarse, so hourly is ample and keeps the
// single write connection almost always idle.
const pruneInterval = time.Hour

// retentionPolicy bounds stored history. Either or both dimensions may be
// active; a zero field disables that dimension.
type retentionPolicy struct {
	days         int // delete messages older than this many days
	maxPerBuffer int // keep only the newest N messages per buffer
}

// startPruner launches the background pruner: it prunes once immediately,
// then every interval until Close closes stopPruner. Age cutoffs use
// wall-clock time, so the goroutine owns the clock (pruneOnce takes now as
// a parameter to stay testable).
func (s *Store) startPruner(interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	s.prunerCancel = cancel
	s.stopPruner = make(chan struct{})
	s.pruneNow = make(chan struct{}, 1)
	s.prunerDone.Add(1)
	go func() {
		defer s.prunerDone.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			// ctx is canceled by Close, so the chunk loops stop promptly and
			// shutdown doesn't block on a long in-flight prune.
			if n, err := s.pruneOnce(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("store: prune: %v", err)
			} else if n > 0 {
				log.Printf("store: pruned %d message(s) past retention", n)
			}
			select {
			case <-s.stopPruner:
				return
			case <-s.pruneNow: // a SetRetention nudge: prune again promptly
			case <-t.C:
			}
		}
	}()
}

// pruneChunk bounds how many rows one DELETE removes before releasing s.mu.
// The first prune after enabling retention on a large existing database can
// delete millions of rows; doing it in one statement would hold the store
// lock (blocking every append and history page) for the whole operation.
// Chunking re-acquires the lock per batch so real traffic interleaves. 2000
// rows keeps each batch's lock-hold to a few ms on the deployment target.
// var (not const) so a test can shrink it to exercise the multi-chunk loop.
var pruneChunk = 2000

// pruneOnce deletes messages that exceed the retention policy: those older
// than the age cutoff, and those beyond the newest maxPerBuffer in each
// buffer. The FTS index stays in step via the messages delete trigger. It
// deletes in bounded chunks, re-acquiring s.mu per chunk rather than holding
// it across a potentially multi-GB delete+vacuum. Returns the rows deleted.
func (s *Store) pruneOnce(ctx context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	days, maxPer := s.retention.days, s.retention.maxPerBuffer
	s.mu.Unlock()

	// Reconcile the hot rings at the end (via defer, so it runs even if a
	// DELETE errors out) with EXACTLY the criteria that reached disk:
	// appliedCutoff/appliedMax are set once a dimension's delete has run.
	var total int64
	var appliedCutoff int64
	var appliedMax int
	defer func() {
		s.mu.Lock()
		s.reconcileRings(appliedCutoff, appliedMax)
		s.mu.Unlock()
	}()

	if days > 0 {
		cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
		// Bail if retention is disabled or its window is widened mid-prune: our
		// fixed cutoff must never delete a row the live (possibly larger) window
		// would keep. A tightened window (live cutoff newer) is fine — we simply
		// delete less than it wants and the next prune catches up.
		ageGuard := func() bool {
			s.mu.Lock()
			liveDays := s.retention.days
			s.mu.Unlock()
			if liveDays == 0 {
				return false
			}
			return cutoff <= now.Add(-time.Duration(liveDays)*24*time.Hour).UnixMilli()
		}
		n, err := s.deleteChunked(ctx, ageGuard,
			`DELETE FROM messages WHERE id IN (SELECT id FROM messages WHERE ts < ? LIMIT ?)`,
			cutoff)
		total += n
		// Trim the rings to the cutoff whenever any pre-cutoff row was (or is
		// being) deleted: an untrimmed complete ring would keep serving them.
		// Safe on partial failure — a ring miss just falls back to disk.
		if n > 0 || err == nil {
			appliedCutoff = cutoff
		}
		if err != nil {
			return total, err
		}
	}
	if maxPer > 0 {
		n, err := s.pruneByCount(ctx, maxPer)
		total += n
		if n > 0 || err == nil {
			appliedMax = maxPer
		}
		if err != nil {
			return total, err
		}
	}
	// Return the freed pages to the OS (auto_vacuum is INCREMENTAL) in bounded
	// steps, so the file shrinks under retention without a long lock-hold.
	// Best-effort: a failure here is not worth failing the prune over.
	if total > 0 {
		s.vacuumChunked(ctx)
	}
	return total, nil
}

// deleteChunked runs a `DELETE ... id IN (SELECT ... LIMIT ?)` statement
// repeatedly — re-acquiring s.mu for each chunk — until a short batch signals
// the last rows are gone. args are the query parameters BEFORE the trailing
// LIMIT. Returns the total rows deleted. guard (if non-nil) is checked before
// each chunk against the LIVE retention policy: if the operator loosens or
// disables retention mid-prune, it returns false and the loop stops cleanly
// rather than continuing to delete rows the new policy wants to keep.
func (s *Store) deleteChunked(ctx context.Context, guard func() bool, query string, args ...any) (int64, error) {
	var total int64
	chunkArgs := append(append(make([]any, 0, len(args)+1), args...), pruneChunk)
	for {
		if guard != nil && !guard() {
			return total, nil // retention loosened mid-prune; stop deleting
		}
		s.mu.Lock()
		res, err := s.db.ExecContext(ctx, query, chunkArgs...)
		if err != nil {
			s.mu.Unlock()
			return total, err
		}
		n, _ := res.RowsAffected()
		s.mu.Unlock()
		total += n
		if n < int64(pruneChunk) {
			return total, nil // last (partial) batch
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
}

// pruneByCount enforces the per-buffer newest-N cap without a global window
// scan: for each buffer it finds the cursor of the last row to KEEP (the
// maxPer-th newest), then chunk-deletes everything older in that buffer using
// the (buffer_id, ts) index. This bounds each chunk's cost — a single
// row_number() over the whole table would re-rank every row per chunk.
func (s *Store) pruneByCount(ctx context.Context, maxPer int) (int64, error) {
	// Read buffer ids from the table, not s.buffers: on the first prune after
	// Open the in-memory cache is empty while the DB is full.
	s.mu.Lock()
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM buffers`)
	if err != nil {
		s.mu.Unlock()
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			s.mu.Unlock()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	err = rows.Err()
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}

	// Stop enforcing this maxPer if the operator raises or disables the per-buffer
	// cap mid-prune — otherwise we keep deleting down to the old, smaller cap.
	countGuard := func() bool {
		s.mu.Lock()
		liveMax := s.retention.maxPerBuffer
		s.mu.Unlock()
		return liveMax > 0 && liveMax <= maxPer
	}
	var total int64
	for _, bufID := range ids {
		if !countGuard() {
			return total, nil
		}
		// The last row to keep: the maxPer-th newest (OFFSET maxPer-1). No row
		// means the buffer has <= maxPer rows — nothing to prune.
		s.mu.Lock()
		var keepTS, keepID int64
		err := s.db.QueryRowContext(ctx,
			`SELECT ts, id FROM messages WHERE buffer_id = ?
			 ORDER BY ts DESC, id DESC LIMIT 1 OFFSET ?`, bufID, maxPer-1).Scan(&keepTS, &keepID)
		s.mu.Unlock()
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return total, err
		}
		// Delete everything strictly older than the last-kept cursor.
		n, err := s.deleteChunked(ctx, countGuard,
			`DELETE FROM messages WHERE id IN (
				SELECT id FROM messages
				WHERE buffer_id = ? AND (ts < ? OR (ts = ? AND id < ?)) LIMIT ?)`,
			bufID, keepTS, keepTS, keepID)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// vacuumChunked returns freed pages to the OS in bounded steps (auto_vacuum is
// INCREMENTAL), re-acquiring s.mu per step so a large reclaim doesn't stall
// traffic. Best-effort: any error just stops early.
func (s *Store) vacuumChunked(ctx context.Context) {
	for {
		s.mu.Lock()
		var free int
		if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil || free == 0 {
			s.mu.Unlock()
			return
		}
		_, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum(1000)`)
		s.mu.Unlock()
		if err != nil {
			log.Printf("store: incremental_vacuum: %v", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// reconcileRings trims each hot ring to the retention criteria that actually
// deleted rows on disk (a zero dimension is a no-op). A ring can be `complete`
// — it holds a buffer's entire history and is then served authoritatively
// without touching the DB — so a dormant buffer whose rows were deleted would
// otherwise keep serving them from memory until process restart. Filtering in
// place keeps the cache warm. Callers must hold s.mu.
func (s *Store) reconcileRings(appliedCutoff int64, appliedMax int) {
	if appliedCutoff <= 0 && appliedMax <= 0 {
		return
	}
	for _, r := range s.rings {
		s.ringBytes += int64(r.applyRetention(appliedCutoff, appliedMax)) // frees rows: a net decrease
	}
}
