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
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// logLimitBase is a fixed synthetic clock origin: logLimiter.allow takes now, so the
// window logic is table-testable without sleeping.
var logLimitBase = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// TestLogLimiterAllow covers the window arithmetic: the first call always
// logs, calls inside the window are counted rather than emitted, and the
// first call past the window logs and hands back everything it swallowed.
func TestLogLimiterAllow(t *testing.T) {
	var l logLimiter

	if ok, n := l.allow(logLimitBase); !ok || n != 0 {
		t.Fatalf("first allow = (%v, %d), want (true, 0)", ok, n)
	}
	// Same window: suppressed, and never reports a count of its own.
	for i := 1; i <= 5; i++ {
		if ok, n := l.allow(logLimitBase.Add(time.Duration(i) * time.Second)); ok || n != 0 {
			t.Fatalf("allow inside window #%d = (%v, %d), want (false, 0)", i, ok, n)
		}
	}
	// Past the window: logs, and reports exactly the five it dropped.
	if ok, n := l.allow(logLimitBase.Add(storeErrLogEvery)); !ok || n != 5 {
		t.Fatalf("allow after window = (%v, %d), want (true, 5)", ok, n)
	}
	// The count resets with the new window rather than accumulating.
	if ok, n := l.allow(logLimitBase.Add(2 * storeErrLogEvery)); !ok || n != 0 {
		t.Fatalf("allow after quiet window = (%v, %d), want (true, 0)", ok, n)
	}
}

// TestLogLimiterSpacedErrorsAlwaysLog pins the no-regression case: failures
// further apart than the window are ordinary isolated errors and must log
// every time, exactly as an unlimited log.Printf did.
func TestLogLimiterSpacedErrorsAlwaysLog(t *testing.T) {
	var l logLimiter
	for i := 0; i < 4; i++ {
		at := logLimitBase.Add(time.Duration(i) * (storeErrLogEvery + time.Second))
		if ok, n := l.allow(at); !ok || n != 0 {
			t.Fatalf("spaced allow #%d = (%v, %d), want (true, 0)", i, ok, n)
		}
	}
}

// TestLogLimiterPrintfBoundsAFlood is the amplification guard this exists
// for: a sustained store fault drives one printf per inbound IRC message,
// and the journal must not grow one line per message. The suppressed tail
// keeps the volume of the fault visible.
func TestLogLimiterPrintfBoundsAFlood(t *testing.T) {
	buf := captureHubLog(t)
	var l logLimiter
	for i := 0; i < 1000; i++ {
		l.printf("irc[%s]: persist %s to %q: %v", "libera", "PRIVMSG", "#chan", "disk I/O error")
	}
	// 1000 failures inside one window: exactly the first line reaches the
	// log. (The test cannot cross a window without sleeping; allow() covers
	// the suppressed-count handoff.)
	if got := strings.Count(buf.String(), "persist PRIVMSG"); got != 1 {
		t.Fatalf("logged %d lines for 1000 failures, want 1:\n%s", got, buf.String())
	}
}

// TestLogLimiterPrintfReportsSuppressed checks the tail an operator reads:
// after the window rolls, the next line carries the dropped count.
func TestLogLimiterPrintfReportsSuppressed(t *testing.T) {
	buf := captureHubLog(t)
	var l logLimiter
	l.printf("boom %d", 1)
	for i := 0; i < 3; i++ {
		l.printf("boom %d", 2)
	}
	// Roll the window by backdating the last emission, rather than sleeping.
	l.mu.Lock()
	l.last = l.last.Add(-2 * storeErrLogEvery)
	l.mu.Unlock()
	l.printf("boom %d", 3)

	got := buf.String()
	if !strings.Contains(got, "boom 3 (+3 suppressed in the last 10s)") {
		t.Fatalf("suppressed tail missing/wrong:\n%s", got)
	}
	// The format arguments must still land in their own verbs — the count
	// is appended, never substituted into the caller's format.
	if strings.Contains(got, "%!") {
		t.Fatalf("format/argument mismatch in limited log:\n%s", got)
	}
}

// TestLogLimiterKindsAreIndependent: a storm of one store-error kind must not
// swallow the FIRST occurrence of another — that is why storeErrLogs keeps a
// limiter per kind instead of one shared limiter.
func TestLogLimiterKindsAreIndependent(t *testing.T) {
	buf := captureHubLog(t)
	var e storeErrLogs
	for i := 0; i < 100; i++ {
		e.persist.printf("persist failed")
	}
	e.redact.printf("redact failed")
	e.marker.printf("marker failed")
	e.adopt.printf("adopt failed")

	for _, want := range []string{"persist failed", "redact failed", "marker failed", "adopt failed"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("%q missing — one kind masked another:\n%s", want, buf.String())
		}
	}
	if got := strings.Count(buf.String(), "persist failed"); got != 1 {
		t.Fatalf("persist logged %d times, want 1", got)
	}
}

// TestLogLimiterConcurrent runs printf from many goroutines: the limiter is
// reached from one goroutine per network read loop, so it must be race-free
// and must still emit exactly one line for a single window.
func TestLogLimiterConcurrent(t *testing.T) {
	buf := captureHubLog(t)
	var l logLimiter
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				l.printf("concurrent failure")
			}
		}()
	}
	wg.Wait()
	if got := strings.Count(buf.String(), "concurrent failure"); got != 1 {
		t.Fatalf("logged %d lines, want 1:\n%s", got, buf.String())
	}
}

// captureHubLog redirects the standard logger to a buffer for the test.
func captureHubLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}
