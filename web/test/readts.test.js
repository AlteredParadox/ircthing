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
import { readTimestamp } from "../src/readts.js";

// The clock-skew scenario from the review finding: a server-stamped message
// at T plus a local (browser-stamped) whois/info card at T+3min. The read
// marker must advance to T only — a browser clock a few minutes ahead is
// inside the server's now+5min clamp, and a future marker would suppress
// unread badges for real messages on every device.
test("readTimestamp ignores local (browser-stamped) info rows", () => {
	const T = 1_752_000_000_000;
	const list = [
		{ id: "m1", time: T - 1000, sender: "alice", command: "PRIVMSG" },
		{ id: "m2", time: T, sender: "bob", command: "PRIVMSG" },
		{ id: "wh000000001", time: T + 3 * 60 * 1000, sender: "", command: "WHOIS", local: true },
	];
	is(readTimestamp(list), T);
});

test("readTimestamp takes the max server time, not the last-arrived", () => {
	// Backdated relay/bridge line at the tail must not rewind the marker.
	is(readTimestamp([{ time: 5 }, { time: 3 }]), 5);
	is(readTimestamp([]), 0);
});

test("readTimestamp of an all-local buffer is 0 (nothing to mark)", () => {
	// A fresh server buffer holding only MOTD/info lines has no server-stamped
	// content; 0 is falsy so chat.jsx never calls onRead for it.
	is(readTimestamp([{ time: 9, local: true }, { time: 11, local: true }]), 0);
});
