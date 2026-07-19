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
