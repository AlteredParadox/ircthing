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
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"ircthing/internal/hub"
	"ircthing/internal/store"

	_ "modernc.org/sqlite"
)

// A legacy store can hold more rows than the runtime cap, with an
// invalid-name row sitting past the cap position. startNetworks must
// register every invalid-name row with the hub regardless of the cap:
// get_networks synthesizes a recovery entry for each such row it pages, and
// an entry without a registered rowid mapping cannot be deleted — its only
// advertised affordance would dead-end on itself.
func TestStartNetworksRegistersInvalidRowsPastCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	st, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Raw inserts: legacy tables predate both ingress validation and the
	// runtime cap, so PutNetworkConfig cannot build this shape.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < store.MaxNetworkConfigs+2; i++ {
		// Config "{" fails netconf.Parse, so no connection goroutines start.
		if _, err := db.ExecContext(ctx, `INSERT INTO network_configs(name, config) VALUES (?, ?)`,
			fmt.Sprintf("legacy-%03d", i), `{`); err != nil {
			t.Fatal(err)
		}
	}
	res, err := db.ExecContext(ctx, `INSERT INTO network_configs(name, config) VALUES (?, ?)`,
		"bad\x1bname", `{`)
	if err != nil {
		t.Fatal(err)
	}
	invalidRow, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	h := hub.New(st)
	if err := startNetworks(ctx, st, h, nil); err != nil {
		t.Fatal(err)
	}

	sess := h.NewSession()
	defer sess.Close()
	// Page get_networks like a client and find the advertised recovery entry.
	label := ""
	var after int64
	for page := 0; page < 20 && label == ""; page++ {
		sess.Handle(ctx, requestEnvelope(t, "get_networks", int64(page+1), hub.GetNetworksReq{After: after}))
		env := recvEnvelope(t, sess, "networks")
		var data hub.NetworksData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		for _, n := range data.Networks {
			if n.InvalidName && n.RecoveryID == invalidRow {
				label = n.Name
			}
		}
		if !data.HasMore {
			break
		}
		after = data.Next
	}
	if label == "" {
		t.Fatalf("no recovery entry advertised for invalid-name row %d", invalidRow)
	}

	// The advertised entry must actually delete, even though its row sits
	// past the runtime cap position.
	sess.Handle(ctx, requestEnvelope(t, "delete_network", 100, hub.NetworkRef{Network: label}))
	if env := recvEnvelope(t, sess, "ok", "network_removed", "networks_changed"); env.Seq != 100 {
		t.Fatalf("delete reply seq = %d, want 100", env.Seq)
	}
	if count, err := st.NetworkConfigCount(ctx); err != nil || count != store.MaxNetworkConfigs+2 {
		t.Fatalf("count after recovery delete = %d err=%v, want %d", count, err, store.MaxNetworkConfigs+2)
	}
}

func requestEnvelope(t *testing.T, typ string, seq int64, data any) hub.Envelope {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return hub.Envelope{V: hub.ProtocolVersion, Type: typ, Seq: seq, Data: raw}
}

func recvEnvelope(t *testing.T, s *hub.Session, wantType string, skip ...string) hub.Envelope {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case frame := <-s.Outbound():
			var env hub.Envelope
			err := json.Unmarshal(frame.Data, &env)
			frame.Release()
			if err != nil {
				t.Fatalf("decode outbound frame: %v", err)
			}
			if env.Type == wantType {
				return env
			}
			skipped := false
			for _, sk := range skip {
				if env.Type == sk {
					skipped = true
				}
			}
			if !skipped {
				t.Fatalf("got envelope type %q, want %q", env.Type, wantType)
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %q envelope", wantType)
		}
	}
}
