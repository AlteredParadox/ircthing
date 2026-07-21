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

// Member-list paging (get_channel `after` cursor) and the fixed-height
// window math for the members panel. Pure functions, tested under node
// (no DOM) like vmath.js.

// MAX_MEMBERS is the client-side accumulation stop. It matches the server
// roster's per-channel bound (maxChannelMembers in internal/irc/roster.go):
// no honest server response can exceed it, so hitting it means the far end
// is buggy or hostile and the walk stops rather than growing without bound.
export const MAX_MEMBERS = 25000;

// initialPaging is the accumulator's starting state for one channel walk.
export function initialPaging() {
	return { members: [], after: "", meta: null, done: false, degraded: false };
}

// applyMemberPage folds one get_channel response into the paging state.
// The server sends pages in ascending folded-key order, so plain
// concatenation keeps the accumulated list sorted. done=true ends the walk;
// degraded=true means it ended early (the list is genuinely incomplete):
//   - truncated with no cursor: an old server without paging support;
//   - a cursor that fails to advance, or a truncated-but-empty page:
//     buggy/hostile server — stop rather than loop forever;
//   - the MAX_MEMBERS accumulation cap.
export function applyMemberPage(st, d) {
	const page = Array.isArray(d?.members) ? d.members : [];
	const next = {
		members: st.members.concat(page),
		after: st.after,
		// Topic/joined come from the first page; later pages repeat them but
		// the first response's values are kept for a stable panel header.
		meta: st.meta || { joined: d?.joined === true, topic: d?.topic || "" },
		done: false,
		degraded: false,
	};
	if (d?.truncated !== true) {
		next.done = true; // complete: the server has no more members
		return next;
	}
	const cursor = typeof d?.members_after === "string" ? d.members_after : "";
	// Cursor must advance (folded keys ascend; string compare approximates
	// the server's byte order — for exotic non-ASCII nicks it may stop a
	// legitimate walk early, which degrades safely to "incomplete"). An
	// empty page that still claims truncation can never advance either.
	if (cursor === "" || cursor <= st.after || page.length === 0) {
		next.done = true;
		next.degraded = true;
		return next;
	}
	if (next.members.length >= MAX_MEMBERS) {
		next.done = true;
		next.degraded = true;
		return next;
	}
	next.after = cursor;
	return next;
}

// fetchAllMembers drives a full cursor walk. request(after) resolves one
// get_channel response; isStale() is consulted after every response and
// abandons the walk (returning null) when the active buffer or socket
// changed underneath it; onPage(state) fires per page so the UI can show
// members as they accumulate. Resolves the final state, or null when
// abandoned or a request failed.
export async function fetchAllMembers({ request, isStale, onPage }) {
	let st = initialPaging();
	for (;;) {
		let d;
		try {
			d = await request(st.after);
		} catch {
			return null;
		}
		if (isStale()) return null;
		st = applyMemberPage(st, d);
		onPage(st);
		if (st.done) return st;
	}
}

// flattenGroups turns groupMembers() output into one flat row array for
// windowed rendering: a head row per group followed by its member rows.
export function flattenGroups(groups) {
	const rows = [];
	for (const g of groups) {
		rows.push({ kind: "head", label: g.label, count: g.members.length });
		for (const m of g.members) rows.push({ kind: "member", m, group: g.label });
	}
	return rows;
}

// memberWindow computes the visible slice of a flat row list plus the
// spacer heights that stand in for everything outside it. Two uniform row
// heights only (member rows and the at-most-three group heads), so the
// math is a linear walk — trivially cheap even at 25k rows and far simpler
// than the variable-height VirtualList, which is deliberately not reused
// here. Returns { start, end, topPad, bottomPad } with rows[start:end] to
// render between the two pads.
export function memberWindow(rows, scrollTop, viewH, rowH, headH, overscan = 10) {
	const h = (r) => (r.kind === "head" ? headH : rowH);
	let total = 0;
	for (const r of rows) total += h(r);
	const top = Math.max(0, scrollTop - overscan * rowH);
	const bottom = scrollTop + Math.max(0, viewH) + overscan * rowH;
	let y = 0;
	let start = 0;
	while (start < rows.length && y + h(rows[start]) <= top) {
		y += h(rows[start]);
		start++;
	}
	const topPad = y;
	let end = start;
	while (end < rows.length && y < bottom) {
		y += h(rows[end]);
		end++;
	}
	return { start, end, topPad, bottomPad: total - y };
}
