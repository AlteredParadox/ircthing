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

// Composer input history: Up/Down recall of recently sent messages, one
// history per buffer, in-memory only. All stateful logic lives here (no DOM)
// so it is testable under node --test; the composer supplies the caret
// gating via isFirstLine/isLastLine below.

// isFirstLine reports whether the caret position sits on the first line of
// the text (no newline between position 0 and the caret) — the gate for Up
// recalling history instead of moving the caret a line up.
export function isFirstLine(text, pos) {
	return !text.slice(0, pos).includes("\n");
}

// isLastLine reports whether the caret position sits on the last line (no
// newline at or after the caret) — the gate for Down acting on history. A
// caret just before a trailing "\n" is NOT on the last line: there is an
// (empty) line below it for the native arrow to reach.
export function isLastLine(text, pos) {
	return !text.includes("\n", pos);
}

export const HISTORY_CAP = 50;

// InputHistory records sent messages per buffer key and serves Up/Down
// navigation. The per-buffer state machine is deliberately explicit:
//
//   entries: sent messages, oldest first, capped, consecutive dupes collapsed
//   pos:     null  -> IDLE (composing a fresh draft, not navigating)
//            0..n  -> NAVIGATING, showing entries[pos]
//
// Transitions (navigate; dir -1 = Up/older, +1 = Down/newer):
//   IDLE       + Up   -> show newest entry (null if history is empty)
//   IDLE       + Down -> CLEAR a non-empty draft (explicit product decision:
//                        Down on the last line while typing empties the box);
//                        no-op on an already-empty composer
//   NAVIGATING + Up   -> older entry; at the oldest, no-op (stay)
//   NAVIGATING + Down -> newer entry; PAST the newest, back to IDLE with an
//                        empty composer
//   push (message sent) -> IDLE
//
// There is deliberately NO draft stash: the conventional readline behaviour
// (first Up stashes the in-progress draft, Down past the newest restores it)
// is overridden — Down at the bottom ALWAYS ends with an empty composer,
// whether the user was typing or navigating, so a stash would never be
// restored. The tests pin this.
//
// Editing a recalled entry ends navigation implicitly: navigate compares the
// composer's current text against entries[pos], and on mismatch the text is
// treated as a fresh draft (IDLE rules apply). This needs no input hook, so
// programmatic draft updates (tab completion, the recall itself) cannot
// disturb the state.
export class InputHistory {
	#cap;
	#bufs = new Map(); // bufKey -> { entries: [], pos: null }

	constructor(cap = HISTORY_CAP) {
		this.#cap = cap;
	}

	#st(key) {
		let s = this.#bufs.get(key);
		if (!s) {
			s = { entries: [], pos: null };
			this.#bufs.set(key, s);
		}
		return s;
	}

	// push records a sent message (commands included) and ends any
	// navigation. Empty text is ignored; a repeat of the newest entry is
	// collapsed; past the cap the oldest entry is dropped.
	push(key, text) {
		if (!text) return;
		const s = this.#st(key);
		s.pos = null;
		if (s.entries[s.entries.length - 1] === text) return;
		s.entries.push(text);
		if (s.entries.length > this.#cap) s.entries.shift();
	}

	// navigate applies one Up (dir -1) or Down (dir +1) press. `current` is
	// the composer's present text. Returns { text } with the new composer
	// content (caret goes to its end), or null when the press should fall
	// through to the textarea's native caret movement.
	navigate(key, dir, current) {
		const s = this.#st(key);
		// A recalled entry that no longer matches the composer was edited:
		// it is a fresh draft now, so drop back to IDLE before applying.
		if (s.pos !== null && s.entries[s.pos] !== current) s.pos = null;
		const last = s.entries.length - 1;
		if (dir < 0) {
			if (s.pos === null) {
				if (last < 0) return null;
				s.pos = last;
			} else if (s.pos > 0) {
				s.pos--;
			} else {
				return null; // at the oldest entry: stay
			}
			return { text: s.entries[s.pos] };
		}
		if (s.pos === null) {
			return current === "" ? null : { text: "" }; // typing fresh: Down clears
		}
		if (s.pos < last) {
			s.pos++;
			return { text: s.entries[s.pos] };
		}
		s.pos = null;
		return { text: "" }; // past the newest: clear (no draft restore — see above)
	}
}
