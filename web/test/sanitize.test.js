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
import { sanitizeRulesForSync } from "../src/notify.js";
import { sanitizeFiltersForSync } from "../src/local.js";

test("sanitizeRulesForSync drops only the over-cap offenders", () => {
	const ok = { pattern: "deploy", network: "", id: "a" };
	const cjk = { pattern: "重".repeat(90), network: "", id: "b" }; // 270 bytes, 90 chars
	const longNet = { pattern: "x", network: "n".repeat(129), id: "c" };
	const out = sanitizeRulesForSync([ok, cjk, longNet]);
	eq(out, [ok], "over-byte pattern and over-byte network dropped, valid kept");

	// 256 bytes exactly is allowed (mirror of the server's <= cap).
	const edge = { pattern: "重".repeat(85) + "x", network: "", id: "d" }; // 256 bytes
	eq(sanitizeRulesForSync([edge]), [edge]);

	// Row cap: 64 kept, the tail dropped.
	const many = Array.from({ length: 70 }, (_, i) => ({ pattern: `k${i}`, network: "", id: String(i) }));
	is(sanitizeRulesForSync(many).length, 64);
});

test("sanitizeFiltersForSync drops offenders, keeps the rest", () => {
	const { ignores, mutes } = sanitizeFiltersForSync(
		{
			libera: ["bob", "重".repeat(60), ""], // 180-byte nick + empty dropped
			"": ["ghost"], // empty network dropped
			oftc: ["carol"],
		},
		["libera\n#chan", "", "m".repeat(513)],
	);
	eq(ignores, { libera: ["bob"], oftc: ["carol"] });
	eq(mutes, ["libera\n#chan"]);
});

test("sanitizeFiltersForSync is a no-op on clean input", () => {
	const ig = { libera: ["bob"] };
	const mu = ["libera\n#chan"];
	const out = sanitizeFiltersForSync(ig, mu);
	// Faithful for the persist flow's stringify nothing-dropped check.
	is(JSON.stringify(out), JSON.stringify({ ignores: ig, mutes: mu }));
});
