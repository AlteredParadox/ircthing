package irc

import (
	"context"
	"time"
)

// tokenBucket throttles outbound messages: an initial burst of `burst`
// messages may go out back-to-back, then one message per `interval` —
// the classic RFC 1459 client-side flood penalty model. It is used only
// by the single writer goroutine, so it needs no locking.
type tokenBucket struct {
	interval time.Duration
	window   time.Duration // burst * interval
	at       time.Time     // virtual send-time watermark
}

func newTokenBucket(burst int, interval time.Duration) *tokenBucket {
	return &tokenBucket{interval: interval, window: time.Duration(burst) * interval}
}

// reserve advances the watermark for one message and returns how long the
// caller must wait before sending it. Split from wait so the arithmetic is
// table-testable with a synthetic clock.
func (tb *tokenBucket) reserve(now time.Time) time.Duration {
	if tb.at.Before(now) {
		tb.at = now
	}
	tb.at = tb.at.Add(tb.interval)
	return tb.at.Sub(now) - tb.window
}

func (tb *tokenBucket) wait(ctx context.Context) error {
	d := tb.reserve(time.Now())
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
