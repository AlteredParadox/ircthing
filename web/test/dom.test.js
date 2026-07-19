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
import { isEditable, modalScrimOpen } from "../src/dom.js";

// Stub the two DOM globals modalScrimOpen touches. scrims is a list of
// {display} objects; querySelectorAll returns them and getComputedStyle
// echoes each one's display.
function withScrims(scrims, fn) {
	const prevDoc = globalThis.document;
	const prevGcs = globalThis.getComputedStyle;
	globalThis.document = { querySelectorAll: () => scrims };
	globalThis.getComputedStyle = (el) => ({ display: el.display });
	try {
		return fn();
	} finally {
		globalThis.document = prevDoc;
		globalThis.getComputedStyle = prevGcs;
	}
}

test("modalScrimOpen ignores display:none scrims", () => {
	// No scrims at all.
	is(withScrims([], modalScrimOpen), false);
	// Desktop: side/right scrims are in the DOM but display:none — the bug
	// was treating these as an open modal, killing type-anywhere.
	is(withScrims([{ display: "none" }, { display: "none" }], modalScrimOpen), false);
	// A genuinely visible modal (search palette / mobile drawer) bails.
	is(withScrims([{ display: "none" }, { display: "flex" }], modalScrimOpen), true);
	is(withScrims([{ display: "block" }], modalScrimOpen), true);
});

test("isEditable", () => {
	is(isEditable({ tagName: "TEXTAREA" }), true);
	is(isEditable({ tagName: "INPUT" }), true);
	is(isEditable({ tagName: "SELECT" }), true);
	is(isEditable({ tagName: "DIV", isContentEditable: true }), true);
	is(isEditable({ tagName: "DIV", isContentEditable: false }), false);
	is(isEditable({ tagName: "BODY", isContentEditable: false }), false);
});
