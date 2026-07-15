import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { Chat } from "./chat.jsx";
import { bufKey, isChannelName, parseHash, parseInput, renderable, toHash, typingExpired } from "./irc.js";
import { applyBadge, highlightText, loadRules, Notifier, saveRules } from "./notify.js";
import { Login } from "./login.jsx";
import { applyPrefs, loadPrefs, normalizePrefs, resolveTheme, savePrefs } from "./prefs.js";
import { Members } from "./members.jsx";
import { SearchOverlay } from "./search.jsx";
import { Settings } from "./settings.jsx";
import { Sidebar } from "./sidebar.jsx";
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
		() => window.matchMedia("(prefers-color-scheme: dark)").matches,
	);
	const theme = resolveTheme(prefs.theme, sysDark);
	const [sideOpen, setSideOpen] = useState(() => window.innerWidth >= 760);
	const [rightOpen, setRightOpen] = useState(() => window.innerWidth >= 1000);
	const [chanInfo, setChanInfo] = useState(null);
	const [chanTick, setChanTick] = useState(0);
	const [cmdError, setCmdError] = useState("");
	const [searchOpen, setSearchOpen] = useState(false);
	const [settingsOpen, setSettingsOpen] = useState(false);
	const [focusId, setFocusId] = useState(null);
	// monitors: network -> [{nick, online}]; the MONITOR buddy list.
	const [monitors, setMonitors] = useState({});
	const [rules, setRules] = useState(loadRules);
	const rulesRef = useRef(rules);
	rulesRef.current = rules;
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
		const mq = window.matchMedia("(prefers-color-scheme: dark)");
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
		window.addEventListener("hashchange", onHash);
		return () => window.removeEventListener("hashchange", onHash);
	}, []);

	// Ctrl/Cmd+K opens search.
	useEffect(() => {
		const onKey = (e) => {
			if ((e.ctrlKey || e.metaKey) && e.key === "k") {
				e.preventDefault();
				setSearchOpen(true);
			}
		};
		window.addEventListener("keydown", onKey);
		return () => window.removeEventListener("keydown", onKey);
	}, []);

	// Socket lifecycle, once authed.
	useEffect(() => {
		if (phase !== "app") return;
		const s = new Socket(wsURL());
		sock.current = s;

		const applyBuffers = (data) => {
			const nets = {};
			for (const n of data.networks || []) nets[n.name] = { state: n.state, nick: n.nick };
			setNetworks(nets);
			// Load each network's MONITOR buddy list with current presence.
			for (const name of Object.keys(nets)) {
				s.request("get_monitors", { network: name })
					.then((md) => setMonitors((all) => ({ ...all, [name]: md.monitors || [] })))
					.catch(() => {});
			}
			setBuffers((prev) => {
				const bufs = {};
				for (const b of data.buffers || []) {
					const key = bufKey(b.network, b.buffer);
					bufs[key] = {
						key, network: b.network, buffer: b.buffer,
						lastTime: b.last_time, marker: b.marker,
						unread: b.unread,
						// The mention flag is client-side state; keep it.
						mention: prev[key]?.mention || false,
					};
				}
				return bufs;
			});
		};

		s.on("_open", async () => {
			setConnected(true);
			// Adopt the server's prefs; a server with none stored yet (fresh
			// install, pre-sync upgrade) is seeded from this browser's cache.
			s.request("get_prefs", null)
				.then((d) => {
					if (d.prefs) setPrefs(normalizePrefs(d.prefs));
					else s.request("set_prefs", { prefs: prefsRef.current }).catch(() => {});
				})
				.catch(() => {});
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
			setMsgs((m) => {
				if (!m[key]) return m;
				const next = { ...m };
				delete next[key];
				return next;
			});
			clearTimeout(bufRefresh);
			bufRefresh = setTimeout(() => {
				s.request("get_buffers", null).then(applyBuffers).catch(() => {});
			}, 300);
		});

		s.on("state", (d) => {
			setNetworks((n) => ({ ...n, [d.network]: { ...(n[d.network] || {}), state: d.state } }));
		});

		s.on("event", (ev) => {
			const key = bufKey(ev.network, ev.buffer);
			setMsgs((m) => {
				const cur = m[key];
				// When a buffer is showing a search-jump window (not the
				// live tail), appending would leave a temporal gap — skip
				// it; the message is on disk and appears when the user
				// returns to the tail.
				if (cur?.loaded && cur.atTail === false) return m;
				// Accumulate even before history is loaded — the fetch
				// response merges and dedupes, so events racing an
				// in-flight history request are never lost.
				let list = [...(cur?.list || []), ev];
				let reachedTop = cur?.reachedTop;
				if (list.length > TRIM_AT) {
					list = list.slice(list.length - TRIM_TO);
					reachedTop = false;
				}
				return { ...m, [key]: { ...(cur || {}), list, reachedTop } };
			});
			// A message from someone clears their typing indicator.
			setTypers((t) => {
				if (!t[key]?.[ev.sender]) return t;
				const cur = { ...t[key] };
				delete cur[ev.sender];
				return { ...t, [key]: cur };
			});
			const r = renderable(ev);
			const isMsg = r.kind !== "system";
			const nick = networksRef.current[ev.network]?.nick;
			const mine = nick && ev.sender === nick;
			// Highlight = a mention/keyword in a channel, or any message in
			// a query (PM) buffer. PMs always alert.
			const isChan = isChannelName(ev.buffer);
			const highlight = isMsg && !mine &&
				(!isChan ? true : highlightText(r.text, nick, rulesRef.current, ev.network));

			setBuffers((b) => {
				const cur = b[key] || {
					key, network: ev.network, buffer: ev.buffer,
					lastTime: 0, marker: 0, unread: 0, mention: false,
				};
				return {
					...b,
					[key]: {
						...cur,
						lastTime: ev.time,
						unread: isMsg && !mine ? cur.unread + 1 : cur.unread,
						mention: cur.mention || highlight,
					},
				};
			});

			// Desktop notification when a highlight lands somewhere the user
			// isn't looking (tab hidden, or a different buffer active).
			if (highlight && (document.hidden || key !== activeKeyRef.current)) {
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

		s.on("presence", (d) => {
			setMonitors((all) => {
				const list = all[d.network] || [];
				const idx = list.findIndex((m) => m.nick === d.nick);
				if (idx === -1) return all; // not (or no longer) in the list
				const next = list.slice();
				next[idx] = { ...next[idx], online: d.online };
				return { ...all, [d.network]: next };
			});
		});

		s.on("redact", (d) => {
			const key = bufKey(d.network, d.buffer);
			setMsgs((m) => {
				const cur = m[key];
				if (!cur?.list) return m;
				let hit = false;
				const list = cur.list.map((ev) => {
					if (ev.msgid !== d.msgid || ev.redacted) return ev;
					hit = true;
					return { ...ev, redacted: true, redact_reason: d.reason };
				});
				return hit ? { ...m, [key]: { ...cur, list } } : m;
			});
		});

		s.on("typing", (d) => {
			const key = bufKey(d.network, d.buffer);
			setTypers((t) => {
				const cur = { ...(t[key] || {}) };
				if (d.state === "done") delete cur[d.nick];
				else cur[d.nick] = { state: d.state, at: Date.now() };
				return { ...t, [key]: cur };
			});
		});

		s.on("members_changed", (d) => {
			const buf = buffersRef.current[activeKeyRef.current];
			if (
				buf && d.network === buf.network &&
				(!d.buffer || d.buffer.toLowerCase() === buf.buffer.toLowerCase())
			) {
				setChanTick((t) => t + 1);
			}
		});

		s.on("read_marker", (d) => {
			const key = bufKey(d.network, d.buffer);
			setBuffers((b) => {
				const cur = b[key];
				if (!cur) return b;
				const cleared = d.time >= cur.lastTime;
				return {
					...b,
					[key]: {
						...cur, marker: d.time,
						unread: cleared ? 0 : cur.unread,
						mention: cleared ? false : cur.mention,
					},
				};
			});
		});

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
		if (!buf || !connected || !isChannelName(buf.buffer)) {
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
			.then((page) => {
				setMsgs((m) => {
					// Keep events that streamed in while the fetch was in
					// flight; the page is authoritative for what it covers.
					const pageMsgs = page.messages || [];
					const seen = new Set(pageMsgs.map((e) => e.id));
					const streamed = (m[activeKey]?.list || []).filter((e) => !seen.has(e.id));
					return {
						...m,
						[activeKey]: {
							list: [...pageMsgs, ...streamed],
							loaded: true,
							reachedTop: pageMsgs.length < PAGE,
							atTail: true,
						},
					};
				});
			})
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

	function makeBuffer(network, buffer) {
		return { key: bufKey(network, buffer), network, buffer, lastTime: 0, marker: 0, unread: 0, mention: false };
	}

	function select(network, buffer) {
		const key = bufKey(network, buffer);
		// A buffer may not exist yet (/join, /msg to a fresh target):
		// create a placeholder so the view renders while events arrive.
		setBuffers((b) => (b[key] ? b : { ...b, [key]: makeBuffer(network, buffer) }));
		// Returning to a buffer that's showing a search-jump window drops
		// it so the live tail reloads.
		setMsgs((m) => {
			if (m[key] && m[key].atTail === false) {
				const next = { ...m };
				delete next[key];
				return next;
			}
			return m;
		});
		setFocusId(null);
		location.hash = toHash(network, buffer);
		setActiveKey(key);
		setCmdError("");
		if (window.innerWidth < 760) setSideOpen(false);
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
		const p = parseInput(text, buf.buffer);
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
			.then((d) => {
				setBuffers((b) => b[activeKey]
					? {
						...b,
						[activeKey]: {
							...b[activeKey], marker: d.time,
							unread: d.time >= b[activeKey].lastTime ? 0 : b[activeKey].unread,
							mention: d.time >= b[activeKey].lastTime ? false : b[activeKey].mention,
						},
					}
					: b);
			})
			.catch(() => {});
	}

	if (phase === "checking") return null;
	if (phase === "login") return <Login onLogin={() => setPhase("app")} />;

	const activeBuf = activeKey ? buffers[activeKey] : null;
	const selfNick = activeBuf ? networks[activeBuf.network]?.nick : "";
	const netState = activeBuf ? networks[activeBuf.network]?.state : "";
	const isChan = activeBuf && isChannelName(activeBuf.buffer);
	const topicText =
		netState && netState !== "registered"
			? `${activeBuf?.network}: ${netState}…`
			: chanInfo?.topic || "";

	return (
		<div class="app">
			<div class={"sidebar" + (sideOpen ? " open" : "")}>
				<Sidebar
					networks={networks} buffers={buffers} activeKey={activeKey}
					monitors={monitors} theme={theme} onSelect={select}
					onSettings={() => setSettingsOpen(true)}
					onAddMonitor={addMonitor} onRemoveMonitor={removeMonitor}
				/>
			</div>
			{sideOpen && <div class="side-scrim" onClick={() => setSideOpen(false)} />}
			<div class="main">
				<div class="topbar">
					<button
						class={"icon-btn" + (sideOpen ? " active" : "")}
						title="Toggle channels"
						onClick={() => setSideOpen(!sideOpen)}
					>◧</button>
					{activeBuf && (
						<span class="topic-name">
							<span class="hash">{isChan ? activeBuf.buffer[0] : "@"}</span>
							{activeBuf.buffer.replace(/^[#&]/, "")}
						</span>
					)}
					<div class="topic-sep" />
					<div class="topic-text" title={topicText}>{topicText}</div>
					<button
						class="icon-btn" title="Search (Ctrl+K)"
						onClick={() => setSearchOpen(true)}
					>⌕</button>
					<button
						class="icon-btn" title="Toggle theme"
						onClick={() => updatePrefs({ ...prefs, theme: theme === "dark" ? "light" : "dark" })}
					>{theme === "dark" ? "☀" : "☾"}</button>
					{isChan && (
						<button
							class={"icon-btn" + (rightOpen ? " active" : "")}
							title="Toggle members"
							onClick={() => setRightOpen(!rightOpen)}
						>◨</button>
					)}
				</div>
				{!connected && <div class="conn-banner">connection lost — reconnecting…</div>}
				{activeBuf ? (
					<Chat
						buf={activeBuf} msgs={msgs[activeKey]} selfNick={selfNick} theme={theme}
						connected={connected && netState === "registered"}
						error={cmdError}
						typers={Object.keys(typers[activeKey] || {})}
						focusId={focusId}
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
			{isChan && rightOpen && <div class="right-scrim" onClick={() => setRightOpen(false)} />}
			{isChan && (
				<div class={"rightbar" + (rightOpen ? " open" : "")}>
					<Members info={chanInfo} theme={theme} />
				</div>
			)}
			{searchOpen && (
				<SearchOverlay sock={sock} onJump={jumpTo} onClose={() => setSearchOpen(false)} />
			)}
			{settingsOpen && (
				<Settings
					networks={networks} rules={rules} onRules={updateRules}
					prefs={prefs} onPrefs={updatePrefs}
					notifier={notifier.current} onClose={() => setSettingsOpen(false)}
				/>
			)}
		</div>
	);
}
