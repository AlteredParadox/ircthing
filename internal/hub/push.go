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

package hub

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ircthing/internal/irc"
	"ircthing/internal/store"
	"ircthing/internal/webpush"
)

// Web Push scheduling: a highlight or PM starts a per-buffer countdown
// (pushDelay); if any device advances the buffer's read marker past the
// newest highlighted message before it fires, the push is cancelled —
// you were clearly reading. Everything here runs in ONE goroutine
// (runPusher) that owns all pending state; handlers feed it through
// non-blocking channel sends, per the hub's no-new-mutexes rule. Pushes
// are best effort: a dropped channel send is caught by the fire-time
// read-marker re-check or, worst case, costs one notification.

const (
	// pushDelay is the cancel-on-read window. Long enough to type a
	// reply on another device (whose markRead then cancels), short
	// enough that a pocketed phone still feels prompt.
	pushDelay = 20 * time.Second
	// pushTTLSeconds is the RFC 8030 §5.2 retention at the push service:
	// an hour-stale highlight is noise, not news.
	pushTTLSeconds = 3600
	// maxPendingPushes bounds the scheduler's memory; 64 buffers with
	// unread highlights inside one 20s window means something is wrong.
	maxPendingPushes = 64
	// maxPushTextBytes caps the payload preview. The encrypted body must
	// fit one record (~3.9 KB); a notification shows ~2 lines anyway.
	maxPushTextBytes = 256
	// pushSubject is the VAPID contact URI (RFC 8292 §2.1).
	pushSubject = "https://github.com/AlteredParadox/ircthing"
	// vapidKeyKey is the settings-table key for the persisted VAPID
	// signing key. Persisted because subscriptions bind to its public
	// half: regenerating on restart would orphan every device.
	vapidKeyKey = "webpush_vapid_key"
)

// PushSender abstracts webpush.Sender so scheduler tests can fake
// delivery.
type PushSender interface {
	Send(ctx context.Context, sub webpush.Subscription, body []byte, ttl int) error
}

// pushCandidate is one live message the pusher should consider — built
// in persistEvent with plain strings only, so the hot path does one
// struct fill and one non-blocking send.
type pushCandidate struct {
	network, buffer string
	sender, text    string
	nick            string // our current nick on that network (mention target)
	ts              int64  // unix ms
	msgid           string
	channelLike     bool // channel or the "*" server buffer: highlight-gated; PMs always push
}

// markerAdvance mirrors a read-marker write (any device, or upstream
// MARKREAD) into the pusher for cancellation.
type markerAdvance struct {
	network, buffer string
	ts              int64 // unix ms, authoritative (never-regress) value
}

// pushCancel invalidates pending pushes when their subject disappears:
// buffer=="" cancels every pending push on the network (delete/rename);
// msgid=="" cancels the buffer's push (close/archive); a set msgid is a
// REDACTION — the matching entry is dropped or its content scrubbed, so
// destructively-redacted text cannot ride out to a notification tray
// the redaction machinery can never reach.
type pushCancel struct {
	network, buffer, msgid string
}

// pendingPush is one scheduled notification, coalescing every further
// highlight in the same buffer until it fires.
type pendingPush struct {
	fireAt      time.Time
	sender      string // first unread highlight's sender/text head the notification
	text        string
	msgid       string
	ts          int64 // first highlight's time (shown time)
	newestTS    int64 // newest coalesced highlight (cancel threshold)
	count       int
	channelLike bool
}

// pushPayload is the (RFC 8291-encrypted) JSON the service worker
// receives.
type pushPayload struct {
	Network string `json:"network"`
	Buffer  string `json:"buffer"`
	Sender  string `json:"sender"`
	Text    string `json:"text"`
	TS      int64  `json:"ts"`
	MsgID   string `json:"msgid,omitempty"`
	Count   int    `json:"count"`
	Channel bool   `json:"channel"`
}

// StartPusher loads (or creates and persists) the VAPID key and starts
// the push scheduler on the process WaitGroup, so shutdown drains it
// before the store closes. Call once, before serving.
func (h *Hub) StartPusher(ctx context.Context, wg *sync.WaitGroup) error {
	priv, err := h.loadOrCreateVapidKey(ctx)
	if err != nil {
		return err
	}
	pub, err := webpush.PublicKeyB64(&priv.PublicKey)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.pushPubKey = pub
	h.mu.Unlock()
	h.startPusher(ctx, wg, webpush.NewSender(priv, pushSubject), pushDelay)
	return nil
}

// loadOrCreateVapidKey returns the persisted VAPID key, generating and
// storing one on first use. An unreadable stored key is replaced (and
// logged loudly): a fresh key orphans existing subscriptions, but the
// client detects the applicationServerKey change and re-subscribes.
func (h *Hub) loadOrCreateVapidKey(ctx context.Context) (*ecdsa.PrivateKey, error) {
	stored, err := h.store.Setting(ctx, vapidKeyKey)
	if err != nil {
		return nil, err
	}
	if stored != "" {
		priv, err := webpush.ParseKey(stored)
		if err == nil {
			return priv, nil
		}
		log.Printf("push: stored VAPID key unreadable (%v); generating a new one — devices will re-subscribe on next load", err)
	}
	priv, err := webpush.GenerateKey()
	if err != nil {
		return nil, err
	}
	marshaled, err := webpush.MarshalKey(priv)
	if err != nil {
		return nil, err
	}
	if err := h.store.SetSetting(ctx, vapidKeyKey, marshaled); err != nil {
		return nil, err
	}
	return priv, nil
}

// startPusher is StartPusher minus key management, split for tests.
func (h *Hub) startPusher(ctx context.Context, wg *sync.WaitGroup, sender PushSender, delay time.Duration) {
	h.RefreshPushCount(ctx)
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.runPusher(ctx, sender, delay)
	}()
}

// PushPublicKey returns the VAPID public key (unpadded base64url), or ""
// before StartPusher has provisioned one — the client treats "" as
// push-not-available.
func (h *Hub) PushPublicKey() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pushPubKey
}

// RefreshPushCount re-reads the subscription count into the atomic the
// per-message fast path checks. Called at startup and by the subscribe/
// unsubscribe endpoints. A failed refresh keeps the previous value and
// is LOGGED — a silently stale 0 would disable every push.
func (h *Hub) RefreshPushCount(ctx context.Context) {
	n, err := h.store.CountPushSubscriptions(ctx)
	if err != nil {
		log.Printf("push: refreshing subscription count: %v", err)
		return
	}
	h.pushSubs.Store(int64(n))
}

// maybePushCandidate feeds one freshly persisted LIVE message to the
// scheduler. stored.Text is non-empty exactly for real PRIVMSG/NOTICE
// content (CTCP ACTION unwrapped) — the same set search indexes.
//
// Everything is taken from `stored`, NOT the wire event: stored.Target
// is the CANONICAL buffer spelling (append resolves #Go -> #go etc.)
// and clients key their buffers and read markers by it, so the pending
// key, the marker cancels, the fire-time re-check, the mute lookup, and
// the payload all agree only in that spelling. stored's fields are also
// store-clamped, which bounds the payload (a hostile server's 64 KiB
// wire target must not reach Encrypt).
func (h *Hub) maybePushCandidate(c Conn, ev irc.Event, stored store.Message, replay, own bool) {
	if replay || own || stored.Text == "" || h.pushSubs.Load() == 0 {
		return
	}
	cand := pushCandidate{
		network:     ev.Network,
		buffer:      stored.Target,
		sender:      stored.Sender,
		text:        stored.Text,
		nick:        c.Nick(),
		ts:          stored.Time.UnixMilli(),
		msgid:       stored.MsgID,
		channelLike: c.IsChannel(stored.Target) || isServerBuffer(stored.Target),
	}
	select {
	case h.pushCandidates <- cand:
	default: // full queue: drop; the fire-time re-check keeps us honest
	}
}

// notifyMarkerAdvance mirrors an authoritative read-marker write into
// the scheduler for cancel-on-read.
func (h *Hub) notifyMarkerAdvance(network, buffer string, t time.Time) {
	if t.IsZero() {
		return
	}
	select {
	case h.pushMarkers <- markerAdvance{network: network, buffer: buffer, ts: t.UnixMilli()}:
	default: // dropped: the fire-time store re-check still cancels
	}
}

// notifyPushConfigChanged tells the scheduler the stored highlight rules
// changed; it reloads them on its own goroutine.
func (h *Hub) notifyPushConfigChanged() {
	select {
	case h.pushConfigDirty <- struct{}{}:
	default: // already flagged
	}
}

// notifyPushCancel feeds one cancellation to the scheduler (see
// pushCancel for the shapes). Non-blocking like every pusher send; the
// fire-time buffer-existence check backstops a dropped close/delete.
func (h *Hub) notifyPushCancel(network, buffer, msgid string) {
	select {
	case h.pushCancels <- pushCancel{network: network, buffer: buffer, msgid: msgid}:
	default:
	}
}

// runPusher is the scheduler loop; sole owner of pending-push state and
// the rules cache.
func (h *Hub) runPusher(ctx context.Context, sender PushSender, delay time.Duration) {
	rules := h.loadRules(ctx)
	filters := buildPushFilters(h.loadFilters(ctx))
	pending := make(map[string]*pendingPush) // network+"\x00"+buffer
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return // pending pushes die with the process: best effort by design

		case c := <-h.pushCandidates:
			if admitCandidate(pending, c, rules, filters, delay) {
				rearmPushTimer(timer, pending)
			}

		case m := <-h.pushMarkers:
			key := m.network + "\x00" + m.buffer
			if p, ok := pending[key]; ok && p.newestTS <= m.ts {
				delete(pending, key)
				rearmPushTimer(timer, pending)
			}

		case cn := <-h.pushCancels:
			if applyPushCancel(pending, cn) {
				rearmPushTimer(timer, pending)
			}

		case <-h.pushConfigDirty:
			rules = h.loadRules(ctx)
			filters = buildPushFilters(h.loadFilters(ctx))
			// Muting a buffer (or ignoring its latest pinger) mid-window
			// is exactly when the user expects silence: re-apply the new
			// filters to what is already scheduled, not just to future
			// candidates.
			if sweepFilteredPending(pending, filters) {
				rearmPushTimer(timer, pending)
			}

		case <-timer.C:
			h.fireDuePushes(ctx, sender, pending)
			rearmPushTimer(timer, pending)
		}
	}
}

// applyPushCancel applies one pushCancel to the pending map; reports
// whether anything changed.
func applyPushCancel(pending map[string]*pendingPush, cn pushCancel) bool {
	if cn.buffer == "" { // network deleted or renamed
		changed := false
		prefix := cn.network + "\x00"
		for key := range pending {
			if strings.HasPrefix(key, prefix) {
				delete(pending, key)
				changed = true
			}
		}
		return changed
	}
	key := cn.network + "\x00" + cn.buffer
	p, ok := pending[key]
	if !ok {
		return false
	}
	if cn.msgid == "" { // buffer closed or archived
		delete(pending, key)
		return true
	}
	if p.msgid == "" || p.msgid != cn.msgid {
		// The redacted message is not the one headlining this push
		// (coalesced siblings keep only the first msgid) — leave it.
		return false
	}
	if p.count == 1 {
		delete(pending, key)
		return true
	}
	// Coalesced siblings remain: keep the notification but scrub the
	// redacted headline — the service worker falls back to its generic
	// title/body when sender/text are absent.
	p.count--
	p.sender, p.text, p.msgid = "", "", ""
	return false
}

// sweepFilteredPending drops pending pushes the just-reloaded filters
// now exclude; reports whether anything changed. Only the FIRST
// coalesced sender is known per entry — good enough: that is the sender
// the user just reacted to.
func sweepFilteredPending(pending map[string]*pendingPush, filters pushFilters) bool {
	changed := false
	for key, p := range pending {
		network, buffer, _ := strings.Cut(key, "\x00")
		if filters.drops(network, buffer, p.sender) {
			delete(pending, key)
			changed = true
		}
	}
	return changed
}

// admitCandidate applies the filter and highlight policy to one live
// message and schedules (or coalesces) its push. Reports whether a NEW
// entry was scheduled, i.e. the timer needs rearming.
func admitCandidate(pending map[string]*pendingPush, c pushCandidate, rules []Rule, filters pushFilters, delay time.Duration) bool {
	// Ignored senders and muted buffers never push — the synced mirror
	// of the client's alert exclusions (app.jsx event path).
	if filters.drops(c.network, c.buffer, c.sender) {
		return false
	}
	// PMs always push; channels (and "*") need a mention/keyword.
	if c.channelLike && !highlightText(c.text, c.nick, rules, c.network) {
		return false
	}
	key := c.network + "\x00" + c.buffer
	if p, ok := pending[key]; ok {
		// Coalesce; do NOT extend fireAt — a chatty channel must not
		// defer its notification forever.
		p.count++
		p.newestTS = max(p.newestTS, c.ts)
		return false
	}
	if len(pending) >= maxPendingPushes {
		log.Printf("push: pending cap reached, dropping highlight in %s/%s", c.network, c.buffer)
		return false
	}
	pending[key] = &pendingPush{
		fireAt: time.Now().Add(delay),
		sender: c.sender, text: c.text, msgid: c.msgid,
		ts: c.ts, newestTS: c.ts, count: 1, channelLike: c.channelLike,
	}
	return true
}

// rearmPushTimer points the single timer at the earliest pending
// deadline (stopped when none). Go 1.23+ timer semantics: Stop/Reset
// without the drain dance is safe.
func rearmPushTimer(timer *time.Timer, pending map[string]*pendingPush) {
	timer.Stop()
	var next time.Time
	for _, p := range pending {
		if next.IsZero() || p.fireAt.Before(next) {
			next = p.fireAt
		}
	}
	if !next.IsZero() {
		timer.Reset(time.Until(next))
	}
}

// fireDuePushes delivers every pending push whose deadline has passed.
func (h *Hub) fireDuePushes(ctx context.Context, sender PushSender, pending map[string]*pendingPush) {
	now := time.Now()
	for key, p := range pending {
		if p.fireAt.After(now) {
			continue
		}
		delete(pending, key)
		h.deliverPush(ctx, sender, key, p)
	}
}

// pushFilters is the pusher's lookup form of the synced ignore/mute
// lists: ignores as network -> lowercased-nick set (the client's ASCII
// fold, local.js isIgnored), mutes as a bufKey set (network+"\n"+buffer,
// the client's exact-match key).
type pushFilters struct {
	ignores map[string]map[string]bool
	mutes   map[string]bool
}

func buildPushFilters(d FiltersData) pushFilters {
	f := pushFilters{ignores: make(map[string]map[string]bool, len(d.Ignores)), mutes: make(map[string]bool, len(d.Mutes))}
	for network, nicks := range d.Ignores {
		set := make(map[string]bool, len(nicks))
		for _, n := range nicks {
			// set_filters stores lowercased, but old/hand-edited blobs
			// must match too. Known, accepted divergence: Go's simple
			// Unicode lowering differs from JS toLowerCase for a few
			// exotic mappings (Turkish İ gains a combining dot in JS;
			// Greek final sigma is contextual there), so an ignore on
			// such a nick can miss server-side while working in the
			// browser. IRC nicks are overwhelmingly ASCII (many networks
			// enforce it); a full JS-compatible case mapper is not worth
			// its weight for this. Same tradeoff family as foldNick's
			// loose rfc1459 superset.
			set[strings.ToLower(n)] = true
		}
		f.ignores[network] = set
	}
	for _, m := range d.Mutes {
		f.mutes[m] = true
	}
	return f
}

func (f pushFilters) drops(network, buffer, sender string) bool {
	if f.mutes[network+"\n"+buffer] {
		return true
	}
	return sender != "" && f.ignores[network][strings.ToLower(sender)]
}

// deliverPush sends one due notification to every subscription, after
// the authoritative cancel re-check: the store's read marker covers
// marker writes whose channel send was dropped AND upstream MARKREAD
// from other bouncer clients.
func (h *Hub) deliverPush(ctx context.Context, sender PushSender, key string, p *pendingPush) {
	network, buffer, _ := strings.Cut(key, "\x00")
	if t, err := h.store.ReadMarker(ctx, network, buffer); err == nil && !t.IsZero() && t.UnixMilli() >= p.newestTS {
		return
	}
	// The buffer may have been purged after scheduling — the close/delete
	// hooks cancel pending pushes, but their channel sends are best
	// effort. A buffer the store no longer knows gets no push (and its
	// notification would navigate nowhere). Identity fold: `buffer` is
	// already the canonical stored spelling.
	name, found, err := h.store.FindBuffer(ctx, network, buffer, func(s string) string { return s })
	if err != nil {
		// Fail closed (skip the push) but never silently: a transient
		// store error here eats a notification.
		log.Printf("push: checking buffer %s/%s: %v", network, buffer, err)
		return
	}
	if !found || name != buffer {
		return
	}
	subs, err := h.store.PushSubscriptions(ctx)
	if err != nil {
		log.Printf("push: loading subscriptions: %v", err)
		return
	}
	if len(subs) == 0 {
		return
	}
	payload, err := json.Marshal(pushPayload{
		Network: network, Buffer: buffer, Sender: p.sender,
		Text: truncatePushText(stripCodes(p.text)), TS: p.ts,
		MsgID: p.msgid, Count: p.count, Channel: p.channelLike,
	})
	if err != nil {
		return
	}
	// Belt and braces below the per-field clamps: an over-limit payload
	// must be skipped HERE, once — Encrypt would reject it per
	// subscription, and that failure must never look prune-worthy.
	if len(payload) > webpush.MaxPlaintext {
		log.Printf("push: payload for %s/%s is %d bytes (limit %d), skipping", network, buffer, len(payload), webpush.MaxPlaintext)
		return
	}
	for _, sub := range subs {
		h.pushToSubscription(ctx, sender, sub, payload)
	}
}

// pushToSubscription encrypts and sends one payload to one endpoint,
// pruning subscriptions that can never work again (undecodable keys,
// push-service 404/410).
func (h *Hub) pushToSubscription(ctx context.Context, sender PushSender, sub store.PushSubscription, payload []byte) {
	p256dh, err1 := base64.RawURLEncoding.DecodeString(sub.P256dh)
	auth, err2 := base64.RawURLEncoding.DecodeString(sub.Auth)
	if err1 != nil || err2 != nil {
		// Unusable keys can never decrypt anything: prune the row.
		h.prunePushSubscription(ctx, sub.Endpoint, errors.New("stored keys undecodable"))
		return
	}
	body, err := webpush.Encrypt(payload, p256dh, auth)
	if err != nil {
		// Prune ONLY key-caused failures: any other Encrypt error (an
		// oversized payload, however it slipped past the caller's size
		// guard) must not delete every registration in one pass.
		if errors.Is(err, webpush.ErrBadKeys) {
			h.prunePushSubscription(ctx, sub.Endpoint, err)
		} else {
			log.Printf("push: encrypting for %s: %v", sub.Endpoint, err)
		}
		return
	}
	switch err := sender.Send(ctx, webpush.Subscription{Endpoint: sub.Endpoint, P256dh: p256dh, Auth: auth}, body, pushTTLSeconds); {
	case err == nil:
		if err := h.store.TouchPushSuccess(ctx, sub.Endpoint, time.Now()); err != nil {
			log.Printf("push: recording success: %v", err)
		}
	case errors.Is(err, webpush.ErrGone):
		h.prunePushSubscription(ctx, sub.Endpoint, err)
	default:
		// Transient (network, 5xx, 429): no retry — the push service
		// owns redelivery, and the next highlight tries again.
		log.Printf("push: delivering to %s: %v", sub.Endpoint, err)
	}
}

func (h *Hub) prunePushSubscription(ctx context.Context, endpoint string, cause error) {
	log.Printf("push: dropping subscription %s: %v", endpoint, cause)
	if err := h.store.DeletePushSubscription(ctx, endpoint); err != nil {
		log.Printf("push: dropping subscription: %v", err)
		return
	}
	h.RefreshPushCount(ctx)
}

// truncatePushText caps the notification preview, cutting on a rune
// boundary so a multibyte character is dropped whole, never split.
func truncatePushText(s string) string {
	if len(s) <= maxPushTextBytes {
		return s
	}
	i := maxPushTextBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
