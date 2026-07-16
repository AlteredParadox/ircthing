import { deepStrictEqual as eq, strictEqual as is } from "node:assert";
import { test } from "node:test";
import { Completer, completions } from "../src/complete.js";

const nicks = ["alice", "Alfred", "bob", "alice"]; // dupe on purpose

test("completions: commands only at the start of the line", () => {
	eq(completions("/pa", 3, {}), { start: 0, options: ["/part "] });
	eq(completions("/m", 2, {}).options, ["/me ", "/msg "]);
	is(completions("say /pa", 7, { nicks }), null, "mid-line /pa is not a command");
	is(completions("/zzz", 4, {}), null);
});

test("completions: emoji shortcodes", () => {
	eq(completions("nice :fir", 9, {}), { start: 5, options: ["🔥 ", "🎆 "] });
	eq(completions(":shrug", 6, {}), { start: 0, options: ["🤷 "] });
	is(completions(":", 1, {}), null, "bare colon completes nothing");
	is(completions(":notanemoji", 11, {}), null);
});

test("completions: nicks, with position-dependent suffix", () => {
	eq(completions("al", 2, { nicks }), { start: 0, options: ["alice: ", "Alfred: "] });
	eq(completions("hey al", 6, { nicks }), { start: 4, options: ["alice ", "Alfred "] });
	is(completions("zz", 2, { nicks }), null);
	is(completions("hey ", 4, { nicks }), null, "empty token completes nothing");
});

test("completions: only the token before the caret matters", () => {
	// Caret in the middle: "al|ice and bob"
	eq(completions("alice and bob", 2, { nicks }).options, ["alice: ", "Alfred: "]);
});

test("Completer: Tab cycles, Shift+Tab reverses, edits reset", () => {
	const c = new Completer();
	let r = c.next("al", 2, 1, { nicks });
	eq(r, { text: "alice: ", caret: 7 });
	r = c.next(r.text, r.caret, 1, { nicks });
	eq(r, { text: "Alfred: ", caret: 8 });
	r = c.next(r.text, r.caret, 1, { nicks });
	eq(r, { text: "alice: ", caret: 7 }, "wraps around");
	r = c.next(r.text, r.caret, -1, { nicks });
	eq(r, { text: "Alfred: ", caret: 8 }, "shift+tab goes back");

	// Any edit starts a fresh completion from the new token.
	r = c.next("Alfred: b", 9, 1, { nicks });
	eq(r, { text: "Alfred: bob ", caret: 12 });
});

test("Completer: nothing to complete returns null and resets", () => {
	const c = new Completer();
	is(c.next("zz", 2, 1, { nicks }), null);
	const r = c.next("al", 2, 1, { nicks });
	eq(r, { text: "alice: ", caret: 7 });
});

test("Completer: shift+tab first starts from the last candidate", () => {
	const c = new Completer();
	const r = c.next("al", 2, -1, { nicks });
	eq(r, { text: "Alfred: ", caret: 8 });
});
