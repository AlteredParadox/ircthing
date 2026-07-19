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

package api

import (
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Link previews: fetch a URL server-side and return a compact card
// (title/description/image) extracted from its OpenGraph/Twitter/<title>
// metadata, or, when the URL is itself an image, a pointer for the client
// to load a thumbnail. Extraction is a bounded best-effort scan — a
// preview that fails just isn't shown.

const (
	// maxHTMLBytes caps the HTML body fetched for a link preview. It must clear
	// the head of real-world pages that front-load large inline JSON before their
	// og/title tags — YouTube's watch page, for one, puts og:title ~660 KiB in.
	// The HTML fetcher truncates (not rejects) at this cap, and extractMeta only
	// scans to </head>, so oversizing costs at most one prefix per preview slot.
	maxHTMLBytes    = 1024 * 1024
	previewMaxTitle = 300
	previewMaxDesc  = 500
	// maxImageURL matches the /api/thumb URL limit; a longer og:image is
	// useless (the proxy rejects it) and dangerous to cache.
	maxImageURL = 2048
	// maxMetaTags bounds how many <meta> tags parseHeadMeta scans — real pages
	// carry a few dozen; a hostile one could carry tens of thousands.
	maxMetaTags = 256
	// maxMetaTagBytes bounds ONE matched <meta …> tag. Without it, a tag
	// missing its closing '>' matches up to the whole 1 MiB body, and a
	// 1 MiB tag stuffed with ~250k tiny attributes made FindAllStringSubmatch
	// materialize six strings plus a slice per attribute — ~30 MiB for one
	// request, ×previewSlots against MemoryMax. Real head metadata is well
	// under this; an over-limit tag is skipped, not truncated.
	maxMetaTagBytes = 8 * 1024
	// maxMetaAttrs bounds the attributes parsed per tag. A real <meta> tag
	// carries 2–4 (property/name, content, maybe charset); this cap only
	// bites hostile attribute stuffing.
	maxMetaAttrs = 32
)

// wantedMeta is the exact set of head-metadata keys extractMeta consumes;
// parseHeadMeta retains only these, so an attacker's unbounded distinct keys
// can't grow the map.
var wantedMeta = map[string]bool{
	"og:title": true, "twitter:title": true,
	"og:description": true, "twitter:description": true, "description": true,
	"og:site_name": true,
	"og:image":     true, "og:image:url": true, "twitter:image": true,
}

// PreviewData is the /api/preview JSON response.
type PreviewData struct {
	URL         string `json:"url"`
	Kind        string `json:"kind"` // "link" or "image"
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"` // absolute; load via /api/thumb
	SiteName    string `json:"site_name,omitempty"`
}

// mediaRequest reads the {url, net} POST body for the media endpoints. The
// target is sent in the body, not a query string, so a URL carrying userinfo
// or signed parameters never lands in a reverse-proxy access log.
func mediaRequest(w http.ResponseWriter, r *http.Request) (target, net string, ok bool) {
	var body struct {
		URL string `json:"url"`
		Net string `json:"net"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return "", "", false
	}
	if len(body.URL) == 0 || len(body.URL) > 2048 {
		http.Error(w, "bad url", http.StatusBadRequest)
		return "", "", false
	}
	return body.URL, body.Net, true
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if !s.previewsEnabled() {
		http.Error(w, "previews disabled", http.StatusForbidden)
		return
	}
	target, net, ok := mediaRequest(w, r)
	if !ok {
		return
	}
	// The link's network selects the proxy, so cache per (network, url): the
	// same URL in a Tor'd network and a direct one must fetch independently.
	ck := net + "\x00" + target
	if pv, ok := s.previewCache.get(ck); ok {
		writeJSON(w, pv)
		return
	}
	if !s.acquirePreview(r.Context()) {
		http.Error(w, "busy, retry later", http.StatusServiceUnavailable)
		return
	}
	defer s.releasePreview()
	// Re-check the cache: another request may have populated it during the wait.
	// This dedups requests that arrived while an earlier one held a slot, but NOT
	// a simultaneous burst: with previewSlots>1, N identical first-time requests
	// (e.g. the same link open in N browser tabs) can all take a free slot, all
	// miss here, and all fetch. Accepted residual — for a SUCCESSFUL fetch it is
	// bounded by the slot count (<=previewSlots duplicate fetches, once, then the
	// cache serves the rest). FAILURES are not server-cached, so every waiting tab
	// can still fetch and retry after its slot wait; that stays acceptable for a
	// single-user deployment because the client bounds its own retries per
	// (network, URL) — see web/src/preview.jsx. A single-slot design (like
	// thumbnails) would serialize it but at the cost of the per-tab responsiveness
	// the extra slots buy; full singleflight isn't worth the request-path locking.
	if pv, ok := s.previewCache.get(ck); ok {
		writeJSON(w, pv)
		return
	}
	// Re-check after the (up to 5s) slot wait: previews may have been disabled
	// while this request was parked, and it must not fetch after that.
	if !s.previewsEnabled() {
		http.Error(w, "previews disabled", http.StatusForbidden)
		return
	}
	// Resolve egress AFTER the wait: a proxy/tunnel may have been configured on
	// the network while we were parked, and a stale (direct) resolution would
	// leak the IP. A nil fetcher means the egress is UNRESOLVABLE (unknown/deleted
	// network or unparseable proxy) — fail closed, 502.
	f := s.htmlFetcherForNetwork(r.Context(), net)
	if f == nil {
		http.Error(w, "preview unavailable", http.StatusBadGateway)
		return
	}
	// Classify the fetch error: a TRANSIENT failure (WireGuard tunnel still coming
	// up, upstream 5xx) → 503 so the client retries a few times; a PERMANENT one
	// (bad/blocked URL, over-size body, upstream 4xx) → 502 so it caches the
	// failure and does NOT retry — retrying a dead link is four tracking hits and,
	// for an over-size image, ~40 MiB re-downloaded to the same end. Fail closed
	// either way: no direct fetch.
	ct, finalURL, body, err := f.get(r.Context(), target)
	if err != nil {
		if fetchErrorRetryable(err) {
			http.Error(w, "preview fetch failed", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "preview unavailable", http.StatusBadGateway)
		}
		return
	}

	pv := PreviewData{URL: target, Kind: "link"}
	if isImageType(ct) {
		pv.Kind = "image"
		pv.Image = target
	} else {
		// Resolve relative og:image against the FINAL (post-redirect) URL, or a
		// redirect would resolve them against the wrong origin/path.
		extractMeta(string(body), finalURL, &pv)
	}
	s.previewCache.put(ck, pv)
	writeJSON(w, pv)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=1800")
	_ = json.NewEncoder(w).Encode(v)
}

var (
	reHead    = regexp.MustCompile(`(?is)</head\s*>`)
	reMeta    = regexp.MustCompile(`(?is)<meta\s+[^>]*>`)
	reAttr    = regexp.MustCompile(`(?is)([a-z][a-z0-9:_-]*)\s*=\s*("([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	reTitleEl = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// extractMeta fills pv from the document's head metadata. Precedence for
// each field: OpenGraph, then Twitter card, then the plain HTML element.
func extractMeta(doc, pageURL string, pv *PreviewData) {
	if loc := reHead.FindStringIndex(doc); loc != nil {
		doc = doc[:loc[0]] // scanning the head is enough and bounds the work
	}
	meta := parseHeadMeta(doc)
	pv.Title = clip(firstNonEmpty(meta["og:title"], meta["twitter:title"], titleElement(doc)), previewMaxTitle)
	pv.Description = clip(firstNonEmpty(meta["og:description"], meta["twitter:description"], meta["description"]), previewMaxDesc)
	pv.SiteName = clip(meta["og:site_name"], previewMaxTitle)
	if img := firstNonEmpty(meta["og:image"], meta["og:image:url"], meta["twitter:image"]); img != "" {
		// Drop an over-length image URL rather than cache it: /api/thumb
		// rejects URLs past this bound anyway, and the raw og:image can
		// be ~500 KB (bounded only by the HTML fetch cap), which would
		// bloat the preview cache past the RSS target.
		if abs := resolveURL(pageURL, img); len(abs) <= maxImageURL {
			pv.Image = abs
		}
	}
}

// parseHeadMeta collects <meta property|name=… content=…> pairs (first
// value wins), unescaped. Only the handful of keys extractMeta reads are
// retained (wantedMeta), and the work is bounded on every axis a hostile
// page controls: tag count (maxMetaTags), bytes per tag (maxMetaTagBytes —
// an unterminated <meta can otherwise match up to the whole 1 MiB body),
// and attributes parsed per tag (maxMetaAttrs — each submatch materializes
// six strings plus a slice, so unbounded attribute stuffing was tens of MiB
// per request). Worst case is now maxMetaTags × maxMetaAttrs tiny strings
// over ≤8 KiB tags — a few MB transient, far under the decoder budget.
func parseHeadMeta(doc string) map[string]string {
	meta := map[string]string{}
	for _, tag := range reMeta.FindAllString(doc, maxMetaTags) {
		if len(tag) > maxMetaTagBytes {
			continue // hostile or degenerate tag; no real metadata is this big
		}
		var key, content string
		for _, a := range reAttr.FindAllStringSubmatch(tag, maxMetaAttrs) {
			name := strings.ToLower(a[1])
			val := firstNonEmpty(a[3], a[4], a[5])
			switch name {
			case "property", "name":
				key = strings.ToLower(val)
			case "content":
				content = val
			}
		}
		if content == "" || !wantedMeta[key] {
			continue // ignore keys extractMeta never reads
		}
		if _, seen := meta[key]; !seen {
			meta[key] = html.UnescapeString(content)
		}
	}
	return meta
}

func titleElement(doc string) string {
	if m := reTitleEl.FindStringSubmatch(doc); m != nil {
		return html.UnescapeString(m[1])
	}
	return ""
}

// resolveURL turns a possibly-relative asset reference into an absolute
// URL against the page it came from; returns "" if it can't.
func resolveURL(pageURL, ref string) string {
	base, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ""
	}
	abs := base.ResolveReference(r)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return ""
	}
	return abs.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func clip(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ") // collapse whitespace/newlines
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…"
}

// isImageType reports whether contentType is an image format the thumbnailer can
// actually DECODE (see thumb.go's registered decoders). It must not claim formats
// we can't decode — a claimed-but-undecodable type routes the client to /api/thumb,
// which 415s, leaving a blank card instead of a link preview. avif and x-icon have
// no pure-Go decoder in our dependency set, so they are deliberately absent.
// image/webp is claimed even though LOSSLESS (VP8L) bodies are refused at the
// decode gate (see webpUsesVP8L): the content type doesn't distinguish them, and
// a blank card for lossless WebP is the accepted cost of that restriction.
func isImageType(contentType string) bool {
	ct, _, _ := strings.Cut(contentType, ";")
	switch strings.TrimSpace(strings.ToLower(ct)) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}
