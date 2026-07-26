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

// Browser-test harness for the real <Chat> — the message list, its rows, and
// the actual stylesheet, which is where the layout bugs live that no
// DOM-less unit test can see (see web/browser-test/).
//
// It mounts the SAME component app.jsx mounts, with the same event shapes the
// hub sends, and exposes window.__h for the Playwright driver. Built by
// `make browser-test`, never by `make frontend` — nothing here ships.
import { render } from "preact";
import { useState } from "preact/hooks";
import { Chat } from "../../src/chat.jsx";

// Event shapes match hub.EventData: `command` drives renderable(), `raw` is
// the full IRC line it parses. Getting this wrong is silent — an unknown
// command renders as a system row rather than erroring — so the tests assert
// on row classes to prove real message rows were produced.
let nextID = 0;

// The raw line MUST look like real wire traffic, tags and all. The estimator
// bug this harness exists to catch was a ratio problem — raw is what the
// broken code measured, rendered text is what the user sees — and that ratio
// is worst exactly where real IRC lives: short messages behind a long tag
// blob and source mask. Generating only long messages hides the bug
// completely (verified: a long-message-only generator scores 0 blank frames
// against the very code that blanked half a phone screen).
function msg(sender, text) {
	const id = ++nextID;
	const tags = `@time=2026-07-25T09:14:${String(id % 60).padStart(2, "0")}.123Z;` +
		`msgid=${"0123456789abcdef".repeat(2)}${id};account=${sender}`;
	return {
		id, network: "net", sender, time: 1_700_000_000_000 + id * 1000,
		command: "PRIVMSG",
		raw: `${tags} :${sender}!~${sender}@user/${sender}/x-${id} PRIVMSG #chan :${text}`,
	};
}

// Realistic length mix: most lines are a handful of words. LENGTHS is drawn
// from the shape of an actual busy channel — a long tail, but a short median.
const LENGTHS = [3, 5, 8, 12, 4, 6, 22, 7, 9, 3, 15, 5, 40, 6, 11, 4];

// Presence lines with the hostmask shapes that actually overflow. Anything
// with a hyphen or a slash has a break opportunity and wraps on its own; the
// dangerous ones are all-dots hostnames, bare IPv6, and long idents.
const HOSTMASKS = [
	"~averylongidentifierhere@host.sub.domain.example.invalid",
	"~u@2001:0db8:0000:0000:0000:8a2e:0370:7334",
	"~identwithoutanybreakopportunities@averylongsingletokenhostname.invalid",
	"~a@node.pool.aaaa.bbbb.cccc.dddd.eeee.ffff.gggg.example.invalid",
];

function presence(i) {
	const id = ++nextID;
	const nick = "user" + (i % 17);
	const mask = HOSTMASKS[i % HOSTMASKS.length];
	return {
		id, network: "net", sender: nick, time: 1_700_000_000_000 + id * 1000,
		command: i % 2 ? "QUIT" : "JOIN",
		raw: `:${nick}!${mask} ${i % 2 ? "QUIT :Quit: leaving" : "JOIN #chan"}`,
	};
}

// A realistic mix: mostly chat of varying length, with presence churn woven
// through it — the combination that produced the blank-viewport bug, since
// presence rows and message rows estimate differently.
function page(n, seed) {
	const out = [];
	for (let i = 0; i < n; i++) {
		const k = seed + i;
		if (k % 5 === 3) {
			out.push(presence(k));
			continue;
		}
		const words = LENGTHS[k % LENGTHS.length];
		let text = "";
		for (let w = 0; w < words; w++) text += (w ? " " : "") + "lorem"[(k + w) % 5].repeat(1 + ((k + w) % 7));
		out.push(msg("user" + (k % 17), text));
	}
	return out;
}

function App() {
	// A buffer big enough that most of it is NEVER measured. With a small
	// list the ResizeObserver measures essentially everything on first render,
	// the estimates stop mattering, and the coverage test becomes vacuous
	// (verified: at 120 rows the pre-fix estimator scores a clean 0 blank
	// frames). Real scrollback is thousands of rows deep; so is this.
	const [msgs, setMsgs] = useState(() => ({
		list: page(3000, 1000), loaded: true, atTail: true, reachedTop: false,
	}));
	const [statusHost, setStatusHost] = useState(true);
	const [seed, setSeed] = useState(0);
	// Bumped on every prepend so the driver can await the commit instead of
	// sleeping and hoping.
	const [commits, setCommits] = useState(0);

	window.__h = {
		commits: () => commits,
		count: () => msgs.list.length,
		// Prepend a page of OLDER history, as onLoadOlder does.
		prepend: (n) => {
			setMsgs((m) => ({ ...m, list: [...page(n, 2000 + nextID), ...m.list] }));
			setCommits((c) => c + 1);
		},
		setStatusHost,
		// Appearance prefs are data attributes on <html> (see applyPrefs), so
		// the driver flips them the same way the app does.
		setLook: (font, size, density) => {
			const r = document.documentElement;
			r.dataset.msgfont = font;
			r.dataset.textsize = size;
			r.dataset.density = density;
			setSeed((s) => s + 1); // force a re-render so layoutKey changes
		},
	};

	return (
		<div class="app">
			<div class="main">
				<Chat
					buf={{ network: "net", buffer: "#chan" }}
					msgs={msgs}
					selfNick="me"
					theme="dark"
					nickColors={{}}
					connected
					typers={[]}
					completionNicks={[]}
					ignoredNicks={[]}
					statusMode="show"
					statusHost={statusHost}
					timeFmt={{ clock: "24" }}
					nickSep=":"
					previews={false}
					highlightNames={false}
					userhosts={new Map()}
					memberPrefixes={new Map()}
					layoutKey={`${statusHost}:${seed}:${document.documentElement.dataset.msgfont}:${document.documentElement.dataset.textsize}`}
					isHighlight={() => false}
					onSend={() => {}}
					onLoadOlder={() => {}}
					onRead={() => {}}
					onTyping={() => {}}
					onNick={() => {}}
				/>
			</div>
		</div>
	);
}

render(<App />, document.getElementById("app"));
