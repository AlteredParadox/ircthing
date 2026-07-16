import { useEffect, useRef, useState } from "preact/hooks";
import { pressable } from "./a11y.js";
import { rankBuffers } from "./irc.js";

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
		<div class="search-scrim" role="presentation" onClick={onClose}>
			<div class="search-panel switch-panel" role="presentation" onClick={(e) => e.stopPropagation()}>
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
					<button class="search-close" onClick={onClose} title="Close (Esc)">✕</button>
				</div>
				<div class="search-results scroll">
					{list.length === 0 && <div class="search-note">no matching buffers</div>}
					{list.map((b, i) => {
						const isChan = (networks?.[b.network]?.chantypes || "#&").includes(b.buffer[0]);
						return (
							<div
								class={"switch-row" + (i === sel ? " sel" : "")}
								key={b.key}
								onMouseEnter={() => setIdx(i)}
								{...pressable(() => onSelect(b.network, b.buffer))}
							>
								<span class="chan-hash">{isChan ? b.buffer[0] : "@"}</span>
								<span class="switch-name">{isChan ? b.buffer.slice(1) : b.buffer}</span>
								<span class="switch-net">{b.network}</span>
								{b.unread > 0 && (
									<span class={"badge" + (b.mention ? " mention" : "")}>
										{b.unread > 99 ? "99+" : b.unread}
									</span>
								)}
							</div>
						);
					})}
				</div>
			</div>
		</div>
	);
}
