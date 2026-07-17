import { strictEqual as is } from "node:assert";
import { test } from "node:test";
import { Socket } from "../src/ws.js";

// dispatch() routing: replies (seq set) settle their pending request and
// are never delivered to push handlers; pushes (no seq) fan out to
// handlers. This guards the data-integrity fix where a late get_prefs
// reply — same type as the prefs push — must not reach the push handler.
test("dispatch settles a pending reply and never emits it as a push", () => {
	const s = new Socket("ws://x");
	let pushed = 0;
	s.on("prefs", () => pushed++);
	let resolved;
	s.pending.set(7, { resolve: (d) => (resolved = d), reject: () => {}, timer: setTimeout(() => {}, 0) });

	s.dispatch({ v: 1, seq: 7, type: "prefs", data: { prefs: { a: 1 } } });
	is(resolved.prefs.a, 1); // request resolved
	is(pushed, 0); // NOT delivered to the push handler
	is(s.pending.has(7), false); // pending cleared
});

test("dispatch drops a late reply (no pending) instead of emitting it", () => {
	const s = new Socket("ws://x");
	let pushed = 0;
	s.on("prefs", () => pushed++);
	// seq set but nothing pending (request already timed out).
	s.dispatch({ v: 1, seq: 42, type: "prefs", data: { prefs: { a: 1 } } });
	is(pushed, 0); // dropped, not misdelivered to the push handler
});

test("dispatch fans a seq-less push out to handlers", () => {
	const s = new Socket("ws://x");
	let got;
	s.on("event", (d) => (got = d));
	s.dispatch({ v: 1, seq: 0, type: "event", data: { x: 5 } });
	is(got.x, 5);
});

test("dispatch rejects an error reply", () => {
	const s = new Socket("ws://x");
	let err;
	s.pending.set(3, { resolve: () => {}, reject: (e) => (err = e), timer: setTimeout(() => {}, 0) });
	s.dispatch({ v: 1, seq: 3, type: "error", data: { message: "nope", code: "bad" } });
	is(err.message, "nope");
	is(err.code, "bad");
});

test("dispatch ignores a wrong protocol version", () => {
	const s = new Socket("ws://x");
	let pushed = 0;
	s.on("event", () => pushed++);
	s.dispatch({ v: 999, seq: 0, type: "event", data: {} });
	is(pushed, 0);
});
