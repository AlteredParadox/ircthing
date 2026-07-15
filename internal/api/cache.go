package api

import (
	"sync"
	"time"
)

// ttlCache is a small bounded cache with per-entry expiry, used for proxy
// previews and thumbnails so a busy channel doesn't refetch the same URL
// repeatedly. Eviction is best-effort (expired entries first, then an
// arbitrary one) — good enough at proxy volumes and easy to reason about.
type ttlCache[V any] struct {
	mu  sync.Mutex
	m   map[string]ttlEntry[V]
	ttl time.Duration
	max int
	now func() time.Time
}

type ttlEntry[V any] struct {
	val V
	exp time.Time
}

func newTTLCache[V any](ttl time.Duration, max int) *ttlCache[V] {
	return &ttlCache[V]{m: make(map[string]ttlEntry[V]), ttl: ttl, max: max, now: time.Now}
}

func (c *ttlCache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || c.now().After(e.exp) {
		if ok {
			delete(c.m, key)
		}
		var zero V
		return zero, false
	}
	return e.val, true
}

func (c *ttlCache[V]) put(key string, val V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		c.evictLocked()
	}
	c.m[key] = ttlEntry[V]{val: val, exp: c.now().Add(c.ttl)}
}

// evictLocked frees room: drop all expired entries, and if still at
// capacity drop one arbitrary entry. Caller holds c.mu.
func (c *ttlCache[V]) evictLocked() {
	now := c.now()
	for k, e := range c.m {
		if now.After(e.exp) {
			delete(c.m, k)
		}
	}
	if len(c.m) < c.max {
		return
	}
	for k := range c.m {
		delete(c.m, k)
		return
	}
}
