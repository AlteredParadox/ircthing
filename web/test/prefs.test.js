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
	const p = normalizePrefs({
		theme: "light", accent: "rose", textSize: "xl",
		density: "compact", msgFont: "mono", statusMsgs: "collapse", css: "a { color: red }",
	});
	eq(p, {
		theme: "light", accent: "rose", textSize: "xl",
		density: "compact", msgFont: "mono", statusMsgs: "collapse", css: "a { color: red }",
	});
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
