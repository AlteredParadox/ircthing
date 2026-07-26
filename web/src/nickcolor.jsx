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

import { useEffect, useState } from "preact/hooks";
import { nickColor } from "./irc.js";
import { NICK_SWATCHES, normalizeHexColor } from "./prefs.js";

// The color fed to <input type="color"> when the nick has no override yet.
// The hashed default is an oklch() string, which the native picker cannot
// parse (it only accepts "#rrggbb"), so it needs a concrete starting point.
const PICKER_SEED = "#4f7cff";

// NickColorPrompt: the "pick a color for this nick" mini-dialog, opened from
// the user context menu. `value` is the nick's current override ("" for the
// hashed default); onSave(hex) stores a canonical "#rrggbb", onSave("")
// clears the override.
//
// The typed field is the source of truth and is kept as raw text so a
// half-typed "#ab" is not silently rewritten under the cursor; every other
// control (swatches, native picker) writes into it. Saving normalizes.
export function NickColorPrompt({ nick, theme, value, onSave, onClose }) {
	const [text, setText] = useState(value || "");
	const hex = normalizeHexColor(text);
	const invalid = text.trim() !== "" && !hex;
	const preview = hex || nickColor(nick, theme);

	useEffect(() => {
		const onKey = (e) => e.key === "Escape" && onClose();
		globalThis.addEventListener("keydown", onKey);
		return () => globalThis.removeEventListener("keydown", onKey);
	}, [onClose]);

	function submit(e) {
		e.preventDefault();
		if (invalid) return;
		onSave(hex);
	}

	return (
		<div class="search-scrim" aria-hidden="true" onClick={(e) => e.target === e.currentTarget && onClose()}>
			<form class="settings-panel nick-color" onSubmit={submit}>
				<div class="settings-head">
					<div class="settings-title">Color for {nick}</div>
					<button type="button" class="search-close" onClick={onClose} title="Close (Esc)">✕</button>
				</div>
				<div class="settings-body">
					<div class="nc-preview">
						<span class="nc-sample" style={{ color: preview }}>{nick}</span>
						<span class="pref-hint">{hex ? hex : "default (hashed from the nick)"}</span>
					</div>
					<div class="swatches nc-swatches">
						{NICK_SWATCHES.map((c) => (
							<button
								type="button"
								key={c}
								class={"swatch" + (c === hex ? " on" : "")}
								style={{ background: c }}
								title={c}
								aria-label={"Use " + c}
								onClick={() => setText(c)}
							/>
						))}
					</div>
					<div class="nc-fields">
						<input
							class="nc-picker"
							type="color"
							value={hex || PICKER_SEED}
							aria-label="Pick a color"
							onInput={(e) => setText(e.currentTarget.value)}
						/>
						<input
							class={"rule-input nc-hex" + (invalid ? " bad" : "")}
							value={text}
							spellcheck={false}
							placeholder="#rrggbb"
							aria-label="Hex color"
							aria-invalid={invalid ? "true" : "false"}
							onInput={(e) => setText(e.currentTarget.value)}
						/>
					</div>
					{invalid && <div class="cmd-error">not a hex color — use #rgb or #rrggbb</div>}
					<div class="nf-actions">
						<button type="button" class="nf-danger" disabled={!value} onClick={() => onSave("")}>
							Use default
						</button>
						<div class="nf-spacer" />
						<button type="submit" class="btn-accent" disabled={invalid}>Save</button>
					</div>
				</div>
			</form>
		</div>
	);
}
