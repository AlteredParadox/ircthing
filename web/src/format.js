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

// Composer-side mIRC formatting insertion. Pure text/selection math, no DOM —
// the composer reads the textarea's selection, calls applyFormat, and writes
// back {text, selStart, selEnd}. The renderer side of these codes lives in
// irc.js (parseFormatting); the byte values here mirror its FMT_TOGGLES.

export const BOLD = "\x02";
export const ITALIC = "\x1d";
export const UNDERLINE = "\x1f";
export const STRIKE = "\x1e";
export const MONO = "\x11";
export const COLOR = "\x03";
export const RESET = "\x0f";

const isDigit = (c) => c >= "0" && c <= "9";

// colorCode returns the foreground colour code for palette index n, ALWAYS
// two digits ("\x0304"), so inserting it before literal digits in the text
// ("\x0304" + "5…") cannot extend the code — parseIndexedColor consumes at
// most two digits.
export function colorCode(n) {
	return COLOR + String(n).padStart(2, "0");
}

// applyFormat inserts a formatting toggle into composer text.
//
// With a selection, the selected text is wrapped: the code before it and a
// closing toggle after it (the attribute codes are toggles, so the trailing
// byte turns the style back off; a colour closes with a bare \x03). The
// returned selection covers the same characters, so repeated toggles stack
// (select, Ctrl+B, Ctrl+I).
//
// With a collapsed caret, the toggle is inserted at the caret and the caret
// lands just after it, ready to type styled text.
//
// RESET is not a paired toggle: it is inserted alone (leading only, when a
// selection exists — everything after it renders unstyled anyway).
export function applyFormat(text, selStart, selEnd, code) {
	const open = code;
	let close = code === RESET ? "" : code.startsWith(COLOR) ? COLOR : code;
	// A bare closing \x03 immediately before a digit ("\x0304abc" + "\x03" +
	// "5…") would be parsed as a NEW colour code and eat the digit; same for
	// ",digit" (consumed as a background). Close with a full reset in that
	// rare case so what the user selected is exactly what stays coloured.
	if (
		close === COLOR &&
		(isDigit(text[selEnd]) || (text[selEnd] === "," && isDigit(text[selEnd + 1])))
	) {
		close = RESET;
	}
	if (selStart === selEnd) {
		const caret = selStart + open.length;
		return {
			text: text.slice(0, selStart) + open + text.slice(selStart),
			selStart: caret,
			selEnd: caret,
		};
	}
	return {
		text: text.slice(0, selStart) + open + text.slice(selStart, selEnd) + close + text.slice(selEnd),
		selStart: selStart + open.length,
		selEnd: selEnd + open.length,
	};
}
