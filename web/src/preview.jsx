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

const cache = new LRU(300, 60 * 60 * 1000); // url -> PreviewData | null
const FAIL_TTL = 5 * 60 * 1000; // a real failure (dead link, undecodable image)
const RETRY_TTL = 12 * 1000; // a TRANSIENT failure (server slot busy / network) — retry soon
const inflight = new Map(); // key -> { promise, controller, refs }

// Thumbnails are fetched as blobs and cached as object URLs (revoked on
// eviction) so the target URL travels in a POST body, not a query string an
// <img src> would put in reverse-proxy access logs.
const thumbCache = new LRU(200, 60 * 60 * 1000, (u) => u && URL.revokeObjectURL(u));
const thumbInflight = new Map();

// Above this many concurrent distinct requests, new fetches are REJECTED
// (resolve null, nothing cached) rather than started untracked — an untracked
// request can be neither deduped nor aborted, so a burst of distinct URLs
// would keep the server's single media slot busy on rows already scrolled
// away. Previews are cosmetic: a rejected row renders nothing now and retries
// on the next mount once below the cap.
const MAX_INFLIGHT = 64;

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
async function mediaFetch503(path, url, net, signal) {
	let r = await mediaFetch(path, url, net, signal);
	for (let attempt = 0; r.status === 503 && attempt < 3 && !signal.aborted; attempt++) {
		await new Promise((res) => setTimeout(res, 1200 * (attempt + 1))); // 1.2s, 2.4s, 3.6s
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
		e = { controller, refs: 0, promise: null };
		e.promise = run(controller.signal).finally(() => {
			if (map.get(key) === e) map.delete(key);
		});
		map.set(key, e);
	}
	e.refs++;
	let released = false;
	return {
		promise: e.promise,
		release() {
			if (released) return;
			released = true;
			if (--e.refs <= 0 && map.get(key) === e) e.controller.abort();
		},
	};
}

// fetchPreview returns { promise, release }; promise resolves to PreviewData|null.
function fetchPreview(url, net) {
	const key = ck(url, net);
	if (cache.has(key)) return { promise: Promise.resolve(cache.get(key)), release() {} };
	return shared(inflight, key, (signal) =>
		mediaFetch503("/api/preview", url, net, signal)
			.then((r) => (r.ok ? r.json() : { __fail: r.status === 503 ? RETRY_TTL : FAIL_TTL }))
			.catch(() => ({ __fail: RETRY_TTL })) // network error: transient, retry soon
			.then((res) => {
				if (signal.aborted) return null; // don't cache a cancelled fetch
				if (res && res.__fail !== undefined) {
					cache.set(key, null, res.__fail); // transient → RETRY_TTL, real → FAIL_TTL
					return null;
				}
				cache.set(key, res);
				return res;
			}));
}

// fetchThumb returns { promise, release }; promise resolves to an object URL or null.
function fetchThumb(url, net) {
	const key = ck(url, net);
	const cached = thumbCache.get(key);
	if (cached !== undefined) return { promise: Promise.resolve(cached), release() {} }; // objURL or cached null
	return shared(thumbInflight, key, (signal) =>
		mediaFetch503("/api/thumb", url, net, signal)
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
					return obj;
				}
				if (signal.aborted) return null; // cancelled: don't cache the failure
				thumbCache.set(key, null, res.__fail); // transient → RETRY_TTL, real → FAIL_TTL
				return null;
			}));
}

// useThumb fetches a thumbnail's object URL, null while loading or on failure.
function useThumb(url, net) {
	const [src, setSrc] = useState(() => {
		const c = thumbCache.get(ck(url, net));
		return c === undefined ? null : c;
	});
	useEffect(() => {
		let alive = true;
		const c = thumbCache.get(ck(url, net));
		if (c !== undefined) {
			setSrc(c);
			return undefined;
		}
		setSrc(null);
		const { promise, release } = fetchThumb(url, net);
		promise.then((u) => alive && setSrc(u));
		return () => {
			alive = false;
			release(); // aborts the proxy fetch if no other row still needs it
		};
	}, [url, net]);
	return src;
}

// LinkPreview renders one URL's preview: an inline thumbnail for images,
// or a compact card (image + title + description) for pages. Renders
// nothing until data resolves, and nothing at all on failure.
export function LinkPreview({ url, net }) {
	const [data, setData] = useState(() => (cache.has(ck(url, net)) ? cache.get(ck(url, net)) : undefined));

	// Re-resolve whenever url or net changes: a reused component must not
	// keep rendering the previous url's preview, and the network selects the
	// fetch's proxy.
	useEffect(() => {
		let alive = true;
		const cached = cache.has(ck(url, net)) ? cache.get(ck(url, net)) : undefined;
		setData(cached);
		if (cached === undefined) {
			const { promise, release } = fetchPreview(url, net);
			promise.then((d) => alive && setData(d));
			return () => {
				alive = false;
				release(); // aborts the proxy fetch if no other row still needs it
			};
		}
		return () => {
			alive = false;
		};
	}, [url, net]);

	// Fast path: obvious image URLs render a thumbnail without waiting for
	// the preview metadata round-trip.
	if (data === undefined) {
		return looksLikeImageURL(url) ? <ImageThumb url={url} net={net} /> : null;
	}
	if (!data) return looksLikeImageURL(url) ? <ImageThumb url={url} net={net} /> : null;
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
