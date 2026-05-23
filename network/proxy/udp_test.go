package proxy

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/network/endpoints"
	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
)

// memAddr is a string-based net.Addr.
type memAddr struct{ s string }

func (a memAddr) Network() string { return "mem" }
func (a memAddr) String() string  { return a.s }

// memPacket holds one buffered datagram + its sender.
type memPacket struct {
	data []byte
	addr net.Addr
}

// memPacketConn is an in-memory net.PacketConn with a buffered
// channel of inbound datagrams. WriteTo enqueues on a peer channel
// supplied by the test harness.
type memPacketConn struct {
	mu     sync.Mutex
	closed bool
	in     chan memPacket
	out    chan memPacket // queue of WriteTo'd packets; tests read these
	addr   net.Addr
	rdline atomic.Value // time.Time
}

func newMemPacketConn(addr string) *memPacketConn {
	return &memPacketConn{
		in:   make(chan memPacket, 128),
		out:  make(chan memPacket, 128),
		addr: memAddr{s: addr},
	}
}

func (c *memPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	var deadlineCh <-chan time.Time
	if dl, ok := c.rdline.Load().(time.Time); ok && !dl.IsZero() {
		// Use a one-shot timer keyed off the stored deadline.
		now := time.Now()
		if dl.Before(now) {
			return 0, nil, &timeoutErr{}
		}
		t := time.NewTimer(dl.Sub(now))
		defer t.Stop()
		deadlineCh = t.C
	}
	select {
	case pkt, ok := <-c.in:
		if !ok {
			return 0, nil, net.ErrClosed
		}
		n := copy(p, pkt.data)
		return n, pkt.addr, nil
	case <-deadlineCh:
		return 0, nil, &timeoutErr{}
	}
}

func (c *memPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	c.mu.Unlock()
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case c.out <- memPacket{data: cp, addr: addr}:
		return len(p), nil
	default:
		return 0, errors.New("memPacketConn: out queue full")
	}
}

func (c *memPacketConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.in)
	return nil
}

func (c *memPacketConn) LocalAddr() net.Addr               { return c.addr }
func (c *memPacketConn) SetDeadline(t time.Time) error     { c.rdline.Store(t); return nil }
func (c *memPacketConn) SetReadDeadline(t time.Time) error { c.rdline.Store(t); return nil }
func (c *memPacketConn) SetWriteDeadline(time.Time) error  { return nil }

// pushFromClient enqueues an inbound datagram from a virtual client.
func (c *memPacketConn) pushFromClient(data []byte, from string) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	c.in <- memPacket{data: data, addr: memAddr{s: from}}
}

// drainOut reads one datagram from out with a timeout.
func (c *memPacketConn) drainOut(d time.Duration) (memPacket, bool) {
	select {
	case p := <-c.out:
		return p, true
	case <-time.After(d):
		return memPacket{}, false
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// memPacketDialer hands out a memPacketConn per dial.
type memPacketDialer struct {
	mu       sync.Mutex
	servers  map[string]*memPacketConn
	failures map[string]error
	dials    atomic.Int64
}

func newMemPacketDialer() *memPacketDialer {
	return &memPacketDialer{
		servers:  make(map[string]*memPacketConn),
		failures: make(map[string]error),
	}
}

func (d *memPacketDialer) DialContext(_ context.Context, _, addr string) (net.PacketConn, error) {
	d.dials.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err, ok := d.failures[addr]; ok {
		return nil, err
	}
	s, ok := d.servers[addr]
	if !ok {
		return nil, errors.New("memPacketDialer: no server for " + addr)
	}
	return s, nil
}

func (d *memPacketDialer) Register(addr string, server *memPacketConn) {
	d.mu.Lock()
	d.servers[addr] = server
	d.mu.Unlock()
}

// --- tests ---

func TestUDP_BidirectionalForwarding(t *testing.T) {
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour))
	defer reg.Stop()
	insertReplicaUDP(reg, "api", "r1", "10.0.0.5", 9090)

	sel := NewSelector(WithRegistry(reg))
	stats := newStats()
	resolver := newStaticResolver(snapshotFor(map[string]int32{"api": 100}))
	dialer := newMemPacketDialer()
	backend := newMemPacketConn("backend-1")
	dialer.Register("10.0.0.5:9090", backend)

	ul := NewUDPListener(
		WithUDPSelector(sel),
		WithUDPResolver(resolver),
		WithUDPStats(stats),
		WithUDPDialer(dialer),
		WithUDPPortName("http"),
		WithUDPIdleTimeout(time.Second),
		WithUDPSweepInterval(50*time.Millisecond),
	)
	clientFacing := newMemPacketConn("local")
	if err := ul.Start(clientFacing); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ul.Stop()

	clientFacing.pushFromClient([]byte("ping"), "client-a:1111")

	// Expect backend to receive a packet.
	got, ok := backend.drainOut(time.Second)
	if !ok {
		t.Fatalf("backend did not receive packet")
	}
	if string(got.data) != "ping" {
		t.Fatalf("backend got %q", got.data)
	}

	// Simulate backend replying.
	backend.in <- memPacket{data: []byte("pong"), addr: memAddr{s: "backend"}}

	// Expect proxy to forward reply to client.
	reply, ok := clientFacing.drainOut(time.Second)
	if !ok {
		t.Fatalf("client did not receive reply")
	}
	if string(reply.data) != "pong" {
		t.Fatalf("client got %q", reply.data)
	}
	if reply.addr.String() != "client-a:1111" {
		t.Fatalf("reply addr = %q", reply.addr.String())
	}
}

func TestUDP_FlowStickiness(t *testing.T) {
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour))
	defer reg.Stop()
	insertReplicaUDP(reg, "v1", "r1", "10.0.0.1", 9090)
	insertReplicaUDP(reg, "v2", "r1", "10.0.0.2", 9090)

	sel := NewSelector(WithRegistry(reg))
	stats := newStats()
	resolver := newStaticResolver(snapshotFor(map[string]int32{"v1": 50, "v2": 50}))
	dialer := newMemPacketDialer()
	v1 := newMemPacketConn("v1")
	v2 := newMemPacketConn("v2")
	dialer.Register("10.0.0.1:9090", v1)
	dialer.Register("10.0.0.2:9090", v2)

	ul := NewUDPListener(
		WithUDPSelector(sel),
		WithUDPResolver(resolver),
		WithUDPStats(stats),
		WithUDPDialer(dialer),
		WithUDPPortName("http"),
		WithUDPIdleTimeout(time.Second),
		WithUDPSweepInterval(50*time.Millisecond),
	)
	clientFacing := newMemPacketConn("local")
	if err := ul.Start(clientFacing); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ul.Stop()

	// One client sends three datagrams; all must go to the same backend.
	for i := 0; i < 3; i++ {
		clientFacing.pushFromClient([]byte("hello"), "client-sticky:2222")
	}

	v1Got, v2Got := 0, 0
	deadline := time.Now().Add(2 * time.Second)
	for v1Got+v2Got < 3 && time.Now().Before(deadline) {
		select {
		case <-v1.out:
			v1Got++
		case <-v2.out:
			v2Got++
		case <-time.After(50 * time.Millisecond):
		}
	}
	if v1Got+v2Got != 3 {
		t.Fatalf("expected 3 packets received total, got v1=%d v2=%d", v1Got, v2Got)
	}
	if v1Got > 0 && v2Got > 0 {
		t.Fatalf("flow stickiness broken: v1=%d v2=%d", v1Got, v2Got)
	}
}

func TestUDP_FlowExpiresAfterIdle(t *testing.T) {
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour))
	defer reg.Stop()
	insertReplicaUDP(reg, "api", "r1", "10.0.0.5", 9090)

	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	sel := NewSelector(WithRegistry(reg))
	stats := newStats()
	resolver := newStaticResolver(snapshotFor(map[string]int32{"api": 100}))
	dialer := newMemPacketDialer()
	backend := newMemPacketConn("backend-x")
	dialer.Register("10.0.0.5:9090", backend)

	ul := NewUDPListener(
		WithUDPSelector(sel),
		WithUDPResolver(resolver),
		WithUDPStats(stats),
		WithUDPDialer(dialer),
		WithUDPPortName("http"),
		WithUDPIdleTimeout(500*time.Millisecond),
		WithUDPSweepInterval(50*time.Millisecond),
		WithUDPNowFunc(clk.Now),
	)
	clientFacing := newMemPacketConn("local")
	if err := ul.Start(clientFacing); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ul.Stop()

	clientFacing.pushFromClient([]byte("x"), "client-idle:3333")
	if _, ok := backend.drainOut(time.Second); !ok {
		t.Fatalf("first packet not delivered")
	}
	waitFor(t, time.Second, func() bool { return ul.ActiveFlows() >= 1 })

	// Advance virtual clock past idle window.
	clk.Advance(2 * time.Second)
	waitFor(t, 2*time.Second, func() bool { return ul.ActiveFlows() == 0 })
}

func TestUDP_MultiFlowConcurrency(t *testing.T) {
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour))
	defer reg.Stop()
	insertReplicaUDP(reg, "api", "r1", "10.0.0.5", 9090)

	sel := NewSelector(WithRegistry(reg))
	stats := newStats()
	resolver := newStaticResolver(snapshotFor(map[string]int32{"api": 100}))
	dialer := newMemPacketDialer()
	backend := newMemPacketConn("backend-multi")
	dialer.Register("10.0.0.5:9090", backend)

	ul := NewUDPListener(
		WithUDPSelector(sel),
		WithUDPResolver(resolver),
		WithUDPStats(stats),
		WithUDPDialer(dialer),
		WithUDPPortName("http"),
		WithUDPIdleTimeout(2*time.Second),
		WithUDPSweepInterval(50*time.Millisecond),
	)
	clientFacing := newMemPacketConn("local")
	if err := ul.Start(clientFacing); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ul.Stop()

	// Each "client" pushes one packet; same backend mock for all flows.
	const N = 20
	for i := 0; i < N; i++ {
		clientFacing.pushFromClient([]byte("x"), "client-"+intToStr(i))
	}
	// Drain N packets on backend out — they all share the single
	// backend mock, but each flow has its own backendConn (also the
	// same mock since we re-register the same conn). Verify that ALL
	// N inbound datagrams were forwarded.
	got := 0
	deadline := time.Now().Add(2 * time.Second)
	for got < N && time.Now().Before(deadline) {
		select {
		case <-backend.out:
			got++
		case <-time.After(50 * time.Millisecond):
		}
	}
	if got != N {
		t.Fatalf("expected %d packets forwarded, got %d", N, got)
	}
	if ul.ActiveFlows() < N {
		t.Fatalf("ActiveFlows = %d, want %d", ul.ActiveFlows(), N)
	}
}

func TestUDP_StopClosesEverything(t *testing.T) {
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour))
	defer reg.Stop()
	insertReplicaUDP(reg, "api", "r1", "10.0.0.5", 9090)

	sel := NewSelector(WithRegistry(reg))
	stats := newStats()
	resolver := newStaticResolver(snapshotFor(map[string]int32{"api": 100}))
	dialer := newMemPacketDialer()
	backend := newMemPacketConn("backend-stop")
	dialer.Register("10.0.0.5:9090", backend)

	ul := NewUDPListener(
		WithUDPSelector(sel),
		WithUDPResolver(resolver),
		WithUDPStats(stats),
		WithUDPDialer(dialer),
		WithUDPPortName("http"),
		WithUDPIdleTimeout(time.Minute),
		WithUDPSweepInterval(50*time.Millisecond),
	)
	clientFacing := newMemPacketConn("local")
	if err := ul.Start(clientFacing); err != nil {
		t.Fatalf("Start: %v", err)
	}

	clientFacing.pushFromClient([]byte("x"), "client-stop:5555")
	if _, ok := backend.drainOut(time.Second); !ok {
		t.Fatalf("first packet not delivered")
	}
	waitFor(t, time.Second, func() bool { return ul.ActiveFlows() >= 1 })

	done := make(chan struct{})
	go func() {
		_ = ul.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Stop hung")
	}
	if ul.ActiveFlows() != 0 {
		t.Fatalf("ActiveFlows after Stop = %d", ul.ActiveFlows())
	}
}

func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// insertReplicaUDP adds an alive replica with one "http" port on udp.
func insertReplicaUDP(r *endpoints.Registry, capsule, replicaID, bridgeIP string, port uint32) {
	r.Insert(&endpointpb.EndpointRecord{
		ClusterPath: "c1",
		GroupId:     "g1",
		CapsuleName: capsule,
		ReplicaId:   replicaID,
		BridgeIp:    bridgeIP,
		SwimState:   endpoints.SwimStateAlive,
		NamedPorts: []*endpointpb.NamedPort{
			{Name: "http", ContainerPort: port, Protocol: "udp"},
		},
		EmittedAt:  timestamppb.Now(),
		TtlSeconds: 3600,
	})
}
