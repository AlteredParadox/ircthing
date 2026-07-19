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
	maxHTMLBytes    = 512 * 1024
	previewMaxTitle = 300
	previewMaxDesc  = 500
	// maxImageURL matches the /api/thumb URL limit; a longer og:image is
	// useless (the proxy rejects it) and dangerous to cache.
	maxImageURL = 2048
)

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
	if !s.acquireMedia(r.Context()) {
		http.Error(w, "busy, retry later", http.StatusServiceUnavailable)
		return
	}
	defer s.releaseMedia()
	// Re-check after the (up to 5s) slot wait: previews may have been disabled
	// while this request was parked, and it must not fetch after that.
	if !s.previewsEnabled() {
		http.Error(w, "previews disabled", http.StatusForbidden)
		return
	}
	// Resolve egress AFTER the wait: a proxy/tunnel may have been configured on
	// the network while we were parked, and a stale (direct) resolution would
	// leak the IP. Fail closed if we can't confirm a direct/proxy/tunnel egress.
	f := s.htmlFetcherForNetwork(r.Context(), net)
	if f == nil {
		http.Error(w, "preview unavailable", http.StatusBadGateway)
		return
	}
	ct, body, err := f.get(r.Context(), target)
	if err != nil {
		http.Error(w, "preview unavailable", http.StatusBadGateway)
		return
	}

	pv := PreviewData{URL: target, Kind: "link"}
	if isImageType(ct) {
		pv.Kind = "image"
		pv.Image = target
	} else {
		extractMeta(string(body), target, &pv)
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
// value wins), unescaped.
func parseHeadMeta(doc string) map[string]string {
	meta := map[string]string{}
	for _, tag := range reMeta.FindAllString(doc, -1) {
		var key, content string
		for _, a := range reAttr.FindAllStringSubmatch(tag, -1) {
			name := strings.ToLower(a[1])
			val := firstNonEmpty(a[3], a[4], a[5])
			switch name {
			case "property", "name":
				key = strings.ToLower(val)
			case "content":
				content = val
			}
		}
		if key != "" && content != "" {
			if _, seen := meta[key]; !seen {
				meta[key] = html.UnescapeString(content)
			}
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

func isImageType(contentType string) bool {
	ct, _, _ := strings.Cut(contentType, ";")
	switch strings.TrimSpace(strings.ToLower(ct)) {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/avif", "image/x-icon":
		return true
	}
	return false
}
