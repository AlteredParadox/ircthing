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
	return !!nick && (ignores[network] || []).includes(nick.toLowerCase());
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

// ignoredFor returns the ignore list for one network (never undefined).
export function ignoredFor(ignores, network) {
	return ignores[network] || [];
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
