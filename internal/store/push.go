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
	"context"
	"errors"
	"time"
)

// PushSubscription is one registered Web Push endpoint (a browser/device
// profile). Keys are stored as the client sent them: base64url.
type PushSubscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// maxPushSubscriptions bounds the table: one household of devices, not an
// unbounded write path. Registration is authenticated, so the cap guards
// against forgotten stale rows more than abuse; hitting it means dead
// subscriptions are piling up faster than 404/410 pruning clears them.
const maxPushSubscriptions = 16

// ErrPushSubscriptionCap reports a refused insert at the row cap.
var ErrPushSubscriptionCap = errors.New("store: push subscription limit reached")

// UpsertPushSubscription registers an endpoint or refreshes its keys in
// place (a browser re-subscribing after eviction keeps the same row). The
// cap check and insert run in one transaction so concurrent registrations
// cannot overshoot.
func (s *Store) UpsertPushSubscription(ctx context.Context, sub PushSubscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM push_subscriptions WHERE endpoint <> ?`, sub.Endpoint).Scan(&n); err != nil {
		return err
	}
	if n >= maxPushSubscriptions {
		return ErrPushSubscriptionCap
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO push_subscriptions (endpoint, p256dh, auth, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth`,
		sub.Endpoint, sub.P256dh, sub.Auth, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// DeletePushSubscription removes an endpoint; deleting an unknown one is
// a no-op (unsubscribe must be idempotent).
func (s *Store) DeletePushSubscription(ctx context.Context, endpoint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
	return err
}

// PushSubscriptions returns every registered endpoint.
func (s *Store) PushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT endpoint, p256dh, auth FROM push_subscriptions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		var p PushSubscription
		if err := rows.Scan(&p.Endpoint, &p.P256dh, &p.Auth); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountPushSubscriptions reports the number of registered endpoints — the
// hub caches this so the per-message push fast path is one atomic load.
func (s *Store) CountPushSubscriptions(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM push_subscriptions`).Scan(&n)
	return n, err
}

// TouchPushSuccess records a successful delivery so stale rows are
// distinguishable from live ones when debugging.
func (s *Store) TouchPushSuccess(ctx context.Context, endpoint string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`UPDATE push_subscriptions SET last_success = ? WHERE endpoint = ?`,
		t.UnixMilli(), endpoint)
	return err
}
