import { deepStrictEqual as eq, strictEqual as is } from "node:assert";
import { test } from "node:test";
import {
	bufKey, firstURL, fmtTime, hostOf, linkify, looksLikeImageURL,
	bufferOrder, isChannelName, mentionsMe, nickColor, parseHash, parseLine, rankBuffers, renderable, sameGroup, toHash, applyStatusMode, mergeById,
} from "../src/irc.js";

test("parseLine", () => {
	const cases = [
		{
			in: ":alice!u@h PRIVMSG #go :hello there",
			want: { prefix: "alice", command: "PRIVMSG", params: ["#go", "hello there"] },
		},
		{
			in: "@msgid=x;time=2026-07-15T00:00:00.000Z :alice!u@h PRIVMSG #go :hi",
			want: { prefix: "alice", command: "PRIVMSG", params: ["#go", "hi"], tags: { msgid: "x" } },
		},
		{
			in: "PING :token",
			want: { prefix: null, command: "PING", params: ["token"] },
		},
		{
			in: ":op!u@h KICK #go alice :bye now",
			want: { prefix: "op", command: "KICK", params: ["#go", "alice", "bye now"] },
		},
		{
			in: ":alice!u@h JOIN #go",
			want: { prefix: "alice", command: "JOIN", params: ["#go"] },
		},
		{
			in: ":srv NOTICE AlteredParadox :colon :in: middle",
			want: { prefix: "srv", command: "NOTICE", params: ["AlteredParadox", "colon :in: middle"] },
		},
	];
	for (const c of cases) {
		const got = parseLine(c.in);
		is(got.prefix?.name ?? null, c.want.prefix, c.in);
		is(got.command, c.want.command, c.in);
		eq(got.params, c.want.params, c.in);
		for (const [k, v] of Object.entries(c.want.tags || {})) is(got.tags[k], v, c.in);
	}
});

test("renderable", () => {
	const ev = (sender, command, raw) => ({ sender, command, raw, time: 0 });
	const cases = [
		{ ev: ev("alice", "PRIVMSG", ":alice!u@h PRIVMSG #go :hi"), kind: "msg", text: "hi" },
		{ ev: ev("alice", "NOTICE", ":alice!u@h NOTICE #go :psst"), kind: "notice", text: "psst" },
		{
			ev: ev("alice", "PRIVMSG", ":alice!u@h PRIVMSG #go :\x01ACTION waves\x01"),
			kind: "action", text: "waves",
		},
		{ ev: ev("alice", "JOIN", ":alice!u@h JOIN #go"), kind: "system", text: "alice has joined" },
		{
			ev: ev("alice", "PART", ":alice!u@h PART #go :bye"),
			kind: "system", text: "alice has left (bye)",
		},
		{
			ev: ev("op", "KICK", ":op!u@h KICK #go alice :flood"),
			kind: "system", text: "alice was kicked by op (flood)",
		},
		{
			ev: ev("alice", "TOPIC", ":alice!u@h TOPIC #go :new topic"),
			kind: "system", text: "alice set the topic: new topic",
		},
		{
			ev: ev("op", "MODE", ":op!u@h MODE #go +o alice"),
			kind: "system", text: "op set mode +o alice",
		},
	];
	for (const c of cases) {
		const got = renderable(c.ev);
		is(got.kind, c.kind, c.ev.raw);
		is(got.text, c.text, c.ev.raw);
	}
});

test("renderable: multiline body preserves the joined text", () => {
	// The reconstructed multiline message carries embedded newlines in
	// its trailing parameter; renderable returns them intact for the
	// Body component to split.
	const ev = { sender: "a", command: "PRIVMSG", raw: ":a!u@h PRIVMSG #go :one\ntwo\nthree", time: 0 };
	const r = renderable(ev);
	is(r.kind, "msg");
	is(r.text, "one\ntwo\nthree");
});

test("renderable: redacted messages become tombstones", () => {
	const ev = { sender: "alice", command: "PRIVMSG", raw: ":alice!u@h PRIVMSG #go :secret", time: 0, redacted: true };
	const r = renderable(ev);
	is(r.kind, "redacted");
	is(r.text, "message deleted");
	is(r.mark, "⌫");
	is(renderable({ ...ev, redact_reason: "spam" }).text, "message deleted (spam)");
	// A non-redacted message renders normally.
	is(renderable({ ...ev, redacted: false }).kind, "msg");
});

test("nickColor is deterministic and theme-aware", () => {
	is(nickColor("alice", "dark"), nickColor("alice", "dark"));
	is(nickColor("alice", "dark").startsWith("oklch(0.74 0.13 "), true);
	is(nickColor("alice", "light").startsWith("oklch(0.5 0.15 "), true);
	// Different nicks should usually differ.
	is(nickColor("alice", "dark") !== nickColor("bob", "dark"), true);
});

test("sameGroup", () => {
	const m = (sender, time) => ({ kind: "msg", sender, time });
	is(sameGroup(m("a", 0), m("a", 60_000)), true);
	is(sameGroup(m("a", 0), m("b", 1000)), false);
	is(sameGroup(m("a", 0), m("a", 6 * 60_000)), false, "5 minute gap breaks the group");
	is(sameGroup(null, m("a", 0)), false);
	is(sameGroup({ kind: "system", sender: "a", time: 0 }, m("a", 1)), false);
});

test("mentionsMe", () => {
	const cases = [
		{ text: "AlteredParadox: hello", nick: "AlteredParadox", want: true },
		{ text: "hey AlteredParadox", nick: "AlteredParadox", want: true },
		{ text: "ALTEREDPARADOX ping", nick: "AlteredParadox", want: true },
		{ text: "mAlteredParadoxat", nick: "AlteredParadox", want: false },
		{ text: "AlteredParadox_ is someone else", nick: "AlteredParadox", want: false },
		{ text: "ping AlteredParadox[]", nick: "AlteredParadox[]", want: true },
		{ text: "nothing here", nick: "AlteredParadox", want: false },
		{ text: "anything", nick: "", want: false },
	];
	for (const c of cases) is(mentionsMe(c.text, c.nick), c.want, `${c.text} / ${c.nick}`);
});

test("linkify", () => {
	eq(linkify("plain text"), [{ link: false, text: "plain text" }]);
	eq(linkify("see https://example.com/x for more"), [
		{ link: false, text: "see " },
		{ link: true, text: "https://example.com/x" },
		{ link: false, text: " for more" },
	]);
	eq(linkify("(https://example.com/a)."), [
		{ link: false, text: "(" },
		{ link: true, text: "https://example.com/a" },
		{ link: false, text: ")." },
	]);
	// Wikipedia-style parenthesized URLs keep their closing paren.
	eq(linkify("https://en.example.org/wiki/Go_(language)")[0], {
		link: true,
		text: "https://en.example.org/wiki/Go_(language)",
	});
});

test("hash routing round-trips", () => {
	const h = toHash("libera", "#go/notes");
	eq(parseHash(h), { network: "libera", buffer: "#go/notes" });
	is(parseHash("#garbage"), null);
	is(parseHash(""), null);
});

test("fmtTime pads", () => {
	const d = new Date();
	d.setHours(9, 5, 0, 0);
	is(fmtTime(d.getTime()), "09:05");
});

test("bufKey separates network and buffer", () => {
	is(bufKey("libera", "#go"), "libera\n#go");
	is(bufKey("ab", "#c") !== bufKey("a", "b#c"), true);
});

test("firstURL", () => {
	is(firstURL("no links here"), "");
	is(firstURL("see https://example.com/x thanks"), "https://example.com/x");
	is(firstURL("two https://a.com and https://b.com"), "https://a.com");
	is(firstURL("http://plain.example works"), "http://plain.example");
});

test("looksLikeImageURL", () => {
	is(looksLikeImageURL("https://x.com/cat.png"), true);
	is(looksLikeImageURL("https://x.com/a/b.JPG?q=1"), true);
	is(looksLikeImageURL("https://x.com/page.html"), false);
	is(looksLikeImageURL("https://x.com/noext"), false);
	is(looksLikeImageURL("not a url"), false);
});

test("hostOf", () => {
	is(hostOf("https://example.com/path?x=1"), "example.com");
	is(hostOf("http://sub.example.com:8080/y"), "sub.example.com:8080");
	is(hostOf("garbage"), "garbage");
});

test("isChannelName: per-network CHANTYPES", () => {
	is(isChannelName("#go"), true, "default #");
	is(isChannelName("&local"), true, "default &");
	is(isChannelName("alice"), false);
	is(isChannelName("", "#"), false);
	is(isChannelName("&local", "#"), false, "network without & channels");
	is(isChannelName("!weird", "#!"), true, "unusual prefix honored");
});

test("bufferOrder: sidebar order (network, then buffer)", () => {
	const bufs = {
		b: { key: "b", network: "oftc", buffer: "#a" },
		a: { key: "a", network: "libera", buffer: "#z" },
		c: { key: "c", network: "libera", buffer: "#b" },
	};
	eq(bufferOrder(bufs), ["c", "a", "b"]);
});

test("rankBuffers: mentions, then unread, then match position", () => {
	const bufs = {
		a: { key: "a", network: "libera", buffer: "#golang", unread: 0, mention: false },
		b: { key: "b", network: "libera", buffer: "#go", unread: 3, mention: false },
		c: { key: "c", network: "oftc", buffer: "#gonzo", unread: 1, mention: true },
		d: { key: "d", network: "libera", buffer: "alice", unread: 0, mention: false },
	};
	eq(rankBuffers(bufs, "go").map((b) => b.key), ["c", "b", "a"], "mention > unread > rest");
	eq(rankBuffers(bufs, "").map((b) => b.key), ["c", "b", "a", "d"], "empty query lists all");
	eq(rankBuffers(bufs, "libera").map((b) => b.key), ["b", "a", "d"], "network name matches too");
	eq(rankBuffers(bufs, "nomatch"), []);
});

test("renderable: bot-mode message tag", () => {
	const bot = renderable({ command: "PRIVMSG", raw: "@bot;msgid=x :guard!u@h PRIVMSG #go :beep boop" });
	is(bot.bot, true);
	is(bot.text, "beep boop");
	const human = renderable({ command: "PRIVMSG", raw: ":alice!u@h PRIVMSG #go :hi" });
	is(human.bot, false);
	const action = renderable({ command: "PRIVMSG", raw: "@bot :guard!u@h PRIVMSG #go :\x01ACTION beeps\x01" });
	is(action.bot, true);
	is(action.kind, "action");
});

test("renderable: QUIT and NICK system lines", () => {
	const quit = renderable({ command: "QUIT", sender: "alice", raw: ":alice!u@h QUIT :gone fishing" });
	is(quit.kind, "system");
	is(quit.text, "alice has quit (gone fishing)");
	const bare = renderable({ command: "QUIT", sender: "alice", raw: ":alice!u@h QUIT" });
	is(bare.text, "alice has quit");
	const nick = renderable({ command: "NICK", sender: "alice", raw: ":alice!u@h NICK alicia" });
	is(nick.kind, "system");
	is(nick.text, "alice is now known as alicia");
});

test("parseHash: malformed input returns null, never throws", () => {
	eq(parseHash("#/libera/%23go"), { network: "libera", buffer: "#go" });
	is(parseHash("#/x/%"), null, "bad percent-escape");
	is(parseHash("#/onlynetwork"), null);
	is(parseHash("#//buffer"), null, "empty network");
	is(parseHash("#/net/"), null, "empty buffer");
	is(parseHash("nonsense"), null);
});

// ---- applyStatusMode ----

function pev(id, command, sender) {
	return { id, command, sender, time: id * 1000, raw: `:${sender}!u@h ${command} #c` };
}

test("applyStatusMode: show returns the list untouched", () => {
	const list = [pev(1, "JOIN", "a"), pev(2, "PRIVMSG", "a")];
	is(applyStatusMode(list, "show", new Set()), list);
});

test("applyStatusMode: hide drops presence lines, keeps kicks and messages", () => {
	const list = [
		pev(1, "JOIN", "a"), pev(2, "PRIVMSG", "a"), pev(3, "QUIT", "b"),
		pev(4, "NICK", "c"), pev(5, "KICK", "op"), pev(6, "PART", "d"),
	];
	eq(applyStatusMode(list, "hide", new Set()).map((e) => e.id), [2, 5]);
});

test("applyStatusMode: collapse folds runs of 2+, leaves singles", () => {
	const list = [
		pev(1, "JOIN", "a"), pev(2, "JOIN", "b"), pev(3, "QUIT", "c"),
		pev(4, "PRIVMSG", "a"), pev(5, "PART", "d"), pev(6, "PRIVMSG", "b"),
	];
	const out = applyStatusMode(list, "collapse", new Set());
	eq(out.map((e) => e.id), ["clp-1", 4, 5, 6]);
	is(out[0].summary, "2 joined, 1 left");
	is(out[0].expanded, false);
	is(out[0].time, 3000);
});

test("applyStatusMode: expanded run keeps the toggle row plus events", () => {
	const list = [pev(1, "NICK", "a"), pev(2, "NICK", "b"), pev(3, "PRIVMSG", "x")];
	const out = applyStatusMode(list, "collapse", new Set(["clp-1"]));
	eq(out.map((e) => e.id), ["clp-1", 1, 2, 3]);
	is(out[0].expanded, true);
	is(out[0].summary, "2 nick changes");
});

test("mergeById: dedups and sorts by (time, id), order-robust", () => {
	const m1 = { id: 1, time: 100 }, m2 = { id: 2, time: 200 }, m3 = { id: 3, time: 300 };
	const held = [m1, m2, m3];
	const slid = [m2, m3, { id: 4, time: 400 }]; // overlapping, out-of-order page
	eq(mergeById(held, slid).map((e) => e.id), [1, 2, 3, 4]);
});

test("mergeById: numeric ids break same-time ties numerically", () => {
	eq(mergeById([{ id: 10, time: 5 }], [{ id: 9, time: 5 }]).map((e) => e.id), [9, 10]);
});

test("mergeById: incoming replaces existing by id", () => {
	const out = mergeById([{ id: 1, time: 1, v: "old" }], [{ id: 1, time: 1, v: "new" }]);
	is(out.length, 1);
	is(out[0].v, "new");
});
