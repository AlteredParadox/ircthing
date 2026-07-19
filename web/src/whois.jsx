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

// WhoisCard renders an accumulated WHOIS reply (see hub WhoisData) as a
// labelled card in the target's query buffer, instead of a scatter of
// cryptic numeric lines.

import { stripFormatting } from "./irc.js";

// fmtIdle turns seconds of idle time into "3d 4h", "12m 30s", etc.
function fmtIdle(secs) {
	if (secs <= 0) return "0s";
	const units = [["d", 86400], ["h", 3600], ["m", 60], ["s", 1]];
	const parts = [];
	let rem = secs;
	for (const [label, size] of units) {
		const v = Math.floor(rem / size);
		if (v > 0) parts.push(v + label);
		rem %= size;
		if (parts.length === 2) break; // two units is plenty
	}
	return parts.join(" ") || "0s";
}

function fmtSignon(unixSecs) {
	const d = new Date(unixSecs * 1000);
	return d.toLocaleString(undefined, {
		year: "numeric", month: "short", day: "numeric",
		hour: "2-digit", minute: "2-digit",
	});
}

export function WhoisCard({ whois: w, focused }) {
	// Strip mIRC formatting: these are plain-text card fields (no styled-run
	// rendering), and a colored gecos/away string would otherwise leak its
	// code digits — the same digit-leak fixed for other non-body surfaces.
	const s = stripFormatting;
	const fields = [];
	if (w.realname) fields.push(["Real name", s(w.realname)]);
	if (w.account) fields.push(["Account", s(w.account)]);
	if (w.user || w.host) fields.push(["Host", `${s(w.user) || "?"}@${s(w.host) || "?"}`]);
	if (w.actual) fields.push(["Connecting from", s(w.actual)]);
	if (w.server) fields.push(["Server", s(w.server)]);
	if (w.channels) fields.push(["Channels", s(w.channels)]);
	if (w.idle) fields.push(["Idle", fmtIdle(w.idle)]);
	if (w.signon) fields.push(["Signed on", fmtSignon(w.signon)]);

	return (
		<div class={"whois-row" + (focused ? " flash" : "")}>
			<div class="whois-card">
				<div class="whois-head">
					<span class="whois-nick">{w.nick}</span>
					{w.bot && <span class="whois-badge">bot</span>}
					{w.operator && <span class="whois-badge op">operator</span>}
					{w.secure && <span class="whois-badge secure">secure</span>}
					{w.away && <span class="whois-badge away">away</span>}
				</div>
				{w.away && <div class="whois-away">{s(w.away)}</div>}
				<dl class="whois-fields">
					{fields.map(([k, v]) => (
						<div class="whois-field" key={k}>
							<dt>{k}</dt>
							<dd>{v}</dd>
						</div>
					))}
				</dl>
			</div>
		</div>
	);
}
