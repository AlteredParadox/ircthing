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
		clock: "12", seconds: true, ampm: false, nickSep: ":", highlightNames: false, css: "a { color: red }",
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
