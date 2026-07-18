package wgdial

import (
	"net"
	"net/netip"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
)

// v4Bind is a minimal IPv4-only conn.Bind. wireguard-go's default bind opens
// BOTH an AF_INET and an AF_INET6 UDP socket and treats a v6 bind failure that
// is not EAFNOSUPPORT — notably EADDRNOTAVAIL, returned when the kernel has
// net.ipv6.conf.all.disable_ipv6=1 (common on budget VPSes) — as fatal, so the
// device never comes up on a v6-disabled host. When the peer endpoint is IPv4
// we don't need v6 at all, so New falls back to this bind (see chooseBind). It
// forgoes the default bind's GSO/offload batching (BatchSize 1) and sticky
// source addresses — both irrelevant at IRC egress rates.
type v4Bind struct {
	mu sync.Mutex
	uc *net.UDPConn
}

var _ conn.Bind = (*v4Bind)(nil)

func newV4Bind() *v4Bind { return &v4Bind{} }

func (b *v4Bind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.uc != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	uc, err := net.ListenUDP("udp4", &net.UDPAddr{Port: int(port)})
	if err != nil {
		return nil, 0, err
	}
	b.uc = uc
	actual := uint16(uc.LocalAddr().(*net.UDPAddr).Port)
	return []conn.ReceiveFunc{b.receive}, actual, nil
}

// receive reads one datagram into bufs[0] (BatchSize is 1). The source address
// is reported as an IPv4 StdNetEndpoint so the device's roaming logic matches
// it against the peer's v4 endpoint.
func (b *v4Bind) receive(bufs [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
	b.mu.Lock()
	uc := b.uc
	b.mu.Unlock()
	if uc == nil {
		return 0, net.ErrClosed
	}
	n, ap, err := uc.ReadFromUDPAddrPort(bufs[0])
	if err != nil {
		return 0, err
	}
	sizes[0] = n
	eps[0] = &conn.StdNetEndpoint{AddrPort: netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())}
	return 1, nil
}

func (b *v4Bind) Send(bufs [][]byte, ep conn.Endpoint) error {
	ne, ok := ep.(*conn.StdNetEndpoint)
	if !ok {
		return conn.ErrWrongEndpointType
	}
	b.mu.Lock()
	uc := b.uc
	b.mu.Unlock()
	if uc == nil {
		return net.ErrClosed
	}
	for _, buf := range bufs {
		if _, err := uc.WriteToUDPAddrPort(buf, ne.AddrPort); err != nil {
			return err
		}
	}
	return nil
}

func (b *v4Bind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.uc == nil {
		return nil
	}
	err := b.uc.Close()
	b.uc = nil
	return err
}

func (b *v4Bind) SetMark(uint32) error { return nil }
func (b *v4Bind) BatchSize() int       { return 1 }

func (b *v4Bind) ParseEndpoint(s string) (conn.Endpoint, error) {
	e, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, err
	}
	return &conn.StdNetEndpoint{AddrPort: e}, nil
}

// chooseBind picks the WireGuard UDP bind. When the endpoint is IPv4 and a
// wildcard IPv6 UDP socket — the exact listen the default bind performs — cannot
// be opened, fall back to the v4-only bind so a v6-disabled host still brings
// the device up. Healthy dual-stack (or v6-only-endpoint) hosts keep the
// optimized default bind.
func chooseBind(resolvedEndpoint string) conn.Bind {
	if endpointIsV4(resolvedEndpoint) && !ipv6Bindable() {
		return newV4Bind()
	}
	return conn.NewDefaultBind()
}

func endpointIsV4(resolvedEndpoint string) bool {
	host, _, err := net.SplitHostPort(resolvedEndpoint)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && (ip.Is4() || ip.Is4In6())
}

// ipv6Bindable reports whether a wildcard IPv6 UDP socket can be opened — the
// same operation the default bind performs internally — so we can predict a v6
// bind failure before committing the device to the default bind.
func ipv6Bindable() bool {
	c, err := net.ListenPacket("udp6", ":0")
	if err != nil {
		return false
	}
	c.Close()
	return true
}
