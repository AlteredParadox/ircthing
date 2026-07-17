import { deepStrictEqual as eq, strictEqual as is } from "node:assert";
import { test } from "node:test";
import { ACCENT_RGB, ACCENTS, DEFAULTS, normalizePrefs, resolveTheme } from "../src/prefs.js";

test("normalizePrefs: defaults for missing/garbage input", () => {
	eq(normalizePrefs(null), DEFAULTS);
	eq(normalizePrefs(undefined), DEFAULTS);
	eq(normalizePrefs("junk"), DEFAULTS);
	eq(normalizePrefs({}), DEFAULTS);
});

test("normalizePrefs: keeps valid values", () => {
	const full = {
		theme: "light", accent: "rose", textSize: "xl",
		density: "compact", sidebarWidth: "wide", msgFont: "mono", statusMsgs: "collapse",
		clock: "12", seconds: true, ampm: false, nickSep: ":", css: "a { color: red }",
	};
	eq(normalizePrefs(full), full);
});

test("normalizePrefs: clamps timestamp/separator prefs", () => {
	const p = normalizePrefs({ clock: "13", seconds: "yes", ampm: 1, nickSep: "::::::" });
	is(p.clock, DEFAULTS.clock); // unknown clock -> default
	is(p.seconds, DEFAULTS.seconds); // non-boolean -> default
	is(p.ampm, DEFAULTS.ampm);
	is(p.nickSep, ":::"); // clamped to MAX_NICK_SEP (3)
});

test("normalizePrefs: clamps unknown values field by field", () => {
	const p = normalizePrefs({ theme: "solarized", accent: "rose", textSize: 12, css: 5 });
	is(p.theme, DEFAULTS.theme);
	is(p.accent, "rose");
	is(p.textSize, DEFAULTS.textSize);
	is(p.css, "");
});

test("resolveTheme", () => {
	is(resolveTheme("dark", false), "dark");
	is(resolveTheme("light", true), "light");
	is(resolveTheme("system", true), "dark");
	is(resolveTheme("system", false), "light");
});

test("every accent has a swatch color", () => {
	for (const a of ACCENTS) is(typeof ACCENT_RGB[a], "string", a);
});
