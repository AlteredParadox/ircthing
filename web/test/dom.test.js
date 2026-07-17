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
