package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Network definitions managed from the web UI (network_configs table).
// Rows hold opaque JSON — the hub owns parsing/validation (netconf);
// the store only persists. The config file seeds this table when empty;
// see cmd/ircd-web.

// NetworkConfig is one stored network definition.
type NetworkConfig struct {
	Name   string
	Config string // JSON netconf.Network
}

// NetworkConfigs returns all stored definitions, name-ordered.
func (s *Store) NetworkConfigs(ctx context.Context) ([]NetworkConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT name, config FROM network_configs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NetworkConfig
	for rows.Next() {
		var nc NetworkConfig
		if err := rows.Scan(&nc.Name, &nc.Config); err != nil {
			return nil, err
		}
		out = append(out, nc)
	}
	return out, rows.Err()
}

// PutNetworkConfig inserts or replaces a definition.
func (s *Store) PutNetworkConfig(ctx context.Context, name, config string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO network_configs (name, config) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET config = excluded.config`,
		name, config)
	return err
}

// DeleteNetworkConfig removes a stored definition (not the network's
// scrollback — see DeleteNetworkData).
func (s *Store) DeleteNetworkConfig(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `DELETE FROM network_configs WHERE name = ?`, name)
	return err
}

// RenameNetworkData moves a network's stored data (buffers, messages,
// read markers, monitors — everything keyed by the networks row) to a
// new name, so scrollback survives a rename from the UI.
func (s *Store) RenameNetworkData(ctx context.Context, oldName, newName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`UPDATE networks SET name = ? WHERE user_id = ? AND name = ?`,
		newName, defaultUserID, oldName)
	if err != nil {
		var target string
		// A unique-constraint hit means the new name already has data;
		// surface something actionable instead of the raw SQLite error.
		if row := s.db.QueryRowContext(ctx,
			`SELECT name FROM networks WHERE user_id = ? AND name = ?`,
			defaultUserID, newName); row.Scan(&target) == nil {
			return fmt.Errorf("network %q already has stored history", newName)
		}
		return err
	}
	s.dropNetworkCachesLocked(oldName)
	return nil
}

// DeleteNetworkData removes a network's stored history: the networks row
// and, via cascades, its buffers, messages (FTS rows via trigger), read
// markers, and monitors. The definition row is separate
// (DeleteNetworkConfig).
func (s *Store) DeleteNetworkData(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`DELETE FROM networks WHERE user_id = ? AND name = ?`, defaultUserID, name)
	if err != nil {
		return err
	}
	s.dropNetworkCachesLocked(name)
	return nil
}

// dropNetworkCachesLocked evicts the in-memory id and ring caches for a
// network whose rows were just deleted or renamed. Caller holds s.mu.
func (s *Store) dropNetworkCachesLocked(network string) {
	delete(s.networks, network)
	for k, id := range s.buffers {
		if k.network == network {
			delete(s.rings, id)
			delete(s.buffers, k)
		}
	}
}

// SeedNetworkConfigs imports definitions only when the table is empty
// (first run with a pre-UI config file). Returns whether it seeded.
func (s *Store) SeedNetworkConfigs(ctx context.Context, configs []NetworkConfig) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM network_configs`).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if n > 0 || len(configs) == 0 {
		return false, nil
	}
	for _, nc := range configs {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO network_configs (name, config) VALUES (?, ?)`,
			nc.Name, nc.Config); err != nil {
			return false, err
		}
	}
	return true, nil
}
