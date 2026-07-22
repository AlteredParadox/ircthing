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

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

self.addEventListener("push", (event) => {
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
		self.registration.showNotification(title, {
			body,
			// One notification per buffer: a later push replaces the
			// earlier one instead of stacking.
			tag: network && buffer ? `${network}/${buffer}` : "ircthing",
			data: { network, buffer },
		}),
	);
});

self.addEventListener("notificationclick", (event) => {
	event.notification.close();
	const { network, buffer } = event.notification.data || {};
	// Hash shape mirrors toHash (web/src/irc.js): #/<network>/<buffer>.
	const hash = network && buffer ? `#/${encodeURIComponent(network)}/${encodeURIComponent(buffer)}` : "";
	event.waitUntil(
		(async () => {
			const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
			if (wins.length) {
				// Focus the existing app and let it navigate itself — a
				// client.navigate would reload the whole SPA.
				await wins[0].focus().catch(() => {});
				if (network && buffer) wins[0].postMessage({ type: "open_buffer", network, buffer });
				return;
			}
			await self.clients.openWindow(`/${hash}`);
		})(),
	);
});

self.addEventListener("pushsubscriptionchange", (event) => {
	// Safari fires this rarely (and unreliably); the app-load resync in
	// push.js is the real repair path. Best effort here for browsers
	// that do fire it: re-subscribe with the same server key and
	// re-register (the session cookie rides the same-origin fetch).
	const opts = event.oldSubscription?.options;
	if (!opts?.applicationServerKey) return;
	event.waitUntil(
		(async () => {
			try {
				const sub = await self.registration.pushManager.subscribe(opts);
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
