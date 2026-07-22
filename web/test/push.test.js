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

import { deepStrictEqual as eq } from "node:assert";
import { test } from "node:test";
import { urlB64ToBytes } from "../src/push.js";

test("urlB64ToBytes decodes unpadded base64url with URL-safe alphabet", () => {
	// "BP4" prefix exercises '-'/'_' translation and padding restoration.
	eq(Array.from(urlB64ToBytes("AQID")), [1, 2, 3]);
	eq(Array.from(urlB64ToBytes("_v8")), [0xfe, 0xff]); // needs 1 pad char
	eq(Array.from(urlB64ToBytes("-_8B")), [0xfb, 0xff, 0x01]);
	eq(Array.from(urlB64ToBytes("")), []);
	// Round-trip a VAPID-key-sized (65-byte) value.
	const bytes = Array.from({ length: 65 }, (_, i) => (i * 7) % 256);
	const b64 = Buffer.from(bytes).toString("base64url");
	eq(Array.from(urlB64ToBytes(b64)), bytes);
});
