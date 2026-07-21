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
	dropPurgedRows,
	emptySearchPurge,
	recordBufferClosed,
	recordNetworkRemoved,
} from "../src/irc.js";

// Rows across two networks; #a exists on both to prove purges match the
// full (network, buffer) identity, not the bare buffer name.
const rows = () => [
	{ id: 1, network: "libera", buffer: "#a", raw: "one" },
	{ id: 2, network: "libera", buffer: "#b", raw: "two" },
	{ id: 3, network: "oftc", buffer: "#a", raw: "three" },
];

const ids = (rs) => rs.map((r) => r.id);

test("a destructive buffer close drops that buffer's rows only", () => {
	const p = emptySearchPurge();
	recordBufferClosed(p, { network: "libera", buffer: "#a", purge: true });
	eq(ids(dropPurgedRows(rows(), p)), [2, 3]); // oftc/#a survives
});

test("an absent purge field is treated as purge (old server, safe direction)", () => {
	const p = emptySearchPurge();
	recordBufferClosed(p, { network: "libera", buffer: "#b" });
	eq(ids(dropPurgedRows(rows(), p)), [1, 3]);
});

test("an archive close (purge:false) drops nothing", () => {
	const p = emptySearchPurge();
	recordBufferClosed(p, { network: "libera", buffer: "#a", purge: false });
	const input = rows();
	const out = dropPurgedRows(input, p);
	eq(ids(out), [1, 2, 3]);
	is(out, input); // unchanged input array, so a state setter is a no-op
});

test("network removal drops all that network's rows", () => {
	const p = emptySearchPurge();
	recordNetworkRemoved(p, { network: "libera" });
	eq(ids(dropPurgedRows(rows(), p)), [3]);
});

test("purges accumulate across events", () => {
	const p = emptySearchPurge();
	recordBufferClosed(p, { network: "libera", buffer: "#b", purge: true });
	recordNetworkRemoved(p, { network: "oftc" });
	eq(ids(dropPurgedRows(rows(), p)), [1]);
});

test("an empty accumulator returns the input array unchanged", () => {
	const input = rows();
	is(dropPurgedRows(input, emptySearchPurge()), input);
});
