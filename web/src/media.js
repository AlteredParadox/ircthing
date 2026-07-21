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

// Inline audio/video playback plumbing (DOM-free; the card component lives in
// preview.jsx). Media elements stream through the server: a short-lived token
// is minted over POST (the target URL travels in the body, never a logged
// query string — the token in the stream URL is sealed/opaque), then the
// element's src points at /api/media/stream. Tokens die on server restart or
// after ~15 minutes; the card re-mints once when an element error looks like
// expiry.

// mintMediaToken POSTs {url, net} and resolves to {token, exp} (exp in unix
// seconds). Rejects on HTTP failure or a malformed body. fetchFn is
// injectable for tests.
export async function mintMediaToken(url, net, fetchFn = fetch) {
	const r = await fetchFn("/api/media/token", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ url, net: net || "" }),
	});
	if (!r.ok) {
		const err = new Error("media token: " + r.status);
		err.status = r.status;
		throw err;
	}
	const d = await r.json();
	if (!d || typeof d.token !== "string" || d.token === "" || typeof d.exp !== "number") {
		throw new Error("media token: malformed response");
	}
	return d;
}

// streamSrc is the <audio>/<video> src for a minted token.
export function streamSrc(token) {
	return "/api/media/stream?t=" + encodeURIComponent(token);
}

// EXPIRY_SKEW_MS: an element error this close to (or past) the token's expiry
// is treated as expiry — the server's clock and the fetch's in-flight time
// make exact comparison useless.
const EXPIRY_SKEW_MS = 30 * 1000;

// shouldRemint decides whether an element error is worth ONE re-mint: only
// when the token has (nearly) expired. A mid-life error is a real stream
// failure (origin gone, unsupported codec) and re-minting would just repeat
// it.
export function shouldRemint(expSec, now = Date.now()) {
	return typeof expSec === "number" && now >= expSec * 1000 - EXPIRY_SKEW_MS;
}

// mediaFileName extracts a display name from the URL's last path segment
// (decoded, query/fragment ignored); falls back to the host when the path is
// bare.
export function mediaFileName(u) {
	try {
		const url = new URL(u);
		const seg = url.pathname.split("/").filter(Boolean).pop() || "";
		if (!seg) return url.host;
		try {
			return decodeURIComponent(seg);
		} catch {
			return seg; // malformed percent-encoding: show it raw
		}
	} catch {
		return u;
	}
}
