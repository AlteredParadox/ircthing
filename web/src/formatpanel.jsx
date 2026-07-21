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

import { useEffect, useRef } from "preact/hooks";
import { BOLD, colorCode, ITALIC, MONO, RESET, STRIKE, UNDERLINE } from "./format.js";
import { IRC_PALETTE } from "./irc.js";

// FormatPanel is the composer's formatting control: the "A" toggle button in
// the compose row plus, when open, a small popover anchored above it with the
// style toggles (B/I/U/S/monospace), the 16 classic mIRC foreground colours,
// and a reset. It is deliberately NOT modal (no scrim, unlike ContextMenu):
// the user keeps typing while it is open — style toggles apply and stay open
// for repeated use; a colour or reset applies and closes. Escape or an
// outside click closes it.
//
// Every button prevents mousedown default so the composer textarea keeps
// focus AND its selection — the click then applies to that selection.

const STYLES = [
	{ code: BOLD, label: "B", title: "Bold (Ctrl+B)", cls: "fmt-style-b" },
	{ code: ITALIC, label: "I", title: "Italic (Ctrl+I)", cls: "fmt-style-i" },
	{ code: UNDERLINE, label: "U", title: "Underline (Ctrl+U)", cls: "fmt-style-u" },
	{ code: STRIKE, label: "S", title: "Strikethrough", cls: "fmt-style-s" },
	{ code: MONO, label: "M", title: "Monospace", cls: "fmt-style-m" },
];

const keepFocus = (e) => e.preventDefault();

function FormatIcon() {
	// An "A" over a baseline bar — the usual text-format glyph (CLAUDE.md:
	// inline SVG only).
	return (
		<svg viewBox="0 0 24 24" width="15" height="15" aria-hidden="true">
			<path
				d="M7 15.5 12 4l5 11.5M8.8 11.5h6.4"
				fill="none"
				stroke="currentColor"
				stroke-width="2"
				stroke-linecap="round"
				stroke-linejoin="round"
			/>
			<path d="M5.5 20h13" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" />
		</svg>
	);
}

export function FormatPanel({ open, onToggle, onClose, onApply }) {
	const ref = useRef(null);

	useEffect(() => {
		if (!open) return;
		const onKey = (e) => e.key === "Escape" && onClose();
		// Close on any press outside the anchor (button + popover). Panel
		// buttons preventDefault their mousedown but the event still fires,
		// so containment — not default-prevented state — is the test.
		const onDown = (e) => {
			if (ref.current && !ref.current.contains(e.target)) onClose();
		};
		globalThis.addEventListener("keydown", onKey);
		document.addEventListener("mousedown", onDown);
		return () => {
			globalThis.removeEventListener("keydown", onKey);
			document.removeEventListener("mousedown", onDown);
		};
	}, [open]);

	return (
		<div class="fmt-anchor" ref={ref}>
			<button
				type="button"
				class={"icon-btn fmt-toggle" + (open ? " active" : "")}
				title="Formatting (Alt+F)"
				aria-label="Formatting"
				aria-expanded={open}
				onMouseDown={keepFocus}
				onClick={onToggle}
			>
				<FormatIcon />
			</button>
			{open && (
				<div class="fmt-panel" role="menu" aria-label="Formatting">
					<div class="fmt-styles">
						{STYLES.map((s) => (
							<button
								key={s.label}
								type="button"
								class={"icon-btn fmt-style " + s.cls}
								title={s.title}
								onMouseDown={keepFocus}
								onClick={() => onApply(s.code)} // stays open: toggles repeat
							>
								{s.label}
							</button>
						))}
					</div>
					<div class="fmt-colors">
						{IRC_PALETTE.slice(0, 16).map((hex, i) => (
							<button
								key={i}
								type="button"
								class="fmt-swatch"
								style={{ background: hex }}
								title={`Colour ${String(i).padStart(2, "0")}`}
								aria-label={`Colour ${i}`}
								onMouseDown={keepFocus}
								onClick={() => {
									onApply(colorCode(i));
									onClose();
								}}
							/>
						))}
					</div>
					<button
						type="button"
						class="fmt-reset"
						title="Insert a formatting reset"
						onMouseDown={keepFocus}
						onClick={() => {
							onApply(RESET);
							onClose();
						}}
					>
						Reset formatting
					</button>
				</div>
			)}
		</div>
	);
}
