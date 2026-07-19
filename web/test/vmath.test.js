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
import { estimateMsgHeight, findIndex, Geometry, prependedCount } from "../src/vmath.js";

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
