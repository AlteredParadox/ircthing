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
	h.maybePushCandidate(c, ev, store.Message{Sender: "alice", Text: "hello", Time: time.Now()}, "alice", false, false)
	select {
	case cand := <-h.pushCandidates:
		t.Fatalf("candidate enqueued with zero subscriptions: %+v", cand)
	default:
	}

	// With a subscription counted, the same call enqueues.
	addTestSubscription(t, h, "https://push.example/dev1")
	h.RefreshPushCount(context.Background())
	h.maybePushCandidate(c, ev, store.Message{Sender: "alice", Text: "hello", Time: time.Now()}, "alice", false, false)
	select {
	case cand := <-h.pushCandidates:
		if cand.buffer != "alice" || cand.channelLike {
			t.Fatalf("candidate = %+v", cand)
		}
	default:
		t.Fatal("no candidate enqueued with a subscription present")
	}

	// Replayed, own, and textless events never become candidates.
	h.maybePushCandidate(c, ev, store.Message{Sender: "alice", Text: "hello"}, "alice", true, false)
	h.maybePushCandidate(c, ev, store.Message{Sender: "me", Text: "hello"}, "alice", false, true)
	h.maybePushCandidate(c, ev, store.Message{Sender: "alice"}, "alice", false, false)
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
