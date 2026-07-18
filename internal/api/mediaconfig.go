package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"ircthing/internal/proxydial"
	"ircthing/internal/store"
)

// The previews switch is runtime-editable from the UI and persisted here,
// so it can be toggled without a config edit and restart. Config.PreviewsDefault
// (resolved from the config-file disable_previews tri-state) is only the
// initial default, used until something is saved. That default is off
// (privacy-first) unless the config explicitly enables previews.
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
	return cfg.PreviewsDefault
}

// maxRetentionDays bounds the runtime retention setting, matching the config
// validator: past ~106752 days the time.Duration cutoff overflows into the
// future and would delete all history.
const maxRetentionDays = 36500

// sessionTTLKey stores the runtime session-cookie lifetime, in days.
const sessionTTLKey = "session_ttl_days"

// loadSessionTTL resolves the effective session lifetime: the settings-table
// value (runtime-set via the UI, in days) when present, else the config value
// (which New already defaulted to 30 days).
func loadSessionTTL(ctx context.Context, st *store.Store, cfg Config) time.Duration {
	if v, err := st.Setting(ctx, sessionTTLKey); err == nil && v != "" {
		if days, err := strconv.Atoi(v); err == nil && days > 0 {
			return time.Duration(days) * 24 * time.Hour
		}
	}
	return cfg.SessionTTL
}

func (s *Server) handleClientConfig(w http.ResponseWriter, r *http.Request) {
	days, maxPer := s.hub.Store().Retention()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Previews             bool `json:"previews"`
		RetentionDays        int  `json:"retention_days"`
		RetentionMaxMessages int  `json:"retention_max_messages"`
		SessionTTLDays       int  `json:"session_ttl_days"`
	}{
		Previews:             s.previewsEnabled(),
		RetentionDays:        days,
		RetentionMaxMessages: maxPer,
		SessionTTLDays:       int(s.sessionTTLDur() / (24 * time.Hour)),
	})
}

func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	// Pointer fields so a PUT can update just one setting (the previews toggle
	// and the retention inputs save independently) without clobbering others.
	var body struct {
		Previews             *bool `json:"previews"`
		RetentionDays        *int  `json:"retention_days"`
		RetentionMaxMessages *int  `json:"retention_max_messages"`
		SessionTTLDays       *int  `json:"session_ttl_days"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "malformed config", http.StatusBadRequest)
		return
	}

	// Serialize the whole read-validate-apply. Retention is a read-modify-write
	// (read the current pair, overlay the changed dimension); without this two
	// concurrent PUTs each setting a different dimension would read the same base
	// and last-writer-wins would drop one. Holding it across SetRetention also
	// keeps that call's persist-then-install from interleaving with another PUT.
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	// Validate EVERY provided field before applying ANY, so a bad value in one
	// (e.g. retention_days:-1) can't leave an earlier one (previews) already
	// changed and then return 400.
	setRetention := body.RetentionDays != nil || body.RetentionMaxMessages != nil
	var rDays, rMax int
	if setRetention {
		rDays, rMax = s.hub.Store().Retention()
		if body.RetentionDays != nil {
			rDays = *body.RetentionDays
		}
		if body.RetentionMaxMessages != nil {
			rMax = *body.RetentionMaxMessages
		}
		if rDays < 0 || rDays > maxRetentionDays || rMax < 0 {
			http.Error(w, "retention out of range", http.StatusBadRequest)
			return
		}
	}
	if body.SessionTTLDays != nil && (*body.SessionTTLDays < 1 || *body.SessionTTLDays > maxRetentionDays) {
		http.Error(w, "session_ttl_days out of range", http.StatusBadRequest)
		return
	}

	// All validated — apply.
	if body.Previews != nil {
		val := "0"
		if *body.Previews {
			val = "1"
		}
		// Persist and update the in-memory flag under one lock so two concurrent
		// PUTs cannot interleave into disagreeing persisted/live states.
		s.mediaMu.Lock()
		err := s.hub.Store().SetSetting(r.Context(), previewsKey, val)
		if err == nil {
			s.previewsOn = *body.Previews
		}
		s.mediaMu.Unlock()
		if err != nil {
			http.Error(w, "storing config failed", http.StatusInternalServerError)
			return
		}
	}
	if setRetention {
		if err := s.hub.Store().SetRetention(r.Context(), rDays, rMax); err != nil {
			http.Error(w, "storing config failed", http.StatusInternalServerError)
			return
		}
	}
	if body.SessionTTLDays != nil {
		if err := s.hub.Store().SetSetting(r.Context(), sessionTTLKey, strconv.Itoa(*body.SessionTTLDays)); err != nil {
			http.Error(w, "storing config failed", http.StatusInternalServerError)
			return
		}
		s.sessionTTL.Store(int64(time.Duration(*body.SessionTTLDays) * 24 * time.Hour))
	}
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
//                   network, store error, malformed stored config, a stored
//                   proxy that no longer parses, OR a WireGuard network (its
//                   egress is an in-process tunnel this path can't reach)
//
// The caller must FAIL CLOSED on ok==false: falling back to a direct fetch
// there would leak the server's egress IP for a link that belongs to a
// proxied or WireGuard network (e.g. a UI request racing a network
// delete/rename, or a transient store error). Only a network known to be
// direct permits a direct fetch.
func (s *Server) proxyForNetwork(ctx context.Context, name string) (*url.URL, bool) {
	if name == "" {
		return nil, false
	}
	nc, found, err := s.hub.Store().NetworkConfig(ctx, name)
	if err != nil || !found {
		return nil, false
	}
	var cfg struct {
		Proxy     string          `json:"proxy"`
		WireGuard json.RawMessage `json:"wireguard"`
	}
	if json.Unmarshal([]byte(nc.Config), &cfg) != nil {
		return nil, false
	}
	// A WireGuard network egresses through an in-process userspace tunnel that
	// the media path has no handle on (it lives in the per-network irc.Manager).
	// A direct fetch would leak the server's real IP and resolve the target on
	// the local resolver — exactly what the tunnel prevents — so fail closed
	// (the caller turns this into a 502) until media can share the tunnel.
	if len(cfg.WireGuard) > 0 && string(cfg.WireGuard) != "null" {
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
