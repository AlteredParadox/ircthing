package store

import (
	"context"
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
func (s *Store) SetRetention(ctx context.Context, days, maxPerBuffer int) error {
	if err := s.SetSetting(ctx, retentionDaysKey, strconv.Itoa(days)); err != nil {
		return err
	}
	if err := s.SetSetting(ctx, retentionMaxKey, strconv.Itoa(maxPerBuffer)); err != nil {
		return err
	}
	s.mu.Lock()
	s.retention = retentionPolicy{days: days, maxPerBuffer: maxPerBuffer}
	s.mu.Unlock()
	go func() {
		if _, err := s.pruneOnce(context.Background(), time.Now()); err != nil {
			log.Printf("store: prune after retention change: %v", err)
		}
	}()
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
	if dv == "" && mv == "" { // first run: seed from config
		if err := s.SetSetting(ctx, retentionDaysKey, strconv.Itoa(opts.RetentionDays)); err != nil {
			return err
		}
		return s.SetSetting(ctx, retentionMaxKey, strconv.Itoa(opts.RetentionMaxMessages))
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
	s.stopPruner = make(chan struct{})
	s.prunerDone.Add(1)
	go func() {
		defer s.prunerDone.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			if n, err := s.pruneOnce(context.Background(), time.Now()); err != nil {
				log.Printf("store: prune: %v", err)
			} else if n > 0 {
				log.Printf("store: pruned %d message(s) past retention", n)
			}
			select {
			case <-s.stopPruner:
				return
			case <-t.C:
			}
		}
	}()
}

// pruneOnce deletes messages that exceed the retention policy: those older
// than the age cutoff, and those beyond the newest maxPerBuffer in each
// buffer. The FTS index stays in step via the messages delete trigger.
// Returns the number of rows deleted.
func (s *Store) pruneOnce(ctx context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Reconcile the hot rings (via defer, so it runs even if a later DELETE
	// errors out) with EXACTLY the criteria that actually landed on disk:
	// appliedCutoff/appliedMax are set only after their DELETE succeeds.
	var total int64
	var appliedCutoff int64
	var appliedMax int
	defer func() { s.reconcileRings(appliedCutoff, appliedMax) }()

	if s.retention.days > 0 {
		cutoff := now.Add(-time.Duration(s.retention.days) * 24 * time.Hour).UnixMilli()
		res, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE ts < ?`, cutoff)
		if err != nil {
			return total, err
		}
		appliedCutoff = cutoff
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	if s.retention.maxPerBuffer > 0 {
		// Window function ranks each buffer's rows newest-first; anything
		// past the cap is deleted. row_number() (SQLite >= 3.25) is
		// supported by modernc/sqlite.
		res, err := s.db.ExecContext(ctx, `
			DELETE FROM messages WHERE id IN (
				SELECT id FROM (
					SELECT id, row_number() OVER (
						PARTITION BY buffer_id ORDER BY ts DESC, id DESC
					) AS rn FROM messages
				) WHERE rn > ?
			)`, s.retention.maxPerBuffer)
		if err != nil {
			return total, err
		}
		appliedMax = s.retention.maxPerBuffer
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	// Return the pages just freed by the deletes to the OS (auto_vacuum is
	// INCREMENTAL), so the database file actually shrinks under retention
	// instead of only ever growing. Best-effort: a failure here is not worth
	// failing the prune over.
	if total > 0 {
		if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
			log.Printf("store: incremental_vacuum: %v", err)
		}
	}
	return total, nil
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
		r.applyRetention(appliedCutoff, appliedMax)
	}
}
