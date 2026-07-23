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
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"testing"
)

// captureLog redirects the standard logger to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// TestLoginLoggingForFail2ban verifies the log lines a fail2ban filter
// keys on: a failed credential attempt and a backoff-rejected retry both
// carry the source IP; the attempted username is escaped and can't inject
// a log line; and a successful login is audited distinctly. Each case
// uses a fresh server so per-source backoff from a prior failure doesn't
// bleed in (httptest connects from 127.0.0.1, the logged source).

func TestLoginLoggingFailure(t *testing.T) {
	ts, _ := newTestServer(t)
	buf := captureLog(t)
	login(t, ts, "AlteredParadox", "wrong-password")
	if got := buf.String(); !strings.Contains(got, "login: failed authentication from 127.0.0.1") {
		t.Fatalf("failed-login log missing/wrong: %q", got)
	}
}

func TestLoginLoggingUsernameCannotInject(t *testing.T) {
	ts, _ := newTestServer(t)
	buf := captureLog(t)
	// A JSON-escaped newline is valid in transit and decodes to a REAL
	// newline server-side — the actual log-injection vector. Post a
	// marshaled body (the login helper does raw concat, which a real
	// newline would make invalid JSON). %q in the log must escape it so
	// the line does not break in two.
	body, _ := json.Marshal(map[string]string{"username": "eve\ninjected 9.9.9.9", "password": "x"})
	req, _ := http.NewRequest("POST", ts.URL+"/api/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	line := strings.TrimRight(buf.String(), "\n")
	if strings.Contains(line, "\n") {
		t.Fatalf("username injected a newline into the log: %q", line)
	}
	if !strings.Contains(line, "login: failed authentication from 127.0.0.1") {
		t.Fatalf("failed-login log missing: %q", line)
	}
}

func TestLoginLoggingRateLimited(t *testing.T) {
	ts, _ := newTestServer(t)
	buf := captureLog(t)
	// The first failure installs per-source backoff; the second is
	// rejected before the credential check and logged distinctly, so a
	// fail2ban filter still sees the continued abuse.
	login(t, ts, "AlteredParadox", "wrong")
	buf.Reset()
	login(t, ts, "AlteredParadox", "wrong-again")
	if got := buf.String(); !strings.Contains(got, "login: rate-limited from 127.0.0.1") {
		t.Fatalf("rate-limited log missing/wrong: %q", got)
	}
}

func TestLoginLoggingSuccess(t *testing.T) {
	ts, _ := newTestServer(t)
	buf := captureLog(t)
	resp := login(t, ts, "AlteredParadox", "hunter2")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	if got := buf.String(); !strings.Contains(got, "login: authenticated from 127.0.0.1") {
		t.Fatalf("success audit log missing: %q", got)
	}
}
