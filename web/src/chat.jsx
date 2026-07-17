import { Fragment } from "preact";
import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { Completer } from "./complete.js";
import { menuTrigger } from "./menu.jsx";
import { applyStatusMode, firstURL, fmtTime, linkify, nickColor, renderable, TypingSender, typingText } from "./irc.js";
import { LinkPreview } from "./preview.jsx";
import { VirtualList } from "./vlist.jsx";
import { WhoisCard } from "./whois.jsx";
import { estimateMsgHeight } from "./vmath.js";

// isEditable reports whether an element is a text field the user may be
// typing in, so window-refocus doesn't yank the cursor out of it.
function isEditable(el) {
	const tag = el.tagName;
	return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || el.isContentEditable;
}

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

function SysRow({ ev, r, focused, timeFmt }) {
	return (
		<div class={"sys-row" + (r.kind === "redacted" ? " redacted" : "") + (focused ? " flash" : "")}>
			<span class="msg-time">{fmtTime(ev.time, timeFmt)}</span>
			<span class={"sys-mark " + r.markClass}>{r.mark}</span>
			<span>{r.text}</span>
		</div>
	);
}

// A folded run of join/part/quit/nick lines; clicking toggles the run.
// The Lounge look: no timestamp, left-aligned, chevron trailing; the
// expanded lines render as ordinary status rows below it.
function CollapsedRow({ ev, onToggle }) {
	return (
		<div class="sys-row collapse-row">
			<button type="button" class="sys-toggle" onClick={() => onToggle(ev.id)}>
				{ev.summary} <span class="sys-chevron">{ev.expanded ? "▾" : "▸"}</span>
			</button>
		</div>
	);
}

function Row({ ev, selfNick, theme, focused, isHighlight, onRedact, onNick, onToggle, timeFmt, nickSep, previews }) {
	if (ev.whois) return <WhoisCard whois={ev.whois} focused={focused} />;
	if (ev.collapse) return <CollapsedRow ev={ev} onToggle={onToggle} />;
	const r = renderable(ev);
	if (r.kind === "system" || r.kind === "redacted") {
		return <SysRow ev={ev} r={r} focused={focused} timeFmt={timeFmt} />;
	}
	const self = selfNick && ev.sender === selfNick;
	const mention = !self && isHighlight(r.text);
	// One preview per message (the first link), only for real messages.
	const link = r.kind === "msg" || r.kind === "action" ? firstURL(r.text) : "";
	const color = self ? "var(--accent)" : nickColor(ev.sender, theme);
	// Own messages can be deleted (server decides authorization).
	const canRedact = self && ev.msgid && onRedact;
	// Actions show "*" in the nick column (the sender leads the body); a
	// real nick can carry a user-chosen separator ("AlteredParadox:"), colored with
	// the nick.
	const isAction = r.kind === "action";
	const nickLabel = isAction ? "*" : ev.sender;
	const sep = !isAction && ev.sender ? (nickSep || "") : "";
	return (
		<div class={"msg-row" + (mention ? " mention" : "") + (focused ? " flash" : "")}>
			<span class="msg-time">{fmtTime(ev.time, timeFmt)}</span>
			<span
				class={"msg-nick" + (ev.sender ? " has-menu" : "")}
				style={{ color }}
				title={ev.sender}
				{...(ev.sender ? menuTrigger((x, y) => onNick(ev.sender, x, y)) : {})}
			>
				{nickLabel}{sep}
			</span>
			<div class={"msg-body" + (r.kind === "action" ? " action" : "") + (r.kind === "notice" ? " notice" : "")}>
				{r.kind === "action" && (
					<span
						class="has-menu"
						style={{ color, fontWeight: 600 }}
						{...menuTrigger((x, y) => onNick(ev.sender, x, y))}
					>{ev.sender} </span>
				)}
				{r.bot && <span class="bot-chip" title="flagged as a bot">bot</span>}
				<Body text={r.text} />
			</div>
			{canRedact && (
				<button class="msg-redact" title="Delete message" onClick={() => onRedact(ev.msgid)}>⌫</button>
			)}
			{/* Previews wrap to their own full-width line, left-aligned under
			    the timestamp rather than indented into the body column.
			    `previews` is the server switch — off means no fetch at all. */}
			{link && previews !== false && <div class="msg-media"><LinkPreview url={link} net={ev.network} /></div>}
		</div>
	);
}

function estimate(ev) {
	if (ev.whois) return 200;
	if (ev.collapse) return 28;
	return estimateMsgHeight(ev.raw);
}

// Chat renders the active buffer: virtualized scrollback plus composer.
// completionNicks feeds tab-completion (channel roster, or the query
// counterpart).
export function Chat({ buf, msgs, selfNick, theme, connected, error, typers, focusId, completionNicks, ignoredNicks, statusMode, timeFmt, nickSep, previews, composerApi, isHighlight, onSend, onLoadOlder, onReloadTail, onRead, onTyping, onRedact, onNick }) {
	const [draft, setDraft] = useState("");
	// Per-buffer drafts: keep half-typed text with its own buffer so a
	// switch swaps the composer contents instead of carrying text into —
	// and letting Enter send it to — the wrong channel.
	const drafts = useRef({});
	const pinned = useRef(true);
	const loadingOlder = useRef(false);
	// Tab cycles candidates; fresh state per buffer.
	const completer = useMemo(() => new Completer(), [buf?.key]);
	const list = msgs?.list || [];
	const last = list[list.length - 1];
	// Hide ignored senders from view (they are still stored, so
	// un-ignoring reveals them live). Zero cost when nobody is ignored.
	const ignoreKey = (ignoredNicks || []).join("\n");
	// Expanded collapse-run ids; fresh per buffer.
	const [expanded, setExpanded] = useState(() => new Set());
	useEffect(() => setExpanded(new Set()), [buf?.key]);
	// Swap in this buffer's saved draft (empty if none).
	useEffect(() => setDraft(drafts.current[buf?.key] || ""), [buf?.key]);
	const toggleRun = (id) => setExpanded((old) => {
		const next = new Set(old);
		if (next.has(id)) next.delete(id);
		else next.add(id);
		return next;
	});
	const shown = useMemo(() => {
		let out = list;
		if (ignoreKey) {
			const set = new Set(ignoreKey.split("\n"));
			out = out.filter((ev) => !ev.sender || !set.has(ev.sender.toLowerCase()));
		}
		return applyStatusMode(out, statusMode || "show", expanded);
	}, [list, ignoreKey, statusMode, expanded]);

	// Typing notifications, one sender per buffer; the previous buffer's
	// session ends with "done" when switching away mid-draft.
	const onTypingRef = useRef(onTyping);
	onTypingRef.current = onTyping;
	const typing = useMemo(() => {
		// Capture THIS buffer's identity: the teardown "done" fires after
		// the active buffer has advanced, so it must carry the buffer it
		// was created for, not whatever is active when it runs.
		const net = buf?.network, name = buf?.buffer;
		return new TypingSender((state) => onTypingRef.current(state, net, name));
	}, [buf?.key]);
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
		drafts.current[buf?.key] = text;
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

	// Type-anywhere (The Lounge behaviour): a printable keystroke while
	// focus isn't in a text field routes into the composer, so the user can
	// click around the UI — or return to the tab — and just start typing
	// without clicking the box first. The triggering key is not consumed
	// (no preventDefault), so it lands in the now-focused textarea.
	useEffect(() => {
		const onKey = (e) => {
			if (e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return; // shortcuts / IME
			if (e.key.length !== 1) return; // Tab/Enter/Escape/arrows/F-keys: ignore
			const el = taRef.current;
			if (!el || el.disabled) return;
			// A modal overlay (settings, search, switcher, network form,
			// context menu, mobile panel) is open — don't yank focus into the
			// composer behind it and silently accumulate a hidden draft.
			if (document.querySelector(".search-scrim, .ctx-scrim, .side-scrim, .right-scrim")) return;
			const active = document.activeElement;
			if (active === el || (active && active !== document.body && isEditable(active))) return;
			el.focus();
		};
		document.addEventListener("keydown", onKey);
		return () => document.removeEventListener("keydown", onKey);
	}, []);

	function submit(e) {
		e.preventDefault();
		const text = draft.trim();
		if (!text || !connected) return;
		onSend(text);
		setDraft("");
		delete drafts.current[buf?.key];
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
					if (!p) return;
					// Reaching the bottom of a buffer showing a non-tail
					// window (paged back past the trim point, or a search
					// jump) reloads the live tail — otherwise live events,
					// which are suppressed while atTail is false, would never
					// reappear until a manual re-select.
					if (msgs?.atTail === false) onReloadTail?.();
					else markRead();
				}}
				renderItem={(ev, i) => (
					<Row ev={ev} selfNick={selfNick} theme={theme} focused={ev.id === focusId} isHighlight={isHighlight} onRedact={onRedact} onNick={onNick} onToggle={toggleRun} timeFmt={timeFmt} nickSep={nickSep} previews={previews} />
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
