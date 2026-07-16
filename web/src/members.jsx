import { useRef } from "preact/hooks";
import { groupMembers, nickColor } from "./irc.js";
import { longPress } from "./menu.jsx";

// Members panel per the mockup: grouped roster with status dot, role
// glyph and hashed nick colors. Away state comes from WHOX-on-join plus
// away-notify; the tooltip names the services account (WHOX,
// extended-join, account-notify). Right-click (or long-press) a member
// for the user menu; ignored members are dimmed.
function memberTitle(m, ignored) {
	let t = m.nick;
	if (m.account) t += m.nick === m.account ? " — identified" : ` — identified as ${m.account}`;
	if (m.bot) t += " — bot";
	if (ignored) t += " — ignored";
	return t;
}

export function Members({ info, theme, ignoredNicks, onNick }) {
	const members = info?.members || [];
	const ignored = new Set(ignoredNicks || []);
	const pressFired = useRef(false);
	return (
		<div class="right-inner">
			<div class="right-head">
				<div class="panel-label">Members</div>
				<div class="side-meta">{members.length}</div>
			</div>
			<div class="side-list scroll">
				{groupMembers(members).map((g) => (
					<div class="net-group" key={g.label}>
						<div class="member-group-head">{g.label} — {g.members.length}</div>
						{g.members.map((m) => {
							const isIgnored = ignored.has(m.nick.toLowerCase());
							const openMenu = (x, y) => onNick(m.nick, x, y);
							return (
								<div
									class={"member-row" + (m.away ? " away" : "") + (isIgnored ? " ignored" : "")}
									key={m.nick}
									title={memberTitle(m, isIgnored)}
									onContextMenu={(e) => {
										e.preventDefault();
										openMenu(e.clientX, e.clientY);
									}}
									onClick={() => {
										if (pressFired.current) pressFired.current = false;
									}}
									{...longPress(openMenu, pressFired)}
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
