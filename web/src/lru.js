// A small bounded LRU with per-entry TTL, for the preview cache: an
// unbounded Map would grow for the lifetime of the page as scrollback
// surfaces unique links (and failed lookups are attacker-cheap keys).
// Backed by Map's insertion order: a get refreshes recency by
// re-inserting; when full, the oldest entry is evicted.

export class LRU {
	// onEvict(value) is called when an entry leaves the cache (evicted, expired,
	// or replaced) — used to revoke blob object URLs so they don't leak.
	constructor(max, ttlMs, onEvict) {
		this.max = max;
		this.ttlMs = ttlMs;
		this.onEvict = onEvict;
		this.map = new Map(); // key -> { v, exp }
	}

	// get returns the cached value, or undefined when absent/expired.
	// Note: stored values may themselves be null (a cached failure).
	get(key) {
		const e = this.map.get(key);
		if (!e) return undefined;
		if (Date.now() > e.exp) {
			this.map.delete(key);
			this.onEvict?.(e.v);
			return undefined;
		}
		// Refresh recency.
		this.map.delete(key);
		this.map.set(key, e);
		return e.v;
	}

	has(key) {
		return this.get(key) !== undefined;
	}

	set(key, v, ttlMs = this.ttlMs) {
		const prev = this.map.get(key);
		if (prev) {
			this.map.delete(key);
			this.onEvict?.(prev.v);
		} else if (this.map.size >= this.max) {
			const oldestKey = this.map.keys().next().value;
			const oldest = this.map.get(oldestKey);
			this.map.delete(oldestKey);
			this.onEvict?.(oldest.v);
		}
		this.map.set(key, { v, exp: Date.now() + ttlMs });
	}

	get size() {
		return this.map.size;
	}
}
