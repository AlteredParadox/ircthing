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

// Service worker for Web Push ONLY: no fetch handler, no caching — the
// app's network and auth paths are untouched. Payload shape comes from
// the server's pushPayload (internal/hub/push.go).

globalThis.addEventListener("install", () => globalThis.skipWaiting());
globalThis.addEventListener("activate", (event) => event.waitUntil(globalThis.clients.claim()));

globalThis.addEventListener("push", (event) => {
	// iOS revokes the push subscription after a few pushes that display
	// nothing, so ALWAYS show a notification — an unparseable payload
	// gets a generic one rather than being swallowed.
	let d = null;
	try {
		d = event.data ? event.data.json() : null;
	} catch {
		d = null;
	}
	const network = typeof d?.network === "string" ? d.network : "";
	const buffer = typeof d?.buffer === "string" ? d.buffer : "";
	let title = "ircthing";
	let body = "New message";
	if (d?.sender) {
		title = d.channel && buffer ? `${d.sender} in ${buffer}` : d.sender;
		body = typeof d.text === "string" ? d.text : "";
		if (d.count > 1) body = `${body}\n(and ${d.count - 1} more)`;
	}
	event.waitUntil(
		globalThis.registration.showNotification(title, {
			body,
			// One notification per buffer: a later push replaces the
			// earlier one instead of stacking. The tag matches the
			// FOREGROUND notifier's (bufKey: network + "\n" + buffer,
			// see notify.js/app.jsx) — tags are origin-scoped, so with
			// both notification paths enabled a push REPLACES the
			// instant alert for the same buffer rather than duplicating
			// it.
			tag: network && buffer ? `${network}\n${buffer}` : "ircthing",
			data: { network, buffer },
		}),
	);
});

// The last tapped notification's target. postMessage to a suspended or
// OS-killed page is silently lost (iOS evicts aggressively), so the page
// also PULLS this on startup/resume via get_pending_nav. Worker-global
// state is enough: the app resumes within moments of the tap, while this
// worker instance is still alive from handling it.
let pendingNav = null;

globalThis.addEventListener("notificationclick", (event) => {
	event.notification.close();
	const { network, buffer } = event.notification.data || {};
	// Hash shape mirrors toHash (web/src/irc.js): #/<network>/<buffer>.
	const hash = network && buffer ? `#/${encodeURIComponent(network)}/${encodeURIComponent(buffer)}` : "";
	if (network && buffer) pendingNav = { network, buffer, at: Date.now() };
	event.waitUntil(
		(async () => {
			// Focus an existing app window and let it navigate itself — a
			// client.navigate would reload the whole SPA. matchAll can
			// return STALE clients whose page the OS already killed; if
			// none can be focused, fall through to a fresh window.
			const wins = await globalThis.clients.matchAll({ type: "window", includeUncontrolled: true });
			for (const w of wins) {
				try {
					await w.focus();
					// `at` lets the page drop a stale delivery: a message
					// posted to a frozen-but-alive client (iOS) queues and
					// can fire hours later when the page finally resumes.
					if (network && buffer) w.postMessage({ type: "open_buffer", network, buffer, at: Date.now() });
					return;
				} catch {
					// Stale client: try the next.
				}
			}
			await globalThis.clients.openWindow(`/${hash}`);
		})(),
	);
});

globalThis.addEventListener("message", (event) => {
	// Only same-origin pages can hold a reference to this worker, but
	// verify anyway: a cross-origin sender has no business here. Empty
	// origin is tolerated as same-origin — some engines omit it on
	// client->worker messages, and rejecting those would silently break
	// the pull path this handler exists for.
	if (event.origin && event.origin !== globalThis.location.origin) return;
	if (event.data?.type !== "get_pending_nav") return;
	const nav = pendingNav;
	pendingNav = null; // deliver once
	// Staleness cap: a tap the page never picked up must not yank a
	// deliberately-opened session to an old target minutes later.
	if (nav && Date.now() - nav.at < 60 * 1000) {
		event.source?.postMessage({ type: "open_buffer", network: nav.network, buffer: nav.buffer, at: nav.at });
	}
});

globalThis.addEventListener("pushsubscriptionchange", (event) => {
	// Safari fires this rarely (and unreliably); the app-load resync in
	// push.js is the real repair path. Best effort here for browsers
	// that do fire it: re-subscribe with the same server key and
	// re-register (the session cookie rides the same-origin fetch).
	const opts = event.oldSubscription?.options;
	if (!opts?.applicationServerKey) return;
	event.waitUntil(
		(async () => {
			try {
				const sub = await globalThis.registration.pushManager.subscribe(opts);
				await fetch("/api/push/subscribe", {
					method: "POST",
					headers: { "Content-Type": "application/json" },
					body: JSON.stringify(sub.toJSON()),
				});
			} catch {
				// Unrecoverable here (permission gone, auth expired): the
				// next app open re-syncs or surfaces the off state.
			}
		})(),
	);
});
