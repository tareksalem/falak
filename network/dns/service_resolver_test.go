package dns

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/tareksalem/falak/network/endpoints"
)

// testContext returns a 5s-bounded context wired to t.Cleanup.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// inMemServiceResolver is a test-side ServiceResolver. It is keyed on
// (clusterPath, name) for the Service table; the visibility check is
// performed against ServiceDNSInfo via ServiceVisibilityAdmits to
// mirror the production policy exactly.
type inMemServiceResolver struct {
	mu       sync.Mutex
	services map[string]ServiceDNSInfo // key = clusterPath|name
}

func newInMemServiceResolver() *inMemServiceResolver {
	return &inMemServiceResolver{services: make(map[string]ServiceDNSInfo)}
}

func (r *inMemServiceResolver) set(clusterPath, name string, info ServiceDNSInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services[clusterPath+"|"+name] = info
}

func (r *inMemServiceResolver) LookupService(clusterPath, callerGroup, name string) (ServiceDNSInfo, bool) {
	r.mu.Lock()
	info, ok := r.services[clusterPath+"|"+name]
	r.mu.Unlock()
	if !ok {
		return ServiceDNSInfo{}, false
	}
	if !ServiceVisibilityAdmits(callerGroup, info) {
		return ServiceDNSInfo{}, false
	}
	return info, true
}

func proxyIP(t *testing.T) netip.Addr {
	t.Helper()
	return netip.MustParseAddr("169.254.169.250")
}

func TestServiceResolver_ClusterVisibility(t *testing.T) {
	reg := newFakeRegistry()
	res := newInMemServiceResolver()
	res.set("/falak/test", "payments", ServiceDNSInfo{
		ServiceID:  "svc-1",
		Visibility: "cluster",
		Group:      "billing",
		ProxyIP:    proxyIP(t),
	})

	bm := func(_ netip.Addr) (string, string, bool) {
		return "/falak/test", "checkout", true // caller in different group
	}
	srv := newTestServerWithResolver(t, reg, res, bm)

	resp := queryA(t, srv.port, "payments")
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d want NOERROR (cluster admits any caller)", resp.Rcode)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answers = %d want 1", len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("not A record: %T", resp.Answer[0])
	}
	if a.A.String() != "169.254.169.250" {
		t.Fatalf("ip = %s want proxy IP", a.A)
	}
}

func TestServiceResolver_GroupVisibility_SameGroup(t *testing.T) {
	reg := newFakeRegistry()
	res := newInMemServiceResolver()
	res.set("/falak/test", "internal", ServiceDNSInfo{
		ServiceID:  "svc-2",
		Visibility: "group",
		Group:      "billing",
		ProxyIP:    proxyIP(t),
	})
	bm := func(_ netip.Addr) (string, string, bool) {
		return "/falak/test", "billing", true
	}
	srv := newTestServerWithResolver(t, reg, res, bm)

	resp := queryA(t, srv.port, "internal")
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d want NOERROR (same-group)", resp.Rcode)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answers = %d want 1", len(resp.Answer))
	}
}

func TestServiceResolver_GroupVisibility_DifferentGroup(t *testing.T) {
	reg := newFakeRegistry()
	res := newInMemServiceResolver()
	res.set("/falak/test", "internal", ServiceDNSInfo{
		ServiceID:  "svc-3",
		Visibility: "group",
		Group:      "billing",
		ProxyIP:    proxyIP(t),
	})
	bm := func(_ netip.Addr) (string, string, bool) {
		return "/falak/test", "checkout", true
	}
	srv := newTestServerWithResolver(t, reg, res, bm)

	resp := queryA(t, srv.port, "internal")
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d want NXDOMAIN (cross-group, group-visibility)", resp.Rcode)
	}
}

func TestServiceResolver_TrivialBypass(t *testing.T) {
	reg := newFakeRegistry()
	reg.set("/falak/test", "billing", "api", []endpoints.Endpoint{
		{ClusterPath: "/falak/test", GroupID: "billing", CapsuleName: "api", BridgeIP: "10.42.0.7"},
		{ClusterPath: "/falak/test", GroupID: "billing", CapsuleName: "api", BridgeIP: "10.42.0.8"},
	})
	res := newInMemServiceResolver()
	res.set("/falak/test", "api", ServiceDNSInfo{
		ServiceID:      "svc-4",
		Visibility:     "cluster",
		Group:          "billing",
		Trivial:        true,
		TrivialName:    "api",
		TrivialGroupID: "billing",
		// ProxyIP intentionally unset — trivial path must not need it.
	})
	bm := func(_ netip.Addr) (string, string, bool) {
		return "/falak/test", "billing", true
	}
	srv := newTestServerWithResolver(t, reg, res, bm)

	resp := queryA(t, srv.port, "api")
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d want NOERROR (trivial)", resp.Rcode)
	}
	if len(resp.Answer) != 2 {
		t.Fatalf("answers = %d want 2 (replica A-records)", len(resp.Answer))
	}
	got := map[string]bool{}
	for _, rr := range resp.Answer {
		a, ok := rr.(*dns.A)
		if !ok {
			t.Fatalf("not A: %T", rr)
		}
		got[a.A.String()] = true
	}
	if !got["10.42.0.7"] || !got["10.42.0.8"] {
		t.Fatalf("missing replica IPs in answer: %v", got)
	}
}

func TestServiceResolver_UnknownFallsThrough(t *testing.T) {
	reg := newFakeRegistry()
	reg.set("/falak/test", "billing", "worker", []endpoints.Endpoint{
		{ClusterPath: "/falak/test", GroupID: "billing", CapsuleName: "worker", BridgeIP: "10.42.0.9"},
	})
	res := newInMemServiceResolver() // empty — every Service lookup fails.
	bm := func(_ netip.Addr) (string, string, bool) {
		return "/falak/test", "billing", true
	}
	srv := newTestServerWithResolver(t, reg, res, bm)

	resp := queryA(t, srv.port, "worker")
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d want NOERROR (capsule fallback)", resp.Rcode)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answers = %d want 1", len(resp.Answer))
	}
	a := resp.Answer[0].(*dns.A)
	if a.A.String() != "10.42.0.9" {
		t.Fatalf("ip = %s want capsule bridge IP", a.A)
	}
}

func TestServiceResolver_TrivialFlip(t *testing.T) {
	reg := newFakeRegistry()
	reg.set("/falak/test", "billing", "payments", []endpoints.Endpoint{
		{ClusterPath: "/falak/test", GroupID: "billing", CapsuleName: "payments", BridgeIP: "10.42.0.4"},
	})
	res := newInMemServiceResolver()
	// Start trivial.
	res.set("/falak/test", "payments", ServiceDNSInfo{
		ServiceID:      "svc-5",
		Visibility:     "cluster",
		Group:          "billing",
		Trivial:        true,
		TrivialName:    "payments",
		TrivialGroupID: "billing",
	})
	bm := func(_ netip.Addr) (string, string, bool) {
		return "/falak/test", "billing", true
	}
	srv := newTestServerWithResolver(t, reg, res, bm)

	resp := queryA(t, srv.port, "payments")
	if len(resp.Answer) != 1 {
		t.Fatalf("trivial answers = %d want 1", len(resp.Answer))
	}
	a := resp.Answer[0].(*dns.A)
	if a.A.String() != "10.42.0.4" {
		t.Fatalf("trivial ip = %s want replica IP", a.A)
	}

	// Flip to non-trivial.
	res.set("/falak/test", "payments", ServiceDNSInfo{
		ServiceID:  "svc-5",
		Visibility: "cluster",
		Group:      "billing",
		ProxyIP:    proxyIP(t),
	})
	resp = queryA(t, srv.port, "payments")
	if len(resp.Answer) != 1 {
		t.Fatalf("non-trivial answers = %d want 1", len(resp.Answer))
	}
	a = resp.Answer[0].(*dns.A)
	if a.A.String() != "169.254.169.250" {
		t.Fatalf("non-trivial ip = %s want proxy IP", a.A)
	}
}

func TestNoopServiceResolverNeverMatches(t *testing.T) {
	t.Parallel()
	r := NoopServiceResolver{}
	if _, ok := r.LookupService("/a", "g", "anything"); ok {
		t.Fatal("NoopServiceResolver must always return ok=false")
	}
}

func TestServiceVisibilityAdmits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		callerGroup string
		info        ServiceDNSInfo
		want        bool
	}{
		{"cluster-cross-group", "checkout", ServiceDNSInfo{Visibility: "cluster", Group: "billing"}, true},
		{"cluster-same-group", "billing", ServiceDNSInfo{Visibility: "cluster", Group: "billing"}, true},
		{"group-same", "billing", ServiceDNSInfo{Visibility: "group", Group: "billing"}, true},
		{"group-cross", "checkout", ServiceDNSInfo{Visibility: "group", Group: "billing"}, false},
		{"group-empty-caller", "", ServiceDNSInfo{Visibility: "group", Group: "billing"}, false},
		{"group-empty-target", "billing", ServiceDNSInfo{Visibility: "group", Group: ""}, false},
		{"external", "billing", ServiceDNSInfo{Visibility: "external", Group: "billing"}, false},
		{"unknown", "billing", ServiceDNSInfo{Visibility: "weird", Group: "billing"}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ServiceVisibilityAdmits(tc.callerGroup, tc.info); got != tc.want {
				t.Fatalf("ServiceVisibilityAdmits(%s, %+v) = %v want %v", tc.callerGroup, tc.info, got, tc.want)
			}
			// ServiceVisibilityPredicate is the BridgeInfo-shaped sibling.
			bi := BridgeInfo{GroupID: tc.callerGroup}
			if got := ServiceVisibilityPredicate(bi, tc.info); got != tc.want {
				t.Fatalf("ServiceVisibilityPredicate(%s,%+v) = %v want %v", tc.callerGroup, tc.info, got, tc.want)
			}
		})
	}
}

// testServer bundles a started Server plus its UDP port for queries.
type testServer struct {
	s    *Server
	port int
}

func newTestServerWithResolver(t *testing.T, reg EndpointRegistry, res ServiceResolver, bm BridgeMapFunc) *testServer {
	t.Helper()
	srv := NewServer(
		WithRegistry(reg),
		WithVisibility(DefaultPredicate),
		WithBridgeMap(bm),
		WithBindAddrs([]netip.Addr{loopback()}),
		WithPort(0),
		WithServiceResolver(res),
	)
	if err := srv.Start(testContext(t)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	addr := srv.UDPListenAddr(loopback())
	if addr == nil {
		t.Fatal("no UDP listen addr")
	}
	return &testServer{s: srv, port: addr.Port}
}

// Compile-time guard: the no-op resolver must satisfy the interface.
var _ ServiceResolver = NoopServiceResolver{}
var _ ServiceResolver = (*inMemServiceResolver)(nil)

// String-form helper to keep error messages cheap.
func (i ServiceDNSInfo) String() string {
	return fmt.Sprintf("Service{id=%s vis=%s group=%s trivial=%v}", i.ServiceID, i.Visibility, i.Group, i.Trivial)
}
