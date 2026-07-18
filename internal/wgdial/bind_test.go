package wgdial

import (
	"net"
	"strconv"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// The v4-only bind's datapath: it must Send to and receive from a plain UDP
// peer over loopback, satisfying the conn.Bind contract the device relies on.
func TestV4BindRoundTrip(t *testing.T) {
	b := newV4Bind()
	fns, port, err := b.Open(0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer b.Close()
	if len(fns) != 1 {
		t.Fatalf("Open returned %d receive fns, want 1", len(fns))
	}

	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("peer listen: %v", err)
	}
	defer peer.Close()
	peerPort := peer.LocalAddr().(*net.UDPAddr).Port

	// bind -> peer
	ep, err := b.ParseEndpoint("127.0.0.1:" + strconv.Itoa(peerPort))
	if err != nil {
		t.Fatalf("ParseEndpoint: %v", err)
	}
	if err := b.Send([][]byte{[]byte("ping")}, ep); err != nil {
		t.Fatalf("Send: %v", err)
	}
	buf := make([]byte, 16)
	peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := peer.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("peer read = %q, %v; want \"ping\"", buf[:n], err)
	}

	// peer -> bind, delivered through the ReceiveFunc.
	if _, err := peer.WriteToUDP([]byte("pong"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)}); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	type res struct {
		n   int
		err error
	}
	ch := make(chan res, 1)
	bufs := [][]byte{make([]byte, 16)}
	sizes := []int{0}
	eps := make([]conn.Endpoint, 1)
	go func() {
		_, err := fns[0](bufs, sizes, eps)
		ch <- res{sizes[0], err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("receive: %v", r.err)
		}
		if string(bufs[0][:r.n]) != "pong" {
			t.Fatalf("bind received %q, want \"pong\"", bufs[0][:r.n])
		}
		sne, ok := eps[0].(*conn.StdNetEndpoint)
		if !ok || int(sne.Port()) != peerPort {
			t.Fatalf("reported endpoint = %v, want port %d", eps[0], peerPort)
		}
	case <-time.After(3 * time.Second):
		b.Close() // unblock the blocked ReadFromUDPAddrPort
		t.Fatal("receive timed out")
	}

	// A second Open without Close must be refused.
	if _, _, err := b.Open(0); err != conn.ErrBindAlreadyOpen {
		t.Fatalf("second Open err = %v, want ErrBindAlreadyOpen", err)
	}
}

func TestEndpointIsV4(t *testing.T) {
	cases := map[string]bool{
		"203.0.113.7:51820":   true,
		"127.0.0.1:1":         true,
		"[2001:db8::1]:51820": false,
		"[::1]:51820":         false,
		"host.example:51820":  false, // not a literal IP
		"203.0.113.7":         false, // no port
	}
	for in, want := range cases {
		if got := endpointIsV4(in); got != want {
			t.Errorf("endpointIsV4(%q) = %v, want %v", in, got, want)
		}
	}
}
