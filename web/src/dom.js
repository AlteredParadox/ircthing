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

// SPACE_ACTIVATES matches elements (or ancestors of the focused node) for
// which Space is an activation key rather than typing: native buttons
// activate on keyup (HTML "activation behavior"), the pressable/menuTrigger
// rows and context-menu items handle it on keydown, and <summary> toggles.
const SPACE_ACTIVATES = "button, [role=button], [role=menuitem], a[href], summary";

// typeAnywhereKey reports whether a keydown that fired with `active`
// focused (and NOT the composer) should be routed into the composer: a
// plain printable key outside a text field. Space on a button-like element
// is left alone — stealing focus on keydown would drop the button's
// activation (a native button activates on keyup, which now lands in the
// textarea) and insert a stray space into the draft.
export function typeAnywhereKey(e, active) {
	if (e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return false; // shortcuts / IME
	if (e.key.length !== 1) return false; // Tab/Enter/Escape/arrows/F-keys: ignore
	if (!active || active === document.body) return true;
	if (isEditable(active)) return false;
	if (e.key === " " && typeof active.closest === "function" && active.closest(SPACE_ACTIVATES)) return false;
	return true;
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
