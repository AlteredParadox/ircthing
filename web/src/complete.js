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

// Composer tab-completion: commands, emoji shortcodes, and nicks.
// completions() is the pure candidate finder; Completer adds the
// Tab/Shift+Tab cycling state the textarea handler drives.

import { EMOJI } from "./emoji.js";
import { COMMANDS } from "./irc.js";

// completions finds candidates for the token ending at caret:
//   - "/pa"  at the start of the line -> commands ("/part ")
//   - ":fi"  anywhere                 -> emoji ("🔥 ")
//   - "ali"  anywhere                 -> nicks ("alice: " at the start
//     of the line, "alice " elsewhere)
// Returns { start, options } (options include their trailing space), or
// null when there is nothing to complete.
export function completions(text, caret, { nicks = [] } = {}) {
	// Token = the maximal non-whitespace run ending at the caret, found
	// by a backward scan (the \S*$ regex shape is quadratic on pastes).
	let start = caret;
	while (start > 0 && !/\s/.test(text[start - 1])) start--;
	const token = text.slice(start, caret);
	if (!token) return null;

	if (token.startsWith("/") && start === 0) {
		const q = token.slice(1).toLowerCase();
		const options = COMMANDS.filter((c) => c.startsWith(q)).map((c) => "/" + c + " ");
		return options.length ? { start, options } : null;
	}
	if (token.startsWith(":") && token.length > 1) {
		const q = token.slice(1).toLowerCase();
		const options = EMOJI.filter(([name]) => name.startsWith(q)).map(([, ch]) => ch + " ");
		return options.length ? { start, options } : null;
	}
	const options = nickOptions(nicks, token.toLowerCase(), start === 0);
	return options.length ? { start, options } : null;
}

// nickOptions matches roster nicks by case-insensitive prefix, deduped,
// with the conventional suffix: "nick: " at the start of the line,
// "nick " elsewhere.
function nickOptions(nicks, q, atStart) {
	const seen = new Set();
	const options = [];
	for (const n of nicks) {
		const low = n?.toLowerCase();
		if (low?.startsWith(q) && !seen.has(low)) {
			seen.add(low);
			options.push(n + (atStart ? ": " : " "));
		}
	}
	return options;
}

// Completer cycles through candidates on repeated Tab. A cycle stays
// alive as long as the text/caret are exactly what the last completion
// produced; any other edit starts a fresh completion.
export class Completer {
	constructor() {
		this.reset();
	}

	reset() {
		this.options = null;
		this.idx = 0;
		this.head = "";
		this.tail = "";
		this.applied = null;
	}

	// next returns { text, caret } for the following candidate (dir +1)
	// or the previous one (dir -1), or null when nothing completes.
	next(text, caret, dir, ctx) {
		if (this.applied?.text === text && this.applied.caret === caret) {
			this.idx = (this.idx + dir + this.options.length) % this.options.length;
		} else {
			const c = completions(text, caret, ctx);
			if (!c) {
				this.reset();
				return null;
			}
			this.options = c.options;
			this.head = text.slice(0, c.start);
			this.tail = text.slice(caret);
			this.idx = dir < 0 ? c.options.length - 1 : 0;
		}
		const opt = this.options[this.idx];
		this.applied = {
			text: this.head + opt + this.tail,
			caret: this.head.length + opt.length,
		};
		return this.applied;
	}
}
