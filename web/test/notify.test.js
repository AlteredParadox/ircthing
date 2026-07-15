import { strictEqual as is } from "node:assert";
import { test } from "node:test";
import { highlightText } from "../src/notify.js";

test("highlightText: nick mention", () => {
	is(highlightText("hey AlteredParadox look", "AlteredParadox", [], "libera"), true);
	is(highlightText("category AlteredParadoxx", "AlteredParadox", [], "libera"), false, "no partial-word mention");
	is(highlightText("nothing here", "AlteredParadox", [], "libera"), false);
	is(highlightText("ALTEREDPARADOX shouted", "AlteredParadox", [], "libera"), true, "case-insensitive");
});

test("highlightText: global keyword rules", () => {
	const rules = [{ pattern: "deploy", network: "" }];
	is(highlightText("time to deploy now", "AlteredParadox", rules, "libera"), true);
	is(highlightText("Deploy the thing", "AlteredParadox", rules, "oftc"), true, "case-insensitive, any network");
	is(highlightText("nothing", "AlteredParadox", rules, "libera"), false);
});

test("highlightText: network-scoped rules", () => {
	const rules = [{ pattern: "release", network: "libera" }];
	is(highlightText("new release out", "AlteredParadox", rules, "libera"), true);
	is(highlightText("new release out", "AlteredParadox", rules, "oftc"), false, "scoped away");
});

test("highlightText: empty and blank patterns ignored", () => {
	is(highlightText("hello", "AlteredParadox", [{ pattern: "", network: "" }], "libera"), false);
	is(highlightText("", "AlteredParadox", [{ pattern: "x", network: "" }], "libera"), false);
	is(highlightText("hi", "", [], "libera"), false, "no nick, no rules");
});

test("highlightText: substring keyword match", () => {
	const rules = [{ pattern: "cat", network: "" }];
	is(highlightText("i love my cat", "AlteredParadox", rules, "libera"), true);
	is(highlightText("category theory", "AlteredParadox", rules, "libera"), true, "keywords match as substrings");
});
