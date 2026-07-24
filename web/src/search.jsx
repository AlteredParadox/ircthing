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
import { pressable } from "./a11y.js";
import {
	bufKey,
	dropPurgedRows,
	emptySearchPurge,
	fmtTime,
	recordBufferClosed,
	recordNetworkRemoved,
	renderable,
	stripFormatting,
} from "./irc.js";

// tombstoneResults re-marks search rows that were redacted while the buffer
// was unloaded — a snapshot taken before the destructive scrub could otherwise
// display deleted content. redactedIds is the app's Map<bufKey, Map<msgid,…>>.
function tombstoneResults(list, redactedIds) {
	if (!redactedIds) return list;
	return list.map((ev) => {
		if (ev.redacted || !ev.msgid) return ev;
		const set = redactedIds.current.get(bufKey(ev.network, ev.buffer));
		return set?.has(ev.msgid)
			? { ...ev, redacted: true, redact_reason: set.get(ev.msgid), raw: "" }
			: ev;
	});
}

// tombstoneMatching marks the result matching a live redaction. Match the
// full identity, not just msgid — the same msgid can exist in another
// network/buffer and must not be tombstoned here. Returns the input array
// unchanged when nothing matched (so the state setter is a no-op).
function tombstoneMatching(rs, d) {
	let hit = false;
	const next = rs.map((ev) => {
		if (ev.redacted || ev.msgid !== d.msgid || ev.network !== d.network || ev.buffer !== d.buffer) return ev;
		hit = true;
		return { ...ev, redacted: true, redact_reason: d.reason, raw: "" };
	});
	return hit ? next : rs;
}

// Full-text search overlay. Debounced queries hit the server FTS index;
// each result renders like a message line (via the shared renderer) and,
// when clicked, jumps to that message in its buffer.
export function SearchOverlay({ sock, onJump, onClose, timeFmt, nickSep, redactedIds }) {
	const [query, setQuery] = useState("");
	const [results, setResults] = useState([]);
	const [hasMore, setHasMore] = useState(false);
	const [state, setState] = useState("idle"); // idle | loading | done
	const inputRef = useRef(null);
	const seq = useRef(0);
	// Buffers purged (destructive close / network removal) while the panel is
	// open. Both the displayed rows and any response still in flight are
	// filtered against it — a delayed response could otherwise install rows
	// for a just-purged buffer. Deliberately NOT a seq bump: bumping would
	// strand the panel "loading" and discard unrelated rows.
	const purged = useRef(null);
	if (!purged.current) purged.current = emptySearchPurge();

	useEffect(() => {
		inputRef.current?.focus();
		const onKey = (e) => {
			if (e.key === "Escape") onClose();
		};
		globalThis.addEventListener("keydown", onKey);
		return () => globalThis.removeEventListener("keydown", onKey);
	}, []);

	// A redaction arriving while the panel is open must tombstone a matching
	// result too: an open result set is independent state that a new query
	// (server-side redacted=0 filter) would exclude, but the already-shown
	// snippet would otherwise keep displaying deleted content.
	useEffect(() => {
		const s = sock.current;
		if (!s) return undefined;
		const onRedact = (d) => setResults((rs) => tombstoneMatching(rs, d));
		// Purges are one-way while the panel is open: accumulate, then drop
		// matching rows already on screen (archive closes record nothing and
		// the setter stays a no-op).
		const onClosed = (d) => {
			recordBufferClosed(purged.current, d);
			setResults((rs) => dropPurgedRows(rs, purged.current));
		};
		const onNetworkRemoved = (d) => {
			recordNetworkRemoved(purged.current, d);
			setResults((rs) => dropPurgedRows(rs, purged.current));
		};
		s.on("redact", onRedact);
		s.on("buffer_closed", onClosed);
		s.on("network_removed", onNetworkRemoved);
		return () => {
			s.off("redact", onRedact);
			s.off("buffer_closed", onClosed);
			s.off("network_removed", onNetworkRemoved);
		};
	}, []);

	useEffect(() => {
		const q = query.trim();
		if (!q) {
			// Invalidate any in-flight response too: without bumping seq, a
			// pending request from the previous non-empty query would still pass
			// its `mine === seq.current` guard and repopulate the cleared panel.
			++seq.current;
			setResults([]);
			setHasMore(false);
			setState("idle");
			return;
		}
		setState("loading");
		setHasMore(false);
		const mine = ++seq.current;
		const t = setTimeout(() => {
			sock.current
				?.request("search", { query: q, limit: 50 })
				.then((data) => {
					if (mine !== seq.current) return; // a newer query superseded us
					// Apply known tombstones to the freshly-installed rows: a row
					// snapshotted before its redaction scrub must not display
					// content. Then drop rows for buffers purged since the panel
					// opened — this response may have been snapshotted pre-purge.
					setResults(dropPurgedRows(tombstoneResults(data.messages || [], redactedIds), purged.current));
					setHasMore(data.has_more === true);
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
					<button type="button" class="search-close" onClick={onClose} title="Close (Esc)">✕</button>
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
									<span class="search-result-text">{stripFormatting(r.text)}</span>
								</div>
							</div>
						);
					})}
					{state === "done" && hasMore && (
						<div class="search-note">more matches exist — refine your search</div>
					)}
				</div>
			</div>
		</div>
	);
}
