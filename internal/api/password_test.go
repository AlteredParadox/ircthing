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

package api

import (
	"bytes"
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"ircthing/internal/hub"
	"ircthing/internal/store"
)

// Once a Settings rotation has stored an override, the config-file hash is
// silently ignored — an owner locked out after an attacker rotated the
// password edits config.json, restarts, and stays locked out. The startup
// path must say so in the log, and loadPasswordHash must report the
// override so the caller can.
func TestPasswordOverrideIsAnnouncedAtStartup(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cfg := Config{Username: "u", PasswordHash: testHash(t, "config-password")}

	// First boot: no override, config seed applies, nothing to announce.
	hash, overridden, err := loadPasswordHash(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if overridden || hash != cfg.PasswordHash {
		t.Fatalf("first boot: overridden=%v hash==seed=%v, want false/true", overridden, hash == cfg.PasswordHash)
	}

	// After a rotation the stored hash wins and the fact is reported.
	rotated := testHash(t, "rotated-password")
	if err := st.SetSettingAndWipePushSubscriptions(ctx, passwordHashKey, rotated); err != nil {
		t.Fatal(err)
	}
	hash, overridden, err = loadPasswordHash(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !overridden || hash != rotated {
		t.Fatalf("after rotation: overridden=%v hash==rotated=%v, want true/true", overridden, hash == rotated)
	}

	// api.New logs the notice, and only while the override is in effect.
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	if _, err := New(cfg, hub.New(st), fs.FS(emptyFS{})); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "user.password_hash in the config file is IGNORED") ||
		!strings.Contains(logged.String(), "-reset-password") {
		t.Fatalf("startup log does not announce the active override; got:\n%s", logged.String())
	}
	if strings.HasPrefix(strings.TrimSpace(logged.String()), "login:") {
		t.Fatal("notice uses the login: prefix the fail2ban failregex anchors on")
	}

	// Reset drops the override (and any planted push grant) so the seed
	// applies again — and says nothing further at the next start.
	if err := st.UpsertPushSubscription(ctx, store.PushSubscription{Endpoint: "https://push.example/planted", P256dh: "k", Auth: "a"}); err != nil {
		t.Fatal(err)
	}
	removed, err := ResetPasswordOverride(ctx, st)
	if err != nil || !removed {
		t.Fatalf("ResetPasswordOverride = (%v, %v), want (true, nil)", removed, err)
	}
	if n, err := st.CountPushSubscriptions(ctx); err != nil || n != 0 {
		t.Fatalf("push subscriptions after reset = %d, %v; want 0", n, err)
	}
	hash, overridden, err = loadPasswordHash(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if overridden || hash != cfg.PasswordHash {
		t.Fatalf("after reset: overridden=%v hash==seed=%v, want false/true", overridden, hash == cfg.PasswordHash)
	}
	logged.Reset()
	if _, err := New(cfg, hub.New(st), fs.FS(emptyFS{})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged.String(), "IGNORED") {
		t.Fatalf("notice logged with no override in effect:\n%s", logged.String())
	}
	// Idempotent: a second reset reports nothing to remove.
	if removed, err := ResetPasswordOverride(ctx, st); err != nil || removed {
		t.Fatalf("second ResetPasswordOverride = (%v, %v), want (false, nil)", removed, err)
	}
}

func testHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

// emptyFS stands in for the embedded web assets, which this test never serves.
type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) { return nil, fs.ErrNotExist }
