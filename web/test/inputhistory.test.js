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
import { HISTORY_CAP, InputHistory, isFirstLine, isLastLine } from "../src/inputhistory.js";

const K = "net\x00#chan";
const UP = -1;
const DOWN = 1;

test("line-boundary predicates", () => {
	// [text, pos, first?, last?]
	const cases = [
		["", 0, true, true],
		["abc", 0, true, true],
		["abc", 3, true, true],
		["ab\ncd", 0, true, false],
		["ab\ncd", 2, true, false], // end of first line
		["ab\ncd", 3, false, true], // start of last line
		["ab\ncd", 5, false, true],
		["\nx", 0, true, false], // empty first line is still the first line
		["x\n", 1, true, false], // trailing newline: an empty last line exists below
		["x\n", 2, false, true],
		["a\nb\nc", 2, false, false], // middle line: neither
	];
	for (const [text, pos, first, last] of cases) {
		is(isFirstLine(text, pos), first, `isFirstLine(${JSON.stringify(text)}, ${pos})`);
		is(isLastLine(text, pos), last, `isLastLine(${JSON.stringify(text)}, ${pos})`);
	}
});

test("up recalls newest, then older; oldest is a wall", () => {
	const h = new InputHistory();
	h.push(K, "one");
	h.push(K, "two");
	h.push(K, "three");
	eq(h.navigate(K, UP, ""), { text: "three" });
	eq(h.navigate(K, UP, "three"), { text: "two" });
	eq(h.navigate(K, UP, "two"), { text: "one" });
	is(h.navigate(K, UP, "one"), null); // at the oldest: native caret move
	// and Down walks back toward newest
	eq(h.navigate(K, DOWN, "one"), { text: "two" });
	eq(h.navigate(K, DOWN, "two"), { text: "three" });
	// past the newest: clear
	eq(h.navigate(K, DOWN, "three"), { text: "" });
});

test("up with no history falls through", () => {
	const h = new InputHistory();
	is(h.navigate(K, UP, "draft"), null);
});

test("down while typing a fresh draft clears; empty composer falls through", () => {
	const h = new InputHistory();
	h.push(K, "sent");
	eq(h.navigate(K, DOWN, "half-typed"), { text: "" }); // explicit: Down clears
	is(h.navigate(K, DOWN, ""), null); // nothing to clear: native (no-op) arrow
});

test("draft is stashed nowhere: down past newest clears, never restores", () => {
	// Deliberate override of the readline convention (feature spec): Down at
	// the bottom ALWAYS ends with an empty composer, so the pre-Up draft is
	// NOT brought back.
	const h = new InputHistory();
	h.push(K, "older");
	h.push(K, "newest");
	eq(h.navigate(K, UP, "my draft"), { text: "newest" });
	eq(h.navigate(K, DOWN, "newest"), { text: "" }); // not "my draft"
	// and navigation has fully ended: next Up starts at the newest again
	eq(h.navigate(K, UP, ""), { text: "newest" });
});

test("up-up-down-down-down full sequence ends empty", () => {
	const h = new InputHistory();
	h.push(K, "a");
	h.push(K, "b");
	eq(h.navigate(K, UP, "wip"), { text: "b" });
	eq(h.navigate(K, UP, "b"), { text: "a" });
	eq(h.navigate(K, DOWN, "a"), { text: "b" });
	eq(h.navigate(K, DOWN, "b"), { text: "" });
	is(h.navigate(K, DOWN, ""), null);
});

test("editing a recalled entry makes it a fresh draft", () => {
	const h = new InputHistory();
	h.push(K, "one");
	h.push(K, "two");
	eq(h.navigate(K, UP, ""), { text: "two" });
	// user edits "two" -> "two!" then presses Up: not "one" — the edited
	// text is a fresh draft, so Up starts over at the newest entry.
	eq(h.navigate(K, UP, "two!"), { text: "two" });
	// user edits then presses Down: fresh-draft rule clears.
	eq(h.navigate(K, DOWN, "two!"), { text: "" });
});

test("consecutive duplicates collapse; non-consecutive repeats do not", () => {
	const h = new InputHistory();
	h.push(K, "same");
	h.push(K, "same");
	h.push(K, "other");
	h.push(K, "same");
	eq(h.navigate(K, UP, ""), { text: "same" });
	eq(h.navigate(K, UP, "same"), { text: "other" });
	eq(h.navigate(K, UP, "other"), { text: "same" });
	is(h.navigate(K, UP, "same"), null); // only three entries
});

test("cap drops the oldest entries", () => {
	const h = new InputHistory(3);
	for (const t of ["1", "2", "3", "4", "5"]) h.push(K, t);
	eq(h.navigate(K, UP, ""), { text: "5" });
	eq(h.navigate(K, UP, "5"), { text: "4" });
	eq(h.navigate(K, UP, "4"), { text: "3" });
	is(h.navigate(K, UP, "3"), null); // "1" and "2" were dropped
});

test("default cap is 50", () => {
	is(HISTORY_CAP, 50);
	const h = new InputHistory();
	for (let i = 0; i < 60; i++) h.push(K, "m" + i);
	// walk to the oldest surviving entry
	let cur = "";
	let last = null;
	for (;;) {
		const r = h.navigate(K, UP, cur);
		if (!r) break;
		last = r.text;
		cur = r.text;
	}
	is(last, "m10"); // 60 pushed, oldest 10 dropped
});

test("empty push is ignored", () => {
	const h = new InputHistory();
	h.push(K, "");
	is(h.navigate(K, UP, ""), null);
});

test("push ends navigation", () => {
	const h = new InputHistory();
	h.push(K, "a");
	eq(h.navigate(K, UP, ""), { text: "a" });
	h.push(K, "b"); // sent while a recalled entry was showing
	eq(h.navigate(K, UP, ""), { text: "b" }); // starts over at the newest
});

test("per-buffer isolation", () => {
	const h = new InputHistory();
	const K2 = "net\x00#other";
	h.push(K, "chan message");
	is(h.navigate(K2, UP, ""), null); // other buffer: no history
	h.push(K2, "other message");
	eq(h.navigate(K2, UP, ""), { text: "other message" });
	// navigation state is independent too: K is mid-navigation, K2 isn't
	eq(h.navigate(K, UP, ""), { text: "chan message" });
	eq(h.navigate(K2, DOWN, "other message"), { text: "" });
	eq(h.navigate(K, DOWN, "chan message"), { text: "" });
});

test("multiline entries recall whole", () => {
	const h = new InputHistory();
	h.push(K, "line1\nline2");
	eq(h.navigate(K, UP, ""), { text: "line1\nline2" });
});
