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

package irc

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	ircv4 "gopkg.in/irc.v4"
)

func TestParseSTS(t *testing.T) {
	cases := []struct {
		in   string
		want stsValue
	}{
		{"port=6697", stsValue{port: 6697}},
		{"duration=300", stsValue{hasDuration: true, duration: 300 * time.Second}},
		{"duration=0", stsValue{hasDuration: true}},
		{"port=6697,duration=60", stsValue{port: 6697, hasDuration: true, duration: time.Minute}},
		// Unknown keys ignored; malformed values dropped.
		{"preload,port=6697,future=x", stsValue{port: 6697}},
		{"port=notaport", stsValue{}},
		{"port=0", stsValue{}},
		{"port=70000", stsValue{}},
		{"duration=-1", stsValue{}},
		// A huge advertised duration is clamped to the ~100-year max, not
		// overflowed to a garbage (past-expiring, STS-disabling) value.
		{"duration=99999999999", stsValue{hasDuration: true, duration: time.Duration(100*365*24*60*60) * time.Second}},
		{"", stsValue{}},
	}
	for _, tc := range cases {
		if got := parseSTS(tc.in); got != tc.want {
			t.Errorf("parseSTS(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestHandshakeSTS(t *testing.T) {
	cfg := Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true}
	ls := func(caps string) *ircv4.Message {
		return ircv4.MustParseMessage("CAP * LS :" + caps)
	}

	t.Run("insecure with port upgrades", func(t *testing.T) {
		hs := newHandshake(&cfg)
		hs.start()
		_, _, err := hs.handle(ls("sts=duration=300,port=6697 multi-prefix"))
		var up errSTSUpgrade
		if !errors.As(err, &up) || up.port != 6697 {
			t.Fatalf("err = %v, want errSTSUpgrade{6697}", err)
		}
	})

	t.Run("insecure without port is ignored", func(t *testing.T) {
		hs := newHandshake(&cfg)
		hs.start()
		out, _, err := hs.handle(ls("sts=duration=300 multi-prefix"))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(out) == 0 {
			t.Fatal("handshake did not continue")
		}
	})

	t.Run("secure records the duration and continues", func(t *testing.T) {
		hs := newHandshake(&cfg)
		hs.secure = true
		hs.start()
		out, _, err := hs.handle(ls("sts=duration=300,port=12345 multi-prefix"))
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if hs.stsDuration == nil || *hs.stsDuration != 300*time.Second {
			t.Fatalf("stsDuration = %v, want 300s", hs.stsDuration)
		}
		// The port key is ignored on secure connections: no upgrade error,
		// registration proceeds.
		if len(out) == 0 {
			t.Fatal("handshake did not continue")
		}
	})

	t.Run("sts is never requested", func(t *testing.T) {
		hs := newHandshake(&cfg)
		hs.secure = true
		hs.start()
		out, _, err := hs.handle(ls("sts=duration=300 multi-prefix"))
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range out {
			if msg.Command == "CAP" && len(msg.Params) >= 2 && msg.Params[0] == "REQ" {
				if slices.Contains(strings.Fields(msg.Params[len(msg.Params)-1]), "sts") {
					t.Fatalf("client requested sts: %v", msg)
				}
			}
		}
	})
}

func TestManagerSTSPolicyState(t *testing.T) {
	m, err := NewManager(Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}

	// No policy: plaintext to the configured address.
	if addr, secure := m.effectiveAddr(); addr != "irc.test:6667" || secure {
		t.Fatalf("effectiveAddr = %q/%v", addr, secure)
	}

	// A duration policy on a secure connection sets port + expiry.
	m.applySTS(t.Context(), "irc.test:6697", 5*time.Minute)
	if addr, secure := m.effectiveAddr(); addr != "irc.test:6697" || !secure {
		t.Fatalf("after applySTS: effectiveAddr = %q/%v", addr, secure)
	}

	// duration=0 clears it.
	m.applySTS(t.Context(), "irc.test:6697", 0)
	if addr, secure := m.effectiveAddr(); addr != "irc.test:6667" || secure {
		t.Fatalf("after clear: effectiveAddr = %q/%v", addr, secure)
	}

	// An expired policy no longer upgrades.
	m.applySTS(t.Context(), "irc.test:6697", time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if _, secure := m.effectiveAddr(); secure {
		t.Fatal("expired policy still upgrades")
	}
}

// fakeSTSStore returns a fixed policy/error; err is the fail-closed signal.
// setDur records the duration passed to the last SetSTSPolicy.
type fakeSTSStore struct {
	port        int
	until       time.Time
	duration    time.Duration
	ok          bool
	err         error
	setDur      time.Duration
	revision    uint64
	policyEpoch uint64
	setErr      error
	reschedErr  error
}

func (f *fakeSTSStore) STSPolicy(_ context.Context, _ string) (int, time.Time, time.Duration, uint64, uint64, bool, error) {
	return f.port, f.until, f.duration, f.revision, f.policyEpoch, f.ok, f.err
}
func (f *fakeSTSStore) SetSTSPolicy(_ context.Context, _ string, port int, until time.Time, d time.Duration) (uint64, uint64, error) {
	f.setDur = d
	if f.setErr != nil {
		return f.revision, f.policyEpoch, f.setErr
	}
	f.revision++
	f.policyEpoch++
	f.port, f.until, f.duration, f.ok = port, until, d, true
	return f.revision, f.policyEpoch, nil
}
func (f *fakeSTSStore) ClearSTSPolicy(context.Context, string) (uint64, uint64, error) {
	f.revision++
	f.policyEpoch++
	f.ok = false
	return f.revision, f.policyEpoch, nil
}
func (f *fakeSTSStore) RescheduleSTSPolicy(_ context.Context, _ string, expected uint64, until time.Time) (uint64, bool, error) {
	if f.reschedErr != nil {
		return f.revision, false, f.reschedErr
	}
	if expected != f.revision || !f.ok {
		return f.revision, false, nil
	}
	f.setDur = f.duration
	f.revision++
	f.until = until
	return f.revision, true, nil
}

func TestFailedSTSPersistRetainsActiveLocalPolicy(t *testing.T) {
	store := &fakeSTSStore{revision: 7, policyEpoch: 3, setErr: errors.New("disk full")}
	m, err := NewManager(Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true, STS: store})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.loadSTS(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.applySTS(t.Context(), "irc.test:6697", 2*time.Hour)
	if addr, secure := m.effectiveAddr(); addr != "irc.test:6697" || !secure {
		t.Fatalf("after failed persist: addr=%q secure=%v", addr, secure)
	}
	// A normal secure disconnect immediately tries to reschedule. Because no
	// full record was persisted, an expiry-only CAS cannot repair it and must
	// not clear the dirty local policy.
	m.rescheduleSTS(t.Context())
	// Run reloads before every reconnect. The still-absent store row must not
	// erase the verified policy merely because its persistence failed.
	if err := m.loadSTS(t.Context()); err != nil {
		t.Fatal(err)
	}
	if addr, secure := m.effectiveAddr(); addr != "irc.test:6697" || !secure {
		t.Fatalf("after reload: addr=%q secure=%v; local policy was lost", addr, secure)
	}
	// A distinct shared semantic generation is authoritative, including a clear.
	store.revision = 8
	store.policyEpoch = 4
	if err := m.loadSTS(t.Context()); err != nil {
		t.Fatal(err)
	}
	if addr, secure := m.effectiveAddr(); addr != "irc.test:6667" || secure {
		t.Fatalf("newer shared clear did not win: addr=%q secure=%v", addr, secure)
	}
}

// A record revision changes for both semantic policy writes and expiry-only
// reschedules. Keep those generations separate: otherwise manager B extending
// the old policy can make manager A discard a newer, verified policy whose full
// Set failed, shortening downgrade protection and restoring the old TLS port.
func TestFailedSTSSetSurvivesOtherManagerReschedule(t *testing.T) {
	oldUntil := time.Now().Add(30 * time.Minute)
	store := &fakeSTSStore{
		port:        6697,
		until:       oldUntil,
		duration:    time.Hour,
		ok:          true,
		revision:    7,
		policyEpoch: 3,
		setErr:      errors.New("disk full"),
	}
	newManager := func(name string) *Manager {
		t.Helper()
		m, err := NewManager(Config{
			Name: name, Addr: "irc.test:6667", Nick: "AlteredParadox",
			AllowPlaintext: true, STS: store,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.loadSTS(t.Context()); err != nil {
			t.Fatal(err)
		}
		return m
	}

	a := newManager("a")
	b := newManager("b")
	a.applySTS(t.Context(), "irc.test:7443", 24*time.Hour)
	a.stsMu.Lock()
	verifiedUntil := a.stsUntil
	a.stsMu.Unlock()

	// B still knows the old policy and successfully performs its expiry-only
	// CAS. This changes the mutation revision, but not the semantic epoch.
	b.rescheduleSTS(t.Context())
	if store.revision != 8 || store.policyEpoch != 3 {
		t.Fatalf("B reschedule generations = revision %d epoch %d, want 8/3", store.revision, store.policyEpoch)
	}
	if !store.until.Before(verifiedUntil) {
		t.Fatalf("test setup did not create a shorter stored policy: store=%v verified=%v", store.until, verifiedUntil)
	}

	if err := a.loadSTS(t.Context()); err != nil {
		t.Fatal(err)
	}
	if addr, secure := a.effectiveAddr(); addr != "irc.test:7443" || !secure {
		t.Fatalf("reschedule displaced failed Set: addr=%q secure=%v", addr, secure)
	}
	a.stsMu.Lock()
	gotUntil, gotDuration := a.stsUntil, a.stsLastDur
	a.stsMu.Unlock()
	if !gotUntil.Equal(verifiedUntil) || gotDuration != 24*time.Hour {
		t.Fatalf("verified policy changed after B reschedule: until=%v duration=%v, want %v/24h", gotUntil, gotDuration, verifiedUntil)
	}
}

func TestFailedSTSRescheduleRetainsLocalExtension(t *testing.T) {
	originalUntil := time.Now().Add(5 * time.Minute)
	store := &fakeSTSStore{
		port: 6697, until: originalUntil, duration: 2 * time.Hour, ok: true, revision: 3, policyEpoch: 2,
		reschedErr: errors.New("database is read-only"),
	}
	m, err := NewManager(Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true, STS: store})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.loadSTS(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.rescheduleSTS(t.Context())
	m.stsMu.Lock()
	extendedUntil := m.stsUntil
	m.stsMu.Unlock()
	if !extendedUntil.After(originalUntil.Add(time.Hour)) {
		t.Fatalf("local expiry was not extended: got %v, original %v", extendedUntil, originalUntil)
	}
	if err := m.loadSTS(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.stsMu.Lock()
	gotUntil := m.stsUntil
	m.stsMu.Unlock()
	if !gotUntil.Equal(extendedUntil) {
		t.Fatalf("reload replaced failed reschedule extension: got %v want %v", gotUntil, extendedUntil)
	}

	// A newer shared policy supersedes the dirty extension even when it expires
	// sooner; its generation represents an explicit hostname-wide update.
	store.revision = 4
	store.policyEpoch = 3
	store.port = 7000
	store.until = time.Now().Add(30 * time.Minute)
	store.duration = 30 * time.Minute
	if err := m.loadSTS(t.Context()); err != nil {
		t.Fatal(err)
	}
	if addr, secure := m.effectiveAddr(); addr != "irc.test:7000" || !secure {
		t.Fatalf("newer shared policy did not win: addr=%q secure=%v", addr, secure)
	}
}

func TestStaleSTSRescheduleRetainsExtensionUntilEpochReload(t *testing.T) {
	originalUntil := time.Now().Add(5 * time.Minute)
	store := &fakeSTSStore{
		port: 6697, until: originalUntil, duration: 2 * time.Hour,
		ok: true, revision: 3, policyEpoch: 9,
	}
	m, err := NewManager(Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true, STS: store})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.loadSTS(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Another profile performed an expiry-only mutation. Our CAS is stale, but
	// the unchanged semantic epoch means reload must merge rather than discard
	// our later local disconnect extension.
	store.revision++
	m.rescheduleSTS(t.Context())
	m.stsMu.Lock()
	localUntil := m.stsUntil
	dirty := m.stsDirty
	m.stsMu.Unlock()
	if !dirty || !localUntil.After(originalUntil.Add(time.Hour)) {
		t.Fatalf("stale CAS did not retain a dirty extension: dirty=%v until=%v", dirty, localUntil)
	}
	if err := m.loadSTS(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.stsMu.Lock()
	gotUntil := m.stsUntil
	m.stsMu.Unlock()
	if !gotUntil.Equal(localUntil) {
		t.Fatalf("same-epoch reload lost extension: got %v want %v", gotUntil, localUntil)
	}
}

// loadSTS must FAIL CLOSED on an indeterminate policy state (store error or a
// corrupt record): it returns a non-nil error, which Run turns into "do not
// dial plaintext, retry". Once the store recovers with a real policy, loadSTS
// applies it and effectiveAddr upgrades to the TLS port — so a plaintext
// network never dials in cleartext across the failure window.
func TestLoadSTSFailClosed(t *testing.T) {
	store := &fakeSTSStore{err: errors.New("db locked")}
	m, err := NewManager(Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true, STS: store})
	if err != nil {
		t.Fatal(err)
	}

	// Indeterminate state → error (Run refuses to dial plaintext on this).
	if err := m.loadSTS(context.Background()); err == nil {
		t.Fatal("loadSTS returned nil on a store error; a plaintext network would dial in cleartext")
	}
	// Nothing was applied, so effectiveAddr would still be plaintext — but Run
	// never reaches the dial because loadSTS errored.
	if _, secure := m.effectiveAddr(); secure {
		t.Fatal("an errored load must not have upgraded the address")
	}

	// Store recovers with an unexpired TLS policy.
	store.err = nil
	store.ok = true
	store.port = 6697
	store.until = time.Now().Add(time.Hour)
	if err := m.loadSTS(context.Background()); err != nil {
		t.Fatalf("loadSTS after recovery: %v", err)
	}
	if addr, secure := m.effectiveAddr(); addr != "irc.test:6697" || !secure {
		t.Fatalf("after recovery effectiveAddr = %q/%v, want the TLS port", addr, secure)
	}
}

// A restored policy must carry its DURATION forward: without it, rescheduleSTS
// (the on-disconnect expiry refresh the STS spec requires) can't extend a
// cached policy, so it would expire at its stored `until` unless the server
// re-advertised STS. loadSTS restores the duration; rescheduleSTS persists the
// refreshed expiry using it.
func TestLoadSTSRestoresDuration(t *testing.T) {
	store := &fakeSTSStore{ok: true, port: 6697, until: time.Now().Add(time.Hour), duration: 2 * time.Hour, revision: 1}
	m, err := NewManager(Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true, STS: store})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.loadSTS(context.Background()); err != nil {
		t.Fatal(err)
	}
	// On disconnect the expiry is rescheduled to now + the restored duration
	// and persisted with that same duration.
	m.rescheduleSTS(context.Background())
	if store.setDur != 2*time.Hour {
		t.Fatalf("reschedule persisted duration %v, want 2h (restored duration lost)", store.setDur)
	}
}

// A post-registration CAP NEW carrying an STS upgrade port over an
// insecure link triggers the same secure-reconnect abort as CAP LS.
func TestCapNotifySTSUpgrade(t *testing.T) {
	m, err := NewManager(Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}

	insecure := &liveConn{secure: false, addr: "irc.test:6667"}
	msg := ircv4.MustParseMessage(":srv CAP AlteredParadox NEW :sts=port=6697,duration=100")
	err = m.capNotifySTS(context.Background(), insecure, msg)
	var up errSTSUpgrade
	if !errors.As(err, &up) || up.port != 6697 {
		t.Fatalf("insecure CAP NEW sts: err = %v, want errSTSUpgrade{6697}", err)
	}

	// On a secure link the same NEW persists the duration, no upgrade.
	secure := &liveConn{secure: true, addr: "irc.test:6697"}
	if err := m.capNotifySTS(context.Background(), secure, msg); err != nil {
		t.Fatalf("secure CAP NEW sts: %v", err)
	}
}

// A post-registration CAP ACK must only enable capabilities we actually
// want (and therefore requested). Otherwise a hostile server can ACK an
// unbounded stream of unique, unrequested names, growing the copy-on-
// write capability map without limit (O(n²) copying) until the process
// dies.
func TestCapNotifyACKRejectsUnrequested(t *testing.T) {
	m, err := NewManager(Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}
	m.handleCapNotify(ircv4.MustParseMessage(":srv CAP AlteredParadox ACK :evil-1 evil-2 evil-3"))
	if m.CapEnabled("evil-1") || m.CapEnabled("evil-2") || m.CapEnabled("evil-3") {
		t.Fatal("ACK of unrequested capabilities was accepted")
	}
	// A wanted capability, by contrast, is enabled by its ACK.
	m.handleCapNotify(ircv4.MustParseMessage(":srv CAP AlteredParadox ACK :away-notify"))
	if !m.CapEnabled("away-notify") {
		t.Fatal("ACK of a wanted capability (away-notify) was not enabled")
	}
	// DEL still drops arbitrary names (no allowlist on removal).
	m.handleCapNotify(ircv4.MustParseMessage(":srv CAP AlteredParadox DEL :away-notify"))
	if m.CapEnabled("away-notify") {
		t.Fatal("DEL did not remove away-notify")
	}
}

// A CAP NEW that repeats a wanted cap many times must not build an
// over-length CAP REQ: without dedup the assembled line trips the writer's
// fatal length guard (the internal path has no sendAll check), looping the
// connection.
func TestCapNotifyNEWDedupsRepeatedCaps(t *testing.T) {
	m, err := NewManager(Config{Addr: "irc.test:6667", Nick: "AlteredParadox", AllowPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}
	rep := strings.Repeat("multi-prefix ", 1000)
	out := m.handleCapNotify(ircv4.MustParseMessage(":srv CAP AlteredParadox NEW :" + rep))
	if len(out) != 1 {
		t.Fatalf("handleCapNotify returned %d messages, want 1", len(out))
	}
	if got := out[0].Param(1); got != "multi-prefix" {
		t.Fatalf("CAP REQ = %q, want a single 'multi-prefix'", got)
	}
	if err := checkLineLen(out[0], defaultLineLen); err != nil {
		t.Fatalf("assembled CAP REQ exceeds the line limit: %v", err)
	}
}
