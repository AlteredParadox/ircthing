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

// Hand-written inline SVG icons (CLAUDE.md: inline SVG only, no icon packs).

// BufIcon is the sidebar/switcher buffer glyph: a filled chat bubble for a
// channel, an outline bubble for a query/PM. Differentiating by icon (like
// The Lounge) instead of a text '#'/'@' prefix keeps the full channel name
// readable — a leading prefix clashes with the channel's own '#', turning
// "##Llamas" into a confusing "# #Llamas".
const bubble = "M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z";

export function BufIcon({ chan, server }) {
	if (server) {
		// A globe for the network/server buffer (the lobby).
		return (
			<svg class="buf-icon" viewBox="0 0 24 24" width="15" height="15" aria-hidden="true">
				<circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" stroke-width="2" />
				<path
					d="M3 12h18M12 3c2.5 2.4 4 5.6 4 9s-1.5 6.6-4 9c-2.5-2.4-4-5.6-4-9s1.5-6.6 4-9z"
					fill="none"
					stroke="currentColor"
					stroke-width="2"
					stroke-linejoin="round"
				/>
			</svg>
		);
	}
	return (
		<svg class="buf-icon" viewBox="0 0 24 24" width="15" height="15" aria-hidden="true">
			<path
				d={bubble}
				fill={chan ? "currentColor" : "none"}
				stroke="currentColor"
				stroke-width={chan ? "0" : "2.2"}
				stroke-linejoin="round"
			/>
		</svg>
	);
}
