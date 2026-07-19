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
	"testing"
	"time"
)

func TestBackoffGrowthAndCap(t *testing.T) {
	b := newBackoff(BackoffConfig{Min: 2 * time.Second, Max: 30 * time.Second})
	b.rnd = func(time.Duration) time.Duration { return 0 } // deterministic: lower bound d/2

	// d = min(Min<<n, Max): 2s, 4s, 8s, 16s, then capped at 30s.
	want := []time.Duration{
		time.Second,      // 2s/2
		2 * time.Second,  // 4s/2
		4 * time.Second,  // 8s/2
		8 * time.Second,  // 16s/2
		15 * time.Second, // capped: 30s/2
		15 * time.Second, // stays capped
	}
	for i, w := range want {
		if got := b.next(); got != w {
			t.Fatalf("next() #%d = %v, want %v", i, got, w)
		}
	}
}

func TestBackoffReset(t *testing.T) {
	b := newBackoff(BackoffConfig{Min: 2 * time.Second, Max: 30 * time.Second})
	b.rnd = func(time.Duration) time.Duration { return 0 }

	b.next()
	b.next()
	b.reset()
	if got := b.next(); got != time.Second {
		t.Fatalf("next() after reset = %v, want %v", got, time.Second)
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	b := newBackoff(BackoffConfig{Min: 8 * time.Second, Max: time.Minute})
	for i := 0; i < 200; i++ {
		b.reset()
		// First delay: d = 8s, so next() must land in [4s, 8s).
		if got := b.next(); got < 4*time.Second || got >= 8*time.Second {
			t.Fatalf("jittered delay %v outside [4s, 8s)", got)
		}
	}
}

func TestBackoffDefaults(t *testing.T) {
	b := newBackoff(BackoffConfig{})
	if b.cfg.Min != 2*time.Second || b.cfg.Max != 5*time.Minute {
		t.Fatalf("defaults = %v/%v, want 2s/5m", b.cfg.Min, b.cfg.Max)
	}
	// Max below Min is clamped rather than producing a shrinking delay.
	b = newBackoff(BackoffConfig{Min: 10 * time.Second, Max: time.Second})
	if b.cfg.Max != 10*time.Second {
		t.Fatalf("clamped Max = %v, want 10s", b.cfg.Max)
	}
}

func TestTokenBucketReserve(t *testing.T) {
	start := time.Unix(1000, 0)
	tb := newTokenBucket(2, 100*time.Millisecond)

	cases := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		// Burst of 2 goes out immediately...
		{"first of burst", start, -100 * time.Millisecond},
		{"second of burst", start, 0},
		// ...then each further message waits one interval more.
		{"third throttled", start, 100 * time.Millisecond},
		{"fourth throttled", start, 200 * time.Millisecond},
		// Idle time refills: 300ms later the watermark has drained.
		{"refilled after idle", start.Add(500 * time.Millisecond), -100 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := tb.reserve(tc.now); got != tc.want {
			t.Fatalf("%s: reserve = %v, want %v", tc.name, got, tc.want)
		}
	}
}
