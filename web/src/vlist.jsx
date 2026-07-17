import { useLayoutEffect, useMemo, useRef, useState } from "preact/hooks";
import { anchorId, Geometry, prependedCount } from "./vmath.js";

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
	let { start, end } = geo.range(viewTop - overscan, viewTop + viewH + overscan);
	if (pinned.current && items.length) {
		// While pinned the tail must be in the window even before the
		// scroll position catches up (first paint, buffer switch, append).
		end = items.length;
		start = Math.max(0, Math.min(start, end - 1));
		const bottom = geo.total();
		const r = geo.range(bottom - viewH - overscan, bottom);
		start = Math.min(start, r.start);
	}
	if (pendingFocus.current && focusIdx !== -1) {
		// Force the target and its neighbors into the DOM so the layout
		// effect can scroll to a rendered, measurable row.
		start = Math.min(start, Math.max(0, focusIdx - 12));
		end = Math.max(end, Math.min(items.length, focusIdx + 12));
	}
	if (k > 0) {
		// Render the ENTIRE just-prepended page this frame so the layout
		// effect can measure every new row and anchor the scroll with real
		// heights. The estimate ignores density and message font, so
		// estimate-based anchoring overshoots (e.g. compact + mono rows are
		// ~20px, the estimate assumes ~27+) and the async ResizeObserver
		// correction shows up as a jarring scroll jump on each older page.
		start = 0;
		end = Math.max(end, Math.min(items.length, k));
	}

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

	const scrollScheduled = useRef(false);
	function handleScroll() {
		const sc = scroller.current;
		if (!sc) return;
		const nowPinned = sc.scrollHeight - sc.scrollTop - sc.clientHeight < 40;
		if (nowPinned !== pinned.current) {
			pinned.current = nowPinned;
			onPinned?.(nowPinned);
		}
		if (sc.scrollTop < nearTopPx) onNearTop?.();
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
			// Center the target row in the viewport. A focus jump replaces the
			// window wholesale, so discard any prepend detected in the same
			// commit — otherwise it fires on the next commit and scrolls the
			// view off the focused row.
			pendingPrepend.current = 0;
			const hh = headerEl.current?.offsetHeight || 0;
			const rowH = geo.offsetOf(focusIdx + 1) - geo.offsetOf(focusIdx);
			sc.scrollTop = hh + geo.offsetOf(focusIdx) - (sc.clientHeight - rowH) / 2;
			pendingFocus.current = false;
			return;
		}
		if (pendingPrepend.current > 0) {
			// Measure the just-prepended rows NOW (they're in the DOM, before
			// paint — the render above forced the whole page into the window)
			// so the anchor compensation uses real heights instead of the
			// density/font-blind estimate. This keeps offsetOf(k) exact, so
			// the previously-visible content stays put with no visible jump;
			// the ResizeObserver later re-measures to the same value (a no-op).
			const kk = pendingPrepend.current;
			for (let i = 0; i < kk; i++) {
				const node = rowEls.current.get(items[i].id);
				if (node) geo.measure(items[i].id, node.offsetHeight);
			}
			sc.scrollTop += geo.offsetOf(kk);
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
