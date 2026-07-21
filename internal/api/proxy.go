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
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"ircthing/internal/proxydial"
)

// Server-side fetcher for the media proxy (link previews, image
// thumbnails). Browsers never fetch remote content directly — this is the
// single choke point. SSRF hardening differs by path:
//
//   - Direct fetches are fully hardened: the dialer re-validates the
//     *resolved* IP of every connection (and every redirect hop) against a
//     public-address policy at connect time, which defeats DNS rebinding
//     because the check is on the IP, not the hostname.
//   - Proxied fetches (through a network's proxy) can only refuse literal
//     non-public IP targets (hostAllowed). The proxy owns DNS, so a hostname
//     that resolves proxy-side to an internal/loopback/metadata address is
//     NOT caught here — such a request can reach whatever the proxy can
//     reach. This is inherent to preserving proxy-side DNS; the mitigation
//     is the proxy's own egress policy (Tor exits publicly and cannot reach
//     your LAN; a plain LAN SOCKS proxy can), or disabling previews for a
//     network whose proxy you don't trust to restrict destinations.

const proxyUserAgent = "ircthing-media-proxy/1.0 (+https://github.com/ircthing)"

// mediaDebug enables privacy-preserving per-fetch phase logging. It requires
// an explicit true value; in particular IRCTHING_DEBUG_MEDIA=0 stays off.
// Targets are represented only by a short one-way correlation ID, and neither
// URL components, page metadata, nor upstream-controlled error strings reach
// the log.
//
// mediaDebugURLs (IRCTHING_DEBUG_MEDIA_URLS) is the EXPLICITLY-UNSAFE step up:
// media debug log lines additionally carry the full target URL, for when the
// ID correlation is not enough. Setting it implies mediaDebug — the log lines
// it decorates must exist. main.go logs a startup warning while it is active,
// because target URLs (which may carry userinfo or signed parameters) then
// reach persistent logs.
var mediaDebug, mediaDebugURLs = mediaDebugFlags(
	os.Getenv("IRCTHING_DEBUG_MEDIA"), os.Getenv("IRCTHING_DEBUG_MEDIA_URLS"))

// mediaDebugFlags resolves the two media-debug env values: urls requires its
// own explicit true value, and debug is on when either is — _URLS without
// _MEDIA still turns the logging on.
func mediaDebugFlags(debugEnv, urlsEnv string) (debug, urls bool) {
	urls = debugFlagValue(urlsEnv)
	return urls || debugFlagValue(debugEnv), urls
}

// MediaDebugURLsWarning returns a startup warning when IRCTHING_DEBUG_MEDIA_URLS
// is active, "" otherwise. Logged by main alongside the config warnings: unlike
// plain IRCTHING_DEBUG_MEDIA (anonymized IDs only), this mode defeats the
// keep-URLs-out-of-logs design on purpose, and the operator should see that
// prominently, once, at startup — not discover it in a shipped journal later.
func MediaDebugURLsWarning() string {
	if !mediaDebugURLs {
		return ""
	}
	return "IRCTHING_DEBUG_MEDIA_URLS is set: media debug log lines include full target URLs — sensitive URLs (signed parameters, userinfo) WILL be written to persistent logs; unset it when done debugging"
}

const (
	mediaTargetIDKeySize  = 32
	mediaTargetIDRedacted = "redacted"
)

type mediaTargetIDKey struct {
	value [mediaTargetIDKeySize]byte
	valid bool
}

// mediaTargetKey is generated once so IDs correlate fetches only within this
// process. If the OS random source fails, the invalid key makes every ID a
// fixed redacted value rather than falling back to an unkeyed digest.
var mediaTargetKey = newMediaTargetIDKey(rand.Reader)

func debugFlagValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func newMediaTargetIDKey(random io.Reader) mediaTargetIDKey {
	var key mediaTargetIDKey
	if _, err := io.ReadFull(random, key.value[:]); err != nil {
		return mediaTargetIDKey{}
	}
	key.valid = true
	return key
}

func (key mediaTargetIDKey) id(target string) string {
	if !key.valid {
		return mediaTargetIDRedacted
	}
	mac := hmac.New(sha256.New, key.value[:])
	_, _ = mac.Write([]byte(target))
	sum := mac.Sum(nil)
	return fmt.Sprintf("%x", sum[:8])
}

func mediaTargetID(target string) string {
	return mediaTargetKey.id(target)
}

type mediaLogResult struct {
	event         string
	class         string
	retryable     bool
	httpStatus    int
	bytes         int
	outputBytes   int
	capBytes      int
	blank         bool
	unpreviewable bool
}

// logMedia records one media-fetch outcome when mediaDebug is on. Every field
// is locally generated and structurally typed, so a future call site cannot
// accidentally feed a target URL, title, content type, or raw upstream error
// into the log — with ONE deliberate exception: under mediaDebugURLs the
// target itself is appended (%q-quoted, so a hostile URL cannot forge extra
// log lines), which is exactly the unsafe behavior that env var opts into.
func logMedia(kind, target string, start time.Time, result mediaLogResult) {
	if !mediaDebug {
		return
	}
	urlField := ""
	if mediaDebugURLs {
		urlField = fmt.Sprintf(" url=%q", target)
	}
	log.Printf("media[%s] id=%s%s event=%s class=%s retryable=%t http=%d bytes=%d output_bytes=%d cap_bytes=%d blank=%t unpreviewable=%t duration_ms=%d",
		kind, mediaTargetID(target), urlField, result.event, result.class, result.retryable,
		result.httpStatus, result.bytes, result.outputBytes, result.capBytes,
		result.blank, result.unpreviewable, time.Since(start).Milliseconds())
}

// mediaIDHeader carries the anonymized per-fetch correlation ID on media-plane
// responses (POST /api/preview, POST /api/thumb) while media debug logging is
// active. The browser's devtools show the target URL in the request body and
// this header in the response, and the server log shows the same ID — so the
// operator can correlate the two without URLs ever entering persistent logs.
// Never emitted when mediaDebug is off: production responses stay clean.
const mediaIDHeader = "X-Ircthing-Media-ID"

// withMediaID wraps a media endpoint handler so its response carries
// mediaIDHeader when media debug logging is on. This is the shared choke point
// for both media endpoints (wrapped at registration in api.go): the target
// travels in the POST body, so the body is read here — one byte past
// mediaRequest's 4096 cap, so an oversized body still overflows the handler's
// MaxBytesReader — and replayed to the wrapped handler unchanged. The ID is
// the same keyed digest logMedia uses (mediaTargetID), so header and log
// lines correlate by construction. Decode errors are left for the handler to
// report; the header is simply omitted.
func withMediaID(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !mediaDebug {
			next(w, r)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096+1))
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(body, &req) == nil && req.URL != "" {
			w.Header().Set(mediaIDHeader, mediaTargetID(req.URL))
		}
		next(w, r)
	}
}

// mediaErrorClass turns a potentially sensitive upstream error into a fixed
// diagnostic category. Raw url.Error values repeat the full URL, and proxy or
// origin errors may contain attacker-controlled text, so callers must never
// append err.Error() to media logs.
func mediaErrorClass(err error) string {
	switch {
	case errors.Is(err, errTooLarge):
		return "too_large"
	case errors.Is(err, errBadURL):
		return "bad_url"
	case errors.Is(err, errBlocked):
		return "blocked_address"
	case errors.Is(err, errRedirectLoop), errors.Is(err, errRedirectScheme):
		return "redirect"
	case errors.Is(err, proxydial.ErrProxyConfig):
		return "proxy_config"
	case errors.Is(err, proxydial.ErrProxyRejected):
		return "proxy_rejected"
	case errors.Is(err, proxydial.ErrProxyProtocol):
		return "proxy_protocol"
	case errors.Is(err, errWireBudget):
		return "wire_budget"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var se *upstreamStatusError
	if errors.As(err, &se) {
		return "upstream_http_" + strconv.Itoa(se.code)
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "tls_certificate"
	}
	var dpe *dialPhaseError
	if errors.As(err, &dpe) {
		return "dial"
	}
	var bre *bodyReadError
	if errors.As(err, &bre) {
		return "body_read"
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "network_io"
	}
	return "protocol"
}

// mediaSlots is the IMAGE-DECODE concurrency: each slot admits one whole
// thumbnail request (fetch through decode + encode), bounding in-flight bodies
// and decoded bitmaps together. One slot: a single worst-case decode (10 MiB
// body + ~36 MiB bitmap) fits comfortably under the unit's MemoryMax; two could
// approach it and OOM-restart the bouncer. Link previews are NOT decode-bound,
// so they use previewSlots instead — serializing them behind a slow thumbnail
// decode made previews trickle in one-at-a-time over slow (WireGuard/Tor)
// egress, some 503-ing out entirely.
const mediaSlots = 1

// previewSlots is the LINK-PREVIEW (HTML fetch + parse) concurrency. A preview
// only holds a <=maxHTMLBytes HTML body and no decoded bitmap, so several run at
// once cheaply (~4 x 1 MiB) without the decode memory bound — they no longer
// wait behind image thumbnails.
const previewSlots = 4

// mediaAcquireWait bounds how long a request waits for a slot (var for
// tests).
var mediaAcquireWait = 5 * time.Second

func (s *Server) acquireMedia(ctx context.Context) bool   { return acquireSlot(ctx, s.mediaSem) }
func (s *Server) releaseMedia()                           { <-s.mediaSem }
func (s *Server) acquirePreview(ctx context.Context) bool { return acquireSlot(ctx, s.previewSem) }
func (s *Server) releasePreview()                         { <-s.previewSem }

// acquireSlot takes a slot on sem, giving up after a short bounded wait (or when
// the request dies) so waiters cannot pile up.
func acquireSlot(ctx context.Context, sem chan struct{}) bool {
	t := time.NewTimer(mediaAcquireWait)
	defer t.Stop()
	select {
	case sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

var (
	errBadURL         = errors.New("proxy: url must be an absolute http(s) URL")
	errTooLarge       = errors.New("proxy: response exceeds size cap")
	errBlocked        = errors.New("proxy: refusing to connect to a non-public address")
	errRedirectLoop   = errors.New("proxy: too many redirects")
	errRedirectScheme = errors.New("proxy: disallowed redirect scheme")
)

// bodyReadError tags a failure that happened while reading the RESPONSE BODY —
// after the origin already answered 200 and streamed headers. In this phase a
// deterministic protocol/framing failure (malformed chunk terminator, bad
// trailer — untyped errors.New strings inside net/http) is indistinguishable
// from hostility: an endpoint can stream ~10 MiB and then break the framing,
// and a transient classification hands it the client's full retry budget
// (~40 MiB and four tracking hits per URL). So the DEFAULT here is permanent;
// the explicitly transient exceptions (fetchErrorRetryable) are genuine
// network-level failures: net.Error (reset/timeout at the socket), context
// deadline, and a bare connection cut (io.ErrUnexpectedEOF).
type bodyReadError struct{ err error }

func (e *bodyReadError) Error() string { return "proxy: reading body: " + e.err.Error() }
func (e *bodyReadError) Unwrap() error { return e.err }

// upstreamStatusError is get's error for a non-200 upstream response. It carries
// the status so the caller can tell a transient upstream hiccup (5xx/429/408 —
// worth a client retry) from a permanent one (403/404/410 — retrying just repeats
// a tracking hit and holds a media slot).
type upstreamStatusError struct{ code int }

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("proxy: upstream status %d", e.code)
}

// fetchErrorRetryable reports whether a get() failure is transient and worth the
// client retrying (mapped to 503). Permanent failures map to 502 so the browser
// caches the failure for FAIL_TTL instead of hammering four requests at it:
//   - errTooLarge: the body already exceeds the cap; a retry re-downloads it (up
//     to ~10 MiB per attempt for an oversized image) to the same end.
//   - errBadURL / errBlocked: our own validation rejected it; deterministic.
//   - errRedirectLoop / errRedirectScheme: the target's redirect behavior is
//     deterministic too, and each retry of a five-hop loop re-walks all five
//     hops — an untyped classification here turned one client retry cycle into
//     ~twenty upstream requests.
//   - a malformed Location header (rejected by net/http before CheckRedirect
//     runs): equally deterministic; recognized by message text, see below.
//   - certificate verification failure: the peer's certificate won't become
//     valid between immediate retries.
//   - permanent upstream status (4xx except 408/429): the origin answered "no".
//
// Everything else — dial/timeout errors (a WireGuard tunnel still warming up),
// TLS handshake I/O errors, truncated reads, and 5xx/408/429 upstream — is
// transient.
// retryableUpstreamStatus reports whether a non-200 upstream status is a
// transient hiccup worth a client retry, as opposed to a permanent "no".
func retryableUpstreamStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, // 408, 429
		http.StatusInternalServerError, http.StatusBadGateway, // 500, 502
		http.StatusServiceUnavailable, http.StatusGatewayTimeout: // 503, 504
		return true
	default:
		return false // 4xx and other permanent statuses
	}
}

func fetchErrorRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errTooLarge) || errors.Is(err, errBadURL) || errors.Is(err, errBlocked) ||
		errors.Is(err, errRedirectLoop) || errors.Is(err, errRedirectScheme) {
		return false
	}
	// A proxy/target MISCONFIGURATION (bad scheme/host/port/creds) or a
	// DETERMINISTIC proxy rejection (SOCKS auth/policy failure, HTTP 4xx other
	// than 408/429) is permanent — retrying re-runs the same rejected dial.
	// Both surface through the dialPhaseError wrapper (transient by default),
	// so classify them permanent explicitly, ahead of that default.
	if errors.Is(err, proxydial.ErrProxyConfig) || errors.Is(err, proxydial.ErrProxyRejected) ||
		errors.Is(err, proxydial.ErrProxyProtocol) {
		return false
	}
	// Wire-budget exhaustion is an upstream that deliberately (or patholog-
	// ically) inflates transport bytes; retrying re-downloads the same flood.
	if errors.Is(err, errWireBudget) {
		return false
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return false
	}
	// A syntactically invalid Location header is rejected inside net/http
	// BEFORE CheckRedirect runs, so it cannot be typed there; the client
	// returns it as an untyped fmt error inside *url.Error (go1.25
	// net/http/client.go, redirectBehavior). Match its stable message text —
	// a malformed redirect is a deterministic property of the target, and an
	// untyped classification gave it the full transient-retry treatment.
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil &&
		strings.Contains(uerr.Err.Error(), "failed to parse Location header") {
		return false
	}
	var se *upstreamStatusError
	if errors.As(err, &se) {
		return retryableUpstreamStatus(se.code)
	}
	// Explicitly TRANSIENT classes, whatever phase they surface in:
	//   - dialPhaseError: TCP/proxy/tunnel dial failures — a WireGuard tunnel
	//     warming up fails exactly here, and its errors are custom types, not
	//     net.Errors, hence the tag.
	//   - net.Error anywhere in the chain: socket-level resets/timeouts,
	//     including mid-handshake and mid-body I/O failures.
	//   - context deadline/cancel: the 15 s client timeout.
	//   - io.ErrUnexpectedEOF / io.EOF: a bare connection cut (mid-body, or
	//     closed before the response line — a restarting server does both).
	// Look for net.Error BELOW the url.Error wrapper: *url.Error itself
	// implements net.Error (delegating Timeout/Temporary), so matching the
	// wrapper would classify EVERY client.Do failure — including malformed-
	// response parser errors — as transient.
	inner := err
	var uw *url.Error
	if errors.As(err, &uw) && uw.Err != nil {
		inner = uw.Err
	}
	var dpe *dialPhaseError
	var ne net.Error
	if errors.As(err, &dpe) || errors.As(inner, &ne) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	// DEFAULT: permanent. What remains is the deterministic residue of both
	// phases — malformed status line/headers from client.Do, malformed
	// chunked framing or trailers mid-body (bodyReadError) — and a hostile
	// endpoint can burn the full wire budget before emitting any of them;
	// handing them the client's retry budget multiplies that by four.
	return false
}

type fetcher struct {
	client   *http.Client
	maxBytes int64
	// proxied is true when fetches tunnel through a configured proxy. Then
	// the connect-time Control hook can't see the (proxy-resolved) target
	// IP, so hostAllowed is applied to literal-IP targets in get and on
	// redirects instead. Direct fetches rely solely on the Control hook.
	proxied bool
	// truncate reads at most maxBytes and USES that prefix instead of failing
	// when the body is larger. Set for HTML link-preview fetches: the og/title
	// metadata lives in <head> at the top of the document, so a large page
	// (Next.js/GitBook/YouTube routinely exceed the cap) yields a good preview
	// from its head. Left false for image fetches, where a truncated body is a
	// corrupt image and must be rejected (errTooLarge).
	truncate bool
	// allowIP decides whether a resolved address may be dialed. Field so
	// tests can permit loopback (httptest listens on 127.0.0.1, which the
	// real policy blocks).
	allowIP func(net.IP) bool
}

// errWireBudget: the connection consumed more RAW network bytes than the fetch
// could legitimately need. Deliberate transport-byte inflation — retrying just
// re-downloads the flood, so it classifies as permanent.
var errWireBudget = errors.New("proxy: connection exceeded its wire-byte budget")

// wireBudgetAllowance is headroom above maxBytes for everything that is not
// body payload: the TLS handshake (certificate chains run tens of KiB),
// response headers (separately capped at 64 KiB), and per-record TLS framing
// overhead on a legitimate transfer (~1–2 %).
const wireBudgetAllowance = 512 << 10

// budgetConn enforces a raw-byte read budget on a dialed connection, BELOW
// TLS. The body LimitReader in get() caps plaintext bytes only — a hostile
// HTTPS origin can wrap each body byte in maximally-padded TLS records
// (~256 KiB of ciphertext per plaintext byte), streaming unbounded wire bytes
// under any plaintext cap until the client timeout. Counting at the conn makes
// the cap a true transport-byte cap. Reads are capped to the remaining budget,
// then fail with a net.OpError wrapping errWireBudget — a net.Error, so
// crypto/tls propagates it, and errors.Is still finds errWireBudget through
// the url.Error/OpError chain for permanent classification.
//
// remaining is SHARED across every connection of one get() call (seeded into
// the request context): redirects dial fresh connections (DisableKeepAlives),
// and a per-connection budget would multiply by the five allowed hops —
// ~52 MiB of padding for one nominal 10 MiB thumbnail. Atomic because the
// transport may race a speculative next-hop dial against a body read.
type budgetConn struct {
	net.Conn
	remaining *atomic.Int64
}

func (c *budgetConn) Read(p []byte) (int, error) {
	left := c.remaining.Load()
	if left <= 0 {
		return 0, &net.OpError{Op: "read", Net: "tcp", Err: errWireBudget}
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	n, err := c.Conn.Read(p)
	c.remaining.Add(-int64(n))
	return n, err
}

// wireBudgetKey carries the request-wide raw-byte counter in the context from
// get() to the dialer.
type wireBudgetKey struct{}

// dialPhaseError tags any failure from the dial func itself — TCP connect,
// SOCKS/HTTP proxy handshake, or a WireGuard tunnel that is down or warming
// up. These are the canonical TRANSIENT failures (the whole reason the client
// retries), and tagging them here is what lets fetchErrorRetryable default
// everything else in the Do phase — malformed status lines, bad headers —
// to permanent without misclassifying custom (non-net.Error) dial errors.
type dialPhaseError struct{ err error }

func (e *dialPhaseError) Error() string { return "proxy: dial: " + e.err.Error() }
func (e *dialPhaseError) Unwrap() error { return e.err }

// withWireBudget wraps a dial func so every connection it returns draws from
// the request's shared raw-byte budget, and tags dial failures as
// dialPhaseError (transient). A dial without a seeded budget (defensive;
// get() always seeds it) gets a fresh single-connection budget.
func (f *fetcher) withWireBudget(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, &dialPhaseError{err}
		}
		budget, _ := ctx.Value(wireBudgetKey{}).(*atomic.Int64)
		if budget == nil {
			budget = new(atomic.Int64)
			budget.Store(f.maxBytes + wireBudgetAllowance)
		}
		return &budgetConn{Conn: c, remaining: budget}, nil
	}
}

// newFetcher builds a media fetcher. When proxy is non-nil, every fetch is
// tunneled through it (SOCKS5/HTTP), so the fetch does not reveal the
// server's real IP — matching a network's proxy for anonymity. Direct and
// proxied paths both refuse a literal non-public IP target (hostAllowed);
// the direct path additionally re-validates the resolved IP at connect
// time (rebinding-safe), while the proxied path leaves DNS to the proxy.
func newFetcher(maxBytes int64, proxy *url.URL) *fetcher {
	f := &fetcher{maxBytes: maxBytes, allowIP: isPublicIP}
	f.client = &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:                  nil, // never honor $HTTP_PROXY implicitly
			DialContext:            f.withWireBudget(f.dialContext(proxy)),
			TLSHandshakeTimeout:    8 * time.Second,
			ResponseHeaderTimeout:  8 * time.Second,
			MaxResponseHeaderBytes: 64 << 10, // hostile targets can otherwise stream ~10 MiB of headers (Go default) outside the body budget
			// Transparent gzip is LEFT ON. It was disabled once so the body
			// LimitReader (which caps DECOMPRESSED bytes) couldn't be bypassed by a
			// hostile origin streaming empty/padded gzip members — but the budgetConn
			// now caps RAW WIRE bytes below TLS (see withWireBudget), so that bomb
			// vector is bounded regardless: wire bytes by budgetConn, decompressed
			// bytes by the LimitReader, CPU by both. Keeping gzip on matters for
			// LATENCY: a large HTML page (YouTube's ~1.1 MiB) fetched uncompressed
			// over a slow WireGuard/Tor egress can blow the 15 s timeout and blank the
			// preview; gzipped it is ~200 KiB.
			DisableKeepAlives: true,
			MaxIdleConns:      0,
		},
		CheckRedirect: f.checkRedirect,
	}
	return f
}

// newTunnelFetcher builds a media fetcher that dials every target through the
// supplied dial function — a network's in-process WireGuard tunnel. Like the
// proxied path, the tunnel owns egress AND DNS (targets resolve in-tunnel), so
// the connect-time Control hook can't see the resolved IP: SSRF handling is the
// proxied mode (literal non-public IPs refused up front and on redirects,
// hostname resolution deferred to the tunnel — the same trust model as a
// SOCKS5 proxy). The dial func is tunnel-only: if the tunnel is down it returns
// an error and the fetch fails closed, never falling back to a direct dial.
func newTunnelFetcher(maxBytes int64, dial func(ctx context.Context, addr string) (net.Conn, error)) *fetcher {
	f := &fetcher{maxBytes: maxBytes, allowIP: isPublicIP, proxied: true}
	f.client = &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: f.withWireBudget(func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dial(ctx, addr)
			}),
			TLSHandshakeTimeout:    8 * time.Second,
			ResponseHeaderTimeout:  8 * time.Second,
			MaxResponseHeaderBytes: 64 << 10, // hostile targets can otherwise stream ~10 MiB of headers (Go default) outside the body budget
			DisableKeepAlives:      true,
			MaxIdleConns:           0,
		},
		CheckRedirect: f.checkRedirect,
	}
	return f
}

// dialContext returns the transport dialer. Proxied: tunnel through the
// proxy (which owns egress + DNS); direct: a plain dialer whose Control
// hook re-validates the resolved IP at connect time (rebinding-safe).
func (f *fetcher) dialContext(proxy *url.URL) func(context.Context, string, string) (net.Conn, error) {
	if proxy != nil {
		f.proxied = true
		return func(ctx context.Context, _, addr string) (net.Conn, error) {
			return proxydial.Dial(ctx, proxy, addr, 8*time.Second)
		}
	}
	d := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: -1}
	d.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !f.allowIP(ip) {
			return fmt.Errorf("%w: %s", errBlocked, address)
		}
		return nil
	}
	return d.DialContext
}

// validMediaURL vets a parsed URL for both the initial fetch and every redirect
// hop, BEFORE any dial: http(s) scheme, a non-empty hostname, and either no
// explicit port or a decimal port in 1..65535. Checking u.Host alone is not
// enough — "http://:80/" has a non-empty Host ("​:80") but an EMPTY Hostname(),
// and the proxy/CONNECT/SOCKS behavior for a hostless authority or a bogus port
// is implementation-defined. Returns false for anything that must be refused.
func validMediaURL(u *url.URL) bool {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return false
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}

// checkRedirect vets each redirect hop. Its errors are TYPED (and survive
// the *url.Error wrapping client.Do applies) so fetchErrorRetryable can
// classify a redirect loop or a forbidden scheme as permanent — both are
// deterministic properties of the target, and retrying a loop re-walks
// every hop.
func (f *fetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errRedirectLoop
	}
	if !validMediaURL(req.URL) {
		return errRedirectScheme // deterministic → permanent (fetchErrorRetryable)
	}
	// The proxied path has no connect-time IP check, so re-apply the literal
	// non-public-IP block on each redirect target.
	if f.proxied && !f.hostAllowed(req.URL.Hostname()) {
		return fmt.Errorf("%w: %s", errBlocked, req.URL.Host)
	}
	// Go's client sets a Referer on redirects; strip it so the full
	// (possibly signed/private) preview URL does not leak into the
	// redirect target's or an intermediary's logs.
	req.Header.Del("Referer")
	return nil
}

// hostAllowed rejects a target whose host is a literal non-public IP. A
// hostname passes: the direct path re-checks its resolved IP at dial via
// the Control hook, and the proxied path defers resolution to the proxy.
func (f *fetcher) hostAllowed(host string) bool {
	ip := net.ParseIP(host)
	return ip == nil || f.allowIP(ip)
}

// get fetches rawURL, returning its content type, the FINAL URL after any
// redirects, and body (capped at maxBytes). Only absolute http(s) URLs are
// allowed. The final URL lets the caller resolve relative assets (og:image)
// against the page they actually came from, not the pre-redirect target.
func (f *fetcher) get(ctx context.Context, rawURL string) (contentType, finalURL string, body []byte, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || !validMediaURL(u) {
		return "", "", nil, errBadURL
	}
	// Proxied path: refuse a literal non-public IP up front, since the
	// Control hook can't re-check the proxy-resolved address at dial time.
	// (Direct fetches are covered by that hook.)
	if f.proxied && !f.hostAllowed(u.Hostname()) {
		return "", "", nil, errBlocked
	}
	// One raw-byte budget for the WHOLE request, including every redirect
	// hop's fresh connection — see budgetConn.
	budget := new(atomic.Int64)
	budget.Store(f.maxBytes + wireBudgetAllowance)
	ctx = context.WithValue(ctx, wireBudgetKey{}, budget)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", nil, err
	}
	req.Header.Set("User-Agent", proxyUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", nil, &upstreamStatusError{resp.StatusCode}
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		// Tag the phase: a failure while READING THE BODY (headers already
		// received) is classified permanent-by-default — see bodyReadError.
		return "", "", nil, &bodyReadError{err}
	}
	if int64(len(body)) > f.maxBytes {
		if !f.truncate {
			return "", "", nil, errTooLarge
		}
		// HTML preview: keep the head-bearing prefix rather than dropping the
		// whole page. extractMeta only scans up to </head> anyway.
		body = body[:f.maxBytes]
	}
	finalURL = rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String() // the URL after redirects
	}
	return resp.Header.Get("Content-Type"), finalURL, body, nil
}

// specialPurposePrefixes are the IANA special-purpose registry blocks
// (plus multicast/reserved space) that net.IP's classification methods
// do not cover — "not private" is weaker than "globally routable".
// Tables: iana.org/assignments/iana-ipv4-special-registry and
// iana-ipv6-special-registry (fetched 2026-07-18).
var specialPurposePrefixes = func() []netip.Prefix {
	specs := []string{
		// IPv4
		"0.0.0.0/8",       // "this network"
		"192.0.0.0/24",    // protocol assignments (incl. 192.0.0.0/29 DS-Lite)
		"192.0.2.0/24",    // TEST-NET-1
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"192.31.196.0/24", // AS112-v4
		"192.52.193.0/24", // AMT
		"192.88.99.0/24",  // deprecated 6to4 relay anycast
		"192.175.48.0/24", // AS112 direct delegation
		"100.64.0.0/10",   // CGNAT shared address space
		"198.18.0.0/15",   // benchmarking
		"240.0.0.0/4",     // reserved (incl. 255.255.255.255 broadcast)
		// IPv6
		"::/128",        // unspecified (also caught by IsUnspecified)
		"::1/128",       // loopback (also caught by IsLoopback)
		"::ffff:0:0/96", // IPv4-mapped (unwrapped before this check)
		// Deprecated IPv4-embedding forms (RFC 5156): like NAT64, these
		// encode an IPv4 destination, and a lingering translation or
		// tunnel route would reach IPv4 space this policy blocks. An
		// SSRF allowlist has no use for deprecated transition space.
		"::/96",           // IPv4-compatible (deprecated, RFC 4291 §2.5.5.1)
		"::ffff:0:0:0/96", // IPv4-translated (SIIT, RFC 2765)
		// NAT64 translation prefixes embed an IPv4 destination: on a
		// NAT64 host, 64:ff9b::a9fe:a9fe reaches 169.254.169.254 even
		// though the direct IPv4 form is blocked. The whole well-known
		// prefix is denied (site-specific NSP prefixes cannot be known
		// statically and are the deployment's responsibility).
		"64:ff9b::/96",   // well-known NAT64 (RFC 6052)
		"64:ff9b:1::/48", // local-use IPv4/IPv6 translation (RFC 8215)
		"100::/64",       // discard-only
		"2001::/23",      // protocol assignments (TEREDO, ORCHID, benchmarking)
		"2001:db8::/32",  // documentation
		"2002::/16",      // 6to4
		"3fff::/20",      // documentation (RFC 9637)
		"5f00::/16",      // segment routing
	}
	out := make([]netip.Prefix, len(specs))
	for i, s := range specs {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}()

// isPublicIP reports whether ip is a globally-routable public address —
// the allowlist gate for outbound proxy fetches. It rejects loopback,
// RFC1918/ULA private ranges, link-local (incl. the 169.254.169.254
// cloud metadata endpoint), unspecified, multicast, CGNAT, and the IANA
// special-purpose blocks (documentation, benchmarking, 0/8, 240/4,
// translation/transition ranges).
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, p := range specialPurposePrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	// IPv6 allowlist backstop: require global unicast (2000::/3). The
	// denylist above is necessarily incomplete for non-global space that
	// net.IP's helpers do not classify — e.g. deprecated site-local
	// fec0::/10 (RFC 3879), which a host with a lingering internal route
	// could still reach. Everything routable on the public Internet lives
	// in 2000::/3; reject anything outside it.
	if addr.Is6() && !globalUnicastV6.Contains(addr) {
		return false
	}
	return true
}

// globalUnicastV6 is the IPv6 global-unicast block (RFC 4291 §2.4). It
// backs the isPublicIP allowlist so non-global IPv6 space (site-local,
// ULA, link-local, multicast) is rejected regardless of the denylist.
var globalUnicastV6 = netip.MustParsePrefix("2000::/3")
