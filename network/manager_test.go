package network

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/tareksalem/falak/network/bridge"
	"github.com/tareksalem/falak/network/dns"
	"github.com/tareksalem/falak/network/endpoints"
	"github.com/tareksalem/falak/network/overlay"
	"github.com/tareksalem/falak/network/startup"
)

// openTestSQLite opens a private file-backed SQLite for the test (file
// not in-memory so the bridge + overlay schema migrations can both run
// in the same DB without contention).
func openTestSQLite(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "network.sqlite")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// inMemoryEventSource is the EventSource used by manager_test.go. Each
// On* registers a callback; Fire* dispatches synchronously to every
// registered callback. Subscriptions returned by On* unregister the
// callback so Stop() draining is observable in tests.
type inMemoryEventSource struct {
	mu       sync.Mutex
	received []func(CapsuleEvent)
	running  []func(CapsuleEvent)
	stopped  []func(CapsuleEvent)
	deleted  []func(CapsuleEvent)
	joined   []func(CapsuleEvent)
	left     []func(CapsuleEvent)
}

func newInMemoryEventSource() *inMemoryEventSource { return &inMemoryEventSource{} }

func (s *inMemoryEventSource) register(list *[]func(CapsuleEvent), fn func(CapsuleEvent)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := len(*list)
	*list = append(*list, fn)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if idx < len(*list) {
			(*list)[idx] = nil
		}
	}
}

func (s *inMemoryEventSource) OnCapsuleReceived(fn func(CapsuleEvent)) func() {
	return s.register(&s.received, fn)
}
func (s *inMemoryEventSource) OnCapsuleRunning(fn func(CapsuleEvent)) func() {
	return s.register(&s.running, fn)
}
func (s *inMemoryEventSource) OnCapsuleStopped(fn func(CapsuleEvent)) func() {
	return s.register(&s.stopped, fn)
}
func (s *inMemoryEventSource) OnCapsuleDeleted(fn func(CapsuleEvent)) func() {
	return s.register(&s.deleted, fn)
}
func (s *inMemoryEventSource) OnPeerJoined(fn func(CapsuleEvent)) func() {
	return s.register(&s.joined, fn)
}
func (s *inMemoryEventSource) OnPeerLeft(fn func(CapsuleEvent)) func() {
	return s.register(&s.left, fn)
}

func (s *inMemoryEventSource) fire(list []func(CapsuleEvent), ev CapsuleEvent) {
	s.mu.Lock()
	cbs := make([]func(CapsuleEvent), len(list))
	copy(cbs, list)
	s.mu.Unlock()
	for _, cb := range cbs {
		if cb != nil {
			cb(ev)
		}
	}
}

func (s *inMemoryEventSource) FireRunning(ev CapsuleEvent) { s.fire(s.running, ev) }
func (s *inMemoryEventSource) FireStopped(ev CapsuleEvent) { s.fire(s.stopped, ev) }
func (s *inMemoryEventSource) FireDeleted(ev CapsuleEvent) { s.fire(s.deleted, ev) }
func (s *inMemoryEventSource) FireJoined(ev CapsuleEvent)  { s.fire(s.joined, ev) }
func (s *inMemoryEventSource) FireLeft(ev CapsuleEvent)    { s.fire(s.left, ev) }

// fakePodman is the local PodmanNetworkClient used by manager tests. It
// counts CreateNetwork + DeleteNetwork calls so refcount transitions can
// be asserted directly. PodmanNetworkLister is intentionally NOT
// implemented; the manager's reap-on-start pass exercises the pure
// dangling-set path.
type fakePodman struct {
	mu       sync.Mutex
	nets     map[string]*bridge.PodmanNetwork
	created  int32
	deleted  int32
	createEr error
	deleteEr error
}

func newFakePodman() *fakePodman { return &fakePodman{nets: map[string]*bridge.PodmanNetwork{}} }

func (f *fakePodman) CreateNetwork(_ context.Context, name, subnet, gateway string, mtu int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createEr != nil {
		return f.createEr
	}
	atomic.AddInt32(&f.created, 1)
	f.nets[name] = &bridge.PodmanNetwork{Name: name, Subnet: subnet, Gateway: gateway, MTU: mtu}
	return nil
}
func (f *fakePodman) DeleteNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteEr != nil {
		return f.deleteEr
	}
	atomic.AddInt32(&f.deleted, 1)
	if _, ok := f.nets[name]; !ok {
		return bridge.ErrPodmanNetworkNotFound
	}
	delete(f.nets, name)
	return nil
}
func (f *fakePodman) InspectNetwork(_ context.Context, name string) (*bridge.PodmanNetwork, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n, ok := f.nets[name]; ok {
		cp := *n
		return &cp, nil
	}
	return nil, bridge.ErrPodmanNetworkNotFound
}
func (f *fakePodman) Has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.nets[name]
	return ok
}

// fakeIpt is a bridge.CommandRunner that records every iptables call.
type fakeIpt struct{ mu sync.Mutex }

func (f *fakeIpt) Run(_ context.Context, _ string, _ ...string) error { return nil }

// fakePubSub satisfies endpoints.PubSub for the publisher/subscriber.
type fakePubSub struct {
	mu        sync.Mutex
	published map[string][][]byte
	subs      map[string][]chan []byte
}

func newFakePubSub() *fakePubSub {
	return &fakePubSub{published: map[string][][]byte{}, subs: map[string][]chan []byte{}}
}

func (f *fakePubSub) Publish(_ context.Context, topic string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published[topic] = append(f.published[topic], append([]byte(nil), data...))
	return nil
}
func (f *fakePubSub) Subscribe(ctx context.Context, topic string) (<-chan []byte, error) {
	f.mu.Lock()
	ch := make(chan []byte, 16)
	f.subs[topic] = append(f.subs[topic], ch)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, c := range f.subs[topic] {
			if c == ch {
				f.subs[topic] = append(f.subs[topic][:i], f.subs[topic][i+1:]...)
				break
			}
		}
		close(ch)
	}()
	return ch, nil
}

// stubSigner produces a deterministic "sig" so publishes succeed.
type stubSigner struct{}

func (stubSigner) Sign(content []byte) ([]byte, error) {
	return append([]byte("S:"), content...), nil
}

// testHarness wires every collaborator the Manager needs.
type testHarness struct {
	mgr      *Manager
	src      *inMemoryEventSource
	pod      *fakePodman
	peerDev  *overlay.MockDeviceManager
	peerIPs  *overlay.MockIPsecManager
	pubsub   *fakePubSub
	gates    *startup.Gates
	dnsSrv   *dns.Server
	registry *endpoints.Registry
	pub      *endpoints.Publisher
	sub      *endpoints.Subscriber
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	return buildHarness(t, true)
}

func newTestHarnessWithBrokenGates(t *testing.T) *testHarness {
	t.Helper()
	return buildHarness(t, false)
}

func buildHarness(t *testing.T, gatesPass bool) *testHarness {
	t.Helper()
	pod := newFakePodman()
	ipt := &fakeIpt{}

	db := openTestSQLite(t)
	alloc, err := bridge.OpenAllocator(db)
	require.NoError(t, err)

	bm, err := bridge.NewManager(
		bridge.WithPodmanClient(pod),
		bridge.WithAllocator(alloc),
		bridge.WithIptablesRunner(ipt),
		bridge.WithDestroyBaseDelay(time.Millisecond),
	)
	require.NoError(t, err)

	vniAlloc, err := overlay.OpenVNIAllocator(overlay.WithVNIDB(db))
	require.NoError(t, err)

	dev := overlay.NewMockDeviceManager()
	ipsec := overlay.NewMockIPsecManager()
	keymgr, err := overlay.NewKeyManager(overlay.WithClusterRootKey([]byte("cluster-secret")))
	require.NoError(t, err)
	mtu := overlay.MTUResolverFunc(func() int { return 1450 })

	peers, err := overlay.NewPeerManager(
		overlay.WithVNIAllocator(vniAlloc),
		overlay.WithDeviceManager(dev),
		overlay.WithIPsecManager(ipsec),
		overlay.WithKeyManager(keymgr),
		overlay.WithMTUResolver(mtu),
		overlay.WithLocalNodeID("node-local"),
		overlay.WithLocalIP("10.0.0.1"),
		overlay.WithFlapGrace(10*time.Millisecond),
	)
	require.NoError(t, err)

	dnsSrv := dns.NewServer(
		dns.WithPort(0),
		dns.WithBindAddrs([]netip.Addr{netip.MustParseAddr("127.0.0.1")}),
	)
	registry := endpoints.NewRegistry()
	t.Cleanup(registry.Stop)

	ps := newFakePubSub()
	pub := endpoints.NewPublisher(
		endpoints.WithPubSub(ps),
		endpoints.WithSigner(stubSigner{}),
		endpoints.WithLocalNodeID("node-local"),
		endpoints.WithTTL(30*time.Second),
	)
	sub := endpoints.NewSubscriber(
		endpoints.WithSubscriberPubSub(ps),
		endpoints.WithRegistry(registry),
		endpoints.WithSubscriberLocalNodeID("node-local"),
	)

	gatesProc := func(path string) ([]byte, error) {
		if path == "/proc/sys/net/ipv4/conf/all/rp_filter-test" {
			if gatesPass {
				return []byte("1"), nil
			}
			return []byte("0"), nil
		}
		// Every other procfs path (firewalld pidfile etc.) is "absent".
		return nil, errors.New("not present")
	}
	gates := startup.New(
		startup.WithRPFilterPath("/proc/sys/net/ipv4/conf/all/rp_filter-test"),
		startup.WithProcReader(gatesProc),
		startup.WithCommandRunner(func(string, ...string) ([]byte, error) {
			return nil, errors.New("not active")
		}),
	)

	src := newInMemoryEventSource()
	mgr, err := New(
		WithBridgeManager(bm),
		WithSubnetAllocator(alloc),
		WithPeerManager(peers),
		WithVNIAllocator(vniAlloc),
		WithDNSServer(dnsSrv),
		WithEndpointRegistry(registry),
		WithEndpointSubscriber(sub),
		WithEndpointPublisher(pub),
		WithStartupGates(gates),
		WithEventSource(src),
		WithClusterPath("default"),
		WithLocalNodeID("node-local"),
		WithLocalIP("10.0.0.1"),
		WithLogger(zaptest.NewLogger(t)),
		WithDNSListenAddrLookup(func(*bridge.BridgeInfo) (netip.Addr, error) {
			return netip.MustParseAddr("127.0.0.1"), nil
		}),
	)
	require.NoError(t, err)

	return &testHarness{
		mgr: mgr, src: src, pod: pod,
		peerDev: dev, peerIPs: ipsec, pubsub: ps,
		gates: gates, dnsSrv: dnsSrv, registry: registry,
		pub: pub, sub: sub,
	}
}

func makeMemberEvent(replica string) CapsuleEvent {
	return CapsuleEvent{
		ClusterPath: "default", GroupID: "g-api", IsGroup: false,
		CapsuleName: "api", ReplicaID: replica, NodeID: "node-local",
		NodeIP: "10.0.0.1", BridgeIP: "10.88.0.5",
		SwimState: endpoints.SwimStateAlive,
		NamedPorts: []endpoints.NamedPort{
			{Name: "http", ContainerPort: 8080, Protocol: "tcp"},
		},
	}
}

func TestManager_FirstMemberProvisionsEverything(t *testing.T) {
	h := newTestHarness(t)
	require.NoError(t, h.mgr.Start(context.Background()))
	t.Cleanup(func() { _ = h.mgr.Stop() })

	h.src.FireRunning(makeMemberEvent("r1"))

	require.True(t, h.pod.Has("falak-g-api"), "bridge should be created on first member")
	require.Len(t, h.peerDev.OpsByKind("create"), 1,
		"VXLAN device should be created exactly once")
}

func TestManager_SecondMemberIsNoOp(t *testing.T) {
	h := newTestHarness(t)
	require.NoError(t, h.mgr.Start(context.Background()))
	t.Cleanup(func() { _ = h.mgr.Stop() })

	h.src.FireRunning(makeMemberEvent("r1"))
	createBefore := atomic.LoadInt32(&h.pod.created)
	h.src.FireRunning(makeMemberEvent("r2"))
	createAfter := atomic.LoadInt32(&h.pod.created)
	require.Equal(t, createBefore, createAfter, "second member must not re-create the bridge")
}

func TestManager_LastMemberTearsDown(t *testing.T) {
	h := newTestHarness(t)
	require.NoError(t, h.mgr.Start(context.Background()))
	t.Cleanup(func() { _ = h.mgr.Stop() })

	h.src.FireRunning(makeMemberEvent("r1"))
	h.src.FireRunning(makeMemberEvent("r2"))
	require.True(t, h.pod.Has("falak-g-api"))

	h.src.FireStopped(makeMemberEvent("r1"))
	require.True(t, h.pod.Has("falak-g-api"), "still one member left; bridge stays")

	h.src.FireStopped(makeMemberEvent("r2"))
	require.False(t, h.pod.Has("falak-g-api"), "last member exit must tear down")
}

func TestManager_ConcurrentArrivalsDoNotDoubleProvision(t *testing.T) {
	h := newTestHarness(t)
	require.NoError(t, h.mgr.Start(context.Background()))
	t.Cleanup(func() { _ = h.mgr.Stop() })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev := makeMemberEvent("r" + string(rune('0'+i)))
			h.src.FireRunning(ev)
		}()
	}
	wg.Wait()

	require.Equal(t, int32(1), atomic.LoadInt32(&h.pod.created),
		"concurrent first-member events must collapse to one bridge create")
}

func TestManager_GroupDeletedForcesTeardown(t *testing.T) {
	h := newTestHarness(t)
	require.NoError(t, h.mgr.Start(context.Background()))
	t.Cleanup(func() { _ = h.mgr.Stop() })

	h.src.FireRunning(makeMemberEvent("r1"))
	h.src.FireRunning(makeMemberEvent("r2"))
	require.True(t, h.pod.Has("falak-g-api"))

	h.src.FireDeleted(CapsuleEvent{
		ClusterPath: "default", GroupID: "g-api", IsGroup: true,
	})
	require.False(t, h.pod.Has("falak-g-api"),
		"deletion of the group capsule must tear down even with live members")
}

func TestManager_StopDrainsCleanly(t *testing.T) {
	h := newTestHarness(t)
	require.NoError(t, h.mgr.Start(context.Background()))

	h.src.FireRunning(makeMemberEvent("r1"))
	require.NoError(t, h.mgr.Stop())
	require.NoError(t, h.mgr.Stop(), "Stop must be idempotent")
}

func TestManager_StartFailsOnBrokenGates(t *testing.T) {
	h := newTestHarnessWithBrokenGates(t)
	err := h.mgr.Start(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "startup gates")
}

func TestManager_NewRequiresEveryDependency(t *testing.T) {
	_, err := New()
	require.Error(t, err)
	require.Contains(t, err.Error(), "WithBridgeManager")
}

func TestManager_PeerJoinedTriggersEnsurePeer(t *testing.T) {
	h := newTestHarness(t)
	require.NoError(t, h.mgr.Start(context.Background()))
	t.Cleanup(func() { _ = h.mgr.Stop() })

	h.src.FireRunning(makeMemberEvent("r1"))
	// Now a remote peer joins for the same group.
	h.src.FireJoined(CapsuleEvent{
		ClusterPath: "default", GroupID: "g-api",
		NodeID: "node-remote", NodeIP: "10.0.0.2",
	})
	require.NotEmpty(t, h.peerIPs.OpsByKind("install"),
		"peer joining must install an IPsec SA")
}

// recordingBridgeListener is a BridgeListener fixture that captures
// every callback for assertion. Safe for concurrent use.
type recordingBridgeListener struct {
	mu      sync.Mutex
	added   []netip.Addr
	removed []netip.Addr
}

// OnBridgeAdded records addr.
func (r *recordingBridgeListener) OnBridgeAdded(addr netip.Addr) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.added = append(r.added, addr)
	return nil
}

// OnBridgeRemoved records addr.
func (r *recordingBridgeListener) OnBridgeRemoved(addr netip.Addr) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, addr)
	return nil
}

// AddedSnapshot returns a copy of the recorded add callbacks.
func (r *recordingBridgeListener) AddedSnapshot() []netip.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]netip.Addr, len(r.added))
	copy(out, r.added)
	return out
}

// RemovedSnapshot returns a copy of the recorded remove callbacks.
func (r *recordingBridgeListener) RemovedSnapshot() []netip.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]netip.Addr, len(r.removed))
	copy(out, r.removed)
	return out
}

// TestManager_EnsureGroup_FiresBridgeListener verifies the
// proxy-bound bridge add/remove callbacks fire in lock-step with the
// 0→1 / 1→0 group ref transitions. Without this seam the proxy never
// learns about bridges and listeners stay unbound — the
// service-mesh "DORMANT in production" regression the Phase 11B
// audit caught.
func TestManager_EnsureGroup_FiresBridgeListener(t *testing.T) {
	h := newTestHarness(t)
	require.NoError(t, h.mgr.Start(context.Background()))
	t.Cleanup(func() { _ = h.mgr.Stop() })

	listener := &recordingBridgeListener{}
	h.mgr.RegisterBridgeListener(listener)

	h.src.FireRunning(makeMemberEvent("r1"))
	require.True(t, h.pod.Has("falak-g-api"), "bridge must exist after first member")

	added := listener.AddedSnapshot()
	require.Len(t, added, 1, "exactly one add callback on first-member transition")
	require.True(t, added[0].IsValid(), "added addr must be a parseable IP")
	require.True(t, len(h.mgr.BridgeGateways()) == 1,
		"BridgeGateways must mirror the live gateway")

	// Second member: ref count goes 1→2, no extra callback.
	h.src.FireRunning(makeMemberEvent("r2"))
	require.Len(t, listener.AddedSnapshot(), 1,
		"second member must not re-fire OnBridgeAdded")

	// Drain the group; the 1→0 transition must fire OnBridgeRemoved.
	h.src.FireStopped(makeMemberEvent("r1"))
	h.src.FireStopped(makeMemberEvent("r2"))
	require.Eventually(t, func() bool {
		return len(listener.RemovedSnapshot()) == 1
	}, time.Second, 5*time.Millisecond, "OnBridgeRemoved must fire on teardown")

	// After teardown the gateway list is empty again.
	require.Empty(t, h.mgr.BridgeGateways(), "no gateways after teardown")
}

// TestManager_ResolveBridgeGroup_ReturnsClusterAndGroup ensures the
// SourceResolver-shaped accessor maps an IP that falls inside a
// bridge's subnet back to (clusterPath, groupID). Used by the proxy
// visibility check.
func TestManager_ResolveBridgeGroup_ReturnsClusterAndGroup(t *testing.T) {
	h := newTestHarness(t)
	require.NoError(t, h.mgr.Start(context.Background()))
	t.Cleanup(func() { _ = h.mgr.Stop() })

	h.src.FireRunning(makeMemberEvent("r1"))
	gws := h.mgr.BridgeGateways()
	require.Len(t, gws, 1)

	cluster, group, ok := h.mgr.ResolveBridgeGroup(gws[0])
	require.True(t, ok, "gateway IP must resolve to its own bridge")
	require.Equal(t, "default", cluster)
	require.Equal(t, "g-api", group)

	// An IP outside the subnet must fail closed.
	_, _, ok = h.mgr.ResolveBridgeGroup(netip.MustParseAddr("198.51.100.1"))
	require.False(t, ok, "off-overlay IP must not resolve to a group")
}

func TestManager_PeerLeftSchedulesRemoval(t *testing.T) {
	h := newTestHarness(t)
	require.NoError(t, h.mgr.Start(context.Background()))
	t.Cleanup(func() { _ = h.mgr.Stop() })

	h.src.FireRunning(makeMemberEvent("r1"))
	h.src.FireJoined(CapsuleEvent{
		ClusterPath: "default", GroupID: "g-api",
		NodeID: "node-remote", NodeIP: "10.0.0.2",
	})
	h.src.FireLeft(CapsuleEvent{
		ClusterPath: "default", GroupID: "g-api",
		NodeID: "node-remote",
	})
	// Flap grace = 10ms; wait briefly for the worker to commit removal.
	require.Eventually(t, func() bool {
		return len(h.peerIPs.OpsByKind("remove")) > 0
	}, time.Second, 5*time.Millisecond, "peer left must eventually call RemoveSA")
}
