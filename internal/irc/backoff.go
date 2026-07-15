package irc

import (
	"math/rand/v2"
	"time"
)

// BackoffConfig controls reconnect delays. Zero values pick the defaults:
// 2s initial, 5m cap.
type BackoffConfig struct {
	Min time.Duration
	Max time.Duration
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

// reset is called after a successful registration so the next disconnect
// starts from the minimum delay again.
func (b *backoff) reset() {
	b.attempt = 0
}
