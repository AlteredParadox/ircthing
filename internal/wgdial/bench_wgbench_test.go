//go:build wgbench

// Measurement harness for the WireGuard egress spike (phase-4 candidate).
// Excluded from the normal build/test by the wgbench tag; run explicitly:
//
//	go test -tags wgbench -run TestWGBench -v -timeout 900s ./internal/wgdial/
//
// It stands up a real loopback WireGuard pair — two userspace wireguard-go
// devices, each on its own gVisor netstack, peering over 127.0.0.1 UDP — so
// the Noise handshake actually completes and traffic really traverses the
// tunnel. That makes the RSS and CPU numbers representative of a live tunnel
// rather than of an idle, unhandshaken device.
//
// Env knobs:
//
//	WGBENCH_IDLE_SEC   idle window for keepalive CPU sampling (default 120)
//	WGBENCH_REPLAY_MB  bytes pushed through the tunnel for the replay CPU
//	                   figure (default 50)
package wgdial

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

type wgPeer struct {
	dev  *device.Device
	tnet *netstack.Net
}

func genKey(t *testing.T) (privHex, pubHex string) {
	var priv [32]byte
	if _, err := io.ReadFull(rand.Reader, priv[:]); err != nil {
		t.Fatal(err)
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(priv[:]), hex.EncodeToString(pub)
}

// bringUp creates one userspace WG device on its own netstack, listening on
// localhost:listenPort, peered with peerPub at 127.0.0.1:peerPort.
func bringUp(t *testing.T, ip string, listenPort int, privHex, peerPub string, peerPort int) *wgPeer {
	addr := netip.MustParseAddr(ip)
	dns := netip.MustParseAddr("10.0.0.53") // unused; we dial by IP
	tun, tnet, err := netstack.CreateNetTUN([]netip.Addr{addr}, []netip.Addr{dns}, 1420)
	if err != nil {
		t.Fatalf("CreateNetTUN(%s): %v", ip, err)
	}
	dev := device.NewDevice(tun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "wg("+ip+") "))
	uapi := fmt.Sprintf("private_key=%s\nlisten_port=%d\npublic_key=%s\nendpoint=127.0.0.1:%d\nallowed_ip=0.0.0.0/0\npersistent_keepalive_interval=25\n",
		privHex, listenPort, peerPub, peerPort)
	if err := dev.IpcSet(uapi); err != nil {
		t.Fatalf("IpcSet(%s): %v", ip, err)
	}
	if err := dev.Up(); err != nil {
		t.Fatalf("Up(%s): %v", ip, err)
	}
	return &wgPeer{dev: dev, tnet: tnet}
}

func vmRSSKB(t *testing.T) int {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "VmRSS:") {
			f := strings.Fields(ln)
			n, _ := strconv.Atoi(f[1])
			return n
		}
	}
	t.Fatal("VmRSS not found")
	return 0
}

func cpuNanos() int64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return ru.Utime.Nano() + ru.Stime.Nano()
}

func heapSysMB() (heapAlloc, sys float64) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.HeapAlloc) / 1048576, float64(ms.Sys) / 1048576
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func TestWGBench(t *testing.T) {
	idleSec := envInt("WGBENCH_IDLE_SEC", 120)
	replayMB := envInt("WGBENCH_REPLAY_MB", 50)

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	rssBase := vmRSSKB(t)
	haBase, sysBase := heapSysMB()
	t.Logf("RSS baseline (Go runtime + harness, no tunnel): %d KB (%.1f MB); heapAlloc %.1f MB, Sys %.1f MB",
		rssBase, float64(rssBase)/1024, haBase, sysBase)

	// Keys.
	aPriv, aPub := genKey(t)
	bPriv, bPub := genKey(t)

	// Device B: stands in for the remote exit + IRC server (inside tunnel).
	b := bringUp(t, "10.0.0.2", 51821, bPriv, aPub, 51820)
	defer b.dev.Close()
	runtime.GC()
	time.Sleep(300 * time.Millisecond)
	rssOne := vmRSSKB(t)
	t.Logf("RSS with ONE userspace WG device up (netstack, idle, no handshake yet): %d KB (%.1f MB) [+%d KB over baseline]",
		rssOne, float64(rssOne)/1024, rssOne-rssBase)

	// Device A: the client tunnel — what ircthing actually adds per network.
	a := bringUp(t, "10.0.0.1", 51820, aPriv, bPub, 51821)
	defer a.dev.Close()

	// Echo/drain server on B inside the tunnel (the "IRC server").
	ln, err := b.tnet.ListenTCPAddrPort(netip.MustParseAddrPort("10.0.0.2:9000"))
	if err != nil {
		t.Fatalf("listen in tunnel: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, c)
		}
	}()

	// Dial A->B through the tunnel; retry until the handshake completes.
	var tconn io.ReadWriteCloser
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, derr := a.tnet.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort("10.0.0.2:9000"))
		cancel()
		if derr == nil {
			tconn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tunnel never came up: %v", derr)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Log("handshake complete; one TCP connection open through the tunnel")

	runtime.GC()
	time.Sleep(300 * time.Millisecond)
	rssConn := vmRSSKB(t)
	haConn, sysConn := heapSysMB()
	t.Logf("RSS with tunnel up + 1 connection (TWO devices in-process): %d KB (%.1f MB)", rssConn, float64(rssConn)/1024)
	t.Logf("  per-client-tunnel incremental RSS (device+netstack+conn) ~= %d KB (%.2f MB)  [rssConn - rssOne]",
		rssConn-rssOne, float64(rssConn-rssOne)/1024)
	t.Logf("  Go heapAlloc %.1f MB, Sys %.1f MB (process-model independent)", haConn, sysConn)

	// Idle keepalive CPU over idleSec.
	t.Logf("sampling idle keepalive CPU for %d s ...", idleSec)
	cpu0 := cpuNanos()
	time.Sleep(time.Duration(idleSec) * time.Second)
	cpuIdle := cpuNanos() - cpu0
	t.Logf("idle CPU over %d s (BOTH devices, 25s keepalive): %.1f ms total = %.2f ms/s = %.1f ms/min (per device ~half)",
		idleSec, float64(cpuIdle)/1e6, float64(cpuIdle)/1e6/float64(idleSec), float64(cpuIdle)/1e6/float64(idleSec)*60)

	// Replay burst: push replayMB through the tunnel (chathistory-replay proxy).
	buf := make([]byte, 64*1024)
	total := int64(replayMB) * 1024 * 1024
	cpu1 := cpuNanos()
	start := time.Now()
	var written int64
	for written < total {
		n, werr := tconn.Write(buf)
		if werr != nil {
			t.Fatalf("write through tunnel: %v", werr)
		}
		written += int64(n)
	}
	wall := time.Since(start)
	cpuReplay := cpuNanos() - cpu1
	tconn.Close()
	mb := float64(written) / 1048576
	t.Logf("replay burst: %.0f MB through tunnel in %.2f s => %.0f MB/s; CPU %.0f ms total = %.2f ms/MB (both encrypt+decrypt sides in-process)",
		mb, wall.Seconds(), mb/wall.Seconds(), float64(cpuReplay)/1e6, float64(cpuReplay)/1e6/mb)

	rssPeak := vmRSSKB(t)
	haPeak, sysPeak := heapSysMB()
	t.Logf("RSS peak right after replay: %d KB (%.1f MB); heapAlloc %.1f MB, Sys %.1f MB", rssPeak, float64(rssPeak)/1024, haPeak, sysPeak)

	// How much of that is retained vs reclaimable? Force return to the OS.
	debug.FreeOSMemory()
	time.Sleep(500 * time.Millisecond)
	rssReclaimed := vmRSSKB(t)
	haRecl, _ := heapSysMB()
	t.Logf("RSS after FreeOSMemory (retained steady state): %d KB (%.1f MB); live heapAlloc %.1f MB",
		rssReclaimed, float64(rssReclaimed)/1024, haRecl)
}
