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

// Client-side, per-browser lists that shape what you see: ignored nicks
// (per network) and muted buffers. Persisted in localStorage, like the
// highlight rules — deliberately not synced to the server, since "who I
// ignore on this laptop" is a local preference.

function load(key, fallback) {
	try {
		const v = JSON.parse(localStorage.getItem(key));
		return v ?? fallback;
	} catch {
		return fallback;
	}
}

// ignores: { network: [foldedNick, ...] }. Nicks are compared
// ASCII-lowercased — a good enough fold without the server's casemapping.
export function loadIgnores() {
	const v = load("ignores", {});
	return v && typeof v === "object" && !Array.isArray(v) ? v : {};
}

export function isIgnored(ignores, network, nick) {
	// Array.isArray guard: a corrupt store where a network maps to a non-array
	// (e.g. {"libera": 5}) would otherwise throw in this message hot path.
	const list = ignores[network];
	return !!nick && Array.isArray(list) && list.includes(nick.toLowerCase());
}

// toggleIgnore returns the updated map (persisted); empty network lists
// are pruned so the map does not grow unbounded.
export function toggleIgnore(ignores, network, nick) {
	const n = nick.toLowerCase();
	const cur = ignores[network] || [];
	const has = cur.includes(n);
	const list = has ? cur.filter((x) => x !== n) : [...cur, n];
	const next = { ...ignores };
	if (list.length) next[network] = list;
	else delete next[network];
	localStorage.setItem("ignores", JSON.stringify(next));
	return next;
}

// ignoredFor returns the ignore list for one network (never undefined, always
// an array even if the stored value for the network is corrupt).
export function ignoredFor(ignores, network) {
	return Array.isArray(ignores[network]) ? ignores[network] : [];
}

// Last-viewed buffer, restored on a cold start whose URL carries no
// hash. iOS home-screen apps relaunch at start_url (hashless) whenever
// the OS evicted the page — which is most reopens — so without this
// every relaunch landed on the alphabetically-first buffer, i.e. the
// "*" server buffer.
export function loadActiveBuffer() {
	const v = load("activeBuffer", null);
	return v && typeof v.network === "string" && typeof v.buffer === "string" ? v : null;
}

export function saveActiveBuffer(network, buffer) {
	try {
		localStorage.setItem("activeBuffer", JSON.stringify({ network, buffer }));
	} catch {
		// Private mode / quota: reopening falls back to the default pick.
	}
}

// mutes: [bufKey, ...]. A muted buffer still counts unread (dimmed in the
// sidebar) but never highlights, reddens the favicon, or notifies.
export function loadMutes() {
	const v = load("mutes", []);
	return Array.isArray(v) ? v : [];
}

export function isMuted(mutes, key) {
	return mutes.includes(key);
}

export function toggleMute(mutes, key) {
	const next = mutes.includes(key) ? mutes.filter((k) => k !== key) : [...mutes, key];
	localStorage.setItem("mutes", JSON.stringify(next));
	return next;
}
