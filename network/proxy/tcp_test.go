package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/network/endpoints"
	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
)

// pipeListener is an in-memory net.Listener backed by net.Pipe().
// Accept blocks until a test goroutine calls Connect, which returns
// the client end of a pipe and hands the server end to Accept.
type pipeListener struct {
	mu      sync.Mutex
	pending chan net.Conn
	closed  bool
}

func newPipeListener() *pipeListener {
	return &pipeListener{pending: make(chan net.Conn, 16)}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	c, ok := <-l.pending
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}

func (l *pipeListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.pending)
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

// Connect builds a paired client conn and returns it; the server end
// is queued for Accept.
func (l *pipeListener) Connect() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, net.ErrClosed
	}
	client, server := net.Pipe()
	l.pending <- server
	return client, nil
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// fakeDialer hands out pre-paired pipe ends keyed by address. The
// "server" end is what NewServer queues; DialContext returns the
// "client" end. Failures are injected via failAddrs.
type fakeDialer struct {
	mu       sync.Mutex
	servers  map[string]chan net.Conn // addr → channel of server conns
	failures map[string]error
	dials    atomic.Int64
}

func newFakeDialer() *fakeDialer {
	return &fakeDialer{
		servers:  make(map[string]chan net.Conn),
		failures: make(map[string]error),
	}
}

func (d *fakeDialer) DialContext(_ context.Context, _ string, addr string) (net.Conn, error) {
	d.dials.Add(1)
	d.mu.Lock()
	if err, ok := d.failures[addr]; ok {
		d.mu.Unlock()
		return nil, err
	}
	ch, ok := d.servers[addr]
	d.mu.Unlock()
	if !ok {
		return nil, errors.New("fakeDialer: no server registered for " + addr)
	}
	client, server := net.Pipe()
	select {
	case ch <- server:
	default:
		_ = server.Close()
		_ = client.Close()
		return nil, errors.New("fakeDialer: server queue full")
	}
	return client, nil
}

func (d *fakeDialer) RegisterServer(addr string) <-chan net.Conn {
	d.mu.Lock()
	defer d.mu.Unlock()
	ch, ok := d.servers[addr]
	if !ok {
		ch = make(chan net.Conn, 64)
		d.servers[addr] = ch
	}
	return ch
}

func (d *fakeDialer) FailNext(addr string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failures[addr] = err
}

func (d *fakeDialer) ClearFailure(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.failures, addr)
}

// staticResolver returns the same ServiceSnapshot on every Resolve.
type staticResolver struct {
	mu   sync.Mutex
	snap ServiceSnapshot
	ok   bool
}

func newStaticResolver(snap ServiceSnapshot) *staticResolver {
	return &staticResolver{snap: snap, ok: true}
}

func (r *staticResolver) Resolve() (ServiceSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snap, r.ok
}

func (r *staticResolver) SetOK(ok bool) {
	r.mu.Lock()
	r.ok = ok
	r.mu.Unlock()
}

// runEcho reads everything from c and writes it back. Returns when c
// closes.
func runEcho(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if _, werr := c.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// insertReplicaTCP is a tiny helper used by tcp/udp tests that adds
// an alive replica with one named port "http".
func insertReplicaTCP(r *endpoints.Registry, capsule, replicaID, bridgeIP string, port uint32) {
	r.Insert(&endpointpb.EndpointRecord{
		ClusterPath: "c1",
		GroupId:     "g1",
		CapsuleName: capsule,
		ReplicaId:   replicaID,
		BridgeIp:    bridgeIP,
		SwimState:   endpoints.SwimStateAlive,
		NamedPorts: []*endpointpb.NamedPort{
			{Name: "http", ContainerPort: port, Protocol: "tcp"},
		},
		EmittedAt:  timestamppb.Now(),
		TtlSeconds: 3600,
	})
}

// snapshotFor returns a ServiceSnapshot for backends with simple
// weights and identity portmap.
func snapshotFor(weights map[string]int32) ServiceSnapshot {
	snap := ServiceSnapshot{
		ServiceID:      "svc-1",
		ClusterPath:    "c1",
		GroupID:        "g1",
		IdleTimeout:    time.Minute,
		ConnectTimeout: time.Second,
	}
	for n, w := range weights {
		snap.Backends = append(snap.Backends, BackendSnapshot{
			Capsule: n,
			Weight:  w,
		})
	}
	return snap
}

// --- tests ---

func TestTCP_BidirectionalForwarding(t *testing.T) {
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour))
	defer reg.Stop()
	insertReplicaTCP(reg, "api", "r1", "10.0.0.5", 8080)

	sel := NewSelector(WithRegistry(reg))
	stats := newStats()
	resolver := newStaticResolver(snapshotFor(map[string]int32{"api": 100}))
	dialer := newFakeDialer()
	backendCh := dialer.RegisterServer("10.0.0.5:8080")

	tl := NewTCPListener(
		WithTCPLogger(zap.NewNop()),
		WithTCPSelector(sel),
		WithTCPResolver(resolver),
		WithTCPStats(stats),
		WithTCPDialer(dialer),
		WithTCPPortName("http"),
		WithTCPIdleTimeout(2*time.Second),
		WithTCPConnectTimeout(2*time.Second),
	)
	lis := newPipeListener()
	if err := tl.Start(lis); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tl.Stop()

	// Backend echo
	go func() {
		bc := <-backendCh
		runEcho(bc)
	}()

	client, err := lis.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	msg := []byte("hello-proxy")
	if _, err := client.Write(msg); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echo mismatch: got %q want %q", got, msg)
	}
	client.Close()

	// Allow stats to settle.
	waitFor(t, time.Second, func() bool {
		return stats.ConnectionsClosed.Load() >= 1
	})
	snap := stats.Snapshot()
	if snap.ConnectionsOpened == 0 {
		t.Fatalf("ConnectionsOpened = 0")
	}
	if snap.BytesSent == 0 || snap.BytesReceived == 0 {
		t.Fatalf("bytes counters not updated: %+v", snap)
	}
}

func TestTCP_NoReplicaConnectionRefused(t *testing.T) {
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour))
	defer reg.Stop()
	// No replicas inserted.

	sel := NewSelector(WithRegistry(reg))
	stats := newStats()
	resolver := newStaticResolver(snapshotFor(map[string]int32{"api": 100}))
	dialer := newFakeDialer()

	tl := NewTCPListener(
		WithTCPSelector(sel),
		WithTCPResolver(resolver),
		WithTCPStats(stats),
		WithTCPDialer(dialer),
		WithTCPPortName("http"),
		WithTCPDialRetries(1),
	)
	lis := newPipeListener()
	if err := tl.Start(lis); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tl.Stop()

	client, _ := lis.Connect()
	// The proxy will not dial — connection is closed quickly.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = client.Read(make([]byte, 1))
	client.Close()

	waitFor(t, time.Second, func() bool {
		return stats.ForwardErrorsNoReplica.Load() >= 1
	})
}

func TestTCP_StopCancelsInFlight(t *testing.T) {
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour))
	defer reg.Stop()
	insertReplicaTCP(reg, "api", "r1", "10.0.0.5", 8080)

	sel := NewSelector(WithRegistry(reg))
	stats := newStats()
	resolver := newStaticResolver(snapshotFor(map[string]int32{"api": 100}))
	dialer := newFakeDialer()
	backendCh := dialer.RegisterServer("10.0.0.5:8080")

	tl := NewTCPListener(
		WithTCPSelector(sel),
		WithTCPResolver(resolver),
		WithTCPStats(stats),
		WithTCPDialer(dialer),
		WithTCPPortName("http"),
		WithTCPIdleTimeout(time.Minute),
	)
	lis := newPipeListener()
	if err := tl.Start(lis); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Set up a backend that holds the connection without echoing.
	backendDone := make(chan struct{})
	go func() {
		bc := <-backendCh
		<-backendDone
		bc.Close()
	}()

	client, _ := lis.Connect()
	// Wait for the backend to attach.
	waitFor(t, time.Second, func() bool { return dialer.dials.Load() >= 1 })

	// Stop must drain without panicking even with the conn in-flight.
	stopErr := make(chan error, 1)
	go func() { stopErr <- tl.Stop() }()
	select {
	case err := <-stopErr:
		if err != nil {
			t.Fatalf("Stop returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Stop timed out")
	}
	close(backendDone)
	client.Close()
}

func TestTCP_WeightedDistribution(t *testing.T) {
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour))
	defer reg.Stop()
	insertReplicaTCP(reg, "v1", "r1", "10.0.0.1", 8080)
	insertReplicaTCP(reg, "v2", "r1", "10.0.0.2", 8080)

	sel := NewSelector(WithRegistry(reg))
	stats := newStats()
	resolver := newStaticResolver(snapshotFor(map[string]int32{"v1": 90, "v2": 10}))
	dialer := newFakeDialer()
	v1Ch := dialer.RegisterServer("10.0.0.1:8080")
	v2Ch := dialer.RegisterServer("10.0.0.2:8080")

	tl := NewTCPListener(
		WithTCPSelector(sel),
		WithTCPResolver(resolver),
		WithTCPStats(stats),
		WithTCPDialer(dialer),
		WithTCPPortName("http"),
	)
	lis := newPipeListener()
	if err := tl.Start(lis); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer tl.Stop()

	// Drain backends as they connect to allow forward path completion.
	go func() {
		for bc := range v1Ch {
			go runEcho(bc)
		}
	}()
	go func() {
		for bc := range v2Ch {
			go runEcho(bc)
		}
	}()

	const N = 200
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := lis.Connect()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("x"))
			buf := make([]byte, 1)
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _ = c.Read(buf)
			c.Close()
		}()
	}
	wg.Wait()

	waitFor(t, 2*time.Second, func() bool {
		s := stats.Snapshot()
		return s.PicksPerBackend["v1"]+s.PicksPerBackend["v2"] >= N
	})

	snap := stats.Snapshot()
	v1 := snap.PicksPerBackend["v1"]
	v2 := snap.PicksPerBackend["v2"]
	if v1 < int64(N)*70/100 {
		t.Fatalf("v1 received %d/%d picks — distribution skew", v1, N)
	}
	if v2 == 0 {
		t.Fatalf("v2 received zero picks — cold-start randomization broken")
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitFor timed out")
}
