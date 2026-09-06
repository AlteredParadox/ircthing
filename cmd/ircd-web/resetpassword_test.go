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

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ircthing/internal/store"
)

// -reset-password is the lockout-recovery lever: it must drop the stored
// override (so the config-file hash applies again), be idempotent, and never
// conjure a database that does not exist.
func TestRunResetPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	st, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The key the API stores a Settings rotation under (api.passwordHashKey).
	if err := st.SetSetting(ctx, "password_hash", "$2a$10$abcdefghijklmnopqrstuv"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	if err := runResetPassword(path); err != nil {
		t.Fatalf("runResetPassword: %v", err)
	}
	st, err = store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, present, err := st.SettingValue(ctx, "password_hash"); err != nil || present {
		t.Fatalf("override still present after reset (present=%v, err=%v)", present, err)
	}
	st.Close()

	// A second run is a harmless no-op.
	if err := runResetPassword(path); err != nil {
		t.Fatalf("second runResetPassword: %v", err)
	}

	// No database: refuse rather than create an empty one.
	missing := filepath.Join(t.TempDir(), "missing.db")
	err = runResetPassword(missing)
	if err == nil || !strings.Contains(err.Error(), "nothing to reset") {
		t.Fatalf("missing database: err = %v, want a nothing-to-reset error", err)
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Fatal("runResetPassword created a database that did not exist")
	}
}
