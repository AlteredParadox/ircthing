package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/bcrypt"

	"ircthing/internal/hub"
	"ircthing/internal/irc"
	"ircthing/internal/store"

	ircv4 "gopkg.in/irc.v4"
)

func newTestServer(t *testing.T) (*httptest.Server, *hub.Hub) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := hub.New(st)

	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Username: "AlteredParadox", PasswordHash: string(hash)}, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, h
}

func login(t *testing.T, ts *httptest.Server, username, password string) *http.Response {
	t.Helper()
	body := strings.NewReader(`{"username":"` + username + `","password":"` + password + `"}`)
	resp, err := http.Post(ts.URL+"/api/login", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func sessionCookieOf(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie in response")
	return nil
}

func TestLogin(t *testing.T) {
	ts, _ := newTestServer(t)
	cases := []struct {
		name, user, pass string
		wantStatus       int
	}{
		{"valid credentials", "AlteredParadox", "hunter2", http.StatusNoContent},
		{"wrong password", "AlteredParadox", "wrong", http.StatusUnauthorized},
		{"wrong username", "nobody", "hunter2", http.StatusUnauthorized},
		{"empty", "", "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := login(t, ts, tc.user, tc.pass)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusNoContent {
				c := sessionCookieOf(t, resp)
				if !c.HttpOnly || c.Value == "" {
					t.Fatalf("cookie not HttpOnly or empty: %+v", c)
				}
			}
		})
	}
}

func TestWSRequiresAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/ws")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	// A bogus cookie is rejected too.
	req, _ := http.NewRequest("GET", ts.URL+"/api/ws", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "forged"})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged cookie status = %d, want 401", resp.StatusCode)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	ts, _ := newTestServer(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	req, _ := http.NewRequest("POST", ts.URL+"/api/logout", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", ts.URL+"/api/ws", nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ws after logout status = %d, want 401", resp.StatusCode)
	}
}

// fakeConn implements hub.Conn so tests can inject IRC events through the
// real fan-out path.
type fakeConn struct {
	ch   chan irc.Event
	name string
	nick string
}

func (f *fakeConn) Events() <-chan irc.Event  { return f.ch }
func (f *fakeConn) Name() string              { return f.name }
func (f *fakeConn) Nick() string              { return f.nick }
func (f *fakeConn) Send(*ircv4.Message) error { return nil }

func (f *fakeConn) Channel(string) (string, []irc.Member, bool) {
	return "", nil, false
}

func (f *fakeConn) privmsg(line string) irc.Event {
	return irc.Event{
		Network: f.name,
		Kind:    irc.EventMessage,
		Msg:     ircv4.MustParseMessage(line),
		Time:    time.Now(),
	}
}

// TestWSEndToEnd drives the full path: login, upgrade, receive a live
// push fanned out from an IRC event, page history over the socket, and
// survive an unknown envelope type.
func TestWSEndToEnd(t *testing.T) {
	ts, h := newTestServer(t)
	cookie := sessionCookieOf(t, login(t, ts, "AlteredParadox", "hunter2"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := &fakeConn{ch: make(chan irc.Event, 4), name: "libera", nick: "AlteredParadox"}
	go h.Run(ctx, conn)

	header := http.Header{}
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	c, _, err := websocket.Dial(ctx, ts.URL+"/api/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	var env hub.Envelope
	readEnv := func() {
		t.Helper()
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("bad envelope: %v", err)
		}
	}

	// A message arriving from IRC is pushed live over the socket.
	conn.ch <- conn.privmsg(":alice!u@h PRIVMSG #go :hello")
	readEnv()
	if env.Type != "event" {
		t.Fatalf("expected event push, got %+v", env)
	}
	var ev hub.EventData
	if err := json.Unmarshal(env.Data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Raw == "" || ev.Sender != "alice" || ev.Buffer != "#go" {
		t.Fatalf("event = %+v", ev)
	}

	// The same message comes back from a history request.
	req, _ := json.Marshal(map[string]any{
		"v": 1, "type": "get_history", "seq": 42,
		"data": map[string]any{"network": "libera", "buffer": "#go", "limit": 10},
	})
	if err := c.Write(ctx, websocket.MessageText, req); err != nil {
		t.Fatal(err)
	}
	readEnv()
	if env.Type != "history" || env.Seq != 42 {
		t.Fatalf("envelope = %+v", env)
	}
	var page hub.HistoryData
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Sender != "alice" {
		t.Fatalf("history = %+v", page)
	}

	// An unknown envelope type is ignored, and the connection lives on to
	// deliver the next push.
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"v":1,"type":"future_thing"}`)); err != nil {
		t.Fatal(err)
	}
	conn.ch <- conn.privmsg(":bob!u@h PRIVMSG #go :live push")
	readEnv()
	if env.Type != "event" {
		t.Fatalf("expected event push after unknown type, got %+v", env)
	}
	if err := json.Unmarshal(env.Data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Sender != "bob" {
		t.Fatalf("event = %+v", ev)
	}
}
