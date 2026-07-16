// Geometry model for the virtualized message list — pure logic, no DOM,
// so node:test can cover it. The list keeps every loaded item's height as
// either a measurement (reported by ResizeObserver) or an estimate, and
// answers: how tall is everything, where does item i sit, and which items
// intersect a viewport.
//
// Offsets are rebuilt lazily and in full when anything changes: a rebuild
// is one O(n) pass over ~50k floats (well under a millisecond), which
// beats maintaining an incremental structure nobody can read at 2 a.m.

export class Geometry {
	constructor(estimate) {
		this.estimate = estimate;
		this.measured = new Map(); // item id -> px
		this.items = [];
		this.index = new Map(); // item id -> index
		this.offsets = new Float64Array(1);
		this.dirty = true;
	}

	setItems(items) {
		const old = this.items;
		if (old === items) return;
		// Append fast path: the common case is one message arriving at the
		// end of a 50k list — extend the index instead of rebuilding it.
		const appended =
			old.length > 0 &&
			items.length >= old.length &&
			items[0] === old[0] &&
			items[old.length - 1] === old.at(-1);
		if (appended) {
			for (let i = old.length; i < items.length; i++) this.index.set(items[i].id, i);
		} else {
			this.index = new Map();
			for (let i = 0; i < items.length; i++) this.index.set(items[i].id, i);
			// Not an append: rows may have been trimmed/removed. Drop
			// their cached measurements so `measured` cannot grow without
			// bound within a single long-lived buffer.
			if (this.measured.size > this.index.size) {
				for (const id of this.measured.keys()) {
					if (!this.index.has(id)) this.measured.delete(id);
				}
			}
		}
		this.items = items;
		this.dirty = true;
	}

	// clearMeasured drops all measurements (container width changed, so
	// every row may rewrap).
	clearMeasured() {
		this.measured.clear();
		this.dirty = true;
	}

	heightAt(i) {
		const it = this.items[i];
		const m = this.measured.get(it.id);
		return m === undefined ? this.estimate(it) : m;
	}

	// measure records a row's real height and returns the delta against
	// the height previously used for it (measurement or estimate). The
	// caller compensates scrollTop by the summed deltas of rows above the
	// viewport so content on screen doesn't jump.
	measure(id, px) {
		const i = this.index.get(id);
		if (i === undefined) return 0; // trimmed away meanwhile
		const prev = this.measured.get(id) ?? this.estimate(this.items[i]);
		if (px === prev) return 0;
		this.measured.set(id, px);
		this.dirty = true;
		return px - prev;
	}

	ensure() {
		if (!this.dirty) return;
		const n = this.items.length;
		const off = new Float64Array(n + 1);
		for (let i = 0; i < n; i++) off[i + 1] = off[i] + this.heightAt(i);
		this.offsets = off;
		this.dirty = false;
	}

	total() {
		this.ensure();
		return this.offsets[this.items.length];
	}

	offsetOf(i) {
		this.ensure();
		return this.offsets[Math.max(0, Math.min(i, this.items.length))];
	}

	indexOf(id) {
		const i = this.index.get(id);
		return i === undefined ? -1 : i;
	}

	// range returns the item window [start, end) covering [y0, y1),
	// clamped to the list. Empty list yields [0, 0).
	range(y0, y1) {
		this.ensure();
		const n = this.items.length;
		if (n === 0) return { start: 0, end: 0 };
		return {
			start: findIndex(this.offsets, n, Math.max(0, y0)),
			end: Math.min(n, findIndex(this.offsets, n, Math.max(0, y1)) + 1),
		};
	}
}

// findIndex: largest i in [0, n-1] with offsets[i] <= y (binary search).
export function findIndex(offsets, n, y) {
	let lo = 0;
	let hi = n - 1;
	while (lo < hi) {
		const mid = (lo + hi + 1) >> 1;
		if (offsets[mid] <= y) lo = mid;
		else hi = mid - 1;
	}
	return lo;
}

// anchorId is a shown row's identity that stays stable across
// collapse/expand transitions: a collapsed run is keyed by its LAST
// underlying event (which a top-prepend never moves). So a lone presence
// event that folds into a run after an older page loads still matches its
// own event id — the row's top-level id changed (e.g. 42 -> "clp-42"), but
// its anchor did not. Non-collapse rows anchor on their own id.
export function anchorId(item) {
	return item.collapse ? item.collapse[item.collapse.length - 1].id : item.id;
}

// prependedCount: how many items were added in front, detected by where
// the previously-first row's anchor landed in the new list. 0 when this
// wasn't a prepend (fresh load, append, or trim). Anchoring (rather than
// matching the raw top-level id) keeps the scroll compensation correct in
// collapse mode, where a prepend can change the top row's derived id.
export function prependedCount(prevAnchor, items) {
	if (prevAnchor == null || items.length === 0 || anchorId(items[0]) === prevAnchor) return 0;
	for (let i = 1; i < items.length; i++) {
		if (anchorId(items[i]) === prevAnchor) return i;
	}
	return 0;
}

// estimateMsgHeight: one 21px line plus 6px row padding, plus wrapped
// lines guessed from length (~90 chars/line at typical widths). Only a
// starting point — real heights arrive from ResizeObserver.
export function estimateMsgHeight(text) {
	return 27 + 21 * Math.floor((text ? text.length : 0) / 90);
}
