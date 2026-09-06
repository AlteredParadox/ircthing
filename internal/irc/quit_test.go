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

package irc

import (
	"context"
	"net"
	"testing"
	"time"
)

// A deliberate teardown — the network's context ending (process shutdown,
// or the network stopped from the UI) — must say QUIT before the socket
// closes (RFC 2812 §3.1.7), so the server logs a clean quit instead of a
// read error. Before, cancel closed the socket outright: no QUIT was ever
// sent outside the SASL-abort path.
func TestCancelSendsQuitBeforeClosing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	m, err := NewManager(testCfg(ln.Addr().String()))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	start := time.Now()
	cancel()
	if q := s.readCmd("QUIT"); q.Trailing() != quitReason {
		t.Fatalf("QUIT reason = %q, want %q", q.Trailing(), quitReason)
	}
	// The socket closes right behind it — no more lines, no lingering.
	s.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := s.r.ReadMessage(); err == nil {
		t.Fatalf("server read %q after QUIT, want the socket closed", line.String())
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if took := time.Since(start); took > quitTimeout {
		t.Fatalf("teardown took %v, want well under quitTimeout (%v)", took, quitTimeout)
	}
}
