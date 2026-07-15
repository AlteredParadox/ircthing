import { groupMembers, nickColor } from "./irc.js";

// Members panel per the mockup: grouped roster with status dot, role
// glyph and hashed nick colors. Away/offline states arrive with Phase 2's
// away-notify — until then everyone shows online.
export function Members({ info, theme }) {
	const members = info?.members || [];
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
						{g.members.map((m) => (
							<div class={"member-row" + (m.away ? " away" : "")} key={m.nick}>
								<span class={"dot " + (m.away ? "away" : "online")} />
								<span class={"member-glyph" + (g.label === "Ops" ? " op" : " voice")}>
									{(m.prefix || "")[0] || ""}
								</span>
								<span class="member-nick" style={{ color: nickColor(m.nick, theme) }}>
									{m.nick}
								</span>
							</div>
						))}
					</div>
				))}
			</div>
		</div>
	);
}
