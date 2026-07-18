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

// NetworkConfig returns one stored definition by name (ok=false when
// absent).
func (s *Store) NetworkConfig(ctx context.Context, name string) (NetworkConfig, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var nc NetworkConfig
	err := s.db.QueryRowContext(ctx,
		`SELECT name, config FROM network_configs WHERE name = ?`, name).Scan(&nc.Name, &nc.Config)
	if errors.Is(err, sql.ErrNoRows) {
		return NetworkConfig{}, false, nil
	}
	if err != nil {
		return NetworkConfig{}, false, err
	}
	return nc, true, nil
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

// ReplaceNetworkConfig stores a definition, atomically retiring an old
// name when the definition was renamed: the history (networks row, so
// buffers/messages/markers/monitors follow) moves to the new name and
// the old definition row goes, all in one transaction — a failure
// changes nothing. oldName == "" or == name is a plain upsert. Caches
// are evicted only after commit.
func (s *Store) ReplaceNetworkConfig(ctx context.Context, oldName, name, config string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	renamed := oldName != "" && oldName != name
	if renamed {
		if _, err := tx.ExecContext(ctx,
			`UPDATE networks SET name = ? WHERE user_id = ? AND name = ?`,
			name, defaultUserID, oldName); err != nil {
			// A unique-constraint hit means the new name already has
			// stored history; surface something actionable.
			var taken string
			if row := tx.QueryRowContext(ctx,
				`SELECT name FROM networks WHERE user_id = ? AND name = ?`,
				defaultUserID, name); row.Scan(&taken) == nil {
				return fmt.Errorf("network %q already has stored history", name)
			}
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM network_configs WHERE name = ?`, oldName); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO network_configs (name, config) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET config = excluded.config`,
		name, config); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if renamed {
		s.dropNetworkCachesLocked(oldName)
	}
	return nil
}

// DeleteNetwork removes a network entirely — its definition row and its
// stored history (the networks row; buffers, messages with their FTS
// rows, read markers, and monitors follow via cascades) — in one
// transaction. Caches are evicted only after commit.
func (s *Store) DeleteNetwork(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM network_configs WHERE name = ?`, name); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM networks WHERE user_id = ? AND name = ?`, defaultUserID, name); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.dropNetworkCachesLocked(name)
	return nil
}

// DeleteBuffer removes one stored buffer and, via cascades, its
// messages (FTS rows via trigger) and read marker. Used by the
// close_buffer request: a closed buffer must not resurrect from the
// store on the next buffer-list refresh.
func (s *Store) DeleteBuffer(ctx context.Context, network, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		DELETE FROM buffers WHERE name = ? AND network_id =
			(SELECT id FROM networks WHERE user_id = ? AND name = ?)`,
		target, defaultUserID, network)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // nothing stored: closing a purely client-side buffer
	}
	k := bufKey{network: network, target: target}
	if id, ok := s.buffers[k]; ok {
		s.dropRingLocked(id)
		delete(s.buffers, k)
	}
	return nil
}

// dropRingLocked removes a resident ring AND subtracts its bytes from the
// global budget accounting — skipping the subtraction would permanently
// shrink the effective budget and eventually thrash the cache. Caller
// holds s.mu.
func (s *Store) dropRingLocked(id int64) {
	if r, ok := s.rings[id]; ok {
		s.ringBytes -= int64(r.bytes)
		delete(s.rings, id)
	}
}

// dropNetworkCachesLocked evicts the in-memory id and ring caches for a
// network whose rows were just deleted or renamed. Caller holds s.mu.
func (s *Store) dropNetworkCachesLocked(network string) {
	delete(s.networks, network)
	for k, id := range s.buffers {
		if k.network == network {
			s.dropRingLocked(id)
			delete(s.buffers, k)
		}
	}
}

// SeedNetworkConfigs imports definitions only when the table is empty
// (first run with a pre-UI config file). Returns whether it seeded. The
// emptiness check and every insert run in one transaction: a partial
// seed would strand the remaining networks forever, because a non-empty
// table makes every later run ignore the config file.
func (s *Store) SeedNetworkConfigs(ctx context.Context, configs []NetworkConfig) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var n int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM network_configs`).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if n > 0 || len(configs) == 0 {
		return false, nil
	}
	for _, nc := range configs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO network_configs (name, config) VALUES (?, ?)`,
			nc.Name, nc.Config); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
