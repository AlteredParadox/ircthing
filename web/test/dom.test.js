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
import { isEditable, modalScrimOpen, typeAnywhereKey } from "../src/dom.js";

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

// typeAnywhereKey runs against a stubbed document: it only reads
// document.body, and `closest` is stubbed per focused element as the
// selector-matching answer.
function withBody(fn) {
	const prevDoc = globalThis.document;
	const body = { tagName: "BODY" };
	globalThis.document = { body };
	try {
		return fn(body);
	} finally {
		globalThis.document = prevDoc;
	}
}
const key = (k, mods = {}) => ({ key: k, ...mods });
const button = { tagName: "BUTTON", closest: (sel) => (sel.includes("button") ? button : null) };
const roleRow = { tagName: "DIV", closest: (sel) => (sel.includes("[role=button]") ? roleRow : null) };
const plainDiv = { tagName: "DIV", closest: () => null };

test("typeAnywhereKey routes printable keys from non-editable focus", () => {
	withBody((body) => {
		is(typeAnywhereKey(key("a"), body), true);
		is(typeAnywhereKey(key("a"), null), true);
		is(typeAnywhereKey(key("a"), plainDiv), true);
		is(typeAnywhereKey(key(" "), plainDiv), true, "space on a plain element still types");
		is(typeAnywhereKey(key("a"), button), true, "letters on a button still type");
		is(typeAnywhereKey(key("a"), { tagName: "INPUT" }), false);
		is(typeAnywhereKey(key("a"), { tagName: "DIV", isContentEditable: true }), false);
		is(typeAnywhereKey(key("Enter"), body), false);
		is(typeAnywhereKey(key("a", { ctrlKey: true }), body), false);
		is(typeAnywhereKey(key("a", { isComposing: true }), body), false);
	});
});

test("typeAnywhereKey leaves Space to a focused button-like element", () => {
	// Space is a native <button>'s activation key (fires on keyup) and the
	// pressable/menuTrigger rows' keydown activation; yanking focus into
	// the composer on keydown dropped the activation and typed a space.
	withBody(() => {
		is(typeAnywhereKey(key(" "), button), false);
		is(typeAnywhereKey(key(" "), roleRow), false);
	});
});
