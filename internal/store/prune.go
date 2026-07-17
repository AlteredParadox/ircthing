package store

import (
	"context"
	"log"
	"time"
)

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

func (p retentionPolicy) enabled() bool { return p.days > 0 || p.maxPerBuffer > 0 }

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

	var total int64
	if s.retention.days > 0 {
		cutoff := now.Add(-time.Duration(s.retention.days) * 24 * time.Hour).UnixMilli()
		res, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE ts < ?`, cutoff)
		if err != nil {
			return total, err
		}
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
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	// A hot ring holds the newest suffix of a buffer. Age pruning removes
	// OLD rows the ring rarely holds, so those rings self-correct on
	// eviction. But if the per-buffer cap is below the ring size, a ring can
	// hold rows we just deleted from disk and would serve them from memory —
	// drop the rings so they re-warm from disk. This only triggers in the
	// unusual small-cap configuration.
	if total > 0 && s.retention.maxPerBuffer > 0 && s.retention.maxPerBuffer < s.ringSize {
		s.rings = make(map[int64]*ring)
	}
	return total, nil
}
