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
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Network definitions managed from the web UI (network_configs table).
// Rows hold opaque JSON — the hub owns parsing/validation (netconf);
// the store only persists. The config file seeds this table when empty;
// see cmd/ircd-web.

// NetworkConfig is one stored network definition.
type NetworkConfig struct {
	Name        string
	Config      string // JSON netconf.Network
	Oversized   bool   // page reads omit a legacy row that exceeds the safe cap
	InvalidName bool   // page reads omit a legacy name outside current invariants
	PageID      int64  // rowid cursor populated only by NetworkConfigsPage
}

const (
	ReservedRecoveryNetworkPrefix = "__ircthing_invalid_row_"
	// MaxNetworkNameBytes keeps identifiers cheap in every state/event envelope
	// and log context. It must cover every legal EffectiveName fallback: an
	// unnamed network is keyed by its addr, and proxydial.ValidHostPort accepts
	// a 255-byte host plus ":65535" (~262 bytes). 300 covers that with room to
	// spare. Mirrored by internal/netconf's maxNetworkNameBytes.
	MaxNetworkNameBytes = 300
	// MaxNetworkConfigs bounds reconnect goroutines and every process-wide
	// network map. Existing over-cap databases may still edit/delete rows, but
	// no ingress can grow them further.
	MaxNetworkConfigs = 64
	// MaxNetworkConfigBytes bounds one canonical network definition at every
	// store ingress. In particular, a hostile server cannot grow the persisted
	// autojoin list until get_networks produces a multi-megabyte WebSocket frame.
	MaxNetworkConfigBytes = 64 << 10
)

// ValidateNetworkConfigRecord is the final persistence-boundary guard shared
// by UI puts, config-file seeds, and incremental autojoin updates.
func ValidateNetworkConfigRecord(name, config string) error {
	if err := validateNetworkName(name); err != nil {
		return err
	}
	if len(config) == 0 || len(config) > MaxNetworkConfigBytes {
		return fmt.Errorf("store: network config exceeds %d-byte limit", MaxNetworkConfigBytes)
	}
	return nil
}

// validateNetworkName rejects only what is unsafe to carry through envelopes,
// logs, and the frontend's plain-object maps: empty or oversized names,
// invalid UTF-8, control characters (NUL/CR/LF/tab injection), the reserved
// recovery prefix, and the JS Object.prototype set below. Spaces and other
// printable Unicode are deliberately legal — legacy databases hold names like
// "Libera Chat", and rejecting them here would strand those rows behind the
// destructive delete-only recovery path. Keep in sync with internal/netconf's
// validNetworkName (the packages intentionally do not import each other:
// store stays free of internal deps, and netconf must not pull in the
// persistence layer).
func validateNetworkName(name string) error {
	if name == "" || strings.HasPrefix(name, ReservedRecoveryNetworkPrefix) || len(name) > MaxNetworkNameBytes || !utf8.ValidString(name) {
		return fmt.Errorf("store: invalid network name")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("store: invalid network name")
		}
	}
	// Keep this fixed ECMAScript Object.prototype set aligned with netconf's
	// user-facing validation. Legacy or direct store callers must not smuggle a
	// key that mutates/inherits from the frontend's plain-object maps.
	switch name {
	case "__proto__", "constructor", "prototype",
		"hasOwnProperty", "isPrototypeOf", "propertyIsEnumerable",
		"toLocaleString", "toString", "valueOf",
		"__defineGetter__", "__defineSetter__", "__lookupGetter__", "__lookupSetter__":
		return fmt.Errorf("store: invalid network name")
	}
	return nil
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

// NetworkConfigsPage returns at most limit definitions after the supplied
// opaque rowid cursor, plus whether another page exists. A legacy row larger
// than MaxNetworkConfigBytes is represented by name with Oversized=true and no
// Config bytes: this keeps the recovery listing bounded instead of allocating
// and enqueueing an attacker-sized blob. New writes cannot create such rows.
func (s *Store) NetworkConfigsPage(ctx context.Context, after int64, limit int) ([]NetworkConfig, bool, error) {
	if limit <= 0 {
		return []NetworkConfig{}, false, nil
	}
	if limit > 64 {
		limit = 64
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT rowid,
			CASE WHEN length(CAST(name AS BLOB)) <= ? THEN name ELSE NULL END,
			length(CAST(name AS BLOB)),
			CASE WHEN length(CAST(config AS BLOB)) <= ? THEN config ELSE NULL END,
			length(CAST(config AS BLOB))
		FROM network_configs
		WHERE rowid > ?
		ORDER BY rowid
		LIMIT ?`, MaxNetworkNameBytes, MaxNetworkConfigBytes, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]NetworkConfig, 0, limit+1)
	for rows.Next() {
		var nc NetworkConfig
		var name sql.NullString
		var nameSize int64
		var config sql.NullString
		var size int64
		if err := rows.Scan(&nc.PageID, &name, &nameSize, &config, &size); err != nil {
			return nil, false, err
		}
		if name.Valid && validateNetworkName(name.String) == nil {
			nc.Name = name.String
		} else {
			nc.InvalidName = true
		}
		if nc.InvalidName {
			// Recovery of an invalid-name row is delete-only by opaque PageID. Do
			// not expose its definition to callers that cannot safely address it.
			nc.Config = ""
		} else if config.Valid {
			nc.Config = config.String
		} else {
			nc.Oversized = size > MaxNetworkConfigBytes
		}
		out = append(out, nc)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// NetworkConfig returns one stored definition by name (ok=false when
// absent).
func (s *Store) NetworkConfig(ctx context.Context, name string) (NetworkConfig, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var nc NetworkConfig
	var config sql.NullString
	var size int64
	err := s.db.QueryRowContext(ctx,
		`SELECT name,
			CASE WHEN length(CAST(config AS BLOB)) <= ? THEN config ELSE NULL END,
			length(CAST(config AS BLOB))
		 FROM network_configs WHERE name = ?`, MaxNetworkConfigBytes, name).
		Scan(&nc.Name, &config, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return NetworkConfig{}, false, nil
	}
	if err != nil {
		return NetworkConfig{}, false, err
	}
	if config.Valid {
		nc.Config = config.String
	}
	nc.Oversized = size > MaxNetworkConfigBytes
	return nc, true, nil
}

// NetworkConfigCount reports how many definitions exist without loading any
// config blobs. Startup uses it to decide whether config-file seeding applies.
func (s *Store) NetworkConfigCount(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM network_configs`).Scan(&count)
	return count, err
}

// PutNetworkConfig inserts or replaces a definition.
func (s *Store) PutNetworkConfig(ctx context.Context, name, config string) error {
	if err := ValidateNetworkConfigRecord(name, config); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var exists, count int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM network_configs WHERE name = ?)`, name).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM network_configs`).Scan(&count); err != nil {
			return err
		}
		if count >= MaxNetworkConfigs {
			return fmt.Errorf("store: network limit reached (%d)", MaxNetworkConfigs)
		}
	}

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
	if err := ValidateNetworkConfigRecord(name, config); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	renamed := oldName != "" && oldName != name
	var count, oldExists, targetExists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM network_configs`).Scan(&count); err != nil {
		return err
	}
	if renamed {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM network_configs WHERE name = ?)`, oldName).Scan(&oldExists); err != nil {
			return err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM network_configs WHERE name = ?)`, name).Scan(&targetExists); err != nil {
		return err
	}
	projected := count
	if renamed && oldExists != 0 {
		projected--
	}
	if targetExists == 0 {
		projected++
	}
	if projected > count && count >= MaxNetworkConfigs {
		return fmt.Errorf("store: network limit reached (%d)", MaxNetworkConfigs)
	}
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

// DeleteNetworkByPageID is the recovery-only counterpart for a legacy row
// whose stored name is too large or malformed to expose outside SQLite. The
// name stays inside SQL; clearing all small in-memory caches avoids ever
// materializing it merely for cache-key eviction.
func (s *Store) DeleteNetworkByPageID(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("store: invalid network recovery id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM networks
		WHERE user_id = ? AND name =
			(SELECT name FROM network_configs WHERE rowid = ?)`, defaultUserID, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM network_configs WHERE rowid = ?`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return errors.New("store: network recovery row not found")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	clear(s.networks)
	clear(s.buffers)
	clear(s.rings)
	s.ringBytes = 0
	return nil
}

// DeleteBuffer removes one stored buffer and, via cascades, its
// messages (FTS rows via trigger) and read marker. Used by the
// close_buffer request: a closed buffer must not resurrect from the
// store on the next buffer-list refresh.
func (s *Store) DeleteBuffer(ctx context.Context, network, target string) error {
	_, err := s.DeleteBufferFolded(ctx, network, target, nil, nil)
	return err
}

// DeleteBufferFolded resolves target to the stored spelling under the
// connection's casemapping and deletes it while holding the same store lock as
// AppendFoldedGuarded. afterDelete runs after a successful database operation
// but before that lock is released; the hub uses it to install its close
// tombstone, making delete+tombstone atomic with respect to every append.
// The canonical spelling is returned even when no row exists.
func (s *Store) DeleteBufferFolded(ctx context.Context, network, target string, fold func(string) string, afterDelete func(canonical string)) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	canonical, err := s.canonicalBufferLocked(ctx, network, target, fold)
	if err != nil {
		return "", err
	}

	res, err := s.db.ExecContext(ctx, `
		DELETE FROM buffers WHERE name = ? AND network_id =
			(SELECT id FROM networks WHERE user_id = ? AND name = ?)`,
		canonical, defaultUserID, network)
	if err != nil {
		return "", err
	}
	if afterDelete != nil {
		afterDelete(canonical)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return canonical, nil // closing a purely client-side buffer
	}
	k := bufKey{network: network, target: canonical}
	if id, ok := s.buffers[k]; ok {
		s.dropRingLocked(id)
		delete(s.buffers, k)
	}
	return canonical, nil
}

func (s *Store) canonicalBufferLocked(ctx context.Context, network, target string, fold func(string) string) (string, error) {
	if fold == nil {
		return target, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.name FROM buffers b
		JOIN networks n ON n.id = b.network_id
		WHERE n.user_id = ? AND n.name = ?`, defaultUserID, network)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	want := fold(target)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		if fold(name) == want {
			return name, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return target, nil
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
	if len(configs) > MaxNetworkConfigs {
		return false, fmt.Errorf("store: network limit reached (%d)", MaxNetworkConfigs)
	}
	for _, nc := range configs {
		if err := ValidateNetworkConfigRecord(nc.Name, nc.Config); err != nil {
			return false, err
		}
	}
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
