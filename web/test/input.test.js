import { deepStrictEqual as eq, strictEqual as is } from "node:assert";
import { test } from "node:test";
import { groupMembers, parseInput } from "../src/irc.js";

test("parseInput", () => {
	const cases = [
		{ in: "hello world", want: { type: "text", text: "hello world" } },
		{ in: "//join is a word", want: { type: "text", text: "/join is a word" } },
		{ in: "/me waves", want: { type: "text", text: "\x01ACTION waves\x01" } },
		{ in: "/me", want: { type: "error" } },
		{ in: "/msg alice hi there", want: { type: "msg", target: "alice", text: "hi there" } },
		{ in: "/query alice hi", want: { type: "msg", target: "alice", text: "hi" } },
		{ in: "/msg alice", want: { type: "error" } },
		{ in: "/msg alice   ", want: { type: "error" } },
		{
			in: "/join #go",
			want: { type: "cmd", command: "JOIN", params: ["#go"], switchTo: "#go" },
		},
		{
			in: "/join #priv sekrit",
			want: { type: "cmd", command: "JOIN", params: ["#priv", "sekrit"], switchTo: "#priv" },
		},
		{ in: "/join", want: { type: "error" } },
		{ in: "/join notachannel", want: { type: "error" } },
		{ in: "/part", want: { type: "cmd", command: "PART", params: ["#active"] } },
		{
			in: "/part bye everyone",
			want: { type: "cmd", command: "PART", params: ["#active", "bye everyone"] },
		},
		{
			in: "/part #other so long",
			want: { type: "cmd", command: "PART", params: ["#other", "so long"] },
		},
		{ in: "/nick AlteredParadox2", want: { type: "cmd", command: "NICK", params: ["AlteredParadox2"] } },
		{ in: "/nick two words", want: { type: "error" } },
		{ in: "/nick", want: { type: "error" } },
		{
			in: "/topic new topic text",
			want: { type: "cmd", command: "TOPIC", params: ["#active", "new topic text"] },
		},
		{ in: "/topic", want: { type: "error" } },
		{ in: "/JOIN #caps", want: { type: "cmd", command: "JOIN", params: ["#caps"], switchTo: "#caps" } },
		{ in: "/frobnicate", want: { type: "error" } },
	];
	for (const c of cases) {
		const got = parseInput(c.in, "#active");
		if (c.want.type === "error") {
			is(got.type, "error", `${c.in}: got ${JSON.stringify(got)}`);
			is(typeof got.message, "string", c.in);
		} else {
			const { message, ...rest } = got;
			eq(rest, c.want, c.in);
		}
	}
});

test("parseInput /part in a query errors without an explicit channel", () => {
	is(parseInput("/part", "alice").type, "error");
	eq(parseInput("/part #go", "alice"), { type: "cmd", command: "PART", params: ["#go"] });
});

test("parseInput /topic outside a channel errors", () => {
	is(parseInput("/topic something", "alice").type, "error");
});

test("groupMembers", () => {
	const got = groupMembers([
		{ nick: "owner", prefix: "~" },
		{ nick: "op", prefix: "@" },
		{ nick: "stacked", prefix: "@+" }, // multi-prefix: highest wins
		{ nick: "half", prefix: "%" },
		{ nick: "voiced", prefix: "+" },
		{ nick: "plain", prefix: "" },
		{ nick: "also" },
	]);
	eq(got.map((g) => [g.label, g.members.length]), [["Ops", 3], ["Voice", 2], ["Members", 2]]);
	// Empty groups are dropped.
	eq(groupMembers([{ nick: "a" }]).map((g) => g.label), ["Members"]);
	eq(groupMembers([]), []);
});

test("parseInput: commands follow the network's CHANTYPES", () => {
	const p = parseInput("/join !weird", "#go", "#!");
	eq(p, { type: "cmd", command: "JOIN", params: ["!weird"], switchTo: "!weird" });
	is(parseInput("/join &nope", "#go", "#").type, "error", "& is not a channel here");
	is(parseInput("/topic new topic", "!weird", "#!").type, "cmd");
});

test("parseInput: informational commands", () => {
	eq(parseInput("/whois alice", "#go"), { type: "cmd", command: "WHOIS", params: ["alice"] });
	eq(parseInput("/whowas ghost", "#go"), { type: "cmd", command: "WHOWAS", params: ["ghost"] });
	eq(parseInput("/who *.example.org", "#go"), { type: "cmd", command: "WHO", params: ["*.example.org"] });
	is(parseInput("/whois", "#go").type, "error");
	is(parseInput("/whois a b", "#go").type, "error");
	eq(parseInput("/list", "#go"), { type: "cmd", command: "LIST", params: [] });
	eq(parseInput("/list #go*", "#go"), { type: "cmd", command: "LIST", params: ["#go*"] });
	eq(parseInput("/motd", "#go"), { type: "cmd", command: "MOTD", params: [] });
});

test("parseInput: away toggles, notice targets", () => {
	eq(parseInput("/away gone fishing", "#go"), { type: "cmd", command: "AWAY", params: ["gone fishing"] });
	eq(parseInput("/away", "#go"), { type: "cmd", command: "AWAY", params: [] });
	eq(parseInput("/notice alice psst over here", "#go"),
		{ type: "cmd", command: "NOTICE", params: ["alice", "psst over here"] });
	is(parseInput("/notice alice", "#go").type, "error");
});

test("parseInput: mode defaults to the current buffer", () => {
	eq(parseInput("/mode", "#go"), { type: "cmd", command: "MODE", params: ["#go"] });
	eq(parseInput("/mode +m", "#go"), { type: "cmd", command: "MODE", params: ["#go", "+m"] });
	eq(parseInput("/mode #other +o alice", "#go"),
		{ type: "cmd", command: "MODE", params: ["#other", "+o", "alice"] });
	eq(parseInput("/mode alice +i", "#go"), { type: "cmd", command: "MODE", params: ["alice", "+i"] });
	is(parseInput("/mode +o a b c d e f", "#go").type, "error", "too many params");
});

test("parseInput: kick and invite default to the current channel", () => {
	eq(parseInput("/kick alice", "#go"), { type: "cmd", command: "KICK", params: ["#go", "alice"] });
	eq(parseInput("/kick alice being rude", "#go"),
		{ type: "cmd", command: "KICK", params: ["#go", "alice", "being rude"] });
	eq(parseInput("/kick #other alice", "#go"), { type: "cmd", command: "KICK", params: ["#other", "alice"] });
	is(parseInput("/kick", "#go").type, "error");
	is(parseInput("/kick alice", "bob").type, "error", "kick from a query needs a channel");
	eq(parseInput("/invite alice", "#go"), { type: "cmd", command: "INVITE", params: ["alice", "#go"] });
	eq(parseInput("/invite alice #other", "#go"), { type: "cmd", command: "INVITE", params: ["alice", "#other"] });
	is(parseInput("/invite alice", "bob").type, "error");
});
