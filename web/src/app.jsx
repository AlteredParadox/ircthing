import { useEffect, useRef, useState } from "preact/hooks";
import { Chat } from "./chat.jsx";
import { bufferOrder, bufKey, isChannelName, mergeById, mergeServerBuffers, parseHash, parseInput, renderable, toHash, typingExpired } from "./irc.js";
import { applyBadge, highlightText, loadRules, Notifier, saveRules } from "./notify.js";
import { Login } from "./login.jsx";
import { applyPrefs, loadPrefs, normalizePrefs, resolveTheme, savePrefs } from "./prefs.js";
import { isIgnored, isMuted, loadIgnores, loadMutes, toggleIgnore, toggleMute } from "./local.js";
import { ContextMenu } from "./menu.jsx";
import { Members } from "./members.jsx";
import { ChannelPrompt, NetworkForm } from "./netform.jsx";
import { SearchOverlay } from "./search.jsx";
import { Settings } from "./settings.jsx";
import { Sidebar } from "./sidebar.jsx";
import { Switcher } from "./switcher.jsx";
import { BufIcon } from "./icons.jsx";
import { Socket } from "./ws.js";

const PAGE = 100;
// Memory bound per buffer: trim to TRIM_TO once past TRIM_AT. Older pages
// are always refetchable, so trimming loses nothing durable. The list is
// virtualized, so these bound JS memory, not DOM size.
const TRIM_AT = 50000;
const TRIM_TO = 25000;
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
	return chanInfo?.topic || "";
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
					<BufIcon chan={isChan} />
					{activeBuf.buffer}
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
	let list = [...(cur?.list || []), ev];
	let reachedTop = cur?.reachedTop;
	if (list.length > TRIM_AT) {
		list = list.slice(list.length - TRIM_TO);
		reachedTop = false;
	}
	return { ...m, [key]: { ...cur, list, reachedTop } };
}

// appendInfoLine appends an ephemeral server_info system line, bounded the
// same way appendEventMsgs is: a flood (e.g. /list streaming one line per
// channel on a huge network) must not grow the buffer without limit.
function appendInfoLine(m, key, ev) {
	const cur = m[key];
	if (cur?.loaded && cur.atTail === false) return m;
	let list = [...(cur?.list || []), ev];
	let reachedTop = cur?.reachedTop;
	if (list.length > TRIM_AT) {
		list = list.slice(list.length - TRIM_TO);
		reachedTop = false;
	}
	return { ...m, [key]: { ...cur, list, reachedTop } };
}

// clearTyperFor drops one nick's typing state (they just spoke).
function clearTyperFor(t, key, sender) {
	if (!t[key]?.[sender]) return t;
	const cur = { ...t[key] };
	delete cur[sender];
	return { ...t, [key]: cur };
}

// setTypingState records a typing push; "done" clears it.
function setTypingState(t, key, d) {
	const cur = { ...t[key] };
	if (d.state === "done") delete cur[d.nick];
	else cur[d.nick] = { state: d.state, at: Date.now() };
	return { ...t, [key]: cur };
}

// bumpBufferActivity advances a buffer's last-activity/unread/mention
// state for a live event (creating the buffer on first traffic).
function bumpBufferActivity(b, key, ev, countUnread, highlight) {
	const cur = b[key] || makeBuffer(ev.network, ev.buffer);
	return {
		...b,
		[key]: {
			...cur,
			lastTime: ev.time,
			unread: countUnread ? cur.unread + 1 : cur.unread,
			mention: cur.mention || highlight,
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
	return hit ? { ...m, [key]: { ...cur, list } } : m;
}

// mergeHistoryPage installs a fetched page, keeping events that streamed
// in while the fetch was in flight (the page is authoritative for what
// it covers).
function mergeHistoryPage(m, key, page) {
	const cur = m[key];
	// An installed jump/search window is a non-tail slice (atTail === false).
	// An initial tail load must not merge into it: that would splice the
	// live tail onto an older window (a temporal gap) and wrongly flip
	// atTail true. The window owns the buffer until the user returns to the
	// tail, which reloads it. (jumpTo also bumps historyGen to discard the
	// in-flight load early; this guards the re-dispatch race belt-and-braces.)
	if (cur?.loaded && cur.atTail === false) return m;
	const pageMsgs = page.messages || [];
	return {
		...m,
		[key]: {
			list: mergeById(cur?.list || [], pageMsgs),
			loaded: true,
			reachedTop: pageMsgs.length < PAGE,
			atTail: true,
		},
	};
}

export function App() {
	const [phase, setPhase] = useState("checking"); // checking | login | app
	const [previews, setPreviews] = useState(true); // link/media previews enabled (server switch)
	const [connected, setConnected] = useState(false);
	const [networks, setNetworks] = useState({});
	const [buffers, setBuffers] = useState({});
	// Network management: the add/edit form ({ initial, oldName } | null)
	// and the join-channel prompt ({ network } | null), with the last
	// server error for each.
	const [netForm, setNetForm] = useState(null);
	const [netFormError, setNetFormError] = useState("");
	const [netFormBusy, setNetFormBusy] = useState(false);
	const [chanPrompt, setChanPrompt] = useState(null);
	const [chanPromptError, setChanPromptError] = useState("");
	const [msgs, setMsgs] = useState({});
	const [activeKey, setActiveKey] = useState(() => {
		const h = parseHash(location.hash);
		return h ? bufKey(h.network, h.buffer) : null;
	});
	const [prefs, setPrefs] = useState(loadPrefs);
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
	const notifier = useRef();
	if (!notifier.current) notifier.current = new Notifier();
	// typers: bufKey -> { nick: { state, at } }; ephemeral, never stored.
	const [typers, setTypers] = useState({});
	const sock = useRef(null);
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
	function updatePrefs(next) {
		setPrefs(next);
		prefsDirty.current = true;
		// Debounced: the custom-CSS textarea changes on every keystroke.
		clearTimeout(prefsPush.current);
		prefsPush.current = setTimeout(() => {
			sock.current?.request("set_prefs", { prefs: next })
				// Only clear the dirty flag if no newer edit landed while this
				// request was in flight (prefs still === the pushed snapshot);
				// otherwise the queued newer edit would look synced and a
				// reconnect would adopt the server's now-stale copy over it.
				.then(() => { if (prefsRef.current === next) prefsDirty.current = false; })
				.catch(() => {});
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

	// Server switches (whether link/media previews are enabled). Fetched
	// once authed so the UI never requests previews the server disabled.
	useEffect(() => {
		if (phase !== "app") return;
		fetch("/api/config")
			.then((r) => (r.ok ? r.json() : null))
			.then((c) => c && typeof c.previews === "boolean" && setPreviews(c.previews))
			.catch(() => {});
	}, [phase]);

	useEffect(() => {
		const onHash = () => {
			const h = parseHash(location.hash);
			if (h) setActiveKey(bufKey(h.network, h.buffer));
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

	// Socket helpers, at component scope so handler nesting stays
	// shallow. All of them read sock.current, which the effect below
	// sets before any handler can fire.

	// loadMonitors fetches one network's MONITOR buddy list with presence.
	function loadMonitors(name) {
		sock.current.request("get_monitors", { network: name })
			.then((md) => setMonitors((all) => ({ ...all, [name]: md.monitors || [] })))
			.catch(() => {});
	}

	// applyBuffers installs a get_buffers response: network states and
	// the sidebar buffer list (the mention flag is client-side; keep it).
	function applyBuffers(data) {
		const nets = {};
		for (const n of data.networks || []) {
			nets[n.name] = { state: n.state, nick: n.nick, chantypes: n.chantypes || "#&" };
		}
		setNetworks(nets);
		for (const name of Object.keys(nets)) loadMonitors(name);

		// mergeServerBuffers preserves client-only ephemeral buffers but never
		// resurrects a real server buffer the server intentionally dropped
		// (e.g. closed on another device while we were offline).
		const bufs = mergeServerBuffers(data.buffers, buffersRef.current, nets);
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
		sock.current.request("get_buffers", null).then(applyBuffers).catch(() => {});
	}

	// adoptPrefs applies the server's prefs; a server with none stored
	// yet (fresh install, pre-sync upgrade) is seeded from this
	// browser's cache.
	function adoptPrefs(d) {
		// A change made while disconnected never reached the server, so
		// its get_prefs is stale — re-push the local prefs instead of
		// reverting to it.
		if (prefsDirty.current) {
			sock.current?.request("set_prefs", { prefs: prefsRef.current })
				.then(() => { prefsDirty.current = false; })
				.catch(() => {});
			return;
		}
		if (d.prefs) setPrefs(normalizePrefs(d.prefs));
		else sock.current.request("set_prefs", { prefs: prefsRef.current }).catch(() => {});
	}

	// Socket lifecycle, once authed.
	useEffect(() => {
		if (phase !== "app") return;
		const s = new Socket(wsURL());
		sock.current = s;

		const wsFailures = { n: 0 };
		s.on("_open", async () => {
			wsFailures.n = 0;
			failedHistory.current.clear(); // fresh connection: let loads retry
			setConnected(true);
			s.request("get_prefs", null).then(adoptPrefs).catch(() => {});
			// Drop cached pages up front so every open buffer refetches a
			// fresh tail covering the offline window. This must NOT hang off
			// get_buffers succeeding: if that request rejects (error envelope
			// or timeout) while the socket stays up, a stale active buffer
			// would keep loaded:true, never refetch, and silently gap when
			// live events resume appending to it.
			setMsgs({});
			try {
				applyBuffers(await s.request("get_buffers", null));
			} catch {
				/* sidebar refresh will retry; scrollback already reset above */
			}
		});
		s.on("_close", () => {
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
						if (r.status === 401) {
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
		s.on("history_changed", (d) => {
			const key = bufKey(d.network, d.buffer);
			historyGen.current[key] = (historyGen.current[key] || 0) + 1;
			setMsgs((m) => dropBufferMsgs(m, key));
			clearTimeout(bufRefresh);
			bufRefresh = setTimeout(refreshBuffers, 300);
		});

		s.on("state", (d) => {
			setNetworks((n) => ({ ...n, [d.network]: { ...n[d.network], state: d.state } }));
			// A (re)registered network's ISUPPORT (chantypes, nick) lands
			// just after 001; refresh the buffer list once it settles.
			if (d.state === "registered") {
				clearTimeout(bufRefresh);
				bufRefresh = setTimeout(refreshBuffers, 1500);
			}
		});

		s.on("event", (ev) => {
			const key = bufKey(ev.network, ev.buffer);
			const keep = key === activeKeyRef.current || loadingHistory.current.has(key);
			setMsgs((m) => appendEventMsgs(m, key, ev, keep));
			// A message from someone clears their typing indicator.
			setTypers((t) => clearTyperFor(t, key, ev.sender));
			const r = renderable(ev);
			const isMsg = r.kind !== "system";
			const nick = networksRef.current[ev.network]?.nick;
			const mine = nick && ev.sender === nick;
			// A UI-initiated join selects the channel we actually ended up
			// in — which may differ from the requested one (channel
			// forwarding, numeric 470) — so selection follows our own JOIN
			// instead of assuming the requested name.
			const pj = pendingJoin.current;
			if (pj && mine && ev.command === "JOIN" && ev.network === pj.network && Date.now() - pj.ts < 15000) {
				pendingJoin.current = null;
				select(ev.network, ev.buffer);
			}
			// Highlight = a mention/keyword in a channel, or any message in
			// a query (PM) buffer. PMs always alert.
			const isChan = isChannelName(ev.buffer, networksRef.current[ev.network]?.chantypes);
			const highlight = isMsg && !mine &&
				(isChan ? highlightText(r.text, nick, rulesRef.current, ev.network) : true);
			// Ignored senders never count or alert (and are hidden at
			// render); muted buffers still count unread but never alert.
			const ignored = isIgnored(ignoresRef.current, ev.network, ev.sender);
			const muted = isMuted(mutesRef.current, key);
			const alert = highlight && !ignored && !muted;

			setBuffers((b) => bumpBufferActivity(b, key, ev, isMsg && !mine && !ignored, alert));

			// Desktop notification when an alert lands somewhere the user
			// isn't looking (tab hidden, or a different buffer active).
			if (alert && (document.hidden || key !== activeKeyRef.current)) {
				const where = isChan ? `${ev.sender} in ${ev.buffer}` : ev.sender;
				notifier.current.show(where, r.text, key, () => {
					location.hash = toHash(ev.network, ev.buffer);
					setActiveKey(key);
				});
			}
		});

		// Another device closed a buffer; drop it here too.
		s.on("buffer_closed", (d) => {
			const key = bufKey(d.network, d.buffer);
			if (!buffersRef.current[key]) return;
			setBuffers((b) => {
				const next = { ...b };
				delete next[key];
				return next;
			});
			// Invalidate any in-flight initial get_history for this buffer, so
			// its resolve doesn't reinstall a phantom msgs entry after teardown.
			historyGen.current[key] = (historyGen.current[key] || 0) + 1;
			setMsgs((m) => dropBufferMsgs(m, key));
			if (activeKeyRef.current === key) {
				setActiveKey(null);
				location.hash = "";
			}
		});

		// A network was deleted or renamed away: drop its buffers and
		// state. New/renamed networks introduce themselves via "state"
		// pushes and the buffer refresh.
		s.on("network_removed", (d) => {
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
			for (const g of gone) historyGen.current[g.key] = (historyGen.current[g.key] || 0) + 1;
			setMsgs((m) => {
				let next = m;
				for (const g of gone) next = dropBufferMsgs(next, g.key);
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
		s.on("prefs", (d) => {
			if (prefsDirty.current) return;
			if (d.prefs) setPrefs(normalizePrefs(d.prefs));
		});

		// Ephemeral server replies (/list, error numerics): shown as
		// system lines in the active buffer, never persisted — a history
		// refetch drops them.
		let infoSeq = 0;
		s.on("server_info", (d) => {
			const key = activeKeyRef.current;
			const buf = buffersRef.current[key];
			if (!buf) return;
			const text = (buf.network === d.network ? "" : `[${d.network}] `) + d.text;
			const ev = {
				id: `si${++infoSeq}`, network: buf.network, buffer: buf.buffer,
				time: Date.now(), sender: "", command: "INFO", raw: text,
			};
			setMsgs((m) => appendInfoLine(m, key, ev));
		});

		// A WHOIS card lands in the target's query buffer; jump there, so
		// /whois does not clutter the channel (The Lounge style).
		let whoisSeq = 0;
		s.on("whois", (d) => {
			const key = bufKey(d.network, d.nick);
			setBuffers((b) => (b[key] ? b : { ...b, [key]: makeBuffer(d.network, d.nick) }));
			const ev = {
				id: `wh${++whoisSeq}`, network: d.network, buffer: d.nick,
				time: Date.now(), sender: "", command: "WHOIS", raw: "", whois: d,
			};
			setMsgs((m) => appendInfoLine(m, key, ev));
			if (activeKeyRef.current !== key) select(d.network, d.nick);
		});

		s.on("presence", (d) => setMonitors((all) => applyPresenceUpdate(all, d)));

		s.on("redact", (d) => setMsgs((m) => applyRedaction(m, bufKey(d.network, d.buffer), d)));

		s.on("typing", (d) => setTypers((t) => setTypingState(t, bufKey(d.network, d.buffer), d)));

		s.on("members_changed", (d) => {
			const buf = buffersRef.current[activeKeyRef.current];
			if (
				buf && d.network === buf.network &&
				(!d.buffer || d.buffer.toLowerCase() === buf.buffer.toLowerCase())
			) {
				setChanTick((t) => t + 1);
			}
		});

		s.on("read_marker", (d) =>
			setBuffers((b) => applyMarkerState(b, bufKey(d.network, d.buffer), d.time)));

		s.connect();
		return () => s.close();
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
	// highlight. Runs whenever unread state changes.
	useEffect(() => {
		let unread = 0;
		let mention = false;
		for (const b of Object.values(buffers)) {
			unread += b.unread || 0;
			if (b.mention) mention = true;
		}
		applyBadge(unread, mention);
	}, [buffers, theme, prefs]);

	function updateRules(next) {
		setRules(next);
		saveRules(next);
	}

	// Expire stale typing states (6s active / 30s paused per spec).
	useEffect(() => {
		const t = setInterval(() => {
			setTypers((all) => {
				const now = Date.now();
				let changed = false;
				const next = {};
				for (const [key, nicks] of Object.entries(all)) {
					const keep = {};
					for (const [nick, v] of Object.entries(nicks)) {
						if (typingExpired(v.state, v.at, now)) changed = true;
						else keep[nick] = v;
					}
					if (Object.keys(keep).length) next[key] = keep;
					else if (Object.keys(nicks).length) changed = true;
				}
				return changed ? next : all;
			});
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
	useEffect(() => {
		const buf = activeKey ? buffers[activeKey] : null;
		if (!buf || !connected || !isChannelName(buf.buffer, networks[buf.network]?.chantypes)) {
			setChanInfo(null);
			return;
		}
		let alive = true;
		const t = setTimeout(() => {
			sock.current
				.request("get_channel", { network: buf.network, buffer: buf.buffer })
				.then((d) => {
					if (alive) setChanInfo(d);
				})
				.catch(() => {});
		}, 150);
		return () => {
			alive = false;
			clearTimeout(t);
		};
	}, [activeKey, connected, chanTick]);

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
				setMsgs((m) => mergeHistoryPage(m, key, page));
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
	// recently active. Runs on every activeKey change so the working set
	// stays bounded no matter how many channels a bouncer feeds; a mid-load
	// buffer is never evicted (its in-flight page would have nowhere to
	// merge). An evicted buffer reloads its tail when reopened.
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
	}, [activeKey]);

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
		// Optimistic: show it immediately (offline until the server replies).
		setMonitors((all) => {
			const list = all[network] || [];
			if (list.some((m) => m.nick === nick)) return all;
			return { ...all, [network]: [...list, { nick, online: false }].sort((a, b) => a.nick.localeCompare(b.nick)) };
		});
		sock.current?.request("monitor_add", { network, nick }).catch(() => {});
	}

	function removeMonitor(network, nick) {
		setMonitors((all) => ({ ...all, [network]: (all[network] || []).filter((m) => m.nick !== nick) }));
		sock.current?.request("monitor_remove", { network, nick }).catch(() => {});
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

	function select(network, buffer) {
		const key = bufKey(network, buffer);
		// A buffer may not exist yet (/join, /msg to a fresh target):
		// create a placeholder so the view renders while events arrive.
		setBuffers((b) => (b[key] ? b : { ...b, [key]: makeBuffer(network, buffer) }));
		// Returning to a buffer that's showing a search-jump window drops
		// it so the live tail reloads.
		setMsgs((m) => (m[key]?.atTail === false ? dropBufferMsgs(m, key) : m));
		setFocusId(null);
		location.hash = toHash(network, buffer);
		setActiveKey(key);
		setCmdError("");
		if (globalThis.innerWidth < 760) setSideOpen(false);
	}

	function sendCommand(network, command, params) {
		sock.current?.request("command", { network, command, params })
			.catch((e) => setCmdError(e.message || "failed"));
	}

	// closeBuffer removes a buffer everywhere (leave/close): the stored
	// history goes too — the sidebar is store-driven, so a buffer that
	// stays in the store resurrects on the next refresh. Other devices
	// drop it via the buffer_closed push.
	function closeBuffer(network, buffer) {
		sock.current?.request("close_buffer", { network, buffer }).catch(() => {});
		const key = bufKey(network, buffer);
		setBuffers((b) => {
			const next = { ...b };
			delete next[key];
			return next;
		});
		// Invalidate any in-flight initial get_history so it can't reinstall a
		// phantom msgs entry for the buffer we're closing.
		historyGen.current[key] = (historyGen.current[key] || 0) + 1;
		setMsgs((m) => dropBufferMsgs(m, key));
		if (activeKeyRef.current !== key) return;
		const rest = Object.values(buffersRef.current)
			.filter((b) => b.key !== key)
			.sort((a, b) => (a.network + a.buffer).localeCompare(b.network + b.buffer));
		if (rest.length) select(rest[0].network, rest[0].buffer);
		else {
			setActiveKey(null);
			location.hash = "";
		}
	}

	// editTopic selects the channel and prefills the composer with its
	// current topic for editing (sent as /topic). The topic is fetched for
	// the target directly: chanInfo tracks only the (previously) active
	// buffer, and activeKeyRef still holds the old key synchronously after
	// select(). Resolving asynchronously also lands the prefill after the
	// buffer-switch draft reset, so it is not clobbered.
	function editTopic(network, buffer) {
		select(network, buffer);
		sock.current?.request("get_channel", { network, buffer })
			.then((d) => composerApi.current?.prefill(`/topic ${d?.topic || ""}`))
			.catch(() => composerApi.current?.prefill("/topic "));
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
		const members = chanInfo?.members;
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
					label: "Leave channel", danger: true,
					onClick: () => {
						// part_channel also removes the channel from autojoin.
						sock.current?.request("part_channel", { network, channel: buffer })
							.catch(() => sendCommand(network, "PART", [buffer]));
						closeBuffer(network, buffer);
					},
				}
				: { label: "Close", danger: true, onClick: () => closeBuffer(network, buffer) },
		];
		setMenu({ x, y, title: buffer, items });
	}

	// openNetworkMenu: click/right-click on a network header row.
	function openNetworkMenu(network, x, y) {
		setMenu({
			x, y, title: network,
			items: [
				{ label: "Join channel…", onClick: () => { setChanPromptError(""); setChanPrompt({ network }); } },
				{ label: "Edit network…", onClick: () => editNetwork(network) },
				{ label: "Add network…", onClick: () => { setNetFormError(""); setNetForm({ initial: null, oldName: "" }); } },
			],
		});
	}

	function editNetwork(network) {
		sock.current?.request("get_networks", null).then((d) => {
			const n = (d.networks || []).find((x) => x.name === network);
			if (!n) return;
			setNetFormError("");
			setNetForm({ initial: n.config, oldName: network });
		}).catch(() => {});
	}

	function saveNetwork(cfg, oldName) {
		setNetFormBusy(true);
		sock.current?.request("put_network", { old_name: oldName || undefined, config: cfg })
			.then(() => {
				setNetForm(null);
				setNetFormError("");
			})
			.catch((e) => setNetFormError(e.message || "saving failed"))
			.finally(() => setNetFormBusy(false));
	}

	function deleteNetwork(name) {
		setNetFormBusy(true);
		sock.current?.request("delete_network", { network: name })
			.then(() => {
				setNetForm(null);
				setNetFormError("");
			})
			.catch((e) => setNetFormError(e.message || "deleting failed"))
			.finally(() => setNetFormBusy(false));
	}

	const pendingJoin = useRef(null);
	function joinChannel(network, channel) {
		sock.current?.request("join_channel", { network, channel })
			.then(() => {
				setChanPrompt(null);
				setChanPromptError("");
				// Select once our JOIN arrives (it may be a forward to a
				// different channel); creating the buffer now would leave a
				// phantom if the server redirects us.
				pendingJoin.current = { network, ts: Date.now() };
			})
			.catch((e) => setChanPromptError(e.message || "join failed"));
	}

	// jumpTo opens a search result: load a window centered on the message
	// and highlight it. The window is not the live tail, so incoming
	// events won't append (see the event handler).
	function jumpTo(ev) {
		const key = bufKey(ev.network, ev.buffer);
		setSearchOpen(false);
		// Invalidate any in-flight initial history load for this buffer so
		// its resolve is discarded (see the load effect's gen check) instead
		// of merging the live tail into — and flipping atTail true on — the
		// around-window we install below, which would leave a temporal gap.
		historyGen.current[key] = (historyGen.current[key] || 0) + 1;
		const gen = historyGen.current[key];
		sock.current
			?.request("get_history", {
				network: ev.network, buffer: ev.buffer,
				around: { ts: ev.time, id: ev.id }, limit: PAGE,
			})
			.then((page) => {
				// The buffer was closed/invalidated (e.g. buffer_closed from
				// another device) while this was in flight — don't recreate a
				// ghost buffer, mirroring the initial load and loadOlder guards.
				if ((historyGen.current[key] || 0) !== gen) return;
				setBuffers((b) => (b[key] ? b : { ...b, [key]: makeBuffer(ev.network, ev.buffer) }));
				setMsgs((m) => ({
					...m,
					[key]: { list: page.messages || [], loaded: true, reachedTop: false, atTail: false },
				}));
				location.hash = toHash(ev.network, ev.buffer);
				setActiveKey(key);
				setFocusId(ev.id);
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
					let list = mergeById(prev.list, older);
					// Bound memory on the paging-back path too: keep the
					// oldest TRIM_TO (we are scrolled up), dropping the
					// newest tail — it reloads on scroll-down / new events.
					let atTail = prev.atTail;
					if (list.length > TRIM_AT) {
						list = list.slice(0, TRIM_TO);
						atTail = false;
					}
					return {
						...m,
						[key]: {
							...prev,
							list,
							reachedTop: older.length < PAGE,
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
		setMsgs((m) => (m[activeKey]?.atTail === false ? dropBufferMsgs(m, activeKey) : m));
	}

	// sendInput returns a promise that resolves when the send is accepted and
	// rejects on a parse error or a rejected request, so the composer can
	// keep the user's text on failure instead of dropping it.
	function sendInput(text) {
		const buf = buffers[activeKey];
		if (!buf) return Promise.reject(new Error("no active buffer"));
		setCmdError("");
		const p = parseInput(text, buf.buffer, networks[buf.network]?.chantypes);
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
			case "msg":
				return sock.current
					.request("send", { network: buf.network, target: p.target, text: p.text })
					.then(() => select(buf.network, p.target))
					.catch(oops);
			case "cmd":
				return sock.current
					.request("command", { network: buf.network, command: p.command, params: p.params })
					.then(() => {
						if (p.switchTo) select(buf.network, p.switchTo);
					})
					.catch(oops);
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
	const topicText = topicFor(activeBuf, netState, chanInfo);
	const ignoredHere = activeBuf ? ignores[activeBuf.network] || [] : [];
	const mutedSet = new Set(mutes);
	const timeFmt = { clock: prefs.clock, seconds: prefs.seconds, ampm: prefs.ampm };

	return (
		<div class="app">
			<div class={"sidebar" + (sideOpen ? " open" : "")}>
				<Sidebar
					networks={networks} buffers={buffers} activeKey={activeKey}
					monitors={monitors} theme={theme} mutedSet={mutedSet} onSelect={select}
					onSettings={() => setSettingsOpen(true)}
					onBufferMenu={openBufferMenu} onNetworkMenu={openNetworkMenu}
					onAddNetwork={() => { setNetFormError(""); setNetForm({ initial: null, oldName: "" }); }}
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
						typers={Object.keys(typers[activeKey] || {})}
						focusId={focusId}
						completionNicks={isChan
							? (chanInfo?.members || []).map((m) => m.nick)
							: [activeBuf.buffer]}
						ignoredNicks={ignoredHere}
						statusMode={prefs.statusMsgs}
						timeFmt={timeFmt} nickSep={prefs.nickSep} previews={previews}
						composerApi={composerApi}
						onNick={(nick, x, y) => openUserMenu(activeBuf.network, nick, x, y)}
						isHighlight={(t) => highlightText(t, selfNick, rules, activeBuf.network)}
						onRedact={(msgid) =>
							sock.current?.request("redact", {
								network: activeBuf.network, buffer: activeBuf.buffer, msgid,
							}).catch((e) => setCmdError(e.message || "delete failed"))}
						onSend={sendInput} onLoadOlder={loadOlder} onReloadTail={reloadTail} onRead={markRead}
						onTyping={(state, net, bufName) =>
							sock.current?.notify("typing", {
								network: net ?? activeBuf.network, buffer: bufName ?? activeBuf.buffer, state,
							})}
					/>
				) : (
					<div class="empty-state">no buffers yet — waiting for traffic</div>
				)}
			</div>
			{isChan && rightOpen && <div class="right-scrim" aria-hidden="true" onClick={() => setRightOpen(false)} />}
			{isChan && (
				<div class={"rightbar" + (rightOpen ? " open" : "")}>
					<Members
							info={chanInfo} theme={theme} ignoredNicks={ignoredHere}
							onNick={(nick, x, y) => openUserMenu(activeBuf.network, nick, x, y)}
						/>
				</div>
			)}
			{searchOpen && (
				<SearchOverlay sock={sock} onJump={jumpTo} onClose={() => setSearchOpen(false)} timeFmt={timeFmt} nickSep={prefs.nickSep} />
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
					prefs={prefs} onPrefs={updatePrefs} onPreviews={setPreviews}
					notifier={notifier.current} onClose={() => setSettingsOpen(false)}
				/>
			)}
			{netForm && (
				<NetworkForm
					key={netForm.oldName || "new"}
					initial={netForm.initial} oldName={netForm.oldName}
					error={netFormError} busy={netFormBusy}
					onSave={saveNetwork} onDelete={deleteNetwork}
					onClose={() => setNetForm(null)}
				/>
			)}
			{chanPrompt && (
				<ChannelPrompt
					network={chanPrompt.network} error={chanPromptError}
					chantypes={networks[chanPrompt.network]?.chantypes}
					onJoin={joinChannel} onClose={() => setChanPrompt(null)}
				/>
			)}
			<ContextMenu menu={menu} onClose={() => setMenu(null)} />
		</div>
	);
}
