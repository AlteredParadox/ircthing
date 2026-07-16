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
		out.prefix = { name: bang === -1 ? prefix : prefix.slice(0, bang) };
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

// renderable turns a protocol EventData into what a row displays:
//   { kind: "msg" | "action" | "notice", text }
//   { kind: "system", mark, markClass, text }
export function renderable(ev) {
	if (ev.redacted) {
		const why = ev.redact_reason ? ` (${ev.redact_reason})` : "";
		return { kind: "redacted", mark: "⌫", markClass: "mode", text: `message deleted${why}` };
	}
	const line = parseLine(ev.raw);
	const last = line.params.length ? line.params[line.params.length - 1] : "";
	switch (ev.command) {
		case "PRIVMSG":
		case "NOTICE": {
			const m = /^\x01ACTION ([^]*?)\x01?$/.exec(last);
			if (m) return { kind: "action", text: m[1] };
			return { kind: ev.command === "NOTICE" ? "notice" : "msg", text: last };
		}
		case "JOIN":
			return { kind: "system", mark: "→", markClass: "join", text: `${ev.sender} has joined` };
		case "PART":
			return {
				kind: "system", mark: "←", markClass: "part",
				text: `${ev.sender} has left` + (line.params.length > 1 ? ` (${last})` : ""),
			};
		case "KICK":
			return {
				kind: "system", mark: "←", markClass: "part",
				text: `${line.params[1] || "?"} was kicked by ${ev.sender}` +
					(line.params.length > 2 ? ` (${last})` : ""),
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

// nickColor implements the mockup's deterministic hash:
// h = (h*31 + charCode) % 360 folded into an oklch color, with lightness/
// chroma per theme.
export function nickColor(nick, theme) {
	let h = 0;
	for (const c of nick) h = (h * 31 + c.charCodeAt(0)) % 360;
	const light = theme === "light";
	return `oklch(${light ? 0.5 : 0.74} ${light ? 0.15 : 0.13} ${h})`;
}

export function fmtTime(ms) {
	const d = new Date(ms);
	return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
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
const NICK_CHARS = "A-Za-z0-9_\\-\\[\\]\\\\`^{}|";
export function mentionsMe(text, nick) {
	if (!nick) return false;
	const esc = nick.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
	return new RegExp(`(^|[^${NICK_CHARS}])${esc}($|[^${NICK_CHARS}])`, "i").test(text);
}

// linkify splits text into { link: bool, text } segments.
const URL_RE = /https?:\/\/[^\s<>"'`]+/g;
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

// isChannelName follows the network's ISUPPORT CHANTYPES (sent with the
// buffer list); "#&" is the RFC 1459 default for networks we have not
// heard from yet.
export function isChannelName(s, chantypes) {
	return !!s && (chantypes || "#&").includes(s[0]);
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
	const m = /^\/(\S+)\s*([^]*)$/.exec(input);
	const cmd = m[1].toLowerCase();
	const rest = m[2].trim();
	const err = (message) => ({ type: "error", message });

	switch (cmd) {
		case "me":
			if (!rest) return err("usage: /me <action>");
			return { type: "text", text: "\x01ACTION " + rest + "\x01" };

		case "msg":
		case "query": {
			const sp = rest.indexOf(" ");
			if (sp === -1 || !rest.slice(sp + 1).trim()) return err("usage: /msg <target> <text>");
			return { type: "msg", target: rest.slice(0, sp), text: rest.slice(sp + 1).trim() };
		}

		case "join": {
			if (!rest) return err("usage: /join <channel> [key]");
			const [chan, key] = rest.split(/\s+/);
			if (!isChannelName(chan, chantypes)) return err("/join: " + chan + " is not a channel");
			return { type: "cmd", command: "JOIN", params: key ? [chan, key] : [chan], switchTo: chan };
		}

		case "part": {
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

		case "nick":
			if (!rest || /\s/.test(rest)) return err("usage: /nick <newnick>");
			return { type: "cmd", command: "NICK", params: [rest] };

		case "topic":
			if (!isChannelName(buffer, chantypes)) return err("/topic: not in a channel");
			if (!rest) return err("usage: /topic <text>");
			return { type: "cmd", command: "TOPIC", params: [buffer, rest] };

		default:
			return err("unknown command /" + cmd);
	}
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
		return /\.(png|jpe?g|gif|webp|avif|bmp|svg)$/.test(path);
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

export function bufKey(network, buffer) {
	return network + "\n" + buffer;
}

export function parseHash(hash) {
	const m = /^#\/(.+?)\/(.+)$/.exec(hash);
	if (!m) return null;
	return { network: decodeURIComponent(m[1]), buffer: decodeURIComponent(m[2]) };
}

export function toHash(network, buffer) {
	return `#/${encodeURIComponent(network)}/${encodeURIComponent(buffer)}`;
}
