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
)

// Setting returns the stored value for key, or "" when unset.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	v, _, err := s.settingValue(ctx, key)
	return v, err
}

// settingValue reads a setting and reports whether the row is PRESENT, so a
// caller can tell an absent key ("" , present=false) from a stored empty value
// ("", present=true) — the STS lookup needs that distinction to treat a
// present-but-empty (tampered) record as corrupt rather than "no policy".
func (s *Store) settingValue(ctx context.Context, key string) (value string, present bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	err = s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting stores (or replaces) the value for key.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// SetSettings stores several key/value pairs atomically — all commit or none
// do — so a caller updating a related group (e.g. the two retention keys)
// can't leave them half-written on a mid-write error or concurrent request.
func (s *Store) SetSettings(ctx context.Context, kv map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// DeleteSetting removes key; deleting an absent key is not an error.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}
