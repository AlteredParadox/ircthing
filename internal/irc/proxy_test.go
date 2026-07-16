package irc

import (
	"bufio"
	"encoding/base64"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSOCKS5 is a minimal RFC 1928/1929 server for tests: it records the
// requested target and pipes the tunnel to upstream.
type fakeSOCKS5 struct {
	ln       net.Listener
	upstream string // where tunnels actually connect
	user     string // require RFC 1929 auth when non-empty
	pass     string

	mu      sync.Mutex
	targets []string // "host:port" as requested by the client
}

func startFakeSOCKS5(t *testing.T, upstream, user, pass string) *fakeSOCKS5 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeSOCKS5{ln: ln, upstream: upstream, user: user, pass: pass}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeSOCKS5) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.targets...)
}

func (f *fakeSOCKS5) serve(conn net.Conn) {
	defer conn.Close()
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 0x05 {
		return
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if f.user != "" {
		conn.Write([]byte{0x05, 0x02})
		auth := make([]byte, 2)
		if _, err := io.ReadFull(conn, auth); err != nil || auth[0] != 0x01 {
			return
		}
		u := make([]byte, auth[1])
		io.ReadFull(conn, u)
		plen := make([]byte, 1)
		io.ReadFull(conn, plen)
		p := make([]byte, plen[0])
		io.ReadFull(conn, p)
		if string(u) != f.user || string(p) != f.pass {
			conn.Write([]byte{0x01, 0x01}) // rejected
			return
		}
		conn.Write([]byte{0x01, 0x00})
	} else {
		conn.Write([]byte{0x05, 0x00})
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil || req[1] != 0x01 {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		b := make([]byte, 4)
		io.ReadFull(conn, b)
		host = net.IP(b).String()
	case 0x03:
		n := make([]byte, 1)
		io.ReadFull(conn, n)
		b := make([]byte, n[0])
		io.ReadFull(conn, b)
		host = string(b)
	case 0x04:
		b := make([]byte, 16)
		io.ReadFull(conn, b)
		host = net.IP(b).String()
	}
	pb := make([]byte, 2)
	io.ReadFull(conn, pb)
	port := int(pb[0])<<8 | int(pb[1])
	target := net.JoinHostPort(host, strconv.Itoa(port))
	f.mu.Lock()
	f.targets = append(f.targets, target)
	f.mu.Unlock()

	up, err := net.Dial("tcp", f.upstream)
	if err != nil {
		conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // refused
		return
	}
	defer up.Close()
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0})
	go io.Copy(up, conn)
	io.Copy(conn, up)
}

// fakeHTTPConnect is a minimal CONNECT-tunnel proxy for tests.
type fakeHTTPConnect struct {
	ln       net.Listener
	upstream string
	auth     string // required Proxy-Authorization value, "" for none

	mu      sync.Mutex
	targets []string
}

func startFakeHTTPConnect(t *testing.T, upstream, user, pass string) *fakeHTTPConnect {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeHTTPConnect{ln: ln, upstream: upstream}
	if user != "" {
		f.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeHTTPConnect) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.targets...)
}

func (f *fakeHTTPConnect) serve(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "CONNECT" {
		conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return
	}
	target := fields[1]
	gotAuth := ""
	for {
		h, err := br.ReadString('\n')
		if err != nil || strings.TrimSpace(h) == "" {
			break
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(h), "Proxy-Authorization: "); ok {
			gotAuth = v
		}
	}
	if f.auth != "" && gotAuth != f.auth {
		conn.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
		return
	}
	f.mu.Lock()
	f.targets = append(f.targets, target)
	f.mu.Unlock()
	up, err := net.Dial("tcp", f.upstream)
	if err != nil {
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer up.Close()
	conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	go io.Copy(up, conn)
	io.Copy(conn, up)
}

// echoOnce starts a listener whose first connection receives greeting
// and is then closed.
func echoOnce(t *testing.T, greeting string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte(greeting))
			c.Close()
		}
	}()
	return ln
}

func TestParseProxyURL(t *testing.T) {
	for _, ok := range []string{
		"socks5://127.0.0.1:1080",
		"socks5h://tor:9050",
		"socks5://alice:pw@10.0.0.1:1080",
		"http://proxy.corp:3128",
	} {
		if _, err := parseProxyURL(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"ftp://x:21",         // wrong scheme
		"socks5://noport",    // missing port
		"http://p:3128/path", // junk path
		"://",
	} {
		if _, err := parseProxyURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDialProxySOCKS5(t *testing.T) {
	up := echoOnce(t, "hello via socks\r\n")
	f := startFakeSOCKS5(t, up.Addr().String(), "", "")
	u, _ := parseProxyURL("socks5://" + f.ln.Addr().String())

	conn, err := dialProxy(t.Context(), u, "localhost:6667", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(line, "hello via socks") {
		t.Fatalf("tunnel read = %q, %v", line, err)
	}
	// The hostname went to the proxy unresolved (ATYP domain).
	if got := f.requested(); len(got) != 1 || got[0] != "localhost:6667" {
		t.Fatalf("proxy saw %v, want localhost:6667", got)
	}
}

func TestDialProxySOCKS5Auth(t *testing.T) {
	up := echoOnce(t, "authed\r\n")
	f := startFakeSOCKS5(t, up.Addr().String(), "alice", "sekrit")

	good, _ := parseProxyURL("socks5://alice:sekrit@" + f.ln.Addr().String())
	conn, err := dialProxy(t.Context(), good, "irc.test:6667", 5*time.Second)
	if err != nil {
		t.Fatalf("auth dial: %v", err)
	}
	conn.Close()

	bad, _ := parseProxyURL("socks5://alice:wrong@" + f.ln.Addr().String())
	if c, err := dialProxy(t.Context(), bad, "irc.test:6667", 5*time.Second); err == nil {
		c.Close()
		t.Fatal("wrong password accepted")
	} else if !strings.Contains(err.Error(), "authentication rejected") {
		t.Fatalf("unexpected error: %v", err)
	}

	// No credentials against an auth-requiring proxy fails cleanly.
	none, _ := parseProxyURL("socks5://" + f.ln.Addr().String())
	if c, err := dialProxy(t.Context(), none, "irc.test:6667", 5*time.Second); err == nil {
		c.Close()
		t.Fatal("credential-less dial accepted")
	}
}

func TestDialProxyHTTPConnect(t *testing.T) {
	up := echoOnce(t, "hello via http\r\n")
	f := startFakeHTTPConnect(t, up.Addr().String(), "bob", "hunter2")

	u, _ := parseProxyURL("http://bob:hunter2@" + f.ln.Addr().String())
	conn, err := dialProxy(t.Context(), u, "irc.test:6697", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if !strings.Contains(line, "hello via http") {
		t.Fatalf("tunnel read = %q", line)
	}
	if got := f.requested(); len(got) != 1 || got[0] != "irc.test:6697" {
		t.Fatalf("proxy saw %v", got)
	}

	bad, _ := parseProxyURL("http://bob:wrong@" + f.ln.Addr().String())
	if c, err := dialProxy(t.Context(), bad, "irc.test:6697", 5*time.Second); err == nil {
		c.Close()
		t.Fatal("wrong password accepted")
	} else if !strings.Contains(err.Error(), "CONNECT refused") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestManagerViaSOCKS5 registers a full IRC session through the proxy:
// the manager must never touch the server address directly.
func TestManagerViaSOCKS5(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := listen(t, ln)
	f := startFakeSOCKS5(t, ln.Addr().String(), "", "")

	cfg := testCfg("localhost:6667") // resolved by the proxy, not us
	cfg.Proxy = "socks5://" + f.ln.Addr().String()
	m := startManager(t, cfg)
	s := accept(t, conns)
	s.register("AlteredParadox")
	waitState(t, m, StateRegistered)

	if got := f.requested(); len(got) != 1 || !strings.HasPrefix(got[0], "localhost:") {
		t.Fatalf("proxy saw %v, want a localhost:6667 tunnel", got)
	}
}
