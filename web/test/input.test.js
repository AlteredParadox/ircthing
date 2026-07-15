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
