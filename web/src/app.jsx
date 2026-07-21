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
import { Chat } from "./chat.jsx";
import { applyTombstones, bufferOrder, bufKey, clearTypingNick, expireTypingState, foldNick, historyHasMore, isChannelName, mergeById, mergeServerBuffers, parseHash, parseInput, rememberRedaction, renderable, SERVER_BUFFER, stripFormatting, toHash, updateTypingState } from "./irc.js";
import { applyBadge, highlightText, loadRules, Notifier, saveRules } from "./notify.js";
import { Login } from "./login.jsx";
import { applyPrefs, loadPrefs, MAX_PREFS_BYTES, normalizePrefs, prefsByteLength, resolveTheme, savePrefs } from "./prefs.js";
import { isIgnored, isMuted, loadIgnores, loadMutes, toggleIgnore, toggleMute } from "./local.js";
import { editableNetwork, networkEditError } from "./networkedit.js";
import { armPendingJoin, clearPendingJoin, notePendingJoinForward, takePendingJoin } from "./pending.js";
import { fetchAllMembers } from "./memberlist.js";
import { ContextMenu } from "./menu.jsx";
import { Members } from "./members.jsx";
import { ChannelPrompt, NetworkForm } from "./netform.jsx";
import { SearchOverlay } from "./search.jsx";
import { Settings, resetSettingsSession } from "./settings.jsx";
import { Sidebar } from "./sidebar.jsx";
import { Switcher } from "./switcher.jsx";
import { BufIcon } from "./icons.jsx";
import { Socket } from "./ws.js";

const PAGE = 100;
// Memory bound per buffer: trim to TRIM_TO once past TRIM_AT. Older pages
// are always refetchable, so trimming loses nothing durable. The list is
// virtualized, so these bound JS memory, not DOM size. Kept deliberately low
// — a hostile/high-traffic stream would otherwise let one buffer's event
// array grow to hundreds of MB, and each live append copies the array.
const TRIM_AT = 5000;
const TRIM_TO = 2500;
// Byte bound per buffer, enforced alongside the count bound: 5000 messages of
// the store's 16 KiB cap would be ~80 MB, so a hostile server sending max-size
// lines is capped here regardless of count. Real scrollback is a fraction of
// this. Tracked incrementally (msgs[key].bytes) so appends stay O(1).
const MAX_BUFFER_BYTES = 8 * 1024 * 1024;

// evBytes estimates one event's retained size. A whois card holds up to 8
// server-clamped fields (~2 KiB each, see hub.clampWhois) plus nick/numeric
// fields, so charge ~18 KiB rather than a flat 4 KiB that under-counts a
// hostile card ~4x and lets the byte budget be bypassed.
function evBytes(ev) {
	if (ev.whois) return 18 * 1024;
	return (ev.raw ? ev.raw.length : 0) + (ev.text ? ev.text.length : 0) + 48;
}
function listBytes(list) {
	let t = 0;
	for (const e of list) t += evBytes(e);
	return t;
}

// trimBuffer bounds a list by BOTH count and bytes, given a running byte total,
// and reports the new total. Drops from the front (oldest); reachedTop clears
// if anything was dropped, since older pages must then refetch.
function trimBuffer(list, bytes, reachedTop) {
	if (list.length > TRIM_AT) {
		const drop = list.length - TRIM_TO;
		for (let i = 0; i < drop; i++) bytes -= evBytes(list[i]);
		list = list.slice(drop);
		reachedTop = false;
	}
	if (bytes > MAX_BUFFER_BYTES) {
		let i = 0;
		while (i < list.length - 1 && bytes > MAX_BUFFER_BYTES) {
			bytes -= evBytes(list[i]);
			i++;
		}
		if (i > 0) {
			list = list.slice(i);
			reachedTop = false;
		}
	}
	return { list, bytes, reachedTop };
}
// Live message lists are kept only for the few most-recently-active
// buffers. A bouncer pushes events for every joined channel, so without
// this bound the in-memory lists grow one-capped-list-per-buffer across
// an entire session; an evicted buffer refetches its tail from the store
// when reopened. Unread/mention counts live in `buffers`, not here, so
// eviction never loses activity state.
const KEEP_BUFFERS = 8;

function wsURL() {
	const proto = location.protocol === "https:" ? "wss:" : "ws:";
	return `${proto}//${location.host}/api/ws`;
}

// topicFor: connection state trumps the topic while (re)connecting.
function topicFor(activeBuf, netState, chanInfo) {
	if (netState && netState !== "registered") return `${activeBuf?.network}: ${netState}…`;
	// The topic bar is plain text (no styled runs), so strip mIRC codes rather
	// than leak their digits into the title/label.
	return stripFormatting(chanInfo?.topic || "");
}

// TopBar: buffer name, topic, and the panel/search/theme buttons.
function TopBar({ activeBuf, isChan, topicText, sideOpen, rightOpen, theme, onSide, onRight, onSearch, onTheme }) {
	return (
		<div class="topbar">
			<button
				class={"icon-btn" + (sideOpen ? " active" : "")}
				title="Toggle channels"
				onClick={onSide}
			>◧</button>
			{activeBuf && (
				<span class="topic-name">
					<BufIcon chan={isChan} server={activeBuf.buffer === SERVER_BUFFER} />
					{activeBuf.buffer === SERVER_BUFFER ? activeBuf.network : activeBuf.buffer}
				</span>
			)}
			<div class="topic-sep" />
			<div class="topic-text" title={topicText}>{topicText}</div>
			<button class="icon-btn" title="Search (Ctrl+Shift+F)" onClick={onSearch}>⌕</button>
			<button class="icon-btn" title="Toggle theme" onClick={onTheme}>
				{theme === "dark" ? "☀" : "☾"}
			</button>
			{isChan && (
				<button
					class={"icon-btn" + (rightOpen ? " active" : "")}
					title="Toggle members"
					onClick={onRight}
				>◨</button>
			)}
		</div>
	);
}

// makeBuffer creates a client-only buffer (a just-opened query/DM, a whois
// card, or one implied by an incoming event before the server list catches
// up). ephemeral marks it as not-yet-server-persisted, so applyBuffers
// preserves it across a refresh but never resurrects a real server buffer
// the server has intentionally dropped. The flag clears once the buffer
// appears in a get_buffers response (rebuilt without it).
function makeBuffer(network, buffer) {
	return { key: bufKey(network, buffer), network, buffer, lastTime: 0, marker: 0, unread: 0, mention: false, ephemeral: true };
}

// Pure state transitions for the socket handlers, hoisted so the
// handlers stay shallow and each shape is testable in isolation.

// dropBufferMsgs forgets a buffer's cached pages (it refetches on view).
function dropBufferMsgs(m, key) {
	if (!m[key]) return m;
	const next = { ...m };
	delete next[key];
	return next;
}

// appendEventMsgs appends a live event, bounding per-buffer memory.
// `keep` is true when this buffer's live list should be retained (it is
// active or mid-load); for any other buffer we do not start (or grow) a
// list — the event is persisted server-side and loads from the store when
// the buffer is opened, which is what bounds cross-buffer memory.
function appendEventMsgs(m, key, ev, keep) {
	const cur = m[key];
	if (!cur && !keep) return m;
	// A buffer showing a search-jump window (not the live tail) must not
	// append — that would leave a temporal gap; the message is on disk
	// and appears when the user returns to the tail.
	if (cur?.loaded && cur.atTail === false) return m;
	// Accumulate even before history is loaded — the fetch response
	// merges and dedupes, so events racing an in-flight history request
	// are never lost.
	const list = [...(cur?.list || []), ev];
	const bytes = (cur?.bytes ?? listBytes(cur?.list || [])) + evBytes(ev);
	return { ...m, [key]: { ...cur, ...trimBuffer(list, bytes, cur?.reachedTop) } };
}

// Bounds for the per-network server buffer, which appendInfoLine can create
// and grow while it sits OUTSIDE the working-set eviction (that only runs on
// buffer switches / load completion). Much tighter than scrollback buffers:
// info lines are ephemeral (never persisted, nothing to refetch), so without
// this an idle client would hold up to 8 MiB of numerics flood per network.
const INFO_TRIM_AT = 1200, INFO_TRIM_TO = 1000, MAX_INFO_BYTES = 1024 * 1024;

// appendInfoLine appends an ephemeral server_info / whois line. It creates the
// buffer (MOTD/whois must show even before it's viewed), but the count+byte
// bound caps its growth, and a recreated-after-eviction buffer is reclaimed on
// the next buffer switch (the eviction effect), so a server_info flood can't
// grow it or the buffer set without limit. `tight` selects the server-buffer
// bounds above (a whois card lands in a real query buffer, which keeps the
// normal scrollback bounds).
function appendInfoLine(m, key, ev, tight) {
	const cur = m[key];
	if (cur?.loaded && cur.atTail === false) return m;
	let list = [...(cur?.list || []), ev];
	let bytes = (cur?.bytes ?? listBytes(cur?.list || [])) + evBytes(ev);
	let { list: l, bytes: b, reachedTop } = trimBuffer(list, bytes, cur?.reachedTop);
	list = l; bytes = b;
	if (tight) {
		if (list.length > INFO_TRIM_AT) {
			const drop = list.length - INFO_TRIM_TO;
			for (let i = 0; i < drop; i++) bytes -= evBytes(list[i]);
			list = list.slice(drop);
			reachedTop = false;
		}
		let i = 0;
		while (i < list.length - 1 && bytes > MAX_INFO_BYTES) {
			bytes -= evBytes(list[i]);
			i++;
		}
		if (i > 0) {
			list = list.slice(i);
			reachedTop = false;
		}
	}
	return { ...m, [key]: { ...cur, list, bytes, reachedTop } };
}

// bumpBufferActivity advances a buffer's last-activity/unread/mention
// state for a live event (creating the buffer on first traffic).
function bumpBufferActivity(b, key, ev, countUnread, highlight) {
	const cur = b[key] || makeBuffer(ev.network, ev.buffer);
	// Only count/alert on messages NEWER than the read marker, mirroring the
	// server's strict `ts > marker`: an event at/below the marker (same-ms on a
	// busy channel, or a backdated relay/bridge line) is already-read. Gate BOTH
	// unread and mention on it, and never regress lastTime — a backdated event
	// must not lower it (which would let markRead clear genuinely-newer unread).
	const newer = ev.time > cur.marker;
	return {
		...b,
		[key]: {
			...cur,
			lastTime: Math.max(cur.lastTime, ev.time),
			unread: countUnread && newer ? cur.unread + 1 : cur.unread,
			mention: cur.mention || (highlight && newer),
		},
	};
}

// applyMarkerState moves a buffer's read marker, clearing unread/mention
// when it reaches the newest message.
function applyMarkerState(b, key, time) {
	const cur = b[key];
	if (!cur) return b;
	const cleared = time >= cur.lastTime;
	return {
		...b,
		[key]: {
			...cur, marker: time,
			unread: cleared ? 0 : cur.unread,
			mention: cleared ? false : cur.mention,
		},
	};
}

// applyPresenceUpdate flips one monitored nick's online state.
function applyPresenceUpdate(all, d) {
	const list = all[d.network] || [];
	const idx = list.findIndex((m) => m.nick === d.nick);
	if (idx === -1) return all; // not (or no longer) in the list
	const next = list.slice();
	next[idx] = { ...next[idx], online: d.online };
	return { ...all, [d.network]: next };
}

// applyRedaction tombstones the message with the redacted msgid.
function applyRedaction(m, key, d) {
	const cur = m[key];
	if (!cur?.list) return m;
	let hit = false;
	const list = cur.list.map((ev) => {
		if (ev.msgid !== d.msgid || ev.redacted) return ev;
		hit = true;
		// Drop the raw content too, mirroring the server's destructive scrub —
		// the tombstone renders from the redacted flag alone, and keeping raw
		// would leave the deleted text in this client's memory.
		return { ...ev, redacted: true, redact_reason: d.reason, raw: "" };
	});
	// Recompute the byte total: dropping raw shrinks the event, and a stale
	// cur.bytes would over-count and trim the buffer earlier than the budget
	// warrants. Redaction is rare, so the O(n) recompute is cheap.
	return hit ? { ...m, [key]: { ...cur, list, bytes: listBytes(list) } } : m;
}

// mergeHistoryPage installs a fetched page, keeping events that streamed
// in while the fetch was in flight (the page is authoritative for what
// it covers).
function mergeHistoryPage(m, key, page, tombstones) {
	const cur = m[key];
	// An installed jump/search window is a non-tail slice (atTail === false).
	// An initial tail load must not merge into it: that would splice the
	// live tail onto an older window (a temporal gap) and wrongly flip
	// atTail true. The window owns the buffer until the user returns to the
	// tail, which reloads it. (jumpTo also bumps historyGen to discard the
	// in-flight load early; this guards the re-dispatch race belt-and-braces.)
	if (cur?.loaded && cur.atTail === false) return m;
	const pageMsgs = page.messages || [];
	const merged = applyTombstones(mergeById(cur?.list || [], pageMsgs), tombstones);
	// Enforce the byte/count budget here too: prev.list (≤ budget) plus a fresh
	// page can exceed MAX_BUFFER_BYTES, and although a live append re-trims, a
	// quiet buffer would sit oversized until then. This is a tail load, so
	// front-trimming (oldest) is correct; reachedTop clears if anything dropped.
	const t = trimBuffer(merged, listBytes(merged), !historyHasMore(page, PAGE));
	return {
		...m,
		[key]: {
			list: t.list,
			bytes: t.bytes,
			loaded: true,
			reachedTop: t.reachedTop,
			atTail: true,
		},
	};
}

export function App() {
	const [phase, setPhase] = useState("checking"); // checking | login | app
	// Fail closed: previews start OFF until /api/config confirms they are on,
	// so a slow or failed config load never issues a /api/preview or /api/thumb
	// request (which would put the full target URL in the reverse-proxy access
	// log even while previews are disabled). The render gate requires === true.
	const [previews, setPreviews] = useState(false); // link/media previews enabled (server switch)
	const previewsPinned = useRef(false); // user toggled it: the initial GET must not clobber that
	const [connected, setConnected] = useState(false);
	const [networks, setNetworks] = useState({});
	const [buffers, setBuffers] = useState({});
	const [buffersTruncated, setBuffersTruncated] = useState(false);
	// Network management: the add/edit form ({ initial, oldName } | null)
	// and the join-channel prompt ({ network } | null), with the last
	// server error for each.
	const [netForm, setNetForm] = useState(null);
	const [netFormError, setNetFormError] = useState("");
	const [netFormBusy, setNetFormBusy] = useState(false);
	const netFormBusyRef = useRef(false);
	const [chanPrompt, setChanPrompt] = useState(null);
	const [chanPromptError, setChanPromptError] = useState("");
	const [chanPromptBusy, setChanPromptBusy] = useState(false);
	const chanPromptBusyRef = useRef(false);
	// Every async modal/prefill operation captures an intent id and Socket
	// object. Closing/reopening a modal, superseding an action, or crossing an
	// auth/socket boundary makes its late callbacks inert.
	const netFormIntent = useRef(0);
	const chanPromptIntent = useRef(0);
	const topicIntent = useRef(0);
	const [msgs, setMsgs] = useState({});
	const [activeKey, setActiveKey] = useState(() => {
		const h = parseHash(location.hash);
		return h ? bufKey(h.network, h.buffer) : null;
	});
	// Explicit navigation and async actions that intend to navigate share one
	// generation. A reply/JOIN may move focus only while its generation is still
	// current, so a later click or command always wins over a delayed callback.
	const navigationIntent = useRef(0);
	const [prefs, setPrefs] = useState(loadPrefs);
	const [prefsError, setPrefsError] = useState("");
	// Last blob the server actually confirmed. Permanent set_prefs rejection
	// rolls the UI back here instead of leaving it showing unsaved state.
	const prefsConfirmed = useRef(prefs);
	const [sysDark, setSysDark] = useState(
		() => globalThis.matchMedia("(prefers-color-scheme: dark)").matches,
	);
	const theme = resolveTheme(prefs.theme, sysDark);
	const [sideOpen, setSideOpen] = useState(() => globalThis.innerWidth >= 760);
	const [rightOpen, setRightOpen] = useState(() => globalThis.innerWidth >= 1000);
	const [chanInfo, setChanInfo] = useState(null);
	const [chanTick, setChanTick] = useState(0);
	const [cmdError, setCmdError] = useState("");
	const [searchOpen, setSearchOpen] = useState(false);
	const [switcherOpen, setSwitcherOpen] = useState(false);
	const [settingsOpen, setSettingsOpen] = useState(false);
	const [focusId, setFocusId] = useState(null);
	// monitors: network -> [{nick, online}]; the MONITOR buddy list.
	const [monitors, setMonitors] = useState({});
	const [rules, setRules] = useState(loadRules);
	const rulesRef = useRef(rules);
	rulesRef.current = rules;
	// Client-side ignore (per network) and mute (per buffer) lists.
	const [ignores, setIgnores] = useState(loadIgnores);
	const ignoresRef = useRef(ignores);
	ignoresRef.current = ignores;
	const [mutes, setMutes] = useState(loadMutes);
	const mutesRef = useRef(mutes);
	mutesRef.current = mutes;
	// Open context menu, or null. { x, y, title, items }.
	const [menu, setMenu] = useState(null);
	// Imperative composer handle (prefill for "edit topic").
	const composerApi = useRef(null);
	// Persistent redaction tombstones: Map<bufKey, Map<msgid, reason>>. Kept in
	// a ref (not state) so socket handlers read the live set without a stale
	// closure; survives buffer eviction so a late history/search page for a
	// closed-then-reopened buffer still can't restore redacted content.
	const redactedIds = useRef(new Map());
	// Nicks we ran /whois on, keyed network+"\n"+lowercased-nick, so ONLY a
	// client-initiated whois opens and selects a query buffer. An unsolicited
	// server-pushed 311/318 must not spawn a buffer per nick or steal focus.
	const pendingWhois = useRef(new Set());
	const notifier = useRef();
	if (!notifier.current) notifier.current = new Notifier();
	// typers: Map<bufKey, Map<nick, {state, at}>>; ephemeral, capped and
	// prototype-safe because every name originates at an IRC server.
	const [typers, setTypers] = useState(() => new Map());
	const sock = useRef(null);
	const socketGen = useRef(0);
	// Buffers with a get_history request in flight — at most one per
	// buffer, so a live event arriving mid-load cannot fire a second,
	// window-slid request that corrupts ordering.
	const loadingHistory = useRef(new Set());
	// Buffers whose initial history load errored/timed out. Without this the
	// load effect would immediately refire (loadTick bumps, `loaded` still
	// false) and hammer the server at WS-RTT rate. Cleared when the buffer is
	// (re)visited, on reconnect, and on an explicit tail reload — so a retry
	// is user- or reconnect-driven, not a tight loop.
	const failedHistory = useRef(new Set());
	// Per-buffer load generation: a history_changed (backfill) bumps it,
	// so an in-flight get_history whose generation is stale on resolve is
	// discarded and refetched instead of installing pre-backfill data.
	const historyGen = useRef({});
	// Bumped when a load settles, to re-run the load effect (a ref clear
	// alone would not).
	const [loadTick, setLoadTick] = useState(0);

	// The server is the source of truth for prefs; localStorage is a
	// write-through cache so the first paint has the right theme. This
	// effect applies + caches; pushing to the server happens only in
	// updatePrefs (explicit local changes), never when adopting a server
	// value — that would echo forever between devices.
	useEffect(() => {
		applyPrefs(prefs, theme);
		savePrefs(prefs);
	}, [prefs, theme]);

	const prefsPush = useRef(null);
	// True while a local pref change has not been confirmed-synced to the
	// server (e.g. changed while the socket was down). Used on reconnect
	// to re-push rather than adopt the server's stale copy.
	const prefsDirty = useRef(false);
	function persistPrefs(next, attempt = 0) {
		if (prefsRef.current !== next) return;
		const s = sock.current;
		if (!s) {
			setPrefsError("Preferences are waiting to sync after reconnect.");
			return;
		}
		s.request("set_prefs", { prefs: next })
			.then(() => {
				if (sock.current !== s) return;
				prefsConfirmed.current = next;
				if (prefsRef.current === next) {
					prefsDirty.current = false;
					setPrefsError("");
				}
			})
			.catch((e) => {
				if (sock.current !== s || prefsRef.current !== next) return;
				if (e?.code === "bad_request") {
					prefsDirty.current = false;
					setPrefs(prefsConfirmed.current);
					setPrefsError(e.message || "Preferences were rejected and rolled back.");
					return;
				}
				// A disconnect/store failure is transient. Retry a few times while
				// this remains the newest edit; if the socket stays down, the dirty
				// value is retried by adoptPrefs on reconnect.
				setPrefsError("Preferences are waiting to sync after reconnect.");
				if (attempt < 3) {
					prefsPush.current = setTimeout(() => persistPrefs(next, attempt + 1), 500 * 2 ** attempt);
				}
			});
	}

	function updatePrefs(rawNext) {
		const next = normalizePrefs(rawNext);
		setPrefs(next);
		prefsDirty.current = true;
		setPrefsError(
			prefsByteLength(rawNext) > MAX_PREFS_BYTES
				? "Preferences are limited to 64 KiB; custom CSS was truncated."
				: "",
		);
		// Debounced: the custom-CSS textarea changes on every keystroke.
		clearTimeout(prefsPush.current);
		prefsPush.current = setTimeout(() => {
			persistPrefs(next);
		}, 400);
	}

	// Track the OS theme so the "system" preference follows it live.
	useEffect(() => {
		const mq = globalThis.matchMedia("(prefers-color-scheme: dark)");
		const onChange = (e) => setSysDark(e.matches);
		mq.addEventListener("change", onChange);
		return () => mq.removeEventListener("change", onChange);
	}, []);

	// Auth probe: a plain GET of the ws endpoint answers 401 when the
	// session cookie is missing/expired, anything else means authed.
	useEffect(() => {
		fetch("/api/ws")
			.then((r) => setPhase(r.status === 401 ? "login" : "app"))
			.catch(() => setPhase("login"));
	}, []);

	// Leaving the authenticated phase resets the preview switches: they are
	// server-session state, and letting them survive logout would (a) keep
	// rendering preview components that fire requests the backend now refuses,
	// and (b) leave previewsPinned latched so the config GET after RE-login is
	// ignored permanently — the client would never re-adopt the authoritative
	// value. Reset both; the next login re-fetches /api/config fail-closed.
	useEffect(() => {
		if (phase !== "login") return;
		previewsPinned.current = false;
		setPreviews(false);
		// Invalidate the module-level settings save queue too: a mutation
		// queued before signing out must not execute against the next
		// session's cookie, and a delayed previews callback must not re-pin
		// the state just reset above.
		resetSettingsSession();
		clearTimeout(prefsPush.current);
		setBuffersTruncated(false);
		setNetForm(null);
		netFormBusyRef.current = false;
		setNetFormBusy(false);
		setChanPrompt(null);
		chanPromptBusyRef.current = false;
		setChanPromptBusy(false);
		setSettingsOpen(false);
		setSearchOpen(false);
		setSwitcherOpen(false);
		setMenu(null);
	}, [phase]);

	// Server switches (whether link/media previews are enabled). Fetched
	// once authed so the UI never requests previews the server disabled.
	useEffect(() => {
		if (phase !== "app") return undefined;
		let ignore = false;
		fetch("/api/config")
			.then((r) => (r.ok ? r.json() : null))
			.then((c) => {
				// Drop the response if this effect was torn down, or the user
				// already toggled previews while the GET was in flight — the
				// user's value (and its PUT) is newer than this stale read.
				if (ignore || previewsPinned.current) return;
				if (c && typeof c.previews === "boolean") setPreviews(c.previews);
			})
			.catch(() => {});
		return () => { ignore = true; };
	}, [phase]);

	useEffect(() => {
		const onHash = () => {
			const h = parseHash(location.hash);
			if (h) {
				navigationIntent.current++;
				setActiveKey(bufKey(h.network, h.buffer));
			}
		};
		globalThis.addEventListener("hashchange", onHash);
		return () => globalThis.removeEventListener("hashchange", onHash);
	}, []);

	// Global shortcuts: Ctrl/Cmd+K channel switcher, Ctrl/Cmd+Shift+F
	// search, Alt+↑/↓ previous/next buffer, Alt+Shift+↑/↓ previous/next
	// unread buffer. Handlers read refs so this registers once.
	const stepRef = useRef(null);
	useEffect(() => {
		const onKey = (e) => {
			const mod = e.ctrlKey || e.metaKey;
			if (mod && !e.shiftKey && e.key.toLowerCase() === "k") {
				e.preventDefault();
				setSwitcherOpen(true);
			} else if (mod && e.shiftKey && e.key.toLowerCase() === "f") {
				e.preventDefault();
				setSearchOpen(true);
			} else if (e.altKey && (e.key === "ArrowUp" || e.key === "ArrowDown")) {
				e.preventDefault();
				stepRef.current?.(e.key === "ArrowDown" ? 1 : -1, e.shiftKey);
			}
		};
		globalThis.addEventListener("keydown", onKey);
		return () => globalThis.removeEventListener("keydown", onKey);
	}, []);

	// Socket helpers, at component scope so handler nesting stays shallow.
	// Async responses are accepted only from the socket that issued them.

	// loadMonitors fetches one network's MONITOR buddy list with presence.
	function loadMonitors(name) {
		const s = sock.current;
		if (!s) return;
		s.request("get_monitors", { network: name })
			.then((md) => {
				if (sock.current !== s) return;
				setMonitors((all) => ({ ...all, [name]: md.monitors || [] }));
			})
			.catch(() => {});
	}

	// applyBuffers installs a get_buffers response: network states and
	// the sidebar buffer list (the mention flag is client-side; keep it).
	function applyBuffers(data) {
		const truncated = data?.truncated === true;
		setBuffersTruncated(truncated);
		// An incomplete network snapshot cannot authoritatively delete names that
		// were outside the response cap. Preserve them until network_removed.
		const nets = truncated ? { ...networksRef.current } : {};
		const responseNetworks = data.networks || [];
		for (const n of responseNetworks) {
			nets[n.name] = { state: n.state, nick: n.nick, chantypes: n.chantypes || "#&" };
		}
		setNetworks(nets);
		// A truncated snapshot preserves omitted old network rows, but refetching
		// every preserved name turns one bounded response into an unbounded burst.
		// Refresh only the networks the server actually included this time.
		for (const { name } of responseNetworks) loadMonitors(name);

		// A complete snapshot drops server buffers absent from the authoritative
		// list. A safety-truncated snapshot cannot distinguish omitted from closed,
		// so retain known rows; live buffer_closed events still remove them.
		const bufs = mergeServerBuffers(data.buffers, buffersRef.current, nets, truncated);
		setBuffers(bufs);

		// If the active buffer was one the server dropped, don't leave the
		// view pointing at it — fall back to another buffer (or none).
		const active = activeKeyRef.current;
		if (active && !bufs[active]) {
			const rest = Object.values(bufs).sort((a, b) =>
				(a.network + a.buffer).localeCompare(b.network + b.buffer));
			if (rest.length) select(rest[0].network, rest[0].buffer);
			else {
				setActiveKey(null);
				location.hash = "";
			}
		}
	}

	function refreshBuffers() {
		const s = sock.current;
		if (!s) return;
		s.request("get_buffers", null)
			.then((data) => {
				if (sock.current === s) applyBuffers(data);
			})
			.catch(() => {});
	}

	// adoptPrefs applies the server's prefs; a server with none stored
	// yet (fresh install, pre-sync upgrade) is seeded from this
	// browser's cache.
	function adoptPrefs(d) {
		// A change made while disconnected never reached the server, so
		// its get_prefs is stale — re-push the local prefs instead of
		// reverting to it. Only clear dirty if no newer edit landed while the
		// re-push was in flight (same guard as updatePrefs); otherwise a queued
		// newer edit would look synced and the next reconnect would adopt the
		// server's now-stale copy over it.
		if (prefsDirty.current) {
			// Preserve the actual rollback point before trying the unsynced local
			// edit. This matters on the first connection, where the localStorage
			// value has never itself been server-confirmed.
			if (d.prefs) prefsConfirmed.current = normalizePrefs(d.prefs);
			const pushed = prefsRef.current;
			persistPrefs(pushed);
			return;
		}
		if (d.prefs) {
			const adopted = normalizePrefs(d.prefs);
			prefsConfirmed.current = adopted;
			setPrefs(adopted);
			setPrefsError("");
		} else {
			const seeded = prefsRef.current;
			prefsDirty.current = true;
			persistPrefs(seeded);
		}
	}

	// Socket lifecycle, once authed.
	useEffect(() => {
		if (phase !== "app") return;
		const s = new Socket(wsURL());
		const gen = ++socketGen.current;
		sock.current = s;
		setConnected(false);
		const live = () => sock.current === s && socketGen.current === gen;
		// Every push handler is generation-guarded in one place. Socket.close also
		// suppresses queued frames, but this protects callbacks already dispatched
		// into the microtask queue before teardown.
		const on = (type, fn) => s.on(type, (data) => {
			if (live()) fn(data);
		});

		const wsFailures = { n: 0 };
		// The failure counter resets on _stable, not _open: a backend that
		// accepts the WS handshake and immediately dies would otherwise reset
		// it every cycle and the 3-close auth re-probe below could never fire.
		on("_stable", () => {
			wsFailures.n = 0;
		});
		on("_open", async () => {
			failedHistory.current.clear(); // fresh connection: let loads retry
			setConnected(true);
			s.request("get_prefs", null)
				.then((d) => { if (live()) adoptPrefs(d); })
				.catch(() => {});
			// Drop cached pages up front so every open buffer refetches a
			// fresh tail covering the offline window. This must NOT hang off
			// get_buffers succeeding: if that request rejects (error envelope
			// or timeout) while the socket stays up, a stale active buffer
			// would keep loaded:true, never refetch, and silently gap when
			// live events resume appending to it.
			setMsgs({});
			try {
				const data = await s.request("get_buffers", null);
				if (live()) applyBuffers(data);
			} catch {
				/* sidebar refresh will retry; scrollback already reset above */
			}
		});
		on("_close", () => {
			setConnected(false);
			// The WebSocket API can't distinguish a 401 handshake rejection
			// from a network outage, and Socket retries forever. After a few
			// closes with no successful open, re-probe auth: if the session is
			// gone (server restart, TTL expiry, logout/eviction elsewhere),
			// return to login instead of looping with a dead cookie. A real
			// outage makes the probe itself fail, so we keep retrying.
			if (++wsFailures.n >= 3) {
				fetch("/api/ws")
					.then((r) => {
						if (live() && r.status === 401) {
							wsFailures.n = 0;
							setPhase("login");
						}
					})
					.catch(() => {});
			}
		});

		// Chathistory backfill rewrote a buffer's history: drop cached
		// pages (the active buffer refetches automatically) and refresh
		// sidebar counts, debounced across a burst of backfills.
		let bufRefresh;
		on("history_changed", (d) => {
			const key = bufKey(d.network, d.buffer);
			historyGen.current[key] = (historyGen.current[key] || 0) + 1;
			// This buffer is being told to refetch — a failed-load marker from
			// before must not veto it, or an active buffer whose initial load
			// errored stays blank until revisited/reconnected. The loadTick bump
			// re-runs the load effect even when dropBufferMsgs was a no-op
			// (nothing cached => no state change => no re-render).
			failedHistory.current.delete(key);
			setMsgs((m) => dropBufferMsgs(m, key));
			setLoadTick((t) => t + 1);
			clearTimeout(bufRefresh);
			bufRefresh = setTimeout(refreshBuffers, 300);
		});

		on("state", (d) => {
			setNetworks((n) => ({ ...n, [d.network]: { ...n[d.network], state: d.state } }));
			// A (re)registered network's ISUPPORT (chantypes, nick) lands
			// just after 001; refresh the buffer list once it settles.
			if (d.state === "registered") {
				clearTimeout(bufRefresh);
				bufRefresh = setTimeout(refreshBuffers, 1500);
			}
		});

		on("event", (ev) => {
			const key = bufKey(ev.network, ev.buffer);
			const keep = key === activeKeyRef.current || loadingHistory.current.has(key);
			setMsgs((m) => appendEventMsgs(m, key, ev, keep));
			// A message from someone clears their typing indicator.
			setTypers((t) => clearTypingNick(t, key, ev.sender));
			const r = renderable(ev);
			const isMsg = r.kind !== "system";
			const nick = networksRef.current[ev.network]?.nick;
			const mine = nick && ev.sender === nick;
			// A UI-initiated join selects the channel we actually ended up in,
			// target-correlated so an asynchronously rejected earlier JOIN cannot
			// consume this event. A 470 adds the forwarded name as an alias below.
			if (mine && ev.command === "JOIN") {
				const token = takePendingJoin(pendingJoins.current, ev.network, ev.buffer);
				if (token) selectForNavigation(token.intent, ev.network, ev.buffer);
			}
			// Highlight = a mention/keyword in a channel, or any message in
			// a query (PM) buffer. PMs always alert. The synthetic server
			// buffer ("*", where service/server NOTICEs land) is treated like a
			// channel, NOT a PM — otherwise every NickServ/"*** Notice" line
			// would alert (favicon red + a desktop-notification storm on a
			// connect burst); only a real nick/keyword match in it alerts.
			const isChan = isChannelName(ev.buffer, networksRef.current[ev.network]?.chantypes);
			const chanLike = isChan || ev.buffer === SERVER_BUFFER;
			const highlight = isMsg && !mine &&
				(chanLike ? highlightText(r.text, nick, rulesRef.current, ev.network) : true);
			// Ignored senders never alert (and are hidden at render); muted
			// buffers never alert either.
			const ignored = isIgnored(ignoresRef.current, ev.network, ev.sender);
			const muted = isMuted(mutesRef.current, key);
			const alert = highlight && !ignored && !muted;

			// Count EVERY message (incl. own and ignored) toward unread so the
			// badge matches the server's authoritative, nick-agnostic count
			// (command IN PRIVMSG/NOTICE > marker) — otherwise a get_buffers on
			// reconnect overwrites the badge with a higher number that "jumps".
			// In the normal case markRead advances the marker past your own send
			// (you're pinned+focused), so this shows no badge; it only shows one
			// for a send made while scrolled up, which then clears on read —
			// consistent, not a surprise jump. `alert` still excludes own so you
			// never notify yourself.
			setBuffers((b) => bumpBufferActivity(b, key, ev, isMsg, alert));

			// Desktop notification when an alert lands somewhere the user
			// isn't looking (tab hidden, or a different buffer active).
			if (alert && (document.hidden || key !== activeKeyRef.current)) {
				const where = isChan ? `${ev.sender} in ${ev.buffer}` : ev.sender;
				// The Notification API renders plain text, so strip mIRC codes.
				notifier.current.show(where, stripFormatting(r.text), key, () => {
					select(ev.network, ev.buffer);
				});
			}
		});

		// Another device closed a buffer; drop it here too.
		on("buffer_closed", (d) => {
			forgetBuffer(d.network, d.buffer);
		});

		// A network was deleted or renamed away: drop its buffers and
		// state. New/renamed networks introduce themselves via "state"
		// pushes and the buffer refresh.
		on("network_removed", (d) => {
			setNetworks((n) => {
				const next = { ...n };
				delete next[d.network];
				return next;
			});
			const gone = Object.values(buffersRef.current).filter((b) => b.network === d.network);
			setBuffers((b) => {
				const next = { ...b };
				for (const g of gone) delete next[g.key];
				return next;
			});
			// Invalidate any in-flight initial loads for the removed buffers.
			for (const g of gone) {
				historyGen.current[g.key] = (historyGen.current[g.key] || 0) + 1;
				redactedIds.current.delete(g.key); // drop tombstones for gone buffers
			}
			setMsgs((m) => {
				let next = m;
				for (const g of gone) next = dropBufferMsgs(next, g.key);
				return next;
			});
			setTypers((all) => {
				let next = all;
				for (const g of gone) {
					if (!next.has(g.key)) continue;
					if (next === all) next = new Map(all);
					next.delete(g.key);
				}
				return next;
			});
			if (gone.some((g) => g.key === activeKeyRef.current)) {
				setActiveKey(null);
				location.hash = "";
			}
		});

		// Another device changed prefs; adopt without echoing back — but not
		// over an unsynced local edit still in the debounce window, or its
		// pending set_prefs would clobber this device's change server-side
		// (mirrors adoptPrefs's dirty guard).
		on("prefs", (d) => {
			if (prefsDirty.current) return;
			if (d.prefs) {
				const adopted = normalizePrefs(d.prefs);
				prefsConfirmed.current = adopted;
				setPrefs(adopted);
				setPrefsError("");
			}
		});

		// Ephemeral server replies (/list, error numerics): shown as
		// system lines in the active buffer, never persisted — a history
		// refetch drops them.
		let infoSeq = 0;
		on("server_info", (d) => {
			// ERR_LINKCHANNEL (470) is formatted as "<original> <target> ...".
			// Correlate only when BOTH leading words are channel names and the
			// original matches an outstanding token; other error numerics remain
			// ordinary informational text.
			const words = String(d.text || "").trim().split(/\s+/);
			const chantypes = networksRef.current[d.network]?.chantypes;
			if (isChannelName(words[0], chantypes) && isChannelName(words[1], chantypes)) {
				notePendingJoinForward(pendingJoins.current, d.network, words[0], words[1]);
			}
			// Server info (MOTD, connect numerics) lands in the network's own
			// server buffer, not whatever buffer happens to be active.
			const key = bufKey(d.network, SERVER_BUFFER);
			setBuffers((b) => (b[key] ? b : { ...b, [key]: makeBuffer(d.network, SERVER_BUFFER) }));
			const ev = {
				// Zero-pad the sequence: a MOTD/connect burst shares one
				// Date.now() ms, so mergeById breaks the tie by STRING id when
				// the server buffer's history loads — unpadded, "si10" sorts
				// before "si2", scrambling the lines.
				id: `si${String(++infoSeq).padStart(9, "0")}`, network: d.network, buffer: SERVER_BUFFER,
				// local: browser-clock time, display/ordering only — a flagged
				// event must never advance the read marker (chat.jsx readTS),
				// or clock skew would set a future marker that hides unread
				// badges for real messages on every device.
				time: Date.now(), sender: "", command: "INFO", raw: d.text, local: true,
			};
			setMsgs((m) => appendInfoLine(m, key, ev, true));
		});

		// A WHOIS card lands in the target's query buffer; jump there, so
		// /whois does not clutter the channel (The Lounge style).
		let whoisSeq = 0;
		on("whois", (d) => {
			// Only a whois WE asked for opens/selects a buffer. Drop an
			// unsolicited server-pushed card so a hostile server can't spawn a
			// buffer per nick or repeatedly steal focus (browser-memory + UX DoS).
			if (!pendingWhois.current.delete(d.network + "\n" + foldNick(d.nick))) return;
			const key = bufKey(d.network, d.nick);
			setBuffers((b) => (b[key] ? b : { ...b, [key]: makeBuffer(d.network, d.nick) }));
			const ev = {
				// Zero-padded like the server_info ids so same-ms cards keep
				// insertion order through mergeById's string tie-break.
				id: `wh${String(++whoisSeq).padStart(9, "0")}`, network: d.network, buffer: d.nick,
				// local: browser-clock stamp, excluded from read-marker
				// computation (see the server_info handler above).
				time: Date.now(), sender: "", command: "WHOIS", raw: "", whois: d, local: true,
			};
			setMsgs((m) => appendInfoLine(m, key, ev));
			if (activeKeyRef.current !== key) select(d.network, d.nick);
		});

		on("presence", (d) => setMonitors((all) => applyPresenceUpdate(all, d)));
		// Another tab/device changed the buddy list: refetch the authoritative
		// list instead of drifting on this tab's local state.
		on("monitors_changed", (d) => d?.network && loadMonitors(d.network));

		on("redact", (d) => {
			const key = bufKey(d.network, d.buffer);
			rememberRedaction(redactedIds.current, key, d.msgid, d.reason);
			setMsgs((m) => applyRedaction(m, key, d));
		});

		on("typing", (d) => {
			const key = bufKey(d.network, d.buffer);
			setTypers((t) => updateTypingState(t, key, d, !!buffersRef.current[key]));
		});

		on("members_changed", (d) => {
			const buf = buffersRef.current[activeKeyRef.current];
			if (
				buf && d.network === buf.network &&
				(!d.buffer || d.buffer.toLowerCase() === buf.buffer.toLowerCase())
			) {
				setChanTick((t) => t + 1);
			}
		});

		on("read_marker", (d) => {
			const key = bufKey(d.network, d.buffer);
			const cur = buffersRef.current[key];
			setBuffers((b) => applyMarkerState(b, key, d.time));
			// A marker pushed by ANOTHER device (our own reads take the direct
			// path below) that moves forward but not to the tail leaves our
			// running unread higher than the server's ts>marker count — resync
			// (debounced) so the badge matches. Our web reads always send the
			// full tail ts (applyMarkerState clears), so this only fires for an
			// external client that read partway via draft/read-marker.
			if (cur && d.time > cur.marker && d.time < cur.lastTime) {
				clearTimeout(bufRefresh);
				bufRefresh = setTimeout(refreshBuffers, 300);
			}
		});

		s.connect();
		return () => {
			clearTimeout(bufRefresh);
			if (live()) {
				socketGen.current++;
				sock.current = null;
				setConnected(false);
			}
			pendingJoins.current.clear();
			// Intent bumps strand any in-flight request's .finally busy-ref
			// reset (intent no longer matches), so reset the refs here too —
			// same discipline as every other bump site.
			netFormIntent.current++;
			netFormBusyRef.current = false;
			chanPromptIntent.current++;
			chanPromptBusyRef.current = false;
			topicIntent.current++;
			s.close();
		};
	}, [phase]);

	// Refs mirror state so long-lived socket handlers read current values
	// without re-registering on every change.
	const prefsRef = useRef(prefs);
	prefsRef.current = prefs;
	const networksRef = useRef(networks);
	networksRef.current = networks;
	const buffersRef = useRef(buffers);
	buffersRef.current = buffers;
	const activeKeyRef = useRef(activeKey);
	activeKeyRef.current = activeKey;

	// Favicon + tab title reflect total unread, red when any is a
	// highlight. The title's unread-count and active-channel parts are
	// pref-gated (see applyBadge); recompute when the active buffer changes
	// so the channel name in the title tracks selection.
	useEffect(() => {
		let unread = 0;
		let mention = false;
		for (const b of Object.values(buffers)) {
			unread += b.unread || 0;
			if (b.mention) mention = true;
		}
		const ab = activeKey ? buffers[activeKey] : null;
		const channel = prefs.titleChannel && ab
			? (ab.buffer === SERVER_BUFFER ? ab.network : ab.buffer)
			: "";
		applyBadge(unread, mention, { showCount: prefs.titleUnread, channel });
	}, [buffers, theme, prefs, activeKey]);

	function updateRules(next) {
		setRules(next);
		saveRules(next);
	}

	// Expire stale typing states (6s active / 30s paused per spec).
	useEffect(() => {
		const t = setInterval(() => {
			setTypers((all) => expireTypingState(all));
		}, 1000);
		return () => clearInterval(t);
	}, []);

	// Clear the previous channel's roster/topic the instant the active
	// buffer changes, so the members panel, topic bar, and op/kick/ban
	// menu never render the old channel's data during the debounced
	// get_channel round-trip below. Keyed on activeKey only (not
	// chanTick), so live member updates don't flicker the panel.
	useEffect(() => {
		setChanInfo(null);
	}, [activeKey]);

	// Channel state (topic + members) for the active buffer. Debounced:
	// members_changed hints arrive in bursts (NAMES floods, netsplits).
	// Big channels page in via the get_channel `after` cursor:
	// fetchAllMembers keeps requesting while the server reports more,
	// pushing each accumulated page into chanInfo so the panel fills in
	// live. It stops on its own at the roster cap or a non-advancing
	// cursor (memberlist.js), and isStale abandons the walk when the
	// active buffer or socket changes mid-page — the same staleness
	// conditions the single-shot fetch guarded.
	useEffect(() => {
		const buf = activeKey ? buffers[activeKey] : null;
		if (!buf || !connected || !isChannelName(buf.buffer, networks[buf.network]?.chantypes)) {
			setChanInfo(null);
			return;
		}
		const s = sock.current;
		if (!s) {
			setChanInfo(null);
			return;
		}
		const key = buf.key;
		let alive = true;
		const t = setTimeout(() => {
			fetchAllMembers({
				request: (after) =>
					s.request("get_channel", {
						network: buf.network,
						buffer: buf.buffer,
						...(after ? { after } : {}),
					}).then((d) => {
						if (d?.network !== buf.network || d?.buffer !== buf.buffer) {
							throw new Error("mismatched channel response");
						}
						return d;
					}),
				isStale: () => !alive || sock.current !== s || activeKeyRef.current !== key,
				onPage: (st) => {
					setChanInfo({
						network: buf.network,
						buffer: buf.buffer,
						joined: st.meta.joined,
						topic: st.meta.topic,
						members: st.members,
						// Only a walk that genuinely stopped early marks the
						// list incomplete; while more pages are inbound the
						// panel just shows what has arrived so far.
						truncated: st.done && st.degraded,
					});
				},
			});
		}, 150);
		return () => {
			alive = false;
			clearTimeout(t);
		};
	}, [
		activeKey, connected, chanTick,
		buffers[activeKey]?.network, buffers[activeKey]?.buffer,
		networks[buffers[activeKey]?.network]?.chantypes,
	]);

	// Visiting a buffer clears any prior load failure so it retries once (runs
	// before the load effect below, so the retry fires this same commit).
	useEffect(() => {
		failedHistory.current.delete(activeKey);
	}, [activeKey]);

	// Load history when a buffer becomes active and has none.
	useEffect(() => {
		if (!activeKey || !connected) return;
		const buf = buffers[activeKey];
		if (!buf || msgs[activeKey]?.loaded || loadingHistory.current.has(activeKey) ||
			failedHistory.current.has(activeKey)) return;
		const key = activeKey;
		const gen = historyGen.current[key] || 0;
		loadingHistory.current.add(key);
		sock.current
			.request("get_history", { network: buf.network, buffer: buf.buffer, limit: PAGE })
			.then((page) => {
				// A history_changed invalidated this buffer while the
				// request was in flight — discard the pre-backfill page;
				// the effect refetches once loadingHistory clears.
				if ((historyGen.current[key] || 0) !== gen) return;
				failedHistory.current.delete(key);
				setMsgs((m) => mergeHistoryPage(m, key, page, redactedIds.current.get(key)));
			})
			.catch(() => {
				// Record the failure so the effect doesn't immediately refire;
				// a (re)visit, reconnect, or reloadTail clears it to retry.
				failedHistory.current.add(key);
			})
			.finally(() => {
				loadingHistory.current.delete(key);
				setLoadTick((t) => t + 1);
			});
	}, [activeKey, connected, buffers, msgs, loadTick]);

	// Evict live message lists for buffers outside the KEEP_BUFFERS most
	// recently active. Runs on every activeKey change AND on load completion
	// (loadTick) so the working set stays bounded no matter how many channels a
	// bouncer feeds; a mid-load buffer is never evicted (its in-flight page
	// would have nowhere to merge). Re-running on loadTick matters when a load
	// finishes for a buffer that has since scrolled out of the recent set (the
	// user switched away while it loaded): the exemption then lifts and it is
	// evicted, instead of lingering until the next buffer switch. An evicted
	// buffer reloads its tail when reopened.
	const recentKeys = useRef([]);
	useEffect(() => {
		if (!activeKey) return;
		recentKeys.current = [activeKey, ...recentKeys.current.filter((k) => k !== activeKey)].slice(0, KEEP_BUFFERS);
		const keep = new Set(recentKeys.current);
		setMsgs((m) => {
			let next = m;
			for (const k of Object.keys(m)) {
				if (keep.has(k) || loadingHistory.current.has(k)) continue;
				if (next === m) next = { ...m };
				delete next[k];
			}
			return next;
		});
	}, [activeKey, loadTick]);

	// Default to the first buffer once the sidebar is known. If the
	// active key came from a hash that names no existing buffer, clear it
	// first so this effect can pick a real one (otherwise the app sticks
	// on the empty state despite having buffers).
	useEffect(() => {
		const keys = Object.keys(buffers);
		if (!keys.length) return;
		if (activeKey && !buffers[activeKey]) {
			setActiveKey(null);
			return;
		}
		if (activeKey) return;
		const firstBuf = Object.values(buffers).sort((a, b) =>
			(a.network + a.buffer).localeCompare(b.network + b.buffer))[0];
		select(firstBuf.network, firstBuf.buffer);
	}, [buffers, activeKey]);

	function addMonitor(network, nick) {
		nick = nick.trim();
		if (!nick) return;
		// Optimistic: show it immediately (offline until the server replies) —
		// but a REJECTION (duplicate under casemapping, invalid nick, store
		// failure) must roll the display back to the authoritative list, not
		// leave a phantom entry the server never accepted.
		setMonitors((all) => {
			const list = all[network] || [];
			if (list.some((m) => m.nick === nick)) return all;
			return { ...all, [network]: [...list, { nick, online: false }].sort((a, b) => a.nick.localeCompare(b.nick)) };
		});
		sock.current?.request("monitor_add", { network, nick }).catch(() => loadMonitors(network));
	}

	function removeMonitor(network, nick) {
		setMonitors((all) => ({ ...all, [network]: (all[network] || []).filter((m) => m.nick !== nick) }));
		sock.current?.request("monitor_remove", { network, nick }).catch(() => loadMonitors(network));
	}

	// stepBuffer moves the active buffer through the sidebar order
	// (wrapping); unreadOnly skips buffers with nothing new.
	function stepBuffer(dir, unreadOnly) {
		const bufs = buffersRef.current;
		const order = bufferOrder(bufs);
		if (!order.length) return;
		const n = order.length;
		const cur = order.indexOf(activeKeyRef.current); // -1 lands on 0/n-1
		for (let i = 1; i <= n; i++) {
			const b = bufs[order[(((cur + dir * i) % n) + n) % n]];
			if (!unreadOnly || b.unread > 0 || b.mention) {
				select(b.network, b.buffer);
				return;
			}
		}
	}
	stepRef.current = stepBuffer;

	function navigate(network, buffer) {
		const key = bufKey(network, buffer);
		// A buffer may not exist yet (/join, /msg to a fresh target):
		// create a placeholder so the view renders while events arrive.
		setBuffers((b) => (b[key] ? b : { ...b, [key]: makeBuffer(network, buffer) }));
		// Returning to a buffer that's showing a search-jump window drops
		// it so the live tail reloads. Bump the history generation on the drop
		// so a loadOlder page already in flight for that window is discarded,
		// not spliced into the fresh tail (a silent hole in scrollback).
		setMsgs((m) => {
			if (m[key]?.atTail === false) {
				historyGen.current[key] = (historyGen.current[key] || 0) + 1;
				return dropBufferMsgs(m, key);
			}
			return m;
		});
		setFocusId(null);
		location.hash = toHash(network, buffer);
		setActiveKey(key);
		setCmdError("");
		if (globalThis.innerWidth < 760) setSideOpen(false);
	}

	function select(network, buffer) {
		navigationIntent.current++;
		navigate(network, buffer);
	}

	function beginNavigation() {
		return ++navigationIntent.current;
	}

	function selectForNavigation(intent, network, buffer) {
		if (navigationIntent.current !== intent) return false;
		navigate(network, buffer);
		return true;
	}

	// rememberPendingWhois records a nick we asked about so its whois reply is
	// treated as client-initiated (opens/selects a buffer). Soft-capped so a
	// stream of never-answered requests can't grow it.
	function rememberPendingWhois(network, nick) {
		if (!nick) return;
		const s = pendingWhois.current;
		s.add(network + "\n" + foldNick(nick));
		if (s.size > 100) s.delete(s.values().next().value);
	}

	function sendCommand(network, command, params) {
		if (command === "WHOIS") rememberPendingWhois(network, params?.[0]);
		sock.current?.request("command", { network, command, params })
			.catch((e) => setCmdError(e.message || "failed"));
	}

	// forgetBuffer is the LOCAL half of a close (either mode): drop the
	// buffer's sidebar row, cached messages, and typing state. The server
	// side (closeBuffer) deletes or archives the stored copy — the sidebar
	// is store-driven, so a buffer left untouched in the store would
	// resurrect on the next refresh. Other devices drop it via the
	// buffer_closed push.
	function forgetBuffer(network, buffer) {
		const key = bufKey(network, buffer);
		setBuffers((b) => {
			if (!b[key]) return b;
			const next = { ...b };
			delete next[key];
			return next;
		});
		// Invalidate any in-flight initial get_history so it can't reinstall a
		// phantom msgs entry for the buffer we're closing.
		historyGen.current[key] = (historyGen.current[key] || 0) + 1;
		redactedIds.current.delete(key);
		setMsgs((m) => dropBufferMsgs(m, key));
		setTypers((all) => {
			if (!all.has(key)) return all;
			const next = new Map(all);
			next.delete(key);
			return next;
		});
		if (activeKeyRef.current === key) {
			const rest = Object.values(buffersRef.current)
				.filter((b) => b.key !== key)
				.sort((a, b) => (a.network + a.buffer).localeCompare(b.network + b.buffer));
			if (rest.length) select(rest[0].network, rest[0].buffer);
			else {
				setActiveKey(null);
				location.hash = "";
			}
		}
	}

	// closeBuffer removes a buffer from every device's sidebar. purge:true
	// deletes its stored history (the destructive close, behind a confirm);
	// purge:false archives it server-side — history kept, and the buffer
	// returns with its scrollback on new activity. Local cleanup
	// (forgetBuffer) is identical in both modes.
	async function closeBuffer(network, buffer, purge) {
		const s = sock.current;
		if (!s) throw new Error("not connected");
		try {
			const d = await s.request("close_buffer", { network, buffer, purge });
			// New servers return the canonical spelling they actually removed;
			// fall back for compatibility with an older nil `ok` response.
			forgetBuffer(d?.network || network, d?.buffer || buffer);
			setCmdError("");
			return d;
		} catch (e) {
			setCmdError(e.message || "closing buffer failed");
			throw e;
		}
	}

	// editTopic selects the channel and prefills that channel's keyed draft.
	// The intent/socket guards discard a superseded request or one that crossed
	// an auth boundary; switching elsewhere merely leaves the draft in its
	// intended channel instead of writing into the newly active composer.
	function editTopic(network, buffer) {
		const key = bufKey(network, buffer);
		const intent = ++topicIntent.current;
		const s = sock.current;
		select(network, buffer);
		if (!s) return;
		s.request("get_channel", { network, buffer })
			.then((d) => {
				if (topicIntent.current !== intent || sock.current !== s) return;
				composerApi.current?.prefill(key, `/topic ${d?.topic || ""}`);
			})
			.catch(() => {
				if (topicIntent.current !== intent || sock.current !== s) return;
				composerApi.current?.prefill(key, "/topic ");
			});
	}

	// openUserMenu: the right-click menu for a nick (member list, message).
	function openUserMenu(network, nick, x, y) {
		if (!nick) return;
		const ignored = isIgnored(ignoresRef.current, network, nick);
		const items = [
			{ label: "Whois", onClick: () => sendCommand(network, "WHOIS", [nick]) },
			{ label: "Direct message", onClick: () => select(network, nick) },
			{
				label: ignored ? "Unignore" : "Ignore", danger: !ignored,
				onClick: () => setIgnores((ig) => toggleIgnore(ig, network, nick)),
			},
			...modItems(network, nick),
		];
		setMenu({ x, y, title: nick, items });
	}

	// modItems: op/voice/kick/ban entries (The Lounge parity), shown only
	// when we hold status in the active channel and the target is present.
	// Halfop (%) can kick and manage voice; @ and above also op and ban.
	function modItems(network, nick) {
		const buf = activeBuf;
		const members = activeChanInfo?.members;
		if (!buf || buf.network !== network || !members) return [];
		const selfN = networks[network]?.nick;
		if (!selfN || nick === selfN) return [];
		const me = members.find((m) => m.nick === selfN);
		const target = members.find((m) => m.nick === nick);
		if (!me || !target) return [];
		const isOp = /[~&@]/.test(me.prefix || "");
		const isHalfop = (me.prefix || "").includes("%");
		if (!isOp && !isHalfop) return [];
		const tp = target.prefix || "";
		const mode = (flag) => () => sendCommand(network, "MODE", [buf.buffer, flag, nick]);
		// Ban is by nick mask; we do not track member hostnames.
		return [
			{ divider: true },
			...(isOp ? [tp.includes("@")
				? { label: "Remove op (-o)", onClick: mode("-o") }
				: { label: "Give op (+o)", onClick: mode("+o") }] : []),
			tp.includes("+")
				? { label: "Remove voice (-v)", onClick: mode("-v") }
				: { label: "Give voice (+v)", onClick: mode("+v") },
			{ label: "Kick", danger: true, onClick: () => sendCommand(network, "KICK", [buf.buffer, nick]) },
			...(isOp ? [{
				label: "Ban", danger: true,
				onClick: () => sendCommand(network, "MODE", [buf.buffer, "+b", `${nick}!*@*`]),
			}] : []),
		];
	}

	// openBufferMenu: the right-click menu for a sidebar row — channel
	// actions for channels, DM actions for query buffers.
	function openBufferMenu(network, buffer, x, y) {
		const key = bufKey(network, buffer);
		const muted = isMuted(mutesRef.current, key);
		const chan = isChannelName(buffer, networksRef.current[network]?.chantypes);
		const ig = !chan && isIgnored(ignoresRef.current, network, buffer);
		// The purgeOnClose pref (default off) decides what closing means:
		// off archives (history kept server-side, no confirmation); on is the
		// destructive delete behind confirmDestructiveClose. Local cleanup is
		// the same either way (closeBuffer -> forgetBuffer).
		const purgeClose = prefsRef.current.purgeOnClose === true;
		const doClose = async (leave, purge) => {
			if (!leave) {
				await closeBuffer(network, buffer, purge).catch(() => {});
				return;
			}
			const s = sock.current;
			if (!s) {
				setCmdError("not connected");
				return;
			}
			try {
				// part_channel also removes the channel from autojoin. The stored
				// history is touched only after both operations are acknowledged.
				await s.request("part_channel", { network, channel: buffer });
				await closeBuffer(network, buffer, purge);
			} catch (e) {
				setCmdError(e.message || "leaving channel failed");
			}
		};
		const confirmDestructiveClose = (leave) => setMenu({
			x, y, title: buffer,
			items: [{
				label: leave
					? "Really leave? This erases its scrollback"
					: "Really close? This erases its scrollback",
				danger: true,
				onClick: () => doClose(leave, true),
			}],
		});
		const items = [
			...(chan
				? [{ label: "Edit topic", onClick: () => editTopic(network, buffer) }]
				: [
					{ label: "Whois", onClick: () => sendCommand(network, "WHOIS", [buffer]) },
					{
						label: ig ? "Unignore" : "Ignore", danger: !ig,
						onClick: () => setIgnores((x2) => toggleIgnore(x2, network, buffer)),
					},
				]),
			{
				label: muted ? "Unmute" : "Mute",
				onClick: () => setMutes((m) => toggleMute(m, key)),
			},
			chan
				? {
					label: purgeClose ? "Leave channel…" : "Leave channel",
					danger: purgeClose,
					onClick: () => (purgeClose ? confirmDestructiveClose(true) : doClose(true, false)),
				}
				: {
					label: purgeClose ? "Close…" : "Close",
					danger: purgeClose,
					onClick: () => (purgeClose ? confirmDestructiveClose(false) : doClose(false, false)),
				},
		];
		setMenu({ x, y, title: buffer, items });
	}

	// openNetworkMenu: click/right-click on a network header row.
	function openJoinPrompt(network) {
		const intent = ++chanPromptIntent.current;
		setChanPromptError("");
		setChanPromptBusy(false);
		chanPromptBusyRef.current = false;
		setChanPrompt({ network, intent });
	}

	function closeJoinPrompt() {
		chanPromptIntent.current++;
		setChanPrompt(null);
		setChanPromptBusy(false);
		chanPromptBusyRef.current = false;
	}

	function openNewNetwork() {
		const intent = ++netFormIntent.current;
		setNetFormError("");
		setNetFormBusy(false);
		netFormBusyRef.current = false;
		setNetForm({ initial: null, oldName: "", intent });
	}

	function closeNetworkForm() {
		netFormIntent.current++;
		setNetForm(null);
		setNetFormBusy(false);
		netFormBusyRef.current = false;
	}

	function openNetworkMenu(network, x, y) {
		setMenu({
			x, y, title: network,
			items: [
				{ label: "Join channel…", onClick: () => openJoinPrompt(network) },
				{ label: "Edit network…", onClick: () => editNetwork(network) },
				{ label: "Add network…", onClick: openNewNetwork },
				{
					// Two-step, like the edit form's guarded delete: this permanently
					// erases the network AND its scrollback, so one misclick on a
					// context-menu row must not be enough. (The menu's onClose fires
					// before onClick, so reopening with the confirm item wins the
					// state batch.)
					label: "Remove network…", danger: true,
					onClick: () => setMenu({
						x, y, title: network,
						items: [{
							label: "Really remove? This erases its scrollback",
							danger: true,
							onClick: () => deleteNetwork(network),
						}],
					}),
				},
			],
		});
	}

	function editNetwork(network) {
		// Bumping the intent invalidates any in-flight save/delete's .finally
		// (it only resets the busy ref when the intent still matches), so every
		// bump site must reset the ref itself or it latches true forever and
		// save/delete silently no-op with enabled-looking buttons.
		const intent = ++netFormIntent.current;
		netFormBusyRef.current = false;
		setNetFormBusy(false);
		setCmdError("");
		const s = sock.current;
		if (!s) {
			setCmdError("not connected");
			return;
		}
		const stale = () => netFormIntent.current !== intent || sock.current !== s;
		s.request("get_network", { network }).then((data) => {
			if (stale()) return;
			const n = editableNetwork(data, network);
			setNetFormError("");
			setNetFormBusy(false);
			setNetForm({ initial: n.config, oldName: network, intent });
		}).catch((e) => {
			if (stale()) return;
			setCmdError(networkEditError(network, e));
		});
	}

	function saveNetwork(cfg, oldName) {
		const intent = netForm?.intent;
		const s = sock.current;
		if (!intent || !s || netFormBusyRef.current) return;
		netFormBusyRef.current = true;
		setNetFormBusy(true);
		s.request("put_network", { old_name: oldName || undefined, config: cfg })
			.then(() => {
				if (netFormIntent.current !== intent || sock.current !== s) return;
				setNetForm(null);
				setNetFormError("");
			})
			.catch((e) => {
				if (netFormIntent.current === intent && sock.current === s) setNetFormError(e.message || "saving failed");
			})
			.finally(() => {
				if (netFormIntent.current === intent && sock.current === s) {
					netFormBusyRef.current = false;
					setNetFormBusy(false);
				}
			});
	}

	function deleteNetwork(name) {
		const fromForm = netForm?.oldName === name;
		const s = sock.current;
		// Busy guard BEFORE the intent bump: a double-click used to bump the
		// intent first, which stranded the first request's .finally (intent
		// mismatch) and latched netFormBusyRef true until the form closed.
		if (!s || netFormBusyRef.current) return;
		const intent = fromForm ? netForm.intent : ++netFormIntent.current;
		netFormBusyRef.current = true;
		setNetFormBusy(true);
		s.request("delete_network", { network: name })
			.then(() => {
				if (netFormIntent.current !== intent || sock.current !== s) return;
				setNetForm(null);
				setNetFormError("");
			})
			.catch((e) => {
				if (netFormIntent.current !== intent || sock.current !== s) return;
				if (fromForm) setNetFormError(e.message || "deleting failed");
				else setCmdError(e.message || "deleting network failed");
			})
			.finally(() => {
				if (netFormIntent.current === intent && sock.current === s) {
					netFormBusyRef.current = false;
					setNetFormBusy(false);
				}
			});
	}

	// Target-correlated pending joins pair overlapping actions with their actual
	// self-JOIN (including 470 aliases) without letting an async IRC rejection
	// consume an unrelated later join.
	const pendingJoins = useRef(new Map());
	function joinChannel(network, channel) {
		const intent = chanPrompt?.intent;
		const s = sock.current;
		if (!intent || !s || chanPromptBusyRef.current) return;
		chanPromptBusyRef.current = true;
		setChanPromptBusy(true);
		// Arm before sending: the self-JOIN push can beat the response envelope.
		const nav = beginNavigation();
		const joinToken = armPendingJoin(pendingJoins.current, network, channel, nav);
		s.request("join_channel", { network, channel })
			.then(() => {
				if (chanPromptIntent.current !== intent || sock.current !== s) return;
				setChanPrompt(null);
				setChanPromptError("");
			})
			.catch((e) => {
				clearPendingJoin(pendingJoins.current, joinToken);
				if (chanPromptIntent.current === intent && sock.current === s) setChanPromptError(e.message || "join failed");
			})
			.finally(() => {
				if (chanPromptIntent.current === intent && sock.current === s) {
					chanPromptBusyRef.current = false;
					setChanPromptBusy(false);
				}
			});
	}

	// jumpTo opens a search result: load a window centered on the message
	// and highlight it. The window is not the live tail, so incoming
	// events won't append (see the event handler).
	function jumpTo(ev) {
		const key = bufKey(ev.network, ev.buffer);
		const s = sock.current;
		if (!s) return;
		const nav = beginNavigation();
		setSearchOpen(false);
		// Clear focus up front so re-jumping to the SAME result still registers
		// as a focusId change — VirtualList only arms a scroll on a change, so
		// without this, clicking the same row twice (after scrolling away) is a
		// no-op and the view never re-centers.
		setFocusId(null);
		// Invalidate any in-flight initial history load for this buffer so
		// its resolve is discarded (see the load effect's gen check) instead
		// of merging the live tail into — and flipping atTail true on — the
		// around-window we install below, which would leave a temporal gap.
		historyGen.current[key] = (historyGen.current[key] || 0) + 1;
		const gen = historyGen.current[key];
		s.request("get_history", {
				network: ev.network, buffer: ev.buffer,
				around: { ts: ev.time, id: ev.id }, limit: PAGE,
			})
			.then((page) => {
				// The buffer was closed/invalidated (e.g. buffer_closed from
				// another device) while this was in flight — don't recreate a
				// ghost buffer, mirroring the initial load and loadOlder guards.
				if (sock.current !== s || navigationIntent.current !== nav ||
					(historyGen.current[key] || 0) !== gen) return;
				setBuffers((b) => (b[key] ? b : { ...b, [key]: makeBuffer(ev.network, ev.buffer) }));
				setMsgs((m) => {
					const list = applyTombstones(page.messages || [], redactedIds.current.get(key));
					return {
						...m,
						[key]: { list, bytes: listBytes(list), loaded: true, reachedTop: false, atTail: false },
					};
				});
				location.hash = toHash(ev.network, ev.buffer);
				setActiveKey(key);
				setFocusId(ev.id);
				setCmdError("");
			})
			.catch(() => {});
	}

	function loadOlder() {
		const buf = buffers[activeKey];
		const cur = msgs[activeKey];
		if (!buf || !cur?.list.length || cur.reachedTop) return Promise.resolve();
		const key = activeKey;
		const gen = historyGen.current[key] || 0;
		const oldest = cur.list[0];
		return sock.current
			.request("get_history", {
				network: buf.network, buffer: buf.buffer,
				before: { ts: oldest.time, id: oldest.id }, limit: PAGE,
			})
			.then((page) => {
				const older = page.messages || [];
				setMsgs((m) => {
					// A history_changed invalidated this buffer while the
					// request was in flight (backfill, close, network removal)
					// — discard the stale page rather than merging it or
					// wrongly marking reachedTop, mirroring the initial load.
					if ((historyGen.current[key] || 0) !== gen) return m;
					const prev = m[key];
					if (!prev) return m;
					let list = applyTombstones(mergeById(prev.list, older), redactedIds.current.get(key));
					// Bound memory on the paging-back path too: keep the
					// oldest (we are scrolled up), dropping the newest tail —
					// it reloads on scroll-down / new events. Enforce BOTH the
					// count cap and the byte cap: a page of max-size lines can
					// blow the byte budget well under TRIM_TO messages, and this
					// non-tail window is never re-trimmed by live appends.
					let atTail = prev.atTail;
					if (list.length > TRIM_AT) {
						list = list.slice(0, TRIM_TO);
						atTail = false;
					}
					let bytes = listBytes(list);
					if (bytes > MAX_BUFFER_BYTES) {
						let end = list.length;
						while (end > 1 && bytes > MAX_BUFFER_BYTES) {
							end--;
							bytes -= evBytes(list[end]);
						}
						list = list.slice(0, end);
						atTail = false;
					}
					return {
						...m,
						[key]: {
							...prev,
							list,
							bytes,
							reachedTop: !historyHasMore(page, PAGE),
							atTail,
						},
					};
				});
			})
			.catch(() => {});
	}

	// reloadTail drops a non-tail window (paged past the trim point, or a
	// search jump) so the history-load effect refetches the live tail and
	// live events flow again.
	function reloadTail() {
		failedHistory.current.delete(activeKey); // explicit reload: allow a retry
		// Bump the history generation on the drop so a loadOlder page already in
		// flight for the dropped (non-tail) window is discarded rather than
		// merged into the reloaded live tail — otherwise old rows splice in
		// adjacent to the newest, leaving a silent gap.
		setMsgs((m) => {
			if (m[activeKey]?.atTail === false) {
				historyGen.current[activeKey] = (historyGen.current[activeKey] || 0) + 1;
				return dropBufferMsgs(m, activeKey);
			}
			return m;
		});
	}

	// sendInput returns a promise that resolves when the send is accepted and
	// rejects on a parse error or a rejected request, so the composer can
	// keep the user's text on failure instead of dropping it.
	function sendInput(text, targetKey) {
		// The composer captures its buffer key at submit time. State updates from
		// a click/hash switch can otherwise make the parent render target B while
		// the still-visible text originated in A.
		const buf = buffersRef.current[targetKey];
		if (!buf) return Promise.reject(new Error("no active buffer"));
		setCmdError("");
		const p = parseInput(text, buf.buffer, networksRef.current[buf.network]?.chantypes);
		const oops = (e) => {
			setCmdError(e.message || "failed");
			throw e; // propagate so submit keeps the draft
		};
		switch (p.type) {
			case "error":
				setCmdError(p.message);
				return Promise.reject(new Error(p.message));
			case "text":
				return sock.current
					.request("send", { network: buf.network, target: buf.buffer, text: p.text })
					.catch(oops);
			case "msg": {
				const s = sock.current;
				if (!s) return Promise.reject(new Error("not connected")).catch(oops);
				const nav = beginNavigation();
				return s
					.request("send", { network: buf.network, target: p.target, text: p.text })
					.then(() => {
						if (sock.current === s) selectForNavigation(nav, buf.network, p.target);
					})
					.catch(oops);
			}
			case "cmd":
				if (p.command === "WHOIS") rememberPendingWhois(buf.network, p.params?.[0]);
				const s = sock.current;
				if (!s) return Promise.reject(new Error("not connected")).catch(oops);
				// Like the modal path, arm /join before the request so a fast
				// self-JOIN cannot arrive in the response/JOIN scheduling gap.
				const joinToken = p.switchTo
					? armPendingJoin(
						pendingJoins.current, buf.network, p.switchTo, beginNavigation(),
					)
					: null;
				return s
					.request("command", { network: buf.network, command: p.command, params: p.params })
					.catch((e) => {
						if (joinToken) clearPendingJoin(pendingJoins.current, joinToken);
						return oops(e);
					});
			default:
				return Promise.reject(new Error("unknown input"));
		}
	}

	// Per-buffer optimistic dedupe: a global value let two buffers that
	// share the newest-message timestamp suppress each other's marker.
	const readSent = useRef({});
	function markRead(time) {
		const buf = buffers[activeKey];
		if (!buf || time <= buf.marker || time === readSent.current[activeKey]) return;
		const key = activeKey;
		readSent.current[key] = time;
		sock.current
			.request("set_read_marker", { network: buf.network, buffer: buf.buffer, time })
			.then((d) => setBuffers((b) => applyMarkerState(b, key, d.time)))
			.catch(() => {
				// The marker never reached the server, so roll back the
				// optimistic guard — otherwise refocusing or reselecting the
				// buffer (the natural recovery paths, which re-mark the same
				// last.time) would short-circuit and the read position would
				// stay desynced on other devices until a NEW message arrives.
				if (readSent.current[key] === time) delete readSent.current[key];
			});
	}

	if (phase === "checking") return null;
	if (phase === "login") return <Login onLogin={() => setPhase("app")} />;

	const activeBuf = activeKey ? buffers[activeKey] : null;
	const selfNick = activeBuf ? networks[activeBuf.network]?.nick : "";
	const netState = activeBuf ? networks[activeBuf.network]?.state : "";
	const isChan = activeBuf && isChannelName(activeBuf.buffer, networks[activeBuf.network]?.chantypes);
	// State clearing is a passive effect, so correctness cannot depend on it
	// running before paint. Only consume a snapshot naming this exact buffer.
	const activeChanInfo = chanInfo && activeBuf &&
		chanInfo.network === activeBuf.network && chanInfo.buffer === activeBuf.buffer
		? chanInfo
		: null;
	const topicText = topicFor(activeBuf, netState, activeChanInfo);
	// Lowercased nick -> "user@host" for the active channel, so a message nick
	// can show its ident/host on hover (from userhost-in-names / JOIN / CHGHOST).
	const userhosts = useMemo(() => {
		const m = new Map();
		for (const mem of activeChanInfo?.members || []) {
			if (mem.user || mem.host) m.set(mem.nick.toLowerCase(), `${mem.user || ""}@${mem.host || ""}`);
		}
		return m;
	}, [activeChanInfo]);
	// Lowercased nick -> highest mode symbol (@, +, …) for the active channel,
	// so the message list can prefix a sender's nick with their status (the
	// nickPrefixes pref). Empty for queries (no roster).
	const memberPrefixes = useMemo(() => {
		const m = new Map();
		for (const mem of activeChanInfo?.members || []) {
			const p = (mem.prefix || "")[0];
			if (p) m.set(mem.nick.toLowerCase(), p);
		}
		return m;
	}, [activeChanInfo]);
	const ignoredHere = activeBuf ? ignores[activeBuf.network] || [] : [];
	const mutedSet = new Set(mutes);
	const timeFmt = { clock: prefs.clock, seconds: prefs.seconds, ampm: prefs.ampm };
	// Any of these can change wrapping/row height without changing message ids.
	// VirtualList uses the key to invalidate measurements while preserving a
	// stable visible-row anchor.
	const rowLayoutKey = JSON.stringify([
		theme, prefs.textSize, prefs.density, prefs.msgFont, prefs.css,
		prefs.statusMsgs, prefs.statusHost, prefs.clock, prefs.seconds,
		prefs.ampm, prefs.nickSep, prefs.highlightNames, prefs.nickPrefixes,
		previews,
	]);

	return (
		<div class="app">
			<div class={"sidebar" + (sideOpen ? " open" : "")}>
				<Sidebar
					networks={networks} buffers={buffers} activeKey={activeKey}
					monitors={monitors} truncated={buffersTruncated}
					theme={theme} mutedSet={mutedSet} onSelect={select}
					onSettings={() => setSettingsOpen(true)}
					onBufferMenu={openBufferMenu} onNetworkMenu={openNetworkMenu}
					onAddNetwork={openNewNetwork}
					onAddMonitor={addMonitor} onRemoveMonitor={removeMonitor}
				/>
			</div>
			{sideOpen && <div class="side-scrim" aria-hidden="true" onClick={() => setSideOpen(false)} />}
			<div class="main">
				<TopBar
					activeBuf={activeBuf} isChan={isChan} topicText={topicText}
					sideOpen={sideOpen} rightOpen={rightOpen} theme={theme}
					onSide={() => setSideOpen(!sideOpen)}
					onRight={() => setRightOpen(!rightOpen)}
					onSearch={() => setSearchOpen(true)}
					onTheme={() => updatePrefs({ ...prefs, theme: theme === "dark" ? "light" : "dark" })}
				/>
				{!connected && <div class="conn-banner">connection lost — reconnecting…</div>}
				{activeBuf ? (
					<Chat
						buf={activeBuf} msgs={msgs[activeKey]} selfNick={selfNick} theme={theme}
						connected={connected && netState === "registered"}
						error={cmdError}
						typers={Array.from(typers.get(activeKey)?.keys() || [])}
						focusId={focusId}
						completionNicks={isChan
							? (activeChanInfo?.members || []).map((m) => m.nick)
							: [activeBuf.buffer]}
						ignoredNicks={ignoredHere}
						statusMode={prefs.statusMsgs}
						statusHost={prefs.statusHost}
						timeFmt={timeFmt} nickSep={prefs.nickSep} previews={previews}
						highlightNames={prefs.highlightNames}
						userhosts={userhosts}
						nickPrefixes={prefs.nickPrefixes}
						memberPrefixes={memberPrefixes}
						layoutKey={rowLayoutKey}
						composerApi={composerApi}
						onNick={(nick, x, y) => openUserMenu(activeBuf.network, nick, x, y)}
						isHighlight={(t) => highlightText(t, selfNick, rules, activeBuf.network)}
						onRedact={(msgid) =>
							sock.current?.request("redact", {
								network: activeBuf.network, buffer: activeBuf.buffer, msgid,
							}).catch((e) => setCmdError(e.message || "delete failed"))}
						onSend={sendInput} onLoadOlder={loadOlder} onReloadTail={reloadTail} onRead={markRead}
						onTyping={(state, net, bufName) => {
							// The sendTyping pref gates ALL our typing tags (including
							// the trailing "done") — off means other clients never see
							// us typing. Remote indicators self-expire (6s/30s), so a
							// suppressed "done" clears on its own.
							if (!prefs.sendTyping) return;
							sock.current?.notify("typing", {
								network: net ?? activeBuf.network, buffer: bufName ?? activeBuf.buffer, state,
							});
						}}
					/>
				) : (
					<div class="empty-state">
						{cmdError && <div class="cmd-error">{cmdError}</div>}
						<div>no buffers yet — waiting for traffic</div>
					</div>
				)}
			</div>
			{isChan && rightOpen && <div class="right-scrim" aria-hidden="true" onClick={() => setRightOpen(false)} />}
			{isChan && (
				<div class={"rightbar" + (rightOpen ? " open" : "")}>
					<Members
							info={activeChanInfo} theme={theme} ignoredNicks={ignoredHere}
							onNick={(nick, x, y) => openUserMenu(activeBuf.network, nick, x, y)}
						/>
				</div>
			)}
			{searchOpen && (
				<SearchOverlay sock={sock} onJump={jumpTo} onClose={() => setSearchOpen(false)} timeFmt={timeFmt} nickSep={prefs.nickSep} redactedIds={redactedIds} />
			)}
			{switcherOpen && (
				<Switcher
					buffers={buffers} networks={networks}
					onSelect={(network, buffer) => {
						setSwitcherOpen(false);
						select(network, buffer);
					}}
					onClose={() => setSwitcherOpen(false)}
				/>
			)}
			{settingsOpen && (
				<Settings
					networks={networks} rules={rules} onRules={updateRules}
					prefs={prefs} prefsError={prefsError} onPrefs={updatePrefs} onPreviews={(v) => { previewsPinned.current = true; setPreviews(v); }}
					notifier={notifier.current} onClose={() => setSettingsOpen(false)}
					onLogout={() => { setSettingsOpen(false); setPhase("login"); }}
				/>
			)}
			{netForm && (
				<NetworkForm
					key={netForm.intent}
					initial={netForm.initial} oldName={netForm.oldName}
					error={netFormError} busy={netFormBusy}
					onSave={saveNetwork} onDelete={deleteNetwork}
					onClose={closeNetworkForm}
				/>
			)}
			{chanPrompt && (
				<ChannelPrompt
					network={chanPrompt.network} error={chanPromptError}
					busy={chanPromptBusy}
					chantypes={networks[chanPrompt.network]?.chantypes}
					onJoin={joinChannel} onClose={closeJoinPrompt}
				/>
			)}
			<ContextMenu menu={menu} onClose={() => setMenu(null)} />
		</div>
	);
}
