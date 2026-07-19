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

//go:build integration

// Package integration runs end-to-end tests against a real Ergo IRCd
// (https://github.com/ergochat/ergo). Ergo is a single pure-Go binary, so
// the harness runs it directly instead of in a container — `make ergo`
// builds it into .cache/bin, or set ERGO_BIN. Guarded by the
// `integration` build tag; run via `make integration`.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ircthing/internal/hub"
	"ircthing/internal/irc"
	"ircthing/internal/store"

	ircv4 "gopkg.in/irc.v4"
)

const testTimeout = 20 * time.Second

func findErgo(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("ERGO_BIN"); bin != "" {
		return bin
	}
	if p, err := exec.LookPath("ergo"); err == nil {
		return p
	}
	local, _ := filepath.Abs("../.cache/bin/ergo")
	if _, err := os.Stat(local); err == nil {
		return local
	}
	t.Skip("ergo not found: run `make ergo` or set ERGO_BIN")
	return ""
}

// startErgo boots an isolated Ergo instance and returns its address.
func startErgo(t *testing.T) string {
	t.Helper()
	bin := findErgo(t)
	dir := t.TempDir()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	tmpl, err := os.ReadFile("testdata/ergo.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	conf := strings.ReplaceAll(string(tmpl), "{{PORT}}", fmt.Sprint(port))
	conf = strings.ReplaceAll(conf, "{{DIR}}", dir)
	confPath := filepath.Join(dir, "ircd.yaml")
	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}

	logf, err := os.Create(filepath.Join(dir, "ergo.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "run", "--conf", confPath)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ergo: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		logf.Close()
		// A failing test's best witness is the ircd's own log.
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "ergo.log"))
			if len(b) > 8192 {
				b = b[len(b)-8192:]
			}
			t.Logf("ergo log tail:\n%s", b)
		}
	})

	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return addr
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(dir, "ergo.log"))
			t.Fatalf("ergo never listened on %s; log:\n%s", addr, log)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// rawClient is a bare-bones IRC client for playing the "other user".
type rawClient struct {
	t *testing.T
	c net.Conn
	r *ircv4.Reader
	w *ircv4.Writer
}

func dialRaw(t *testing.T, addr, nick string) *rawClient {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	rc := &rawClient{t: t, c: c, r: ircv4.NewReader(c), w: ircv4.NewWriter(c)}
	rc.send("NICK " + nick)
	rc.send("USER " + nick + " 0 * :" + nick)
	rc.waitFor(func(m *ircv4.Message) bool { return m.Command == "001" })
	return rc
}

func (rc *rawClient) send(line string) {
	rc.t.Helper()
	rc.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := rc.w.Write(line); err != nil {
		rc.t.Fatalf("raw write %q: %v", line, err)
	}
}

// waitFor reads (answering PINGs) until pred matches or the timeout hits.
func (rc *rawClient) waitFor(pred func(*ircv4.Message) bool) *ircv4.Message {
	rc.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		rc.c.SetReadDeadline(deadline)
		m, err := rc.r.ReadMessage()
		if err != nil {
			rc.t.Fatalf("raw read: %v", err)
		}
		if m.Command == "PING" {
			rc.send("PONG :" + m.Trailing())
			continue
		}
		if pred(m) {
			return m
		}
	}
}

// stack wires store -> hub -> manager the way cmd/ircd-web does.
type stack struct {
	t         *testing.T
	st        *store.Store
	h         *hub.Hub
	sess      *hub.Session
	cancel    context.CancelFunc
	rosterSeq int64
}

func startStack(t *testing.T, st *store.Store, h *hub.Hub, cfg irc.Config) *stack {
	t.Helper()
	cfg.AllowPlaintext = true
	cfg.Backoff = irc.BackoffConfig{Min: 50 * time.Millisecond, Max: 200 * time.Millisecond}
	// Keep flood protection out of the tests' way.
	cfg.SendBurst = 64
	cfg.SendInterval = 10 * time.Millisecond
	m, err := irc.NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// The session must exist before the hub starts broadcasting, or a
	// fast registration can slip past waitRegistered.
	s := &stack{t: t, st: st, h: h, sess: h.NewSession(), cancel: cancel}
	go m.Run(ctx)
	go h.Run(ctx, m)
	t.Cleanup(s.stop)
	return s
}

func (s *stack) stop() {
	if s.cancel != nil {
		s.sess.Close()
		s.cancel()
		s.cancel = nil
	}
}

// waitEnvelope drains the session until an envelope of the given type
// matches pred (nil = any). On timeout it reports everything it saw.
func (s *stack) waitEnvelope(typ string, pred func(json.RawMessage) bool) hub.Envelope {
	s.t.Helper()
	deadline := time.After(testTimeout)
	var seen []string
	for {
		select {
		case env := <-s.sess.Outbound():
			if env.Type == typ && (pred == nil || pred(env.Data)) {
				return env
			}
			seen = append(seen, env.Type+" "+string(env.Data))
		case <-s.sess.Done():
			s.t.Fatalf("session evicted while waiting for %q; saw:\n%s", typ, strings.Join(seen, "\n"))
		case <-deadline:
			s.t.Fatalf("timed out waiting for %q envelope; saw %d others:\n%s", typ, len(seen), strings.Join(seen, "\n"))
		}
	}
}

func (s *stack) waitRegistered() {
	s.t.Helper()
	s.waitEnvelope("state", func(d json.RawMessage) bool {
		var sd hub.StateData
		return json.Unmarshal(d, &sd) == nil && sd.State == "registered"
	})
}

// waitJoined waits until the server has processed nick's join to channel
// (its JOIN echo came back). Tests must sequence on this before other
// clients act, or the join order races.
func (s *stack) waitJoined(nick, channel string) {
	s.t.Helper()
	s.waitEnvelope("event", func(d json.RawMessage) bool {
		var ev hub.EventData
		return json.Unmarshal(d, &ev) == nil &&
			ev.Command == "JOIN" && ev.Sender == nick && ev.Buffer == channel
	})
}

// channelMembers requests a channel's roster from a session and returns
// the member nicks, retrying until non-empty or timeout.
func (s *stack) channelMembers(network, buffer string) []string {
	s.t.Helper()
	deadline := time.Now().Add(testTimeout)
	seq := int64(1000)
	for {
		seq++
		s.sess.Handle(context.Background(), envelope(s.t, "get_channel", seq, hub.ChannelReq{
			Network: network, Buffer: buffer,
		}))
		env := s.waitEnvelope("channel", func(d json.RawMessage) bool {
			var cd hub.ChannelData
			return json.Unmarshal(d, &cd) == nil && cd.Buffer == buffer
		})
		var cd hub.ChannelData
		json.Unmarshal(env.Data, &cd)
		if len(cd.Members) > 0 {
			out := make([]string, len(cd.Members))
			for i, m := range cd.Members {
				out[i] = m.Nick
			}
			return out
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("channel %s never populated members", buffer)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// waitStored polls the store until pred over the buffer's latest page
// holds.
func (s *stack) waitStored(network, target string, pred func([]store.Message) bool) []store.Message {
	s.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		msgs, err := s.st.Latest(context.Background(), network, target, 100)
		if err != nil {
			s.t.Fatal(err)
		}
		if pred(msgs) {
			return msgs
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("store never satisfied predicate; have %d messages: %v", len(msgs), rawsOf(msgs))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func rawsOf(msgs []store.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Raw
	}
	return out
}

func countContaining(msgs []store.Message, sub string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Raw, sub) {
			n++
		}
	}
	return n
}

func newStoreAndHub(t *testing.T) (*store.Store, *hub.Hub) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "it.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, hub.New(st)
}

// envelope builds a client-side protocol envelope for Session.Handle.
func envelope(t *testing.T, typ string, seq int64, data any) hub.Envelope {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return hub.Envelope{V: hub.ProtocolVersion, Type: typ, Seq: seq, Data: raw}
}

// channelRoster requests a channel's full member data once.
func (s *stack) channelRoster(network, buffer string) []hub.MemberData {
	s.t.Helper()
	s.rosterSeq++
	s.sess.Handle(context.Background(), envelope(s.t, "get_channel", 5000+s.rosterSeq, hub.ChannelReq{
		Network: network, Buffer: buffer,
	}))
	env := s.waitEnvelope("channel", func(d json.RawMessage) bool {
		var cd hub.ChannelData
		return json.Unmarshal(d, &cd) == nil && cd.Buffer == buffer
	})
	var cd hub.ChannelData
	json.Unmarshal(env.Data, &cd)
	return cd.Members
}
