package dns

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/tareksalem/falak/network/endpoints"
)

// fakeRegistry is an in-memory EndpointRegistry the tests drive
// directly. It implements only Lookup — the surface the DNS server
// touches.
type fakeRegistry struct {
	mu      sync.Mutex
	entries map[string][]endpoints.Endpoint
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{entries: map[string][]endpoints.Endpoint{}}
}

func (f *fakeRegistry) set(clusterPath, groupID, capsule string, eps []endpoints.Endpoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[regKey(clusterPath, groupID, capsule)] = eps
}

func (f *fakeRegistry) Lookup(clusterPath, groupID, capsule string) []endpoints.Endpoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := f.entries[regKey(clusterPath, groupID, capsule)]
	out := make([]endpoints.Endpoint, len(src))
	copy(out, src)
	return out
}

func regKey(clusterPath, groupID, capsule string) string {
	return clusterPath + "|" + groupID + "|" + capsule
}

// loopback returns 127.0.0.1 as a netip.Addr for tests.
func loopback() netip.Addr { return netip.MustParseAddr("127.0.0.1") }

// startTestServer wires the registry, bridge map, and predicate
// together and returns the running Server plus the assigned UDP port.
func startTestServer(t *testing.T, reg EndpointRegistry, pred VisibilityPredicate, bridgeMap BridgeMapFunc, randSrc func() *rand.Rand) (*Server, int) {
	t.Helper()
	opts := []Option{
		WithRegistry(reg),
		WithVisibility(pred),
		WithBridgeMap(bridgeMap),
		WithBindAddrs([]netip.Addr{loopback()}),
		WithPort(0),
		WithTTLSeconds(DefaultTTLSeconds),
	}
	if randSrc != nil {
		opts = append(opts, WithRandSource(randSrc))
	}
	srv := NewServer(opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	addr := srv.UDPListenAddr(loopback())
	if addr == nil {
		t.Fatal("UDPListenAddr returned nil")
	}
	return srv, addr.Port
}

// queryA issues an A query against the loopback responder on port.
func queryA(t *testing.T, port int, name string) *dns.Msg {
	t.Helper()
	c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	resp, _, err := c.Exchange(m, fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	return resp
}

func TestServerAnswersAQuery(t *testing.T) {
	reg := newFakeRegistry()
	reg.set("/falak/test", "billing", "api", []endpoints.Endpoint{
		{ClusterPath: "/falak/test", GroupID: "billing", CapsuleName: "api", ReplicaID: "r1", BridgeIP: "10.42.0.10"},
	})
	bm := func(_ netip.Addr) (string, string, bool) {
		return "/falak/test", "billing", true
	}
	_, port := startTestServer(t, reg, DefaultPredicate, bm, nil)

	resp := queryA(t, port, "api")
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d want NOERROR", resp.Rcode)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("len(answer) = %d want 1", len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer not A: %T", resp.Answer[0])
	}
	if a.A.String() != "10.42.0.10" {
		t.Fatalf("ip = %s want 10.42.0.10", a.A)
	}
	if a.Hdr.Ttl != DefaultTTLSeconds {
		t.Fatalf("ttl = %d want %d", a.Hdr.Ttl, DefaultTTLSeconds)
	}
}

func TestServerVisibilityRejection(t *testing.T) {
	reg := newFakeRegistry()
	reg.set("/falak/test", "checkout", "api", []endpoints.Endpoint{
		{ClusterPath: "/falak/test", GroupID: "checkout", CapsuleName: "api", BridgeIP: "10.42.0.20"},
	})
	bm := func(_ netip.Addr) (string, string, bool) {
		return "/falak/test", "billing", true // caller is in billing
	}
	// Visibility predicate would reject because target group differs.
	// In Phase 11A the responder always treats the target group as the
	// caller's own group; with billing→billing the registry has no
	// entry, so we get NXDOMAIN. To exercise the predicate path we use
	// a custom rejector that always returns false.
	reject := func(_ BridgeInfo, _ string) bool { return false }
	_, port := startTestServer(t, reg, reject, bm, nil)

	resp := queryA(t, port, "api")
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d want NXDOMAIN", resp.Rcode)
	}
}

func TestServerUnknownName(t *testing.T) {
	reg := newFakeRegistry()
	bm := func(_ netip.Addr) (string, string, bool) {
		return "/falak/test", "billing", true
	}
	_, port := startTestServer(t, reg, DefaultPredicate, bm, nil)

	resp := queryA(t, port, "missing")
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d want NXDOMAIN", resp.Rcode)
	}
	if len(resp.Answer) != 0 {
		t.Fatalf("expected no answers, got %d", len(resp.Answer))
	}
}

func TestServerMultipleReplicasRandomized(t *testing.T) {
	reg := newFakeRegistry()
	reg.set("/falak/test", "billing", "api", []endpoints.Endpoint{
		{GroupID: "billing", CapsuleName: "api", BridgeIP: "10.42.0.1"},
		{GroupID: "billing", CapsuleName: "api", BridgeIP: "10.42.0.2"},
		{GroupID: "billing", CapsuleName: "api", BridgeIP: "10.42.0.3"},
	})
	bm := func(_ netip.Addr) (string, string, bool) {
		return "/falak/test", "billing", true
	}
	// Force a deterministic but non-identity shuffle.
	randSrc := func() *rand.Rand { return rand.New(rand.NewSource(99)) }
	_, port := startTestServer(t, reg, DefaultPredicate, bm, randSrc)

	resp := queryA(t, port, "api")
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d want NOERROR", resp.Rcode)
	}
	if len(resp.Answer) != 3 {
		t.Fatalf("len(answer) = %d want 3", len(resp.Answer))
	}
	got := []string{}
	for _, rr := range resp.Answer {
		a, ok := rr.(*dns.A)
		if !ok {
			t.Fatalf("answer not A: %T", rr)
		}
		got = append(got, a.A.String())
	}
	// All three IPs present.
	want := map[string]bool{"10.42.0.1": false, "10.42.0.2": false, "10.42.0.3": false}
	for _, ip := range got {
		if _, ok := want[ip]; !ok {
			t.Fatalf("unexpected ip %s in answer set", ip)
		}
		want[ip] = true
	}
	for ip, seen := range want {
		if !seen {
			t.Fatalf("missing ip %s", ip)
		}
	}
}

func TestServerUnknownCallerBridge(t *testing.T) {
	reg := newFakeRegistry()
	reg.set("/falak/test", "billing", "api", []endpoints.Endpoint{
		{GroupID: "billing", CapsuleName: "api", BridgeIP: "10.42.0.1"},
	})
	bm := func(_ netip.Addr) (string, string, bool) {
		return "", "", false
	}
	_, port := startTestServer(t, reg, DefaultPredicate, bm, nil)

	resp := queryA(t, port, "api")
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d want NXDOMAIN", resp.Rcode)
	}
}

func TestServerAAAANoError(t *testing.T) {
	reg := newFakeRegistry()
	reg.set("/falak/test", "billing", "api", []endpoints.Endpoint{
		{GroupID: "billing", CapsuleName: "api", BridgeIP: "10.42.0.1"},
	})
	bm := func(_ netip.Addr) (string, string, bool) {
		return "/falak/test", "billing", true
	}
	_, port := startTestServer(t, reg, DefaultPredicate, bm, nil)

	c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn("api"), dns.TypeAAAA)
	resp, _, err := c.Exchange(m, fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d want NOERROR for AAAA", resp.Rcode)
	}
	if len(resp.Answer) != 0 {
		t.Fatalf("AAAA should have no answers, got %d", len(resp.Answer))
	}
}

func TestServerStartTwiceFails(t *testing.T) {
	reg := newFakeRegistry()
	srv := NewServer(
		WithRegistry(reg),
		WithBindAddrs([]netip.Addr{loopback()}),
		WithPort(0),
	)
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	if err := srv.Start(ctx); err != ErrAlreadyStarted {
		t.Fatalf("second Start = %v, want ErrAlreadyStarted", err)
	}
}

func TestServerAddRemoveListenerIdempotent(t *testing.T) {
	reg := newFakeRegistry()
	srv := NewServer(
		WithRegistry(reg),
		WithBindAddrs([]netip.Addr{loopback()}),
		WithPort(0),
	)
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	// Re-adding the existing addr is a no-op.
	if err := srv.AddListener(ctx, loopback()); err != nil {
		t.Fatalf("AddListener idempotent: %v", err)
	}
	addrs := srv.ListenerAddrs()
	if len(addrs) != 1 {
		t.Fatalf("listener count = %d want 1 after idempotent add", len(addrs))
	}

	// Remove + re-remove.
	if err := srv.RemoveListener(loopback()); err != nil {
		t.Fatalf("RemoveListener: %v", err)
	}
	if err := srv.RemoveListener(loopback()); err != nil {
		t.Fatalf("RemoveListener idempotent: %v", err)
	}
	if got := len(srv.ListenerAddrs()); got != 0 {
		t.Fatalf("listener count = %d want 0 after remove", got)
	}
}

func TestServerStopBeforeStart(t *testing.T) {
	srv := NewServer()
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
}

func TestServerAddListenerBeforeStart(t *testing.T) {
	srv := NewServer()
	if err := srv.AddListener(context.Background(), loopback()); err != ErrNotStarted {
		t.Fatalf("AddListener before Start = %v, want ErrNotStarted", err)
	}
	if err := srv.RemoveListener(loopback()); err != ErrNotStarted {
		t.Fatalf("RemoveListener before Start = %v, want ErrNotStarted", err)
	}
}

func TestServerStopCloseSocket(t *testing.T) {
	reg := newFakeRegistry()
	srv := NewServer(
		WithRegistry(reg),
		WithBindAddrs([]netip.Addr{loopback()}),
		WithPort(0),
	)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := srv.UDPListenAddr(loopback())
	if addr == nil {
		t.Fatal("no UDP addr")
	}
	port := addr.Port
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// After Stop, binding to the same port should succeed without
	// EADDRINUSE.
	conn, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("post-Stop bind failed: %v", err)
	}
	_ = conn.Close()
}

func TestParseQName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		capsule   string
		port      string
		ok        bool
	}{
		{"api.", "api", "", true},
		{"API.", "api", "", true},
		{"api.http.", "api", "http", true},
		{"a.b.c.", "", "", false},
		{".", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		cap, port, ok := parseQName(tc.in)
		if ok != tc.ok || cap != tc.capsule || port != tc.port {
			t.Fatalf("parseQName(%q) = (%q,%q,%v) want (%q,%q,%v)",
				tc.in, cap, port, ok, tc.capsule, tc.port, tc.ok)
		}
	}
}

// TestServerListenerAddrsSorted is a small sanity check that
// ListenerAddrs returns the expected count after multiple adds. Uses
// two loopback aliases.
func TestServerMultipleListeners(t *testing.T) {
	// 127.0.0.2 may not be usable on all platforms, but Linux's
	// 127.0.0.0/8 covers it. We skip cleanly if bind fails.
	a1 := netip.MustParseAddr("127.0.0.1")
	a2 := netip.MustParseAddr("127.0.0.2")

	probe, err := net.ListenPacket("udp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 not available: %v", err)
	}
	_ = probe.Close()

	srv := NewServer(
		WithRegistry(newFakeRegistry()),
		WithBindAddrs([]netip.Addr{a1}),
		WithPort(0),
	)
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	if err := srv.AddListener(ctx, a2); err != nil {
		t.Fatalf("AddListener: %v", err)
	}
	addrs := srv.ListenerAddrs()
	if len(addrs) != 2 {
		t.Fatalf("listener count = %d want 2", len(addrs))
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].String() < addrs[j].String() })
	if addrs[0] != a1 || addrs[1] != a2 {
		t.Fatalf("addrs = %v want %v,%v", addrs, a1, a2)
	}
}
