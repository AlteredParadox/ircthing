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
import { applyFormat, BOLD, COLOR, colorCode, ITALIC, MONO, RESET, STRIKE, UNDERLINE } from "../src/format.js";
import { parseFormatting } from "../src/irc.js";

test("colorCode always emits two digits", () => {
	is(colorCode(4), "\x0304");
	is(colorCode(0), "\x0300");
	is(colorCode(13), "\x0313");
});

test("applyFormat table", () => {
	const cases = [
		// Wrap a selection: toggle before and after, selection still covers
		// the same characters (offset by the opening code).
		{
			name: "bold wraps selection",
			in: ["hello world", 0, 5, BOLD],
			want: { text: "\x02hello\x02 world", selStart: 1, selEnd: 6 },
		},
		{
			name: "italic wraps mid-string selection",
			in: ["say it loud", 4, 6, ITALIC],
			want: { text: "say \x1dit\x1d loud", selStart: 5, selEnd: 7 },
		},
		{
			name: "underline wraps to end of string",
			in: ["abc", 1, 3, UNDERLINE],
			want: { text: "a\x1fbc\x1f", selStart: 2, selEnd: 4 },
		},
		{
			name: "strike wraps",
			in: ["nope", 0, 4, STRIKE],
			want: { text: "\x1enope\x1e", selStart: 1, selEnd: 5 },
		},
		{
			name: "mono wraps",
			in: ["ls -la", 0, 6, MONO],
			want: { text: "\x11ls -la\x11", selStart: 1, selEnd: 7 },
		},
		// Caret (no selection): insert the toggle, caret lands after it.
		{
			name: "bold at caret",
			in: ["hi", 2, 2, BOLD],
			want: { text: "hi\x02", selStart: 3, selEnd: 3 },
		},
		{
			name: "bold at caret mid-string",
			in: ["hi there", 3, 3, BOLD],
			want: { text: "hi \x02there", selStart: 4, selEnd: 4 },
		},
		{
			name: "bold in empty composer",
			in: ["", 0, 0, BOLD],
			want: { text: "\x02", selStart: 1, selEnd: 1 },
		},
		// Colour: two-digit opening code, bare \x03 closes.
		{
			name: "colour wraps selection",
			in: ["red text", 0, 3, colorCode(4)],
			want: { text: "\x0304red\x03 text", selStart: 3, selEnd: 6 },
		},
		{
			name: "colour at caret",
			in: ["x", 1, 1, colorCode(13)],
			want: { text: "x\x0313", selStart: 4, selEnd: 4 },
		},
		// A closing \x03 that would sit before a digit (or ",digit") becomes
		// a full reset so the renderer can't eat the digit as a colour code.
		{
			name: "colour close before digit uses reset",
			in: ["ab55", 0, 2, colorCode(4)],
			want: { text: "\x0304ab\x0f55", selStart: 3, selEnd: 5 },
		},
		{
			name: "colour close before comma-digit uses reset",
			in: ["ab,5", 0, 2, colorCode(4)],
			want: { text: "\x0304ab\x0f,5", selStart: 3, selEnd: 5 },
		},
		{
			name: "colour close before comma-nondigit stays bare",
			in: ["ab,x", 0, 2, colorCode(4)],
			want: { text: "\x0304ab\x03,x", selStart: 3, selEnd: 5 },
		},
		// Opening a colour before a digit is safe: the code is always two
		// digits, which is all the parser consumes.
		{
			name: "two-digit colour code cannot absorb a following digit",
			in: ["5 items", 0, 0, colorCode(3)],
			want: { text: "\x03035 items", selStart: 3, selEnd: 3 },
		},
		// Reset: inserted alone; with a selection it leads the selection only.
		{
			name: "reset at caret",
			in: ["ab", 1, 1, RESET],
			want: { text: "a\x0fb", selStart: 2, selEnd: 2 },
		},
		{
			name: "reset before selection, no trailing pair",
			in: ["abcd", 1, 3, RESET],
			want: { text: "a\x0fbcd", selStart: 2, selEnd: 4 },
		},
	];
	for (const c of cases) {
		eq(applyFormat(...c.in), c.want, c.name);
	}
});

test("stacked toggles via returned selection compose correctly", () => {
	// select "hi", bold it, then italicize the SAME logical selection.
	const a = applyFormat("hi", 0, 2, BOLD);
	const b = applyFormat(a.text, a.selStart, a.selEnd, ITALIC);
	is(b.text, "\x02\x1dhi\x1d\x02");
	// Renderer round-trip: one run, both styles, clean text.
	const runs = parseFormatting(b.text);
	is(runs.length, 1);
	is(runs[0].text, "hi");
	is(runs[0].bold, true);
	is(runs[0].italic, true);
});

test("renderer round-trip: wrapped colour ends at the toggle", () => {
	const r = applyFormat("red rest", 0, 3, colorCode(4));
	const runs = parseFormatting(r.text);
	eq(runs.map((x) => [x.text, x.fg]), [["red", "#ff0000"], [" rest", null]]);
});

test("renderer round-trip: digit-guard reset keeps the digits", () => {
	const r = applyFormat("ab55", 0, 2, colorCode(4));
	const runs = parseFormatting(r.text);
	eq(runs.map((x) => [x.text, x.fg]), [["ab", "#ff0000"], ["55", null]]);
});

test("COLOR constant matches the wrapped close byte", () => {
	const r = applyFormat("abc x", 0, 3, colorCode(7));
	is(r.text[r.selEnd], COLOR);
});
