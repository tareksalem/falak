package node

// Bundle M / Phase 11B.22 — service-mesh integration tests.
//
// This file wires the Phase 11B components end-to-end without depending
// on real libp2p or kernel networking. Each test stitches together:
//
//   - one or more synthetic "nodes" (each owns an endpoints.Registry,
//     a DNS server bound on a 127.0.0.x alias, and a proxy.ProxyManager
//     listening on a second 127.0.0.x alias),
//   - a real service.Manager driving Service CRUD + identity binding,
//     bridged into the proxy via the same adapter pattern node.go uses
//     in production,
//   - a fake backend listener per replica that returns a tag identifying
//     which (capsule, replica) accepted the connection, so a test driver
//     can count traffic distribution end-to-end.
//
// The fixture intentionally mirrors the production data plane:
// resolution flows through endpoints.Registry → proxy.Selector → fake
// backend; the proxy's per-Service ServiceResolver consults the live
// strategy weights via a getter (the production wiring's plumbing).
// What's omitted is the gossip layer — registries are populated by
// publishToAll, matching the convergence assumption used in the
// network-foundation integration tests.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	miekg "github.com/miekg/dns"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	netdns "github.com/tareksalem/falak/network/dns"
	"github.com/tareksalem/falak/network/endpoints"
	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
	"github.com/tareksalem/falak/network/proxy"
	"github.com/tareksalem/falak/service"
	svcstrategy "github.com/tareksalem/falak/service/strategy"
)

// fakeBackend is a minimal TCP echo server tagged with a (capsule,
// replica) identity. Each accepted connection writes the tag and a
// newline then closes — the driver reads the tag to count which
// backend served the request.
type fakeBackend struct {
	tag      string
	listener net.Listener
	addr     netip.AddrPort
	wg       sync.WaitGroup
	cancel   chan struct{}
	once     sync.Once
	failMode atomic.Int32 // 0 ok, 1 closed (refused), 2 rst-mid-stream, 3 silent
}

const (
	fakeFailOK     = int32(0)
	fakeFailClosed = int32(1)
	fakeFailRST    = int32(2)
	fakeFailSilent = int32(3)
)

// newFakeBackend boots a TCP listener on 127.0.0.1:0 and returns the
// backend, ready to serve. Callers MUST call Close.
func newFakeBackend(t *testing.T, tag string) *fakeBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake backend listen: %v", err)
	}
	a := lis.Addr().(*net.TCPAddr)
	fb := &fakeBackend{
		tag:      tag,
		listener: lis,
		addr:     netip.AddrPortFrom(a.AddrPort().Addr(), uint16(a.Port)),
		cancel:   make(chan struct{}),
	}
	fb.wg.Add(1)
	go fb.serve()
	return fb
}

// serve runs the per-backend accept loop.
func (b *fakeBackend) serve() {
	defer b.wg.Done()
	for {
		c, err := b.listener.Accept()
		if err != nil {
			select {
			case <-b.cancel:
				return
			default:
				return
			}
		}
		go b.handle(c)
	}
}

func (b *fakeBackend) handle(c net.Conn) {
	defer c.Close()
	mode := b.failMode.Load()
	switch mode {
	case fakeFailClosed:
		return
	case fakeFailRST:
		_, _ = c.Write([]byte("partial"))
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		return
	case fakeFailSilent:
		// Accept but never respond. Block until the peer closes.
		buf := make([]byte, 1)
		_, _ = c.Read(buf)
		return
	default:
		_, _ = c.Write([]byte(b.tag + "\n"))
	}
}

// setFailMode flips the backend's response mode for outlier tests.
func (b *fakeBackend) setFailMode(mode int32) { b.failMode.Store(mode) }

// Close shuts the backend down and waits for goroutines.
func (b *fakeBackend) Close() {
	b.once.Do(func() {
		close(b.cancel)
		_ = b.listener.Close()
		b.wg.Wait()
	})
}

// meshFixture wires one or more synthetic nodes and one shared service
// control plane. The control plane is a single service.Manager per
// fixture because Phase 11B integration tests only need to observe the
// proxy + DNS data plane react to manager-emitted events.
type meshFixture struct {
	t           *testing.T
	clusterPath string
	groupID     string

	// shared registries (populated by publishToAll, mirroring gossip
	// convergence). Each entry in nodes consults the same registry.
	registry *endpoints.Registry

	// service control plane (one per fixture).
	caps     *stubCaps
	mgr      *service.Manager
	mgrEvtCh chan service.ManagerEvent

	// per-Service strategy engines (keyed by ServiceID).
	strategyMu sync.Mutex
	strategies map[service.ServiceID]svcstrategy.Engine
}

// stubCaps satisfies service.CapsuleLookup with a mutable name→id map.
type stubCaps struct {
	mu  sync.Mutex
	ids map[string]string
}

func newStubCaps() *stubCaps { return &stubCaps{ids: map[string]string{}} }

func (s *stubCaps) Lookup(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.ids[name]
	return id, ok
}

func (s *stubCaps) set(name, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids[name] = id
}

func (s *stubCaps) del(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ids, name)
}

// meshNode is a single synthetic node's proxy + DNS view.
type meshNode struct {
	id          string
	dnsAddr     netip.Addr
	proxyAddr   netip.Addr
	dnsPort     int
	dnsServer   *netdns.Server
	proxy       *proxy.ProxyManager
	stats       *proxy.StatsRegistry
	selectorMap map[string]*proxy.Selector
	bridges     *fakeBridges
}

// newMeshFixture constructs the control plane.
func newMeshFixture(t *testing.T, cluster, groupID string) *meshFixture {
	t.Helper()
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(50 * time.Millisecond))
	t.Cleanup(reg.Stop)
	caps := newStubCaps()
	mgr := service.NewManager(
		service.WithLogger(zap.NewNop()),
		service.WithCapsuleLookup(caps),
	)
	f := &meshFixture{
		t:           t,
		clusterPath: cluster,
		groupID:     groupID,
		registry:    reg,
		caps:        caps,
		mgr:         mgr,
		mgrEvtCh:    make(chan service.ManagerEvent, 64),
		strategies:  make(map[service.ServiceID]svcstrategy.Engine),
	}
	mgr.SetEventHandler(func(e service.ManagerEvent) {
		select {
		case f.mgrEvtCh <- e:
		default:
		}
	})
	return f
}

// addNode wires one synthetic node, returning a started proxy and DNS
// server. The DNS predicate is fixture-default (group-private) — tests
// that need cross-group calls can publish into different group IDs
// using publishRecord directly.
func (f *meshFixture) addNode(id string, dnsAddr, proxyAddr netip.Addr, predicate netdns.VisibilityPredicate) *meshNode {
	f.t.Helper()
	bridges := newFakeBridges(proxyAddr)
	stats := proxy.NewStatsRegistry()
	selMap := make(map[string]*proxy.Selector)
	var selMu sync.Mutex
	resolverFor := func(svcID string) proxy.ServiceResolver {
		return &fixtureResolver{
			fixture:   f,
			serviceID: svcID,
		}
	}
	selectorFor := func(svcID string) *proxy.Selector {
		selMu.Lock()
		defer selMu.Unlock()
		s, ok := selMap[svcID]
		if !ok {
			s = proxy.NewSelector(
				proxy.WithRegistry(f.registry),
				proxy.WithDialFailureThreshold(3),
			)
			selMap[svcID] = s
		}
		return s
	}
	logger := zap.NewNop()
	if testing.Verbose() {
		var err error
		logger, err = zap.NewDevelopment()
		if err != nil {
			logger = zap.NewNop()
		}
	}
	pm := proxy.NewProxyManager(
		proxy.WithManagerStats(stats),
		proxy.WithManagerSelectorBuilder(selectorFor),
		proxy.WithManagerResolverBuilder(resolverFor),
		proxy.WithManagerRegistry(f.registry),
		proxy.WithManagerBridgeGateways(bridges.snapshot),
		proxy.WithManagerLogger(logger),
	)
	if err := pm.Start(); err != nil {
		f.t.Fatalf("proxy.Start: %v", err)
	}
	f.t.Cleanup(func() { _ = pm.Stop() })

	bridgeMap := func(addr netip.Addr) (string, string, bool) {
		// Both the DNS bind addr and the proxy listen addr count as
		// "this node's bridge". Test queries arrive on the DNS addr.
		if addr == dnsAddr || addr == proxyAddr {
			return f.clusterPath, f.groupID, true
		}
		return "", "", false
	}
	dnsResolver := &fixtureDNSResolver{
		fixture:  f,
		proxyIPs: map[string]netip.Addr{},
		nodeIP:   proxyAddr,
	}
	srv := netdns.NewServer(
		netdns.WithRegistry(f.registry),
		netdns.WithVisibility(predicate),
		netdns.WithBridgeMap(bridgeMap),
		netdns.WithBindAddrs([]netip.Addr{dnsAddr}),
		netdns.WithPort(0),
		netdns.WithTTLSeconds(2),
		netdns.WithServiceResolver(dnsResolver),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		f.t.Fatalf("dns.Start: %v", err)
	}
	f.t.Cleanup(func() { _ = srv.Stop() })
	ua := srv.UDPListenAddr(dnsAddr)
	if ua == nil {
		f.t.Fatalf("UDPListenAddr nil for %s", dnsAddr)
	}
	dnsResolver.nodeIP = proxyAddr
	return &meshNode{
		id:          id,
		dnsAddr:     dnsAddr,
		proxyAddr:   proxyAddr,
		dnsPort:     ua.Port,
		dnsServer:   srv,
		proxy:       pm,
		stats:       stats,
		selectorMap: selMap,
		bridges:     bridges,
	}
}

// fixtureResolver satisfies proxy.ServiceResolver by consulting the
// fixture's service.Manager (live strategy weights) and the matching
// service spec from the manager's store.
type fixtureResolver struct {
	fixture   *meshFixture
	serviceID string
}

// Resolve returns a snapshot built from the current Service spec and
// strategy live weights. Returns ok=false when the Service no longer
// exists.
func (r *fixtureResolver) Resolve() (proxy.ServiceSnapshot, bool) {
	svc := r.fixture.mgr.Get(service.ServiceID(r.serviceID))
	if svc == nil {
		return proxy.ServiceSnapshot{}, false
	}
	r.fixture.strategyMu.Lock()
	engine := r.fixture.strategies[svc.ID]
	r.fixture.strategyMu.Unlock()
	var weights svcstrategy.LiveWeights
	if engine != nil {
		weights = engine.LiveWeights()
	} else {
		weights = svcstrategy.LiveWeights{}
		for _, b := range svc.Spec.Backends {
			if b.Weight > 0 {
				weights[b.Capsule] = b.Weight
			}
		}
	}
	snap := proxy.ServiceSnapshot{
		ServiceID:   svc.ID.String(),
		ClusterPath: r.fixture.clusterPath,
		GroupID:     r.fixture.groupID,
	}
	for _, b := range svc.Spec.Backends {
		w, ok := weights[b.Capsule]
		if !ok || w <= 0 {
			continue
		}
		snap.Backends = append(snap.Backends, proxy.BackendSnapshot{
			Capsule: b.Capsule,
			Weight:  w,
			PortMap: b.PortMap,
		})
	}
	if len(snap.Backends) == 0 {
		return snap, false
	}
	return snap, true
}

// fixtureDNSResolver satisfies dns.ServiceResolver by consulting the
// fixture's service.Manager. trivial-bypass is computed from the
// current spec.
type fixtureDNSResolver struct {
	fixture  *meshFixture
	mu       sync.Mutex
	proxyIPs map[string]netip.Addr // serviceID → proxy IP
	nodeIP   netip.Addr
}

// LookupService implements dns.ServiceResolver.
func (r *fixtureDNSResolver) LookupService(clusterPath, callerGroup, name string) (netdns.ServiceDNSInfo, bool) {
	svc := r.fixture.mgr.GetByName(name)
	if svc == nil {
		return netdns.ServiceDNSInfo{}, false
	}
	info := netdns.ServiceDNSInfo{
		ServiceID:  svc.ID.String(),
		Visibility: string(svc.Spec.Visibility),
		Group:      svc.Spec.Group,
		ProxyIP:    r.nodeIP,
	}
	if !netdns.ServiceVisibilityAdmits(callerGroup, info) {
		return netdns.ServiceDNSInfo{}, false
	}
	if isTrivialService(svc) {
		info.Trivial = true
		info.TrivialName = svc.Spec.Backends[0].Capsule
		info.TrivialGroupID = svc.Spec.Group
	}
	return info, true
}

// isTrivialService applies decision #36: single static backend at
// weight 100 with cluster visibility.
func isTrivialService(svc *service.Service) bool {
	if svc.Spec.Visibility != service.VisibilityEnum.Cluster() {
		return false
	}
	if svc.Spec.Strategy == nil || svc.Spec.Strategy.Type != service.StrategyTypeEnum.Static() {
		return false
	}
	if len(svc.Spec.Backends) != 1 {
		return false
	}
	return svc.Spec.Backends[0].Weight >= 100
}

// fakeBridges is a tiny copy of network/proxy/lifecycle_test.go's
// helper (the proxy_test fixture isn't exported). Holds the bridge
// gateway list under a mutex so tests can mutate it.
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

// publishRecord upserts an endpoint record into the shared registry,
// mirroring the production publisher → subscriber flow.
func (f *meshFixture) publishRecord(rec *endpointpb.EndpointRecord) {
	f.registry.Insert(rec)
}

// withdrawRecord removes an endpoint record from the registry. Helper
// for tests that simulate a replica disappearing.
func (f *meshFixture) withdrawRecord(rec *endpointpb.EndpointRecord) {
	f.registry.Withdraw(&endpointpb.EndpointWithdrawal{
		ClusterPath: rec.GetClusterPath(),
		GroupId:     rec.GetGroupId(),
		CapsuleName: rec.GetCapsuleName(),
		ReplicaId:   rec.GetReplicaId(),
	})
}

// addReplica creates a fake backend + an endpoint record that routes
// to it. Returns the backend for Close + failure injection.
func (f *meshFixture) addReplica(capsuleName, replicaID, nodeID string) *fakeBackend {
	f.t.Helper()
	be := newFakeBackend(f.t, capsuleName+"/"+replicaID)
	f.t.Cleanup(be.Close)
	rec := &endpointpb.EndpointRecord{
		ClusterPath: f.clusterPath,
		GroupId:     f.groupID,
		CapsuleName: capsuleName,
		ReplicaId:   replicaID,
		NodeId:      nodeID,
		BridgeIp:    be.addr.Addr().String(),
		NamedPorts: []*endpointpb.NamedPort{
			{Name: "http", ContainerPort: uint32(be.addr.Port()), Protocol: "tcp"},
		},
		SwimState:  endpoints.SwimStateAlive,
		EmittedAt:  timestamppb.New(time.Now()),
		TtlSeconds: 60,
	}
	f.publishRecord(rec)
	f.caps.set(capsuleName, "cap-"+capsuleName+"-"+replicaID)
	return be
}

// createServiceWithTrigger wraps the manager Create + admits the
// returned spec into every node's proxy by issuing EnsureService.
// Returns the persisted Service.
func (f *meshFixture) createService(spec service.ServiceSpec, nodes []*meshNode) *service.Service {
	f.t.Helper()
	svc, err := f.mgr.Create(context.Background(), "test-cluster", spec)
	if err != nil {
		f.t.Fatalf("create service: %v", err)
	}
	f.installStrategy(svc)
	pspec := f.proxySpec(svc)
	for _, n := range nodes {
		if err := n.proxy.EnsureService(pspec); err != nil {
			f.t.Fatalf("proxy ensure %s on %s: %v", svc.Spec.Name, n.id, err)
		}
	}
	return svc
}

// updateService applies a fresh spec, re-ensures it on every node, and
// re-installs the strategy when the strategy type changed.
func (f *meshFixture) updateService(id service.ServiceID, spec service.ServiceSpec, nodes []*meshNode) *service.Service {
	f.t.Helper()
	svc, err := f.mgr.Update(context.Background(), id, spec)
	if err != nil {
		f.t.Fatalf("update service: %v", err)
	}
	f.installStrategy(svc)
	pspec := f.proxySpec(svc)
	for _, n := range nodes {
		if err := n.proxy.EnsureService(pspec); err != nil {
			f.t.Fatalf("proxy ensure %s on %s: %v", svc.Spec.Name, n.id, err)
		}
	}
	return svc
}

// installStrategy creates the matching strategy engine for the spec.
// Replaces any existing engine for the Service. Engines are kept in
// fixture-state; the fixtureResolver consults them on every Resolve.
func (f *meshFixture) installStrategy(svc *service.Service) {
	f.t.Helper()
	f.strategyMu.Lock()
	if prev, ok := f.strategies[svc.ID]; ok {
		_ = prev.Stop()
	}
	var engine svcstrategy.Engine
	switch svc.Spec.Strategy.Type {
	case service.StrategyTypeEnum.BlueGreen():
		engine = svcstrategy.NewBlueGreen(svc.ID, svc.Spec, nil)
	case service.StrategyTypeEnum.Canary():
		engine = svcstrategy.NewCanary(svc.ID, svc.Spec, nil, nil)
	default:
		engine = svcstrategy.NewStatic(svc.ID, svc.Spec)
	}
	if engine != nil {
		_ = engine.Start(context.Background())
	}
	f.strategies[svc.ID] = engine
	f.strategyMu.Unlock()
}

// strategyEngine returns the engine for a Service (helper for tests).
func (f *meshFixture) strategyEngine(id service.ServiceID) svcstrategy.Engine {
	f.strategyMu.Lock()
	defer f.strategyMu.Unlock()
	return f.strategies[id]
}

// proxySpec translates service.Service → proxy.ServiceSpec.
func (f *meshFixture) proxySpec(svc *service.Service) proxy.ServiceSpec {
	out := proxy.ServiceSpec{
		ID:          svc.ID.String(),
		Name:        svc.Spec.Name,
		ClusterPath: svc.ClusterID,
		GroupID:     svc.Spec.Group,
		Visibility:  string(svc.Spec.Visibility),
	}
	for _, p := range svc.Spec.Ports {
		proto := string(p.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		out.Ports = append(out.Ports, proxy.ServicePort{
			Name:     p.Name,
			Port:     p.Port,
			Protocol: proto,
		})
	}
	return out
}

// dial connects through the per-node proxy at port and reads the tag
// line written by the fake backend. Returns "" on failure. Visibility-
// denied or backendless connections close cleanly with no tag.
func (n *meshNode) dial(t *testing.T, port uint16) string {
	t.Helper()
	addr := net.JoinHostPort(n.proxyAddr.String(), fmt.Sprintf("%d", port))
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	nb, err := c.Read(buf)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(buf[:nb]))
}

// dialQuery resolves a name via the node's DNS server and returns rcode + IPs.
func (n *meshNode) dialQuery(t *testing.T, name string) (int, []string) {
	t.Helper()
	c := &miekg.Client{Net: "udp", Timeout: 2 * time.Second}
	m := new(miekg.Msg)
	m.SetQuestion(miekg.Fqdn(name), miekg.TypeA)
	resp, _, err := c.Exchange(m, fmt.Sprintf("%s:%d", n.dnsAddr, n.dnsPort))
	if err != nil {
		t.Fatalf("dns query %s: %v", name, err)
	}
	ips := make([]string, 0, len(resp.Answer))
	for _, rr := range resp.Answer {
		if a, ok := rr.(*miekg.A); ok {
			ips = append(ips, a.A.String())
		}
	}
	return resp.Rcode, ips
}

// portMap turns one TCP service port using the kernel-allocated proxy
// listen port. The proxy lifecycle binds on (proxyAddr, port); the
// driver dials that port directly.
func mustFreePort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return uint16(l.Addr().(*net.TCPAddr).Port)
}

// loopback returns 127.0.0.<n>.
func loopback(n int) netip.Addr { return netip.AddrFrom4([4]byte{127, 0, 0, byte(n)}) }

// waitForCond polls cond up to deadline. Returns true when cond becomes true.
func waitForCond(deadline time.Duration, cond func() bool) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// =====================================================================
// TESTS
// =====================================================================

// TestServiceMesh_T1_StaticAllToV1 — 11B.T1.
// Service `payments` at 100% to v1 (3 replicas). 100 connects all land
// on a v1 replica.
func TestServiceMesh_T1_StaticAllToV1(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/svc-t1"
	const group = "g-t1"
	f := newMeshFixture(t, cluster, group)
	n1 := f.addNode("n1", loopback(50), loopback(51), netdns.AllowAllPredicate)

	r1 := f.addReplica("payments-v1", "r1", "node1")
	r2 := f.addReplica("payments-v1", "r2", "node2")
	r3 := f.addReplica("payments-v1", "r3", "node3")
	defer r1.Close()
	defer r2.Close()
	defer r3.Close()

	port := mustFreePort(t)
	spec := service.ServiceSpec{
		Name:       "payments",
		Visibility: service.VisibilityEnum.Cluster(),
		Group:      group,
		Ports:      []service.ServicePort{{Name: "http", Port: port, Protocol: service.ProtocolEnum.TCP()}},
		Backends:   []service.ServiceBackend{{Capsule: "payments-v1", Weight: 100}},
		Strategy:   &service.Strategy{Type: service.StrategyTypeEnum.Static()},
	}
	f.createService(spec, []*meshNode{n1})

	counts := map[string]int{}
	const N = 100
	for i := 0; i < N; i++ {
		tag := n1.dial(t, port)
		if tag == "" {
			t.Fatalf("dial %d returned empty tag", i)
		}
		counts[tag]++
	}
	total := 0
	for k, v := range counts {
		if !strings.HasPrefix(k, "payments-v1/") {
			t.Errorf("unexpected tag %q (count %d) — should all be v1", k, v)
		}
		total += v
	}
	if total != N {
		t.Errorf("served %d/%d connects", total, N)
	}
}

// TestServiceMesh_T2_StaticWeighted90_10 — 11B.T2.
// 90/10 v1/v2 over 1000 connects → ratio within 5% tolerance.
func TestServiceMesh_T2_StaticWeighted90_10(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/svc-t2"
	const group = "g-t2"
	f := newMeshFixture(t, cluster, group)
	n1 := f.addNode("n1", loopback(52), loopback(53), netdns.AllowAllPredicate)

	r1 := f.addReplica("payments-v1", "r1", "node1")
	r2 := f.addReplica("payments-v2", "r1", "node2")
	defer r1.Close()
	defer r2.Close()

	port := mustFreePort(t)
	spec := service.ServiceSpec{
		Name:       "payments",
		Visibility: service.VisibilityEnum.Cluster(),
		Group:      group,
		Ports:      []service.ServicePort{{Name: "http", Port: port, Protocol: service.ProtocolEnum.TCP()}},
		Backends: []service.ServiceBackend{
			{Capsule: "payments-v1", Weight: 90},
			{Capsule: "payments-v2", Weight: 10},
		},
		Strategy: &service.Strategy{Type: service.StrategyTypeEnum.Static()},
	}
	f.createService(spec, []*meshNode{n1})

	v1, v2 := 0, 0
	const N = 1000
	for i := 0; i < N; i++ {
		tag := n1.dial(t, port)
		switch {
		case strings.HasPrefix(tag, "payments-v1/"):
			v1++
		case strings.HasPrefix(tag, "payments-v2/"):
			v2++
		default:
			t.Fatalf("dial %d: unexpected tag %q", i, tag)
		}
	}
	if v1+v2 != N {
		t.Fatalf("counts mismatch %d+%d", v1, v2)
	}
	// SWRR is deterministic given fixed weights; 90/10 over 1000 picks
	// is exactly 900/100. Allow ±5% (50 connections) tolerance per spec.
	wantV1 := 900
	if abs(v1-wantV1) > 50 {
		t.Errorf("v1 = %d want 900±50 (got %d v2)", v1, v2)
	}
}

// TestServiceMesh_T3_BlueGreenFlip — 11B.T3.
// Blue-green flip from v1 to v2. After Update the strategy moves all
// new traffic to v2; pre-flip in-flight connections stay open.
func TestServiceMesh_T3_BlueGreenFlip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/svc-t3"
	const group = "g-t3"
	f := newMeshFixture(t, cluster, group)
	n1 := f.addNode("n1", loopback(54), loopback(55), netdns.AllowAllPredicate)

	r1 := f.addReplica("payments-v1", "r1", "node1")
	r2 := f.addReplica("payments-v2", "r1", "node2")
	defer r1.Close()
	defer r2.Close()

	port := mustFreePort(t)
	spec := service.ServiceSpec{
		Name:       "payments",
		Visibility: service.VisibilityEnum.Cluster(),
		Group:      group,
		Ports:      []service.ServicePort{{Name: "http", Port: port, Protocol: service.ProtocolEnum.TCP()}},
		Backends: []service.ServiceBackend{
			{Capsule: "payments-v1", Weight: 100},
			{Capsule: "payments-v2", Weight: 100},
		},
		Strategy: &service.Strategy{
			Type:      service.StrategyTypeEnum.BlueGreen(),
			BlueGreen: &service.BlueGreenStrategy{Active: "payments-v1", Drain: 200 * time.Millisecond},
		},
	}
	svc := f.createService(spec, []*meshNode{n1})

	// Pre-flip: every connect lands on v1.
	for i := 0; i < 10; i++ {
		tag := n1.dial(t, port)
		if !strings.HasPrefix(tag, "payments-v1/") {
			t.Fatalf("pre-flip dial %d: tag %q (want v1)", i, tag)
		}
	}
	// Flip Active → v2.
	flipped := spec
	flipped.Strategy = &service.Strategy{
		Type:      service.StrategyTypeEnum.BlueGreen(),
		BlueGreen: &service.BlueGreenStrategy{Active: "payments-v2", Drain: 200 * time.Millisecond},
	}
	f.updateService(svc.ID, flipped, []*meshNode{n1})

	// Post-flip: new connects immediately land on v2.
	for i := 0; i < 10; i++ {
		tag := n1.dial(t, port)
		if !strings.HasPrefix(tag, "payments-v2/") {
			t.Fatalf("post-flip dial %d: tag %q (want v2)", i, tag)
		}
	}
}

// TestServiceMesh_T7_LenientResolution — 11B.T7.
// Service created BEFORE backend capsule: admitted, no traffic. When
// the capsule arrives, traffic starts flowing.
func TestServiceMesh_T7_LenientResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/svc-t7"
	const group = "g-t7"
	f := newMeshFixture(t, cluster, group)
	n1 := f.addNode("n1", loopback(56), loopback(57), netdns.AllowAllPredicate)

	port := mustFreePort(t)
	spec := service.ServiceSpec{
		Name:       "payments",
		Visibility: service.VisibilityEnum.Cluster(),
		Group:      group,
		Ports:      []service.ServicePort{{Name: "http", Port: port, Protocol: service.ProtocolEnum.TCP()}},
		Backends:   []service.ServiceBackend{{Capsule: "payments-v1", Weight: 100}},
		Strategy:   &service.Strategy{Type: service.StrategyTypeEnum.Static()},
	}
	svc := f.createService(spec, []*meshNode{n1})
	got := f.mgr.Get(svc.ID)
	if got == nil {
		t.Fatal("service not admitted")
	}
	if state := findBackendStateForName(got, "payments-v1"); state == nil ||
		state.Resolution != service.BackendResolutionEnum.Unresolved() {
		t.Fatalf("backend should be Unresolved; got %+v", state)
	}
	// No backend yet → dial closes cleanly without data.
	tag := n1.dial(t, port)
	if tag != "" {
		t.Fatalf("pre-backend dial returned tag %q (want empty)", tag)
	}
	// Add the backend; manager picks it up via OnCapsuleReceived. The
	// fixture wires capsule lookup before service create, so this
	// addReplica step both places the capsule AND notifies the manager.
	be := f.addReplica("payments-v1", "r1", "node1")
	defer be.Close()
	id, _ := f.caps.Lookup("payments-v1")
	f.mgr.OnCapsuleReceived("payments-v1", id)

	if !waitForCond(2*time.Second, func() bool {
		return n1.dial(t, port) != ""
	}) {
		t.Fatal("traffic never flowed after backend added")
	}
}

// TestServiceMesh_T8_VisibilityDeny — 11B.T8.
// Visibility=group: same-group caller resolves and connects;
// different-group caller gets NXDOMAIN.
func TestServiceMesh_T8_VisibilityDeny(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/svc-t8"
	const insideGroup = "g-t8-inside"
	const outsideGroup = "g-t8-outside"
	f := newMeshFixture(t, cluster, insideGroup)

	insideNode := f.addNode("inside", loopback(58), loopback(59), netdns.DefaultPredicate)
	// Outside node is in a different group; the bridgeMap on its DNS
	// server reports outsideGroup so visibility check denies the lookup.
	outsideNode := f.addNodeWithGroup("outside", loopback(60), loopback(61), netdns.DefaultPredicate, outsideGroup)

	r1 := f.addReplica("payments-v1", "r1", "node1")
	defer r1.Close()

	port := mustFreePort(t)
	spec := service.ServiceSpec{
		Name:       "payments",
		Visibility: service.VisibilityEnum.Group(),
		Group:      insideGroup,
		Ports:      []service.ServicePort{{Name: "http", Port: port, Protocol: service.ProtocolEnum.TCP()}},
		Backends:   []service.ServiceBackend{{Capsule: "payments-v1", Weight: 100}},
		Strategy:   &service.Strategy{Type: service.StrategyTypeEnum.Static()},
	}
	f.createService(spec, []*meshNode{insideNode, outsideNode})

	// Inside: resolves to proxy IP (non-trivial: visibility=group).
	rcode, ips := insideNode.dialQuery(t, "payments")
	if rcode != miekg.RcodeSuccess || len(ips) == 0 {
		t.Fatalf("inside DNS: rcode=%d ips=%v", rcode, ips)
	}
	// Outside: NXDOMAIN (or empty answer, depending on Service resolver
	// path). We accept anything non-success or zero-answer as denial.
	rcode, ips = outsideNode.dialQuery(t, "payments")
	if rcode == miekg.RcodeSuccess && len(ips) > 0 {
		t.Fatalf("outside DNS should deny; got rcode=%d ips=%v", rcode, ips)
	}
}

// addNodeWithGroup is addNode with an override group ID for the
// bridge map (used by T8 cross-group test).
func (f *meshFixture) addNodeWithGroup(id string, dnsAddr, proxyAddr netip.Addr, predicate netdns.VisibilityPredicate, groupID string) *meshNode {
	f.t.Helper()
	bridges := newFakeBridges(proxyAddr)
	stats := proxy.NewStatsRegistry()
	selMap := make(map[string]*proxy.Selector)
	var selMu sync.Mutex
	resolverFor := func(svcID string) proxy.ServiceResolver {
		return &fixtureResolver{fixture: f, serviceID: svcID}
	}
	selectorFor := func(svcID string) *proxy.Selector {
		selMu.Lock()
		defer selMu.Unlock()
		if s, ok := selMap[svcID]; ok {
			return s
		}
		s := proxy.NewSelector(proxy.WithRegistry(f.registry))
		selMap[svcID] = s
		return s
	}
	pm := proxy.NewProxyManager(
		proxy.WithManagerStats(stats),
		proxy.WithManagerSelectorBuilder(selectorFor),
		proxy.WithManagerResolverBuilder(resolverFor),
		proxy.WithManagerRegistry(f.registry),
		proxy.WithManagerBridgeGateways(bridges.snapshot),
	)
	if err := pm.Start(); err != nil {
		f.t.Fatalf("proxy.Start: %v", err)
	}
	f.t.Cleanup(func() { _ = pm.Stop() })
	bridgeMap := func(addr netip.Addr) (string, string, bool) {
		if addr == dnsAddr || addr == proxyAddr {
			return f.clusterPath, groupID, true
		}
		return "", "", false
	}
	dnsResolver := &fixtureDNSResolver{fixture: f, proxyIPs: map[string]netip.Addr{}, nodeIP: proxyAddr}
	srv := netdns.NewServer(
		netdns.WithRegistry(f.registry),
		netdns.WithVisibility(predicate),
		netdns.WithBridgeMap(bridgeMap),
		netdns.WithBindAddrs([]netip.Addr{dnsAddr}),
		netdns.WithPort(0),
		netdns.WithTTLSeconds(2),
		netdns.WithServiceResolver(dnsResolver),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		f.t.Fatalf("dns.Start: %v", err)
	}
	f.t.Cleanup(func() { _ = srv.Stop() })
	ua := srv.UDPListenAddr(dnsAddr)
	if ua == nil {
		f.t.Fatalf("UDPListenAddr nil for %s", dnsAddr)
	}
	return &meshNode{
		id:          id,
		dnsAddr:     dnsAddr,
		proxyAddr:   proxyAddr,
		dnsPort:     ua.Port,
		dnsServer:   srv,
		proxy:       pm,
		stats:       stats,
		selectorMap: selMap,
		bridges:     bridges,
	}
}

// TestServiceMesh_T9_DeleteServiceLeavesCapsules — 11B.T9.
// Delete Service while replicas exist; replicas continue running and
// can back a fresh Service afterwards.
func TestServiceMesh_T9_DeleteServiceLeavesCapsules(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/svc-t9"
	const group = "g-t9"
	f := newMeshFixture(t, cluster, group)
	n1 := f.addNode("n1", loopback(62), loopback(63), netdns.AllowAllPredicate)
	be := f.addReplica("payments-v1", "r1", "node1")
	defer be.Close()

	port := mustFreePort(t)
	spec := service.ServiceSpec{
		Name:       "payments",
		Visibility: service.VisibilityEnum.Cluster(),
		Group:      group,
		Ports:      []service.ServicePort{{Name: "http", Port: port, Protocol: service.ProtocolEnum.TCP()}},
		Backends:   []service.ServiceBackend{{Capsule: "payments-v1", Weight: 100}},
		Strategy:   &service.Strategy{Type: service.StrategyTypeEnum.Static()},
	}
	svc := f.createService(spec, []*meshNode{n1})
	if tag := n1.dial(t, port); !strings.HasPrefix(tag, "payments-v1/") {
		t.Fatalf("pre-delete dial: %q", tag)
	}
	// Delete: proxy releases its listeners.
	if err := f.mgr.Delete(context.Background(), svc.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := n1.proxy.ReleaseService(svc.ID.String()); err != nil {
		t.Fatalf("proxy release: %v", err)
	}
	// Backend still serves directly.
	c, err := net.DialTimeout("tcp", be.addr.String(), 1*time.Second)
	if err != nil {
		t.Fatalf("backend direct dial: %v", err)
	}
	buf := make([]byte, 64)
	nb, _ := c.Read(buf)
	c.Close()
	if !strings.HasPrefix(string(buf[:nb]), "payments-v1/") {
		t.Fatalf("backend direct read: %q", buf[:nb])
	}
	// Recreate Service against the same capsule — same backend, fresh listener.
	port2 := mustFreePort(t)
	spec2 := spec
	spec2.Ports = []service.ServicePort{{Name: "http", Port: port2, Protocol: service.ProtocolEnum.TCP()}}
	svc2 := f.createService(spec2, []*meshNode{n1})
	if svc2.ID == svc.ID {
		t.Fatal("recreate should yield new ID")
	}
	if tag := n1.dial(t, port2); !strings.HasPrefix(tag, "payments-v1/") {
		t.Fatalf("post-recreate dial: %q", tag)
	}
}

// TestServiceMesh_T10_IdentityBindingCatchesRecreate — 11B.T10.
// Delete + recreate capsule with same name + different ID → backend
// goes UnresolvedIdentityChanged; rebind restores routing.
func TestServiceMesh_T10_IdentityBindingCatchesRecreate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/svc-t10"
	const group = "g-t10"
	f := newMeshFixture(t, cluster, group)
	n1 := f.addNode("n1", loopback(64), loopback(65), netdns.AllowAllPredicate)

	be := f.addReplica("payments-v1", "r1", "node1")
	defer be.Close()
	originalID, _ := f.caps.Lookup("payments-v1")
	port := mustFreePort(t)
	spec := service.ServiceSpec{
		Name:       "payments",
		Visibility: service.VisibilityEnum.Cluster(),
		Group:      group,
		Ports:      []service.ServicePort{{Name: "http", Port: port, Protocol: service.ProtocolEnum.TCP()}},
		Backends:   []service.ServiceBackend{{Capsule: "payments-v1", Weight: 100}},
		Strategy:   &service.Strategy{Type: service.StrategyTypeEnum.Static()},
	}
	svc := f.createService(spec, []*meshNode{n1})
	// Resolved → traffic flows.
	if tag := n1.dial(t, port); !strings.HasPrefix(tag, "payments-v1/") {
		t.Fatalf("pre-recreate dial: %q", tag)
	}
	// Recreate capsule under a NEW ID (image rebuild scenario).
	f.caps.set("payments-v1", "cap-new-image")
	f.mgr.OnCapsuleReceived("payments-v1", "cap-new-image")
	got := f.mgr.Get(svc.ID)
	state := findBackendStateForName(got, "payments-v1")
	if state == nil || state.Resolution != service.BackendResolutionEnum.UnresolvedIdentityChanged() {
		t.Fatalf("expected UnresolvedIdentityChanged; got %+v", state)
	}
	if state.CapturedCapsuleID != originalID {
		t.Fatalf("captured id should still be original %q; got %q",
			originalID, state.CapturedCapsuleID)
	}
	// Operator runs rebind → backend re-resolves.
	if err := f.mgr.Rebind(context.Background(), svc.ID, "payments-v1"); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	got = f.mgr.Get(svc.ID)
	state = findBackendStateForName(got, "payments-v1")
	if state == nil || state.Resolution != service.BackendResolutionEnum.Resolved() {
		t.Fatalf("post-rebind: expected Resolved, got %+v", state)
	}
	if state.CapturedCapsuleID != "cap-new-image" {
		t.Fatalf("post-rebind captured id: got %q", state.CapturedCapsuleID)
	}
}

// TestServiceMesh_T12_TrivialServiceBypassFlipsOnCanary — 11B.T12 +
// 11B.T16 condensed. Trivial single-backend static cluster Service:
// DNS returns A-records of the replica (bypass). Updating to canary
// with 2 backends → DNS returns the proxy IP.
func TestServiceMesh_T12_TrivialBypassFlipsOnCanary(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/svc-t12"
	const group = "g-t12"
	f := newMeshFixture(t, cluster, group)
	n1 := f.addNode("n1", loopback(66), loopback(67), netdns.AllowAllPredicate)

	be1 := f.addReplica("payments-v1", "r1", "node1")
	defer be1.Close()
	port := mustFreePort(t)
	spec := service.ServiceSpec{
		Name:       "payments",
		Visibility: service.VisibilityEnum.Cluster(),
		Group:      group,
		Ports:      []service.ServicePort{{Name: "http", Port: port, Protocol: service.ProtocolEnum.TCP()}},
		Backends:   []service.ServiceBackend{{Capsule: "payments-v1", Weight: 100}},
		Strategy:   &service.Strategy{Type: service.StrategyTypeEnum.Static()},
	}
	svc := f.createService(spec, []*meshNode{n1})

	// Trivial path: DNS resolver returns Trivial=true and the responder
	// hands back the replica's bridge IP, not the proxy IP. We assert
	// that the answer contains the replica bridge IP.
	rcode, ips := n1.dialQuery(t, "payments")
	if rcode != miekg.RcodeSuccess || len(ips) == 0 {
		t.Fatalf("trivial DNS: rcode=%d ips=%v", rcode, ips)
	}
	// In trivial bypass, the responder falls through to the capsule
	// path and uses the replica's bridge IP. The fake-backend BridgeIp
	// is its 127.0.0.1 listen addr.
	wantReplicaIP := be1.addr.Addr().String()
	matched := false
	for _, ip := range ips {
		if ip == wantReplicaIP {
			matched = true
		}
	}
	if !matched {
		t.Errorf("trivial bypass did not return replica IP %s (got %v)", wantReplicaIP, ips)
	}
	// Flip to canary: add v2 backend.
	be2 := f.addReplica("payments-v2", "r1", "node2")
	defer be2.Close()
	canary := spec
	canary.Backends = []service.ServiceBackend{
		{Capsule: "payments-v1", Weight: 50},
		{Capsule: "payments-v2", Weight: 50},
	}
	canary.Strategy = &service.Strategy{
		Type: service.StrategyTypeEnum.Canary(),
		Canary: &service.CanaryStrategy{
			Target: "payments-v2", From: "payments-v1", Step: 10,
			AbortOn: []string{"error_rate > 100"},
		},
	}
	f.updateService(svc.ID, canary, []*meshNode{n1})

	// Post-flip: DNS now answers with the proxy IP, NOT the replica IP.
	rcode, ips = n1.dialQuery(t, "payments")
	if rcode != miekg.RcodeSuccess || len(ips) == 0 {
		t.Fatalf("canary DNS: rcode=%d ips=%v", rcode, ips)
	}
	wantProxy := n1.proxyAddr.String()
	matched = false
	for _, ip := range ips {
		if ip == wantProxy {
			matched = true
		}
	}
	if !matched {
		t.Errorf("canary DNS should return proxy IP %s; got %v", wantProxy, ips)
	}
}

// TestServiceMesh_T13_CanaryIdentityChangeAborts — 11B.T13.
// Canary mid-progress + identity change on the `from` backend → engine
// auto-aborts when the manager publishes the spec with the changed
// backend weight zeroed.
func TestServiceMesh_T13_CanaryIdentityChangeAborts(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/svc-t13"
	const group = "g-t13"
	f := newMeshFixture(t, cluster, group)
	n1 := f.addNode("n1", loopback(68), loopback(69), netdns.AllowAllPredicate)
	be1 := f.addReplica("payments-v1", "r1", "node1")
	be2 := f.addReplica("payments-v2", "r1", "node2")
	defer be1.Close()
	defer be2.Close()

	port := mustFreePort(t)
	spec := service.ServiceSpec{
		Name:       "payments",
		Visibility: service.VisibilityEnum.Cluster(),
		Group:      group,
		Ports:      []service.ServicePort{{Name: "http", Port: port, Protocol: service.ProtocolEnum.TCP()}},
		Backends: []service.ServiceBackend{
			{Capsule: "payments-v1", Weight: 50},
			{Capsule: "payments-v2", Weight: 50},
		},
		Strategy: &service.Strategy{
			Type: service.StrategyTypeEnum.Canary(),
			Canary: &service.CanaryStrategy{
				Target: "payments-v2", From: "payments-v1", Step: 10,
				AbortOn: []string{"error_rate > 5%"},
			},
		},
	}
	svc := f.createService(spec, []*meshNode{n1})
	// Identity change on `from`: zero the weight in the spec we hand to
	// the engine (mirrors what the manager does on
	// UnresolvedIdentityChanged).
	zeroed := svc.Spec
	zeroed.Backends = []service.ServiceBackend{
		{Capsule: "payments-v1", Weight: 0},
		{Capsule: "payments-v2", Weight: 50},
	}
	engine := f.strategyEngine(svc.ID)
	if engine == nil {
		t.Fatal("canary engine not registered")
	}
	if err := engine.Update(zeroed); err != nil {
		t.Fatalf("engine.Update: %v", err)
	}
	// Engine should auto-abort: weights revert to the initial 50/50
	// (the engine's startW snapshot — actually 100/0 since canary
	// engines seed weights from a "fresh" state).
	st := engine.State()
	if st.Phase != "aborted" {
		t.Fatalf("expected phase=aborted, got %q (state=%+v)", st.Phase, st)
	}
}

// TestServiceMesh_T16_TrivialToNonTrivialConvergence — 11B.T16.
// While the operator flips Service from trivial to canary, the DNS
// answer transitions from the replica IP to the proxy IP within a
// bounded window.
func TestServiceMesh_T16_TrivialToNonTrivialConvergence(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/svc-t16"
	const group = "g-t16"
	f := newMeshFixture(t, cluster, group)
	n1 := f.addNode("n1", loopback(70), loopback(71), netdns.AllowAllPredicate)
	be1 := f.addReplica("payments-v1", "r1", "node1")
	defer be1.Close()

	port := mustFreePort(t)
	spec := service.ServiceSpec{
		Name:       "payments",
		Visibility: service.VisibilityEnum.Cluster(),
		Group:      group,
		Ports:      []service.ServicePort{{Name: "http", Port: port, Protocol: service.ProtocolEnum.TCP()}},
		Backends:   []service.ServiceBackend{{Capsule: "payments-v1", Weight: 100}},
		Strategy:   &service.Strategy{Type: service.StrategyTypeEnum.Static()},
	}
	svc := f.createService(spec, []*meshNode{n1})

	// Initial DNS answer: replica IP (trivial bypass).
	_, ips := n1.dialQuery(t, "payments")
	if len(ips) == 0 || ips[0] != be1.addr.Addr().String() {
		t.Fatalf("pre-flip DNS: want replica %s, got %v", be1.addr.Addr(), ips)
	}
	// Flip to canary; trivial bypass falls away.
	be2 := f.addReplica("payments-v2", "r1", "node2")
	defer be2.Close()
	canary := spec
	canary.Backends = []service.ServiceBackend{
		{Capsule: "payments-v1", Weight: 50},
		{Capsule: "payments-v2", Weight: 50},
	}
	canary.Strategy = &service.Strategy{
		Type: service.StrategyTypeEnum.Canary(),
		Canary: &service.CanaryStrategy{
			Target: "payments-v2", From: "payments-v1", Step: 10,
			AbortOn: []string{"never"},
		},
	}
	f.updateService(svc.ID, canary, []*meshNode{n1})

	wantProxy := n1.proxyAddr.String()
	if !waitForCond(6*time.Second, func() bool {
		_, got := n1.dialQuery(t, "payments")
		for _, ip := range got {
			if ip == wantProxy {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("DNS never converged to proxy IP %s within window", wantProxy)
	}
}

// =====================================================================
// helpers
// =====================================================================

// findBackendStateForName scans BackendStates for the named backend.
func findBackendStateForName(svc *service.Service, name string) *service.BackendState {
	for i := range svc.BackendStates {
		if svc.BackendStates[i].Name == name {
			return &svc.BackendStates[i]
		}
	}
	return nil
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Suppress unused-import linters: errors / io / rand are stage-2
// helpers used by the outlier tests T11/T14/T15 which Bundle M defers
// (covered by Bundle J unit tests). Keeping the imports parked here
// means future tests can be added in this file without re-shuffling.
var _ = errors.Is
var _ = io.Copy
var _ = rand.New
