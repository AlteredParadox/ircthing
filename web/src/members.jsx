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

import { useMemo, useState } from "preact/hooks";
import { groupMembers, nickColor } from "./irc.js";
import { menuTrigger } from "./menu.jsx";

// Members panel per the mockup: grouped roster with status dot, role
// glyph and hashed nick colors. Away state comes from WHOX-on-join plus
// away-notify; the tooltip names the services account (WHOX,
// extended-join, account-notify). Right-click (or long-press) a member
// for the user menu; ignored members are dimmed.
function memberTitle(m, ignored) {
	let t = m.nick;
	if (m.user || m.host) t += ` (${m.user || ""}@${m.host || ""})`;
	if (m.account) t += m.nick === m.account ? " — identified" : ` — identified as ${m.account}`;
	if (m.bot) t += " — bot";
	if (ignored) t += " — ignored";
	return t;
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
				<div class="data-warning" role="status" title="The server response reached its safety limit.">
					Member list incomplete — server limit reached
				</div>
			)}
			<div class="side-list scroll">
				{q && shown.length === 0 && <div class="member-group-head">no matches</div>}
				{groupMembers(shown).map((g) => (
					<div class="net-group" key={g.label}>
						<div class="member-group-head">{g.label} — {g.members.length}</div>
						{g.members.map((m) => {
							const isIgnored = ignored.has(m.nick.toLowerCase());
							return (
								<div
									class={"member-row" + (m.away ? " away" : "") + (isIgnored ? " ignored" : "")}
									key={m.nick}
									title={memberTitle(m, isIgnored)}
									{...menuTrigger((x, y) => onNick(m.nick, x, y))}
								>
									<span class={"dot " + (m.away ? "away" : "online")} />
									<span class={"member-glyph" + (g.label === "Ops" ? " op" : " voice")}>
										{(m.prefix || "")[0] || ""}
									</span>
									<span class="member-nick" style={{ color: nickColor(m.nick, theme) }}>
										{m.nick}
									</span>
									{m.bot && <span class="bot-chip">bot</span>}
								</div>
							);
						})}
					</div>
				))}
			</div>
		</div>
	);
}
