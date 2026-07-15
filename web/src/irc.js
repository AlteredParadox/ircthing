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
