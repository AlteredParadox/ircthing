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

import { useRef, useState } from "preact/hooks";
import { pressable } from "./a11y.js";
import { longPress } from "./menu.jsx";
import { bufKey, nickColor, SERVER_BUFFER } from "./irc.js";
import { BufIcon } from "./icons.jsx";

function stateDot(state) {
	if (state === "registered") return "online";
	if (state === "connecting") return "away";
	return "offline";
}

export function Sidebar({ networks, buffers, activeKey, monitors, theme, mutedSet, onSelect, onSettings, onBufferMenu, onNetworkMenu, onAddNetwork, onAddMonitor, onRemoveMonitor }) {
	// One shared flag: a long-press that opened a menu suppresses the tap
	// that follows it. pressTimer holds the pending hold timer across
	// re-renders (see longPress) so a mid-hold re-render can't orphan it.
	const pressFired = useRef(false);
	const pressTimer = useRef(null);
	// Group buffers under their network; networks without buffers still
	// get a section so a fresh install isn't a blank panel.
	const names = new Set(Object.keys(networks));
	for (const b of Object.values(buffers)) names.add(b.network);
	const sections = [...names].sort((a, b) => a.localeCompare(b)).map((name) => ({
		name,
		state: networks[name]?.state || "disconnected",
		nick: networks[name]?.nick || "",
		chantypes: networks[name]?.chantypes || "#&",
		// The server buffer ("*") is the network header itself, not a row.
		buffers: Object.values(buffers)
			.filter((b) => b.network === name && b.buffer !== SERVER_BUFFER)
			.sort((a, b) => a.buffer.localeCompare(b.buffer)),
		server: buffers[bufKey(name, SERVER_BUFFER)],
	}));
	const me = sections.map((s) => s.nick).find(Boolean) || "";
	const online = Object.values(networks).some((n) => n.state === "registered");

	return (
		<div class="side-inner">
			<div class="side-head">
				<div class="logo">λ</div>
				<div class="side-title">ircthing</div>
				<div class="side-meta">{sections.length} net{sections.length === 1 ? "" : "s"}</div>
				<button class="monitor-addbtn" title="Add network" onClick={onAddNetwork}>+</button>
			</div>
			<div class="side-list scroll">
				{sections.map((sec) => (
					<div class="net-group" key={sec.name}>
						{(() => {
							// The network header IS the server buffer (lobby): left-click
							// opens it, right-click / long-press opens the management menu.
							const srvKey = bufKey(sec.name, SERVER_BUFFER);
							const srvActive = srvKey === activeKey;
							const srvUnread = (sec.server?.unread || 0) > 0;
							const openMenu = (x, y) => onNetworkMenu(sec.name, x, y);
							return (
								<button
									type="button"
									class={"net-head has-menu" + (srvActive ? " active" : "") + (srvUnread ? " unread" : "")}
									onClick={() => {
										if (pressFired.current) {
											pressFired.current = false;
											return;
										}
										onSelect(sec.name, SERVER_BUFFER);
									}}
									onContextMenu={(e) => {
										e.preventDefault();
										openMenu(e.clientX, e.clientY);
									}}
									{...longPress(openMenu, pressFired, pressTimer)}
								>
									<span class={"dot " + stateDot(sec.state)} />
									<span class="net-name">{sec.name}</span>
									{srvUnread && (
										<span class={"badge" + (sec.server?.mention ? " mention" : "")}>
											{sec.server.unread > 99 ? "99+" : sec.server.unread}
										</span>
									)}
								</button>
							);
						})()}
						{sec.buffers.map((b) => {
							const key = bufKey(b.network, b.buffer);
							const active = key === activeKey;
							const unread = b.unread > 0;
							const muted = mutedSet?.has(key);
							const isChan = sec.chantypes.includes(b.buffer[0]);
							const openMenu = (x, y) => onBufferMenu(b.network, b.buffer, x, y);
							return (
								<button
									type="button"
									class={"chan-row" + (active ? " active" : "") + (unread ? " unread" : "") + (muted ? " muted" : "")}
									key={key}
									onClick={() => {
										if (pressFired.current) {
											pressFired.current = false;
											return;
										}
										onSelect(b.network, b.buffer);
									}}
									onContextMenu={(e) => {
										e.preventDefault();
										openMenu(e.clientX, e.clientY);
									}}
									{...longPress(openMenu, pressFired, pressTimer)}
								>
									<BufIcon chan={isChan} />
									<span class="chan-name">{b.buffer}</span>
									{muted && <span class="chan-mute" title="Muted">🔇</span>}
									{unread && (
										<span class={"badge" + (b.mention ? " mention" : "")}>
											{b.unread > 99 ? "99+" : b.unread}
										</span>
									)}
								</button>
							);
						})}
						<MonitorSection
							network={sec.name}
							list={monitors?.[sec.name] || []}
							onOpen={(nick) => onSelect(sec.name, nick)}
							onAdd={onAddMonitor}
							onRemove={onRemoveMonitor}
						/>
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

// MonitorSection is the per-network MONITOR buddy list: nicks with an
// online/offline dot, click to open a query, hover to remove, plus an
// inline add field.
function MonitorSection({ network, list, onOpen, onAdd, onRemove }) {
	const [adding, setAdding] = useState(false);
	const [value, setValue] = useState("");

	function submit(e) {
		e.preventDefault();
		onAdd(network, value);
		setValue("");
		setAdding(false);
	}

	return (
		<div class="monitor-section">
			<div class="monitor-head">
				<span>monitor</span>
				<button class="monitor-addbtn" title="Add buddy" onClick={() => setAdding((a) => !a)}>+</button>
			</div>
			{adding && (
				<form class="monitor-add" onSubmit={submit}>
					<input
						class="monitor-input"
						autofocus
						value={value}
						placeholder="nick…"
						onInput={(e) => setValue(e.currentTarget.value)}
						onBlur={() => !value && setAdding(false)}
					/>
				</form>
			)}
			{list.map((m) => (
				<div class="monitor-row" key={m.nick} {...pressable(() => onOpen(m.nick))}>
					<span class={"dot " + (m.online ? "online" : "offline")} />
					<span class={"monitor-nick" + (m.online ? "" : " off")}>{m.nick}</span>
					<button
						class="monitor-remove"
						title="Stop monitoring"
						onClick={(e) => {
							e.stopPropagation();
							onRemove(network, m.nick);
						}}
					>✕</button>
				</div>
			))}
		</div>
	);
}
