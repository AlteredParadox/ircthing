package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"ircthing/internal/proxydial"
	"ircthing/internal/store"
)

// The media proxy and previews switch are runtime-editable from the UI and
// persisted here, so they can change over time without a config edit and
// restart. The config-file fields are only the initial default, used until
// something is saved.
const mediaConfigKey = "media_config"

// mediaConfigJSON is the wire + stored shape. Proxy is the full URL
// (including any credentials) so the settings form can round-trip it;
// empty means fetch directly.
type mediaConfigJSON struct {
	Proxy    string `json:"proxy"`
	Previews bool   `json:"previews"`
}

// loadMediaConfig resolves the effective media config: the stored value if
// present and valid, else the config-file default.
func loadMediaConfig(ctx context.Context, st *store.Store, cfg Config) (*url.URL, bool) {
	if v, err := st.Setting(ctx, mediaConfigKey); err == nil && v != "" {
		var m mediaConfigJSON
		if json.Unmarshal([]byte(v), &m) == nil {
			var proxy *url.URL
			if m.Proxy != "" {
				// A stored value was validated when saved; ignore it if it no
				// longer parses rather than fail startup.
				if u, perr := proxydial.Parse(m.Proxy); perr == nil {
					proxy = u
				}
			}
			return proxy, m.Previews
		}
	}
	return cfg.MediaProxy, !cfg.PreviewsDisabled
}

func proxyString(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.String()
}

func (s *Server) handleGetMediaConfig(w http.ResponseWriter, r *http.Request) {
	s.mediaMu.RLock()
	out := mediaConfigJSON{Proxy: proxyString(s.mediaProxy), Previews: s.previewsOn}
	s.mediaMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleSetMediaConfig(w http.ResponseWriter, r *http.Request) {
	var m mediaConfigJSON
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&m); err != nil {
		http.Error(w, "malformed media config", http.StatusBadRequest)
		return
	}
	var proxy *url.URL
	if p := strings.TrimSpace(m.Proxy); p != "" {
		u, err := proxydial.Parse(p)
		if err != nil {
			http.Error(w, "invalid proxy: "+err.Error(), http.StatusBadRequest)
			return
		}
		proxy = u
	}
	// Persist the normalized form, then swap the live fetchers.
	blob, _ := json.Marshal(mediaConfigJSON{Proxy: proxyString(proxy), Previews: m.Previews})
	if err := s.hub.Store().SetSetting(r.Context(), mediaConfigKey, string(blob)); err != nil {
		http.Error(w, "storing media config failed", http.StatusInternalServerError)
		return
	}
	s.applyMediaConfig(proxy, m.Previews)
	w.WriteHeader(http.StatusNoContent)
}

// applyMediaConfig swaps the live proxy, previews switch, and fetchers. The
// fetchers are cheap http.Clients; rebuilding drops the old ones (in-flight
// requests keep the fetcher they captured before the swap).
func (s *Server) applyMediaConfig(proxy *url.URL, previews bool) {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	s.mediaProxy = proxy
	s.previewsOn = previews
	s.htmlFetcher = newFetcher(maxHTMLBytes, proxy)
	s.imageFetcher = newFetcher(maxImageBytes, proxy)
}
