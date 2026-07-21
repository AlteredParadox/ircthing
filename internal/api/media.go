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
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Inline audio/video playback: the browser's <audio>/<video> elements stream
// remote media THROUGH the server, matching the media plane's privacy rules —
// the browser never contacts a remote origin, and the fetch uses the source
// network's egress (direct / proxy / WireGuard tunnel) so no IP leaks.
//
// Media elements can only GET, and target URLs must never appear in a
// reverse-proxy access log's query string (the same hardening that made
// preview/thumb POST-only). So playback is two steps:
//
//  1. POST /api/media/token {url, net} — authenticated + sameSiteOnly, like
//     /api/preview — returns a short-lived opaque token binding (url, net, exp).
//  2. GET /api/media/stream?t=<token> — session cookie still required (the
//     token is NOT a bearer capability on its own) — verifies the token and
//     streams the target through the network's egress with Range support.
//
// The token is SEALED with AES-256-GCM under a per-process random key rather
// than the plainer base64(payload)||HMAC shape: the token travels in a query
// string, and a merely MAC'd payload would put the base64 of the target URL
// straight back into access logs — the exact leak the POST design exists to
// prevent. GCM gives integrity (tamper → open fails) AND confidentiality.
// The key is generated at startup and never persisted: tokens die on restart,
// which is fine — the client just mints a new one (a play click or an element
// error path re-POSTs /api/media/token).

// mediaTokenTTL is how long a minted stream token stays valid. Long enough to
// cover the gap between a card render and the user pressing play in normal
// use; the client re-mints on expiry-shaped element errors, so a longer life
// would only widen the replay window of a leaked token.
const mediaTokenTTL = 15 * time.Minute

// streamSlots bounds concurrent media streams. This is a SEPARATE semaphore
// from mediaSlots (=1) on purpose: mediaSlots is sized for thumbnail DECODE
// memory (~36 MiB peak per slot), while a stream holds only a fixed 32 KiB
// copy buffer — but holds it for minutes. A playing song must not park every
// thumbnail behind it (mediaSlots), and a thumbnail decode must not stall
// playback. Two slots ≈ 64 KiB of buffers; the third concurrent stream gets
// 429 immediately (never queued) so a player fails visibly instead of
// hanging on a silent wait.
const streamSlots = 2

// streamCopyBufBytes is the fixed per-stream copy buffer. The body is never
// accumulated: bytes move origin → buffer → client, so a feature-length video
// costs the same memory as a jingle.
const streamCopyBufBytes = 32 * 1024

// Header names used on both sides of the stream relay (canonical form, as
// http.Header stores them).
const (
	headerContentType  = "Content-Type"
	headerContentRange = "Content-Range"
)

// msgStreamUnavailable is the 502 body for permanent stream refusals:
// unresolvable egress, non-retryable fetch failure, unforwardable origin
// status. Clients match on this exact text; keep it stable.
const msgStreamUnavailable = "stream unavailable"

// streamIdleTimeout bounds PROGRESS, not total duration: a long track is
// legitimate (no total deadline), but a hung origin (no bytes readable) or a
// stalled client (no bytes writable) must not pin one of the two stream slots
// forever. Reset on every successful read; also applied as a per-write
// deadline toward the client. Var so tests can shrink it.
var streamIdleTimeout = 60 * time.Second

// mediaToken is the sealed token payload. Short keys: the base64url of this
// JSON rides in every stream request's query string.
type mediaToken struct {
	URL string `json:"u"`
	Net string `json:"n"`
	Exp int64  `json:"e"` // unix seconds
}

// newMediaTokenAEAD builds the per-process AES-256-GCM sealer for stream
// tokens. Random key, never persisted — see the package comment above.
func newMediaTokenAEAD() (cipher.AEAD, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("api: media token key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealMediaToken encrypts tok under the server's per-process key. Output is
// base64url(nonce || ciphertext) — opaque in logs.
func (s *Server) sealMediaToken(tok mediaToken) (string, error) {
	plain, err := json.Marshal(tok)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.mediaTokenAEAD.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.mediaTokenAEAD.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// openMediaToken decrypts and validates a stream token. Any failure —
// garbage, truncation, tampering, a key from a previous process — returns an
// error; expiry is checked here too so callers have one gate.
func (s *Server) openMediaToken(token string) (mediaToken, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return mediaToken{}, errors.New("stream: undecodable token")
	}
	ns := s.mediaTokenAEAD.NonceSize()
	if len(raw) < ns {
		return mediaToken{}, errors.New("stream: token too short")
	}
	plain, err := s.mediaTokenAEAD.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return mediaToken{}, errors.New("stream: token rejected")
	}
	var tok mediaToken
	if err := json.Unmarshal(plain, &tok); err != nil {
		return mediaToken{}, errors.New("stream: token payload invalid")
	}
	if time.Now().Unix() >= tok.Exp {
		return mediaToken{}, errors.New("stream: token expired")
	}
	return tok, nil
}

// handleMediaToken mints a stream token for {url, net}. Same admission as
// /api/preview: authenticated, sameSiteOnly, POST body (URL never in a query
// string), refused while previews are disabled, and the URL passes the same
// static validation the fetch path enforces (scheme, host shape, literal
// non-public IPs). The dynamic policy — resolved-IP checks, redirect vetting,
// egress selection — is re-applied at stream time by the shared fetcher code;
// the token only proves "an authenticated same-origin session asked for this
// exact target recently".
func (s *Server) handleMediaToken(w http.ResponseWriter, r *http.Request) {
	if !s.previewsEnabled() {
		http.Error(w, "previews disabled", http.StatusForbidden)
		return
	}
	target, netName, ok := mediaRequest(w, r)
	if !ok {
		return
	}
	u, err := url.Parse(target)
	if err != nil || !validMediaURL(u) {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	// Literal non-public IP: refuse at mint time, matching hostAllowed. A
	// hostname is not resolved here — the stream fetch re-validates it at
	// dial time (direct) or defers DNS to the proxy/tunnel, exactly like
	// preview/thumb fetches.
	if ip := net.ParseIP(u.Hostname()); ip != nil && !isPublicIP(ip) {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	exp := time.Now().Add(mediaTokenTTL).Unix()
	token, err := s.sealMediaToken(mediaToken{URL: target, Net: netName, Exp: exp})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set(headerContentType, "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Token string `json:"token"`
		Exp   int64  `json:"exp"`
	}{token, exp})
}

// streamableType allowlists what the stream endpoint will forward: real
// audio/video types plus application/ogg (the registered type many servers
// still use for .ogg/.opus). Anything else — text/html above all — is refused
// with 502 and NO body: this endpoint must never become a generic
// authenticated GET proxy to arbitrary origins.
func streamableType(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/") ||
		mt == "application/ogg"
}

// forwardRangeHeader returns the client's Range header if it is a plausible
// bytes range to forward to the origin, else "". Only this ONE request header
// crosses to the origin (plus our own UA/Accept): cookies and credentials
// never do, in either direction.
func forwardRangeHeader(r *http.Request) string {
	rng := r.Header.Get("Range")
	if rng == "" || len(rng) > 256 || !strings.HasPrefix(rng, "bytes=") {
		return ""
	}
	return rng
}

// acquireStream takes a stream slot WITHOUT queueing: a player that can't
// stream now should fail visibly (429, retryable by pressing play again)
// rather than sit in a wait the user can't see.
func (s *Server) acquireStream() bool {
	select {
	case s.streamSem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseStream() { <-s.streamSem }

func (s *Server) handleMediaStream(w http.ResponseWriter, r *http.Request) {
	// requireAuth already ran: the session cookie is mandatory — the token
	// alone must not be a bearer capability (a leaked query string must not
	// let anyone else stream through this server).
	tok, err := s.openMediaToken(r.URL.Query().Get("t"))
	if err != nil {
		http.Error(w, "invalid token", http.StatusForbidden)
		return
	}
	// Enablement is re-checked at REQUEST time, like preview.go/thumb.go: a
	// token minted before the switch was flipped must not stream after it.
	if !s.previewsEnabled() {
		http.Error(w, "previews disabled", http.StatusForbidden)
		return
	}
	if mediaDebug {
		w.Header().Set(mediaIDHeader, mediaTargetID(tok.URL))
	}
	if !s.acquireStream() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many streams", http.StatusTooManyRequests)
		return
	}
	defer s.releaseStream()
	// (No enablement re-check after the acquire: unlike the preview/thumb
	// slots there is no parked wait — acquireStream is non-blocking.)

	// Resolve egress per the token's network, exactly like preview/thumb: nil
	// means it cannot be determined safely (deleted network, unparseable
	// proxy) — fail closed, never a direct fetch.
	f := s.streamFetcherForNetwork(r.Context(), tok.Net)
	if f == nil {
		http.Error(w, msgStreamUnavailable, http.StatusBadGateway)
		return
	}

	// ctx is canceled by the idle watchdog (origin made no progress), by the
	// client going away (r.Context()), or by revocation of the session it
	// rides on; any of these tears the origin fetch down so neither a hung
	// origin nor a dead session can pin this stream slot.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Register the cancel under the session token, exactly like the WebSocket
	// path: requireAuth checked the session ONCE, and every token-deletion
	// path (logout, password rotation, lazy expiry, capacity eviction)
	// funnels through deleteTokenLocked, which cancels registered streams
	// alongside sockets — so a stream never outlives its session.
	// Registration re-validates the token under the registry lock; a
	// revocation that landed since requireAuth already ran its cancel sweep,
	// so registering blind would leave this stream relaying until the track
	// ends.
	var token string
	if ck, err := r.Cookie(s.cookieName()); err == nil {
		token = ck.Value
	}
	unregister, live := s.registerStreamCancel(token, cancel)
	if !live {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	defer unregister()
	watchdog := time.AfterFunc(streamIdleTimeout, cancel)
	defer watchdog.Stop()

	start := time.Now()
	resp, err := f.stream(ctx, tok.URL, forwardRangeHeader(r))
	if err != nil {
		streamFetchFailed(w, tok.URL, start, err)
		return
	}
	defer resp.Body.Close()

	if !forwardStreamStatus(w, resp, tok.URL, start) {
		return
	}

	ct := resp.Header.Get(headerContentType)
	if !streamableType(ct) {
		// Refused type: 502, NO body bytes — text/html and friends must never
		// relay through here.
		logMedia("stream", tok.URL, start, mediaLogResult{event: "reject", class: "not_media", httpStatus: 502})
		http.Error(w, "unsupported media type", http.StatusBadGateway)
		return
	}

	writeStreamHeaders(w, resp, ct)
	relayStreamBody(w, resp, watchdog, tok.URL, start)
}

// streamFetchFailed answers a failed origin fetch: transient failures map to
// 503 so the client retries, permanent ones to 502 so it caches the failure.
func streamFetchFailed(w http.ResponseWriter, url string, start time.Time, err error) {
	if fetchErrorRetryable(err) {
		logMedia("stream", url, start, mediaLogResult{event: "fetch_error", class: mediaErrorClass(err), retryable: true, httpStatus: 503})
		http.Error(w, "stream fetch failed", http.StatusServiceUnavailable)
	} else {
		logMedia("stream", url, start, mediaLogResult{event: "fetch_error", class: mediaErrorClass(err), httpStatus: 502})
		http.Error(w, msgStreamUnavailable, http.StatusBadGateway)
	}
}

// forwardStreamStatus vets the origin's status line. Only the three statuses
// a media element understands are forwarded; it returns true when the caller
// should proceed to the type gate and body copy (200/206), false when the
// response has been fully answered here (416 or a refused status).
func forwardStreamStatus(w http.ResponseWriter, resp *http.Response, url string, start time.Time) bool {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		return true
	case http.StatusRequestedRangeNotSatisfiable:
		// 416 carries no media body worth forwarding (origins put error HTML
		// there); forward the status and Content-Range so the element can
		// re-request sanely.
		if cr := resp.Header.Get(headerContentRange); cr != "" {
			w.Header().Set(headerContentRange, cr)
		}
		logMedia("stream", url, start, mediaLogResult{event: "ok", class: "range_unsatisfiable", httpStatus: 416})
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return false
	default:
		logMedia("stream", url, start, mediaLogResult{event: "reject", class: mediaErrorClass(&upstreamStatusError{resp.StatusCode}), httpStatus: 502})
		http.Error(w, msgStreamUnavailable, http.StatusBadGateway)
		return false
	}
}

// writeStreamHeaders forwards ONLY the entity headers a player needs. Never
// Set-Cookie or anything else origin-controlled.
func writeStreamHeaders(w http.ResponseWriter, resp *http.Response, ct string) {
	h := w.Header()
	h.Set(headerContentType, ct)
	for _, k := range []string{"Content-Length", headerContentRange, "Accept-Ranges"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set("X-Content-Type-Options", "nosniff")
	// no-store: media must not persist in the browser's HTTP cache — the same
	// 30-minute redaction bound that governs thumbnails has no server copy to
	// expire here, so don't create a client one.
	h.Set("Cache-Control", "private, no-store")
	w.WriteHeader(resp.StatusCode)
}

// relayStreamBody is the fixed-buffer relay: at no point does the body
// accumulate. Each origin read re-arms the idle watchdog (progress, not total
// time); each client write gets its own deadline so a stalled browser can't
// pin the slot either.
func relayStreamBody(w http.ResponseWriter, resp *http.Response, watchdog *time.Timer, url string, start time.Time) {
	rc := http.NewResponseController(w)
	// On exit, grant the server's FINAL buffered flush (which happens after
	// the stream handler returns) its own idle budget: the last in-loop write
	// deadline may already be in the past by then, and an expired deadline
	// would abort the connection mid-tail — the client saw truncated streams
	// exactly this way in tests.
	defer func() { _ = rc.SetWriteDeadline(time.Now().Add(streamIdleTimeout)) }()
	buf := make([]byte, streamCopyBufBytes)
	var copied int
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			// Re-arm the idle watchdog only AFTER a successful origin read:
			// re-arming before it would make one idle budget span
			// read + client write + next loop, spuriously canceling a
			// slow-but-progressing transfer. Nothing is lost on the
			// stalled-client side — the per-write deadline below bounds that
			// case on its own.
			watchdog.Reset(streamIdleTimeout)
			_ = rc.SetWriteDeadline(time.Now().Add(streamIdleTimeout))
			if _, werr := w.Write(buf[:n]); werr != nil {
				logMedia("stream", url, start, mediaLogResult{event: "client_gone", class: "network_io", outputBytes: copied})
				return
			}
			copied += n
		}
		if rerr != nil {
			// io.EOF is the clean end; anything else is a mid-body origin
			// failure — headers are long gone, so the log line is the only
			// signal.
			cls := "eof"
			if !errors.Is(rerr, io.EOF) {
				cls = mediaErrorClass(rerr)
			}
			logMedia("stream", url, start, mediaLogResult{event: "ok", class: cls, httpStatus: resp.StatusCode, outputBytes: copied})
			return
		}
	}
}

// stream fetches rawURL for relaying, forwarding rangeHdr (if any) to the
// origin and returning the raw *http.Response for the caller to stream. It
// applies the SAME admission policy as get(): URL validation, the literal
// non-public-IP refusal on proxied/tunneled paths (the direct path's dialer
// Control hook re-validates resolved IPs), and checkRedirect on every hop.
// Unlike get() it does NOT read the body — the caller relays it through a
// fixed buffer — and its client has no overall timeout (long tracks are
// legitimate; the caller enforces idle-progress instead).
func (f *fetcher) stream(ctx context.Context, rawURL, rangeHdr string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !validMediaURL(u) {
		return nil, errBadURL
	}
	if f.proxied && !f.hostAllowed(u.Hostname()) {
		return nil, errBlocked
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", proxyUserAgent)
	req.Header.Set("Accept", "*/*")
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	return f.client.Do(req)
}

// tagDialPhase wraps a dial func so its failures classify as dialPhaseError
// (transient), matching withWireBudget's tagging — stream fetchers skip the
// wire budget (see newStreamFetcher) but must keep the error taxonomy.
func tagDialPhase(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, &dialPhaseError{err}
		}
		return c, nil
	}
}

// streamTransport builds the shared transport shape for stream fetchers:
// bounded connect/TLS/first-byte phases, but no overall deadline and NO wire
// budget — a stream's payload is unbounded by design (the user is playing
// it), memory is bounded by the relay buffer, and the handler's idle-progress
// watchdog bounds a stalled connection. The absent wire budget leaves a real
// gap the buffered fetchers don't have: TLS strips record padding BELOW
// Response.Body, so a hostile origin can pad its records into ingress bytes
// that never reach the client — ingress amplification that body-level
// accounting cannot see. Accepted posture (2026-07-21): the exposure is
// bounded by the 2-slot cap, the idle watchdog, and a stream existing only
// because the user pressed play on that link; a below-TLS wire meter was
// considered and rejected as disproportionate.
func streamTransport(dial func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		Proxy:       nil, // never honor $HTTP_PROXY implicitly
		DialContext: tagDialPhase(dial),
		// Determinism on the no-Range path: without this the transport adds
		// Accept-Encoding: gzip (only when no Range header is set) and would
		// transparently decompress, making the relayed bytes diverge from the
		// origin's Content-Length. Zero UX cost — no real origin gzips a/v.
		DisableCompression:     true,
		TLSHandshakeTimeout:    8 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second, // time-to-first-byte bound
		MaxResponseHeaderBytes: 64 << 10,
		DisableKeepAlives:      true,
		MaxIdleConns:           0,
	}
}

// newStreamFetcher builds a stream fetcher for direct or proxied egress,
// mirroring newFetcher's SSRF posture (direct: connect-time resolved-IP
// checks via the dialer Control hook; proxied: literal-IP refusal with DNS
// deferred to the proxy).
func newStreamFetcher(proxy *url.URL) *fetcher {
	f := &fetcher{allowIP: isPublicIP}
	f.client = &http.Client{
		Transport:     streamTransport(f.dialContext(proxy)),
		CheckRedirect: f.checkRedirect,
	}
	return f
}

// newTunnelStreamFetcher is the WireGuard-tunnel variant, mirroring
// newTunnelFetcher: the tunnel owns egress and DNS, dial fails closed when it
// is down.
func newTunnelStreamFetcher(dial func(ctx context.Context, addr string) (net.Conn, error)) *fetcher {
	f := &fetcher{allowIP: isPublicIP, proxied: true}
	f.client = &http.Client{
		Transport: streamTransport(func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dial(ctx, addr)
		}),
		CheckRedirect: f.checkRedirect,
	}
	return f
}

// streamFetcherFor / streamFetcherForNetwork mirror the html/image fetcher
// resolution: per-proxy and per-tunnel-network pools, nil = FAIL CLOSED.
func (s *Server) streamFetcherFor(proxy *url.URL) *fetcher {
	key := proxyString(proxy)
	s.mediaMu.RLock()
	f := s.streamByProxy[key]
	s.mediaMu.RUnlock()
	if f != nil {
		return f
	}
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	if f = s.streamByProxy[key]; f == nil {
		if len(s.streamByProxy) >= maxProxyFetchers {
			clear(s.streamByProxy)
		}
		f = newStreamFetcher(proxy)
		s.streamByProxy[key] = f
	}
	return f
}

func (s *Server) streamFetcherForNetwork(ctx context.Context, network string) *fetcher {
	e := s.egressForNetwork(ctx, network)
	if !e.ok {
		return nil
	}
	if e.tunnel {
		s.mediaMu.RLock()
		f := s.tunnelStreamByNet[e.network]
		s.mediaMu.RUnlock()
		if f != nil {
			return f
		}
		s.mediaMu.Lock()
		defer s.mediaMu.Unlock()
		if f = s.tunnelStreamByNet[e.network]; f == nil {
			if len(s.tunnelStreamByNet) >= maxProxyFetchers {
				clear(s.tunnelStreamByNet)
			}
			name := e.network
			f = newTunnelStreamFetcher(func(ctx context.Context, addr string) (net.Conn, error) {
				return s.hub.NetworkTunnelDial(ctx, name, addr)
			})
			s.tunnelStreamByNet[name] = f
		}
		return f
	}
	return s.streamFetcherFor(e.proxy)
}
