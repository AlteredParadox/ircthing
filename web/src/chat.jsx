import { useEffect, useLayoutEffect, useRef, useState } from "preact/hooks";
import { fmtTime, linkify, mentionsMe, nickColor, renderable, sameGroup } from "./irc.js";

function Body({ text }) {
	return linkify(text).map((seg) =>
		seg.link
			? <a href={seg.text} target="_blank" rel="noopener noreferrer">{seg.text}</a>
			: seg.text,
	);
}

function Row({ ev, prev, selfNick, theme }) {
	const r = renderable(ev);
	if (r.kind === "system") {
		return (
			<div class="sys-row">
				<span class="msg-time">{fmtTime(ev.time)}</span>
				<span class={"sys-mark " + r.markClass}>{r.mark}</span>
				<span>{r.text}</span>
			</div>
		);
	}
	const self = selfNick && ev.sender === selfNick;
	const mention = !self && mentionsMe(r.text, selfNick);
	const grouped = !mention && sameGroup(prev && { ...renderable(prev), sender: prev.sender, time: prev.time }, { ...r, sender: ev.sender, time: ev.time });
	const color = self ? "var(--accent)" : nickColor(ev.sender, theme);
	return (
		<div class={"msg-row" + (mention ? " mention" : "")}>
			<span class="msg-time">{grouped ? "" : fmtTime(ev.time)}</span>
			<span class="msg-nick" style={{ color }} title={ev.sender}>
				{r.kind === "action" ? "*" : grouped ? "" : ev.sender}
			</span>
			<div class={"msg-body" + (r.kind === "action" ? " action" : "") + (r.kind === "notice" ? " notice" : "")}>
				{r.kind === "action" && <span style={{ color, fontWeight: 600 }}>{ev.sender} </span>}
				<Body text={r.text} />
			</div>
		</div>
	);
}

// Chat renders the active buffer: topic bar handled by the parent; this
// is the scrollback plus composer. Scrollback stays pinned to the bottom
// unless the user scrolls up; nearing the top loads an older page with
// scroll anchoring.
export function Chat({ buf, msgs, selfNick, theme, connected, onSend, onLoadOlder, onRead }) {
	const el = useRef(null);
	const pinned = useRef(true);
	const anchor = useRef(null); // scrollHeight before an older-page prepend
	const [draft, setDraft] = useState("");
	const list = msgs?.list || [];
	const first = list[0];
	const last = list[list.length - 1];

	function onScroll() {
		const e = el.current;
		if (!e) return;
		pinned.current = e.scrollHeight - e.scrollTop - e.clientHeight < 40;
		if (e.scrollTop < 80 && !msgs?.reachedTop && !anchor.current && list.length) {
			anchor.current = e.scrollHeight - e.scrollTop;
			onLoadOlder();
		}
		if (pinned.current) markRead();
	}

	// Keep the viewport stable: pinned follows new messages; a prepend
	// keeps the previously visible message where it was.
	useLayoutEffect(() => {
		const e = el.current;
		if (!e) return;
		if (anchor.current !== null && first) {
			e.scrollTop = e.scrollHeight - anchor.current;
			anchor.current = null;
		} else if (pinned.current) {
			e.scrollTop = e.scrollHeight;
		}
	}, [first?.id, last?.id, buf?.key]);

	useEffect(() => {
		pinned.current = true;
		anchor.current = null;
	}, [buf?.key]);

	const markRead = () => {
		if (last && document.hasFocus()) onRead(last.time);
	};
	useEffect(() => {
		if (pinned.current) markRead();
		window.addEventListener("focus", markRead);
		return () => window.removeEventListener("focus", markRead);
	}, [last?.id, buf?.key]);

	function submit(e) {
		e.preventDefault();
		const text = draft.trim();
		if (!text || !connected) return;
		onSend(text);
		setDraft("");
	}

	return (
		<>
			<div class="msgs scroll" ref={el} onScroll={onScroll}>
				{msgs?.reachedTop && list.length > 0 && (
					<div class="top-note">beginning of history</div>
				)}
				{list.map((ev, i) => (
					<Row key={ev.id} ev={ev} prev={list[i - 1]} selfNick={selfNick} theme={theme} />
				))}
				{!msgs?.loaded && <div class="top-note">loading…</div>}
			</div>
			<div class="composer">
				<form class="compose-box" onSubmit={submit}>
					<span class="prompt">{selfNick || "…"} ›</span>
					<input
						class="compose-input"
						value={draft}
						onInput={(e) => setDraft(e.currentTarget.value)}
						placeholder={connected ? `Message ${buf?.buffer || ""}` : "disconnected — reconnecting…"}
						disabled={!connected}
					/>
					<button class="btn-accent" type="submit" disabled={!connected}>Send</button>
				</form>
			</div>
		</>
	);
}
