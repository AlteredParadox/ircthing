import { useEffect, useRef, useState } from "preact/hooks";
import { Chat } from "./chat.jsx";
import { bufferOrder, bufKey, isChannelName, parseHash, parseInput, renderable, toHash, typingExpired } from "./irc.js";
import { applyBadge, highlightText, loadRules, Notifier, saveRules } from "./notify.js";
import { Login } from "./login.jsx";
import { applyPrefs, loadPrefs, normalizePrefs, resolveTheme, savePrefs } from "./prefs.js";
import { isIgnored, isMuted, loadIgnores, loadMutes, toggleIgnore, toggleMute } from "./local.js";
import { ContextMenu } from "./menu.jsx";
import { Members } from "./members.jsx";
import { SearchOverlay } from "./search.jsx";
import { Settings } from "./settings.jsx";
import { Sidebar } from "./sidebar.jsx";
import { Switcher } from "./switcher.jsx";
import { Socket } from "./ws.js";

const PAGE = 100;
// Memory bound per buffer: trim to TRIM_TO once past TRIM_AT. Older pages
// are always refetchable, so trimming loses nothing durable. The list is
// virtualized, so these bound JS memory, not DOM size.
const TRIM_AT = 50000;
const TRIM_TO = 25000;

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
					<span class="hash">{isChan ? activeBuf.buffer[0] : "@"}</span>
					{activeBuf.buffer.replace(/^[#&]/, "")}
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

function makeBuffer(network, buffer) {
	return { key: bufKey(network, buffer), network, buffer, lastTime: 0, marker: 0, unread: 0, mention: false };
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
function appendEventMsgs(m, key, ev) {
	const cur = m[key];
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

// appendInfoLine appends an ephemeral server_info system line.
function appendInfoLine(m, key, ev) {
	const cur = m[key];
	if (cur?.loaded && cur.atTail === false) return m;
	return { ...m, [key]: { ...cur, list: [...(cur?.list || []), ev] } };
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
		return { ...ev, redacted: true, redact_reason: d.reason };
	});
	return hit ? { ...m, [key]: { ...cur, list } } : m;
}

// mergeHistoryPage installs a fetched page, keeping events that streamed
// in while the fetch was in flight (the page is authoritative for what
// it covers).
function mergeHistoryPage(m, key, page) {
	const pageMsgs = page.messages || [];
	const seen = new Set(pageMsgs.map((e) => e.id));
	const streamed = (m[key]?.list || []).filter((e) => !seen.has(e.id));
	return {
		...m,
		[key]: {
			list: [...pageMsgs, ...streamed],
			loaded: true,
			reachedTop: pageMsgs.length < PAGE,
			atTail: true,
		},
	};
}

export function App() {
	const [phase, setPhase] = useState("checking"); // checking | login | app
	const [connected, setConnected] = useState(false);
	const [networks, setNetworks] = useState({});
	const [buffers, setBuffers] = useState({});
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
	function updatePrefs(next) {
		setPrefs(next);
		// Debounced: the custom-CSS textarea changes on every keystroke.
		clearTimeout(prefsPush.current);
		prefsPush.current = setTimeout(() => {
			sock.current?.request("set_prefs", { prefs: next }).catch(() => {});
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
		setBuffers((prev) => {
			const bufs = {};
			for (const b of data.buffers || []) {
				const key = bufKey(b.network, b.buffer);
				bufs[key] = {
					key, network: b.network, buffer: b.buffer,
					lastTime: b.last_time, marker: b.marker,
					unread: b.unread,
					mention: prev[key]?.mention || false,
				};
			}
			return bufs;
		});
	}

	function refreshBuffers() {
		sock.current.request("get_buffers", null).then(applyBuffers).catch(() => {});
	}

	// adoptPrefs applies the server's prefs; a server with none stored
	// yet (fresh install, pre-sync upgrade) is seeded from this
	// browser's cache.
	function adoptPrefs(d) {
		if (d.prefs) setPrefs(normalizePrefs(d.prefs));
		else sock.current.request("set_prefs", { prefs: prefsRef.current }).catch(() => {});
	}

	// Socket lifecycle, once authed.
	useEffect(() => {
		if (phase !== "app") return;
		const s = new Socket(wsURL());
		sock.current = s;

		s.on("_open", async () => {
			setConnected(true);
			s.request("get_prefs", null).then(adoptPrefs).catch(() => {});
			try {
				applyBuffers(await s.request("get_buffers", null));
				// History may have advanced while we were away; refetch the
				// open buffer.
				setMsgs({});
			} catch {
				/* reconnect will retry */
			}
		});
		s.on("_close", () => setConnected(false));

		// Chathistory backfill rewrote a buffer's history: drop cached
		// pages (the active buffer refetches automatically) and refresh
		// sidebar counts, debounced across a burst of backfills.
		let bufRefresh;
		s.on("history_changed", (d) => {
			const key = bufKey(d.network, d.buffer);
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
			setMsgs((m) => appendEventMsgs(m, key, ev));
			// A message from someone clears their typing indicator.
			setTypers((t) => clearTyperFor(t, key, ev.sender));
			const r = renderable(ev);
			const isMsg = r.kind !== "system";
			const nick = networksRef.current[ev.network]?.nick;
			const mine = nick && ev.sender === nick;
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

		// Another device changed prefs; adopt without echoing back.
		s.on("prefs", (d) => {
			if (d.prefs) setPrefs(normalizePrefs(d.prefs));
		});

		// Ephemeral server replies (/whois, /list, error numerics): shown
		// as system lines in the active buffer, never persisted — a
		// history refetch drops them.
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

	// Load history when a buffer becomes active and has none.
	useEffect(() => {
		if (!activeKey || !connected) return;
		const buf = buffers[activeKey];
		if (!buf || msgs[activeKey]?.loaded) return;
		sock.current
			.request("get_history", { network: buf.network, buffer: buf.buffer, limit: PAGE })
			.then((page) => setMsgs((m) => mergeHistoryPage(m, activeKey, page)))
			.catch(() => {});
	}, [activeKey, connected, buffers, msgs]);

	// Default to the first buffer once the sidebar is known.
	useEffect(() => {
		if (activeKey || !Object.keys(buffers).length) return;
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

	// closeBuffer removes a buffer from the client (leave/close); if it was
	// active, moves to the first remaining buffer.
	function closeBuffer(network, buffer) {
		const key = bufKey(network, buffer);
		setBuffers((b) => {
			const next = { ...b };
			delete next[key];
			return next;
		});
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
	// current topic for editing (sent as /topic).
	function editTopic(network, buffer) {
		select(network, buffer);
		const active = activeKeyRef.current === bufKey(network, buffer);
		const topic = active ? chanInfo?.topic || "" : "";
		composerApi.current?.prefill(`/topic ${topic}`);
	}

	// openUserMenu: the right-click menu for a nick (member list, message).
	function openUserMenu(network, nick, x, y) {
		if (!nick) return;
		const ignored = isIgnored(ignoresRef.current, network, nick);
		setMenu({
			x, y, title: nick,
			items: [
				{ label: "Whois", onClick: () => sendCommand(network, "WHOIS", [nick]) },
				{ label: "Direct message", onClick: () => select(network, nick) },
				{
					label: ignored ? "Unignore" : "Ignore", danger: !ignored,
					onClick: () => setIgnores((ig) => toggleIgnore(ig, network, nick)),
				},
			],
		});
	}

	// openBufferMenu: the right-click menu for a sidebar row — channel
	// actions for channels, DM actions for query buffers.
	function openBufferMenu(network, buffer, x, y) {
		const key = bufKey(network, buffer);
		const muted = isMuted(mutesRef.current, key);
		const chan = isChannelName(buffer, networksRef.current[network]?.chantypes);
		const items = [];
		if (chan) {
			items.push({ label: "Edit topic", onClick: () => editTopic(network, buffer) });
		} else {
			const ig = isIgnored(ignoresRef.current, network, buffer);
			items.push({ label: "Whois", onClick: () => sendCommand(network, "WHOIS", [buffer]) });
			items.push({
				label: ig ? "Unignore" : "Ignore", danger: !ig,
				onClick: () => setIgnores((x2) => toggleIgnore(x2, network, buffer)),
			});
		}
		items.push({
			label: muted ? "Unmute" : "Mute",
			onClick: () => setMutes((m) => toggleMute(m, key)),
		});
		items.push(chan
			? {
				label: "Leave channel", danger: true,
				onClick: () => {
					sendCommand(network, "PART", [buffer]);
					closeBuffer(network, buffer);
				},
			}
			: { label: "Close", danger: true, onClick: () => closeBuffer(network, buffer) });
		setMenu({ x, y, title: buffer, items });
	}

	// jumpTo opens a search result: load a window centered on the message
	// and highlight it. The window is not the live tail, so incoming
	// events won't append (see the event handler).
	function jumpTo(ev) {
		const key = bufKey(ev.network, ev.buffer);
		setSearchOpen(false);
		sock.current
			?.request("get_history", {
				network: ev.network, buffer: ev.buffer,
				around: { ts: ev.time, id: ev.id }, limit: PAGE,
			})
			.then((page) => {
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
		const oldest = cur.list[0];
		return sock.current
			.request("get_history", {
				network: buf.network, buffer: buf.buffer,
				before: { ts: oldest.time, id: oldest.id }, limit: PAGE,
			})
			.then((page) => {
				const older = page.messages || [];
				setMsgs((m) => ({
					...m,
					[activeKey]: {
						...m[activeKey],
						list: [...older, ...m[activeKey].list],
						reachedTop: older.length < PAGE,
					},
				}));
			})
			.catch(() => {});
	}

	function sendInput(text) {
		const buf = buffers[activeKey];
		if (!buf) return;
		setCmdError("");
		const p = parseInput(text, buf.buffer, networks[buf.network]?.chantypes);
		const oops = (e) => setCmdError(e.message || "failed");
		switch (p.type) {
			case "error":
				setCmdError(p.message);
				break;
			case "text":
				sock.current
					.request("send", { network: buf.network, target: buf.buffer, text: p.text })
					.catch(oops);
				break;
			case "msg":
				sock.current
					.request("send", { network: buf.network, target: p.target, text: p.text })
					.then(() => select(buf.network, p.target))
					.catch(oops);
				break;
			case "cmd":
				sock.current
					.request("command", { network: buf.network, command: p.command, params: p.params })
					.then(() => {
						if (p.switchTo) select(buf.network, p.switchTo);
					})
					.catch(oops);
				break;
		}
	}

	const readSent = useRef(0);
	function markRead(time) {
		const buf = buffers[activeKey];
		if (!buf || time <= buf.marker || time === readSent.current) return;
		readSent.current = time;
		sock.current
			.request("set_read_marker", { network: buf.network, buffer: buf.buffer, time })
			.then((d) => setBuffers((b) => applyMarkerState(b, activeKey, d.time)))
			.catch(() => {});
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

	return (
		<div class="app">
			<div class={"sidebar" + (sideOpen ? " open" : "")}>
				<Sidebar
					networks={networks} buffers={buffers} activeKey={activeKey}
					monitors={monitors} theme={theme} mutedSet={mutedSet} onSelect={select}
					onSettings={() => setSettingsOpen(true)}
					onBufferMenu={openBufferMenu}
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
						composerApi={composerApi}
						onNick={(nick, x, y) => openUserMenu(activeBuf.network, nick, x, y)}
						isHighlight={(t) => highlightText(t, selfNick, rules, activeBuf.network)}
						onRedact={(msgid) =>
							sock.current?.request("redact", {
								network: activeBuf.network, buffer: activeBuf.buffer, msgid,
							}).catch((e) => setCmdError(e.message || "delete failed"))}
						onSend={sendInput} onLoadOlder={loadOlder} onRead={markRead}
						onTyping={(state) =>
							sock.current?.notify("typing", {
								network: activeBuf.network, buffer: activeBuf.buffer, state,
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
				<SearchOverlay sock={sock} onJump={jumpTo} onClose={() => setSearchOpen(false)} />
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
					prefs={prefs} onPrefs={updatePrefs}
					notifier={notifier.current} onClose={() => setSettingsOpen(false)}
				/>
			)}
			<ContextMenu menu={menu} onClose={() => setMenu(null)} />
		</div>
	);
}
