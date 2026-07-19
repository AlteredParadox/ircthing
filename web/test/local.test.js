import { deepStrictEqual as eq, strictEqual as is } from "node:assert";
import { afterEach, test } from "node:test";

// Minimal localStorage shim for the persistence helpers.
const store = new Map();
globalThis.localStorage = {
	getItem: (k) => (store.has(k) ? store.get(k) : null),
	setItem: (k, v) => store.set(k, String(v)),
	removeItem: (k) => store.delete(k),
};

const {
	loadIgnores, isIgnored, toggleIgnore, ignoredFor,
	loadMutes, isMuted, toggleMute,
} = await import("../src/local.js");

afterEach(() => store.clear());

test("ignores: toggle, fold, persist, prune", () => {
	let ig = loadIgnores();
	eq(ig, {});
	ig = toggleIgnore(ig, "libera", "Spammer");
	is(isIgnored(ig, "libera", "spammer"), true, "case-insensitive match");
	is(isIgnored(ig, "libera", "SPAMMER"), true);
	is(isIgnored(ig, "oftc", "spammer"), false, "per-network");
	eq(ignoredFor(ig, "libera"), ["spammer"]);
	eq(loadIgnores(), { libera: ["spammer"] }, "persisted");

	ig = toggleIgnore(ig, "libera", "spammer");
	is(isIgnored(ig, "libera", "spammer"), false, "toggled off");
	eq(ig, {}, "empty network pruned");
	eq(loadIgnores(), {}, "prune persisted");
});

test("ignores: empty/garbage nick is never ignored", () => {
	const ig = toggleIgnore(loadIgnores(), "libera", "bob");
	is(isIgnored(ig, "libera", ""), false);
	is(isIgnored(ig, "libera", undefined), false);
});

test("ignores: a corrupt non-array network value does not throw the hot path", () => {
	// A hand-edited / partially-written store could map a network to a non-array.
	const corrupt = { libera: 5, oftc: "x" };
	is(isIgnored(corrupt, "libera", "bob"), false);
	is(isIgnored(corrupt, "oftc", "bob"), false);
	eq(ignoredFor(corrupt, "libera"), []);
	eq(ignoredFor(corrupt, "missing"), []);
});

test("mutes: toggle and persist by buffer key", () => {
	let m = loadMutes();
	eq(m, []);
	m = toggleMute(m, "libera|#go");
	is(isMuted(m, "libera|#go"), true);
	is(isMuted(m, "libera|#rust"), false);
	eq(loadMutes(), ["libera|#go"]);
	m = toggleMute(m, "libera|#go");
	is(isMuted(m, "libera|#go"), false);
	eq(loadMutes(), []);
});

test("corrupt storage falls back to defaults", () => {
	store.set("ignores", "{not json");
	store.set("mutes", "also broken");
	eq(loadIgnores(), {});
	eq(loadMutes(), []);
});
