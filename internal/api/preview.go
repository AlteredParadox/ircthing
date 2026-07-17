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

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if !s.previewsEnabled() {
		http.Error(w, "previews disabled", http.StatusForbidden)
		return
	}
	target := r.URL.Query().Get("url")
	if len(target) == 0 || len(target) > 2048 {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	// The link's network selects the proxy, so cache per (network, url): the
	// same URL in a Tor'd network and a direct one must fetch independently.
	net := r.URL.Query().Get("net")
	ck := net + "\x00" + target
	if pv, ok := s.previewCache.get(ck); ok {
		writeJSON(w, pv)
		return
	}
	// Fail closed: if we can't confirm the link's network is direct or has a
	// valid proxy, refuse rather than risk a direct fetch that leaks the IP.
	proxy, ok := s.proxyForNetwork(r.Context(), net)
	if !ok {
		http.Error(w, "preview unavailable", http.StatusBadGateway)
		return
	}

	if !s.acquireMedia(r.Context()) {
		http.Error(w, "busy, retry later", http.StatusServiceUnavailable)
		return
	}
	defer s.releaseMedia()
	ct, body, err := s.htmlFetcherFor(proxy).get(r.Context(), target)
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
