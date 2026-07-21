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
import {
	armPendingJoin, clearPendingJoin, MAX_PENDING_JOINS,
	notePendingJoinForward, takePendingJoin,
} from "../src/pending.js";

test("pending joins are target-correlated and isolated per network", () => {
	const queues = new Map();
	const a1 = armPendingJoin(queues, "a", "#one", 1, 100);
	const b1 = armPendingJoin(queues, "b", "#one", 2, 101);
	const a2 = armPendingJoin(queues, "a", "#two", 3, 102);
	is(takePendingJoin(queues, "a", "#two", 200), a2);
	is(takePendingJoin(queues, "b", "#one", 200), b1);
	is(takePendingJoin(queues, "a", "#one", 200), a1);
	is(queues.size, 0);
});

test("a rejection clears its exact pending-join token", () => {
	const queues = new Map();
	const first = armPendingJoin(queues, "a", "#first", 1, 100);
	const rejected = armPendingJoin(queues, "a", "#rejected", 2, 101);
	const last = armPendingJoin(queues, "a", "#last", 3, 102);
	clearPendingJoin(queues, rejected);
	is(takePendingJoin(queues, "a", "#first", 200), first);
	is(takePendingJoin(queues, "a", "#last", 200), last);
	is(takePendingJoin(queues, "a", "#rejected", 200), null);
});

test("expired join tokens cannot consume a later self-JOIN", () => {
	const queues = new Map();
	armPendingJoin(queues, "a", "#old", 1, 100);
	const fresh = armPendingJoin(queues, "a", "#fresh", 2, 15050);
	is(takePendingJoin(queues, "a", "#fresh", 15100), fresh);
	is(queues.has("a"), false);
});

test("a 470 forward aliases only the matching pending target", () => {
	const queues = new Map();
	const forwarded = armPendingJoin(queues, "a", "#from", 7, 100);
	armPendingJoin(queues, "a", "#other", 8, 101);
	is(notePendingJoinForward(queues, "a", "#from", "##landing", 110), true);
	is(takePendingJoin(queues, "a", "##landing", 120), forwarded);
	is(takePendingJoin(queues, "a", "#other", 120).intent, 8);
});

test("duplicate attempts for one target collapse on the actual JOIN", () => {
	const queues = new Map();
	armPendingJoin(queues, "a", "#same", 1, 100); // async IRC rejection
	const latest = armPendingJoin(queues, "a", "#same", 2, 101);
	is(takePendingJoin(queues, "a", "#same", 110), latest);
	is(takePendingJoin(queues, "a", "#same", 110), null);
});

test("pending joins have a hard global bound", () => {
	const queues = new Map();
	for (let i = 0; i < MAX_PENDING_JOINS + 20; i++) {
		armPendingJoin(queues, `n${i}`, `#${i}`, i, 100 + i);
	}
	let count = 0;
	for (const queue of queues.values()) count += queue.length;
	is(count, MAX_PENDING_JOINS);
	is(takePendingJoin(queues, "n0", "#0", 200), null, "oldest token was evicted");
});
