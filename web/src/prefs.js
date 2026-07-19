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

// Appearance preferences: theme, accent color, text size, density,
// message font, and a raw user-CSS override. The server's settings table
// is the source of truth (prefs sync across devices via get_prefs /
// set_prefs / "prefs" pushes — see app.jsx); localStorage is a
// write-through cache so the first paint has the right theme before the
// socket connects. Applied as data attributes / CSS custom properties on
// <html> — the stylesheet keys everything off those (see style.css).

export const THEMES = ["system", "dark", "light"];
export const ACCENTS = ["blue", "violet", "teal", "green", "amber", "rose", "slate", "gray"];
export const TEXT_SIZES = ["sm", "md", "lg", "xl"];
export const DENSITIES = ["compact", "cozy", "comfortable"];
export const MSG_FONTS = ["sans", "mono"];
// Left sidebar width — the channel/network list. Maps to --sidebar-width in
// the stylesheet (see the [data-sidebar] rules).
export const SIDEBAR_WIDTHS = ["compact", "comfortable", "wide"];
// How join/part/quit/nick lines appear in the message window (The
// Lounge's status-message setting): shown, collapsed into one summary
// row per run, or hidden entirely.
export const STATUS_MSGS = ["show", "collapse", "hide"];
// Clock style for message timestamps: 24-hour ("14:05") or 12-hour
// ("02:05 PM"). Seconds and the AM/PM suffix are separate toggles.
export const CLOCKS = ["24", "12"];
// Longest allowed nick/message separator (e.g. ":"); a few chars is plenty
// and bounds a hand-edited pref.
export const MAX_NICK_SEP = 3;

// Swatch colors shown in settings; must match the data-accent blocks in
// style.css.
export const ACCENT_RGB = {
	blue: "42 111 219",
	violet: "124 92 230",
	teal: "16 150 138",
	green: "47 158 89",
	amber: "200 121 20",
	rose: "219 72 120",
	slate: "100 116 139",
	gray: "115 115 118",
};

export const DEFAULTS = {
	theme: "system",
	accent: "blue",
	textSize: "md",
	density: "cozy",
	sidebarWidth: "comfortable",
	msgFont: "sans",
	statusMsgs: "show",
	clock: "24",
	seconds: false,
	ampm: true,
	nickSep: "",
	highlightNames: true,
	css: "",
};

// normalizePrefs clamps unknown/missing values to defaults so a stale or
// hand-edited localStorage entry can never wedge the UI.
export function normalizePrefs(raw) {
	const p = raw && typeof raw === "object" ? raw : {};
	const pick = (v, allowed, def) => (allowed.includes(v) ? v : def);
	return {
		theme: pick(p.theme, THEMES, DEFAULTS.theme),
		accent: pick(p.accent, ACCENTS, DEFAULTS.accent),
		textSize: pick(p.textSize, TEXT_SIZES, DEFAULTS.textSize),
		density: pick(p.density, DENSITIES, DEFAULTS.density),
		sidebarWidth: pick(p.sidebarWidth, SIDEBAR_WIDTHS, DEFAULTS.sidebarWidth),
		msgFont: pick(p.msgFont, MSG_FONTS, DEFAULTS.msgFont),
		statusMsgs: pick(p.statusMsgs, STATUS_MSGS, DEFAULTS.statusMsgs),
		clock: pick(p.clock, CLOCKS, DEFAULTS.clock),
		seconds: typeof p.seconds === "boolean" ? p.seconds : DEFAULTS.seconds,
		ampm: typeof p.ampm === "boolean" ? p.ampm : DEFAULTS.ampm,
		nickSep: typeof p.nickSep === "string" ? p.nickSep.slice(0, MAX_NICK_SEP) : DEFAULTS.nickSep,
		highlightNames: typeof p.highlightNames === "boolean" ? p.highlightNames : DEFAULTS.highlightNames,
		css: typeof p.css === "string" ? p.css : DEFAULTS.css,
	};
}

// resolveTheme turns the theme preference into the concrete theme to
// render ("dark" | "light").
export function resolveTheme(pref, systemDark) {
	if (pref === "dark" || pref === "light") return pref;
	return systemDark ? "dark" : "light";
}

export function loadPrefs() {
	let raw = null;
	try {
		raw = JSON.parse(localStorage.getItem("prefs"));
	} catch {
		/* corrupted entry — fall through to defaults */
	}
	const p = normalizePrefs(raw);
	// Migrate the pre-prefs "theme" key (an explicit dark/light choice).
	if (!raw) {
		const legacy = localStorage.getItem("theme");
		if (legacy === "dark" || legacy === "light") p.theme = legacy;
	}
	return p;
}

export function savePrefs(p) {
	localStorage.setItem("prefs", JSON.stringify(p));
	localStorage.removeItem("theme");
}

// applyPrefs stamps the resolved preferences onto <html> and injects the
// user CSS override as the last <style> in <head> so it wins the cascade.
export function applyPrefs(p, resolvedTheme) {
	const root = document.documentElement;
	root.dataset.theme = resolvedTheme;
	root.dataset.accent = p.accent;
	root.dataset.textsize = p.textSize;
	root.dataset.density = p.density;
	root.dataset.sidebar = p.sidebarWidth;
	root.dataset.msgfont = p.msgFont;
	let el = document.getElementById("user-css");
	if (!el) {
		el = document.createElement("style");
		el.id = "user-css";
		document.head.appendChild(el);
	}
	if (el.textContent !== p.css) el.textContent = p.css;
	// Keep it last so user rules override ours at equal specificity.
	if (el !== document.head.lastElementChild) document.head.appendChild(el);
}
