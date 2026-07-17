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
