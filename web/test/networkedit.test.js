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

import { match, strictEqual as is, throws } from "node:assert";
import { test } from "node:test";
import { editableNetwork, networkEditError } from "../src/networkedit.js";

test("editableNetwork accepts only the exact usable definition", () => {
	const item = { name: "wanted", state: "registered", config: { addr: "x:1" } };
	is(editableNetwork({ network: item }, "wanted"), item);
	for (const data of [null, {}, { network: null }, { network: [] }]) {
		throws(() => editableNetwork(data, "wanted"), (e) => e.code === "unavailable");
	}
	throws(
		() => editableNetwork({ network: { ...item, name: "other" } }, "wanted"),
		(e) => e.code === "unavailable",
	);
});

test("editableNetwork preserves every legacy recovery reason", () => {
	for (const code of ["invalid_name", "oversized", "invalid"]) {
		throws(
			() => editableNetwork({ network: { name: "bad", [code]: true } }, "bad"),
			(e) => e.code === code,
		);
	}
	throws(
		() => editableNetwork({ network: { name: "bad", config: [] } }, "bad"),
		(e) => e.code === "unavailable",
	);
});

test("networkEditError gives recovery rows an actionable path", () => {
	match(networkEditError("huge", { code: "oversized" }), /64 KiB.*Remove network/s);
	match(networkEditError("legacy", { code: "invalid_name" }), /invalid.*Remove network/s);
	match(networkEditError("broken", { code: "invalid" }), /invalid.*Remove network/s);
	is(networkEditError("gone", { code: "unknown_network" }), "gone: network no longer exists.");
});
