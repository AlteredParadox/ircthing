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

test("close synchronously rejects and clears every pending request", () => {
	const s = new Socket("ws://x");
	let rejected;
	s.pending.set(9, {
		resolve: () => {},
		reject: (e) => (rejected = e),
		timer: setTimeout(() => {}, 10000),
	});
	s.close();
	is(rejected.message, "disconnected");
	is(s.pending.size, 0);
});

test("a closed socket suppresses queued replies and pushes", () => {
	const s = new Socket("ws://x");
	let pushed = 0;
	let resolved = false;
	s.on("event", () => pushed++);
	s.pending.set(4, {
		resolve: () => (resolved = true),
		reject: () => {},
		timer: setTimeout(() => {}, 10000),
	});
	s.close();
	s.dispatch({ v: 1, seq: 0, type: "event", data: {} });
	s.dispatch({ v: 1, seq: 4, type: "ok", data: {} });
	is(pushed, 0);
	is(resolved, false);
});

test("a superseded transport close cannot tear down the current transport", () => {
	const previous = globalThis.WebSocket;
	const made = [];
	class FakeWebSocket {
		static OPEN = 1;
		constructor() {
			this.readyState = FakeWebSocket.OPEN;
			made.push(this);
		}
		close() {}
	}
	globalThis.WebSocket = FakeWebSocket;
	try {
		const s = new Socket("ws://x");
		let closes = 0;
		let rejected = false;
		s.on("_close", () => closes++);
		s.connect();
		const old = made[0];
		s.connect();
		const current = made[1];
		s.pending.set(11, {
			resolve: () => {},
			reject: () => (rejected = true),
			timer: setTimeout(() => {}, 10000),
		});
		old.onclose();
		is(s.ws, current);
		is(rejected, false);
		is(s.pending.has(11), true);
		is(closes, 0);
		s.close();
	} finally {
		globalThis.WebSocket = previous;
	}
});
