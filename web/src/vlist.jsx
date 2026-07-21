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

import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "preact/hooks";
import { anchorId, Geometry, prependedCount } from "./vmath.js";

// computeWindow picks the [start, end) row slice to render this frame: the
// overscan window around the scroll position, widened to (a) include the tail
// while pinned, (b) surround the focus target during a search jump, and (c)
// cover the ENTIRE just-prepended page so the layout effect can measure real
// heights — the estimate ignores density and font, so estimate-based anchoring
// overshoots (compact + mono rows are ~20px, the estimate assumes ~27+) and the
// async ResizeObserver correction otherwise shows up as a jarring scroll jump.
function computeWindow(geo, items, { viewTop, viewH, overscan, pinned, focusing, focusIdx, prepended }) {
	let { start, end } = geo.range(viewTop - overscan, viewTop + viewH + overscan);
	if (pinned && items.length) {
		// While pinned the tail must be in the window even before the scroll
		// position catches up (first paint, buffer switch, append).
		end = items.length;
		start = Math.max(0, Math.min(start, end - 1));
		const bottom = geo.total();
		const r = geo.range(bottom - viewH - overscan, bottom);
		start = Math.min(start, r.start);
	}
	if (focusing && focusIdx !== -1) {
		// Force the target and its neighbors into the DOM so the layout effect
		// can scroll to a rendered, measurable row.
		start = Math.min(start, Math.max(0, focusIdx - 12));
		end = Math.max(end, Math.min(items.length, focusIdx + 12));
	}
	if (prepended > 0) {
		start = 0;
		end = Math.max(end, Math.min(items.length, prepended));
	}
	return { start, end };
}

// centerRow scrolls so row `idx` sits centered in the viewport (used for a
// search-jump focus).
function centerRow(sc, geo, headerH, idx) {
	const rowH = geo.offsetOf(idx + 1) - geo.offsetOf(idx);
	sc.scrollTop = headerH + geo.offsetOf(idx) - (sc.clientHeight - rowH) / 2;
}

// anchorPrepended measures the k just-prepended rows (in the DOM, pre-paint)
// and compensates scrollTop by their real height, so the previously-visible
// content stays put with no jump instead of using the density/font-blind
// estimate.
function anchorPrepended(sc, geo, items, rowEls, k) {
	for (let i = 0; i < k; i++) {
		const node = rowEls.get(items[i].id);
		if (node) geo.measure(items[i].id, node.offsetHeight);
	}
	sc.scrollTop += geo.offsetOf(k);
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
	const prevLastId = useRef(null);
	const pendingPrepend = useRef(0);
	const pendingFocus = useRef(false);
	const prevFocus = useRef(undefined);
	const width = useRef(0);
	const rowEls = useRef(new Map()); // id -> element

	geo.setItems(items);

	// A new focus target (a search jump) unpins and requests a scroll-to
	// once the item is present. Set synchronously so the window below
	// renders around the target rather than the tail.
	const focusIdx = focusId == null ? -1 : geo.indexOf(focusId);
	if (focusId !== prevFocus.current) {
		prevFocus.current = focusId;
		if (focusId != null && focusIdx !== -1) {
			pendingFocus.current = true;
			pinned.current = false;
		}
	}

	// Detect a prepend during render; the layout effect below compensates
	// scrollTop after the new spacers apply.
	const k = prependedCount(prevFirstId.current, items);
	if (k > 0) pendingPrepend.current = k;
	const appended = items.length > 0 && prevLastId.current !== items[items.length - 1].id;
	// Anchor on the top row's stable identity (collapse-run aware) so the
	// prepend compensation survives a lone presence event folding into a run.
	prevFirstId.current = items.length ? anchorId(items[0]) : null;
	prevLastId.current = items.length ? items[items.length - 1].id : null;

	// Current window from the last known scroll position.
	const el = scroller.current;
	const headerH = headerEl.current?.offsetHeight || 0;
	const viewTop = el ? el.scrollTop - headerH : 0;
	const viewH = el ? el.clientHeight : 800;
	const { start, end } = computeWindow(geo, items, {
		viewTop, viewH, overscan,
		pinned: pinned.current, focusing: pendingFocus.current, focusIdx, prepended: k,
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
				const hh = headerEl.current?.offsetHeight || 0;
				const batch = classifyEntries(entries, geo, sc.scrollTop, hh);
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
				if (pinned.current) sc.scrollTop = sc.scrollHeight;
				else if (above !== 0) sc.scrollTop += above;
				bump();
			}),
		[],
	);
	useLayoutEffect(() => () => ro.disconnect(), []);

	// Emit the initial pinned state once after mount. The parent (Chat) is
	// NOT remounted per buffer, so its own pinned tracking would otherwise
	// carry a stale value from the previous buffer and silently suppress
	// read-marker updates; this list IS keyed per buffer, so it knows the
	// truth — true at the tail, false for a focus (search) jump.
	useLayoutEffect(() => {
		onPinned?.(pinned.current);
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

	// Scrollbar-thumb drags break prepend anchoring: while the thumb is
	// held, the browser re-derives scrollTop from the thumb's position on
	// EVERY mouse move, so a prepend that grows scrollHeight mid-drag
	// clobbers the anchored scrollTop and teleports the content
	// proportionally — and near the top each teleport lands under
	// nearTopPx and re-fires onNearTop, cascading page loads until the
	// view sits at the beginning of the loaded window. Content anchoring
	// and a user-held thumb are inherently incompatible (the thumb under
	// the cursor wins), so hold onNearTop while the gutter is being
	// dragged and run it once on release. Chromium dispatches pointer
	// events for scrollbar interactions (target = the scroller, offsetX
	// past clientWidth); Firefox never delivers them, so there this is a
	// no-op and behavior is unchanged.
	const barDrag = useRef(false);
	const onNearTopRef = useRef(onNearTop);
	onNearTopRef.current = onNearTop;
	function gutterDown(e) {
		const sc = scroller.current;
		if (sc && e.target === sc && e.offsetX >= sc.clientWidth) barDrag.current = true;
	}
	useEffect(() => {
		const release = () => {
			if (!barDrag.current) return;
			barDrag.current = false;
			const sc = scroller.current;
			// The drag may have parked the view in the near-top zone with
			// loads suppressed; retrigger once so paging resumes.
			if (sc && sc.scrollTop < nearTopPx) onNearTopRef.current?.();
		};
		window.addEventListener("pointerup", release);
		window.addEventListener("pointercancel", release);
		window.addEventListener("blur", release);
		return () => {
			window.removeEventListener("pointerup", release);
			window.removeEventListener("pointercancel", release);
			window.removeEventListener("blur", release);
		};
	}, []);

	const scrollScheduled = useRef(false);
	function handleScroll() {
		const sc = scroller.current;
		if (!sc) return;
		const nowPinned = sc.scrollHeight - sc.scrollTop - sc.clientHeight < 40;
		if (nowPinned !== pinned.current) {
			pinned.current = nowPinned;
			onPinned?.(nowPinned);
		}
		if (sc.scrollTop < nearTopPx && !barDrag.current) onNearTop?.();
		// One re-render per frame, however many scroll events arrive.
		if (scrollScheduled.current) return;
		scrollScheduled.current = true;
		requestAnimationFrame(() => {
			scrollScheduled.current = false;
			bump();
		});
	}

	// After every commit: apply prepend anchoring, follow the bottom while
	// pinned, and watch for width changes that invalidate measurements.
	useLayoutEffect(() => {
		const sc = scroller.current;
		if (!sc) return;
		if (pendingFocus.current && focusIdx !== -1) {
			// A focus jump replaces the window wholesale, so discard any prepend
			// detected in the same commit — otherwise it fires on the next commit
			// and scrolls the view off the focused row.
			pendingPrepend.current = 0;
			centerRow(sc, geo, headerEl.current?.offsetHeight || 0, focusIdx);
			pendingFocus.current = false;
			return;
		}
		if (pendingPrepend.current > 0) {
			anchorPrepended(sc, geo, items, rowEls.current, pendingPrepend.current);
			pendingPrepend.current = 0;
		}
		if (pinned.current && appended) {
			sc.scrollTop = sc.scrollHeight;
		}
		const w = sc.clientWidth;
		if (w !== width.current) {
			width.current = w;
			geo.clearMeasured();
			if (pinned.current) sc.scrollTop = sc.scrollHeight;
			bump();
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
			onPinned?.(true);
		}
	});

	return (
		<div class="msgs scroll" ref={scroller} onScroll={handleScroll} onPointerDown={gutterDown}>
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
function classifyEntries(entries, geo, scrollTop, headerH) {
	const batch = [];
	for (const e of entries) {
		const id = idOf(e.target.dataset.vid);
		if (id === null) continue;
		const h = e.borderBoxSize?.[0]?.blockSize ?? e.target.offsetHeight;
		if (h === 0) continue; // detached row
		const i = geo.indexOf(id);
		batch.push({ id, h, above: i !== -1 && geo.offsetOf(i) + headerH < scrollTop });
	}
	return batch;
}

function idOf(s) {
	if (s === undefined) return null;
	const n = Number(s);
	return Number.isNaN(n) ? s : n;
}
