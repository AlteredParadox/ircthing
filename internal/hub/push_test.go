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
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"testing"
	"time"

	"reflect"

	"ircthing/internal/irc"
	"ircthing/internal/store"
	"ircthing/internal/webpush"

	ircv4 "gopkg.in/irc.v4"
)

// pusher tests use the project's real-time test style (no fake clock in
// hub tests): a ~30ms delay and generous waits.
const testPushDelay = 30 * time.Millisecond

type fakeSender struct {
	mu    sync.Mutex
	calls []webpush.Subscription
	err   error
}

func (f *fakeSender) Send(_ context.Context, sub webpush.Subscription, _ []byte, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sub)
	return f.err
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// addTestSubscription stores a subscription with a REAL P-256 key so
// deliverPush's encryption succeeds.
func addTestSubscription(t *testing.T, h *Hub, endpoint string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	err = h.store.UpsertPushSubscription(context.Background(), store.PushSubscription{
		Endpoint: endpoint,
		P256dh:   base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// seedBuffer creates the buffer a candidate targets: deliverPush skips
// buffers the store does not know (they were purged), and every real
// candidate follows a successful append anyway.
func seedBuffer(t *testing.T, h *Hub, network, buffer string) {
	t.Helper()
	_, err := h.store.Append(context.Background(), network, buffer, store.Message{
		Time: time.Now(), Sender: "seed", Command: "PRIVMSG", Text: "seed",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func startTestPusher(t *testing.T, h *Hub, sender PushSender) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	h.startPusher(ctx, &wg, sender, testPushDelay)
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

// waitDrained blocks until the pusher goroutine has consumed everything
// queued on ch (select order between the pusher's channels is random, so
// tests sequence causally-ordered sends by waiting for consumption).
func waitDrained[T any](t *testing.T, ch chan T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for len(ch) > 0 {
		select {
		case <-deadline:
			t.Fatal("pusher never drained the channel")
		case <-time.After(time.Millisecond):
		}
	}
}

func candidate(buffer, sender, text string, ts int64, channelLike bool) pushCandidate {
	return pushCandidate{
		network: "libera", buffer: buffer, sender: sender, text: text,
		nick: "me", ts: ts, channelLike: channelLike,
	}
}

// waitSends polls until the sender saw want calls (then holds a beat to
// catch overshoot) or times out.
func waitSends(t *testing.T, f *fakeSender, want int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for f.count() < want {
		select {
		case <-deadline:
			t.Fatalf("sends = %d, want %d", f.count(), want)
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(5 * testPushDelay)
	if got := f.count(); got != want {
		t.Fatalf("sends = %d, want exactly %d", got, want)
	}
}

func TestPusherFiresAndCoalesces(t *testing.T) {
	h := newTestHub(t)
	addTestSubscription(t, h, "https://push.example/dev1")
	seedBuffer(t, h, "libera", "alice")
	f := &fakeSender{}
	startTestPusher(t, h, f)

	// A PM always pushes; two in the same buffer coalesce to ONE send.
	now := time.Now().UnixMilli()
	h.pushCandidates <- candidate("alice", "alice", "hi", now, false)
	h.pushCandidates <- candidate("alice", "alice", "you there?", now+1, false)
	waitSends(t, f, 1)
	if f.calls[0].Endpoint != "https://push.example/dev1" {
		t.Fatalf("endpoint = %q", f.calls[0].Endpoint)
	}
}

func TestPusherChannelNeedsHighlight(t *testing.T) {
	h := newTestHub(t)
	addTestSubscription(t, h, "https://push.example/dev1")
	seedBuffer(t, h, "libera", "#go")
	f := &fakeSender{}
	startTestPusher(t, h, f)

	now := time.Now().UnixMilli()
	// Channel chatter without a mention: no push.
	h.pushCandidates <- candidate("#go", "bob", "unrelated chatter", now, true)
	// A mention pushes.
	h.pushCandidates <- candidate("#go", "bob", "me: ping", now+1, true)
	waitSends(t, f, 1)
}

func TestPusherKeywordRulesApply(t *testing.T) {
	h := newTestHub(t)
	ctx := context.Background()
	if err := h.store.SetSetting(ctx, rulesKey, `{"rules":[{"pattern":"deploy","network":"","id":"r1"}]}`); err != nil {
		t.Fatal(err)
	}
	addTestSubscription(t, h, "https://push.example/dev1")
	seedBuffer(t, h, "libera", "#go")
	f := &fakeSender{}
	startTestPusher(t, h, f)

	h.pushCandidates <- candidate("#go", "bob", "time to deploy", time.Now().UnixMilli(), true)
	waitSends(t, f, 1)

	// Removing the rule (set_rules path pokes notifyPushConfigChanged)
	// reloads the cache: the same text no longer pushes.
	if err := h.store.SetSetting(ctx, rulesKey, `{"rules":[]}`); err != nil {
		t.Fatal(err)
	}
	h.notifyPushConfigChanged()
	waitDrained(t, h.pushConfigDirty)
	h.pushCandidates <- candidate("#go", "bob", "time to deploy", time.Now().UnixMilli(), true)
	time.Sleep(5 * testPushDelay)
	if got := f.count(); got != 1 {
		t.Fatalf("sends after rule removal = %d, want 1", got)
	}
}

func TestPusherHonorsFilters(t *testing.T) {
	h := newTestHub(t)
	ctx := context.Background()
	// bob ignored on libera; #noisy muted (client bufKey form).
	if err := h.store.SetSetting(ctx, filtersKey,
		`{"ignores":{"libera":["bob"]},"mutes":["libera`+"\\n"+`#noisy"]}`); err != nil {
		t.Fatal(err)
	}
	addTestSubscription(t, h, "https://push.example/dev1")
	seedBuffer(t, h, "libera", "bob")
	seedBuffer(t, h, "libera", "#noisy")
	seedBuffer(t, h, "libera", "#go")
	f := &fakeSender{}
	startTestPusher(t, h, f)

	now := time.Now().UnixMilli()
	// A PM from an ignored sender: dropped (PMs otherwise always push).
	h.pushCandidates <- candidate("bob", "Bob", "hi", now, false) // case-insensitive ignore
	// A mention in a muted buffer: dropped.
	h.pushCandidates <- candidate("#noisy", "carol", "me: ping", now+1, true)
	// A mention from the ignored sender in another channel: dropped.
	h.pushCandidates <- candidate("#go", "bob", "me: ping", now+2, true)
	// Control: same shape from an unfiltered sender pushes.
	h.pushCandidates <- candidate("#go", "carol", "me: ping", now+3, true)
	waitSends(t, f, 1)

	// Clearing the filters (set_filters path pokes the config-dirty
	// channel) makes the ignored sender push again.
	if err := h.store.SetSetting(ctx, filtersKey, `{"ignores":{},"mutes":[]}`); err != nil {
		t.Fatal(err)
	}
	h.notifyPushConfigChanged()
	waitDrained(t, h.pushConfigDirty)
	h.pushCandidates <- candidate("bob", "bob", "hi again", now+4, false)
	waitSends(t, f, 2)
}

func TestPusherCancelOnRead(t *testing.T) {
	h := newTestHub(t)
	addTestSubscription(t, h, "https://push.example/dev1")
	seedBuffer(t, h, "libera", "alice")
	f := &fakeSender{}
	startTestPusher(t, h, f)

	now := time.Now().UnixMilli()
	h.pushCandidates <- candidate("alice", "alice", "hi", now, false)
	waitDrained(t, h.pushCandidates) // candidate scheduled before the marker lands
	// Reading AT the newest message cancels.
	h.notifyMarkerAdvance("libera", "alice", time.UnixMilli(now))
	time.Sleep(5 * testPushDelay)
	if got := f.count(); got != 0 {
		t.Fatalf("sends after cancel = %d, want 0", got)
	}

	// A marker BEFORE the newest highlight does not cancel.
	h.pushCandidates <- candidate("alice", "alice", "again", now+100, false)
	waitDrained(t, h.pushCandidates)
	h.notifyMarkerAdvance("libera", "alice", time.UnixMilli(now+50))
	waitSends(t, f, 1)
}

// TestPusherFireTimeRecheck: a marker that reached the STORE but whose
// channel notification was dropped still cancels, via the authoritative
// re-check at fire time.
func TestPusherFireTimeRecheck(t *testing.T) {
	h := newTestHub(t)
	addTestSubscription(t, h, "https://push.example/dev1")
	f := &fakeSender{}
	startTestPusher(t, h, f)

	// SetReadMarker never creates a buffer, so give it one to mark.
	ctx := context.Background()
	if _, err := h.store.Append(ctx, "libera", "alice", store.Message{
		Time: time.Now(), Sender: "alice", Command: "PRIVMSG", Text: "hi",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	h.pushCandidates <- candidate("alice", "alice", "hi", now, false)
	if err := h.store.SetReadMarker(ctx, "libera", "alice", time.UnixMilli(now)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * testPushDelay)
	if got := f.count(); got != 0 {
		t.Fatalf("sends = %d, want 0 (store re-check)", got)
	}
}

func TestPusherPrunesGoneSubscription(t *testing.T) {
	h := newTestHub(t)
	addTestSubscription(t, h, "https://push.example/dead")
	seedBuffer(t, h, "libera", "alice")
	h.RefreshPushCount(context.Background())
	if h.pushSubs.Load() != 1 {
		t.Fatalf("seeded count = %d", h.pushSubs.Load())
	}
	f := &fakeSender{err: fmt.Errorf("wrapped: %w", webpush.ErrGone)}
	startTestPusher(t, h, f)

	h.pushCandidates <- candidate("alice", "alice", "hi", time.Now().UnixMilli(), false)
	waitSends(t, f, 1)
	deadline := time.After(5 * time.Second)
	for h.pushSubs.Load() != 0 {
		select {
		case <-deadline:
			t.Fatalf("count after prune = %d, want 0", h.pushSubs.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if n, _ := h.store.CountPushSubscriptions(context.Background()); n != 0 {
		t.Fatalf("stored subscriptions after prune = %d", n)
	}
}

func TestPusherCancelOnBufferCloseAndNetworkRemoval(t *testing.T) {
	h := newTestHub(t)
	addTestSubscription(t, h, "https://push.example/dev1")
	seedBuffer(t, h, "libera", "alice")
	seedBuffer(t, h, "libera", "#go")
	f := &fakeSender{}
	startTestPusher(t, h, f)

	now := time.Now().UnixMilli()
	// Buffer close cancels that buffer's pending push.
	h.pushCandidates <- candidate("alice", "alice", "hi", now, false)
	waitDrained(t, h.pushCandidates)
	h.notifyPushCancel("libera", "alice", "")
	time.Sleep(5 * testPushDelay)
	if got := f.count(); got != 0 {
		t.Fatalf("sends after buffer close = %d, want 0", got)
	}

	// Network removal cancels every pending push on the network.
	h.pushCandidates <- candidate("alice", "alice", "hi", now+1, false)
	h.pushCandidates <- candidate("#go", "bob", "me: ping", now+2, true)
	waitDrained(t, h.pushCandidates)
	h.notifyPushCancel("libera", "", "")
	time.Sleep(5 * testPushDelay)
	if got := f.count(); got != 0 {
		t.Fatalf("sends after network removal = %d, want 0", got)
	}
}

func TestPusherRedactionScrubsPending(t *testing.T) {
	h := newTestHub(t)
	addTestSubscription(t, h, "https://push.example/dev1")
	seedBuffer(t, h, "libera", "alice")
	f := &fakeSender{}
	startTestPusher(t, h, f)

	// Sole pending message redacted: the push is dropped entirely.
	now := time.Now().UnixMilli()
	c := candidate("alice", "alice", "my password is hunter2", now, false)
	c.msgid = "m1"
	h.pushCandidates <- c
	waitDrained(t, h.pushCandidates)
	h.notifyPushCancel("libera", "alice", "m1")
	time.Sleep(5 * testPushDelay)
	if got := f.count(); got != 0 {
		t.Fatalf("sends after redaction = %d, want 0", got)
	}

	// With a coalesced sibling the notification survives (scrubbed).
	c2 := candidate("alice", "alice", "secret", now+1, false)
	c2.msgid = "m2"
	h.pushCandidates <- c2
	h.pushCandidates <- candidate("alice", "alice", "and a follow-up", now+2, false)
	waitDrained(t, h.pushCandidates)
	h.notifyPushCancel("libera", "alice", "m2")
	waitSends(t, f, 1)
}

// TestPusherFireTimeRedactionRecheck: even when the redaction CANCEL is
// lost (dropped channel send, select ordering), the authoritative store
// re-check at fire time stops redacted text from reaching a
// notification tray.
func TestPusherFireTimeRedactionRecheck(t *testing.T) {
	h := newTestHub(t)
	ctx := context.Background()
	addTestSubscription(t, h, "https://push.example/dev1")
	stored, err := h.store.Append(ctx, "libera", "alice", store.Message{
		Time: time.Now(), Sender: "alice", Command: "PRIVMSG",
		Text: "my password is hunter2", MsgID: "m1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.MsgID != "m1" {
		t.Fatalf("stored msgid = %q", stored.MsgID)
	}
	f := &fakeSender{}
	startTestPusher(t, h, f)

	c := candidate("alice", "alice", "my password is hunter2", stored.Time.UnixMilli(), false)
	c.msgid = "m1"
	h.pushCandidates <- c
	waitDrained(t, h.pushCandidates)
	// Redact in the STORE only — no pushCancel — simulating a lost send.
	if ok, err := h.store.SetRedacted(ctx, "libera", "alice", "m1", "oops"); err != nil || !ok {
		t.Fatalf("SetRedacted = %v, %v", ok, err)
	}
	time.Sleep(5 * testPushDelay)
	if got := f.count(); got != 0 {
		t.Fatalf("sends for redacted message = %d, want 0", got)
	}
}

func TestRenameSyncedNetworkRefs(t *testing.T) {
	h := newTestHub(t)
	ctx := context.Background()
	if err := h.store.SetSetting(ctx, rulesKey,
		`{"rules":[{"pattern":"release","network":"libera","id":"r1"},{"pattern":"deploy","network":"","id":"r2"}]}`); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(ctx, filtersKey,
		`{"ignores":{"libera":["troll"],"oftc":["bob"]},"mutes":["libera`+"\\n"+`#noisy","oftc`+"\\n"+`#other"]}`); err != nil {
		t.Fatal(err)
	}

	h.renameSyncedNetworkRefs(ctx, "libera", "libera-chat")

	rules := h.loadRules(ctx)
	if rules[0].Network != "libera-chat" || rules[1].Network != "" {
		t.Fatalf("rules after rename = %+v", rules)
	}
	d := h.loadFilters(ctx)
	if len(d.Ignores["libera-chat"]) != 1 || d.Ignores["libera-chat"][0] != "troll" || len(d.Ignores["libera"]) != 0 {
		t.Fatalf("ignores after rename = %+v", d.Ignores)
	}
	want := []string{"libera-chat\n#noisy", "oftc\n#other"}
	if !reflect.DeepEqual(d.Mutes, want) {
		t.Fatalf("mutes after rename = %+v", d.Mutes)
	}
	// A rename that touches nothing writes nothing (no spurious broadcasts).
	before, _ := h.store.Setting(ctx, filtersKey)
	h.renameSyncedNetworkRefs(ctx, "nonesuch", "other")
	after, _ := h.store.Setting(ctx, filtersKey)
	if before != after {
		t.Fatal("no-op rename rewrote the filters blob")
	}
}

// TestPusherSkipsPurgedBuffer: the fire-time existence check backstops a
// dropped close/delete cancel — a buffer the store no longer knows gets
// no push.
func TestPusherSkipsPurgedBuffer(t *testing.T) {
	h := newTestHub(t)
	addTestSubscription(t, h, "https://push.example/dev1")
	f := &fakeSender{}
	startTestPusher(t, h, f)

	// Never seeded: the store has no such buffer (as after a purge).
	h.pushCandidates <- candidate("ghost", "alice", "hi", time.Now().UnixMilli(), false)
	time.Sleep(5 * testPushDelay)
	if got := f.count(); got != 0 {
		t.Fatalf("sends for purged buffer = %d, want 0", got)
	}
}

func TestPusherMuteMidWindowCancels(t *testing.T) {
	h := newTestHub(t)
	ctx := context.Background()
	addTestSubscription(t, h, "https://push.example/dev1")
	seedBuffer(t, h, "libera", "#noisy")
	f := &fakeSender{}
	startTestPusher(t, h, f)

	h.pushCandidates <- candidate("#noisy", "bob", "me: ping", time.Now().UnixMilli(), true)
	waitDrained(t, h.pushCandidates)
	// Muting the buffer during the window sweeps the pending push.
	if err := h.store.SetSetting(ctx, filtersKey, `{"ignores":{},"mutes":["libera`+"\\n"+`#noisy"]}`); err != nil {
		t.Fatal(err)
	}
	h.notifyPushConfigChanged()
	time.Sleep(5 * testPushDelay)
	if got := f.count(); got != 0 {
		t.Fatalf("sends after mid-window mute = %d, want 0", got)
	}
}

// TestNoCandidatesWithoutSubscriptions proves the zero-subscription fast
// path: persistEvent must not even enqueue a candidate.
func TestNoCandidatesWithoutSubscriptions(t *testing.T) {
	h := newTestHub(t)
	c := &fakeConn{name: "libera", nick: "me"}
	ev := irc.Event{
		Network: "libera", Kind: irc.EventMessage,
		Msg:  ircv4.MustParseMessage(":alice!u@h PRIVMSG me :hello"),
		Time: time.Now(),
	}
	h.maybePushCandidate(c, ev, store.Message{Target: "alice", Sender: "alice", Text: "hello", Time: time.Now()}, false, false)
	select {
	case cand := <-h.pushCandidates:
		t.Fatalf("candidate enqueued with zero subscriptions: %+v", cand)
	default:
	}

	// With a subscription counted, the same call enqueues — and the
	// candidate carries the CANONICAL stored spelling (stored.Target),
	// not the wire casing, so marker cancels / mutes / the fire-time
	// re-check all key consistently.
	addTestSubscription(t, h, "https://push.example/dev1")
	h.RefreshPushCount(context.Background())
	h.maybePushCandidate(c, ev, store.Message{Target: "alice", Sender: "Alice", Text: "hello", Time: time.Now()}, false, false)
	select {
	case cand := <-h.pushCandidates:
		if cand.buffer != "alice" || cand.channelLike {
			t.Fatalf("candidate = %+v", cand)
		}
	default:
		t.Fatal("no candidate enqueued with a subscription present")
	}

	// Replayed, own, and textless events never become candidates.
	h.maybePushCandidate(c, ev, store.Message{Target: "alice", Sender: "alice", Text: "hello"}, true, false)
	h.maybePushCandidate(c, ev, store.Message{Target: "alice", Sender: "me", Text: "hello"}, false, true)
	h.maybePushCandidate(c, ev, store.Message{Target: "alice", Sender: "alice"}, false, false)
	select {
	case cand := <-h.pushCandidates:
		t.Fatalf("filtered event enqueued: %+v", cand)
	default:
	}
}

func TestTruncatePushText(t *testing.T) {
	if got := truncatePushText("short"); got != "short" {
		t.Errorf("short = %q", got)
	}
	long := ""
	for len(long) < maxPushTextBytes {
		long += "é" // 2 bytes: forces a boundary check at the cap
	}
	long += "tail"
	got := truncatePushText(long)
	if len(got) > maxPushTextBytes {
		t.Errorf("len = %d", len(got))
	}
	for _, r := range got {
		if r != 'é' {
			t.Errorf("split rune: %q", r)
		}
	}
}
