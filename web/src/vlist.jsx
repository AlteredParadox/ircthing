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

import { useLayoutEffect, useMemo, useRef, useState } from "preact/hooks";
import { anchorId, computeWindow, Geometry, pinnedAfterScroll, prependedCount } from "./vmath.js";

const SCROLL_SETTLE_MS = 160;

// writeScroll is the only path for code-owned scroll changes. Remembering the
// browser-clamped result lets handleScroll distinguish those events from a
// Firefox thumb/wheel movement without a timeout that could swallow real input.
function writeScroll(sc, position, top) {
	sc.scrollTop = top;
	const actual = sc.scrollTop;
	position.current.internalTop = actual;
	position.current.knownTop = actual;
	position.current.owned = true;
}

// A native thumb can move scrollTop before its queued scroll event reaches
// Preact. If layout work lands in that interval, do not let a stale pinned ref
// overwrite the physical user movement. Layout growth alone leaves scrollTop
// unchanged (overflow-anchor is disabled), while shrink clamping lands at the
// new bottom, so an untagged changed position away from bottom is user intent.
function canRestoreTail(sc, position, pinned, tailNeedsRestore, reportPinned) {
	if (!pinned.current) return false;
	// A keyed remount can inherit Firefox's saved nonzero scrollTop. Until this
	// component has placed a nonempty list itself, that position is not user
	// intent and must not veto the initial jump to the live tail.
	if (!position.current.owned) return true;
	const top = sc.scrollTop;
	const maxTop = Math.max(0, sc.scrollHeight - sc.clientHeight);
	const internal = position.current.internalTop !== null &&
		Math.abs(top - position.current.internalTop) <= 1;
	if (maxTop - top >= 40 && !internal && Math.abs(top - position.current.knownTop) > 1) {
		pinned.current = false;
		tailNeedsRestore.current = false;
		position.current.internalTop = null;
		position.current.knownTop = top;
		reportPinned?.(false);
		return false;
	}
	return true;
}

// Geometry offsets start after the scroll container's top padding and header.
// Keep that origin consistent for windowing, focus centering, and deciding
// whether a resized row is wholly above the visible viewport.
function contentOrigin(sc, header) {
	const paddingTop = Number.parseFloat(getComputedStyle(sc).paddingTop) || 0;
	return paddingTop + (header?.offsetHeight || 0);
}

// centerRow scrolls so row `idx` sits centered in the viewport (used for a
// search-jump focus).
function centerRow(sc, position, geo, origin, idx) {
	const rowH = geo.offsetOf(idx + 1) - geo.offsetOf(idx);
	writeScroll(sc, position, origin + geo.offsetOf(idx) - (sc.clientHeight - rowH) / 2);
}

// anchorPrepended measures the k just-prepended rows (in the DOM, pre-paint)
// and compensates scrollTop by their real height, so the previously-visible
// content stays put with no jump instead of using the density/font-blind
// estimate.
function anchorPrepended(sc, position, geo, items, rowEls, k) {
	for (let i = 0; i < k; i++) {
		const node = rowEls.get(items[i].id);
		if (node) geo.measure(items[i].id, node.offsetHeight);
	}
	writeScroll(sc, position, sc.scrollTop + geo.offsetOf(k));
}

// VirtualList: windowed rendering for the message list. Only the rows
// intersecting the viewport (plus `overscan` px each side) exist in the
// DOM; spacer divs stand in for everything else, so 50k+ loaded messages
// stay smooth.
//
// Variable heights: rows start at estimate(item) and are corrected by a
// ResizeObserver as they render. When a row above the viewport changes
// height (measurement or late wrap), scrollTop is compensated in the same
// batch so visible content doesn't jump.
//
// Chat semantics: the list follows the bottom while pinned (user at the
// bottom); scrolling near the top calls onNearTop() so the parent can
// prepend an older page, and the prepend is anchored so the viewport
// keeps showing the same messages.
//
// Callers should key the component by buffer so switching buffers
// remounts with fresh state.
export function VirtualList({
	items,
	renderItem,
	estimate,
	header,
	onNearTop,
	onPinned,
	focusId,
	overscan = 600,
	nearTopPx = 400,
}) {
	const scroller = useRef(null);
	const headerEl = useRef(null);
	const geo = useMemo(() => new Geometry(estimate), []);
	const [, setTick] = useState(0);
	const bump = () => setTick((t) => t + 1);

	const pinned = useRef(true);
	const prevFirstId = useRef(null);
	const pendingPrepend = useRef(0);
	const pendingFocus = useRef(false);
	const pendingFocusUnpin = useRef(false);
	const prevFocus = useRef(undefined);
	const headerHeight = useRef(null);
	const rowEls = useRef(new Map()); // id -> element
	const position = useRef({ knownTop: 0, internalTop: null, owned: false });
	const prevItems = useRef(null);
	const tailNeedsRestore = useRef(true);
	const onPinnedRef = useRef(onPinned);
	onPinnedRef.current = onPinned;
	if (prevItems.current !== items) {
		prevItems.current = items;
		tailNeedsRestore.current = true;
	}

	geo.setItems(items);

	// A new focus target (a search jump) unpins and requests a scroll-to
	// once the item is present. Set synchronously so the window below
	// renders around the target rather than the tail.
	const focusIdx = focusId == null ? -1 : geo.indexOf(focusId);
	if (focusId !== prevFocus.current) {
		prevFocus.current = focusId;
		pendingFocus.current = focusId != null;
	}
	// A search page and its focus id can arrive in separate renders. Keep the
	// request armed until the target exists instead of consuming it early.
	if (pendingFocus.current && focusId != null && focusIdx !== -1) {
		if (pinned.current) pendingFocusUnpin.current = true;
		pinned.current = false;
	}

	// Detect a prepend during render; the layout effect below compensates
	// scrollTop after the new spacers apply.
	const k = prependedCount(prevFirstId.current, items);
	if (k > 0) pendingPrepend.current = k;
	// Anchor on the top row's stable identity (collapse-run aware) so the
	// prepend compensation survives a lone presence event folding into a run.
	prevFirstId.current = items.length ? anchorId(items[0]) : null;

	// Current window from the last known scroll position.
	const el = scroller.current;
	const origin = el ? contentOrigin(el, headerEl.current) : 0;
	const viewTop = el ? el.scrollTop - origin : 0;
	const viewH = el ? el.clientHeight : 800;
	const { start, end } = computeWindow(geo, items.length, {
		viewTop, viewH, overscan,
		pinned: pinned.current, focusing: pendingFocus.current, focusIdx,
		prepended: pendingPrepend.current,
	});

	const topPad = geo.offsetOf(start);
	const bottomPad = geo.total() - geo.offsetOf(end);

	// One ResizeObserver for every row: correct heights, keep the
	// viewport still, then re-render with the fixed geometry.
	const ro = useMemo(
		() =>
			new ResizeObserver((entries) => {
				const sc = scroller.current;
				if (!sc) return;
				// Two passes so the offset table is rebuilt once, not per
				// entry: classify rows against pre-measure geometry, then
				// apply all measurements.
				const batch = classifyEntries(
					entries,
					geo,
					sc.scrollTop,
					contentOrigin(sc, headerEl.current),
				);
				let above = 0;
				let changed = false;
				for (const b of batch) {
					const d = geo.measure(b.id, b.h);
					if (d !== 0) {
						changed = true;
						if (b.above) above += d;
					}
				}
				if (!changed) return;
				if (pinned.current) {
					tailNeedsRestore.current = true;
					if (canRestoreTail(
						sc, position, pinned, tailNeedsRestore, onPinnedRef.current,
					)) writeScroll(sc, position, sc.scrollHeight);
				}
				else if (above !== 0) writeScroll(sc, position, sc.scrollTop + above);
				bump();
			}),
		[],
	);
	useLayoutEffect(() => () => ro.disconnect(), []);

	// Container dimensions can change without a component render (window
	// resize, mobile browser chrome, composer/sidebar layout). Observe them so
	// a tail-following viewport remains at the tail; width changes also
	// invalidate row measurements because wrapping changes.
	useLayoutEffect(() => {
		const sc = scroller.current;
		if (!sc) return;
		let lastWidth = sc.clientWidth;
		let lastHeight = sc.clientHeight;
		const observer = new ResizeObserver(() => {
			const nextWidth = sc.clientWidth;
			const nextHeight = sc.clientHeight;
			if (nextWidth === lastWidth && nextHeight === lastHeight) return;
			const widthChanged = nextWidth !== lastWidth;
			lastWidth = nextWidth;
			lastHeight = nextHeight;
			if (widthChanged) geo.clearMeasured();
			if (pinned.current) {
				tailNeedsRestore.current = true;
				if (canRestoreTail(
					sc, position, pinned, tailNeedsRestore, onPinnedRef.current,
				)) writeScroll(sc, position, sc.scrollHeight);
			}
			bump();
		});
		observer.observe(sc);
		return () => observer.disconnect();
	}, []);

	// Emit the initial pinned state once after mount. The parent (Chat) is
	// NOT remounted per buffer, so its own pinned tracking would otherwise
	// carry a stale value from the previous buffer and silently suppress
	// read-marker updates; this list IS keyed per buffer, so it knows the
	// truth — true at the tail, false for a focus (search) jump.
	useLayoutEffect(() => {
		onPinnedRef.current?.(pinned.current);
		pendingFocusUnpin.current = false;
	}, []);

	// Row ref callback: (un)observe and track elements by id.
	function rowRef(id) {
		return (node) => {
			const old = rowEls.current.get(id);
			if (old && old !== node) ro.unobserve(old);
			if (node) {
				rowEls.current.set(id, node);
				ro.observe(node);
			} else {
				rowEls.current.delete(id);
			}
		};
	}

	// Loading a page while Firefox still owns a native scrollbar-thumb drag
	// makes it recompute scrollTop against the newly-grown scrollHeight and can
	// cascade the viewport toward the beginning. Wait for the browser's native
	// scrollend before loading. Older engines use a quiet-period fallback.
	const onNearTopRef = useRef(onNearTop);
	onNearTopRef.current = onNearTop;
	const nearTopPxRef = useRef(nearTopPx);
	nearTopPxRef.current = nearTopPx;
	const nativeScrollEnd = useRef(false);
	const settleTimer = useRef(null);
	const fallbackActive = useRef(false);
	const userScrollPending = useRef(false);

	function clearUserScroll() {
		if (settleTimer.current !== null) {
			clearTimeout(settleTimer.current);
			settleTimer.current = null;
		}
		fallbackActive.current = false;
		userScrollPending.current = false;
	}

	function finishUserScroll() {
		if (settleTimer.current !== null) clearTimeout(settleTimer.current);
		settleTimer.current = null;
		fallbackActive.current = false;
		if (!userScrollPending.current) return;
		userScrollPending.current = false;
		const sc = scroller.current;
		if (sc && !pinned.current && sc.scrollTop < nearTopPxRef.current) {
			onNearTopRef.current?.();
		}
	}

	function armSettleFallback() {
		userScrollPending.current = true;
		fallbackActive.current = true;
		if (settleTimer.current !== null) clearTimeout(settleTimer.current);
		settleTimer.current = setTimeout(finishUserScroll, SCROLL_SETTLE_MS);
	}

	function finishNativeScroll() {
		// Wheel/trackpad motion owns a quiet timer because Firefox can emit an
		// early scrollend while its remaining wheel scroll events are still
		// arriving. Native thumb drags never arm that timer.
		if (!fallbackActive.current) finishUserScroll();
	}

	useLayoutEffect(() => {
		const sc = scroller.current;
		if (!sc) return;
		nativeScrollEnd.current = "onscrollend" in sc;
		if (nativeScrollEnd.current) {
			sc.addEventListener("scrollend", finishNativeScroll);
		}
		return () => {
			sc.removeEventListener("scrollend", finishNativeScroll);
			if (settleTimer.current !== null) clearTimeout(settleTimer.current);
		};
	}, []);

	const scrollScheduled = useRef(false);
	function handleScroll() {
		const sc = scroller.current;
		if (!sc) return;
		const top = sc.scrollTop;
		const maxTop = Math.max(0, sc.scrollHeight - sc.clientHeight);
		const internal = position.current.internalTop !== null &&
			Math.abs(top - position.current.internalTop) <= 1;
		position.current.internalTop = null;

		const wasPinned = pinned.current;
		const nowPinned = pinnedAfterScroll(wasPinned, internal, top, maxTop);
		if (nowPinned !== wasPinned) {
			pinned.current = nowPinned;
			onPinnedRef.current?.(nowPinned);
		}
		// A row grew underneath a tail-following viewport. Preserve the intent
		// immediately instead of treating the temporary gap as a user unpin.
		if (nowPinned && maxTop - top >= 40) {
			writeScroll(sc, position, sc.scrollHeight);
		}

		if (!internal) {
			userScrollPending.current = true;
			if (!nativeScrollEnd.current || fallbackActive.current) armSettleFallback();
		}
		position.current.knownTop = sc.scrollTop;
		// One re-render per frame, however many scroll events arrive.
		if (scrollScheduled.current) return;
		scrollScheduled.current = true;
		requestAnimationFrame(() => {
			scrollScheduled.current = false;
			bump();
		});
	}

	// Firefox 140 advertises scrollend but can emit its last such event before
	// the last wheel-driven scroll event. Wheel events are not generated by a
	// native scrollbar-thumb drag, so this fallback closes that hole without
	// reintroducing a timer that can fire while a held thumb is stationary.
	function handleWheel(e) {
		if (e.ctrlKey || e.deltaY === 0) return; // browser zoom / horizontal gesture
		// Upward wheel intent precedes the browser's scroll event. Record it now
		// so a simultaneous append/preview ResizeObserver commit cannot restore
		// the tail in the small event-delivery window.
		if (e.deltaY < 0 && pinned.current) {
			pinned.current = false;
			tailNeedsRestore.current = false;
			position.current.internalTop = null;
			position.current.knownTop = scroller.current?.scrollTop || 0;
			onPinnedRef.current?.(false);
		}
		armSettleFallback();
	}

	// After every commit: apply focus/prepend/header anchoring and restore a
	// tail position only when an actual geometry change requested it.
	useLayoutEffect(() => {
		const sc = scroller.current;
		if (!sc) return;
		const hh = headerEl.current?.offsetHeight || 0;
		const headerDelta = headerHeight.current === null ? 0 : hh - headerHeight.current;
		headerHeight.current = hh;
		if (pinned.current && headerDelta !== 0) tailNeedsRestore.current = true;
		if (pendingFocus.current && focusIdx !== -1) {
			// A focus jump replaces the window wholesale, so discard any prepend
			// detected in the same commit — otherwise it fires on the next commit
			// and scrolls the view off the focused row.
			pendingPrepend.current = 0;
			tailNeedsRestore.current = false;
			clearUserScroll();
			centerRow(sc, position, geo, contentOrigin(sc, headerEl.current), focusIdx);
			pendingFocus.current = false;
			if (pendingFocusUnpin.current) {
				pendingFocusUnpin.current = false;
				onPinnedRef.current?.(false);
			}
			if (!pinned.current && sc.scrollHeight - sc.clientHeight < 40) {
				pinned.current = true;
				onPinnedRef.current?.(true);
			}
			return;
		}
		if (pendingPrepend.current > 0) {
			if (pinned.current) {
				// The user returned to the live tail while this page was in flight;
				// there is no historical viewport left to preserve.
				tailNeedsRestore.current = true;
			} else {
				anchorPrepended(sc, position, geo, items, rowEls.current, pendingPrepend.current);
				if (headerDelta !== 0) writeScroll(sc, position, sc.scrollTop + headerDelta);
			}
			pendingPrepend.current = 0;
		} else if (!pinned.current && headerDelta !== 0) {
			writeScroll(sc, position, sc.scrollTop + headerDelta);
		}
		// Tail-following is an intent, but only write when mount/items/geometry
		// actually moved. An unrelated parent render can land between native
		// thumb movement and its queued scroll event and must not erase it.
		if (pinned.current && tailNeedsRestore.current) {
			if (canRestoreTail(
				sc, position, pinned, tailNeedsRestore, onPinnedRef.current,
			)) writeScroll(sc, position, sc.scrollHeight);
			// Keep the mount exception alive across an empty/loading list; the
			// first commit containing messages is the authoritative placement.
			if (items.length === 0) position.current.owned = false;
			tailNeedsRestore.current = false;
		} else if (!pinned.current) {
			tailNeedsRestore.current = false;
		}
		// A list whose content is shorter than the viewport never fires a scroll
		// event, so handleScroll can't flip `pinned` true. Signal it here so a
		// search-jump into a buffer whose whole window fits on screen still
		// reports pinned — otherwise the parent never reloads the live tail and
		// incoming messages are silently blocked (atTail stays false) until the
		// buffer is manually re-selected. Harmless when already at the tail
		// (the parent's reloadTail no-ops on an atTail buffer).
		if (!pendingFocus.current && !pinned.current &&
			sc.scrollHeight - sc.clientHeight < 40) {
			pinned.current = true;
			onPinnedRef.current?.(true);
		}
	});

	return (
		<div class="msgs scroll" ref={scroller} onScroll={handleScroll} onWheel={handleWheel}>
			<div ref={headerEl}>{header}</div>
			<div style={{ height: topPad }} />
			{items.slice(start, end).map((item, j) => (
				<div key={item.id} data-vid={item.id} ref={rowRef(item.id)}>
					{renderItem(item, start + j)}
				</div>
			))}
			<div style={{ height: bottomPad }} />
		</div>
	);
}

// classifyEntries turns ResizeObserver entries into measurement records,
// noting which rows sit above the viewport (their growth must be
// compensated in scrollTop).
function classifyEntries(entries, geo, scrollTop, origin) {
	const batch = [];
	for (const e of entries) {
		const id = idOf(e.target.dataset.vid);
		if (id === null) continue;
		const h = e.borderBoxSize?.[0]?.blockSize ?? e.target.offsetHeight;
		if (h === 0) continue; // detached row
		const i = geo.indexOf(id);
		batch.push({ id, h, above: i !== -1 && geo.offsetOf(i + 1) + origin <= scrollTop });
	}
	return batch;
}

function idOf(s) {
	if (s === undefined) return null;
	const n = Number(s);
	return Number.isNaN(n) ? s : n;
}
