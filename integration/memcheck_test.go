//go:build memcheck

// The CLAUDE.md memory scenario: the real binary under GOMEMLIMIT=64MiB
// carrying 5 networks / 50 channels / 10k messages of hot scrollback
// (50 × the 200-message ring default) must stay under 72 MB RSS.
// Run with `make memcheck` — before releases and after changes to
// buffering, caching, or the store.
package integration

import (
	"bufio"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const (
	memNetworks    = 5
	memChannels    = 10 // per network
	memMsgsPerChan = 200
	rssBudgetKB    = 72 * 1024
)

// fakeIRCServer accepts one client, walks it through a minimal
// registration, echoes its JOINs, and then blasts scrollback at it.
type fakeIRCServer struct {
	ln     net.Listener
	joined atomic.Int64

	mu   sync.Mutex // guards conn writes (serve replies vs. blast)
	conn net.Conn
}

func (f *fakeIRCServer) writeString(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn == nil {
		return fmt.Errorf("no client connected")
	}
	_, err := f.conn.Write([]byte(s))
	return err
}

func startFakeIRC(t *testing.T) *fakeIRCServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeIRCServer{ln: ln}
	go f.serve(t)
	return f
}

func (f *fakeIRCServer) serve(t *testing.T) {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()
	nick := "*"
	send := func(line string) {
		_ = f.writeString(line + "\r\n")
	}
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "CAP":
			if len(fields) >= 2 && fields[1] == "LS" {
				send(":srv CAP * LS :") // no capabilities: shortest handshake
			}
		case "NICK":
			nick = fields[1]
		case "USER":
			send(fmt.Sprintf(":srv 001 %s :welcome", nick))
		case "JOIN":
			send(fmt.Sprintf(":%s!u@h JOIN %s", nick, fields[1]))
			f.joined.Add(1)
		case "PING":
			send(":srv PONG srv " + strings.TrimPrefix(fields[1], ":"))
		}
	}
}

// startBlast writes the scrollback: memMsgsPerChan messages to each
// channel, ~90 bytes of realistic payload each, interleaved across
// channels the way live traffic arrives.
func (f *fakeIRCServer) startBlast(t *testing.T, channels []string) {
	var b strings.Builder
	for i := 0; i < memMsgsPerChan; i++ {
		for _, ch := range channels {
			fmt.Fprintf(&b, ":user%d!u@example.com PRIVMSG %s :line %04d — the quick brown fox jumps over the lazy dog\r\n",
				i%20, ch, i)
		}
	}
	if err := f.writeString(b.String()); err != nil {
		t.Errorf("blast: %v", err)
	}
}

func TestMemoryScenario(t *testing.T) {
	bin := os.Getenv("IRCTHING_BIN")
	if bin == "" {
		t.Fatal("IRCTHING_BIN not set (run via `make memcheck`)")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mem.db")

	// 5 fake networks, 10 channels each.
	var servers []*fakeIRCServer
	var networks []string
	channels := make([]string, memChannels)
	for i := range channels {
		channels[i] = fmt.Sprintf("#mem%d", i)
	}
	for i := 0; i < memNetworks; i++ {
		f := startFakeIRC(t)
		servers = append(servers, f)
		networks = append(networks, fmt.Sprintf(
			`{"name":"net%d","addr":"%s","allow_plaintext":true,"nick":"memuser","channels":[%s]}`,
			i, f.ln.Addr(), `"`+strings.Join(channels, `","`)+`"`))
	}
	cfg := fmt.Sprintf(`{
		"listen": "127.0.0.1:0",
		"database": %q,
		"user": {"username": "mem", "password_hash": "$2a$10$/vXcvxwnd0BAE188Vf9aSOFQFZeGKQsf1817JpdYiDhibk6nh7QQ."},
		"networks": [%s]
	}`, dbPath, strings.Join(networks, ","))
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-config", cfgPath)
	cmd.Env = append(os.Environ(), "GOMEMLIMIT=64MiB")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	// Wait until every network has joined all its channels, then blast.
	deadline := time.Now().Add(60 * time.Second)
	for _, f := range servers {
		for f.joined.Load() < memChannels {
			if time.Now().After(deadline) {
				t.Fatalf("client never joined all channels (%d/%d)", f.joined.Load(), memChannels)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	for _, f := range servers {
		f.startBlast(t, channels)
	}

	// All 10k messages persisted (read-only connection; WAL allows it).
	ro, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	want := memNetworks * memChannels * memMsgsPerChan
	deadline = time.Now().Add(120 * time.Second)
	for {
		var n int
		_ = ro.QueryRow(`SELECT count(*) FROM messages`).Scan(&n)
		if n >= want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("persisted %d/%d messages", n, want)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Let allocation churn settle, then measure.
	time.Sleep(3 * time.Second)
	rss, hwm := readRSS(t, cmd.Process.Pid)
	t.Logf("RSS = %.1f MB, peak = %.1f MB (budget %.0f MB, GOMEMLIMIT=64MiB)",
		float64(rss)/1024, float64(hwm)/1024, float64(rssBudgetKB)/1024)
	if rss > rssBudgetKB {
		t.Fatalf("RSS %d kB exceeds the %d kB budget", rss, rssBudgetKB)
	}
}

// Adversarial scenario for the hot-ring global byte budget (store #5): one
// hostile network floods many channels with near-max-size messages. The total
// hot-ring payload without the budget would be advChannels*advMsgs*advMsgBytes
// (~75 MB), far past the 16 MiB budget; LRU eviction must keep the process RSS
// under the 72 MB target.
const (
	advChannels = 24
	advMsgs     = 200   // fill each ring (DefaultRingSize)
	advMsgBytes = 16000 // near the 16 KiB per-message clamp
)

// startBigBlast writes advMsgs rounds of one near-max-size PRIVMSG per channel,
// one round per write so the test never builds the whole flood in memory.
func (f *fakeIRCServer) startBigBlast(t *testing.T, channels []string) {
	body := strings.Repeat("x", advMsgBytes)
	for i := 0; i < advMsgs; i++ {
		var b strings.Builder
		for _, ch := range channels {
			fmt.Fprintf(&b, ":user%d!u@example.com PRIVMSG %s :%s\r\n", i%20, ch, body)
		}
		if err := f.writeString(b.String()); err != nil {
			t.Errorf("big blast: %v", err)
			return
		}
	}
}

func TestMemoryScenarioAdversarial(t *testing.T) {
	bin := os.Getenv("IRCTHING_BIN")
	if bin == "" {
		t.Fatal("IRCTHING_BIN not set (run via `make memcheck`)")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "adv.db")

	f := startFakeIRC(t)
	channels := make([]string, advChannels)
	for i := range channels {
		channels[i] = fmt.Sprintf("#adv%d", i)
	}
	cfg := fmt.Sprintf(`{
		"listen": "127.0.0.1:0",
		"database": %q,
		"user": {"username": "mem", "password_hash": "$2a$10$/vXcvxwnd0BAE188Vf9aSOFQFZeGKQsf1817JpdYiDhibk6nh7QQ."},
		"networks": [{"name":"adv","addr":"%s","allow_plaintext":true,"nick":"memuser","channels":[%s]}]
	}`, dbPath, f.ln.Addr(), `"`+strings.Join(channels, `","`)+`"`)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-config", cfgPath)
	cmd.Env = append(os.Environ(), "GOMEMLIMIT=64MiB")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	deadline := time.Now().Add(60 * time.Second)
	for f.joined.Load() < advChannels {
		if time.Now().After(deadline) {
			t.Fatalf("client never joined all channels (%d/%d)", f.joined.Load(), advChannels)
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.startBigBlast(t, channels)

	ro, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	want := advChannels * advMsgs
	deadline = time.Now().Add(180 * time.Second)
	for {
		var n int
		_ = ro.QueryRow(`SELECT count(*) FROM messages`).Scan(&n)
		if n >= want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("persisted %d/%d messages", n, want)
		}
		time.Sleep(100 * time.Millisecond)
	}

	time.Sleep(3 * time.Second)
	rss, hwm := readRSS(t, cmd.Process.Pid)
	t.Logf("adversarial RSS = %.1f MB, peak = %.1f MB (budget %.0f MB, ~75 MB flood, 16 MiB ring budget)",
		float64(rss)/1024, float64(hwm)/1024, float64(rssBudgetKB)/1024)
	if rss > rssBudgetKB {
		t.Fatalf("adversarial RSS %d kB exceeds the %d kB budget — hot-ring LRU not bounding memory", rss, rssBudgetKB)
	}
}

// readRSS returns VmRSS and VmHWM in kB from /proc.
func readRSS(t *testing.T, pid int) (rss, hwm int) {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		var dst *int
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			dst = &rss
		case strings.HasPrefix(line, "VmHWM:"):
			dst = &hwm
		default:
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 {
			*dst, _ = strconv.Atoi(f[1])
		}
	}
	if rss == 0 {
		t.Fatal("could not read VmRSS")
	}
	return rss, hwm
}
