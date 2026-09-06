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
	"math/rand/v2"
	"time"
)

// BackoffConfig controls reconnect delays. Zero values pick the defaults:
// 2s initial, 5m cap, 30s stable uptime.
type BackoffConfig struct {
	Min time.Duration
	Max time.Duration
	// Stable is how long a connection must stay registered before the
	// delay ladder resets to Min. Resetting on 001 alone let a server that
	// registers us and then drops the link (post-welcome K-line, excess
	// flood during the rejoin burst, bouncer eviction) hold the client in
	// a minimum-delay reconnect loop forever.
	Stable time.Duration
}

// backoff produces exponentially growing reconnect delays with jitter:
// the n-th delay is uniform in [d/2, d) where d = min(Min<<n, Max).
// Jitter avoids reconnect stampedes when a server restart drops many
// clients at once.
type backoff struct {
	cfg     BackoffConfig
	attempt int
	// rnd returns a uniform duration in [0, d); replaced in tests.
	rnd func(d time.Duration) time.Duration
}

func newBackoff(cfg BackoffConfig) *backoff {
	if cfg.Min <= 0 {
		cfg.Min = 2 * time.Second
	}
	if cfg.Max <= 0 {
		cfg.Max = 5 * time.Minute
	}
	if cfg.Max < cfg.Min {
		cfg.Max = cfg.Min
	}
	if cfg.Stable <= 0 {
		cfg.Stable = 30 * time.Second
	}
	return &backoff{
		cfg: cfg,
		rnd: func(d time.Duration) time.Duration { return rand.N(d) },
	}
}

func (b *backoff) next() time.Duration {
	d := b.cfg.Max
	// The shift can overflow to <= 0 for large attempt counts; the guard
	// treats that as "past the cap" and stops growing attempt.
	if shifted := b.cfg.Min << b.attempt; shifted > 0 && shifted < b.cfg.Max {
		d = shifted
		b.attempt++
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + b.rnd(half)
}

// reset restarts the ladder so the next delay is the minimum again.
func (b *backoff) reset() {
	b.attempt = 0
}

// settle is called when a registered connection ends, with how long it
// stayed registered: only an uptime of at least Stable counts as a
// recovery that earns a reset. A shorter one — registered, then dropped —
// keeps climbing the ladder, so a server that welcomes and immediately
// kills us is retried at a growing interval rather than hammered.
func (b *backoff) settle(uptime time.Duration) {
	if uptime >= b.cfg.Stable {
		b.reset()
	}
}
