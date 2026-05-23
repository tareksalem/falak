package proxy

import (
	"errors"
	"net"
	"net/netip"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/network/endpoints"
)

// lifecycleResolver is a ServiceResolver that returns a canned
// snapshot. The lifecycle tests don't drive traffic through the
// listeners — they only verify the listener set converges to the
// desired shape — so the snapshot's content is intentionally minimal.
type lifecycleResolver struct {
	serviceID string
}

func (r *lifecycleResolver) Resolve() (ServiceSnapshot, bool) {
	return ServiceSnapshot{ServiceID: r.serviceID}, true
}

// fakeBridges returns the current bridge set under a mutex so tests
// can add / remove gateways and re-read snapshot-style.
type fakeBridges struct {
	mu    sync.Mutex
	addrs []netip.Addr
}

func newFakeBridges(initial ...netip.Addr) *fakeBridges {
	out := make([]netip.Addr, len(initial))
	copy(out, initial)
	return &fakeBridges{addrs: out}
}

func (b *fakeBridges) snapshot() []netip.Addr {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]netip.Addr, len(b.addrs))
	copy(out, b.addrs)
	return out
}

func (b *fakeBridges) add(addr netip.Addr) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, a := range b.addrs {
		if a == addr {
			return
		}
	}
	b.addrs = append(b.addrs, addr)
}

func (b *fakeBridges) remove(addr netip.Addr) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.addrs[:0]
	for _, a := range b.addrs {
		if a != addr {
			out = append(out, a)
		}
	}
	b.addrs = out
}

// listenLoopback returns a netip.Addr the OS will let us bind to. We
// use 127.0.0.x so multiple "bridges" can coexist on the loopback
// interface (Linux gives us the whole /8 by default).
func listenLoopback(i int) netip.Addr {
	return netip.AddrFrom4([4]byte{127, 0, 0, byte(i)})
}

func newTestManager(t *testing.T, bridges *fakeBridges) *ProxyManager {
	t.Helper()
	stats := NewStatsRegistry()
	reg := endpoints.NewRegistry()
	m := NewProxyManager(
		WithManagerStats(stats),
		WithManagerSelectorBuilder(func(_ string) *Selector { return NewSelector() }),
		WithManagerResolverBuilder(func(svcID string) ServiceResolver { return &lifecycleResolver{serviceID: svcID} }),
		WithManagerRegistry(reg),
		WithManagerBridgeGateways(bridges.snapshot),
	)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })
	return m
}

func mkSpec(id, name string, ports ...ServicePort) ServiceSpec {
	return ServiceSpec{
		ID:          id,
		Name:        name,
		ClusterPath: "/falak/test",
		GroupID:     "billing",
		Visibility:  "cluster",
		Ports:       ports,
	}
}

func sortedKeys(keys []listenerKey) []listenerKey {
	out := make([]listenerKey, len(keys))
	copy(out, keys)
	sort.Slice(out, func(i, j int) bool {
		if out[i].serviceID != out[j].serviceID {
			return out[i].serviceID < out[j].serviceID
		}
		if out[i].portName != out[j].portName {
			return out[i].portName < out[j].portName
		}
		return out[i].bridgeIP.String() < out[j].bridgeIP.String()
	})
	return out
}

func TestProxyManagerEnsureServiceSpawnsListenersPerPortPerBridge(t *testing.T) {
	b := newFakeBridges(listenLoopback(1), listenLoopback(2))
	m := newTestManager(t, b)

	spec := mkSpec("svc-1", "payments",
		ServicePort{Name: "http", Port: pickFreePort(t), Protocol: "tcp"},
		ServicePort{Name: "metrics", Port: pickFreePort(t), Protocol: "tcp"},
	)
	if err := m.EnsureService(spec); err != nil {
		t.Fatalf("EnsureService: %v", err)
	}
	got := m.ActiveListeners()
	if want := 2 * 2; len(got) != want {
		t.Fatalf("listener count = %d want %d", len(got), want)
	}
}

func TestProxyManagerEnsureServiceIdempotent(t *testing.T) {
	b := newFakeBridges(listenLoopback(3))
	m := newTestManager(t, b)
	spec := mkSpec("svc-2", "api",
		ServicePort{Name: "http", Port: pickFreePort(t), Protocol: "tcp"},
	)
	if err := m.EnsureService(spec); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	first := m.ActiveListeners()
	if err := m.EnsureService(spec); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	second := m.ActiveListeners()
	if len(first) != len(second) {
		t.Fatalf("listener count drifted: %d → %d", len(first), len(second))
	}
}

func TestProxyManagerOnBridgeAddedSpawnsForActiveServices(t *testing.T) {
	b := newFakeBridges(listenLoopback(4))
	m := newTestManager(t, b)
	spec := mkSpec("svc-3", "api",
		ServicePort{Name: "http", Port: pickFreePort(t), Protocol: "tcp"},
	)
	if err := m.EnsureService(spec); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := len(m.ActiveListeners()); got != 1 {
		t.Fatalf("pre-add count = %d want 1", got)
	}
	// Add a bridge; the manager should spawn one more listener.
	newAddr := listenLoopback(5)
	b.add(newAddr)
	if err := m.OnBridgeAdded(newAddr); err != nil {
		t.Fatalf("OnBridgeAdded: %v", err)
	}
	got := sortedKeys(m.ActiveListeners())
	if len(got) != 2 {
		t.Fatalf("post-add count = %d want 2", len(got))
	}
	found := false
	for _, k := range got {
		if k.bridgeIP == newAddr {
			found = true
		}
	}
	if !found {
		t.Fatalf("listener for new bridge %s missing", newAddr)
	}
}

func TestProxyManagerOnBridgeRemovedClosesListeners(t *testing.T) {
	a1 := listenLoopback(6)
	a2 := listenLoopback(7)
	b := newFakeBridges(a1, a2)
	m := newTestManager(t, b)
	spec := mkSpec("svc-4", "api",
		ServicePort{Name: "http", Port: pickFreePort(t), Protocol: "tcp"},
	)
	if err := m.EnsureService(spec); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := len(m.ActiveListeners()); got != 2 {
		t.Fatalf("pre-remove count = %d want 2", got)
	}
	b.remove(a2)
	if err := m.OnBridgeRemoved(a2); err != nil {
		t.Fatalf("OnBridgeRemoved: %v", err)
	}
	got := m.ActiveListeners()
	if len(got) != 1 {
		t.Fatalf("post-remove count = %d want 1", len(got))
	}
	if got[0].bridgeIP != a1 {
		t.Fatalf("remaining bridge = %s want %s", got[0].bridgeIP, a1)
	}
}

func TestProxyManagerReleaseServiceStopsEveryListener(t *testing.T) {
	b := newFakeBridges(listenLoopback(8), listenLoopback(9))
	m := newTestManager(t, b)
	spec := mkSpec("svc-5", "api",
		ServicePort{Name: "http", Port: pickFreePort(t), Protocol: "tcp"},
		ServicePort{Name: "grpc", Port: pickFreePort(t), Protocol: "tcp"},
	)
	if err := m.EnsureService(spec); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := len(m.ActiveListeners()); got != 4 {
		t.Fatalf("pre-release count = %d want 4", got)
	}
	if err := m.ReleaseService(spec.ID); err != nil {
		t.Fatalf("ReleaseService: %v", err)
	}
	if got := len(m.ActiveListeners()); got != 0 {
		t.Fatalf("post-release count = %d want 0", got)
	}
	// Idempotent release.
	if err := m.ReleaseService(spec.ID); err != nil {
		t.Fatalf("release idempotent: %v", err)
	}
}

func TestProxyManagerStopDrainsEverything(t *testing.T) {
	b := newFakeBridges(listenLoopback(10), listenLoopback(11))
	m := newTestManager(t, b)
	spec := mkSpec("svc-6", "api",
		ServicePort{Name: "http", Port: pickFreePort(t), Protocol: "tcp"},
	)
	if err := m.EnsureService(spec); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := len(m.ActiveListeners()); got != 2 {
		t.Fatalf("pre-stop count = %d want 2", got)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := len(m.ActiveListeners()); got != 0 {
		t.Fatalf("post-stop count = %d want 0", got)
	}
	// EnsureService after Stop must fail.
	if err := m.EnsureService(spec); !errors.Is(err, ErrManagerStopped) {
		t.Fatalf("post-stop EnsureService = %v want ErrManagerStopped", err)
	}
	// Stop is idempotent.
	if err := m.Stop(); err != nil {
		t.Fatalf("stop idempotent: %v", err)
	}
}

func TestProxyManagerExternalVisibilityRejected(t *testing.T) {
	b := newFakeBridges(listenLoopback(12))
	m := newTestManager(t, b)
	spec := mkSpec("svc-7", "api",
		ServicePort{Name: "http", Port: pickFreePort(t), Protocol: "tcp"},
	)
	spec.Visibility = "external"
	err := m.EnsureService(spec)
	if !errors.Is(err, ErrExternalVisibilityNotSupported) {
		t.Fatalf("EnsureService external = %v want ErrExternalVisibilityNotSupported", err)
	}
	if got := len(m.ActiveListeners()); got != 0 {
		t.Fatalf("listeners after rejected ensure = %d want 0", got)
	}
}

func TestProxyManagerMissingRequiredOption(t *testing.T) {
	t.Parallel()
	m := NewProxyManager()
	if err := m.Start(); !errors.Is(err, ErrManagerMisconfigured) {
		t.Fatalf("Start without opts = %v want ErrManagerMisconfigured", err)
	}
}

func TestProxyManagerEventSubscriberDrives(t *testing.T) {
	b := newFakeBridges(listenLoopback(13))
	src := newFakeEventSource()
	stats := NewStatsRegistry()
	reg := endpoints.NewRegistry()
	m := NewProxyManager(
		WithManagerStats(stats),
		WithManagerSelectorBuilder(func(_ string) *Selector { return NewSelector() }),
		WithManagerResolverBuilder(func(svcID string) ServiceResolver { return &lifecycleResolver{serviceID: svcID} }),
		WithManagerRegistry(reg),
		WithManagerBridgeGateways(b.snapshot),
		WithManagerEventSource(src),
	)
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })

	spec := mkSpec("svc-8", "api",
		ServicePort{Name: "http", Port: pickFreePort(t), Protocol: "tcp"},
	)
	src.fireService("create", spec)
	if !waitForListeners(func() bool { return len(m.ActiveListeners()) == 1 }) {
		t.Fatalf("create event did not spawn listener (got %d)", len(m.ActiveListeners()))
	}

	newAddr := listenLoopback(14)
	b.add(newAddr)
	src.fireBridge(true, newAddr)
	if !waitForListeners(func() bool { return len(m.ActiveListeners()) == 2 }) {
		t.Fatalf("bridge-add event did not spawn listener (got %d)", len(m.ActiveListeners()))
	}

	src.fireService("delete", spec)
	if !waitForListeners(func() bool { return len(m.ActiveListeners()) == 0 }) {
		t.Fatalf("delete event did not stop listeners (got %d)", len(m.ActiveListeners()))
	}
}

// fakeEventSource captures subscribers and fires events synchronously
// from the test driver.
type fakeEventSource struct {
	mu      sync.Mutex
	svcSubs []func(string, ServiceSpec)
	brSubs  []func(bool, netip.Addr)
}

func newFakeEventSource() *fakeEventSource { return &fakeEventSource{} }

func (f *fakeEventSource) SubscribeServiceEvents(fn func(string, ServiceSpec)) func() {
	f.mu.Lock()
	idx := len(f.svcSubs)
	f.svcSubs = append(f.svcSubs, fn)
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.svcSubs[idx] = nil
		f.mu.Unlock()
	}
}

func (f *fakeEventSource) SubscribeBridgeEvents(fn func(bool, netip.Addr)) func() {
	f.mu.Lock()
	idx := len(f.brSubs)
	f.brSubs = append(f.brSubs, fn)
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.brSubs[idx] = nil
		f.mu.Unlock()
	}
}

func (f *fakeEventSource) fireService(kind string, spec ServiceSpec) {
	f.mu.Lock()
	subs := make([]func(string, ServiceSpec), len(f.svcSubs))
	copy(subs, f.svcSubs)
	f.mu.Unlock()
	for _, s := range subs {
		if s != nil {
			s(kind, spec)
		}
	}
}

func (f *fakeEventSource) fireBridge(added bool, addr netip.Addr) {
	f.mu.Lock()
	subs := make([]func(bool, netip.Addr), len(f.brSubs))
	copy(subs, f.brSubs)
	f.mu.Unlock()
	for _, s := range subs {
		if s != nil {
			s(added, addr)
		}
	}
}

// waitForListeners polls cond up to 2s and reports whether it became true.
func waitForListeners(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// pickFreePort returns an unused TCP port on loopback. We bind to
// 127.0.0.1:0, capture the kernel-assigned port, and close so the
// caller can re-bind by spec.Port. There's a vanishingly small race
// against other binders; fine for these listener-spawn tests.
func pickFreePort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pickFreePort listen: %v", err)
	}
	defer l.Close()
	a, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("pickFreePort: unexpected addr type %T", l.Addr())
	}
	return uint16(a.Port)
}
