import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { Chat } from "./chat.jsx";
import { bufKey, isChannelName, mentionsMe, parseHash, parseInput, renderable, toHash, typingExpired } from "./irc.js";
import { Login } from "./login.jsx";
import { Members } from "./members.jsx";
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
	const [theme, setTheme] = useState(() => localStorage.getItem("theme") || "dark");
	const [sideOpen, setSideOpen] = useState(() => window.innerWidth >= 760);
	const [rightOpen, setRightOpen] = useState(() => window.innerWidth >= 1000);
	const [chanInfo, setChanInfo] = useState(null);
	const [chanTick, setChanTick] = useState(0);
	const [cmdError, setCmdError] = useState("");
	// typers: bufKey -> { nick: { state, at } }; ephemeral, never stored.
	const [typers, setTypers] = useState({});
	const sock = useRef(null);

	useEffect(() => {
		document.documentElement.dataset.theme = theme;
		localStorage.setItem("theme", theme);
	}, [theme]);

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

	// Socket lifecycle, once authed.
	useEffect(() => {
		if (phase !== "app") return;
		const s = new Socket(wsURL());
		sock.current = s;

		const applyBuffers = (data) => {
			const nets = {};
			for (const n of data.networks || []) nets[n.name] = { state: n.state, nick: n.nick };
			setNetworks(nets);
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
				// Accumulate even before history is loaded — the fetch
				// response merges and dedupes, so events racing an
				// in-flight history request are never lost.
				const cur = m[key];
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
			setBuffers((b) => {
				const cur = b[key] || {
					key, network: ev.network, buffer: ev.buffer,
					lastTime: 0, marker: 0, unread: 0, mention: false,
				};
				const r = renderable(ev);
				const isMsg = r.kind !== "system";
				const nick = networksRef.current[ev.network]?.nick;
				const mine = nick && ev.sender === nick;
				return {
					...b,
					[key]: {
						...cur,
						lastTime: ev.time,
						unread: isMsg && !mine ? cur.unread + 1 : cur.unread,
						mention: cur.mention || (isMsg && !mine && mentionsMe(r.text, nick)),
					},
				};
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
	const networksRef = useRef(networks);
	networksRef.current = networks;
	const buffersRef = useRef(buffers);
	buffersRef.current = buffers;
	const activeKeyRef = useRef(activeKey);
	activeKeyRef.current = activeKey;

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

	function select(network, buffer) {
		const key = bufKey(network, buffer);
		// A buffer may not exist yet (/join, /msg to a fresh target):
		// create a placeholder so the view renders while events arrive.
		setBuffers((b) =>
			b[key] ? b : {
				...b,
				[key]: { key, network, buffer, lastTime: 0, marker: 0, unread: 0, mention: false },
			});
		location.hash = toHash(network, buffer);
		setActiveKey(key);
		setCmdError("");
		if (window.innerWidth < 760) setSideOpen(false);
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
					theme={theme} onSelect={select}
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
						class="icon-btn" title="Toggle theme"
						onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
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
			{isChan && (
				<div class={"rightbar" + (rightOpen ? " open" : "")}>
					<Members info={chanInfo} theme={theme} />
				</div>
			)}
		</div>
	);
}
