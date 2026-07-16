import { strictEqual as is } from "node:assert";
import { test } from "node:test";
import { LRU } from "../src/lru.js";

test("LRU: caps size, evicting the oldest", () => {
	const c = new LRU(3, 60000);
	c.set("a", 1);
	c.set("b", 2);
	c.set("c", 3);
	c.set("d", 4); // evicts a
	is(c.size, 3);
	is(c.get("a"), undefined);
	is(c.get("b"), 2);
});

test("LRU: get refreshes recency", () => {
	const c = new LRU(2, 60000);
	c.set("a", 1);
	c.set("b", 2);
	c.get("a"); // a is now the most recent
	c.set("c", 3); // evicts b
	is(c.get("a"), 1);
	is(c.get("b"), undefined);
});

test("LRU: entries expire; null values (cached failures) are held", () => {
	const c = new LRU(4, 60000);
	c.set("fail", null, -1); // already expired
	is(c.has("fail"), false);
	c.set("fail2", null, 60000);
	is(c.has("fail2"), true); // a live cached failure counts as present
	is(c.get("fail2"), null);
});

test("LRU: re-setting a key does not evict others", () => {
	const c = new LRU(2, 60000);
	c.set("a", 1);
	c.set("b", 2);
	c.set("a", 9);
	is(c.size, 2);
	is(c.get("a"), 9);
	is(c.get("b"), 2);
});
