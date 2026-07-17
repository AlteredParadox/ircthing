package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"ircthing/internal/proxydial"
	"ircthing/internal/store"
)

// The previews switch is runtime-editable from the UI and persisted here,
// so it can be toggled without a config edit and restart. The config-file
// disable_previews field is only the initial default, used until something
// is saved.
//
// The media *proxy* is not a global setting: preview/thumbnail fetches use
// the proxy of the network the link came from (proxyForNetwork), so they
// automatically inherit that network's anonymity posture — a link in a
// Tor'd network is previewed over Tor, one in a direct network goes direct.
const previewsKey = "previews_enabled"

// loadPreviews resolves the effective previews switch: the stored value if
// present, else the config-file default.
func loadPreviews(ctx context.Context, st *store.Store, cfg Config) bool {
	if v, err := st.Setting(ctx, previewsKey); err == nil && v != "" {
		return v == "1"
	}
	return !cfg.PreviewsDisabled
}

func (s *Server) handleClientConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Previews bool `json:"previews"`
	}{Previews: s.previewsEnabled()})
}

func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Previews bool `json:"previews"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "malformed config", http.StatusBadRequest)
		return
	}
	val := "0"
	if body.Previews {
		val = "1"
	}
	if err := s.hub.Store().SetSetting(r.Context(), previewsKey, val); err != nil {
		http.Error(w, "storing config failed", http.StatusInternalServerError)
		return
	}
	s.mediaMu.Lock()
	s.previewsOn = body.Previews
	s.mediaMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func proxyString(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.String()
}

// proxyForNetwork resolves the proxy a media fetch for network name must
// use. It returns (proxy, ok):
//
//   - (nil, true)   the network is known and configured for DIRECT access
//   - (u,   true)   the network is known with a valid proxy
//   - (nil, false)  cannot determine — no network, unknown/deleted/renamed
//                   network, store error, malformed stored config, or a
//                   stored proxy that no longer parses
//
// The caller must FAIL CLOSED on ok==false: falling back to a direct fetch
// there would leak the server's egress IP for a link that belongs to a
// proxied network (e.g. a UI request racing a network delete/rename, or a
// transient store error). Only a network known to be direct permits a
// direct fetch.
func (s *Server) proxyForNetwork(ctx context.Context, name string) (*url.URL, bool) {
	if name == "" {
		return nil, false
	}
	nc, found, err := s.hub.Store().NetworkConfig(ctx, name)
	if err != nil || !found {
		return nil, false
	}
	var cfg struct {
		Proxy string `json:"proxy"`
	}
	if json.Unmarshal([]byte(nc.Config), &cfg) != nil {
		return nil, false
	}
	if cfg.Proxy == "" {
		return nil, true // known, direct
	}
	u, err := proxydial.Parse(cfg.Proxy)
	if err != nil {
		return nil, false // configured a proxy, but it no longer parses
	}
	return u, true
}
