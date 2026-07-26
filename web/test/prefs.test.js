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
import {
	ACCENT_RGB, ACCENTS, DEFAULTS, MAX_NICK_COLORS, MAX_NICK_LEN, MAX_PREFS_BYTES, NICK_SWATCHES,
	clampPrefsToBudget, normalizeHexColor, normalizeNickColors, normalizePrefs, prefsByteLength, resolveTheme,
} from "../src/prefs.js";

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
		statusHost: true, clock: "12", seconds: true, ampm: false, nickSep: ":", highlightNames: false,
		sendTyping: false, titleUnread: false, titleChannel: true, nickPrefixes: true, purgeOnClose: true,
		mediaPlayers: false, showMemory: true,
		nickColors: { alice: "#ff0000" },
		css: "a { color: red }",
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

test("normalizePrefs: non-boolean toggles fall back to defaults", () => {
	const p = normalizePrefs({ statusHost: "yes", sendTyping: 0, titleUnread: null, titleChannel: "x", nickPrefixes: 1, purgeOnClose: "on" });
	is(p.statusHost, DEFAULTS.statusHost);
	is(p.sendTyping, DEFAULTS.sendTyping);
	is(p.titleUnread, DEFAULTS.titleUnread);
	is(p.titleChannel, DEFAULTS.titleChannel);
	is(p.nickPrefixes, DEFAULTS.nickPrefixes);
	is(p.purgeOnClose, DEFAULTS.purgeOnClose);
});

test("purgeOnClose: defaults off (closing keeps history) and round-trips", () => {
	is(DEFAULTS.purgeOnClose, false);
	is(normalizePrefs({}).purgeOnClose, false);
	is(normalizePrefs({ purgeOnClose: true }).purgeOnClose, true);
	is(normalizePrefs({ purgeOnClose: false }).purgeOnClose, false);
});

test("mediaPlayers: defaults on (still gated by the server previews switch) and round-trips", () => {
	is(DEFAULTS.mediaPlayers, true);
	is(normalizePrefs({}).mediaPlayers, true);
	is(normalizePrefs({ mediaPlayers: false }).mediaPlayers, false);
	is(normalizePrefs({ mediaPlayers: "on" }).mediaPlayers, DEFAULTS.mediaPlayers); // non-boolean -> default
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

test("custom CSS is clamped by serialized UTF-8 bytes", () => {
	const p = normalizePrefs({ css: "😀\\\"\n".repeat(30000) });
	is(prefsByteLength(p) <= MAX_PREFS_BYTES, true);
	is(p.css.length > 0, true);
	is(p.css.endsWith("\ud83d"), false, "does not split a surrogate pair");
	const again = clampPrefsToBudget(p);
	eq(again, p, "already-valid prefs are unchanged");
});

test("normalizeHexColor: canonicalizes, expands #rgb, rejects everything else", () => {
	is(normalizeHexColor("#A1B2C3"), "#a1b2c3");
	is(normalizeHexColor("a1b2c3"), "#a1b2c3", "the leading # is optional");
	is(normalizeHexColor("  #FFF  "), "#ffffff", "shorthand expands, whitespace trimmed");
	is(normalizeHexColor("#abcd"), "", "4 digits is not a color");
	is(normalizeHexColor("#12345"), "");
	is(normalizeHexColor("red"), "", "named colors are not accepted");
	// The value lands in an inline style — nothing that could carry a
	// function call or a URL may survive normalization.
	is(normalizeHexColor("var(--accent)"), "");
	is(normalizeHexColor("url(x)"), "");
	is(normalizeHexColor("#fff;background:url(x)"), "");
	is(normalizeHexColor(""), "");
	is(normalizeHexColor(null), "");
	is(normalizeHexColor(undefined), "");
	is(normalizeHexColor(0xffffff), "", "a number is not hex text");
});

test("normalizeNickColors: lowercases keys and drops invalid entries", () => {
	eq(normalizeNickColors({ Alice: "#FFF", bob: "#00ff00" }), {
		alice: "#ffffff",
		bob: "#00ff00",
	});
	eq(normalizeNickColors({ carol: "chartreuse" }), {}, "invalid color -> dropped");
	eq(normalizeNickColors({ "": "#fff" }), {}, "empty nick -> dropped");
	eq(normalizeNickColors({ "a b": "#fff" }), {}, "nicks cannot contain whitespace");
	eq(normalizeNickColors({ ["x".repeat(MAX_NICK_LEN + 1)]: "#fff" }), {}, "over-long nick -> dropped");
	eq(normalizeNickColors(null), {});
	eq(normalizeNickColors("junk"), {});
	eq(normalizeNickColors(undefined), {});
});

test("normalizeNickColors: caps the map, keeping the entries written first", () => {
	const raw = {};
	for (let i = 0; i < MAX_NICK_COLORS + 50; i++) raw["nick" + i] = "#010203";
	const out = normalizeNickColors(raw);
	is(Object.keys(out).length, MAX_NICK_COLORS);
	is(out.nick0, "#010203", "the head of the insertion order survives");
	is(out["nick" + (MAX_NICK_COLORS + 49)], undefined, "the tail is dropped");
});

test("nickColors: default empty, round-trips through normalizePrefs", () => {
	eq(DEFAULTS.nickColors, {});
	eq(normalizePrefs({}).nickColors, {});
	eq(normalizePrefs({ nickColors: { Bob: "#abc" } }).nickColors, { bob: "#aabbcc" });
	eq(normalizePrefs({ nickColors: "junk" }).nickColors, {}, "garbage -> empty, never undefined");
});

test("every picker swatch is a canonical hex color", () => {
	for (const c of NICK_SWATCHES) is(normalizeHexColor(c), c, c);
	is(new Set(NICK_SWATCHES).size, NICK_SWATCHES.length, "no duplicate swatches");
});

test("a full nick color map still leaves the prefs blob under budget", () => {
	// The cap exists so nickColors can never starve the user-CSS field, which
	// is the only thing clampPrefsToBudget trims.
	const nickColors = {};
	for (let i = 0; i < MAX_NICK_COLORS; i++) nickColors["n".repeat(MAX_NICK_LEN - 3) + i] = "#010203";
	const p = normalizePrefs({ nickColors, css: "a{color:red}" });
	is(Object.keys(p.nickColors).length, MAX_NICK_COLORS);
	is(prefsByteLength(p) <= MAX_PREFS_BYTES, true);
	is(p.css, "a{color:red}", "the CSS field survives a full nick map");
});
