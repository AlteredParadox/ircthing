import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { Chat } from "./chat.jsx";
import { bufKey, mentionsMe, parseHash, renderable, toHash } from "./irc.js";
import { Login } from "./login.jsx";
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

		s.on("_open", async () => {
			setConnected(true);
			try {
				const data = await s.request("get_buffers", null);
				const nets = {};
				for (const n of data.networks || []) nets[n.name] = { state: n.state, nick: n.nick };
				setNetworks(nets);
				const bufs = {};
				for (const b of data.buffers || []) {
					bufs[bufKey(b.network, b.buffer)] = {
						key: bufKey(b.network, b.buffer),
						network: b.network, buffer: b.buffer,
						lastTime: b.last_time, marker: b.marker,
						unread: b.unread, mention: false,
					};
				}
				setBuffers(bufs);
				// History may have advanced while we were away; refetch the
				// open buffer.
				setMsgs({});
			} catch {
				/* reconnect will retry */
			}
		});
		s.on("_close", () => setConnected(false));

		s.on("state", (d) => {
			setNetworks((n) => ({ ...n, [d.network]: { ...(n[d.network] || {}), state: d.state } }));
		});

		s.on("event", (ev) => {
			const key = bufKey(ev.network, ev.buffer);
			setMsgs((m) => {
				const cur = m[key];
				if (!cur?.loaded) return m; // not fetched yet: history covers it
				let list = [...cur.list, ev];
				if (list.length > TRIM_AT) {
					list = list.slice(list.length - TRIM_TO);
				}
				return { ...m, [key]: { ...cur, list, reachedTop: list.length <= TRIM_AT && cur.reachedTop } };
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

	// networksRef lets the event handler read the current nick without
	// re-registering socket handlers on every state change.
	const networksRef = useRef(networks);
	networksRef.current = networks;

	// Load history when a buffer becomes active and has none.
	useEffect(() => {
		if (!activeKey || !connected) return;
		const buf = buffers[activeKey];
		if (!buf || msgs[activeKey]?.loaded) return;
		sock.current
			.request("get_history", { network: buf.network, buffer: buf.buffer, limit: PAGE })
			.then((page) => {
				setMsgs((m) => ({
					...m,
					[activeKey]: {
						list: page.messages || [],
						loaded: true,
						reachedTop: (page.messages || []).length < PAGE,
					},
				}));
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
		location.hash = toHash(network, buffer);
		setActiveKey(bufKey(network, buffer));
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

	function sendMsg(text) {
		const buf = buffers[activeKey];
		if (!buf) return;
		sock.current.request("send", { network: buf.network, target: buf.buffer, text }).catch(() => {});
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
							<span class="hash">{activeBuf.buffer.startsWith("#") || activeBuf.buffer.startsWith("&") ? activeBuf.buffer[0] : "@"}</span>
							{activeBuf.buffer.replace(/^[#&]/, "")}
						</span>
					)}
					<div class="topic-sep" />
					<div class="topic-text">
						{netState && netState !== "registered" ? `${activeBuf?.network}: ${netState}…` : ""}
					</div>
					<button
						class="icon-btn" title="Toggle theme"
						onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
					>{theme === "dark" ? "☀" : "☾"}</button>
				</div>
				{!connected && <div class="conn-banner">connection lost — reconnecting…</div>}
				{activeBuf ? (
					<Chat
						buf={activeBuf} msgs={msgs[activeKey]} selfNick={selfNick} theme={theme}
						connected={connected && netState === "registered"}
						onSend={sendMsg} onLoadOlder={loadOlder} onRead={markRead}
					/>
				) : (
					<div class="empty-state">no buffers yet — waiting for traffic</div>
				)}
			</div>
		</div>
	);
}
