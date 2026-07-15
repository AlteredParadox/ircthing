package store

import (
	"context"
	"database/sql"
	"errors"
)

// The MONITOR buddy list, persisted per network. Presence (online/offline)
// is ephemeral and lives in the hub, not here.

// AddMonitor records a monitored nick for a network (creating the network
// row if needed). Idempotent.
func (s *Store) AddMonitor(ctx context.Context, network, nick string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	netID, err := s.networkID(ctx, network, true)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO monitors (network_id, nick) VALUES (?, ?)`, netID, nick)
	return err
}

// RemoveMonitor drops a monitored nick from a network.
func (s *Store) RemoveMonitor(ctx context.Context, network, nick string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	netID, err := s.networkID(ctx, network, false)
	if err != nil || netID == 0 {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM monitors WHERE network_id = ? AND nick = ?`, netID, nick)
	return err
}

// Monitors returns the monitored nicks for a network.
func (s *Store) Monitors(ctx context.Context, network string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	netID, err := s.networkID(ctx, network, false)
	if err != nil || netID == 0 {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT nick FROM monitors WHERE network_id = ? ORDER BY nick`, netID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var nick string
		if err := rows.Scan(&nick); err != nil {
			return nil, err
		}
		out = append(out, nick)
	}
	return out, rows.Err()
}

// networkID resolves a network name to its row id (caller holds s.mu),
// creating it when create is set; returns 0 for an unknown network when
// not creating.
func (s *Store) networkID(ctx context.Context, network string, create bool) (int64, error) {
	if id, ok := s.networks[network]; ok {
		return id, nil
	}
	var netID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM networks WHERE user_id = ? AND name = ?`, defaultUserID, network).Scan(&netID)
	if err == nil {
		s.networks[network] = netID
		return netID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if !create {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO networks (user_id, name) VALUES (?, ?)`, defaultUserID, network)
	if err != nil {
		return 0, err
	}
	netID, err = res.LastInsertId()
	if err != nil {
		return 0, err
	}
	s.networks[network] = netID
	return netID, nil
}
