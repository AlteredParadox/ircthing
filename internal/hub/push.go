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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/url"
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
	nick        string // our nick when admitted (for re-evaluating rules changes)
	ts          int64  // first highlight's time (shown time)
	newestTS    int64  // newest coalesced highlight (cancel threshold)
	count       int
	channelLike bool
	rulesGen    uint64 // rules generation at admit; a change suppresses an un-re-evaluable (scrubbed) channel job
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
	// Store the new key AND wipe subscriptions in ONE transaction:
	// anything bound to a retired key is cryptographically dead (the push
	// service rejects the new VAPID signature 401/403 — which never
	// prunes, only 404/410 do — so it fails forever and squats the cap).
	// On first-ever generation there are no subscriptions, so the wipe is
	// a harmless no-op. Failing here fails startup — a half-rotated
	// credential is worse than not starting.
	if err := h.store.SetSettingAndWipePushSubscriptions(ctx, vapidKeyKey, marshaled); err != nil {
		return nil, err
	}
	h.BumpPushEpoch() // no in-flight jobs at startup, but keep the invariant uniform
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

// PushEpoch returns the current delivery-credential epoch — exported so
// tests can assert a rotation or logout advanced it.
func (h *Hub) PushEpoch() uint64 { return h.pushEpoch.Load() }

// BumpPushEpoch invalidates every in-flight delivery — called
// synchronously with a subscription wipe (password rotation, VAPID
// replacement). It advances the epoch (so a worker aborts before its
// next send) AND cancels the current delivery-generation context (so a
// send already in flight to a now-revoked endpoint aborts instead of
// running out its timeout).
func (h *Hub) BumpPushEpoch() {
	h.pushEpoch.Add(1)
	h.pushGenMu.Lock()
	defer h.pushGenMu.Unlock()
	if h.pushGenCancel != nil {
		h.pushGenCancel()
	}
	if h.pushGenBase != nil {
		h.pushGenCtx, h.pushGenCancel = context.WithCancel(h.pushGenBase)
	}
}

// initPushGen installs the first delivery-generation context, a child of
// the pusher's root context. Called once from runPusher.
func (h *Hub) initPushGen(ctx context.Context) {
	h.pushGenMu.Lock()
	defer h.pushGenMu.Unlock()
	h.pushGenBase = ctx
	h.pushGenCtx, h.pushGenCancel = context.WithCancel(ctx)
}

// currentPushGen returns the live delivery-generation context. A send
// uses it so a concurrent wipe (BumpPushEpoch) cancels it mid-flight.
func (h *Hub) currentPushGen() context.Context {
	h.pushGenMu.Lock()
	defer h.pushGenMu.Unlock()
	if h.pushGenCtx == nil {
		return context.Background()
	}
	return h.pushGenCtx
}

// RefreshPushCount re-reads the subscription count into the atomic the
// per-message fast path checks. Called at startup, by the subscribe/
// unsubscribe endpoints, and by the pusher's prune path. A failed
// refresh keeps the previous value and is LOGGED — a silently stale 0
// would disable every push. pushCountMu makes each read+write atomic so
// a slow refresh cannot overwrite a newer one with its older count.
func (h *Hub) RefreshPushCount(ctx context.Context) {
	h.pushCountMu.Lock()
	defer h.pushCountMu.Unlock()
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

// Fixed delivery workers consume a bounded job channel. FIXED, not
// spawn-per-job: a spawn-per-due-entry model bounds only the ACTIVE
// sends (a semaphore) while waiting goroutines pile up unbounded — with
// slow endpoints and buffers re-scheduling, that grows until OOM.
const (
	maxConcurrentDeliveries = 4
	pushJobQueue            = maxPendingPushes // bounded; a full queue drops (best effort)
)

// pushJob is one buffer's due notification, handed to a worker. *p is
// owned exclusively once removed from the pending map.
type pushJob struct {
	key string
	p   *pendingPush
}

// runPusher is the scheduler loop; sole owner of pending-push state and
// the rules/filters caches. Delivery runs on a FIXED worker pool reading
// a bounded channel, so a slow push service can neither stall the loop
// (marker cancels, redaction scrubs, mute sweeps stay responsive) nor
// leak goroutines. Every store/sender access from a worker is
// mutex-guarded, so off-loop delivery is safe.
func (h *Hub) runPusher(ctx context.Context, sender PushSender, delay time.Duration) {
	h.initPushGen(ctx)
	rules, filters, filtersOK := h.reloadPushConfig(ctx, nil, pushFilters{}, false)
	pending := make(map[string]*pendingPush) // network+"\x00"+buffer
	timer := time.NewTimer(time.Hour)
	timer.Stop()

	jobs := make(chan pushJob, pushJobQueue)
	var workers sync.WaitGroup
	for range maxConcurrentDeliveries {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-jobs:
					h.deliverPush(ctx, sender, job.key, job.p)
				}
			}
		}()
	}
	// Workers drain before runPusher returns, which (via the process
	// WaitGroup) precedes store close — no store use after close.
	defer workers.Wait()

	for {
		select {
		case <-ctx.Done():
			return // pending pushes die with the process: best effort by design

		case c := <-h.pushCandidates:
			if admitCandidate(pending, c, rules, filters, filtersOK, delay, h.pushRulesGen.Load()) {
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
			rules, filters, filtersOK = h.reloadPushConfig(ctx, rules, filters, filtersOK)
			// Advance the rules generation so an already-handed-off channel
			// job whose headline was scrubbed (can't be re-evaluated
			// against the new rules) is suppressed at delivery.
			h.pushRulesGen.Add(1)
			// Muting a buffer, ignoring its latest pinger, or deleting a
			// (half-typed) keyword mid-window is exactly when the user
			// expects silence: re-apply the new config to what is
			// already scheduled, not just to future candidates.
			if sweepFilteredPending(pending, rules, filters) {
				rearmPushTimer(timer, pending)
			}

		case <-timer.C:
			enqueueDuePushes(pending, jobs)
			rearmPushTimer(timer, pending)
		}
	}
}

// reloadPushConfig loads the rules and filters, KEEPING the previous
// values when a stored blob is unreadable: a corrupt filters blob
// silently becoming "nothing filtered" would push exactly the content
// the user suppressed. Returns filtersOK = whether the filter policy is
// TRUSTWORTHY (loaded or legitimately absent): while false — an
// unreadable blob with no prior good value, e.g. a first-load failure —
// the caller suppresses ALL candidates (fail closed). prevOK carries the
// last trustworthy state forward.
func (h *Hub) reloadPushConfig(ctx context.Context, prevRules []Rule, prevFilters pushFilters, prevOK bool) ([]Rule, pushFilters, bool) {
	rules := prevRules
	if r, ok := h.loadRulesChecked(ctx); ok {
		rules = r
	} else {
		log.Printf("push: keeping previous highlight rules (stored blob unreadable)")
	}
	filters := prevFilters
	filtersOK := prevOK
	if f, ok := h.loadFiltersChecked(ctx); ok {
		filters = buildPushFilters(f)
		filtersOK = true
	} else if prevOK {
		log.Printf("push: keeping previous ignore/mute filters (stored blob unreadable)")
	} else {
		log.Printf("push: ignore/mute filters unreadable and never loaded — suppressing all pushes until they load")
	}
	return rules, filters, filtersOK
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

// sweepFilteredPending drops pending pushes the just-reloaded config
// now excludes; reports whether anything changed. Only the FIRST
// coalesced message is known per entry — good enough: that is what the
// user just reacted to (the mid-window mute/ignore, or deleting the
// half-typed keyword that spuriously matched).
func sweepFilteredPending(pending map[string]*pendingPush, rules []Rule, filters pushFilters) bool {
	changed := false
	for key, p := range pending {
		network, buffer, _ := strings.Cut(key, "\x00")
		if filters.drops(network, buffer, p.sender) {
			delete(pending, key)
			changed = true
			continue
		}
		// A sole channel highlight whose headline no longer matches any
		// rule or mention was admitted by a since-changed rule set (a
		// keyword mid-edit, a deleted rule). Coalesced entries stay:
		// later siblings may have matched independently.
		if p.channelLike && p.count == 1 && p.text != "" &&
			!highlightText(p.text, p.nick, rules, network) {
			delete(pending, key)
			changed = true
		}
	}
	return changed
}

// admitCandidate applies the filter and highlight policy to one live
// message and schedules (or coalesces) its push. Reports whether a NEW
// entry was scheduled, i.e. the timer needs rearming.
func admitCandidate(pending map[string]*pendingPush, c pushCandidate, rules []Rule, filters pushFilters, filtersOK bool, delay time.Duration, rulesGen uint64) bool {
	// Fail closed while the filter policy is untrustworthy (a corrupt
	// blob that never loaded): pushing here could leak content the user
	// suppressed. Recovers on the next successful config reload.
	if !filtersOK {
		return false
	}
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
		log.Printf("push: pending cap reached, dropping highlight in %s/%.60q", c.network, c.buffer)
		return false
	}
	pending[key] = &pendingPush{
		fireAt: time.Now().Add(delay),
		sender: c.sender, text: c.text, msgid: c.msgid, nick: c.nick,
		ts: c.ts, newestTS: c.ts, count: 1, channelLike: c.channelLike,
		rulesGen: rulesGen,
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

// enqueueDuePushes hands every due pending push to the worker queue.
// Deleting the entry FIRST transfers exclusive ownership of *p to its
// job; the scheduler loop never touches it again. A full queue drops the
// job (best effort — the queue is already backed up on slow endpoints),
// logged rather than silent.
//
// A key removed here can be re-scheduled (a new pending entry) while its
// job is still in flight, so two jobs for one buffer can coexist. We do
// NOT track in-flight keys to coalesce them: delivery is BOUNDED
// BEST-EFFORT — the service worker tags notifications by buffer
// (network+"\n"+buffer), so a second delivery REPLACES the first on the
// device rather than stacking; the queue is bounded (drops at cap); and
// per-send revalidation aborts stale sends. The residual is that an
// older in-flight notification can replace a newer one for the same
// buffer, or a slow buffer can occupy a worker slot; a scheduler-owned
// in-flight set + completion channel would tighten ordering/fairness but
// adds real complexity for marginal gain at single-user scale.
func enqueueDuePushes(pending map[string]*pendingPush, jobs chan pushJob) {
	now := time.Now()
	for key, p := range pending {
		if p.fireAt.After(now) {
			continue
		}
		delete(pending, key)
		select {
		case jobs <- pushJob{key: key, p: p}:
		default:
			network, buffer, _ := strings.Cut(key, "\x00")
			log.Printf("push: delivery queue full, dropping notification for %s/%.60q", network, buffer)
		}
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

// pushStillDue runs the authoritative fire-time re-checks against the
// store — the cancel channels are all best-effort, so the store has the
// final word. It reports whether the push should still go out, and may
// SCRUB p's headline (coalesced redaction) as a side effect.
func (h *Hub) pushStillDue(ctx context.Context, network, buffer string, p *pendingPush) bool {
	// Read marker: covers marker writes whose channel send was dropped
	// AND upstream MARKREAD from other bouncer clients. A read ERROR
	// fails open (deliver): a possibly-already-read notification is an
	// annoyance, an eaten one defeats the feature — but log it.
	if t, err := h.store.ReadMarker(ctx, network, buffer); err != nil {
		log.Printf("push: read-marker re-check for %.60q: %v", buffer, err)
	} else if !t.IsZero() && t.UnixMilli() >= p.newestTS {
		return false
	}
	// The buffer may have been purged OR archived after scheduling. A
	// buffer the store no longer knows gets no push (its notification
	// would navigate nowhere); an archived one is hidden from every
	// sidebar and must stay silent too, mirroring persistEvent's
	// stillArchived suppression. `buffer` is already the canonical
	// stored spelling, so the exact-name lookup is faithful.
	found, archived, err := h.store.BufferState(ctx, network, buffer)
	if err != nil {
		// Fail closed (skip the push) but never silently: a transient
		// store error here eats a notification.
		log.Printf("push: checking buffer %s/%.60q: %v", network, buffer, err)
		return false
	}
	if !found || archived {
		return false
	}
	// Redaction of the headlining message: a sole redacted message has
	// nothing left to say; coalesced siblings deliver, scrubbed. Unlike
	// the marker check this fails CLOSED on error — indeterminate
	// redaction state must not leak content the store may have
	// destroyed; the cost is one eaten notification on a transient
	// store error.
	if p.msgid != "" {
		redacted, err := h.store.MessageRedacted(ctx, network, buffer, p.msgid)
		if err != nil {
			log.Printf("push: redaction re-check for %.60q: %v (suppressing)", buffer, err)
			return false
		}
		if redacted {
			if p.count == 1 {
				return false
			}
			p.count--
			p.sender, p.text, p.msgid = "", "", ""
		}
	}
	return true
}

// deliverPush sends one due notification to every subscription. Runs on
// a worker goroutine, so it re-validates against the AUTHORITATIVE store
// before EACH endpoint send: a delivery can take up to ~15s per
// subscription, and a read, redaction, close, mute, ignore, or
// credential rotation arriving mid-delivery must stop (or scrub) the
// remaining sends. Every check is a store read — safe off the scheduler
// loop.
func (h *Hub) deliverPush(ctx context.Context, sender PushSender, key string, p *pendingPush) {
	network, buffer, _ := strings.Cut(key, "\x00")
	// Capture the credential epoch with the subscription slice: if a
	// rotation wipes subscriptions after this load, the epoch advances
	// and the per-send check below aborts — so this slice's (now stale,
	// possibly attacker-planted) endpoints are never sent to.
	epoch := h.pushEpoch.Load()
	subs, err := h.store.PushSubscriptions(ctx)
	if err != nil {
		log.Printf("push: loading subscriptions for %s/%.60q: %v", network, buffer, err)
		return
	}
	// The generation context (captured once) is canceled by a wipe, so an
	// in-flight Send to a revoked endpoint aborts rather than completing.
	genCtx := h.currentPushGen()
	for _, sub := range subs {
		if h.pushEpoch.Load() != epoch || genCtx.Err() != nil {
			return // subscriptions wiped since we loaded the slice
		}
		if !h.deliveryStillAllowed(ctx, network, buffer, p) {
			return
		}
		// Build the payload PER SEND from the current *p: pushStillDue may
		// have scrubbed a coalesced-redacted headline since the last one.
		payload, err := json.Marshal(pushPayload{
			Network: network, Buffer: buffer, Sender: p.sender,
			Text: truncatePushText(stripCodes(p.text)), TS: p.ts,
			MsgID: p.msgid, Count: p.count, Channel: p.channelLike,
		})
		if err != nil {
			return
		}
		// An over-limit payload must be skipped, never pruned: Encrypt
		// would reject it per subscription and that must not look
		// prune-worthy.
		if len(payload) > webpush.MaxPlaintext {
			log.Printf("push: payload for %s/%.60q is %d bytes (limit %d), skipping", network, buffer, len(payload), webpush.MaxPlaintext)
			return
		}
		h.pushToSubscription(genCtx, sender, sub, payload, epoch)
	}
}

// deliveryStillAllowed re-runs every authoritative privacy/liveness gate
// immediately before a send. All store reads: read marker, buffer
// existence/archive, headline redaction (pushStillDue), a FRESH filter
// re-check (a mute/ignore during the delivery), and the subscription
// count (a credential rotation wiped them). Fail closed on an
// unreadable filter policy.
func (h *Hub) deliveryStillAllowed(ctx context.Context, network, buffer string, p *pendingPush) bool {
	if h.pushSubs.Load() == 0 {
		return false // rotation/prune emptied the table mid-delivery
	}
	if !h.pushStillDue(ctx, network, buffer, p) {
		return false
	}
	f, ok := h.loadFiltersChecked(ctx)
	if !ok {
		return false // indeterminate filter policy: suppress
	}
	if buildPushFilters(f).drops(network, buffer, p.sender) {
		return false
	}
	// A channel highlight whose keyword was removed while the job waited
	// must not still send. A scrubbed headline (coalesced then redacted,
	// text == "") can't be re-evaluated: if the rules changed since it
	// was admitted, suppress it rather than emit a generic metadata-only
	// notification for a buffer that may no longer match any rule.
	if p.channelLike && p.text == "" && p.rulesGen != h.pushRulesGen.Load() {
		return false
	}
	if p.channelLike && p.text != "" {
		rules, rok := h.loadRulesChecked(ctx)
		if !rok {
			return false // indeterminate rules: suppress
		}
		if !highlightText(p.text, p.nick, rules, network) {
			return false
		}
	}
	return true
}

// pushToSubscription encrypts and sends one payload to one endpoint,
// pruning subscriptions that can never work again (undecodable keys,
// push-service 404/410). Pruning is GATED on the capture epoch: if a
// wipe advanced the epoch since the slice was loaded, this row is stale
// (already deleted, or a replacement re-registered under the same
// endpoint) — a 410/bad-keys verdict on the stale snapshot must not
// delete the live row.
func (h *Hub) pushToSubscription(ctx context.Context, sender PushSender, sub store.PushSubscription, payload []byte, epoch uint64) {
	prune := func(cause error) {
		if h.pushEpoch.Load() == epoch {
			h.prunePushSubscription(ctx, sub.Endpoint, cause)
		}
	}
	p256dh, err1 := base64.RawURLEncoding.DecodeString(sub.P256dh)
	auth, err2 := base64.RawURLEncoding.DecodeString(sub.Auth)
	if err1 != nil || err2 != nil {
		// Unusable keys can never decrypt anything: prune the row.
		prune(errors.New("stored keys undecodable"))
		return
	}
	body, err := webpush.Encrypt(payload, p256dh, auth)
	if err != nil {
		// Prune ONLY key-caused failures: any other Encrypt error (an
		// oversized payload, however it slipped past the caller's size
		// guard) must not delete every registration in one pass.
		if errors.Is(err, webpush.ErrBadKeys) {
			prune(err)
		} else {
			log.Printf("push: encrypting for %s: %v", redactEndpoint(sub.Endpoint), err)
		}
		return
	}
	switch err := sender.Send(ctx, webpush.Subscription{Endpoint: sub.Endpoint, P256dh: p256dh, Auth: auth}, body, pushTTLSeconds); {
	case err == nil:
		if err := h.store.TouchPushSuccess(context.Background(), sub.Endpoint, time.Now()); err != nil {
			log.Printf("push: recording success: %v", err)
		}
	case errors.Is(err, webpush.ErrGone):
		prune(err)
	default:
		// Transient (network, 5xx, 429, or a generation-cancel abort): no
		// retry — the push service owns redelivery, and the next highlight
		// tries again.
		log.Printf("push: delivering to %s: %s", redactEndpoint(sub.Endpoint), scrubEndpoint(err, sub.Endpoint))
	}
}

// redactEndpoint reduces a push endpoint to origin + a short digest for
// logging: the path is a capability URL (holding it plus the VAPID key
// delivers to the device), and full endpoints also ride inside wrapped
// url.Error strings — scrub those too via scrubEndpoint.
func redactEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "endpoint(unparseable)"
	}
	sum := sha256.Sum256([]byte(endpoint))
	return u.Scheme + "://" + u.Host + "/#" + hex.EncodeToString(sum[:4])
}

// scrubEndpoint removes the raw endpoint from an error's text (url.Error
// embeds the full request URL).
func scrubEndpoint(err error, endpoint string) string {
	return strings.ReplaceAll(err.Error(), endpoint, "<endpoint>")
}

func (h *Hub) prunePushSubscription(ctx context.Context, endpoint string, cause error) {
	log.Printf("push: dropping subscription %s: %s", redactEndpoint(endpoint), scrubEndpoint(cause, endpoint))
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
