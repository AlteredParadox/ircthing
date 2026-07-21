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
	applyMemberPage,
	fetchAllMembers,
	flattenGroups,
	initialPaging,
	MAX_MEMBERS,
	memberWindow,
} from "../src/memberlist.js";

const nicks = (n, from = 0) =>
	Array.from({ length: n }, (_, i) => ({ nick: "n" + String(from + i).padStart(5, "0") }));

// page builds a get_channel-shaped response.
const page = (members, truncated, after) => ({
	joined: true,
	topic: "t",
	members,
	truncated,
	...(after !== undefined ? { members_after: after } : {}),
});

test("applyMemberPage accumulates pages in order and finishes clean", () => {
	let st = initialPaging();
	st = applyMemberPage(st, page(nicks(3), true, "n00002"));
	is(st.done, false);
	is(st.degraded, false);
	is(st.after, "n00002");
	eq(st.meta, { joined: true, topic: "t" });
	st = applyMemberPage(st, page(nicks(3, 3), true, "n00005"));
	is(st.done, false);
	st = applyMemberPage(st, page(nicks(2, 6), false));
	is(st.done, true);
	is(st.degraded, false);
	eq(
		st.members.map((m) => m.nick),
		nicks(8).map((m) => m.nick),
	);
});

test("applyMemberPage keeps the first page's meta", () => {
	let st = applyMemberPage(initialPaging(), page(nicks(1), true, "n00000"));
	const later = { ...page(nicks(1, 1), false), topic: "changed", joined: false };
	st = applyMemberPage(st, later);
	eq(st.meta, { joined: true, topic: "t" });
});

test("applyMemberPage stops degraded on truncated without cursor (old server)", () => {
	const st = applyMemberPage(initialPaging(), page(nicks(5), true));
	is(st.done, true);
	is(st.degraded, true);
	is(st.members.length, 5);
});

test("applyMemberPage stops degraded on a non-advancing cursor", () => {
	let st = applyMemberPage(initialPaging(), page(nicks(3), true, "n00002"));
	is(st.done, false);
	// Same cursor again: no progress — hostile or buggy server.
	st = applyMemberPage(st, page(nicks(3, 3), true, "n00002"));
	is(st.done, true);
	is(st.degraded, true);
	// A cursor sorting before the previous one is equally stuck.
	let back = applyMemberPage(initialPaging(), page(nicks(3), true, "n00002"));
	back = applyMemberPage(back, page(nicks(3, 3), true, "n00001"));
	is(back.done, true);
	is(back.degraded, true);
});

test("applyMemberPage stops degraded on a truncated empty page", () => {
	const st = applyMemberPage(initialPaging(), page([], true, "zzz"));
	is(st.done, true);
	is(st.degraded, true);
});

test("applyMemberPage stops degraded at the accumulation cap", () => {
	let st = initialPaging();
	const size = 5000;
	for (let i = 0; ; i++) {
		st = applyMemberPage(st, page(nicks(size, i * size), true, "c" + i));
		if (st.done) break;
		if (i > MAX_MEMBERS / size + 1) throw new Error("cap never hit");
	}
	is(st.degraded, true);
	is(st.members.length, MAX_MEMBERS);
});

test("fetchAllMembers walks pages and reports each accumulation step", async () => {
	const pages = {
		"": page(nicks(2), true, "n00001"),
		n00001: page(nicks(2, 2), false),
	};
	const asked = [];
	const seen = [];
	const st = await fetchAllMembers({
		request: (after) => {
			asked.push(after);
			return Promise.resolve(pages[after]);
		},
		isStale: () => false,
		onPage: (s) => seen.push(s.members.length),
	});
	eq(asked, ["", "n00001"]);
	eq(seen, [2, 4]);
	is(st.done, true);
	is(st.degraded, false);
	is(st.members.length, 4);
});

test("fetchAllMembers abandons a stale walk without reporting further pages", async () => {
	let calls = 0;
	const seen = [];
	const st = await fetchAllMembers({
		request: () => {
			calls++;
			return Promise.resolve(page(nicks(2), true, "n00001"));
		},
		isStale: () => calls >= 1, // buffer switched / socket replaced after page 1
		onPage: (s) => seen.push(s.members.length),
	});
	is(st, null);
	is(calls, 1);
	eq(seen, []); // nothing applied after staleness
});

test("fetchAllMembers resolves null on request failure", async () => {
	const st = await fetchAllMembers({
		request: () => Promise.reject(new Error("boom")),
		isStale: () => false,
		onPage: () => {
			throw new Error("onPage after failure");
		},
	});
	is(st, null);
});

test("flattenGroups emits a head row per group then its members", () => {
	const rows = flattenGroups([
		{ label: "Ops", members: [{ nick: "a" }] },
		{ label: "Members", members: [{ nick: "b" }, { nick: "c" }] },
	]);
	eq(
		rows.map((r) => (r.kind === "head" ? "H:" + r.label + ":" + r.count : r.m.nick)),
		["H:Ops:1", "a", "H:Members:2", "b", "c"],
	);
	eq(rows[3].group, "Members");
});

// Window math: 1 head (height 20) + 100 member rows (height 10 each),
// total 1020.
const rowsFixture = flattenGroups([{ label: "Members", members: nicks(100) }]);
const win = (scrollTop, viewH, overscan = 0) =>
	memberWindow(rowsFixture, scrollTop, viewH, 10, 20, overscan);

test("memberWindow at the top starts at row zero with no top pad", () => {
	const w = win(0, 100);
	is(w.start, 0);
	is(w.topPad, 0);
	// 20 (head) + 8*10 = 100 exactly fills the viewport.
	is(w.end, 9);
	is(w.bottomPad, 1020 - 100);
});

test("memberWindow at the bottom ends at the last row with no bottom pad", () => {
	const w = win(920, 100);
	is(w.end, rowsFixture.length);
	is(w.bottomPad, 0);
	// Rows fully above 920 are skipped: head(20) + 90 rows = 920.
	is(w.start, 91);
	is(w.topPad, 920);
});

test("memberWindow exact fit renders everything with zero pads", () => {
	const w = win(0, 1020);
	is(w.start, 0);
	is(w.end, rowsFixture.length);
	is(w.topPad, 0);
	is(w.bottomPad, 0);
});

test("memberWindow mid-list slices with both pads consistent", () => {
	const w = win(500, 100, 2);
	// Pads plus rendered heights always reconstruct the total height.
	let rendered = 0;
	for (const r of rowsFixture.slice(w.start, w.end)) rendered += r.kind === "head" ? 20 : 10;
	is(w.topPad + rendered + w.bottomPad, 1020);
	// A row straddling the top edge stays rendered (no gap).
	is(w.topPad <= 500 - 2 * 10, true);
	is(w.start > 0, true);
	is(w.end < rowsFixture.length, true);
});

test("memberWindow handles an empty row list", () => {
	eq(memberWindow([], 0, 100, 10, 20), { start: 0, end: 0, topPad: 0, bottomPad: 0 });
});
