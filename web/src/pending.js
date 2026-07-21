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

import { foldNick } from "./irc.js";

const JOIN_INTENT_MS = 15000;
export const MAX_PENDING_JOINS = 32;
const MAX_PENDING_PER_NETWORK = 8;
const MAX_FORWARD_ALIASES = 4;

function prunePendingJoins(queues, now) {
	for (const [network, queue] of queues) {
		for (let i = queue.length - 1; i >= 0; i--) {
			const age = now - queue[i].ts;
			// Treat a clock jump backwards as stale too; otherwise a future-dated
			// token can survive indefinitely.
			if (age < 0 || age >= JOIN_INTENT_MS) queue.splice(i, 1);
		}
		if (queue.length === 0) queues.delete(network);
	}
}

function trimGlobalOldest(queues) {
	let count = 0;
	for (const queue of queues.values()) count += queue.length;
	while (count > MAX_PENDING_JOINS) {
		let oldestNetwork = null;
		let oldest = Infinity;
		for (const [network, queue] of queues) {
			if (queue[0]?.ts < oldest) {
				oldest = queue[0].ts;
				oldestNetwork = network;
			}
		}
		if (oldestNetwork === null) return;
		const queue = queues.get(oldestNetwork);
		queue.shift();
		count--;
		if (queue.length === 0) queues.delete(oldestNetwork);
	}
}

// Pending UI joins are target-correlated per network. WebSocket admission only
// means the JOIN reached the IRC writer; an IRC numeric can still reject it, so
// an unmatched token must never consume a later, unrelated self-JOIN. A token's
// object identity matters: clearing one synchronously rejected request must not
// remove a newer request beside it. intent is the app navigation generation.
export function armPendingJoin(queues, network, target, intent, now = Date.now()) {
	prunePendingJoins(queues, now);
	const aliases = new Set(String(target || "").split(",").filter(Boolean).map(foldNick));
	const token = { network, target, aliases, intent, ts: now };
	const queue = queues.get(network);
	if (queue) {
		queue.push(token);
		while (queue.length > MAX_PENDING_PER_NETWORK) queue.shift();
	} else {
		queues.set(network, [token]);
	}
	trimGlobalOldest(queues);
	return token;
}

export function clearPendingJoin(queues, token) {
	const queue = queues.get(token.network);
	if (!queue) return;
	const i = queue.indexOf(token);
	if (i !== -1) queue.splice(i, 1);
	if (queue.length === 0) queues.delete(token.network);
}

// notePendingJoinForward correlates the 470 text's original and destination
// channels. The backend emits the numeric before the resulting self-JOIN; add
// the destination as a bounded alias so target correlation still follows a
// legitimate channel forward.
export function notePendingJoinForward(queues, network, from, to, now = Date.now()) {
	prunePendingJoins(queues, now);
	const queue = queues.get(network);
	if (!queue || !from || !to) return false;
	const oldTarget = foldNick(from);
	const newTarget = foldNick(to);
	let matched = false;
	for (const token of queue) {
		if (!token.aliases.has(oldTarget)) continue;
		matched = true;
		if (token.aliases.size < MAX_FORWARD_ALIASES) token.aliases.add(newTarget);
	}
	return matched;
}

// Consume every token matching this actual joined target and return the newest.
// Clearing duplicates together matters when an earlier attempt received only an
// asynchronous IRC rejection: it must not remain behind to steal another JOIN.
export function takePendingJoin(queues, network, target, now = Date.now()) {
	prunePendingJoins(queues, now);
	const queue = queues.get(network);
	if (!queue || !target) return null;
	const actual = foldNick(target);
	let token = null;
	for (let i = queue.length - 1; i >= 0; i--) {
		if (!queue[i].aliases.has(actual)) continue;
		if (!token) token = queue[i]; // newest match (reverse traversal)
		queue.splice(i, 1);
	}
	if (queue.length === 0) queues.delete(network);
	return token;
}
