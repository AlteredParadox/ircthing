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

// withFakeWS swaps in a WebSocket stub that records every constructed
// transport, so tests can drive onopen/onmessage/onclose by hand.
function withFakeWS(fn) {
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
		return fn(made);
	} finally {
		globalThis.WebSocket = previous;
	}
}

// Reconnect-storm guard: a backend that accepts the WS handshake and then
// dies immediately (proxy read timeout, crash loop) must NOT reset backoff
// on the bare open — it keeps doubling toward the cap.
test("backoff keeps doubling when connections open then immediately drop", () => {
	withFakeWS((made) => {
		const s = new Socket("ws://x");
		let stables = 0;
		s.on("_stable", () => stables++);
		try {
			s.connect();
			made[0].onopen();
			is(s.backoff, 1000); // open alone must not reset anything
			made[0].onclose();
			is(s.backoff, 2000);
			// Stand in for the reconnect timer (cleared so it can't fire later).
			clearTimeout(s.reconnectTimer);
			s.connect();
			made[1].onopen();
			made[1].onclose();
			is(s.backoff, 4000);
			is(stables, 0); // never stable, app failure counter never resets
		} finally {
			s.close();
		}
	});
});

test("first server frame marks the connection stable and resets backoff", () => {
	withFakeWS((made) => {
		const s = new Socket("ws://x");
		let stables = 0;
		let pushed = 0;
		s.on("_stable", () => stables++);
		s.on("event", () => pushed++);
		try {
			s.backoff = 8000; // several failed cycles behind us
			s.connect();
			made[0].onopen();
			is(s.backoff, 8000); // handshake alone is not stability
			is(stables, 0);
			made[0].onmessage({ data: JSON.stringify({ v: 1, seq: 0, type: "event", data: {} }) });
			is(s.backoff, 1000);
			is(stables, 1);
			is(pushed, 1); // the frame still dispatches normally
			// Further frames are idempotent for stability.
			made[0].onmessage({ data: JSON.stringify({ v: 1, seq: 0, type: "event", data: {} }) });
			is(stables, 1);
		} finally {
			s.close();
		}
	});
});

// The stability signal moved from raw frame delivery into dispatch(): only a
// parsed, version-matching envelope proves a real backend. A proxy error page
// or wrong upstream babbling at us must not reset backoff.
test("a garbage (unparseable) frame does not reset backoff or mark stable", () => {
	withFakeWS((made) => {
		const s = new Socket("ws://x");
		let stables = 0;
		s.on("_stable", () => stables++);
		try {
			s.backoff = 8000;
			s.connect();
			made[0].onopen();
			made[0].onmessage({ data: "<html>502 Bad Gateway</html>" });
			is(s.backoff, 8000);
			is(stables, 0);
			is(s.stable, false);
			// A wrong-version envelope parses but fails the version check —
			// equally not proof of OUR backend.
			made[0].onmessage({ data: JSON.stringify({ v: 999, seq: 0, type: "event", data: {} }) });
			is(s.backoff, 8000);
			is(stables, 0);
			// The first valid envelope on the same transport still stabilizes.
			made[0].onmessage({ data: JSON.stringify({ v: 1, seq: 0, type: "event", data: {} }) });
			is(s.backoff, 1000);
			is(stables, 1);
		} finally {
			s.close();
		}
	});
});

test("a connection that stays open goes stable on the timer without traffic", (t) => {
	t.mock.timers.enable({ apis: ["setTimeout"] });
	withFakeWS((made) => {
		const s = new Socket("ws://x");
		let stables = 0;
		s.on("_stable", () => stables++);
		s.backoff = 8000;
		s.connect();
		made[0].onopen();
		is(stables, 0);
		t.mock.timers.tick(5000); // STABLE_MS
		is(stables, 1);
		is(s.backoff, 1000);
		s.close();
	});
});

test("a drop before the stability timer fires never marks stable", (t) => {
	t.mock.timers.enable({ apis: ["setTimeout"] });
	withFakeWS((made) => {
		const s = new Socket("ws://x");
		let stables = 0;
		s.on("_stable", () => stables++);
		s.connect();
		made[0].onopen();
		made[0].onclose(); // dies at 0ms; must cancel the pending stability timer
		is(s.backoff, 2000);
		t.mock.timers.tick(60000); // fires the reconnect (made[1]) AND any leaked
		// stability timer from made[0] — which must have been canceled on close.
		is(stables, 0);
		made[1].onopen();
		made[1].onclose();
		is(stables, 0);
		is(s.backoff, 4000);
		s.close();
	});
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
