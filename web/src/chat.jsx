import { Fragment } from "preact";
import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { Completer } from "./complete.js";
import { longPress } from "./menu.jsx";
import { firstURL, fmtTime, linkify, nickColor, renderable, TypingSender, typingText } from "./irc.js";
import { LinkPreview } from "./preview.jsx";
import { VirtualList } from "./vlist.jsx";
import { WhoisCard } from "./whois.jsx";
import { estimateMsgHeight } from "./vmath.js";

function Body({ text }) {
	// draft/multiline messages carry embedded newlines; render each line
	// on its own row, linkifying within each.
	const lines = text.split("\n");
	return lines.map((line, li) => (
		<Fragment key={li}>
			{li > 0 && <br />}
			{linkify(line).map((seg, si) =>
				seg.link
					? <a key={si} href={seg.text} target="_blank" rel="noopener noreferrer">{seg.text}</a>
					: seg.text,
			)}
		</Fragment>
	));
}

function SysRow({ ev, r, focused }) {
	return (
		<div class={"sys-row" + (r.kind === "redacted" ? " redacted" : "") + (focused ? " flash" : "")}>
			<span class="msg-time">{fmtTime(ev.time)}</span>
			<span class={"sys-mark " + r.markClass}>{r.mark}</span>
			<span>{r.text}</span>
		</div>
	);
}

function Row({ ev, selfNick, theme, focused, isHighlight, onRedact, onNick }) {
	const pressFired = { current: false };
	if (ev.whois) return <WhoisCard whois={ev.whois} focused={focused} />;
	const r = renderable(ev);
	if (r.kind === "system" || r.kind === "redacted") {
		return <SysRow ev={ev} r={r} focused={focused} />;
	}
	const self = selfNick && ev.sender === selfNick;
	const mention = !self && isHighlight(r.text);
	// One preview per message (the first link), only for real messages.
	const link = r.kind === "msg" || r.kind === "action" ? firstURL(r.text) : "";
	const color = self ? "var(--accent)" : nickColor(ev.sender, theme);
	// Own messages can be deleted (server decides authorization).
	const canRedact = self && ev.msgid && onRedact;
	// Actions show "*" in the nick column (the sender leads the body).
	const nickLabel = r.kind === "action" ? "*" : ev.sender;
	return (
		<div class={"msg-row" + (mention ? " mention" : "") + (focused ? " flash" : "")}>
			<span class="msg-time">{fmtTime(ev.time)}</span>
			<span
				class={"msg-nick" + (ev.sender ? " has-menu" : "")}
				style={{ color }}
				title={ev.sender}
				onContextMenu={ev.sender ? (e) => {
					e.preventDefault();
					onNick(ev.sender, e.clientX, e.clientY);
				} : undefined}
				{...(ev.sender ? longPress((x, y) => onNick(ev.sender, x, y), pressFired) : {})}
			>
				{nickLabel}
			</span>
			<div class={"msg-body" + (r.kind === "action" ? " action" : "") + (r.kind === "notice" ? " notice" : "")}>
				{r.kind === "action" && (
					<span
						class="has-menu"
						style={{ color, fontWeight: 600 }}
						onContextMenu={(e) => {
							e.preventDefault();
							onNick(ev.sender, e.clientX, e.clientY);
						}}
					>{ev.sender} </span>
				)}
				{r.bot && <span class="bot-chip" title="flagged as a bot">bot</span>}
				<Body text={r.text} />
				{link && <LinkPreview url={link} />}
			</div>
			{canRedact && (
				<button class="msg-redact" title="Delete message" onClick={() => onRedact(ev.msgid)}>⌫</button>
			)}
		</div>
	);
}

function estimate(ev) {
	return ev.whois ? 200 : estimateMsgHeight(ev.raw);
}

// Chat renders the active buffer: virtualized scrollback plus composer.
// completionNicks feeds tab-completion (channel roster, or the query
// counterpart).
export function Chat({ buf, msgs, selfNick, theme, connected, error, typers, focusId, completionNicks, ignoredNicks, composerApi, isHighlight, onSend, onLoadOlder, onRead, onTyping, onRedact, onNick }) {
	const [draft, setDraft] = useState("");
	const pinned = useRef(true);
	const loadingOlder = useRef(false);
	// Tab cycles candidates; fresh state per buffer.
	const completer = useMemo(() => new Completer(), [buf?.key]);
	const list = msgs?.list || [];
	const last = list[list.length - 1];
	// Hide ignored senders from view (they are still stored, so
	// un-ignoring reveals them live). Zero cost when nobody is ignored.
	const ignoreKey = (ignoredNicks || []).join("\n");
	const shown = useMemo(() => {
		if (!ignoreKey) return list;
		const set = new Set(ignoreKey.split("\n"));
		return list.filter((ev) => !ev.sender || !set.has(ev.sender.toLowerCase()));
	}, [list, ignoreKey]);

	// Typing notifications, one sender per buffer; the previous buffer's
	// session ends with "done" when switching away mid-draft.
	const typing = useMemo(() => new TypingSender((state) => onTypingRef.current(state)), [buf?.key]);
	const onTypingRef = useRef(onTyping);
	onTypingRef.current = onTyping;
	const pauseTimer = useRef(null);
	useEffect(() => () => {
		clearTimeout(pauseTimer.current);
		typing.done();
	}, [typing]);

	// Imperative composer prefill (context-menu "edit topic").
	const taRef = useRef(null);
	useEffect(() => {
		if (!composerApi) return;
		composerApi.current = {
			prefill: (text) => {
				setDraft(text);
				requestAnimationFrame(() => {
					const el = taRef.current;
					if (el) {
						el.focus();
						el.setSelectionRange(text.length, text.length);
					}
				});
			},
		};
	}, [composerApi]);

	function draftChanged(text) {
		setDraft(text);
		typing.input(text);
		clearTimeout(pauseTimer.current);
		pauseTimer.current = setTimeout(() => typing.pause(text), 5000);
	}

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
		clearTimeout(pauseTimer.current);
		typing.messageSent();
	}

	const header = msgs?.loaded
		? (msgs.reachedTop && list.length > 0 && <div class="top-note">beginning of history</div>)
		: <div class="top-note">loading…</div>;

	return (
		<>
			<VirtualList
				key={buf?.key}
				items={shown}
				estimate={estimate}
				header={header}
				focusId={focusId}
				onNearTop={nearTop}
				onPinned={(p) => {
					pinned.current = p;
					if (p) markRead();
				}}
				renderItem={(ev, i) => (
					<Row ev={ev} selfNick={selfNick} theme={theme} focused={ev.id === focusId} isHighlight={isHighlight} onRedact={onRedact} onNick={onNick} />
				)}
			/>
			<div class="composer">
				{typers?.length > 0 && (
					<div class="typing-bubble">
						<span class="typing-dots">
							<span /><span /><span />
						</span>
						<span class="typing-label">{typingText(typers)}</span>
					</div>
				)}
				{error && <div class="cmd-error">{error}</div>}
				<form class="compose-box" onSubmit={submit}>
					<span class="prompt">{selfNick || "…"} ›</span>
					<textarea
						ref={taRef}
						class="compose-input"
						rows={draft.includes("\n") ? Math.min(draft.split("\n").length, 8) : 1}
						value={draft}
						onInput={(e) => draftChanged(e.currentTarget.value)}
						onKeyDown={(e) => {
							// Tab completes commands/emoji/nicks and cycles on
							// repeat; Shift+Tab cycles backwards.
							if (e.key === "Tab") {
								e.preventDefault();
								const ta = e.currentTarget;
								const r = completer.next(ta.value, ta.selectionStart, e.shiftKey ? -1 : 1, {
									nicks: completionNicks || [],
								});
								if (r) {
									draftChanged(r.text);
									requestAnimationFrame(() => ta.setSelectionRange(r.caret, r.caret));
								}
								return;
							}
							// Enter sends; Shift+Enter inserts a newline (multiline).
							if (e.key === "Enter" && !e.shiftKey) {
								e.preventDefault();
								submit(e);
							}
						}}
						placeholder={connected ? `Message ${buf?.buffer || ""}` : "disconnected — reconnecting…"}
						disabled={!connected}
					/>
					<button class="btn-accent" type="submit" disabled={!connected}>Send</button>
				</form>
			</div>
		</>
	);
}
