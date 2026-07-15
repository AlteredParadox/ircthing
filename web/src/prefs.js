// Appearance preferences: theme, accent color, text size, density,
// message font, and a raw user-CSS override. Stored per browser in
// localStorage and applied as data attributes / CSS custom properties on
// <html> — the stylesheet keys everything off those (see style.css).

export const THEMES = ["system", "dark", "light"];
export const ACCENTS = ["blue", "violet", "teal", "green", "amber", "rose"];
export const TEXT_SIZES = ["sm", "md", "lg", "xl"];
export const DENSITIES = ["compact", "cozy", "comfortable"];
export const MSG_FONTS = ["sans", "mono"];

// Swatch colors shown in settings; must match the data-accent blocks in
// style.css.
export const ACCENT_RGB = {
	blue: "42 111 219",
	violet: "124 92 230",
	teal: "16 150 138",
	green: "47 158 89",
	amber: "200 121 20",
	rose: "219 72 120",
};

export const DEFAULTS = {
	theme: "system",
	accent: "blue",
	textSize: "md",
	density: "cozy",
	msgFont: "sans",
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
		msgFont: pick(p.msgFont, MSG_FONTS, DEFAULTS.msgFont),
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
