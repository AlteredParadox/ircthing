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
const FAIL_TTL = 5 * 60 * 1000;
const inflight = new Map(); // key -> { promise, controller, refs }

// Thumbnails are fetched as blobs and cached as object URLs (revoked on
// eviction) so the target URL travels in a POST body, not a query string an
// <img src> would put in reverse-proxy access logs.
const thumbCache = new LRU(200, 60 * 60 * 1000, (u) => u && URL.revokeObjectURL(u));
const thumbInflight = new Map();

// Above this many concurrent distinct requests, extra fetches still run and
// cache but are not tracked for dedup/abort — a hard ceiling so a pathological
// burst of distinct URLs cannot grow the maps without bound.
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

// shared runs `run(signal)` deduped by key: concurrent callers share one request
// (and one server-side proxy fetch). It returns { promise, release }; the caller
// MUST release() when it no longer needs the result (component unmount). When the
// LAST waiter releases a still-pending request it is ABORTED — so a preview
// scrolled out of the virtualized list stops the server doing remote work —
// without cancelling a fetch another visible row still needs. The map self-empties
// as requests settle; entries above MAX_INFLIGHT run untracked.
function shared(map, key, run) {
	let e = map.get(key);
	if (!e || e.controller.signal.aborted) {
		const controller = new AbortController();
		e = { controller, refs: 0, promise: null };
		e.promise = run(controller.signal).finally(() => {
			if (map.get(key) === e) map.delete(key);
		});
		if (map.size < MAX_INFLIGHT) map.set(key, e);
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
		mediaFetch("/api/preview", url, net, signal)
			.then((r) => (r.ok ? r.json() : null))
			.catch(() => null)
			.then((data) => {
				// Never cache an aborted fetch's null: it would pin a 5-min failure
				// TTL on a URL we merely cancelled, blocking a legitimate retry.
				if (!signal.aborted) cache.set(key, data, data === null ? FAIL_TTL : undefined);
				return data;
			}));
}

// fetchThumb returns { promise, release }; promise resolves to an object URL or null.
function fetchThumb(url, net) {
	const key = ck(url, net);
	const cached = thumbCache.get(key);
	if (cached !== undefined) return { promise: Promise.resolve(cached), release() {} }; // objURL or cached null
	return shared(thumbInflight, key, (signal) =>
		mediaFetch("/api/thumb", url, net, signal)
			.then((r) => (r.ok ? r.blob() : null))
			.catch(() => null)
			.then((blob) => {
				const obj = blob ? URL.createObjectURL(blob) : null;
				if (signal.aborted) {
					if (obj) URL.revokeObjectURL(obj); // don't leak an object URL we won't cache
					return null;
				}
				thumbCache.set(key, obj, obj === null ? FAIL_TTL : undefined);
				return obj;
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
