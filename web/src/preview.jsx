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
const inflight = new Map(); // url -> Promise

// Cache/fetch key by (network, url): the network selects the server-side
// proxy, so the same URL in two networks is fetched independently.
function ck(url, net) {
	return (net || "") + "\n" + url;
}

function fetchPreview(url, net) {
	const key = ck(url, net);
	if (cache.has(key)) return Promise.resolve(cache.get(key));
	if (inflight.has(key)) return inflight.get(key);
	const p = fetch("/api/preview?url=" + encodeURIComponent(url) + "&net=" + encodeURIComponent(net || ""))
		.then((r) => (r.ok ? r.json() : null))
		.catch(() => null)
		.then((data) => {
			cache.set(key, data, data === null ? FAIL_TTL : undefined);
			inflight.delete(key);
			return data;
		});
	inflight.set(key, p);
	return p;
}

function thumbSrc(url, net) {
	return "/api/thumb?url=" + encodeURIComponent(url) + "&net=" + encodeURIComponent(net || "");
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
			fetchPreview(url, net).then((d) => alive && setData(d));
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
			{data.image && <img class="preview-card-img" src={thumbSrc(data.image, net)} alt="" loading="lazy" />}
			<div class="preview-card-body">
				<div class="preview-card-site">{data.site_name || hostOf(url)}</div>
				{data.title && <div class="preview-card-title">{data.title}</div>}
				{data.description && <div class="preview-card-desc">{data.description}</div>}
			</div>
		</a>
	);
}

function ImageThumb({ url, net }) {
	const [failed, setFailed] = useState(false);
	// A failure belongs to one url; reset when the component is reused.
	useEffect(() => setFailed(false), [url]);
	if (failed) return null;
	return (
		<a class="preview-thumb" href={url} target="_blank" rel="noopener noreferrer">
			<img src={thumbSrc(url, net)} alt="" loading="lazy" onError={() => setFailed(true)} />
		</a>
	);
}
