import { useEffect, useRef, useState } from "preact/hooks";
import { pressable } from "./a11y.js";
import { fmtTime, renderable } from "./irc.js";

// Full-text search overlay. Debounced queries hit the server FTS index;
// each result renders like a message line (via the shared renderer) and,
// when clicked, jumps to that message in its buffer.
export function SearchOverlay({ sock, onJump, onClose, timeFmt, nickSep }) {
	const [query, setQuery] = useState("");
	const [results, setResults] = useState([]);
	const [state, setState] = useState("idle"); // idle | loading | done
	const inputRef = useRef(null);
	const seq = useRef(0);

	useEffect(() => {
		inputRef.current?.focus();
		const onKey = (e) => {
			if (e.key === "Escape") onClose();
		};
		globalThis.addEventListener("keydown", onKey);
		return () => globalThis.removeEventListener("keydown", onKey);
	}, []);

	useEffect(() => {
		const q = query.trim();
		if (!q) {
			setResults([]);
			setState("idle");
			return;
		}
		setState("loading");
		const mine = ++seq.current;
		const t = setTimeout(() => {
			sock.current
				?.request("search", { query: q, limit: 50 })
				.then((data) => {
					if (mine !== seq.current) return; // a newer query superseded us
					setResults(data.messages || []);
					setState("done");
				})
				.catch(() => mine === seq.current && setState("done"));
		}, 200);
		return () => clearTimeout(t);
	}, [query]);

	return (
		<div class="search-scrim" aria-hidden="true" onClick={(e) => e.target === e.currentTarget && onClose()}>
			<div class="search-panel">
				<div class="search-head">
					<span class="search-icon">⌕</span>
					<input
						ref={inputRef}
						class="search-input"
						value={query}
						onInput={(e) => setQuery(e.currentTarget.value)}
						placeholder="Search all messages…"
					/>
					<button class="search-close" onClick={onClose} title="Close (Esc)">✕</button>
				</div>
				<div class="search-results scroll">
					{state === "loading" && results.length === 0 && <div class="search-note">searching…</div>}
					{state === "done" && results.length === 0 && <div class="search-note">no matches</div>}
					{results.map((ev) => {
						const r = renderable(ev);
						return (
							<div
								class="search-result"
								key={ev.id}
								{...pressable(() => onJump(ev))}
							>
								<div class="search-result-meta">
									<span class="search-result-buf">{ev.network}/{ev.buffer}</span>
									<span class="search-result-time">{fmtTime(ev.time, timeFmt)}</span>
								</div>
								<div class="search-result-line">
									<span class="search-result-nick">{ev.sender}{ev.sender ? (nickSep || "") : ""}</span>
									<span class="search-result-text">{r.text}</span>
								</div>
							</div>
						);
					})}
				</div>
			</div>
		</div>
	);
}
