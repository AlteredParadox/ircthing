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

import { deepStrictEqual as eq, strictEqual as is } from "node:assert";
import { test } from "node:test";
import {
	computeWindow,
	captureViewportAnchor,
	estimateMsgHeight,
	findIndex,
	Geometry,
	hasNonAppendIdentityChange,
	pinnedAfterScroll,
	prependedCount,
	restoreViewportAnchor,
	scrollSettleStep,
} from "../src/vmath.js";

const items = (n, from = 0) =>
	Array.from({ length: n }, (_, i) => ({ id: from + i, raw: "x" }));

function geo(list, est = () => 30) {
	const g = new Geometry(est);
	g.setItems(list);
	return g;
}

test("offsets from estimates", () => {
	const g = geo(items(4));
	is(g.total(), 120);
	is(g.offsetOf(0), 0);
	is(g.offsetOf(2), 60);
	is(g.offsetOf(4), 120);
	is(g.offsetOf(99), 120, "clamped past the end");
	is(geo([]).total(), 0);
});

test("measure corrects heights and reports deltas", () => {
	const g = geo(items(4));
	is(g.measure(1, 50), 20, "delta vs the estimate");
	is(g.total(), 140);
	is(g.offsetOf(2), 80);
	is(g.measure(1, 50), 0, "same measurement is a no-op");
	is(g.measure(1, 40), -10, "delta vs the previous measurement");
	is(g.measure(999, 50), 0, "unknown id ignored");
});

test("measurements survive item churn by id", () => {
	const g = geo(items(3));
	g.measure(2, 90);
	g.setItems([{ id: 5, raw: "x" }, ...items(3)]); // prepend id 5
	is(g.offsetOf(1), 30, "prepended row uses its estimate");
	is(g.total(), 30 + 30 + 30 + 90);
});

test("append fast path keeps the index valid", () => {
	const base = items(3);
	const g = geo(base);
	g.setItems([...base, { id: 100, raw: "x" }]); // shared prefix → fast path
	is(g.measure(100, 55), 25, "appended item is indexed");
	is(g.total(), 30 * 3 + 55);
	is(g.indexOf(100), 3);
	// Same array reference is a no-op.
	const cur = g.items;
	g.setItems(cur);
	is(g.dirty, false, "no-op setItems does not invalidate offsets");
});

test("range windows the viewport", () => {
	const g = geo(items(100)); // 30px each, total 3000
	// A row starting exactly at the bottom edge is included — one row of
	// free overscan keeps the search simple.
	eq(g.range(0, 90), { start: 0, end: 4 });
	eq(g.range(0, 89.9), { start: 0, end: 3 });
	eq(g.range(45, 46), { start: 1, end: 2 });
	eq(g.range(60, 60), { start: 2, end: 3 }, "zero-height viewport still yields one row");
	eq(g.range(2970, 5000), { start: 99, end: 100 });
	eq(g.range(-500, -100), { start: 0, end: 1 }, "clamped above");
	eq(g.range(9999, 10999), { start: 99, end: 100 }, "clamped below");
	eq(geo([]).range(0, 100), { start: 0, end: 0 });
});

test("range boundaries are exclusive of the next row", () => {
	const g = geo(items(10));
	// Viewport exactly [30, 60) shows only row 1.
	eq(g.range(30, 59.9), { start: 1, end: 2 });
});

test("pinned window renders a bounded tail instead of joining top to bottom", () => {
	const list = items(5000);
	const g = geo(list);
	const got = computeWindow(g, list.length, {
		viewTop: 0,
		viewH: 800,
		overscan: 600,
		pinned: true,
		focusing: false,
		focusIdx: -1,
		prepended: 0,
	});
	is(got.end, 5000);
	is(got.start > 4900, true, "first paint remains a small tail window");
	is(got.end - got.start < 100, true, "DOM row count stays bounded");
	const latePrepend = computeWindow(g, list.length, {
		viewTop: 0,
		viewH: 800,
		overscan: 600,
		pinned: true,
		focusing: false,
		focusIdx: -1,
		prepended: 100,
	});
	is(latePrepend.start > 4900, true, "a late prepend cannot reopen the whole pinned buffer");
	is(latePrepend.end - latePrepend.start < 100, true, "late-prepend tail stays bounded");
	const prepended = computeWindow(geo(items(200)), 200, {
		viewTop: 200,
		viewH: 800,
		overscan: 600,
		pinned: false,
		focusing: false,
		focusIdx: -1,
		prepended: 100,
	});
	is(prepended.start, 0, "new rows are present for real-height measurement");
	is(prepended.end > 100, true, "the future anchored viewport is rendered too");
	is(prepended.end < 200, true, "prepend anchoring remains windowed");

	eq(computeWindow(g, list.length, {
		viewTop: 0,
		viewH: 800,
		overscan: 600,
		pinned: false,
		focusing: false,
		focusIdx: -1,
		prepended: 0,
	}), { start: 0, end: 47 }, "an unpinned top viewport remains at the top");
});

test("pinned intent survives layout growth but not a real upward scroll", () => {
	is(pinnedAfterScroll(true, true, 1000, 1400), true,
		"a tagged bottom write stays pinned while the layout grows");
	is(pinnedAfterScroll(true, false, 1200, 2000), false,
		"an unmatched thumb move unpins even if concurrent growth raises scrollTop");
	is(pinnedAfterScroll(true, false, 700, 1400), false,
		"an unmatched move upward away from the tail unpins");
	is(pinnedAfterScroll(false, false, 900, 1400), false,
		"an unpinned viewport stays unpinned away from the tail");
	is(pinnedAfterScroll(false, false, 1370, 1400), true,
		"reaching the bottom repins");
});

test("findIndex binary search", () => {
	const off = Float64Array.from([0, 10, 30, 60, 100]);
	is(findIndex(off, 4, 0), 0);
	is(findIndex(off, 4, 9.9), 0);
	is(findIndex(off, 4, 10), 1);
	is(findIndex(off, 4, 59), 2);
	is(findIndex(off, 4, 60), 3);
	is(findIndex(off, 4, 1e9), 3, "clamped to the last row");
});

test("clearMeasured falls back to estimates", () => {
	const g = geo(items(2));
	g.measure(0, 99);
	g.clearMeasured();
	is(g.total(), 60);
});

test("layout invalidation restores the same row and intra-row offset", () => {
	const list = items(5);
	const g = geo(list);
	for (let i = 0; i < list.length; i++) g.measure(list[i].id, 40 + i * 10);
	const anchor = captureViewportAnchor(g, list, g.offsetOf(3) + 17);
	is(anchor.id, 3);
	is(anchor.member, 3);
	is(anchor.intra, 17);
	g.clearMeasured();
	is(restoreViewportAnchor(g, anchor), g.offsetOf(3) + 17);
});

test("viewport anchor survives a presence row collapsing into a run", () => {
	const shown = [{ id: 40 }, { id: 41 }, { id: 42 }, { id: 50 }];
	const g = geo(shown);
	const anchor = captureViewportAnchor(g, shown, g.offsetOf(1) + 5);
	const collapsed = [
		{ id: "clp-42", collapse: shown.slice(0, 3) },
		shown[3],
	];
	g.setItems(collapsed);
	is(restoreViewportAnchor(g, anchor), 5, "containing collapsed row becomes the anchor");
});

test("an expanded collapse summary restores to the summary, not its last child", () => {
	const run = [{ id: 40 }, { id: 41 }, { id: 42 }];
	const expanded = [
		{ id: "clp-42", collapse: run, expanded: true },
		...run,
		{ id: 50 },
	];
	const g = geo(expanded);
	const anchor = captureViewportAnchor(g, expanded, 7);
	is(anchor.id, "clp-42");
	is(anchor.member, 42);
	g.clearMeasured();
	is(restoreViewportAnchor(g, anchor), 7,
		"the exact synthetic row wins over the real child carrying member id 42");
});

test("a removed viewport row falls forward to its nearest surviving neighbor", () => {
	const list = items(8);
	const g = geo(list);
	for (const item of list) g.measure(item.id, 40);
	const anchor = captureViewportAnchor(g, list, g.offsetOf(3) + 9);
	const filtered = [list[0], list[1], list[4], list[5], list[6], list[7]];
	g.setItems(filtered, true);
	is(restoreViewportAnchor(g, anchor), g.offsetOf(2) + 9,
		"row 4, the next survivor, replaces removed row 3 at the anchor");
});

test("viewport fallback crosses a long run of removed rows", () => {
	const list = items(24);
	const g = geo(list);
	for (const item of list) g.measure(item.id, 40);
	const anchor = captureViewportAnchor(g, list, g.offsetOf(10) + 11);
	const filtered = [list[0], ...list.slice(18)];
	g.setItems(filtered, true);
	is(restoreViewportAnchor(g, anchor), g.offsetOf(1) + 11,
		"the next survivor wins even when more than a small neighbor window was removed");
});

test("anchor fallback crosses a huge removed run to collapse-row survivors", () => {
	// A 4000-row contiguous run (an ignored spammer) is removed from a
	// 5000-row buffer, and the survivors past the run are collapsed synthetic
	// rows — the shape that made the naive per-candidate findIndex fallback
	// O(removed x list). The lazy member index must land on the same row the
	// naive outward scan would: the first following survivor, at the same
	// intra-row offset.
	const list = items(5000);
	const g = geo(list);
	const anchor = captureViewportAnchor(g, list, g.offsetOf(2500) + 12);
	is(anchor.id, 2500);
	const head = list.slice(0, 500); // rows 500..4499 removed
	const runs = [];
	for (let i = 4500; i < 5000; i += 10) {
		runs.push({ id: `clp-${i + 9}`, collapse: list.slice(i, i + 10) });
	}
	g.setItems([...head, ...runs], true);
	// The nearest following survivor of removed row 2500 is row 4500, now a
	// member of the first collapse row (index 500 in the new list).
	is(restoreViewportAnchor(g, anchor), g.offsetOf(500) + 12,
		"first following collapse survivor keeps the viewport position");
	// A preceding survivor wins when it is strictly nearer than any follower.
	const g2 = geo(list);
	const anchor2 = captureViewportAnchor(g2, list, g2.offsetOf(600) + 3);
	const farRuns = [{ id: "clp-4999", collapse: list.slice(4500, 5000) }];
	g2.setItems([...list.slice(0, 500), ...farRuns], true);
	is(restoreViewportAnchor(g2, anchor2), g2.offsetOf(499) + 3,
		"nearest preceding real survivor beats the distant collapse run");
});

test("scrollSettleStep: transitions on scrollend-capable engines", () => {
	// { pending-in, event } -> expected step result. The load-firing DOM
	// check stays in the component; `fire` only asks for it.
	const cases = [
		["wheel", false, { pending: true, timer: "arm", fire: false }],
		["wheel", true, { pending: true, timer: "arm", fire: false }],
		// Scroll events carry no source (thumb drags emit only these), so they
		// must never arm or extend the settle countdown.
		["scroll", false, { pending: true, timer: "keep", fire: false }],
		["scroll", true, { pending: true, timer: "keep", fire: false }],
		// scrollend (drag release / settled wheel) always fires the pending
		// check and keeps a wheel-armed timer for Firefox's early scrollend.
		["scrollend", true, { pending: false, timer: "keep", fire: true }],
		["scrollend", false, { pending: false, timer: "keep", fire: false }],
		["timer", true, { pending: false, timer: "clear", fire: true }],
		["timer", false, { pending: false, timer: "clear", fire: false }],
		["cancel", true, { pending: false, timer: "clear", fire: false }],
	];
	for (const [event, pending, want] of cases) {
		eq(scrollSettleStep(true, pending, event), want, `${event} pending=${pending}`);
	}
});

test("scrollSettleStep: legacy engines re-arm the quiet timer on every scroll", () => {
	eq(scrollSettleStep(false, false, "scroll"), { pending: true, timer: "arm", fire: false });
	eq(scrollSettleStep(false, true, "scroll"), { pending: true, timer: "arm", fire: false });
	eq(scrollSettleStep(false, true, "timer"), { pending: false, timer: "clear", fire: true });
});

// runSettle folds an event sequence through the pure machine, tracking the
// armed flag the way the component's timer handle does, and records which
// events fired the near-top check and whether the timer was armed at each
// step.
function runSettle(hasScrollEnd, events) {
	let pending = false;
	let armed = false;
	const fires = [];
	const armedAt = [];
	events.forEach((ev, i) => {
		if (ev === "timer") is(armed, true, `event ${i}: timer fired while unarmed`);
		const r = scrollSettleStep(hasScrollEnd, pending, ev);
		pending = r.pending;
		if (r.timer === "arm") armed = true;
		else if (r.timer === "clear") armed = false;
		if (r.fire) fires.push(i);
		armedAt.push(armed);
	});
	return { pending, armed, fires, armedAt };
}

test("scrollSettleStep: wheel gesture loads once motion settles", () => {
	// Normal engine: wheel motion, scroll events, then a well-ordered
	// scrollend. The scrollend fires the check; the kept timer later expires
	// with nothing pending.
	const r = runSettle(true, ["wheel", "scroll", "scroll", "scrollend", "timer"]);
	eq(r.fires, [3], "scrollend fires; the stale timer expiry does not");
	is(r.pending, false);
});

test("scrollSettleStep: Firefox early scrollend still loads via the wheel timer", () => {
	// Firefox 140 can emit its last scrollend BEFORE the last wheel-driven
	// scroll events. The trailing events re-set pending and the still-armed
	// wheel timer (<=160ms after the last wheel) catches the near-top
	// crossing — the hole the wheel fallback exists to close.
	const r = runSettle(true, ["wheel", "scroll", "scrollend", "scroll", "scroll", "timer"]);
	eq(r.fires, [2, 5], "early scrollend fires, and the timer catches the trailing events");
	is(r.armed, false);
});

test("scrollSettleStep: a thumb drag never arms the settle timer", () => {
	// Pure drag on a scrollend engine: only unattributable scroll events,
	// stationary holds (no events at all), then release -> scrollend. No
	// step may ever arm the timer, so no load can fire mid-drag.
	const r = runSettle(true, ["scroll", "scroll", "scroll", "scrollend"]);
	eq(r.armedAt, [false, false, false, false], "no drag event arms the timer");
	eq(r.fires, [3], "the load fires exactly at drag release");
});

test("scrollSettleStep: wheel-then-drag cannot re-arm into a held thumb", () => {
	// The regression this machine exists to prevent: wheel up, grab the thumb
	// within the settle window, drag. Drag scroll events must not extend the
	// wheel-armed countdown, so after the single bounded wheel window expires
	// no timer exists to fire during a stationary hold; the load waits for
	// the release scrollend.
	const events = ["wheel", "scroll", "scroll", "timer", "scroll", "scroll", "scrollend"];
	const r = runSettle(true, events);
	is(r.armedAt[3], false, "the wheel window is one-shot");
	eq(r.armedAt.slice(4), [false, false, false],
		"drag scroll events after the window never re-arm");
	// fires[0]=3 is the accepted bounded residual: one fire can land inside
	// the single settle window after the last wheel event (a motionless grab
	// always had that hole). Everything after it must wait for the release.
	eq(r.fires, [3, 6], "after the wheel window, only the release scrollend fires");
});

test("scrollSettleStep: cancel clears a pending gesture", () => {
	const r = runSettle(true, ["wheel", "scroll", "cancel", "scrollend"]);
	eq(r.fires, [], "a focus jump consumes the pending scroll");
	is(r.pending, false);
	is(r.armed, false);
});

test("non-append identity changes distinguish filters from live tail appends", () => {
	const list = items(5);
	is(hasNonAppendIdentityChange(list, [...list, { id: 5 }]), false,
		"ordinary tail append does not request a viewport anchor");
	is(hasNonAppendIdentityChange(list, list.slice()), false,
		"a fresh array with the same stable identities is not structural");
	is(hasNonAppendIdentityChange(list, [list[0], list[2], list[3], list[4]]), true,
		"filtering a row is structural even when all survivors are the same objects");
	is(hasNonAppendIdentityChange(list, list.slice(1)), true, "head trim is structural");
});

test("a stable visible row restores after unchanged rows above it are filtered", () => {
	const list = items(6);
	const g = geo(list);
	for (const item of list) g.measure(item.id, 40);
	const anchor = captureViewportAnchor(g, list, g.offsetOf(4) + 9);
	const filtered = [list[0], list[2], list[4], list[5]];
	is(hasNonAppendIdentityChange(list, filtered), true);
	g.setItems(filtered);
	is(restoreViewportAnchor(g, anchor), g.offsetOf(2) + 9,
		"the same row stays at the same intra-row viewport position");
});

test("non-append identity changes recognize collapse/show rewrites", () => {
	const shown = [{ id: 40 }, { id: 41 }, { id: 42 }, { id: 50 }];
	const collapsed = [
		{ id: "clp-42", collapse: shown.slice(0, 3) },
		shown[3],
	];
	is(hasNonAppendIdentityChange(shown, collapsed), true);
	is(hasNonAppendIdentityChange(collapsed, shown), true);
	is(hasNonAppendIdentityChange(collapsed, [...collapsed, { id: 60 }]), false,
		"a tail append after an unchanged collapsed run stays on the append path");
});

test("same-id content changes invalidate only their cached measurement", () => {
	const list = items(3);
	const g = geo(list);
	g.measure(0, 60);
	g.measure(1, 70);
	g.measure(2, 80);
	const changed = [list[0], { ...list[1], raw: "redacted" }, list[2]];
	is(g.invalidateChanged(changed), 1);
	g.setItems(changed);
	is(g.heightAt(0), 60);
	is(g.heightAt(1), 30, "changed row falls back to estimate");
	is(g.heightAt(2), 80);
});

test("prependedCount", () => {
	is(prependedCount(null, items(3)), 0, "fresh load");
	is(prependedCount(0, items(3)), 0, "unchanged head");
	is(prependedCount(5, [...items(3)]), 0, "old head gone (trim)");
	is(prependedCount(0, [{ id: 90 }, { id: 91 }, ...items(3)]), 2, "two prepended");
	is(prependedCount(7, []), 0, "emptied");
});

test("prependedCount anchors on collapsed-run identity", () => {
	// A lone presence event (id 42) folds into a collapsed run after an
	// older page prepends events before it: its top-level id changes
	// (42 -> "clp-42") but its anchor (the run's last underlying event) does
	// not, so the prepend is still detected and the viewport stays put.
	const collapsed = { id: "clp-42", collapse: [{ id: 40 }, { id: 41 }, { id: 42 }] };
	is(prependedCount(42, [{ id: 30 }, collapsed, { id: 50 }]), 1, "lone event folded into a run");
	is(prependedCount(42, [collapsed, { id: 50 }]), 0, "same run still on top");
});

test("estimateMsgHeight grows with wrapped length", () => {
	is(estimateMsgHeight(""), 27);
	is(estimateMsgHeight("x".repeat(89)), 27);
	is(estimateMsgHeight("x".repeat(90)), 48);
	is(estimateMsgHeight("x".repeat(300)), 27 + 3 * 21);
	is(estimateMsgHeight(null), 27);
});

test("Geometry: measured is pruned for removed rows (no unbounded growth)", () => {
	const g = new Geometry(() => 20);
	const list = items(10);
	g.setItems(list);
	for (let i = 0; i < 10; i++) g.measure(list[i].id, 30 + i);
	is(g.measured.size, 10);
	// Trim to the newest 3 (a non-append replacement, like a buffer trim).
	g.setItems(list.slice(7));
	is(g.measured.size, 3, "measurements for trimmed rows dropped");
	is(g.measured.get(list[9].id), 39, "survivor keeps its measured height");
});

test("Geometry: a same-length structural rewrite forces index rebuild", () => {
	const g = new Geometry(() => 20);
	const list = items(5);
	g.setItems(list);
	const rewritten = [list[0], list[2], list[1], list[3], list[4]];
	g.setItems(rewritten, true);
	is(g.indexOf(list[2].id), 1);
	is(g.indexOf(list[1].id), 2);
});
