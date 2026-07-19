import { mentionsMe, stripFormatting, uuid } from "./irc.js";

// Notifications: highlight detection, desktop Web Notifications, and a
// dynamically-drawn favicon badge. Highlight rules live in localStorage
// (per-device: you may want alerts on desktop but not your phone) — a
// future enhancement could sync them server-side for multi-device parity.

// highlightText reports whether a message should highlight: it mentions
// our nick, or matches a user highlight rule scoped to this network (or
// global). Pure and testable — the caller handles self-exclusion.
export function highlightText(text, nick, rules, network) {
	if (!text) return false;
	// Strip mIRC formatting first: a colour/bold code inside a keyword
	// ("de\x02ploy") renders invisibly but would defeat a substring rule.
	// (mentionsMe strips internally too; passing clean text is harmless.)
	const clean = stripFormatting(text);
	if (nick && mentionsMe(clean, nick)) return true;
	if (!rules) return false;
	const lower = clean.toLowerCase();
	for (const r of rules) {
		// Guard the type: this runs in the message hot path (event handler AND
		// row render), so a corrupt non-string pattern must not throw and blank
		// the chat. loadRules also sanitizes; this is defense in depth.
		if (typeof r.pattern !== "string" || !r.pattern) continue;
		if (r.network && r.network !== network) continue;
		if (lower.includes(r.pattern.toLowerCase())) return true;
	}
	return false;
}

// ---- persistence ----

export function loadRules() {
	try {
		const v = JSON.parse(localStorage.getItem("highlightRules"));
		if (!Array.isArray(v)) return [];
		// Drop corrupt entries (a hand-edited or partially-written store): a
		// non-string pattern would throw in highlightText. Stable ids key the
		// settings rows (rules are edited in place); coerce network to a string.
		return v
			.filter((r) => r && typeof r.pattern === "string")
			.map((r) => ({ ...r, network: typeof r.network === "string" ? r.network : "", id: r.id || uuid() }));
	} catch {
		return [];
	}
}

export function saveRules(rules) {
	localStorage.setItem("highlightRules", JSON.stringify(rules));
}

function loadNotifEnabled() {
	return localStorage.getItem("notifEnabled") === "1";
}

// ---- desktop notifications ----

export class Notifier {
	constructor() {
		this.enabled = loadNotifEnabled();
	}

	supported() {
		return typeof Notification !== "undefined";
	}

	permission() {
		return this.supported() ? Notification.permission : "unsupported";
	}

	// requestAndEnable prompts for permission (a user gesture must drive
	// this) and enables notifications if granted.
	async requestAndEnable() {
		if (!this.supported()) return false;
		let p = Notification.permission;
		if (p === "default") p = await Notification.requestPermission();
		this.enabled = p === "granted";
		localStorage.setItem("notifEnabled", this.enabled ? "1" : "0");
		return this.enabled;
	}

	setEnabled(on) {
		this.enabled = on && this.permission() === "granted";
		localStorage.setItem("notifEnabled", this.enabled ? "1" : "0");
	}

	// show pops a notification (no-op unless enabled and granted). The tag
	// coalesces repeated alerts for the same buffer.
	show(title, body, tag, onClick) {
		if (!this.enabled || this.permission() !== "granted") return;
		try {
			const n = new Notification(title, { body: (body || "").slice(0, 200), tag });
			n.onclick = () => {
				window.focus();
				onClick?.();
				n.close();
			};
		} catch {
			/* construction can throw on some platforms; ignore */
		}
	}
}

// ---- favicon badge + title ----

// applyBadge sets the tab title and favicon to reflect unread state:
// the total unread count, coloured red when any of it is a highlight.
export function applyBadge(unread, mention) {
	if (typeof document === "undefined") return;
	const count = unread > 99 ? "99+" : unread;
	document.title = unread > 0 ? `(${count}) ircthing` : "ircthing";
	const accent =
		getComputedStyle(document.documentElement).getPropertyValue("--accent").trim() || "#2a6fdb";
	setFaviconHref(renderFavicon(unread, mention, accent));
}

function setFaviconHref(href) {
	let link = document.querySelector('link[rel~="icon"]');
	if (!link) {
		link = document.createElement("link");
		link.rel = "icon";
		document.head.appendChild(link);
	}
	link.href = href;
}

function renderFavicon(count, mention, accent) {
	const s = 64;
	const c = document.createElement("canvas");
	c.width = c.height = s;
	const x = c.getContext("2d");

	roundRect(x, 3, 3, s - 6, s - 6, 13);
	x.fillStyle = accent;
	x.fill();

	x.fillStyle = "#ffffff";
	x.font = "bold 40px ui-monospace, monospace";
	x.textAlign = "center";
	x.textBaseline = "middle";
	x.fillText("λ", s / 2, s / 2 + 3);

	if (count > 0) {
		const r = 17;
		x.beginPath();
		x.arc(s - r, r, r, 0, 2 * Math.PI);
		x.fillStyle = mention ? "#e0403a" : "#7a828f";
		x.fill();
		x.fillStyle = "#ffffff";
		x.font = "bold 26px sans-serif";
		x.textAlign = "center";
		x.textBaseline = "middle";
		x.fillText(count > 9 ? "9+" : String(count), s - r, r + 2);
	}
	return c.toDataURL("image/png");
}

function roundRect(ctx, x, y, w, h, r) {
	ctx.beginPath();
	ctx.moveTo(x + r, y);
	ctx.arcTo(x + w, y, x + w, y + h, r);
	ctx.arcTo(x + w, y + h, x, y + h, r);
	ctx.arcTo(x, y + h, x, y, r);
	ctx.arcTo(x, y, x + w, y, r);
	ctx.closePath();
}
