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

import { useEffect, useRef, useState } from "preact/hooks";
import { rankBuffers, SERVER_BUFFER } from "./irc.js";
import { BufIcon } from "./icons.jsx";

// Channel switcher palette (Ctrl+K): type to filter buffers, arrows to
// move, Enter to jump. Mentions and unread buffers float to the top, so
// Ctrl+K Enter is also "go to the most urgent thing".
export function Switcher({ buffers, networks, onSelect, onClose }) {
	const [query, setQuery] = useState("");
	const [idx, setIdx] = useState(0);
	const inputRef = useRef(null);
	const list = rankBuffers(buffers, query).slice(0, 12);
	const sel = Math.min(idx, list.length - 1);

	useEffect(() => inputRef.current?.focus(), []);
	useEffect(() => {
		const onKey = (e) => e.key === "Escape" && onClose();
		globalThis.addEventListener("keydown", onKey);
		return () => globalThis.removeEventListener("keydown", onKey);
	}, []);

	function onKeyDown(e) {
		if (e.key === "ArrowDown") {
			e.preventDefault();
			setIdx(Math.min(sel + 1, list.length - 1));
		} else if (e.key === "ArrowUp") {
			e.preventDefault();
			setIdx(Math.max(sel - 1, 0));
		} else if (e.key === "Enter" && list[sel]) {
			e.preventDefault();
			onSelect(list[sel].network, list[sel].buffer);
		}
	}

	return (
		<div class="search-scrim" aria-hidden="true" onClick={(e) => e.target === e.currentTarget && onClose()}>
			<div class="search-panel switch-panel">
				<div class="search-head">
					<span class="search-icon">›</span>
					<input
						ref={inputRef}
						class="search-input"
						value={query}
						placeholder="Jump to channel or query…"
						onInput={(e) => {
							setQuery(e.currentTarget.value);
							setIdx(0);
						}}
						onKeyDown={onKeyDown}
					/>
					<button type="button" class="search-close" onClick={onClose} title="Close (Esc)">✕</button>
				</div>
				<div class="search-results scroll">
					{list.length === 0 && <div class="search-note">no matching buffers</div>}
					{list.map((b, i) => {
						const isServer = b.buffer === SERVER_BUFFER;
						const isChan = (networks?.[b.network]?.chantypes || "#&").includes(b.buffer[0]);
						return (
							<button
								type="button"
								class={"switch-row" + (i === sel ? " sel" : "")}
								key={b.key}
								onMouseEnter={() => setIdx(i)}
								onFocus={() => setIdx(i)}
								onClick={() => onSelect(b.network, b.buffer)}
							>
								<BufIcon chan={isChan} server={isServer} />
								<span class="switch-name">{isServer ? "server" : b.buffer}</span>
								<span class="switch-net">{b.network}</span>
								{b.unread > 0 && (
									<span class={"badge" + (b.mention ? " mention" : "")}>
										{b.unread > 99 ? "99+" : b.unread}
									</span>
								)}
							</button>
						);
					})}
				</div>
			</div>
		</div>
	);
}
