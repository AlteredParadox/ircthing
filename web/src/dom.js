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

// Small DOM helpers, separated from the components so they can be unit
// tested against a stubbed document (the frontend test runner has no DOM).

// isEditable reports whether an element is a text field the user may be
// typing in, so window-refocus / type-anywhere doesn't yank the cursor out
// of it.
export function isEditable(el) {
	const tag = el.tagName;
	return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || el.isContentEditable;
}

export const MODAL_SCRIMS = ".search-scrim, .ctx-scrim, .side-scrim, .right-scrim";

// modalScrimOpen reports whether a *visible* modal overlay is present. It
// checks computed display rather than DOM presence: the side/right drawer
// scrims are always rendered on desktop but display:none there (they are
// modal only at their mobile breakpoints), so a plain querySelector would
// report them as open and disable type-anywhere on desktop.
export function modalScrimOpen() {
	for (const s of document.querySelectorAll(MODAL_SCRIMS)) {
		if (getComputedStyle(s).display !== "none") return true;
	}
	return false;
}
