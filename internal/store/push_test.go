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

package store

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPushSubscriptionsCRUD(t *testing.T) {
	s, _ := openTest(t, 10)

	if got, err := s.PushSubscriptions(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
	if n, err := s.CountPushSubscriptions(ctx); err != nil || n != 0 {
		t.Fatalf("count empty: %d, %v", n, err)
	}

	a := PushSubscription{Endpoint: "https://push.example/a", P256dh: "ka", Auth: "aa"}
	b := PushSubscription{Endpoint: "https://push.example/b", P256dh: "kb", Auth: "ab"}
	if err := s.UpsertPushSubscription(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPushSubscription(ctx, b); err != nil {
		t.Fatal(err)
	}

	// Re-subscribing the same endpoint refreshes keys in place.
	a.P256dh, a.Auth = "ka2", "aa2"
	if err := s.UpsertPushSubscription(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.PushSubscriptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("subscriptions = %+v", got)
	}
	if n, _ := s.CountPushSubscriptions(ctx); n != 2 {
		t.Fatalf("count = %d", n)
	}

	if err := s.TouchPushSuccess(ctx, a.Endpoint, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := s.DeletePushSubscription(ctx, a.Endpoint); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePushSubscription(ctx, a.Endpoint); err != nil { // idempotent
		t.Fatal(err)
	}
	if n, _ := s.CountPushSubscriptions(ctx); n != 1 {
		t.Fatalf("count after delete = %d", n)
	}
}

func TestPushSubscriptionCap(t *testing.T) {
	s, _ := openTest(t, 10)

	for i := 0; i < maxPushSubscriptions; i++ {
		sub := PushSubscription{Endpoint: fmt.Sprintf("https://push.example/%d", i), P256dh: "k", Auth: "a"}
		if err := s.UpsertPushSubscription(ctx, sub); err != nil {
			t.Fatalf("subscription %d: %v", i, err)
		}
	}
	err := s.UpsertPushSubscription(ctx, PushSubscription{Endpoint: "https://push.example/over", P256dh: "k", Auth: "a"})
	if !errors.Is(err, ErrPushSubscriptionCap) {
		t.Fatalf("over cap: err = %v", err)
	}
	// Refreshing an EXISTING endpoint at the cap must still work.
	if err := s.UpsertPushSubscription(ctx, PushSubscription{Endpoint: "https://push.example/0", P256dh: "k2", Auth: "a2"}); err != nil {
		t.Fatalf("refresh at cap: %v", err)
	}
}

// TestPushSubscriptionsSurviveReopen proves the migration applies on an
// existing database and rows persist across close/open.
func TestPushSubscriptionsSurviveReopen(t *testing.T) {
	s, path := openTest(t, 10)
	sub := PushSubscription{Endpoint: "https://push.example/persist", P256dh: "k", Auth: "a"}
	if err := s.UpsertPushSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := reopen(t, path, 10)
	got, err := s2.PushSubscriptions(ctx)
	if err != nil || len(got) != 1 || got[0] != sub {
		t.Fatalf("after reopen: %+v, %v", got, err)
	}
}
