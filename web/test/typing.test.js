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
	clearTypingNick,
	expireTypingState,
	MAX_TYPERS_PER_BUFFER,
	MAX_TYPING_BUFFERS,
	TypingSender,
	typingExpired,
	typingText,
	updateTypingState,
} from "../src/irc.js";

// harness: TypingSender with a manual clock, recording notifications.
function sender(startAt = 100000) {
	const sent = [];
	let now = startAt;
	const s = new TypingSender((state) => sent.push({ state, at: now }), () => now);
	return {
		s,
		sent,
		tick(ms) {
			now += ms;
		},
	};
}

test("active is throttled to one notification per 3s", () => {
	const h = sender();
	h.s.input("h");
	h.tick(500);
	h.s.input("he");
	h.tick(500);
	h.s.input("hel");
	eq(h.sent.map((x) => x.state), ["active"]);
	h.tick(2500); // 3.5s since the first notification
	h.s.input("hell");
	eq(h.sent.map((x) => x.state), ["active", "active"]);
});

test("paused fires once while text rests, active resumes after", () => {
	const h = sender();
	h.s.input("hello");
	h.tick(5000);
	h.s.pause("hello");
	h.s.pause("hello"); // idempotent
	eq(h.sent.map((x) => x.state), ["active", "paused"]);
	h.tick(3100);
	h.s.input("hello!");
	eq(h.sent.map((x) => x.state), ["active", "paused", "active"]);
});

test("clearing the input sends done once", () => {
	const h = sender();
	h.s.input("hello");
	h.tick(100);
	h.s.input("");
	h.s.input("");
	eq(h.sent.map((x) => x.state), ["active", "done"]);
});

test("slash commands never trigger typing", () => {
	const h = sender();
	h.s.input("/");
	h.s.input("/join #go");
	eq(h.sent, []);
	// Text that becomes a command ends the session with done.
	h.s.input("hello");
	h.tick(3100);
	h.s.input("/me hello");
	eq(h.sent.map((x) => x.state), ["active", "done"]);
});

test("pause on a slash command or empty text is a no-op", () => {
	const h = sender();
	h.s.input("hello");
	h.s.pause("/join #x");
	h.s.pause("");
	eq(h.sent.map((x) => x.state), ["active"]);
});

test("messageSent ends the session without a notification", () => {
	const h = sender();
	h.s.input("hello");
	h.s.messageSent();
	h.s.input(""); // no done: the session already ended
	eq(h.sent.map((x) => x.state), ["active"]);
	// New composing session resumes (after the throttle window).
	h.tick(3100);
	h.s.input("again");
	eq(h.sent.map((x) => x.state), ["active", "active"]);
});

test("typingText wording", () => {
	is(typingText([]), "");
	is(typingText(["alice"]), "alice is typing…");
	is(typingText(["alice", "bob"]), "alice and bob are typing…");
	is(typingText(["a", "b", "c"]), "several people are typing…");
});

test("typingExpired per spec windows", () => {
	is(typingExpired("active", 1000, 6500), false);
	is(typingExpired("active", 1000, 7100), true);
	is(typingExpired("paused", 1000, 30500), false);
	is(typingExpired("paused", 1000, 31100), true);
});

test("typing state ignores unknown buffers and stray done events", () => {
	const empty = new Map();
	is(updateTypingState(empty, "net\n#unknown", { nick: "alice", state: "active" }, false, 1), empty);
	is(updateTypingState(empty, "net\n#known", { nick: "__proto__", state: "done" }, true, 1), empty);
	const active = updateTypingState(empty, "net\n#known", { nick: "__proto__", state: "active" }, true, 1);
	is(active.get("net\n#known").has("__proto__"), true, "Map safely stores special names");
	is(clearTypingNick(active, "net\n#known", "__proto__").size, 0, "last nick removes outer entry");
});

test("typing state caps both dimensions and expires empty entries", () => {
	let all = new Map();
	for (let i = 0; i < MAX_TYPING_BUFFERS; i++) {
		all = updateTypingState(all, `n\n#${i}`, { nick: "a", state: "active" }, true, 1);
	}
	is(all.size, MAX_TYPING_BUFFERS);
	is(updateTypingState(all, "n\n#overflow", { nick: "a", state: "active" }, true, 1), all);

	let one = new Map();
	for (let i = 0; i < MAX_TYPERS_PER_BUFFER; i++) {
		one = updateTypingState(one, "n\n#c", { nick: `n${i}`, state: "paused" }, true, 1);
	}
	is(one.get("n\n#c").size, MAX_TYPERS_PER_BUFFER);
	is(updateTypingState(one, "n\n#c", { nick: "overflow", state: "active" }, true, 1), one);
	is(expireTypingState(one, 40000).size, 0, "expiry removes the now-empty buffer");
});
