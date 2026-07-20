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
import { hostOf, looksLikeImageURL } from "./irc.js";
import { LRU } from "./lru.js";

// Link previews and image thumbnails. All remote content is loaded
// through the server media proxy — the browser never fetches a remote
// origin directly. Results are cached per URL in a bounded LRU so a URL
// repeated in scrollback is fetched once, without the cache growing for
// the lifetime of the page; failures (null) are kept only briefly so a
// flood of dead links cannot occupy the cache and a flaky preview
// retries soon.

// 30 min, matching the server thumbCache and Cache-Control TTLs deliberately:
// redaction does not purge media caches (keys are (net,url), shared across
// messages), so this chain of TTLs is what bounds how long a redacted image
// stays renderable. A longer client TTL would quietly extend that bound past
// the documented 30 minutes.
const MEDIA_TTL = 30 * 60 * 1000;

const cache = new LRU(300, MEDIA_TTL); // url -> PreviewData | null
const FAIL_TTL = 5 * 60 * 1000; // a real failure (dead link, undecodable image)
const RETRY_TTL = 12 * 1000; // a TRANSIENT failure (server slot busy / network) — retry soon
const inflight = new Map(); // key -> { promise, controller, refs }

// Thumbnails are fetched as blobs and cached as object URLs (revoked on
// eviction) so the target URL travels in a POST body, not a query string an
// <img src> would put in reverse-proxy access logs.
const thumbCache = new LRU(200, MEDIA_TTL, (u) => u && URL.revokeObjectURL(u));
const thumbInflight = new Map();

// Above this many concurrent distinct requests, new fetches are REJECTED
// (resolve null, nothing cached) rather than started untracked — an untracked
// request can be neither deduped nor aborted, so a burst of distinct URLs
// would keep the server's single media slot busy on rows already scrolled
// away. Previews are cosmetic: a rejected row renders nothing now and retries
// on the next mount once below the cap.
const MAX_INFLIGHT = 64;

// ABORT_GRACE_MS delays aborting an in-flight fetch after its last waiter
// releases. In a busy channel a link message scrolls out of the virtualized
// render window (unmounting its preview) within a second or two — often BEFORE
// a slow-egress (WireGuard/Tor) fetch has returned. Aborting instantly then
// throws that work away and re-fetches on the next mount, so previews churn
// (context-canceled → refetch) and feel flaky. The grace lets an in-flight
// fetch finish and populate the cache, so scrolling back shows it instantly and
// the server isn't asked twice. A genuinely abandoned fetch (no remount within
// the window) still aborts, bounding runaway work.
const ABORT_GRACE_MS = 8000;

// Cache/fetch key by (network, url): the network selects the server-side
// proxy, so the same URL in two networks is fetched independently.
function ck(url, net) {
	return (net || "") + "\n" + url;
}

// mediaFetch POSTs {url, net} to a media endpoint — the target URL in the body,
// never a logged query string. signal lets a caller abort the request.
function mediaFetch(path, url, net, signal) {
	return fetch(path, {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ url, net: net || "" }),
		signal,
	});
}

// mediaFetch503 wraps mediaFetch with a bounded retry on 503 (the server's media
// slot was briefly busy — common over slow WireGuard/Tor egress). Without it a
// transient 503 blanks the preview for FAIL_TTL, and a message row (keyed by id)
// never re-mounts to retry. Aborts (row scrolled away) stop it immediately.
// Every completed 503 spends the key's network-attempt budget (the final
// failure is counted by the caller), so the in-loop retries and the outer
// auto-retry/remount cycles draw on ONE shared bound — without this, each
// outer cycle multiplied into up to four requests of its own.
async function mediaFetch503(path, url, net, signal, key) {
	let r = await mediaFetch(path, url, net, signal);
	for (let attempt = 0; r.status === 503 && attempt < 3 && !signal.aborted; attempt++) {
		noteFailure(key);
		if (spent(key)) break;
		// Abort-aware backoff: a row scrolled away mid-wait resolves immediately
		// (and its abort tears the fetch down) instead of holding the inflight
		// entry for the full delay.
		await new Promise((res) => {
			const t = setTimeout(res, 1200 * (attempt + 1)); // 1.2s, 2.4s, 3.6s
			signal.addEventListener("abort", () => { clearTimeout(t); res(); }, { once: true });
		});
		if (signal.aborted) break;
		r = await mediaFetch(path, url, net, signal);
	}
	return r;
}

// shared runs `run(signal)` deduped by key: concurrent callers share one request
// (and one server-side proxy fetch). It returns { promise, release }; the caller
// MUST release() when it no longer needs the result (component unmount). When the
// LAST waiter releases a still-pending request it is ABORTED — so a preview
// scrolled out of the virtualized list stops the server doing remote work —
// without cancelling a fetch another visible row still needs. The map self-empties
// as requests settle; at MAX_INFLIGHT, new keys are rejected (see the constant).
function shared(map, key, run) {
	let e = map.get(key);
	if (!e || e.controller.signal.aborted) {
		if (map.size >= MAX_INFLIGHT) return { promise: Promise.resolve(null), release() {} };
		const controller = new AbortController();
		e = { controller, refs: 0, promise: null, abortTimer: null };
		e.promise = run(controller.signal).finally(() => {
			if (e.abortTimer) clearTimeout(e.abortTimer);
			if (map.get(key) === e) map.delete(key);
		});
		map.set(key, e);
	}
	e.refs++;
	// A remount within the grace window cancels a pending abort — the fetch
	// this row shares is still wanted.
	if (e.abortTimer) {
		clearTimeout(e.abortTimer);
		e.abortTimer = null;
	}
	let released = false;
	return {
		promise: e.promise,
		release() {
			if (released) return;
			released = true;
			if (--e.refs <= 0 && map.get(key) === e && !e.abortTimer) {
				// Grace period before aborting: a briefly-unmounted row (message
				// scrolled out of the render window) must not kill an in-flight
				// fetch and force a re-fetch. Abort only if still unwanted after
				// the delay; a completed fetch clears this via the finally above.
				e.abortTimer = setTimeout(() => {
					if (e.refs <= 0 && map.get(key) === e) e.controller.abort();
				}, ABORT_GRACE_MS);
			}
		},
	};
}

// retryCounts tracks COMPLETED failed HTTP attempts per (network, URL) key,
// outside any component: it is the ONE budget every fetch path draws on —
// the in-flight 503 retries inside mediaFetch503, the mounted-row auto-retry
// (useAutoRetry), and fresh fetches from remounts (fetchPreview/fetchThumb
// refuse to fetch once it is spent). Component-local bounds are not enough:
// a per-mount counter reset on every remount, so repeated messages, buffer
// revisits, or virtual-list remounts restarted a full request cycle against
// a hostile origin forever — an unbounded tracking beacon. The count
// increments only when an attempt actually completes and fails (never on
// abort), and resets only on success or when the entry ages out (1h) —
// bounding a permanently failing key to MAX_NET_ATTEMPTS requests per hour.
const retryCounts = new LRU(300, 60 * 60 * 1000); // key -> failed attempts

// MAX_NET_ATTEMPTS is that budget: completed failed HTTP requests per key
// (per retryCounts TTL window) across ALL mounts, remounts, and in-flight
// 503 retries combined.
const MAX_NET_ATTEMPTS = 4;

// spent reports whether a key's network-attempt budget is exhausted.
function spent(key) {
	return (retryCounts.get(key) || 0) >= MAX_NET_ATTEMPTS;
}

function noteFailure(key) {
	retryCounts.set(key, (retryCounts.get(key) || 0) + 1);
}

function noteSuccess(key) {
	// Clear only an EXISTING count: inserting a key per success would let a
	// stream of healthy fetches evict failing keys from the bounded LRU and
	// hand a hostile URL its retry budget back.
	if (retryCounts.get(key)) retryCounts.set(key, 0);
}

// fetchPreview returns { promise, release }; promise resolves to PreviewData|null.
function fetchPreview(url, net) {
	const key = ck(url, net);
	if (cache.has(key)) return { promise: Promise.resolve(cache.get(key)), release() {} };
	// The budget gate must sit on the FETCH, not only on the retry timer: a
	// fresh mount (repeated message, buffer revisit, virtual-list remount)
	// fetches unconditionally once the short failure-cache TTL lapses, which
	// used to hand an exhausted key a whole new request cycle per remount.
	if (spent(key)) return { promise: Promise.resolve(null), release() {} };
	return shared(inflight, key, (signal) =>
		mediaFetch503("/api/preview", url, net, signal, key)
			.then((r) => (r.ok ? r.json() : { __fail: r.status === 503 ? RETRY_TTL : FAIL_TTL }))
			.catch(() => ({ __fail: RETRY_TTL })) // network error: transient, retry soon
			.then((res) => {
				if (signal.aborted) return null; // don't cache a cancelled fetch
				if (res && res.__fail !== undefined) {
					cache.set(key, null, res.__fail); // transient → RETRY_TTL, real → FAIL_TTL
					noteFailure(key); // a completed failed attempt: counts against auto-retry
					return null;
				}
				cache.set(key, res);
				noteSuccess(key);
				return res;
			}));
}

// fetchThumb returns { promise, release }; promise resolves to an object URL or null.
function fetchThumb(url, net) {
	const key = ck(url, net);
	const cached = thumbCache.get(key);
	if (cached !== undefined) return { promise: Promise.resolve(cached), release() {} }; // objURL or cached null
	if (spent(key)) return { promise: Promise.resolve(null), release() {} }; // budget gate (see fetchPreview)
	return shared(thumbInflight, key, (signal) =>
		mediaFetch503("/api/thumb", url, net, signal, key)
			.then((r) => (r.ok ? r.blob().then((b) => ({ blob: b })) : { __fail: r.status === 503 ? RETRY_TTL : FAIL_TTL }))
			.catch(() => ({ __fail: RETRY_TTL })) // network error: transient
			.then((res) => {
				if (res.blob) {
					const obj = URL.createObjectURL(res.blob);
					if (signal.aborted) {
						URL.revokeObjectURL(obj); // don't leak an object URL we won't cache
						return null;
					}
					thumbCache.set(key, obj);
					noteSuccess(key);
					return obj;
				}
				if (signal.aborted) return null; // cancelled: don't cache the failure
				thumbCache.set(key, null, res.__fail); // transient → RETRY_TTL, real → FAIL_TTL
				noteFailure(key); // a completed failed attempt: counts against auto-retry
				return null;
			}));
}

// MAX_AUTO_RETRIES bounds how many times a still-mounted, still-blank preview
// re-attempts on its own. A transient failure caches null for only RETRY_TTL, so
// after that the cache lookup misses and the retry refetches; a permanent failure
// stays cached (FAIL_TTL) so the retry is a cheap cache hit, no network. Without
// this a row that failed transiently (e.g. WireGuard warming up) stays blank until
// it scrolls out and back, because its effect deps never change.
//
// The bound is enforced on TWO axes, both required: retryCounts caps completed
// failed NETWORK attempts per (network, URL) at MAX_NET_ATTEMPTS, shared across
// every row, mount, and in-flight 503 retry — and enforced at the fetch itself,
// so remounts cannot bypass it; the local tick additionally caps how often one
// mounted row re-runs its effect against a cached failure.
const MAX_AUTO_RETRIES = 3;

// useAutoRetry returns a counter that increments up to MAX_AUTO_RETRIES, once
// per RETRY_TTL, while `blank` is true. It is a deps-changing tick that re-runs
// a fetch effect. blank must mean "a COMPLETED attempt failed" — never "still
// loading" — so no timer runs while a request is in flight (a live timer used
// to abort a healthy request that was merely slower than the retry interval).
// The tick is keyed to (network, URL): a reused component with a new key starts
// from 0 instead of inheriting an exhausted counter, and never resets merely
// because blank flickered false during a refetch.
function useAutoRetry(blank, key) {
	const [last, setLast] = useState({ key, tick: 0 });
	const tick = last.key === key ? last.tick : 0;
	useEffect(() => {
		if (!blank || tick >= MAX_AUTO_RETRIES) return undefined;
		if (spent(key)) return undefined; // network budget gone: don't even tick
		const t = setTimeout(() => setLast({ key, tick: tick + 1 }), RETRY_TTL + 500);
		return () => clearTimeout(t);
	}, [blank, key, tick]);
	return tick;
}

// useThumb fetches a thumbnail's object URL, null while loading or on failure.
function useThumb(url, net) {
	const [src, setSrc] = useState(() => {
		const c = thumbCache.get(ck(url, net));
		return c === undefined ? null : c;
	});
	const [blank, setBlank] = useState(false);
	const tick = useAutoRetry(blank, ck(url, net));
	useEffect(() => {
		let alive = true;
		const settle = (u) => {
			if (!alive) return;
			setSrc(u);
			setBlank(u === null); // a COMPLETED failure; arms the bounded retry
		};
		const c = thumbCache.get(ck(url, net));
		if (c !== undefined) {
			settle(c);
			return undefined;
		}
		setSrc(null);
		// Loading is not blank: clearing it disarms the retry timer while the
		// request is in flight, so a slow-but-healthy fetch (backend allows
		// 15s; the retry interval is shorter) is never aborted and restarted
		// by its own retry.
		setBlank(false);
		const { promise, release } = fetchThumb(url, net);
		promise.then(settle);
		return () => {
			alive = false;
			release(); // aborts the proxy fetch if no other row still needs it
		};
	}, [url, net, tick]);
	return src;
}

// LinkPreview renders one URL's preview. An OBVIOUS image URL goes straight to a
// thumbnail — no /api/preview round-trip, which would only tell us it's an image
// (a second remote request to the same target, a second tracking hit). Anything
// else fetches page metadata via LinkCard. (No hooks here, so the branch is a
// legal conditional render; each sub-component owns its own effects.)
export function LinkPreview({ url, net }) {
	if (looksLikeImageURL(url)) return <ImageThumb url={url} net={net} />;
	return <LinkCard url={url} net={net} />;
}

// LinkCard fetches page metadata for a non-obvious-image URL and renders a card
// (or an inline thumbnail if the server reports the target is actually an image,
// e.g. an extension-less image URL). Renders nothing until data resolves, and
// nothing at all on failure.
function LinkCard({ url, net }) {
	const [data, setData] = useState(() => (cache.has(ck(url, net)) ? cache.get(ck(url, net)) : undefined));
	// blank => a COMPLETED fetch resolved to null (failure); while a fetch is in
	// flight blank stays false, so no retry timer runs against it. A resolved
	// PreviewData with no renderable fields is a SUCCESS (cached as a value), so
	// it does not retry.
	const [blank, setBlank] = useState(false);
	const tick = useAutoRetry(blank, ck(url, net));

	// Re-resolve whenever url or net changes: a reused component must not keep
	// rendering the previous url's preview, and the network selects the proxy.
	// tick drives the bounded auto-retry for a transiently failed row.
	useEffect(() => {
		let alive = true;
		const cached = cache.has(ck(url, net)) ? cache.get(ck(url, net)) : undefined;
		setData(cached);
		setBlank(cached === null);
		if (cached === undefined) {
			const { promise, release } = fetchPreview(url, net);
			promise.then((d) => {
				if (!alive) return;
				setData(d);
				setBlank(d === null);
			});
			return () => {
				alive = false;
				release(); // aborts the proxy fetch if no other row still needs it
			};
		}
		return () => {
			alive = false;
		};
	}, [url, net, tick]);

	if (!data) return null; // loading or failed: no card (not an obvious image)
	if (data.kind === "image") return <ImageThumb url={data.image || url} net={net} />;
	if (!data.title && !data.description && !data.image) return null;

	return (
		<a class="preview-card" href={url} target="_blank" rel="noopener noreferrer">
			{data.image && <CardImg url={data.image} net={net} />}
			<div class="preview-card-body">
				<div class="preview-card-site">{data.site_name || hostOf(url)}</div>
				{data.title && <div class="preview-card-title">{data.title}</div>}
				{data.description && <div class="preview-card-desc">{data.description}</div>}
			</div>
		</a>
	);
}

function CardImg({ url, net }) {
	const src = useThumb(url, net);
	return src ? <img class="preview-card-img" src={src} alt="" /> : null;
}

function ImageThumb({ url, net }) {
	const src = useThumb(url, net);
	if (!src) return null;
	return (
		<a class="preview-thumb" href={url} target="_blank" rel="noopener noreferrer">
			<img src={src} alt="" />
		</a>
	);
}
