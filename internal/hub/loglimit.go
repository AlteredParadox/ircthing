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

package hub

import (
	"log"
	"sync"
	"time"
)

// storeErrLogEvery bounds how often one kind of inbound-message store error
// reaches the log. Long enough that a persistent fault costs a handful of
// lines per minute instead of one per message; short enough that an operator
// watching the journal sees the fault promptly.
const storeErrLogEvery = 10 * time.Second

// logLimiter collapses a repeating log line into at most one emission per
// storeErrLogEvery window, reporting how many it dropped when it next emits.
//
// Every store write on the inbound path is driven by a remote IRC line, so a
// store fault that persists — a full data filesystem is the motivating case —
// makes EVERY message fail, and an unlimited log line per failure turns one
// fault into unbounded journald writes. On a full filesystem those writes
// compete for the very space that is exhausted, so the diagnostic makes the
// condition it reports worse. Same reasoning as loginSource.blockLogged in
// internal/api: a cheap remote action must not buy unbounded disk writes.
//
// The suppressed count is carried into the next line rather than dropped, so
// the log still distinguishes a single transient error from a sustained one.
// The zero value is ready to use.
type logLimiter struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int64
}

// allow reports whether the caller should log now and, if so, how many
// emissions were suppressed since the previous one. The first call always
// logs; a fault slower than the window is never suppressed, so ordinary
// isolated errors log exactly as they did before.
func (l *logLimiter) allow(now time.Time) (ok bool, suppressed int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && now.Sub(l.last) < storeErrLogEvery {
		l.suppressed++
		return false, 0
	}
	l.last = now
	suppressed, l.suppressed = l.suppressed, 0
	return true, suppressed
}

// printf logs at most one line per window, appending the count of lines the
// limiter dropped since the last emission. Arguments are formatted as by
// log.Printf; callers keep their existing clamping/quoting of server-derived
// fields (the limiter bounds volume, not content).
func (l *logLimiter) printf(format string, args ...any) {
	ok, suppressed := l.allow(time.Now())
	if !ok {
		return
	}
	if suppressed > 0 {
		log.Printf(format+" (+%d suppressed in the last %s)",
			append(args, suppressed, storeErrLogEvery)...)
		return
	}
	log.Printf(format, args...)
}

// storeErrLogs holds one limiter per kind of inbound-message store error, so
// a storm of one kind cannot mask the first occurrence of another.
type storeErrLogs struct {
	persist logLimiter // message/system-line append
	adopt   logLimiter // own-message msgid dedup
	redact  logLimiter // draft/message-redaction scrub
	marker  logLimiter // upstream draft/read-marker sync
}
