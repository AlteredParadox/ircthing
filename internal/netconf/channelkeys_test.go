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

package netconf

import (
	"encoding/json"
	"strings"
	"testing"
)

// channel_keys round-trips through Parse / json.Marshal / IRCConfig, is
// omitted from the stored JSON when empty (old rows without the field keep
// loading), and never leaks the key into a validation error.
func TestChannelKeysRoundTrip(t *testing.T) {
	n, err := Parse([]byte(`{"addr": "a:1", "nick": "me", "channels": ["#k", "#open"], "channel_keys": {"#k": "sesame"}}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := n.IRCConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChannelKeys["#k"] != "sesame" || len(cfg.ChannelKeys) != 1 {
		t.Fatalf("IRCConfig().ChannelKeys = %v, want {#k: sesame}", cfg.ChannelKeys)
	}
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"channel_keys":{"#k":"sesame"}`) {
		t.Fatalf("marshal lost the keys: %s", raw)
	}
	again, err := Parse(raw)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if again.ChannelKeys["#k"] != "sesame" {
		t.Fatalf("round trip lost the key: %v", again.ChannelKeys)
	}

	plain, err := Parse([]byte(`{"addr": "a:1", "nick": "me", "channels": ["#k"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(plain); strings.Contains(string(raw), "channel_keys") {
		t.Fatalf("empty channel_keys serialized: %s", raw)
	}
	if cfg, _ := plain.IRCConfig(); cfg.ChannelKeys != nil {
		t.Fatalf("IRCConfig().ChannelKeys = %v, want nil", cfg.ChannelKeys)
	}

	_, err = Parse([]byte(`{"addr": "a:1", "nick": "me", "channel_keys": {"#k": "top secret"}}`))
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("invalid key error = %v, want a rejection that does not echo the key", err)
	}
}
