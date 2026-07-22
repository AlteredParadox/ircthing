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

import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { Completer } from "./complete.js";
import { isEditable, modalScrimOpen } from "./dom.js";
import { menuTrigger } from "./menu.jsx";
import { applyFormat, BOLD, ITALIC, UNDERLINE } from "./format.js";
import { FormatPanel } from "./formatpanel.jsx";
import { InputHistory, isFirstLine, isLastLine } from "./inputhistory.js";
import { applyStatusMode, firstURL, fmtTime, highlightNicks, linkify, nickColor, nickSet, parseFormatting, renderable, SERVER_BUFFER, stripFormatting, TypingSender, typingText } from "./irc.js";
import { LinkPreview } from "./preview.jsx";
import { VirtualList } from "./vlist.jsx";
import { WhoisCard } from "./whois.jsx";
import { estimateMsgHeight } from "./vmath.js";
import { readTimestamp } from "./readts.js";


// MAX_BODY_NODES caps the DOM nodes ONE message may render across all passes
// (formatting runs, <br>, links, nick spans). The formatting-run cap alone
// doesn't bound this: nick highlighting can split a single run into thousands
// of spans, and a visible message is re-diffed on every composer keystroke. A
// message's text is already clamped to 16 KiB by the store, so the true ceiling
// is ~16-25k nodes; 4000 is far above any real message yet caps the adversarial
// case. Past it, the remainder renders as one plain text node.
const MAX_BODY_NODES = 4000;

// pushBodyText appends one non-link text segment to `out`: plain text unless
// nick highlighting is on (nicks map present), in which case known nicks
// mentioned in the text become colored, clickable spans with the user menu.
// It decrements the shared node budget; when the budget runs out mid-segment
// the untouched tail is appended as one plain string (never dropped).
function pushBodyText(out, budget, text, nicks, theme, onNick, keyBase) {
	if (!nicks) {
		out.push(text);
		budget.n--;
		return;
	}
	const parts = highlightNicks(text, nicks);
	for (let i = 0; i < parts.length; i++) {
		if (budget.n <= 0) {
			out.push(parts.slice(i).map((p) => p.text).join(""));
			return;
		}
		const p = parts[i];
		out.push(p.nick
			? (
				<span
					key={keyBase + "n" + i}
					class="body-nick has-menu"
					style={{ color: nickColor(p.nick, theme) }}
					{...menuTrigger((x, y) => onNick(p.nick, x, y))}
				>{p.text}</span>
			)
			: p.text);
		budget.n--;
	}
}

// fmtWrap wraps a run's rendered inner content in a span carrying its mIRC
// formatting. An unstyled run renders inline with no wrapper (the common case).
// Every style value comes from the fixed palette or a boolean, so there is no
// injection surface (and Preact escapes the text content).
function fmtWrap(run, inner, key) {
	if (!run.bold && !run.italic && !run.underline && !run.strike && !run.mono && !run.reverse && run.fg == null && run.bg == null) {
		return inner;
	}
	let { fg, bg } = run;
	if (run.reverse) { // swap fg/bg, falling back to the theme defaults
		[fg, bg] = [bg ?? "var(--bg)", fg ?? "var(--text)"];
	}
	const style = {};
	if (fg != null) style.color = fg;
	if (bg != null) { style.background = bg; style.padding = "0 2px"; style.borderRadius = "3px"; }
	if (run.bold) style.fontWeight = "700";
	if (run.italic) style.fontStyle = "italic";
	if (run.mono) style.fontFamily = "var(--mono-font)";
	const deco = [run.underline && "underline", run.strike && "line-through"].filter(Boolean).join(" ");
	if (deco) style.textDecoration = deco;
	return <span key={key} class="fmt" style={style}>{inner}</span>;
}

// renderRun renders one formatting run's inner nodes under the shared node
// budget, resolving <br/>, links, and nick mentions within each \n-delimited
// segment. Returns { inner, truncated }: truncated means the budget ran out
// partway, so the caller drops the partial and re-emits the whole run as plain
// text (no styling, but no text lost).
function renderRun(run, ri, budget, nicks, theme, onNick) {
	const inner = [];
	const lines = run.text.split("\n");
	for (let pi = 0; pi < lines.length; pi++) {
		if (pi > 0) {
			if (budget.n <= 0) return { inner, truncated: true };
			inner.push(<br key={ri + "-br-" + pi} />);
			budget.n--;
		}
		for (const [si, seg] of linkify(lines[pi]).entries()) {
			if (budget.n <= 0) return { inner, truncated: true };
			const kp = ri + "-" + pi + "-" + si;
			if (seg.link) {
				inner.push(<a key={kp} href={seg.text} target="_blank" rel="noopener noreferrer">{seg.text}</a>);
				budget.n--;
			} else {
				pushBodyText(inner, budget, seg.text, nicks, theme, onNick, kp + "-");
			}
		}
	}
	return { inner, truncated: false };
}

function Body({ text, nicks, theme, onNick }) {
	// Parse mIRC formatting ONCE across the WHOLE message so the styled-run cap
	// (MAX_FMT_RUNS) bounds the entire body — splitting per line first would let a
	// hostile many-line body multiply the cap by the line count. draft/multiline
	// newlines survive as plain text inside runs and become <br/> at render;
	// links and nick mentions resolve within each \n-delimited run segment.
	const runs = parseFormatting(text);
	const budget = { n: MAX_BODY_NODES }; // shared across every run/line/segment
	const out = [];
	let ri = 0;
	for (; ri < runs.length && budget.n > 0; ri++) {
		const { inner, truncated } = renderRun(runs[ri], ri, budget, nicks, theme, onNick);
		// On truncation, leave ri ON this run (no push, no advance): the plain-text
		// tail below re-includes it whole (unstyled but complete) — nothing dropped
		// or duplicated.
		if (truncated) break;
		out.push(fmtWrap(runs[ri], inner, ri));
	}
	if (ri < runs.length) {
		// Node budget spent by an adversarial body: everything still unprocessed
		// (including the run we broke out of) renders as ONE plain text node.
		out.push(runs.slice(ri).map((r) => r.text).join(""));
	}
	return out;
}

function SysRow({ ev, r, focused, timeFmt }) {
	return (
		<div class={"sys-row" + (r.kind === "redacted" ? " redacted" : "") + (focused ? " flash" : "")}>
			<span class="msg-time">{fmtTime(ev.time, timeFmt)}</span>
			<span class={"sys-mark " + r.markClass}>{r.mark}</span>
			{/* System rows (part/quit/kick reasons, TOPIC) are never colour-
			    rendered, so strip mIRC codes rather than leak their digits. */}
			<span>{stripFormatting(r.text)}</span>
		</div>
	);
}

// A folded run of join/part/quit/nick lines; clicking toggles the run.
// The Lounge look: no timestamp, left-aligned, chevron trailing; the
// expanded lines render as ordinary status rows below it.
function CollapsedRow({ ev, onToggle }) {
	return (
		<div class="sys-row collapse-row">
			<button type="button" class="sys-toggle" onClick={() => onToggle(ev.collapse)}>
				{ev.summary} <span class="sys-chevron">{ev.expanded ? "▾" : "▸"}</span>
			</button>
		</div>
	);
}

// Row dispatches by event kind; a real message renders via MsgRow.
function Row({ ev, selfNick, theme, focused, isHighlight, onRedact, onNick, onToggle, nicks, userhosts, statusHost, memberPrefixes, timeFmt, nickSep, previews }) {
	if (ev.whois) return <WhoisCard whois={ev.whois} focused={focused} />;
	if (ev.collapse) return <CollapsedRow ev={ev} onToggle={onToggle} />;
	const r = renderable(ev, statusHost);
	if (r.kind === "system" || r.kind === "redacted") {
		return <SysRow ev={ev} r={r} focused={focused} timeFmt={timeFmt} />;
	}
	return (
		<MsgRow
			ev={ev} r={r} selfNick={selfNick} theme={theme} focused={focused}
			isHighlight={isHighlight} onRedact={onRedact} onNick={onNick} nicks={nicks} userhosts={userhosts}
			memberPrefixes={memberPrefixes}
			timeFmt={timeFmt} nickSep={nickSep} previews={previews}
		/>
	);
}

// RowNick is the nick column: colored, right-click menu, optional leading
// mode symbol ("@nick", the nickPrefixes pref) and trailing separator
// ("AlteredParadox:").
function RowNick({ sender, color, label, sep, onNick, userhost, prefix }) {
	return (
		<span
			class={"msg-nick" + (sender ? " has-menu" : "")}
			style={{ color }}
			title={userhost ? `${sender} (${userhost})` : sender}
			{...(sender ? menuTrigger((x, y) => onNick(sender, x, y)) : {})}
		>{prefix && <span class="msg-nick-mode">{prefix}</span>}{label}{sep}</span>
	);
}

// RowBody is the message text column: an action leads with the sender, a
// bot chip, then the (multiline-aware) body.
function RowBody({ r, color, sender, onNick, nicks, theme }) {
	return (
		<div class={"msg-body" + (r.kind === "action" ? " action" : "") + (r.kind === "notice" ? " notice" : "")}>
			{r.kind === "action" && (
				<span class="has-menu" style={{ color, fontWeight: 600 }} {...menuTrigger((x, y) => onNick(sender, x, y))}>{sender} </span>
			)}
			{r.bot && <span class="bot-chip" title="flagged as a bot">bot</span>}
			<Body text={r.text} nicks={nicks} theme={theme} onNick={onNick} />
		</div>
	);
}

function MsgRow({ ev, r, selfNick, theme, focused, isHighlight, onRedact, onNick, nicks, userhosts, memberPrefixes, timeFmt, nickSep, previews }) {
	const self = selfNick && ev.sender === selfNick;
	const mention = !self && isHighlight(r.text);
	// One preview per message (the first link), only for real messages.
	const link = r.kind === "msg" || r.kind === "action" ? firstURL(r.text) : "";
	const color = self ? "var(--accent)" : nickColor(ev.sender, theme);
	// Own messages can be deleted (server decides authorization).
	const canRedact = self && ev.msgid && onRedact;
	// Actions show "*" in the nick column (the sender leads the body); a
	// real nick can carry a user-chosen separator ("AlteredParadox:").
	const isAction = r.kind === "action";
	const nickLabel = isAction ? "*" : ev.sender;
	const sep = !isAction && ev.sender ? (nickSep || "") : "";
	// Channel mode symbol (@, +, …) in front of the nick — only for real nicks
	// (an action already leads with "*"), and only when memberPrefixes is
	// supplied (the nickPrefixes pref is on and the sender is on the roster).
	const modePrefix = !isAction && ev.sender ? memberPrefixes?.get(ev.sender.toLowerCase()) || "" : "";
	return (
		<div class={"msg-row" + (mention ? " mention" : "") + (focused ? " flash" : "")}>
			<span class="msg-time">{fmtTime(ev.time, timeFmt)}</span>
			<RowNick sender={ev.sender} color={color} label={nickLabel} sep={sep} onNick={onNick} userhost={userhosts?.get(ev.sender?.toLowerCase())} prefix={modePrefix} />
			<RowBody r={r} color={color} sender={ev.sender} onNick={onNick} nicks={nicks} theme={theme} />
			{canRedact && (
				<button class="msg-redact" title="Delete message" onClick={() => onRedact(ev.msgid)}>⌫</button>
			)}
			{/* Previews wrap to their own full-width line, left-aligned under
			    the timestamp rather than indented into the body column.
			    `previews` is the server switch — off means no fetch at all. */}
			{link && previews === true && <div class="msg-media"><LinkPreview url={link} net={ev.network} /></div>}
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
export function Chat({ buf, msgs, selfNick, theme, tailNav, connected, error, typers, focusId, completionNicks, ignoredNicks, statusMode, statusHost, timeFmt, nickSep, previews, highlightNames, userhosts, nickPrefixes, memberPrefixes, layoutKey, composerApi, isHighlight, onSend, onLoadOlder, onReloadTail, onRead, onTyping, onRedact, onNick }) {
	// Lookup for in-body nick highlighting (Settings toggle): channel roster
	// minus our own nick. Null when off, so the row renderer skips the scan.
	const nickHi = useMemo(
		() => (highlightNames ? nickSet(completionNicks, selfNick) : null),
		[highlightNames, completionNicks, selfNick],
	);
	// Per-buffer drafts: keep half-typed text with its own buffer so a
	// switch swaps the composer contents instead of carrying text into —
	// and letting Enter send it to — the wrong channel.
	const [drafts, setDrafts] = useState(() => new Map());
	const activeDraftKey = buf?.key;
	const activeDraftKeyRef = useRef(activeDraftKey);
	activeDraftKeyRef.current = activeDraftKey;
	const draft = drafts.get(activeDraftKey) || "";
	const pinned = useRef(true);
	const loadingOlder = useRef(false);
	// Tab cycles candidates; fresh state per buffer.
	const completer = useMemo(() => new Completer(), [buf?.key]);
	// Up/Down recall of sent messages. One instance for the session (the
	// module keeps per-buffer state internally, so switching buffers keeps
	// each buffer's history); in-memory only, gone on reload.
	const inputHistory = useMemo(() => new InputHistory(), []);
	// The mIRC-formatting popover (format button / Alt+F).
	const [fmtOpen, setFmtOpen] = useState(false);
	const list = msgs?.list || [];
	const last = list[list.length - 1];
	// Max server-stamped time, excluding browser-clock-stamped local info
	// rows — see readTimestamp for why both matter. Monotonic under budget
	// trims (they drop the oldest).
	const readTS = useMemo(() => readTimestamp(list), [list]);
	// Hide ignored senders from view (they are still stored, so
	// un-ignoring reveals them live). Zero cost when nobody is ignored.
	const ignoreKey = (ignoredNicks || []).join("\n");
	// Expanded collapse runs, tracked by a member event id (see
	// applyStatusMode): a run stays open across growth at either end.
	const [expanded, setExpanded] = useState(() => new Set());
	useEffect(() => setExpanded(new Set()), [buf?.key]);
	const toggleRun = (run) => setExpanded((old) => {
		const next = new Set(old);
		const open = run.find((e) => next.has(e.id));
		if (open) next.delete(open.id); // collapse: drop whichever member opened it
		else next.add(run[run.length - 1].id); // expand: anchor on the last event
		return next;
	});
	const shown = useMemo(() => {
		let out = list;
		if (ignoreKey) {
			const set = new Set(ignoreKey.split("\n"));
			// Always keep the jump target (a search result can be from an
			// ignored sender — ignores are client-only, FTS is not). Filtering
			// it out would leave the jump with no row to scroll to, so the
			// buffer silently snaps back to its live tail.
			out = out.filter((ev) => ev.id === focusId || !ev.sender || !set.has(ev.sender.toLowerCase()));
		}
		return applyStatusMode(out, statusMode || "show", expanded);
	}, [list, ignoreKey, statusMode, expanded, focusId]);

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
		const api = {
			// The caller must name the destination. An async topic fetch for A
			// must never place its text into whichever buffer happens to be active
			// when it resolves.
			prefill: (key, text) => {
				if (!key) return;
				setDrafts((old) => {
					const next = new Map(old);
					next.set(key, text);
					return next;
				});
				requestAnimationFrame(() => {
					if (activeDraftKeyRef.current !== key) return;
					const el = taRef.current;
					if (el) {
						el.focus();
						el.setSelectionRange(text.length, text.length);
					}
				});
			},
		};
		composerApi.current = api;
		return () => {
			if (composerApi.current === api) composerApi.current = null;
		};
	}, [composerApi]);

	function draftChanged(text) {
		const key = activeDraftKey;
		if (!key) return;
		setDrafts((old) => {
			const next = new Map(old);
			if (text) next.set(key, text);
			else next.delete(key);
			return next;
		});
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
		if (readTS && pinned.current && document.hasFocus()) onRead(readTS);
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
			// context menu, mobile drawer) is open — don't yank focus into the
			// composer behind it and silently accumulate a hidden draft. Test
			// actual visibility, not mere DOM presence: the side/right scrims
			// are always rendered on desktop but display:none there (they are
			// modal only at mobile breakpoints), and querySelector ignores CSS
			// display — matching them would kill type-anywhere on desktop.
			if (modalScrimOpen()) return;
			const active = document.activeElement;
			if (active === el || (active && active !== document.body && isEditable(active))) return;
			el.focus();
		};
		document.addEventListener("keydown", onKey);
		return () => document.removeEventListener("keydown", onKey);
	}, []);

	function submit(e) {
		e.preventDefault();
		const raw = draft; // untrimmed keyed-draft snapshot
		const text = raw.trim();
		if (!text || !connected) return;
		const key = buf?.key;
		clearTimeout(pauseTimer.current);
		typing.messageSent();
		// Clear the composer only once the send is ACCEPTED, so a rejected
		// send (parse error, full queue, mid-send disconnect) keeps the typed
		// text instead of silently dropping it. Clear the buffer we sent from
		// (a /msg or /join may have switched the active buffer), and guard on
		// equality against the UNTRIMMED snapshot so text the user kept typing
		// while waiting isn't clobbered — and so a draft with surrounding
		// whitespace (e.g. "hi " from a mobile keyboard) still clears.
		Promise.resolve(onSend(text, key))
			.then(() => {
				// Record in Up/Down history only once the send is ACCEPTED —
				// same rule as clearing — so a rejected line isn't recallable
				// as if it had been sent. Commands count as entries too.
				inputHistory.push(key, text);
				setDrafts((old) => {
					if (old.get(key) !== raw) return old;
					const next = new Map(old);
					next.delete(key);
					return next;
				});
			})
			.catch(() => {});
	}

	// applyFmt inserts a formatting toggle at the composer's current
	// selection (wrapping it if non-empty) and restores focus + the computed
	// selection. Shared by the Ctrl+B/I/U shortcuts and the format panel.
	function applyFmt(code) {
		const ta = taRef.current;
		if (!ta || ta.disabled) return;
		const r = applyFormat(ta.value, ta.selectionStart, ta.selectionEnd, code);
		draftChanged(r.text);
		requestAnimationFrame(() => {
			ta.focus();
			ta.setSelectionRange(r.selStart, r.selEnd);
		});
	}

	// Ctrl/Cmd+B/I/U insert mIRC formatting toggles. Strikethrough/monospace/
	// colours live in the format panel only (no conflict-free chords; Ctrl+S
	// stays the browser's). Ctrl+K is NOT handled here — it must fall through
	// to the global channel-switcher shortcut.
	function handleFormatShortcut(e) {
		if (!(e.ctrlKey || e.metaKey) || e.altKey || e.shiftKey) return false;
		const k = e.key.toLowerCase();
		let code = null;
		if (k === "b") code = BOLD;
		else if (k === "i") code = ITALIC;
		else if (k === "u") code = UNDERLINE;
		// Any other Ctrl/Cmd chord falls through
		// (Ctrl+Enter must still reach the send branch).
		if (!code) return false;
		e.preventDefault(); // Ctrl+U is view-source etc.
		applyFmt(code);
		return true;
	}

	// Alt+F toggles the format panel (free in-app: the global shortcuts only
	// use Alt+arrows; browsers' Alt+F menus honour preventDefault).
	function handleFormatPanelToggle(e) {
		if (!e.altKey || e.ctrlKey || e.metaKey || e.key.toLowerCase() !== "f") return false;
		e.preventDefault();
		setFmtOpen((o) => !o);
		return true;
	}

	// Plain Up/Down recall sent-message history — but only from the FIRST line
	// (Up) / LAST line (Down) with no selection, so arrows inside a multiline
	// draft keep their native line movement. A null from navigate also falls
	// through to the native caret move. Down while typing a fresh draft clears
	// the composer (and past the newest entry it clears too — Down at the
	// bottom always ends empty).
	function handleHistoryNav(e) {
		if (e.key !== "ArrowUp" && e.key !== "ArrowDown") return false;
		if (e.ctrlKey || e.metaKey || e.altKey || e.shiftKey) return false;
		const ta = e.currentTarget;
		if (ta.selectionStart !== ta.selectionEnd) return true;
		const dir = e.key === "ArrowDown" ? 1 : -1;
		const atEdge = dir < 0
			? isFirstLine(ta.value, ta.selectionStart)
			: isLastLine(ta.value, ta.selectionEnd);
		if (!atEdge) return true;
		const r = inputHistory.navigate(activeDraftKey, dir, ta.value);
		if (!r) return true;
		e.preventDefault();
		draftChanged(r.text);
		requestAnimationFrame(() => ta.setSelectionRange(r.text.length, r.text.length));
		return true;
	}

	// Tab completes commands/emoji/nicks and cycles on repeat; Shift+Tab
	// cycles backwards.
	function handleTabComplete(e) {
		if (e.key !== "Tab") return false;
		e.preventDefault();
		const ta = e.currentTarget;
		const r = completer.next(ta.value, ta.selectionStart, e.shiftKey ? -1 : 1, {
			nicks: completionNicks || [],
		});
		if (r) {
			draftChanged(r.text);
			requestAnimationFrame(() => ta.setSelectionRange(r.caret, r.caret));
		}
		return true;
	}

	// Enter sends; Shift+Enter inserts a newline (multiline).
	function handleComposerEnter(e) {
		if (e.key === "Enter" && !e.shiftKey) {
			e.preventDefault();
			submit(e);
		}
	}

	function composerKeyDown(e) {
		// Per UI Events, the keystroke that commits an IME conversion candidate
		// fires keydown with key=="Enter" and isComposing==true (keyCode 229 in
		// Chromium) BEFORE compositionend. Without this guard that Enter would
		// preventDefault the commit and send the half-composed draft. Mirrors
		// the type-anywhere handler's isComposing check above. This guard MUST
		// stay first.
		if (e.isComposing || e.keyCode === 229) return;
		if (handleFormatShortcut(e)) return;
		if (handleFormatPanelToggle(e)) return;
		if (handleHistoryNav(e)) return;
		if (handleTabComplete(e)) return;
		handleComposerEnter(e);
	}

	const header = msgs?.loaded
		? (msgs.reachedTop && list.length > 0 && <div class="top-note">beginning of history</div>)
		: <div class="top-note">loading…</div>;

	return (
		<>
			<VirtualList
				key={(buf?.key ?? "") + ":" + (tailNav || 0)}
				items={shown}
				estimate={estimate}
				header={header}
				focusId={focusId}
				layoutKey={layoutKey}
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
					<Row ev={ev} selfNick={selfNick} theme={theme} focused={ev.id === focusId} isHighlight={isHighlight} onRedact={onRedact} onNick={onNick} onToggle={toggleRun} nicks={nickHi} userhosts={userhosts} statusHost={statusHost} memberPrefixes={nickPrefixes ? memberPrefixes : null} timeFmt={timeFmt} nickSep={nickSep} previews={previews} />
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
				{buf?.buffer === SERVER_BUFFER ? (
					<div class="compose-box compose-readonly">
						Server and service notices appear here. This buffer is read-only.
					</div>
				) : (
				<form class="compose-box" onSubmit={submit}>
					<span class="prompt">{selfNick || "…"} ›</span>
					<textarea
						ref={taRef}
						class="compose-input"
						rows={draft.includes("\n") ? Math.min(draft.split("\n").length, 8) : 1}
						value={draft}
						onInput={(e) => draftChanged(e.currentTarget.value)}
						onKeyDown={composerKeyDown}
						placeholder={connected ? `Message ${buf?.buffer || ""}` : "disconnected — reconnecting…"}
						disabled={!connected}
					/>
					<FormatPanel
						open={fmtOpen}
						onToggle={() => setFmtOpen((o) => !o)}
						onClose={() => setFmtOpen(false)}
						onApply={applyFmt}
					/>
					<button class="btn-accent" type="submit" disabled={!connected}>Send</button>
				</form>
				)}
			</div>
		</>
	);
}
