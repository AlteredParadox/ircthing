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
import {
	anchorId,
	captureViewportAnchor,
	computeWindow,
	Geometry,
	hasNonAppendIdentityChange,
	pinnedAfterScroll,
	prependedCount,
	restoreViewportAnchor,
	scrollSettleStep,
	touchGestureEnds,
} from "./vmath.js";

const SCROLL_SETTLE_MS = 160;

// writeScroll is the only path for code-owned scroll changes. Remembering the
// browser-clamped result lets handleScroll distinguish those events from a
// Firefox thumb/wheel movement without a timeout that could swallow real input.
// A write whose clamped target is within 1px of the current position skips the
// DOM assignment: iOS WebKit cancels an active touch pan/momentum on ANY
// scrollTop write, even one that does not change the value. Writes that do
// move (prepend compensation, tail restore) stay exact.
function writeScroll(sc, position, top) {
	const maxTop = Math.max(0, sc.scrollHeight - sc.clientHeight);
	const target = Math.min(Math.max(0, top), maxTop);
	if (Math.abs(sc.scrollTop - target) >= 1) sc.scrollTop = top;
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
	layoutKey,
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
	const prevLayoutKey = useRef(layoutKey);
	const pendingLayoutAnchor = useRef(null);
	const tailNeedsRestore = useRef(true);
	const onPinnedRef = useRef(onPinned);
	onPinnedRef.current = onPinned;
	// Touch gesture state. touchActive mirrors "a finger is on the glass";
	// touchGesture latches from touchstart until the gesture INCLUDING its
	// momentum phase settles (touchGestureEnds in vmath.js decides when).
	// While latched, every code-owned scrollTop write is deferred, because on
	// iOS WebKit any programmatic write cancels the pan/momentum — a live
	// append's tail pin, a row measurement's compensation, or a near-top
	// prepend landing mid-gesture made the list stop dead under the finger.
	const touchActive = useRef(false);
	const touchScrolled = useRef(false); // any scroll event while a finger was down
	const touchGesture = useRef(false);
	const deferredMeasure = useRef(new Map()); // id -> px, applied at gesture settle
	const lastWindow = useRef(null); // { start, end } rendered by the last commit
	const el = scroller.current;
	const origin = el ? contentOrigin(el, headerEl.current) : 0;
	const actualViewTop = el ? Math.max(0, el.scrollTop - origin) : 0;

	// A layout-affecting preference can change every row without changing its
	// item identity. Capture the current row before dropping measurements, then
	// render around its new estimated offset in this same commit. Doing this
	// during render (the layoutKey prop caused the render) avoids one frame whose
	// spacers still use stale font/density/CSS heights.
	function syncLayoutKey() {
		if (prevLayoutKey.current === layoutKey) return;
		if (!pinned.current && el) {
			pendingLayoutAnchor.current = captureViewportAnchor(geo, geo.items, actualViewTop);
		}
		geo.clearMeasured();
		prevLayoutKey.current = layoutKey;
		if (pinned.current) tailNeedsRestore.current = true;
	}

	// syncItems absorbs a changed items prop and reports whether the id->index
	// map must be rebuilt from scratch (a structural rewrite).
	function syncItems() {
		if (prevItems.current === items) return false;
		// Same-id rows can change their rendered body (redaction, collapsed-run
		// membership, preview data). A filter/collapse rewrite can instead remove
		// rows while every survivor keeps the same object identity, so detect that
		// from the stable-id sequence too. Ordinary tail appends remain unanchored.
		const oldItems = geo.items;
		const contentAnchor = !pinned.current && el
			? captureViewportAnchor(geo, oldItems, actualViewTop)
			: null;
		const structuralChange = hasNonAppendIdentityChange(oldItems, items);
		const tailAppend = !structuralChange && items.length > oldItems.length;
		// Collapse mode rebuilds synthetic row objects when the source list grows,
		// even when their stable ids/content did not change. Keep those useful
		// measurements on a pure tail append; visible real changes are still
		// corrected by ResizeObserver.
		const changedRows = tailAppend ? 0 : geo.invalidateChanged(items);
		if ((structuralChange || (changedRows > 0 && !tailAppend)) &&
			contentAnchor && !pendingLayoutAnchor.current) {
			pendingLayoutAnchor.current = contentAnchor;
		}
		prevItems.current = items;
		tailNeedsRestore.current = true;
		return structuralChange;
	}

	// A new focus target (a search jump) unpins and requests a scroll-to
	// once the item is present. Set synchronously so the window below
	// renders around the target rather than the tail.
	function syncFocus(focusIdx) {
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
	}

	syncLayoutKey();
	geo.setItems(items, syncItems());

	const focusIdx = focusId == null ? -1 : geo.indexOf(focusId);
	syncFocus(focusIdx);

	// Detect a prepend during render; the layout effect below compensates
	// scrollTop after the new spacers apply.
	const k = prependedCount(prevFirstId.current, items);
	if (k > 0) pendingPrepend.current = k;
	// Anchor on the top row's stable identity (collapse-run aware) so the
	// prepend compensation survives a lone presence event folding into a run.
	prevFirstId.current = items.length ? anchorId(items[0]) : null;

	// Current window from the last known scroll position.
	const anchorTop = pinned.current
		? null
		: restoreViewportAnchor(geo, pendingLayoutAnchor.current);
	const viewTop = anchorTop ?? actualViewTop;
	const viewH = el ? el.clientHeight : 800;
	const { start, end } = computeWindow(geo, items.length, {
		viewTop, viewH, overscan,
		pinned: pinned.current, focusing: pendingFocus.current, focusIdx,
		prepended: pendingPrepend.current,
	});

	const topPad = geo.offsetOf(start);
	const bottomPad = geo.total() - geo.offsetOf(end);
	lastWindow.current = { start, end };

	// One ResizeObserver for every row: correct heights, keep the
	// viewport still, then re-render with the fixed geometry.
	const ro = useMemo(
		() =>
			new ResizeObserver((entries) => {
				const sc = scroller.current;
				if (!sc) return;
				// Mid touch gesture, applying a measurement means either writing
				// scrollTop (cancels the iOS pan/momentum) or letting the spacers
				// shift the content under the finger. Queue the heights instead;
				// the gesture-settle path applies them with one compensation
				// write. Until then windowing keeps using the estimates the
				// current spacers were built from, so nothing on screen moves.
				if (touchGesture.current) {
					for (const b of classifyEntries(entries, geo, 0, 0)) {
						deferredMeasure.current.set(b.id, b.h);
					}
					return;
				}
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
			if (widthChanged) {
				if (!pinned.current) {
					pendingLayoutAnchor.current = captureViewportAnchor(
						geo,
						geo.items,
						Math.max(0, sc.scrollTop - contentOrigin(sc, headerEl.current)),
					);
				}
				geo.clearMeasured();
			}
			if (pinned.current) {
				tailNeedsRestore.current = true;
				// Mobile browser chrome collapses/expands DURING a touch scroll;
				// the restore write would cancel the gesture. The latch above
				// keeps the intent for the gesture-settle path.
				if (!touchGesture.current && canRestoreTail(
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

	// Row ref callback: (un)observe and track elements by id. The callback per
	// id is memoized and stable across renders: Preact compares refs by
	// identity, so a fresh closure per render would run old(null)+new(node) —
	// an unobserve/observe pair and a fresh initial ResizeObserver delivery —
	// for every windowed row on every scroll frame and composer keystroke.
	// Entries are pruned when the row unmounts (ref called with null), so the
	// map only ever holds the currently windowed rows.
	const refFns = useRef(new Map()); // id -> stable ref callback
	function rowRef(id) {
		let fn = refFns.current.get(id);
		if (fn) return fn;
		fn = (node) => {
			const old = rowEls.current.get(id);
			if (old === node) return; // unchanged element: skip observer churn
			if (old) ro.unobserve(old);
			if (node) {
				rowEls.current.set(id, node);
				ro.observe(node);
			} else {
				rowEls.current.delete(id);
				refFns.current.delete(id);
			}
		};
		refFns.current.set(id, fn);
		return fn;
	}

	// Loading a page while Firefox still owns a native scrollbar-thumb drag
	// makes it recompute scrollTop against the newly-grown scrollHeight and can
	// cascade the viewport toward the beginning. Loads therefore fire only at
	// moments that cannot be mid-drag: the browser's native scrollend (drag
	// release / settled wheel motion), or a settle timer that ONLY wheel input
	// may arm — scroll events carry no source and never extend it, so no chain
	// of drag-produced events can push a fire into a held drag. The transitions
	// live in scrollSettleStep (vmath.js) with the full state enumeration;
	// engines without scrollend keep the legacy quiet-period fallback there.
	// Touch gestures get the same deferral: while a finger is down (or its
	// momentum has not settled) no load may fire, because the prepend's
	// scrollTop compensation would cancel the gesture on iOS WebKit.
	const onNearTopRef = useRef(onNearTop);
	onNearTopRef.current = onNearTop;
	const nearTopPxRef = useRef(nearTopPx);
	nearTopPxRef.current = nearTopPx;
	const nativeScrollEnd = useRef(false);
	const settleTimer = useRef(null);
	const userScrollPending = useRef(false);

	function applySettle(event) {
		const touch = touchActive.current;
		const r = scrollSettleStep(nativeScrollEnd.current, userScrollPending.current, event, touch);
		userScrollPending.current = r.pending;
		if (r.timer === "arm") {
			if (settleTimer.current !== null) clearTimeout(settleTimer.current);
			settleTimer.current = setTimeout(() => {
				settleTimer.current = null;
				applySettle("timer");
			}, SCROLL_SETTLE_MS);
		} else if (r.timer === "clear" && settleTimer.current !== null) {
			clearTimeout(settleTimer.current);
			settleTimer.current = null;
		}
		// End-of-gesture work runs before the near-top check so the deferred
		// measurements and tail restore settle the geometry first.
		if (touchGesture.current &&
			touchGestureEnds(nativeScrollEnd.current, event, touch, touchScrolled.current)) {
			settleTouchGesture();
		}
		if (r.fire) {
			const sc = scroller.current;
			if (sc && !pinned.current && sc.scrollTop < nearTopPxRef.current) {
				onNearTopRef.current?.();
			}
		}
	}

	// applyDeferredMeasurements replays row heights queued while a touch
	// gesture owned the viewport: same two-pass shape as the live
	// ResizeObserver path, classified against the now-settled scroll position.
	function applyDeferredMeasurements(sc) {
		const q = deferredMeasure.current;
		if (q.size === 0) return;
		const origin = contentOrigin(sc, headerEl.current);
		const batch = [];
		for (const [id, h] of q) {
			const i = geo.indexOf(id);
			if (i === -1) continue; // trimmed away meanwhile
			batch.push({ id, h, above: geo.offsetOf(i + 1) + origin <= sc.scrollTop });
		}
		q.clear();
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
		if (pinned.current) tailNeedsRestore.current = true;
		else if (above !== 0) writeScroll(sc, position, sc.scrollTop + above);
		bump();
	}

	// settleTouchGesture releases the write latch once no pan/momentum can be
	// canceled, then lands everything deferred during the gesture.
	function settleTouchGesture() {
		touchGesture.current = false;
		const sc = scroller.current;
		if (!sc) return;
		applyDeferredMeasurements(sc);
		if (pinned.current && tailNeedsRestore.current) {
			if (canRestoreTail(
				sc, position, pinned, tailNeedsRestore, onPinnedRef.current,
			)) writeScroll(sc, position, sc.scrollHeight);
			tailNeedsRestore.current = false;
			bump();
		}
	}

	useLayoutEffect(() => {
		const sc = scroller.current;
		if (!sc) return;
		nativeScrollEnd.current = "onscrollend" in sc;
		const onScrollEnd = () => applySettle("scrollend");
		if (nativeScrollEnd.current) {
			sc.addEventListener("scrollend", onScrollEnd);
		}
		// All of these are passive: none ever calls preventDefault, and a
		// non-passive wheel/touch listener on the scroller adds scroll latency
		// (Preact's JSX onWheel would register non-passive). The touch pair
		// maintains the gesture latch; iOS has no scrollend, so touchend hands
		// the settle machine the job of detecting the end of momentum.
		const onTouchStart = () => {
			if (!touchActive.current) touchScrolled.current = false;
			touchActive.current = true;
			touchGesture.current = true;
			applySettle("touchstart");
		};
		const onTouchEnd = (e) => {
			if (e.touches.length > 0 || !touchActive.current) return;
			touchActive.current = false;
			applySettle("touchend");
		};
		sc.addEventListener("wheel", handleWheel, { passive: true });
		sc.addEventListener("touchstart", onTouchStart, { passive: true });
		sc.addEventListener("touchend", onTouchEnd, { passive: true });
		sc.addEventListener("touchcancel", onTouchEnd, { passive: true });
		return () => {
			sc.removeEventListener("scrollend", onScrollEnd);
			sc.removeEventListener("wheel", handleWheel);
			sc.removeEventListener("touchstart", onTouchStart);
			sc.removeEventListener("touchend", onTouchEnd);
			sc.removeEventListener("touchcancel", onTouchEnd);
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

		// Wheel-parity for touch (handleWheel's deltaY<0 branch): an untagged
		// finger-down movement toward older content unpins immediately. The
		// threshold alone cannot decide this — a live append landing between
		// the physical movement and its scroll event grows the gap, making a
		// 5px pan look far from the bottom (wrong unpin reason) or, inside
		// the threshold, re-pinning against the finger.
		const touchUp = touchActive.current && !internal &&
			top < position.current.knownTop - 0.5 && maxTop - top > 1;
		const wasPinned = pinned.current;
		const nowPinned = touchUp ? false : pinnedAfterScroll(wasPinned, internal, top, maxTop);
		if (touchUp) tailNeedsRestore.current = false;
		if (nowPinned !== wasPinned) {
			pinned.current = nowPinned;
			onPinnedRef.current?.(nowPinned);
		}
		// A row grew underneath a tail-following viewport. Preserve the intent
		// immediately instead of treating the temporary gap as a user unpin —
		// but never by writing into a live touch gesture; latch it instead.
		if (nowPinned && maxTop - top >= 40) {
			if (touchGesture.current) tailNeedsRestore.current = true;
			else writeScroll(sc, position, sc.scrollHeight);
		}

		if (!internal && top !== position.current.knownTop) {
			// Untagged AND moved: user/browser scrolling. A burst of code-owned
			// writes can queue several scroll events; the first consumes the
			// internal tag and the coalesced stragglers all read the same final
			// scrollTop — an unchanged position is not user movement and must
			// not latch a pending near-top check or arm the legacy quiet timer.
			if (touchActive.current) touchScrolled.current = true;
			applySettle("scroll");
		}
		position.current.knownTop = sc.scrollTop;
		// One re-render per frame, however many scroll events arrive.
		if (scrollScheduled.current) return;
		scrollScheduled.current = true;
		requestAnimationFrame(() => {
			scrollScheduled.current = false;
			// A full subtree re-render per scroll frame is what phones cannot
			// afford. Two binary searches decide whether this frame's row
			// window differs from the one already rendered; identical windows
			// (most frames, and every frame while pinned) skip the render.
			const cur = scroller.current;
			const lw = lastWindow.current;
			if (cur && lw && !pendingFocus.current && pendingPrepend.current === 0 &&
				!pendingLayoutAnchor.current) {
				const vt = Math.max(0, cur.scrollTop - contentOrigin(cur, headerEl.current));
				const w = computeWindow(geo, geo.items.length, {
					viewTop: vt, viewH: cur.clientHeight, overscan,
					pinned: pinned.current, focusing: false, focusIdx: -1, prepended: 0,
				});
				if (w.start === lw.start && w.end === lw.end) return;
			}
			bump();
		});
	}

	// A wheel event is the one scroll source that proves the hand is not on
	// the scrollbar thumb, so it alone may arm the settle timer — which both
	// closes Firefox 140's early-scrollend hole (its last scrollend can
	// precede the last wheel-driven scroll event) and guarantees no timer can
	// fire while a held thumb sits stationary beyond one settle window of the
	// last wheel input. See scrollSettleStep in vmath.js.
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
		applySettle("wheel");
	}

	// headerDeltaThisCommit tracks the header's height across commits and
	// returns how much it changed since the last one.
	function headerDeltaThisCommit() {
		const hh = headerEl.current?.offsetHeight || 0;
		const headerDelta = headerHeight.current === null ? 0 : hh - headerHeight.current;
		headerHeight.current = hh;
		if (pinned.current && headerDelta !== 0) tailNeedsRestore.current = true;
		return headerDelta;
	}

	// applyFocusCommit centers a rendered focus target (search jump); true when
	// it owned this commit.
	function applyFocusCommit(sc) {
		if (!pendingFocus.current || focusIdx === -1) return false;
		// A focus jump replaces the window wholesale, so discard any prepend
		// detected in the same commit — otherwise it fires on the next commit
		// and scrolls the view off the focused row.
		pendingPrepend.current = 0;
		pendingLayoutAnchor.current = null;
		tailNeedsRestore.current = false;
		applySettle("cancel");
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
		return true;
	}

	// applyLayoutAnchorCommit restores a captured viewport anchor after a
	// layout/filter rewrite; true when it owned this commit.
	function applyLayoutAnchorCommit(sc) {
		if (pinned.current || !pendingLayoutAnchor.current) return false;
		// The window above was deliberately rendered around this id using
		// the new estimates. Place it at the same intra-row pixel before paint;
		// ResizeObserver then compensates as the rendered neighborhood acquires
		// real heights.
		const top = restoreViewportAnchor(geo, pendingLayoutAnchor.current);
		pendingLayoutAnchor.current = null;
		pendingPrepend.current = 0; // the stable-id anchor subsumes prepend math
		tailNeedsRestore.current = false;
		if (top !== null) writeScroll(sc, position, contentOrigin(sc, headerEl.current) + top);
		if (sc.scrollHeight - sc.clientHeight < 40) {
			pinned.current = true;
			onPinnedRef.current?.(true);
		}
		return true;
	}

	// applyPrependCommit compensates scrollTop for rows prepended (and a header
	// height change) in this commit.
	function applyPrependCommit(sc, headerDelta) {
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
	}

	// applyTailCommit re-places a tail-following viewport at the bottom.
	// Tail-following is an intent, but only write when mount/items/geometry
	// actually moved. An unrelated parent render can land between native
	// thumb movement and its queued scroll event and must not erase it.
	function applyTailCommit(sc) {
		if (pinned.current && tailNeedsRestore.current) {
			// Mid touch gesture the restore write would cancel the pan/momentum
			// (iOS) and fight the finger; keep the latch — settleTouchGesture
			// places the tail once the gesture is over.
			if (touchGesture.current) return;
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
	}

	// pinShortList: a list whose content is shorter than the viewport never
	// fires a scroll event, so handleScroll can't flip `pinned` true. Signal it
	// here so a search-jump into a buffer whose whole window fits on screen
	// still reports pinned — otherwise the parent never reloads the live tail
	// and incoming messages are silently blocked (atTail stays false) until the
	// buffer is manually re-selected. Harmless when already at the tail
	// (the parent's reloadTail no-ops on an atTail buffer).
	function pinShortList(sc) {
		if (!pendingFocus.current && !pinned.current &&
			sc.scrollHeight - sc.clientHeight < 40) {
			pinned.current = true;
			onPinnedRef.current?.(true);
		}
	}

	// After every commit: apply focus/prepend/header anchoring and restore a
	// tail position only when an actual geometry change requested it.
	useLayoutEffect(() => {
		const sc = scroller.current;
		if (!sc) return;
		const headerDelta = headerDeltaThisCommit();
		if (applyFocusCommit(sc)) return;
		if (applyLayoutAnchorCommit(sc)) return;
		applyPrependCommit(sc, headerDelta);
		applyTailCommit(sc);
		pinShortList(sc);
	});

	return (
		<div class="msgs scroll" ref={scroller} onScroll={handleScroll}>
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
