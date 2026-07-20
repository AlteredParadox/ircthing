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
// The media egress is not a global setting: preview/thumbnail fetches use the
// egress of the network the link came from (egressForNetwork), so they
// automatically inherit that network's anonymity posture — a link in a Tor'd
// network is previewed over Tor, one in a WireGuard network over the tunnel,
// one in a direct network goes direct.
const previewsKey = "previews_enabled"

// loadPreviews resolves the effective previews switch: the stored value if
// present, the config-file default when the key is unset. A genuine store READ
// ERROR fails CLOSED (previews off) rather than falling through to a config
// default that may be on — a persisted "off" must not be silently re-enabled.
// (Setting returns "" with nil err for an absent key, so err != nil is a real
// error, not "unset".)
func loadPreviews(ctx context.Context, st *store.Store, cfg Config) bool {
	v, err := st.Setting(ctx, previewsKey)
	if err != nil {
		return false // fail closed on a read error
	}
	if v != "" {
		return v == "1"
	}
	return cfg.PreviewsDefault
}

// maxRetentionDays bounds the runtime retention setting, matching the config
// validator: past ~106752 days the time.Duration cutoff overflows into the
// future and would delete all history.
const maxRetentionDays = 36500

// maxRetentionMessages bounds retention_max_messages. Anything past 2^31-1
// would round-trip through the frontend's 32-bit coercion as a negative
// (breaking later saves); a billion messages is already beyond any real
// SQLite deployment here.
const maxRetentionMessages = 1_000_000_000

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

// configPatch is a PUT /api/config body. Pointer fields so a PUT can update
// just one setting (the previews toggle and the retention inputs save
// independently) without clobbering others.
type configPatch struct {
	Previews             *bool `json:"previews"`
	RetentionDays        *int  `json:"retention_days"`
	RetentionMaxMessages *int  `json:"retention_max_messages"`
	SessionTTLDays       *int  `json:"session_ttl_days"`
}

func (p *configPatch) setsRetention() bool {
	return p.RetentionDays != nil || p.RetentionMaxMessages != nil
}

const storeConfigFailedMsg = "storing config failed"

func (s *Server) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var body configPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "malformed config", http.StatusBadRequest)
		return
	}
	// An empty patch is a client bug, not a save: JSON null/absent fields all
	// decode to nil pointers (e.g. a frontend serializing Infinity produces
	// null), and 204-ing it would let the client record an unpersisted value
	// as confirmed.
	//
	// A patch may touch at most ONE logical group: previews, retention (both
	// dimensions count as one), or session TTL. applyConfigPatch persists the
	// groups sequentially and is NOT a cross-store transaction, so a mixed
	// patch that fails partway could leave one group changed (previews enabled,
	// or pruning rescheduled) and return 500. The frontend always sends one
	// group per request, so rejecting multi-group patches costs nothing and
	// keeps every accepted request single-group — hence trivially atomic.
	groups := 0
	if body.Previews != nil {
		groups++
	}
	if body.setsRetention() {
		groups++
	}
	if body.SessionTTLDays != nil {
		groups++
	}
	if groups == 0 {
		http.Error(w, "empty config patch", http.StatusBadRequest)
		return
	}
	if groups > 1 {
		http.Error(w, "one setting group per request", http.StatusBadRequest)
		return
	}

	// Serialize the whole read-validate-apply. Retention is a read-modify-write
	// (read the current pair, overlay the changed dimension); without this two
	// concurrent PUTs each setting a different dimension would read the same base
	// and last-writer-wins would drop one. Holding it across SetRetention also
	// keeps that call's persist-then-install from interleaving with another PUT.
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	// Re-validate the session at commit time, not just at the router: a PUT
	// dispatched before a logout can reach here after it, and committing would
	// overwrite a setting the NEXT session (or another device) owns.
	//
	// This recheck narrows the window to the gap between authed() returning and
	// the store write below; a request that has passed this final admission may
	// still complete after a concurrent logout returns. That is an ACCEPTED
	// semantic, not a claim the settings are trivial — these are meaningful (a
	// shorter retention prunes history; previews affect privacy; session TTL is
	// security-relevant). Strict "nothing commits after logout returns" is
	// achievable by holding s.mu (which every revocation path already takes)
	// across an inlined token check and applyConfigPatch — no new lock, no
	// reverse ordering — at the cost of serializing config commits behind that
	// hot lock across a store write. For a single-user deployment the accepted
	// semantic is the proportionate choice; revisit if this ever serves
	// multiple distinct principals. (GPT audit #5, rechecked 2026-07-20.)
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rDays, rMax, badField := s.validateConfigPatch(&body)
	if badField != "" {
		http.Error(w, badField, http.StatusBadRequest)
		return
	}
	if err := s.applyConfigPatch(r.Context(), &body, rDays, rMax); err != nil {
		http.Error(w, storeConfigFailedMsg, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateConfigPatch checks EVERY provided field before anything is applied,
// so a bad value in one (e.g. retention_days:-1) can't leave an earlier one
// (previews) already changed and then return 400. It resolves the effective
// retention pair (current values overlaid with the patch); badField is the
// 400 message, "" when valid. Caller holds settingsMu.
func (s *Server) validateConfigPatch(p *configPatch) (rDays, rMax int, badField string) {
	if p.setsRetention() {
		rDays, rMax = s.hub.Store().Retention()
		if p.RetentionDays != nil {
			rDays = *p.RetentionDays
		}
		if p.RetentionMaxMessages != nil {
			rMax = *p.RetentionMaxMessages
		}
		if rDays < 0 || rDays > maxRetentionDays || rMax < 0 || rMax > maxRetentionMessages {
			return 0, 0, "retention out of range"
		}
	}
	if p.SessionTTLDays != nil && (*p.SessionTTLDays < 1 || *p.SessionTTLDays > maxRetentionDays) {
		return 0, 0, "session_ttl_days out of range"
	}
	return rDays, rMax, ""
}

// applyConfigPatch persists each provided setting. Caller holds settingsMu
// and has validated the patch.
func (s *Server) applyConfigPatch(ctx context.Context, p *configPatch, rDays, rMax int) error {
	if p.Previews != nil {
		if err := s.applyPreviews(ctx, *p.Previews); err != nil {
			return err
		}
	}
	if p.setsRetention() {
		if err := s.hub.Store().SetRetention(ctx, rDays, rMax); err != nil {
			return err
		}
	}
	if p.SessionTTLDays != nil {
		if err := s.hub.Store().SetSetting(ctx, sessionTTLKey, strconv.Itoa(*p.SessionTTLDays)); err != nil {
			return err
		}
		s.sessionTTL.Store(int64(time.Duration(*p.SessionTTLDays) * 24 * time.Hour))
	}
	return nil
}

// applyPreviews persists and updates the in-memory previews flag under one
// lock, so two concurrent PUTs cannot interleave into disagreeing
// persisted/live states.
func (s *Server) applyPreviews(ctx context.Context, on bool) error {
	val := "0"
	if on {
		val = "1"
	}
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	err := s.hub.Store().SetSetting(ctx, previewsKey, val)
	if err == nil {
		s.previewsOn = on
	}
	return err
}

func proxyString(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.String()
}

// mediaEgress is how a media fetch for a network must leave the box, so a
// link's preview fetch matches the network's own egress and never leaks its
// real IP. Exactly one of direct/proxy/tunnel is set when ok is true.
type mediaEgress struct {
	proxy   *url.URL // via a SOCKS5/HTTP proxy (nil with ok+!tunnel => direct)
	tunnel  bool     // via the network's in-process WireGuard tunnel
	network string   // the network name, when tunnel
	ok      bool     // false => cannot determine safely => fail closed (502)
}

// egressForNetwork resolves how a media fetch for network `name` must egress:
//
//   - {ok}                 known, DIRECT access
//   - {proxy, ok}          known, via a valid SOCKS5/HTTP proxy
//   - {tunnel, network, ok} a WireGuard network — dial through its tunnel
//   - {} (ok=false)        cannot determine: no/unknown/deleted/renamed
//     network, store error, malformed config, or a proxy
//     that no longer parses
//
// The caller must FAIL CLOSED on ok==false: a direct fetch there would leak the
// server's egress IP for a link belonging to a proxied/tunneled network (e.g. a
// UI request racing a network delete/rename, or a transient store error).
func (s *Server) egressForNetwork(ctx context.Context, name string) mediaEgress {
	if name == "" {
		return mediaEgress{}
	}
	nc, found, err := s.hub.Store().NetworkConfig(ctx, name)
	if err != nil || !found {
		return mediaEgress{}
	}
	var cfg struct {
		Proxy     string          `json:"proxy"`
		WireGuard json.RawMessage `json:"wireguard"`
	}
	if json.Unmarshal([]byte(nc.Config), &cfg) != nil {
		return mediaEgress{}
	}
	// A WireGuard network egresses through its in-process userspace tunnel; the
	// media fetch dials through that same tunnel (NetworkTunnelDial), so the
	// preview shares the network's IP and in-tunnel DNS. If the tunnel is down
	// the tunnel fetcher fails closed — it never falls back to a direct dial.
	if len(cfg.WireGuard) > 0 && string(cfg.WireGuard) != "null" {
		return mediaEgress{tunnel: true, network: name, ok: true}
	}
	if cfg.Proxy == "" {
		return mediaEgress{ok: true} // known, direct
	}
	u, err := proxydial.Parse(cfg.Proxy)
	if err != nil {
		return mediaEgress{} // configured a proxy, but it no longer parses
	}
	return mediaEgress{proxy: u, ok: true}
}
