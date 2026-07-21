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

// Pure IRC/display helpers — no DOM, no Preact, so node:test can run them
// directly.

// parseLine splits a raw IRC line into tags, prefix, command and params.
// Minimal client-side parser: the server already validated the line; this
// only recovers display fields from the stored raw form.
export function parseLine(raw) {
	let rest = raw;
	const out = { tags: {}, prefix: null, command: "", params: [] };

	if (rest.startsWith("@")) {
		const sp = rest.indexOf(" ");
		for (const tag of rest.slice(1, sp).split(";")) {
			const eq = tag.indexOf("=");
			if (eq === -1) out.tags[tag] = "";
			else out.tags[tag.slice(0, eq)] = tag.slice(eq + 1);
		}
		rest = rest.slice(sp + 1);
	}
	if (rest.startsWith(":")) {
		const sp = rest.indexOf(" ");
		const prefix = rest.slice(1, sp);
		const bang = prefix.indexOf("!");
		// mask is the "user@host" portion (empty for a server/bare-nick prefix);
		// the join/part "full details" pref renders it (see renderable).
		out.prefix = {
			name: bang === -1 ? prefix : prefix.slice(0, bang),
			mask: bang === -1 ? "" : prefix.slice(bang + 1),
		};
		rest = rest.slice(sp + 1);
	}
	let trailing = null;
	const colon = rest.indexOf(" :");
	if (colon !== -1) {
		trailing = rest.slice(colon + 2);
		rest = rest.slice(0, colon);
	}
	const words = rest.split(" ").filter(Boolean);
	out.command = (words.shift() || "").toUpperCase();
	out.params = words;
	if (trailing !== null) out.params.push(trailing);
	return out;
}

// renderMessage renders a PRIVMSG/NOTICE: plain text, CTCP ACTION, or a
// notice — with the bot-mode chip when the server tagged the sender as a
// bot (a bare "bot" tag).
function renderMessage(command, line, last) {
	const bot = "bot" in line.tags;
	const m = /^\x01ACTION ([^]*?)\x01?$/.exec(last);
	if (m) return { kind: "action", text: m[1], bot };
	return { kind: command === "NOTICE" ? "notice" : "msg", text: last, bot };
}

// renderable turns a protocol EventData into what a row displays:
//   { kind: "msg" | "action" | "notice", text }
//   { kind: "system", mark, markClass, text }
// statusHost (the "full join/part details" pref) appends the sender's
// ident@host to join/part/quit lines: "nick (~user@host) has quit".
export function renderable(ev, statusHost) {
	if (ev.redacted) {
		const why = ev.redact_reason ? ` (${ev.redact_reason})` : "";
		return { kind: "redacted", mark: "⌫", markClass: "mode", text: `message deleted${why}` };
	}
	const line = parseLine(ev.raw);
	const last = line.params.at(-1) ?? "";
	// "nick (~user@host)" when the pref is on and the raw line carried a mask.
	const who = statusHost && line.prefix?.mask ? `${ev.sender} (${line.prefix.mask})` : ev.sender;
	switch (ev.command) {
		case "PRIVMSG":
		case "NOTICE":
			return renderMessage(ev.command, line, last);
		case "JOIN":
			return { kind: "system", mark: "→", markClass: "join", text: `${who} has joined` };
		case "PART":
			return {
				kind: "system", mark: "←", markClass: "part",
				text: `${who} has left` + (line.params.length > 1 ? ` (${last})` : ""),
			};
		case "KICK":
			return {
				kind: "system", mark: "←", markClass: "part",
				text: `${line.params[1] || "?"} was kicked by ${ev.sender}` +
					(line.params.length > 2 ? ` (${last})` : ""),
			};
		case "QUIT":
			return {
				kind: "system", mark: "←", markClass: "part",
				text: `${who} has quit` + (line.params.length > 0 && last ? ` (${last})` : ""),
			};
		case "NICK":
			return {
				kind: "system", mark: "•", markClass: "mode",
				text: `${ev.sender} is now known as ${line.params[0] || "?"}`,
			};
		case "MODE":
			return {
				kind: "system", mark: "•", markClass: "mode",
				text: `${ev.sender} set mode ${line.params.slice(1).join(" ")}`,
			};
		case "TOPIC":
			return { kind: "system", mark: "•", markClass: "mode", text: `${ev.sender} set the topic: ${last}` };
		default:
			return { kind: "system", mark: "•", markClass: "mode", text: ev.raw };
	}
}

// ---- status-message visibility (The Lounge-style show/collapse/hide) ----

// Presence churn: the lines the statusMsgs pref governs. KICK stays
// visible always — moderation is signal, not noise.
const PRESENCE = new Set(["JOIN", "PART", "QUIT", "NICK"]);

export function isPresence(ev) {
	return PRESENCE.has(ev.command) && !ev.whois;
}

function collapseSummary(events) {
	const n = { JOIN: 0, PART: 0, QUIT: 0, NICK: 0 };
	for (const ev of events) n[ev.command]++;
	const parts = [];
	if (n.JOIN) parts.push(`${n.JOIN} joined`);
	if (n.PART + n.QUIT) parts.push(`${n.PART + n.QUIT} left`);
	if (n.NICK) parts.push(`${n.NICK} nick change` + (n.NICK > 1 ? "s" : ""));
	return parts.join(", ");
}

// applyStatusMode filters/folds presence events per the statusMsgs pref.
// "collapse" replaces each run of 2+ consecutive presence events with a
// synthetic toggle row { collapse: [...], expanded }; expanded runs (ids
// in `expanded`) keep the toggle row and emit their events after it.
export function applyStatusMode(list, mode, expanded) {
	if (mode === "show") return list;
	if (mode === "hide") return list.filter((ev) => !isPresence(ev));
	const out = [];
	let i = 0;
	while (i < list.length) {
		if (!isPresence(list[i])) {
			out.push(list[i]);
			i++;
			continue;
		}
		let j = i;
		while (j < list.length && isPresence(list[j])) j++;
		const run = list.slice(i, j);
		if (run.length === 1) {
			out.push(run[0]);
		} else {
			// Anchor the collapse-row id on the run's LAST event: a
			// prepended older page can extend the run at its top (moving
			// run[0]), which would change the id and break the virtual
			// list's prepend scroll anchor.
			const id = "clp-" + run[run.length - 1].id;
			// Expand state is keyed on run MEMBERSHIP, not the derived id:
			// the run can grow at the top (prepend) or the tail (a live
			// join/part), moving both run[0] and run[last]. `expanded` holds
			// an event id from the run at toggle time, so the run stays open
			// as long as it still contains that event — surviving growth at
			// either end (an id-based key re-collapsed on tail extension).
			const isOpen = run.some((e) => expanded?.has(e.id));
			out.push({
				id, collapse: run, expanded: isOpen,
				time: run[run.length - 1].time,
				summary: collapseSummary(run),
			});
			if (isOpen) out.push(...run);
		}
		i = j;
	}
	return out;
}

// nickColor implements the mockup's deterministic hash:
// h = (h*31 + charCode) % 360 folded into an oklch color, with lightness/
// chroma per theme.
export function nickColor(nick, theme) {
	let h = 0;
	for (const c of nick) h = (h * 31 + c.codePointAt(0)) % 360;
	const light = theme === "light";
	return `oklch(${light ? 0.5 : 0.74} ${light ? 0.15 : 0.13} ${h})`;
}

// fmtTime renders a timestamp per the user's clock prefs. opts:
//   clock:   "24" (default) or "12"
//   seconds: append ":SS" when true
//   ampm:    append " AM"/" PM" in 12-hour mode when true
// Hours are zero-padded in both modes so the mono column stays aligned.
// With no opts it yields the historical "HH:MM" 24-hour form.
export function fmtTime(ms, opts) {
	const o = opts || {};
	const d = new Date(ms);
	let h = d.getHours();
	let suffix = "";
	if (o.clock === "12") {
		const pm = h >= 12;
		h = h % 12 || 12;
		if (o.ampm) suffix = pm ? " PM" : " AM";
	}
	let out = String(h).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
	if (o.seconds) out += ":" + String(d.getSeconds()).padStart(2, "0");
	return out + suffix;
}

// sameGroup: consecutive-message grouping — hide the nick on runs of
// messages from the same sender within 5 minutes.
export function sameGroup(prev, cur) {
	return !!prev && !!cur &&
		prev.kind === "msg" && cur.kind === "msg" &&
		prev.sender === cur.sender &&
		cur.time - prev.time < 5 * 60 * 1000;
}

// mentionsMe: \b is wrong for IRC nicks (nick_ , nick[] ...), so use the
// nickname alphabet as the boundary.
const NICK_CHARS = String.raw`A-Za-z0-9_\-\[\]\\` + "`^{}|";
export function mentionsMe(text, nick) {
	if (!nick) return false;
	text = stripFormatting(text); // a colour code between chars must not hide a mention
	// Fold BOTH sides with the rfc1459 casemapping (RFC 2812 §2.2: {}|^ ≡ []\~)
	// before matching: on a CASEMAPPING=rfc1459 network "dan{m}: hi" addresses
	// dan[m], and missing that ping is worse than the false-positive highlight
	// this can produce on a strict-ascii network — the same loose-superset
	// tradeoff foldNick already makes for whois/pending keys (we don't know the
	// network's CASEMAPPING here either). foldNick is a 1:1 character mapping,
	// so lengths and boundary positions in the folded text line up with the
	// original; only the boolean result is used by callers anyway. The folded
	// text contains {}|^ where the original had []\~ — NICK_CHARS lists both
	// sets, so those stay nick characters, not word boundaries. The "i" flag
	// stays for ASCII case (foldNick lowercases, so it is belt-and-braces).
	const esc = foldNick(nick).replace(/[.*+?^${}()|[\]\\]/g, String.raw`\$&`);
	return new RegExp(`(^|[^${NICK_CHARS}])${esc}($|[^${NICK_CHARS}])`, "i").test(foldNick(text));
}

// linkify splits text into { link: bool, text } segments. The character class
// also excludes C0 control bytes (\x00-\x1f) so a mIRC formatting code adjacent
// to a URL — e.g. a colour reset in "\x0312http://x/path\x0f" — is not captured
// into the match. firstURL runs on the raw (still-formatted) body, and a URL
// carrying a control byte fails the preview endpoint's url.Parse (silently no
// preview) and the image fast-path; inside Body, linkify sees per-run text that
// parseFormatting already stripped, so it is unaffected either way.
const URL_RE = /https?:\/\/[^\s<>"'`\x00-\x08\x0e-\x1f]+/g;
export function linkify(text) {
	const out = [];
	let last = 0;
	for (const m of text.matchAll(URL_RE)) {
		let url = m[0];
		// Trailing punctuation almost never belongs to the URL.
		while (/[)\]}.,;:!?]$/.test(url) && !(url.endsWith(")") && url.includes("("))) {
			url = url.slice(0, -1);
		}
		if (m.index > last) out.push({ link: false, text: text.slice(last, m.index) });
		out.push({ link: true, text: url });
		last = m.index + url.length;
	}
	if (last < text.length) out.push({ link: false, text: text.slice(last) });
	return out;
}

// IRC_PALETTE is the standard mIRC / IRCv3 99-colour table (index 0..98; 99 and
// out-of-range mean "default", handled as null). These are FIXED hex values —
// the only server-controlled input to formatting is the numeric index, clamped
// into this table, so there is no way to inject an arbitrary colour string.
// Exported for the composer's format-panel colour swatches (indices 0-15).
// prettier-ignore
export const IRC_PALETTE = [
	"#ffffff", "#000000", "#00007f", "#009300", "#ff0000", "#7f0000", "#9c009c", "#fc7f00",
	"#ffff00", "#00fc00", "#009393", "#00ffff", "#0000fc", "#ff00ff", "#7f7f7f", "#d2d2d2",
	"#470000", "#472100", "#474700", "#324700", "#004700", "#00472c", "#004747", "#002747",
	"#000047", "#2e0047", "#470047", "#47002a", "#740000", "#743a00", "#747400", "#517400",
	"#007400", "#007449", "#007474", "#004074", "#000074", "#4b0074", "#740074", "#740045",
	"#b50000", "#b56300", "#b5b500", "#7db500", "#00b500", "#00b571", "#00b5b5", "#0063b5",
	"#0000b5", "#7500b5", "#b500b5", "#b5006b", "#ff0000", "#ff8c00", "#ffff00", "#b2ff00",
	"#00ff00", "#00ffa0", "#00ffff", "#008cff", "#0000ff", "#a500ff", "#ff00ff", "#ff0098",
	"#ff5959", "#ffb459", "#ffff71", "#cfff60", "#6fff6f", "#65ffc9", "#6dffff", "#59b4ff",
	"#5959ff", "#c459ff", "#ff66ff", "#ff59bc", "#ff9c9c", "#ffd39c", "#ffff9c", "#e2ff9c",
	"#9cff9c", "#9cffdb", "#9cffff", "#9cd3ff", "#9c9cff", "#dc9cff", "#ff9cff", "#ff94d3",
	"#000000", "#131313", "#282828", "#363636", "#4d4d4d", "#656565", "#818181", "#9f9f9f",
	"#bcbcbc", "#e2e2e2", "#ffffff",
];

function paletteColor(digits) {
	const n = Number.parseInt(digits, 10);
	return n >= 0 && n < IRC_PALETTE.length ? IRC_PALETTE[n] : null; // 99 / OOR = default
}

const isDigit = (c) => c >= "0" && c <= "9";
const HEX6 = /^[0-9a-fA-F]{6}$/;

// The simple attribute toggles, by control byte. \x0f (reset) and the two
// colour codes need their own handling.
const FMT_TOGGLES = {
	0x02: "bold", 0x1d: "italic", 0x1f: "underline",
	0x1e: "strike", 0x11: "mono", 0x16: "reverse",
};

// parseIndexedColor consumes the digits of a \x03 fg[,bg] code starting at i
// (just past the control byte), updating st. Returns the next index. A bare
// \x03 (no digits) resets both colours.
function parseIndexedColor(text, i, st) {
	let d = "";
	while (i < text.length && d.length < 2 && isDigit(text[i])) d += text[i++];
	if (d === "") {
		st.fg = null;
		st.bg = null;
		return i;
	}
	st.fg = paletteColor(d);
	if (text[i] === "," && isDigit(text[i + 1])) {
		i++;
		let e = "";
		while (i < text.length && e.length < 2 && isDigit(text[i])) e += text[i++];
		st.bg = paletteColor(e);
	}
	return i;
}

// parseHexColor consumes the arguments of a \x04 RRGGBB[,RRGGBB] code
// starting at i (just past the control byte), updating st. Returns the next
// index. A \x04 without six hex digits resets both colours and consumes
// nothing further (stripFormatting mirrors this).
function parseHexColor(text, i, st) {
	const fg = text.slice(i, i + 6);
	if (!HEX6.test(fg)) {
		st.fg = null;
		st.bg = null;
		return i;
	}
	st.fg = "#" + fg;
	i += 6;
	if (text[i] === "," && HEX6.test(text.slice(i + 1, i + 7))) {
		st.bg = "#" + text.slice(i + 1, i + 7);
		i += 7;
	}
	return i;
}

// parseFormatting turns an IRC message body into styled runs. It consumes mIRC
// control codes — attributes \x02 bold, \x1d italic, \x1f underline, \x1e
// strikethrough, \x11 monospace, \x16 reverse, \x0f reset; \x03 fg[,bg] indexed
// colour; \x04 RRGGBB[,RRGGBB] hex colour — and returns
// [{ text, bold, italic, underline, strike, mono, reverse, fg, bg }], where
// fg/bg are resolved CSS colour strings or null. The control bytes themselves
// (and their numeric arguments) are removed, so callers never see them and the
// old digit-leak (`\06^13.05^04/`) is gone.
// MAX_FMT_RUNS caps the styled runs one message can produce: a hostile body
// alternating control codes could otherwise explode 16 KiB into ~8k runs,
// each of which Body maps through linkify/highlight into VNodes — and a
// visible message re-renders on every composer keystroke. Past the cap the
// remainder renders as one run with its codes stripped. Real formatted
// messages use a handful of runs; even ASCII-art bots stay far below this.
const MAX_FMT_RUNS = 1024;

export function parseFormatting(text) {
	const runs = [];
	const st = { bold: false, italic: false, underline: false, strike: false, mono: false, reverse: false, fg: null, bg: null };
	let buf = "";
	const flush = () => { if (buf) { runs.push({ text: buf, ...st }); buf = ""; } };
	const reset = () => Object.assign(st, { bold: false, italic: false, underline: false, strike: false, mono: false, reverse: false, fg: null, bg: null });
	let i = 0;
	while (i < text.length) {
		// Reserve one slot for the merged-remainder run the trailing flush pushes,
		// so the hard ceiling is exactly MAX_FMT_RUNS (not MAX_FMT_RUNS+1).
		if (runs.length >= MAX_FMT_RUNS - 1) {
			buf += stripFormatting(text.slice(i));
			break;
		}
		const c = text.codePointAt(i);
		const toggle = FMT_TOGGLES[c];
		if (toggle) {
			flush();
			st[toggle] = !st[toggle];
			i++;
		} else if (c === 0x0f) {
			flush();
			reset();
			i++;
		} else if (c === 0x03) {
			flush();
			i = parseIndexedColor(text, i + 1, st);
		} else if (c === 0x04) {
			flush();
			i = parseHexColor(text, i + 1, st);
		} else {
			buf += text[i++];
		}
	}
	flush();
	return runs;
}

// stripFormatting removes all mIRC control codes (and their colour arguments)
// so text matching — mention detection especially — sees the plain body. The
// \x04 arguments are optional, mirroring parseFormatting's consume-one-byte
// handling of a bare \x04: "al\x04ice" renders as "alice" and must also
// MATCH as "alice", or the mention is visible but never alerts.
export function stripFormatting(text) {
	// Two passes (colour codes with their arguments, then the bare attribute
	// bytes) — the patterns are disjoint by lead byte, so this matches the
	// single-alternation form exactly while keeping each regex simple. The
	// \x03 form consumes the ",BG" part ONLY when a foreground digit precedes
	// it, mirroring parseIndexedColor: a bare "\x03,5" is a colour reset plus
	// the literal ",5", so strip must leave ",5" or mention detection diverges
	// from what the body renders.
	// Three disjoint-by-lead-byte passes (indexed colour, hex colour, then the
	// bare attribute bytes). Splitting the two colour forms keeps each regex
	// simple; the passes can't interact since removing one lead byte's code
	// never forms another's.
	return text
		.replace(/\x03(?:\d{1,2}(?:,\d{1,2})?)?/g, "")
		.replace(/\x04(?:[0-9a-fA-F]{6}(?:,[0-9a-fA-F]{6})?)?/g, "")
		.replace(/[\x02\x0f\x11\x16\x1d\x1e\x1f]/g, "");
}

// nickSet builds the lookup for in-body nick highlighting: lowercased nick ->
// canonical nick (used for a stable color and the user menu). The own nick is
// excluded — it already highlights the whole row as a mention.
export function nickSet(nicks, exclude) {
	const m = new Map();
	for (const n of nicks || []) {
		if (n && n !== exclude) m.set(n.toLowerCase(), n);
	}
	return m;
}

// A run of nick-legal characters (letters, digits, and the RFC 2812 "special"
// set) — used to tokenize body text so a nick matches as a WHOLE token, never
// a substring.
const NICK_SPLIT_RE = /([\w\-[\]\\{}|^`~]+)/;

// highlightNicks splits body text into plain and nick segments given the map
// from nickSet(). A token becomes a nick only when the whole token is a known
// nick ("bob" inside "bobby" stays plain). Returns [{nick, text}] where nick
// is the canonical nick (null for plain text) and text is the text as typed.
export function highlightNicks(text, nickMap) {
	if (!nickMap?.size) return [{ nick: null, text }];
	const parts = text.split(NICK_SPLIT_RE);
	const out = [];
	const pushPlain = (t) => {
		const last = out.at(-1);
		if (last?.nick === null) last.text += t;
		else out.push({ nick: null, text: t });
	};
	for (let i = 0; i < parts.length; i++) {
		const seg = parts[i];
		if (seg === "") continue;
		if (i % 2 === 1) { // odd indices are the captured nick-candidate tokens
			const canon = nickMap.get(seg.toLowerCase());
			if (canon) {
				out.push({ nick: canon, text: seg });
				continue;
			}
		}
		pushPlain(seg);
	}
	return out;
}

// proxyCredsExposed reports whether a proxy URL carries credentials to a
// non-loopback host. SOCKS5 (RFC 1929) and HTTP Basic proxy auth are sent
// unencrypted, so credentials to a remote proxy travel in the clear unless the
// transport to it is itself protected (VPN / SSH tunnel). Loopback is exempt.
export function proxyCredsExposed(proxy) {
	if (!proxy) return false;
	const rest = proxy.replace(/^[a-zA-Z0-9]+:\/\//, ""); // drop scheme
	const auth = rest.split("/")[0]; // authority only
	const at = auth.lastIndexOf("@");
	if (at === -1) return false; // no userinfo -> no proxy auth
	let host = auth.slice(at + 1);
	host = host.startsWith("[") ? host.slice(1, host.indexOf("]")) : host.split(":")[0];
	host = host.toLowerCase();
	// 127.0.0.0/8 only as a real IPv4 literal — a prefix test (/^127\./) would
	// wrongly treat a public name like "127.attacker.example" as loopback and
	// suppress the warning.
	const v4 = host.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
	const isLoopbackV4 = !!v4 && v4.slice(1).every((o) => +o <= 255) && +v4[1] === 127;
	return !(host === "localhost" || host === "::1" || isLoopbackV4);
}

// foldNick applies an RFC1459-superset case fold for keying nick comparisons
// client-side (we don't know the network's exact CASEMAPPING): lowercase, plus
// the full []\~ -> {}|^ mapping of rfc1459 (rfc1459-strict folds only []\,
// and ascii folds none of them — this is deliberately the LOOSEST of the
// three). Folding a superset never fails to match equivalent nicks; the
// accepted tradeoff is that two nicks the server keeps distinct (e.g. Name[
// vs Name{ on an ascii network) can collide in the pending-/whois Set, so
// one of two near-simultaneous whois replies may be dropped — rare, bounded,
// and recoverable by re-issuing the /whois.
export function foldNick(s) {
	const m = { "[": "{", "]": "}", "\\": "|", "~": "^" };
	return String(s).toLowerCase().replace(/[[\]\\~]/g, (c) => m[c]);
}

// isChannelName follows the network's ISUPPORT CHANTYPES (sent with the
// buffer list); "#&" is the RFC 1459 default for networks we have not
// heard from yet.
export function isChannelName(s, chantypes) {
	return !!s && (chantypes || "#&").includes(s[0]);
}

// bufferOrder returns buffer keys in sidebar order (network, then name),
// for keyboard prev/next navigation.
export function bufferOrder(buffers) {
	return Object.values(buffers)
		.sort((a, b) => a.network.localeCompare(b.network) || a.buffer.localeCompare(b.buffer))
		.map((b) => b.key);
}

// rankBuffers filters and orders buffers for the switcher palette:
// substring match on buffer name or network; mentions first, then
// unread, then earlier match position, then sidebar order.
export function rankBuffers(buffers, query) {
	const q = query.trim().toLowerCase();
	const pos = (b) => {
		const i = b.buffer.toLowerCase().indexOf(q);
		return i === -1 ? 1000 : i;
	};
	return Object.values(buffers)
		.filter((b) => !q || b.buffer.toLowerCase().includes(q) || b.network.toLowerCase().includes(q))
		.sort((a, b) =>
			(b.mention ? 1 : 0) - (a.mention ? 1 : 0) ||
			(b.unread > 0 ? 1 : 0) - (a.unread > 0 ? 1 : 0) ||
			(q ? pos(a) - pos(b) : 0) ||
			a.network.localeCompare(b.network) ||
			a.buffer.localeCompare(b.buffer));
}

// parseInput interprets composer input: plain text, a "//"-escaped
// literal slash, or a command. `buffer` is the active buffer (default
// target for /part and /topic); `chantypes` is the network's channel
// prefixes. Returns one of:
//   { type: "text", text }                    — send to the active buffer
//   { type: "msg", target, text }             — /msg (caller switches)
//   { type: "cmd", command, params, switchTo? }
//   { type: "error", message }
export function parseInput(input, buffer, chantypes) {
	if (!input.startsWith("/")) return { type: "text", text: input };
	if (input.startsWith("//")) return { type: "text", text: input.slice(1) };
	const err = (message) => ({ type: "error", message });
	// Split "/cmd rest" by the first whitespace — no backtracking, and a
	// bare "/" is an error instead of a crash.
	const body = input.slice(1);
	const sp = body.search(/\s/);
	const cmd = (sp === -1 ? body : body.slice(0, sp)).toLowerCase();
	if (!cmd) return err("usage: /<command>");
	const rest = sp === -1 ? "" : body.slice(sp + 1).trim();
	// The command name is untrusted composer input. Never walk Object.prototype:
	// `/constructor` used to select the inherited constructor function, while
	// `/__proto__` selected an object and then threw when invoked.
	const parse = Object.prototype.hasOwnProperty.call(CMD_PARSERS, cmd) ? CMD_PARSERS[cmd] : null;
	return parse ? parse(rest, buffer, chantypes, err) : err("unknown command /" + cmd);
}

// One parser per command, uniform (rest, buffer, chantypes, err)
// signature; the table doubles as the COMMANDS completion source.
const CMD_PARSERS = Object.assign(Object.create(null), {
	me: (rest, _buffer, _ct, err) =>
		rest ? { type: "text", text: "\x01ACTION " + rest + "\x01" } : err("usage: /me <action>"),
	msg: parseMsgCmd,
	query: parseMsgCmd,
	join: parseJoinCmd,
	part: parsePartCmd,
	nick: (rest, _buffer, _ct, err) =>
		!rest || /\s/.test(rest)
			? err("usage: /nick <newnick>")
			: { type: "cmd", command: "NICK", params: [rest] },
	topic: parseTopicCmd,
	whois: nickArgCmd("WHOIS"),
	whowas: nickArgCmd("WHOWAS"),
	who: nickArgCmd("WHO"),
	list: (rest, _buffer, _ct, err) =>
		/\s/.test(rest)
			? err("usage: /list [pattern]")
			: { type: "cmd", command: "LIST", params: rest ? [rest] : [] },
	motd: (rest, _buffer, _ct, err) =>
		rest ? err("usage: /motd") : { type: "cmd", command: "MOTD", params: [] },
	// A message marks us away; none marks us back.
	away: (rest) => ({ type: "cmd", command: "AWAY", params: rest ? [rest] : [] }),
	notice: parseNoticeCmd,
	mode: parseModeCmd,
	kick: parseKickCmd,
	invite: parseInviteCmd,
});

// COMMANDS lists every slash command parseInput understands, for
// composer tab-completion — derived from the parser table so the two
// cannot drift.
export const COMMANDS = Object.keys(CMD_PARSERS).sort((a, b) => a.localeCompare(b));

// nickArgCmd builds a parser for commands taking exactly one nick/mask.
function nickArgCmd(command) {
	return (rest, _buffer, _ct, err) =>
		!rest || /\s/.test(rest)
			? err(`usage: /${command.toLowerCase()} <nick>`)
			: { type: "cmd", command, params: [rest] };
}

function parseTopicCmd(rest, buffer, chantypes, err) {
	if (!isChannelName(buffer, chantypes)) return err("/topic: not in a channel");
	if (!rest) return err("usage: /topic <text>");
	return { type: "cmd", command: "TOPIC", params: [buffer, rest] };
}

function parseNoticeCmd(rest, _buffer, _ct, err) {
	const sp = rest.indexOf(" ");
	if (sp === -1 || !rest.slice(sp + 1).trim()) return err("usage: /notice <target> <text>");
	return { type: "cmd", command: "NOTICE", params: [rest.slice(0, sp), rest.slice(sp + 1).trim()] };
}

function parseMsgCmd(rest, _buffer, _ct, err) {
	const sp = rest.indexOf(" ");
	if (sp === -1 || !rest.slice(sp + 1).trim()) return err("usage: /msg <target> <text>");
	return { type: "msg", target: rest.slice(0, sp), text: rest.slice(sp + 1).trim() };
}

function parseJoinCmd(rest, _buffer, chantypes, err) {
	if (!rest) return err("usage: /join <channel> [key]");
	const [chan, key] = rest.split(/\s+/);
	if (!isChannelName(chan, chantypes)) return err("/join: " + chan + " is not a channel");
	return { type: "cmd", command: "JOIN", params: key ? [chan, key] : [chan], switchTo: chan };
}

// parsePartCmd: /part [channel] [reason], defaulting to the current
// buffer when the first word is not a channel.
function parsePartCmd(rest, buffer, chantypes, err) {
	let chan = buffer;
	let reason = rest;
	if (isChannelName(rest.split(/\s+/)[0] || "", chantypes)) {
		const sp = rest.indexOf(" ");
		chan = sp === -1 ? rest : rest.slice(0, sp);
		reason = sp === -1 ? "" : rest.slice(sp + 1).trim();
	}
	if (!isChannelName(chan, chantypes)) return err("/part: not in a channel");
	return { type: "cmd", command: "PART", params: reason ? [chan, reason] : [chan] };
}

// parseModeCmd: /mode queries the current buffer; /mode +m sets modes
// on it; /mode <target> ... names one explicitly.
function parseModeCmd(rest, buffer, _ct, err) {
	const parts = rest ? rest.split(/\s+/) : [];
	let target = buffer;
	if (parts.length && !/^[+-]/.test(parts[0])) target = parts.shift();
	if (parts.length > 5) return err("/mode: too many parameters");
	return { type: "cmd", command: "MODE", params: [target, ...parts] };
}

// parseKickCmd: /kick <nick> [reason] in a channel; /kick <channel>
// <nick> [reason] anywhere.
function parseKickCmd(rest, buffer, chantypes, err) {
	const parts = rest ? rest.split(/\s+/) : [];
	let chan = buffer;
	if (parts.length && isChannelName(parts[0], chantypes)) chan = parts.shift();
	const nick = parts.shift();
	if (!nick) return err("usage: /kick [channel] <nick> [reason]");
	if (!isChannelName(chan, chantypes)) return err("/kick: not in a channel");
	const reason = parts.join(" ");
	return { type: "cmd", command: "KICK", params: reason ? [chan, nick, reason] : [chan, nick] };
}

function parseInviteCmd(rest, buffer, chantypes, err) {
	const parts = rest ? rest.split(/\s+/) : [];
	if (!parts.length || parts.length > 2) return err("usage: /invite <nick> [channel]");
	const chan = parts[1] || buffer;
	if (!isChannelName(chan, chantypes)) return err("/invite: not in a channel");
	return { type: "cmd", command: "INVITE", params: [parts[0], chan] };
}

// groupMembers buckets a channel roster for the members panel by the
// highest (first) status prefix: Ops (~ & @), Voice (% +), Members.
// Empty groups are dropped.
export function groupMembers(members) {
	const groups = [
		{ label: "Ops", members: [] },
		{ label: "Voice", members: [] },
		{ label: "Members", members: [] },
	];
	for (const m of members) {
		const p = (m.prefix || "")[0];
		if (p && "~&@".includes(p)) groups[0].members.push(m);
		else if (p && "%+".includes(p)) groups[1].members.push(m);
		else groups[2].members.push(m);
	}
	return groups.filter((g) => g.members.length > 0);
}

// TypingSender implements the sending side of the draft/typing client
// tag (https://ircv3.net/specs/client-tags/typing, fetched 2026-07-15):
//   - "active" continuously while composing, throttled so no
//     notification goes out within 3 seconds of another;
//   - "paused" once when input rests non-empty (owner calls pause()
//     from an idle timer);
//   - "done" once when the field is cleared or composing is abandoned.
// Slash commands never trigger notifications. Sending the message ends
// the session silently — the message itself clears remote indicators.
export class TypingSender {
	constructor(send, now = () => Date.now()) {
		this.send = send;
		this.now = now;
		this.lastSent = 0;
		this.state = "none"; // none | active | paused
	}

	notify(state) {
		this.lastSent = this.now();
		this.send(state);
	}

	// input is called on every draft change. The 3s throttle applies to
	// all notifications, so resuming right after a pause may briefly show
	// as paused remotely — the spec mandates the throttle regardless.
	input(text) {
		if (!text || text.startsWith("/")) {
			this.done();
			return;
		}
		if (this.now() - this.lastSent >= 3000) {
			this.state = "active";
			this.notify("active");
		}
	}

	// pause is called by the owner's idle timer while text remains.
	pause(text) {
		if (this.state === "active" && text && !text.startsWith("/")) {
			this.state = "paused";
			this.notify("paused");
		}
	}

	// done is called when the input clears without sending.
	done() {
		if (this.state !== "none") {
			this.state = "none";
			this.notify("done");
		}
	}

	// messageSent ends the session without a notification: the delivered
	// message clears remote indicators by itself.
	messageSent() {
		this.state = "none";
	}
}

// typingText renders the indicator wording for a set of typing nicks.
export function typingText(nicks) {
	if (nicks.length === 0) return "";
	if (nicks.length === 1) return nicks[0] + " is typing…";
	if (nicks.length === 2) return nicks[0] + " and " + nicks[1] + " are typing…";
	return "several people are typing…";
}

// typing states expire per the spec: 6s after active, 30s after paused.
export function typingExpired(state, at, now) {
	return now - at > (state === "paused" ? 30000 : 6000);
}

// Browser-side typing state is deliberately much smaller than the message
// working set. A server controls typing pushes, so both dimensions need hard
// caps even though legitimate channels rarely have more than a handful of
// simultaneous typers. Maps also avoid special property names such as
// "__proto__" changing the representation itself.
export const MAX_TYPING_BUFFERS = 128;
export const MAX_TYPERS_PER_BUFFER = 64;

export function clearTypingNick(all, key, nick) {
	const cur = all.get(key);
	if (!cur?.has(nick)) return all;
	const next = new Map(all);
	const nicks = new Map(cur);
	nicks.delete(nick);
	if (nicks.size) next.set(key, nicks);
	else next.delete(key);
	return next;
}

// updateTypingState applies one already-authorized push. Callers pass
// knownBuffer=false for unknown/closed buffers so a hostile server cannot
// create state for arbitrary names. A stray "done" is always a no-op.
export function updateTypingState(all, key, d, knownBuffer, now = Date.now()) {
	if (!knownBuffer || !key || !d?.nick) return all;
	const cur = all.get(key);
	if (d.state === "done") return clearTypingNick(all, key, d.nick);
	if (d.state !== "active" && d.state !== "paused") return all;
	if (!cur && all.size >= MAX_TYPING_BUFFERS) return all;
	if (!cur?.has(d.nick) && (cur?.size || 0) >= MAX_TYPERS_PER_BUFFER) return all;
	const next = new Map(all);
	const nicks = new Map(cur || []);
	nicks.set(d.nick, { state: d.state, at: now });
	next.set(key, nicks);
	return next;
}

export function expireTypingState(all, now = Date.now()) {
	let next = all;
	for (const [key, cur] of all) {
		let nicks = cur;
		for (const [nick, v] of cur) {
			if (!typingExpired(v.state, v.at, now)) continue;
			if (next === all) next = new Map(all);
			if (nicks === cur) nicks = new Map(cur);
			nicks.delete(nick);
		}
		if (nicks === cur) continue;
		if (nicks.size) next.set(key, nicks);
		else next.delete(key);
	}
	return next;
}

// New servers explicitly say whether another byte-bounded history page is
// available. Retain the old full-page heuristic for rolling upgrades.
export function historyHasMore(page, limit) {
	if (typeof page?.has_more === "boolean") return page.has_more;
	return (page?.messages?.length || 0) >= limit;
}

// firstURL returns the first http(s) link in text, or "".
export function firstURL(text) {
	for (const seg of linkify(text)) {
		if (seg.link) return seg.text;
	}
	return "";
}

// looksLikeImageURL is a cheap client-side guess by extension, used to
// skip the preview round-trip and render a thumbnail straight away; the
// server is authoritative either way.
export function looksLikeImageURL(u) {
	try {
		const path = new URL(u).pathname.toLowerCase();
		// Only extensions the server can decode (see api isImageType / thumb.go):
		// an undecodable one would route here to a thumbnail that 415s (blank).
		return /\.(png|jpe?g|gif|webp)$/.test(path);
	} catch {
		return false;
	}
}

export function hostOf(u) {
	try {
		return new URL(u).host;
	} catch {
		return u;
	}
}

// mediaKindOf classifies a link as playable media by its URL path extension
// (case-insensitive, query/fragment ignored): "audio" | "video" | null.
// Extension lists match what browsers commonly play natively; the server's
// stream endpoint is authoritative (it allowlists by Content-Type), so a
// mislabeled URL just yields a card whose player errors to "open original".
const AUDIO_EXTS = new Set(["mp3", "ogg", "opus", "flac", "m4a", "wav", "aac"]);
const VIDEO_EXTS = new Set(["mp4", "webm", "m4v", "ogv"]);
export function mediaKindOf(u) {
	try {
		const path = new URL(u).pathname.toLowerCase();
		const dot = path.lastIndexOf(".");
		if (dot < 0 || dot === path.length - 1 || path.includes("/", dot)) return null;
		const ext = path.slice(dot + 1);
		if (AUDIO_EXTS.has(ext)) return "audio";
		if (VIDEO_EXTS.has(ext)) return "video";
		return null;
	} catch {
		return null;
	}
}

// SERVER_BUFFER is the per-network "server buffer" target (must match the
// hub's serverBufferTarget): server/service notices and server-info lines
// live here, surfaced as the sidebar's network header (The Lounge lobby).
export const SERVER_BUFFER = "*";

export function bufKey(network, buffer) {
	return network + "\n" + buffer;
}

// mergeServerBuffers builds the sidebar buffer map from an authoritative
// get_buffers response. It carries over the client-side mention flag and
// preserves client-only buffers the server omits — but ONLY those still
// flagged `ephemeral` (a just-opened query/DM or whois card not yet
// persisted). A non-ephemeral buffer the server omits was a real server
// buffer the server has intentionally dropped (e.g. closed on another
// device while this client was offline); it must not be resurrected. Pure,
// so it can be unit tested.
export function mergeServerBuffers(dataBuffers, prev, nets, truncated = false) {
	const bufs = {};
	for (const b of dataBuffers || []) {
		const key = bufKey(b.network, b.buffer);
		bufs[key] = {
			key, network: b.network, buffer: b.buffer,
			lastTime: b.last_time, marker: b.marker, unread: b.unread,
			// Only carry the client mention flag while the AUTHORITATIVE unread is
			// still positive: unread==0 means the buffer was read (here or on
			// another device), so a leftover mention badge would be stale.
			mention: b.unread > 0 ? (prev[key]?.mention || false) : false,
		};
	}
	for (const key of Object.keys(prev)) {
		if (!bufs[key] && (prev[key].ephemeral || truncated) && prev[key].network in nets) bufs[key] = prev[key];
	}
	return bufs;
}

export function parseHash(hash) {
	// "#/<network>/<buffer>", split at the first slash; the hash is
	// attacker-influenceable (links), so no backtracking regex and no
	// uncaught URIError on malformed percent-escapes.
	if (!hash.startsWith("#/")) return null;
	const rest = hash.slice(2);
	const slash = rest.indexOf("/");
	if (slash <= 0 || slash === rest.length - 1) return null;
	try {
		return { network: decodeURIComponent(rest.slice(0, slash)), buffer: decodeURIComponent(rest.slice(slash + 1)) };
	} catch {
		return null;
	}
}

export function toHash(network, buffer) {
	return `#/${encodeURIComponent(network)}/${encodeURIComponent(buffer)}`;
}

// byTimeId orders messages chronologically; the id breaks a same-time
// tie (numeric store ids compare numerically, synthetic string ids
// lexically).
function cmpStr(a, b) {
	if (a < b) return -1;
	if (a > b) return 1;
	return 0;
}

export function byTimeId(a, b) {
	if (a.time !== b.time) return a.time - b.time;
	if (typeof a.id === "number" && typeof b.id === "number") return a.id - b.id;
	return cmpStr(String(a.id), String(b.id));
}

// mergeById unions two message lists, de-duplicating by id and sorting
// by (time, id). Robust to overlapping/out-of-order history pages — a
// get_history window can slide between requests, so a page is not
// assumed strictly older or newer than what is already held.
export function mergeById(existing, incoming) {
	const byId = new Map();
	for (const e of existing) byId.set(e.id, e);
	for (const e of incoming) byId.set(e.id, e);
	return [...byId.values()].sort(byTimeId);
}

// rememberRedaction records a redaction tombstone in the persistent per-buffer
// set so a later history/search response that re-delivers the pre-scrub row
// cannot restore its content (see applyTombstones). store is a
// Map<bufKey, Map<msgid, reason>>.
// maxTombstonesPerBuffer bounds a buffer's redaction tombstone set, and
// maxTombstoneBuffers bounds how many buffers carry a set at all, so a hostile
// redaction flood across endlessly-varying targets can't grow the outer map
// without limit either (it is only a belt-and-braces guard over the server's
// destructive scrub). The reason string is server-clamped upstream.
// Bounds on the redaction-tombstone store (belt-and-braces over the server's
// DESTRUCTIVE scrub, so it only guards the narrow snapshot-before-scrub race).
// Kept modest so a hostile server redacting endlessly can't grow browser memory
// without limit: worst case ~maxTombstoneBuffers × maxTombstonesPerBuffer ×
// (msgid ≤512 B + maxTombstoneReason) ≈ tens of MB, not hundreds.
const maxTombstonesPerBuffer = 400;
const maxTombstoneBuffers = 128;
const maxTombstoneReason = 256; // display only needs a short reason

export function rememberRedaction(store, key, msgid, reason) {
	if (!msgid) return;
	let byId = store.get(key);
	if (!byId) {
		byId = new Map();
		store.set(key, byId);
		if (store.size > maxTombstoneBuffers) {
			store.delete(store.keys().next().value); // evict oldest buffer, insertion order
		}
	}
	byId.set(msgid, (reason || "").slice(0, maxTombstoneReason));
	if (byId.size > maxTombstonesPerBuffer) {
		byId.delete(byId.keys().next().value); // evict oldest by insertion order
	}
}

// applyTombstones re-marks any incoming row whose msgid was redacted while the
// buffer was unloaded. A history/search page can snapshot a row on the server
// BEFORE its destructive redaction scrub commits and then arrive AFTER the
// redact push — and mergeById lets incoming rows win — so without this the
// deleted content would be restored on screen. tombstones is a
// Map<msgid, reason> (may be undefined/empty).
export function applyTombstones(list, tombstones) {
	if (!tombstones || tombstones.size === 0) return list;
	return list.map((ev) =>
		ev.msgid && !ev.redacted && tombstones.has(ev.msgid)
			? { ...ev, redacted: true, redact_reason: tombstones.get(ev.msgid), raw: "" }
			: ev,
	);
}

// uuid returns a random id. crypto.randomUUID is secure-context-only
// (HTTPS/localhost) — unavailable over plain HTTP at a LAN address, the
// "open from my phone" case — so fall back to getRandomValues, which is
// available in any context.
export function uuid() {
	if (globalThis.crypto?.randomUUID) return crypto.randomUUID();
	const b = crypto.getRandomValues(new Uint32Array(4));
	return "r-" + b[0].toString(16) + b[1].toString(16) + b[2].toString(16) + b[3].toString(16);
}
