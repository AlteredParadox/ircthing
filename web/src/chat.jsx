import { useEffect, useRef, useState } from "preact/hooks";
import { fmtTime, linkify, mentionsMe, nickColor, renderable, sameGroup } from "./irc.js";
import { VirtualList } from "./vlist.jsx";
import { estimateMsgHeight } from "./vmath.js";

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

function estimate(ev) {
	return estimateMsgHeight(ev.raw);
}

// Chat renders the active buffer: virtualized scrollback plus composer.
export function Chat({ buf, msgs, selfNick, theme, connected, error, onSend, onLoadOlder, onRead }) {
	const [draft, setDraft] = useState("");
	const pinned = useRef(true);
	const loadingOlder = useRef(false);
	const list = msgs?.list || [];
	const last = list[list.length - 1];

	function nearTop() {
		if (loadingOlder.current || msgs?.reachedTop || !list.length) return;
		loadingOlder.current = true;
		Promise.resolve(onLoadOlder()).finally(() => {
			loadingOlder.current = false;
		});
	}

	const markRead = () => {
		if (last && pinned.current && document.hasFocus()) onRead(last.time);
	};
	useEffect(() => {
		markRead();
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

	const header = msgs?.loaded
		? (msgs.reachedTop && list.length > 0 && <div class="top-note">beginning of history</div>)
		: <div class="top-note">loading…</div>;

	return (
		<>
			<VirtualList
				key={buf?.key}
				items={list}
				estimate={estimate}
				header={header}
				onNearTop={nearTop}
				onPinned={(p) => {
					pinned.current = p;
					if (p) markRead();
				}}
				renderItem={(ev, i) => (
					<Row ev={ev} prev={list[i - 1]} selfNick={selfNick} theme={theme} />
				)}
			/>
			<div class="composer">
				{error && <div class="cmd-error">{error}</div>}
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
