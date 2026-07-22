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

// Web Push subscribe/unsubscribe flow. The service worker (sw.js) shows
// the notifications; this module manages the subscription lifecycle:
// enable/disable from settings, and the on-load resync that heals iOS
// subscription eviction and VAPID key rotation.

// pushAuthEpoch guards push lifecycle operations against crossing a
// logout/login boundary: logout bumps it, and every async push
// operation captures it up front and bails after each await if it
// changed — so a resync or unsubscribe in flight when the user signs out
// (or back in) cannot register/delete against the wrong session.
let pushAuthEpoch = 0;
export function invalidatePushAuth() {
	pushAuthEpoch++;
}

// pushSupported: Push API present in THIS context. On iOS the API only
// exists inside an installed (home-screen) web app, so a plain Safari
// tab reports false — see isIOSNeedingInstall for the guidance case.
export function pushSupported() {
	return (
		"serviceWorker" in navigator &&
		"PushManager" in globalThis &&
		"Notification" in globalThis &&
		globalThis.isSecureContext === true
	);
}

// isIOSNeedingInstall: an iOS browser tab where push WOULD work if the
// app were added to the home screen (iOS 16.4+ exposes push only to
// installed web apps). iPadOS masquerades as macOS but keeps touch.
export function isIOSNeedingInstall() {
	if (pushSupported()) return false;
	const ua = navigator.userAgent || "";
	const iOS = /iPhone|iPad|iPod/.test(ua) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
	const standalone = navigator.standalone === true || globalThis.matchMedia?.("(display-mode: standalone)")?.matches;
	return iOS && !standalone;
}

// urlB64ToBytes decodes an unpadded base64url applicationServerKey into
// the Uint8Array pushManager.subscribe expects.
export function urlB64ToBytes(s) {
	const pad = "=".repeat((4 - (s.length % 4)) % 4);
	const raw = atob((s + pad).replaceAll("-", "+").replaceAll("_", "/"));
	// codePointAt === charCodeAt here: atob output is latin1 (0-255).
	return Uint8Array.from(raw, (c) => c.codePointAt(0));
}

async function postJSON(path, body) {
	const r = await fetch(path, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify(body),
	});
	if (!r.ok) throw new Error(`${path}: HTTP ${r.status}`);
}

// currentSubscription returns this browser's push subscription, or null.
export async function currentSubscription() {
	if (!pushSupported()) return null;
	const reg = await navigator.serviceWorker.ready;
	return reg.pushManager.getSubscription();
}

// subscribePush runs the full enable flow. Must be called from a user
// gesture (the permission prompt requires one, notably on iOS). Throws
// with .code = "denied" when the user refused notifications.
export async function subscribePush(publicKey) {
	const epoch = pushAuthEpoch;
	const perm = await Notification.requestPermission();
	if (perm !== "granted") {
		const err = new Error("notification permission not granted");
		err.code = "denied";
		throw err;
	}
	const reg = await navigator.serviceWorker.ready;
	if (epoch !== pushAuthEpoch) return null; // logged out mid-flow: don't register
	const sub = await reg.pushManager.subscribe({
		userVisibleOnly: true, // required; and iOS shows every push anyway
		applicationServerKey: urlB64ToBytes(publicKey),
	});
	if (epoch !== pushAuthEpoch) {
		// Session changed between subscribe and the server POST: drop the
		// browser subscription rather than register it against a session
		// the user has left.
		await sub.unsubscribe().catch(() => {});
		return null;
	}
	try {
		await postJSON("/api/push/subscribe", sub.toJSON());
	} catch (e) {
		// The server never learned about it: a dangling browser-side
		// subscription would push nowhere but still block a clean retry.
		await sub.unsubscribe().catch(() => {});
		throw e;
	}
	return sub;
}

// unsubscribePush disables push for this device. Browser-side removal
// wins: even if the server POST fails, the endpoint is dead and the
// server prunes it on the next delivery's 404/410.
export async function unsubscribePush() {
	const sub = await currentSubscription();
	if (!sub) return;
	const endpoint = sub.endpoint;
	await sub.unsubscribe().catch(() => {});
	await postJSON("/api/push/unsubscribe", { endpoint }).catch(() => {});
}

// unsubscribeForLogout is unsubscribePush for the sign-out path. It uses
// getRegistration (which resolves PROMPTLY, unlike serviceWorker.ready
// which never settles if worker activation failed) so logout can neither
// hang on it nor have a late-resolving cleanup act on the NEXT login's
// subscription. Returns quickly; caller need not race it.
export async function unsubscribeForLogout() {
	if (!("serviceWorker" in navigator)) return;
	const reg = await navigator.serviceWorker.getRegistration("/").catch(() => null);
	if (!reg) return;
	const sub = await reg.pushManager.getSubscription().catch(() => null);
	if (!sub) return;
	const endpoint = sub.endpoint;
	await sub.unsubscribe().catch(() => {});
	await postJSON("/api/push/unsubscribe", { endpoint }).catch(() => {});
}

// appServerKeyOf extracts a subscription's server key for comparison
// (comma-joined bytes; ArrayBuffers don't compare structurally).
function appServerKeyOf(sub) {
	const k = sub.options?.applicationServerKey;
	return k ? new Uint8Array(k).join(",") : "";
}

// syncPushOnLoad, called once per app start when the server key is
// known: re-upserts the current subscription (idempotent — heals a
// server DB that lost the row, and iOS quietly rotating the endpoint),
// and rebinds to a rotated VAPID key by re-subscribing.
export async function syncPushOnLoad(publicKey) {
	if (!publicKey) return;
	const epoch = pushAuthEpoch;
	const sub = await currentSubscription().catch(() => null);
	if (!sub || epoch !== pushAuthEpoch) return;
	if (appServerKeyOf(sub) !== urlB64ToBytes(publicKey).join(",")) {
		// Server key rotated (e.g. database reset): the old subscription
		// can never verify again. Rebind without prompting — permission
		// is already granted.
		await sub.unsubscribe().catch(() => {});
		let fresh = null;
		try {
			const reg = await navigator.serviceWorker.ready;
			if (epoch !== pushAuthEpoch) return;
			fresh = await reg.pushManager.subscribe({
				userVisibleOnly: true,
				applicationServerKey: urlB64ToBytes(publicKey),
			});
			if (epoch !== pushAuthEpoch) throw new Error("session changed");
			await postSubscribeWithRetry(fresh.toJSON(), epoch);
		} catch {
			// The server never learned about the fresh subscription: a
			// dangling browser-side one would push nowhere but block a
			// clean re-enable from settings. Re-enable if wanted; push
			// stays off meanwhile.
			await fresh?.unsubscribe().catch(() => {});
		}
		return;
	}
	// Same key: re-upsert with a couple of bounded retries so a transient
	// failure self-heals within the session (the subscription is valid;
	// only the server row is missing) instead of waiting for the next
	// app load. Silent on final failure — the browser sub stays valid.
	await postSubscribeWithRetry(sub.toJSON(), epoch).catch(() => {});
}

// postSubscribeWithRetry POSTs a subscription with bounded backoff,
// bailing if the auth epoch changed (logout/login) between tries.
async function postSubscribeWithRetry(body, epoch) {
	for (let attempt = 0; attempt < 3; attempt++) {
		if (epoch !== pushAuthEpoch) return;
		try {
			await postJSON("/api/push/subscribe", body);
			return;
		} catch (e) {
			if (attempt === 2) throw e;
			await new Promise((r) => setTimeout(r, 500 * 2 ** attempt));
		}
	}
}
