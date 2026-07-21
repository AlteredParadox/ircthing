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

// Geometry model for the virtualized message list — pure logic, no DOM,
// so node:test can cover it. The list keeps every loaded item's height as
// either a measurement (reported by ResizeObserver) or an estimate, and
// answers: how tall is everything, where does item i sit, and which items
// intersect a viewport.
//
// Offsets are rebuilt lazily and in full when anything changes: a rebuild
// is one O(n) pass over ~50k floats (well under a millisecond), which
// beats maintaining an incremental structure nobody can read at 2 a.m.

// isAppendOf reports whether `items` is `old` with only rows appended at
// the end (same head, same old tail) — the fast path for a new message.
function isAppendOf(old, items) {
	return (
		old.length > 0 &&
		items.length >= old.length &&
		items[0] === old[0] &&
		items[old.length - 1] === old.at(-1)
	);
}

export class Geometry {
	constructor(estimate) {
		this.estimate = estimate;
		this.measured = new Map(); // item id -> px
		this.items = [];
		this.index = new Map(); // item id -> index
		this.offsets = new Float64Array(1);
		this.dirty = true;
	}

	setItems(items, forceRebuild = false) {
		const old = this.items;
		if (old === items) return;
		// Append fast path: the common case is one message arriving at the
		// end of a 50k list — extend the index instead of rebuilding it.
		if (!forceRebuild && isAppendOf(old, items)) {
			for (let i = old.length; i < items.length; i++) this.index.set(items[i].id, i);
		} else {
			this.rebuildIndex(items);
		}
		this.items = items;
		this.dirty = true;
	}

	// rebuildIndex rebuilds the id->index map from scratch and, since rows
	// may have been trimmed/removed, drops cached measurements for ids no
	// longer present so `measured` cannot grow without bound.
	rebuildIndex(items) {
		this.index = new Map();
		for (let i = 0; i < items.length; i++) this.index.set(items[i].id, i);
		if (this.measured.size > this.index.size) {
			for (const id of this.measured.keys()) {
				if (!this.index.has(id)) this.measured.delete(id);
			}
		}
	}

	// clearMeasured drops all measurements (container width changed, so
	// every row may rewrap).
	clearMeasured() {
		this.measured.clear();
		this.dirty = true;
	}

	// Rows can keep the same stable id while their rendered content changes
	// (redaction, collapse expansion, preview metadata). Drop only those stale
	// measurements; unchanged object identities retain their useful heights.
	invalidateChanged(items) {
		let changed = 0;
		for (const it of items) {
			const i = this.index.get(it.id);
			if (i === undefined || this.items[i] === it) continue;
			this.measured.delete(it.id);
			changed++;
		}
		if (changed) this.dirty = true;
		return changed;
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

// Capture and restore a stable viewport identity across global measurement
// invalidation (width/font/density/custom CSS). Keep the EXACT rendered row id
// separately from its collapse-member fallback: while a collapse run is open,
// the fallback id is also the id of a real child row. Looking that id up first
// would move a viewport anchored on the summary all the way to the last child.
// The previous immutable list provides a deterministic nearest-survivor
// fallback when filtering removes the captured row itself (ignore/status-mode
// changes), even when a long contiguous run disappears.
export function captureViewportAnchor(geo, items, viewTop) {
	if (!items.length) return null;
	const i = geo.range(viewTop, viewTop).start;
	const ref = (item) => ({ id: item.id, member: anchorId(item) });
	return {
		...ref(items[i]),
		intra: Math.max(0, viewTop - geo.offsetOf(i)),
		// Retaining the immutable old list for one layout commit avoids copying
		// thousands of ids. restore can scan outward only if the exact row went
		// away, finding a survivor even across a long filtered status/ignore run.
		source: items,
		index: i,
	};
}

export function restoreViewportAnchor(geo, anchor) {
	if (!anchor) return null;
	const locate = (ref) => {
		// Exact top-level identity always wins. This is what distinguishes an
		// expanded run's summary row from its real last child.
		let i = geo.indexOf(ref.id);
		if (i !== -1) return i;
		// A status-mode change may replace a real presence row with a collapsed
		// synthetic row, or extend a run and change its synthetic id.
		return geo.items.findIndex((it) =>
			it.collapse?.some((ev) => ev.id === ref.member));
	};
	let i = locate(anchor);
	if (i === -1 && anchor.source) {
		const ref = (item) => ({ id: item.id, member: anchorId(item) });
		for (let distance = 1; distance < anchor.source.length; distance++) {
			// Prefer the following row at each distance: when the anchored row was
			// filtered, it naturally moves into the vacated viewport position.
			const after = anchor.index + distance;
			if (after < anchor.source.length) i = locate(ref(anchor.source[after]));
			if (i !== -1) break;
			const before = anchor.index - distance;
			if (before >= 0) i = locate(ref(anchor.source[before]));
			if (i !== -1) break;
		}
	}
	if (i === -1) return null;
	const height = geo.offsetOf(i + 1) - geo.offsetOf(i);
	return geo.offsetOf(i) + Math.min(anchor.intra, Math.max(0, height - 1));
}

// hasNonAppendIdentityChange reports a structural list rewrite: filtering,
// reordering, trimming, or folding rows. A stable-id prefix followed only by
// new tail rows is the ordinary live-message path and deliberately returns
// false; anchoring that path would fight normal scrolling for every append.
// anchorId makes this comparison aware of collapse/show transitions.
export function hasNonAppendIdentityChange(oldItems, items) {
	if (oldItems.length === 0) return false;
	const common = Math.min(oldItems.length, items.length);
	for (let i = 0; i < common; i++) {
		if (anchorId(oldItems[i]) !== anchorId(items[i])) return true;
	}
	return items.length < oldItems.length;
}

// computeWindow picks the [start, end) row slice to render this frame. While
// following the live tail it deliberately replaces the stale viewport range
// with a bounded tail range: on first paint scrollTop is still zero, and
// unioning that top range with the tail would render the entire buffer before
// the layout effect can move the scrollbar to the bottom.
export function computeWindow(
	geo,
	itemCount,
	{ viewTop, viewH, overscan, pinned, focusing, focusIdx, prepended },
) {
	let { start, end } = geo.range(viewTop - overscan, viewTop + viewH + overscan);
	if (pinned && itemCount > 0) {
		const bottom = geo.total();
		// Estimates can be taller than compact rendered rows. Include at least
		// another half viewport so a tall display cannot briefly expose a blank
		// band while ResizeObserver replaces estimates with real heights.
		const tailOverscan = Math.max(overscan, viewH / 2);
		({ start, end } = geo.range(
			Math.max(0, bottom - viewH - tailOverscan),
			bottom,
		));
		end = itemCount;
	}
	if (focusing && focusIdx !== -1) {
		// Force the target and its neighbors into the DOM so the layout effect
		// can scroll to a rendered, measurable row.
		start = Math.min(start, Math.max(0, focusIdx - 12));
		end = Math.max(end, Math.min(itemCount, focusIdx + 12));
	}
	if (!pinned && prepended > 0) {
		// Render the whole new page so prepend anchoring can use real heights,
		// plus the viewport where the old rows will land after compensation.
		// Without the shifted range, the layout effect scrolls into a spacer
		// and shows a blank frame before the next render catches up.
		const shiftedTop = viewTop + geo.offsetOf(prepended);
		const shifted = geo.range(
			shiftedTop - overscan,
			shiftedTop + viewH + overscan,
		);
		start = 0;
		end = Math.max(end, shifted.end, Math.min(itemCount, prepended));
	}
	return { start, end };
}

// pinnedAfterScroll preserves tail intent for a scroll event caused by one of
// our tagged writes. Every unmatched event away from the bottom is user/browser
// movement and unpins even if concurrent layout growth made its absolute
// scrollTop increase while a Firefox thumb moved toward older content.
export function pinnedAfterScroll(wasPinned, internal, top, maxTop, threshold = 40) {
	if (maxTop - top < threshold) return true;
	return internal && wasPinned;
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
