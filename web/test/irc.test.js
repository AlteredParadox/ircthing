import { deepStrictEqual as eq, strictEqual as is } from "node:assert";
import { test } from "node:test";
import {
	bufKey, firstURL, fmtTime, hostOf, linkify, looksLikeImageURL,
	bufferOrder, isChannelName, mentionsMe, nickColor, parseHash, parseLine, rankBuffers, renderable, sameGroup, toHash, applyStatusMode, mergeById, mergeServerBuffers,
	applyTombstones, rememberRedaction, nickSet, highlightNicks, proxyCredsExposed, foldNick,
	parseFormatting, stripFormatting,
} from "../src/irc.js";

test("parseFormatting: plain text is one unstyled run", () => {
	const runs = parseFormatting("hello world");
	is(runs.length, 1);
	is(runs[0].text, "hello world");
	is(runs[0].fg, null);
	is(runs[0].bold, false);
});

test("parseFormatting: bold/italic toggles and reset", () => {
	const runs = parseFormatting("a\x02b\x1dc\x0fd");
	eq(runs.map((r) => [r.text, r.bold, r.italic]), [
		["a", false, false],
		["b", true, false],
		["c", true, true],
		["d", false, false],
	]);
});

test("parseFormatting: the garbled-emoticon case renders clean with colors", () => {
	// \x0306^\x0313.\x0305^\x0304/  ->  "\^.^/" (was shown as "\06^13.05^04/")
	const runs = parseFormatting("\\\x0306^\x0313.\x0305^\x0304/");
	is(runs.map((r) => r.text).join(""), "\\^.^/");
	is(runs.find((r) => r.text === "^").fg, "#9c009c"); // colour 6
});

test("parseFormatting: the YouTube badge case (fg,bg with reset)", () => {
	const runs = parseFormatting("^ \x030,4▶\x031,0YouTube\x03 :: rest");
	const play = runs.find((r) => r.text === "▶");
	is(play.fg, "#ffffff"); // 0 white
	is(play.bg, "#ff0000"); // 4 red
	const yt = runs.find((r) => r.text === "YouTube");
	is(yt.fg, "#000000"); // 1 black
	is(yt.bg, "#ffffff"); // 0 white
	is(runs.find((r) => r.text === " :: rest").fg, null); // bare \x03 reset colour
});

test("stripFormatting removes codes; mentionsMe sees through them", () => {
	is(stripFormatting("\\\x0306^\x0313.\x0305^\x0304/"), "\\^.^/");
	is(stripFormatting("\x02bold\x0f \x0304,05colored\x03"), "bold colored");
	is(mentionsMe("hey \x0304alice\x03!", "alice"), true); // colour code must not hide it
});

test("bare \\x03 + comma-digits strips exactly as parseFormatting renders it", () => {
	// A bare \x03 (no fg digit) is a colour RESET; the following ",5" is literal
	// text, so strip must keep it — matching what the body shows — or mention
	// detection diverges from the rendered text.
	is(stripFormatting("al\x03,1ice"), "al,1ice");
	is(mentionsMe("al\x03,1ice", "alice"), false); // visibly "al,1ice", not a mention
	is(stripFormatting("\x03,5 o'clock"), ",5 o'clock");
	// A real fg (with or without bg) is still fully stripped.
	is(stripFormatting("\x0304,05red\x03"), "red");
	is(stripFormatting("\x034warm"), "warm");
	// Parity with parseFormatting's visible text on the tricky forms.
	for (const t of ["al\x03,1ice", "\x03,5x", "\x0312,34y", "\x033z", "\x03w"]) {
		const rendered = parseFormatting(t).map((r) => r.text).join("");
		is(stripFormatting(t), rendered);
	}
});

test("bare \\x04 strips like it renders: no invisible mention-splitting", () => {
	// parseFormatting consumes a hex-less \x04 (renders "alice"), so strip
	// must remove it too — or the visible mention never alerts.
	is(stripFormatting("al\x04ice"), "alice");
	is(mentionsMe("al\x04ice", "alice"), true);
	is(stripFormatting("\x04ff0000red\x04"), "red"); // valid args still stripped
	const runs = parseFormatting("al\x04ice");
	is(runs.map((r) => r.text).join(""), "alice");
});

test("parseFormatting caps runs; the remainder renders plainly, codes stripped", () => {
	const bomb = "\x02a".repeat(5000); // would be 5000 one-char bold toggle runs
	const runs = parseFormatting(bomb);
	is(runs.length <= 1025, true); // MAX_FMT_RUNS + the merged remainder
	// No content is lost and no control bytes leak into the rendered text.
	const joined = runs.map((r) => r.text).join("");
	is(joined, "a".repeat(5000));
});

test("proxyCredsExposed flags credentials to a non-loopback proxy", () => {
	is(proxyCredsExposed("socks5://user:pass@proxy.example.com:1080"), true);
	is(proxyCredsExposed("http://u:p@203.0.113.9:3128"), true);
	is(proxyCredsExposed("socks5://user:pass@127.0.0.1:9050"), false); // loopback
	is(proxyCredsExposed("socks5://user:pass@localhost:9050"), false);
	is(proxyCredsExposed("socks5://user:pass@[::1]:9050"), false);
	is(proxyCredsExposed("socks5://proxy.example.com:1080"), false); // no creds
	is(proxyCredsExposed(""), false);
	// A public host that merely STARTS with "127." must not be treated as
	// loopback (the warning must still show).
	is(proxyCredsExposed("socks5://u:p@127.attacker.example:1080"), true);
	is(proxyCredsExposed("socks5://u:p@127.0.0.1.evil.com:1080"), true);
	is(proxyCredsExposed("socks5://u:p@127.5.6.7:1080"), false); // real 127/8
});

test("foldNick applies an rfc1459-superset case fold", () => {
	is(foldNick("Nick"), "nick");
	is(foldNick("Foo[]\\~"), "foo{}|^"); // []\~ -> {}|^
	is(foldNick("nick[]"), foldNick("NICK{}")); // equivalent under rfc1459
});

test("highlightNicks matches whole tokens, not substrings", () => {
	const m = nickSet(["bob", "Alice", "me"], "me"); // own nick excluded
	is(m.has("me"), false);

	// A mention mid-sentence, bounded by punctuation.
	eq(highlightNicks("hey bob, ping Alice!", m), [
		{ nick: null, text: "hey " },
		{ nick: "bob", text: "bob" },
		{ nick: null, text: ", ping " },
		{ nick: "Alice", text: "Alice" }, // canonical casing preserved for color/menu
		{ nick: null, text: "!" },
	]);

	// Substrings never match; the typed casing is shown but maps to canonical.
	eq(highlightNicks("bobby BOB", m), [
		{ nick: null, text: "bobby " },
		{ nick: "bob", text: "BOB" },
	]);

	// No map / empty map -> single plain segment.
	eq(highlightNicks("bob", null), [{ nick: null, text: "bob" }]);
});

test("rememberRedaction + applyTombstones re-tombstones a re-delivered row", () => {
	const store = new Map();
	rememberRedaction(store, "libera\x00#go", "m1", "spam");
	rememberRedaction(store, "libera\x00#go", "", "ignored"); // empty msgid is a no-op

	const tomb = store.get("libera\x00#go");
	is(tomb.size, 1);

	// A stale history page re-delivers m1 with full content and redacted=false.
	const page = [
		{ id: 1, msgid: "m1", redacted: false, raw: ":bob PRIVMSG #go :deleted text", text: "deleted text" },
		{ id: 2, msgid: "m2", redacted: false, raw: ":bob PRIVMSG #go :kept", text: "kept" },
	];
	const out = applyTombstones(page, tomb);
	is(out[0].redacted, true);
	is(out[0].raw, ""); // content scrubbed
	is(out[0].redact_reason, "spam");
	is(out[1].redacted, false); // unrelated row untouched
	is(out[1].raw, ":bob PRIVMSG #go :kept");
});

test("rememberRedaction bounds the outer per-buffer map", () => {
	const store = new Map();
	// Redactions across endlessly-varying targets must not grow the outer map
	// without limit (a hostile flood): the oldest buffers are evicted past the cap.
	for (let i = 0; i < 400; i++) rememberRedaction(store, "libera\x00#c" + i, "m", "r");
	is(store.size <= 128, true);
	// The oldest buffer was evicted; the newest is retained.
	is(store.has("libera\x00#c0"), false);
	is(store.has("libera\x00#c399"), true);
	// A long redaction reason is truncated in the retained tombstone.
	rememberRedaction(store, "libera\x00#c399", "big", "x".repeat(5000));
	is(store.get("libera\x00#c399").get("big").length, 256);
});

test("applyTombstones is a no-op without tombstones", () => {
	const list = [{ id: 1, msgid: "m1", redacted: false, raw: "x" }];
	is(applyTombstones(list, undefined), list);
	is(applyTombstones(list, new Map()), list);
});

test("mergeServerBuffers", () => {
	const nets = { libera: {}, oftc: {} };
	// A server buffer, a client-only ephemeral buffer, a non-ephemeral buffer
	// the server no longer lists (closed elsewhere), and one on a gone network.
	const prev = {
		[bufKey("libera", "#go")]: { key: bufKey("libera", "#go"), network: "libera", buffer: "#go", mention: true },
		[bufKey("libera", "query")]: { key: bufKey("libera", "query"), network: "libera", buffer: "query", ephemeral: true },
		[bufKey("libera", "#closed")]: { key: bufKey("libera", "#closed"), network: "libera", buffer: "#closed" },
		[bufKey("dead", "#x")]: { key: bufKey("dead", "#x"), network: "dead", buffer: "#x", ephemeral: true },
	};
	const data = [{ network: "libera", buffer: "#go", last_time: 5, marker: 2, unread: 1 }];
	const out = mergeServerBuffers(data, prev, nets);

	// Server buffer present, keeps client-side mention flag.
	is(out[bufKey("libera", "#go")].unread, 1);
	is(out[bufKey("libera", "#go")].mention, true);
	// Ephemeral client-only buffer is preserved.
	is(!!out[bufKey("libera", "query")], true);
	// A non-ephemeral buffer the server dropped is NOT resurrected.
	is(out[bufKey("libera", "#closed")], undefined);
	// An ephemeral buffer on a no-longer-known network is dropped too.
	is(out[bufKey("dead", "#x")], undefined);
});

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

test("fmtTime honours clock/seconds/ampm options", () => {
	const at = (h, m, s) => {
		const d = new Date();
		d.setHours(h, m, s, 0);
		return d.getTime();
	};
	// 24-hour (default) with and without seconds.
	is(fmtTime(at(14, 5, 9)), "14:05");
	is(fmtTime(at(14, 5, 9), { clock: "24", seconds: true }), "14:05:09");
	// 12-hour: zero-padded hour, AM/PM toggle, midnight/noon edge cases.
	is(fmtTime(at(14, 5, 9), { clock: "12", ampm: true }), "02:05 PM");
	is(fmtTime(at(14, 5, 9), { clock: "12", ampm: false }), "02:05");
	is(fmtTime(at(14, 5, 9), { clock: "12", seconds: true, ampm: true }), "02:05:09 PM");
	is(fmtTime(at(0, 0, 0), { clock: "12", ampm: true }), "12:00 AM");
	is(fmtTime(at(12, 0, 0), { clock: "12", ampm: true }), "12:00 PM");
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
	// A mIRC formatting code adjacent to the URL must NOT be captured — else the
	// preview endpoint's url.Parse rejects it and the colored link never previews.
	is(firstURL("\x0312http://example.com/path\x0f"), "http://example.com/path");
	is(firstURL("\x02\x0304https://a.com/x\x0f rest"), "https://a.com/x");
	is(looksLikeImageURL(firstURL("\x0312https://x.com/cat.png\x0f")), true);
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
	// The collapse-row id anchors on the run's LAST event (stable under
	// a top-prepend), so run [1,2,3] -> "clp-3".
	eq(out.map((e) => e.id), ["clp-3", 4, 5, 6]);
	is(out[0].summary, "2 joined, 1 left");
	is(out[0].expanded, false);
	is(out[0].time, 3000);
});

test("applyStatusMode: expanded run keeps the toggle row plus events", () => {
	const list = [pev(1, "NICK", "a"), pev(2, "NICK", "b"), pev(3, "PRIVMSG", "x")];
	// Expand state is keyed on a run MEMBER id (here the anchor, event 2).
	const out = applyStatusMode(list, "collapse", new Set([2]));
	eq(out.map((e) => e.id), ["clp-2", 1, 2, 3]);
	is(out[0].expanded, true);
	is(out[0].summary, "2 nick changes");
});

test("applyStatusMode: an expanded run stays open when it grows at the tail", () => {
	const expanded = new Set([2]); // opened when the run was [1,2]
	// A live join extends the run to [1,2,3]; membership keying keeps it open
	// even though the last event (and the derived id) changed from 2 to 3.
	const list = [pev(1, "JOIN", "a"), pev(2, "JOIN", "b"), pev(3, "JOIN", "c")];
	const out = applyStatusMode(list, "collapse", expanded);
	is(out[0].id, "clp-3");
	is(out[0].expanded, true, "run must stay expanded after tail growth");
	eq(out.map((e) => e.id), ["clp-3", 1, 2, 3]);
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

test("mergeById: zero-padded ephemeral ids keep insertion order on same-ms ties", () => {
	// server_info/whois lines share one Date.now() ms; mergeById breaks the
	// tie by STRING id. Unpadded "si10" would sort before "si2"; zero-padding
	// (padStart 9) keeps them in emission order when history loads.
	const lines = [];
	for (let i = 1; i <= 12; i++) {
		lines.push({ id: `si${String(i).padStart(9, "0")}`, time: 1000, raw: `line ${i}` });
	}
	const shuffled = [lines[9], lines[1], lines[11], lines[0]]; // si10, si2, si12, si1
	const order = mergeById([], shuffled).map((e) => e.id);
	eq(order, [lines[0].id, lines[1].id, lines[9].id, lines[11].id]); // si1, si2, si10, si12
});

test("applyStatusMode: collapse-row id is stable when a run is extended at its top (prepend)", () => {
	// Run of presence events 3,4,5; then an older page prepends 1,2 which
	// are contiguous presence events, extending the run's TOP.
	const runB = [pev(3, "JOIN", "a"), pev(4, "JOIN", "b"), pev(5, "QUIT", "c"), pev(6, "PRIVMSG", "x")];
	const before = applyStatusMode(runB, "collapse", new Set());
	const runA = [pev(1, "JOIN", "y"), pev(2, "PART", "z"), ...runB];
	const after = applyStatusMode(runA, "collapse", new Set());
	// The collapse row covering the (now larger) top run keeps its id,
	// because it anchors on the unchanged last event (5).
	is(before[0].id, "clp-5");
	is(after[0].id, "clp-5");
});
