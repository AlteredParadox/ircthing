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

import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { groupMembers, nickColor } from "./irc.js";
import { flattenGroups, memberWindow } from "./memberlist.js";
import { menuTrigger } from "./menu.jsx";

// Members panel per the mockup: grouped roster with status dot, role
// glyph and hashed nick colors. Away state comes from WHOX-on-join plus
// away-notify; the tooltip names the services account (WHOX,
// extended-join, account-notify). Right-click (or long-press) a member
// for the user menu; ignored members are dimmed.
//
// Large channels arrive via cursor paging (app.jsx accumulates pages) and
// can reach 25k members; above WINDOW_ROWS the list renders through a
// simple fixed-height window (scrollTop -> slice + two spacer divs, see
// memberWindow) instead of one DOM row per member. Below it, the plain
// grouped markup is kept exactly as before.
const WINDOW_ROWS = 400;

function memberTitle(m, ignored) {
	let t = m.nick;
	if (m.user || m.host) t += ` (${m.user || ""}@${m.host || ""})`;
	if (m.account) t += m.nick === m.account ? " — identified" : ` — identified as ${m.account}`;
	if (m.bot) t += " — bot";
	if (ignored) t += " — ignored";
	return t;
}

function MemberRow({ m, group, theme, ignored, onNick }) {
	const isIgnored = ignored.has(m.nick.toLowerCase());
	return (
		<div
			class={"member-row" + (m.away ? " away" : "") + (isIgnored ? " ignored" : "")}
			key={m.nick}
			title={memberTitle(m, isIgnored)}
			{...menuTrigger((x, y) => onNick(m.nick, x, y))}
		>
			<span class={"dot " + (m.away ? "away" : "online")} />
			<span class={"member-glyph" + (group === "Ops" ? " op" : " voice")}>
				{(m.prefix || "")[0] || ""}
			</span>
			<span class="member-nick" style={{ color: nickColor(m.nick, theme) }}>
				{m.nick}
			</span>
			{m.bot && <span class="bot-chip">bot</span>}
		</div>
	);
}

export function Members({ info, theme, ignoredNicks, onNick }) {
	const members = info?.members || [];
	const ignored = new Set(ignoredNicks || []);
	// Live filter: the header doubles as a filter field (placeholder "Members"),
	// so the panel gains a filter without an extra row of chrome.
	const [filter, setFilter] = useState("");
	const q = filter.trim().toLowerCase();
	const shown = useMemo(
		() => (q ? members.filter((m) => m.nick.toLowerCase().includes(q)) : members),
		[members, q],
	);
	const groups = useMemo(() => groupMembers(shown), [shown]);
	const rows = useMemo(() => flattenGroups(groups), [groups]);
	const windowed = rows.length > WINDOW_ROWS;

	const listRef = useRef(null);
	const [scrollTop, setScrollTop] = useState(0);
	const [viewH, setViewH] = useState(600);
	const [heights, setHeights] = useState({ row: 30, head: 24 });

	// The panel is mounted unkeyed, so a buffer switch reuses this instance:
	// without a reset the previous channel's filter text and scroll offset
	// would silently apply to the new roster. Reset in an effect rather than
	// keying the component, which would tear down the measured row heights
	// and the ResizeObserver for no reason.
	useEffect(() => {
		setFilter("");
		if (listRef.current) listRef.current.scrollTop = 0;
		setScrollTop(0);
	}, [info?.network, info?.buffer]);

	// Track the scroller's viewport height (the panel resizes with the
	// window / layout changes).
	useEffect(() => {
		const el = listRef.current;
		if (!el || typeof ResizeObserver === "undefined") return;
		const ro = new ResizeObserver(() => setViewH(el.clientHeight));
		setViewH(el.clientHeight);
		ro.observe(el);
		return () => ro.disconnect();
	}, []);

	// Row heights are uniform per kind but depend on theme/density/text
	// size, so measure real rendered rows (margins are zero on both kinds;
	// offsetHeight is exact) and correct the initial estimate. The setState
	// is guarded, so this settles after one render instead of looping.
	useEffect(() => {
		const el = listRef.current;
		if (!windowed || !el) return;
		const row = el.querySelector(".member-row")?.offsetHeight;
		const head = el.querySelector(".member-group-head")?.offsetHeight;
		setHeights((h) => {
			const next = { row: row || h.row, head: head || h.head };
			return next.row === h.row && next.head === h.head ? h : next;
		});
	});

	// The browser clamps scrollTop when the list shrinks (filtering, buffer
	// switch) without firing a scroll event; follow it so the window slice
	// matches the real scroll position.
	useEffect(() => {
		const el = listRef.current;
		if (el) setScrollTop(el.scrollTop);
	}, [rows.length]);

	const win = windowed
		? memberWindow(rows, scrollTop, viewH, heights.row, heights.head)
		: null;
	const renderRow = (r) =>
		r.kind === "head" ? (
			<div class="member-group-head" key={"head:" + r.label}>
				{r.label} — {r.count}
			</div>
		) : (
			<MemberRow key={r.m.nick} m={r.m} group={r.group} theme={theme} ignored={ignored} onNick={onNick} />
		);
	return (
		<div class="right-inner">
			<div class="right-head">
				<input
					class="member-filter panel-label"
					value={filter}
					onInput={(e) => setFilter(e.currentTarget.value)}
					placeholder="Members"
					aria-label="Filter members"
				/>
				<div class="side-meta">{q ? `${shown.length}/${members.length}` : members.length}</div>
			</div>
			{info?.truncated === true && (
				<output
					class="data-warning"
					title="Fetching the membership stopped early (server without paging support, or a safety stop), so some members are not shown."
				>
					Member list incomplete — paging stopped early
				</output>
			)}
			<div
				class="side-list scroll"
				ref={listRef}
				onScroll={windowed ? (e) => setScrollTop(e.currentTarget.scrollTop) : undefined}
			>
				{q && shown.length === 0 && <div class="member-group-head">no matches</div>}
				{windowed ? (
					<>
						<div style={{ height: win.topPad + "px" }} />
						{rows.slice(win.start, win.end).map(renderRow)}
						<div style={{ height: win.bottomPad + "px" }} />
					</>
				) : (
					groups.map((g) => (
						<div class="net-group" key={g.label}>
							<div class="member-group-head">
								{g.label} — {g.members.length}
							</div>
							{g.members.map((m) => (
								<MemberRow key={m.nick} m={m} group={g.label} theme={theme} ignored={ignored} onNick={onNick} />
							))}
						</div>
					))
				)}
			</div>
		</div>
	);
}
