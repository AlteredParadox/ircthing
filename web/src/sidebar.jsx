import { bufKey, nickColor } from "./irc.js";

function stateDot(state) {
	if (state === "registered") return "online";
	if (state === "connecting") return "away";
	return "offline";
}

export function Sidebar({ networks, buffers, activeKey, theme, onSelect, onSettings }) {
	// Group buffers under their network; networks without buffers still
	// get a section so a fresh install isn't a blank panel.
	const names = new Set(Object.keys(networks));
	for (const b of Object.values(buffers)) names.add(b.network);
	const sections = [...names].sort().map((name) => ({
		name,
		state: networks[name]?.state || "disconnected",
		nick: networks[name]?.nick || "",
		buffers: Object.values(buffers)
			.filter((b) => b.network === name)
			.sort((a, b) => a.buffer.localeCompare(b.buffer)),
	}));
	const me = sections.map((s) => s.nick).find(Boolean) || "";
	const online = Object.values(networks).some((n) => n.state === "registered");

	return (
		<div class="side-inner">
			<div class="side-head">
				<div class="logo">λ</div>
				<div class="side-title">ircthing</div>
				<div class="side-meta">{sections.length} net{sections.length === 1 ? "" : "s"}</div>
			</div>
			<div class="side-list scroll">
				{sections.map((sec) => (
					<div class="net-group" key={sec.name}>
						<div class="net-head">
							<span class={"dot " + stateDot(sec.state)} />
							<span>{sec.name}</span>
						</div>
						{sec.buffers.map((b) => {
							const key = bufKey(b.network, b.buffer);
							const active = key === activeKey;
							const unread = b.unread > 0;
							const isChan = b.buffer.startsWith("#") || b.buffer.startsWith("&");
							return (
								<div
									class={"chan-row" + (active ? " active" : "") + (unread ? " unread" : "")}
									key={key}
									onClick={() => onSelect(b.network, b.buffer)}
								>
									<span class="chan-hash">{isChan ? b.buffer[0] : "@"}</span>
									<span class="chan-name">{isChan ? b.buffer.slice(1) : b.buffer}</span>
									{unread && (
										<span class={"badge" + (b.mention ? " mention" : "")}>
											{b.unread > 99 ? "99+" : b.unread}
										</span>
									)}
								</div>
							);
						})}
					</div>
				))}
			</div>
			<div class="side-foot">
				<div class="avatar-wrap">
					<div class="avatar" style={{ background: me ? nickColor(me, theme) : "var(--elev)" }}>
						{(me[0] || "?").toUpperCase()}
					</div>
					<span class={"foot-dot " + (online ? "online" : "offline")} />
				</div>
				<div class="foot-id">
					<div class="foot-nick">{me || "—"}</div>
					<div class="foot-state">{online ? "online" : "offline"}</div>
				</div>
				<button class="foot-gear" title="Settings" onClick={onSettings}>⚙</button>
			</div>
		</div>
	);
}
