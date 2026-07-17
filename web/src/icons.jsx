// Hand-written inline SVG icons (CLAUDE.md: inline SVG only, no icon packs).

// BufIcon is the sidebar/switcher buffer glyph: a filled chat bubble for a
// channel, an outline bubble for a query/PM. Differentiating by icon (like
// The Lounge) instead of a text '#'/'@' prefix keeps the full channel name
// readable — a leading prefix clashes with the channel's own '#', turning
// "##Llamas" into a confusing "# #Llamas".
const bubble = "M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z";

export function BufIcon({ chan }) {
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
