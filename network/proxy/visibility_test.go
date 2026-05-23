package proxy

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// staticSourceResolver returns a fixed (clusterPath, groupID) for any
// source IP in its map. Missing IPs return ok=false (spoofed source).
type staticSourceResolver struct {
	mu      sync.Mutex
	local   string
	entries map[netip.Addr]struct {
		clusterPath string
		groupID     string
	}
}

func newStaticSourceResolver() *staticSourceResolver {
	return &staticSourceResolver{
		entries: make(map[netip.Addr]struct {
			clusterPath string
			groupID     string
		}),
	}
}

// withLocalCluster returns the receiver after recording the local
// cluster path used by the cross-cluster boundary check. Lets tests
// opt into the check explicitly without breaking pre-boundary tests
// that omit it.
func (s *staticSourceResolver) withLocalCluster(local string) *staticSourceResolver {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.local = local
	return s
}

func (s *staticSourceResolver) set(ip netip.Addr, clusterPath, groupID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[ip] = struct {
		clusterPath string
		groupID     string
	}{clusterPath, groupID}
}

func (s *staticSourceResolver) ResolveGroup(ip netip.Addr) (string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.entries[ip]
	if !ok {
		return "", "", false
	}
	return v.clusterPath, v.groupID, true
}

func (s *staticSourceResolver) LocalCluster() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.local
}

// fakeConnWithRemote is a net.Conn with a tunable RemoteAddr; all
// other methods are minimally satisfied so the test can drive
// VerifyTCPSource without spinning up a real network.
type fakeConnWithRemote struct {
	remote net.Addr
}

func (f *fakeConnWithRemote) Read(_ []byte) (int, error)         { return 0, errors.New("not implemented") }
func (f *fakeConnWithRemote) Write(_ []byte) (int, error)        { return 0, errors.New("not implemented") }
func (f *fakeConnWithRemote) Close() error                       { return nil }
func (f *fakeConnWithRemote) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (f *fakeConnWithRemote) RemoteAddr() net.Addr               { return f.remote }
func (f *fakeConnWithRemote) SetDeadline(_ time.Time) error      { return nil }
func (f *fakeConnWithRemote) SetReadDeadline(_ time.Time) error  { return nil }
func (f *fakeConnWithRemote) SetWriteDeadline(_ time.Time) error { return nil }

func mkTCPRemote(t *testing.T, ipStr string) net.Addr {
	t.Helper()
	return &net.TCPAddr{IP: net.ParseIP(ipStr), Port: 4242}
}

func TestVisibilityClusterScopeAdmitsAnySource(t *testing.T) {
	t.Parallel()
	src := newStaticSourceResolver()
	src.set(netip.MustParseAddr("10.42.0.1"), "/falak/test", "checkout")
	vis := ServiceVisibility{Scope: "cluster", Group: "billing"}
	conn := &fakeConnWithRemote{remote: mkTCPRemote(t, "10.42.0.1")}

	if err := VerifyTCPSource(conn, src, vis); err != nil {
		t.Fatalf("cluster admit expected, got %v", err)
	}
}

func TestVisibilityGroupSameGroupAdmits(t *testing.T) {
	t.Parallel()
	src := newStaticSourceResolver()
	src.set(netip.MustParseAddr("10.42.0.2"), "/falak/test", "billing")
	vis := ServiceVisibility{Scope: "group", Group: "billing"}
	conn := &fakeConnWithRemote{remote: mkTCPRemote(t, "10.42.0.2")}

	if err := VerifyTCPSource(conn, src, vis); err != nil {
		t.Fatalf("same-group admit expected, got %v", err)
	}
}

func TestVisibilityGroupCrossGroupRejects(t *testing.T) {
	t.Parallel()
	src := newStaticSourceResolver()
	src.set(netip.MustParseAddr("10.42.0.3"), "/falak/test", "checkout")
	vis := ServiceVisibility{Scope: "group", Group: "billing"}
	conn := &fakeConnWithRemote{remote: mkTCPRemote(t, "10.42.0.3")}

	err := VerifyTCPSource(conn, src, vis)
	if !errors.Is(err, ErrVisibilityDenied) {
		t.Fatalf("expected ErrVisibilityDenied, got %v", err)
	}
}

func TestVisibilitySpoofedSourceRejected(t *testing.T) {
	t.Parallel()
	src := newStaticSourceResolver()
	// Note: 10.42.0.99 deliberately NOT registered.
	vis := ServiceVisibility{Scope: "cluster", Group: "billing"}
	conn := &fakeConnWithRemote{remote: mkTCPRemote(t, "10.42.0.99")}

	err := VerifyTCPSource(conn, src, vis)
	if !errors.Is(err, ErrVisibilityDenied) {
		t.Fatalf("expected ErrVisibilityDenied for spoofed source, got %v", err)
	}
}

func TestVisibilityNilConnRejected(t *testing.T) {
	t.Parallel()
	src := newStaticSourceResolver()
	vis := ServiceVisibility{Scope: "cluster"}
	if err := VerifyTCPSource(nil, src, vis); !errors.Is(err, ErrVisibilityDenied) {
		t.Fatalf("nil conn must be rejected, got %v", err)
	}
}

func TestVisibilityUDPSourceCheck(t *testing.T) {
	t.Parallel()
	src := newStaticSourceResolver()
	src.set(netip.MustParseAddr("10.42.0.4"), "/falak/test", "billing")
	vis := ServiceVisibility{Scope: "group", Group: "billing"}

	good := netip.MustParseAddr("10.42.0.4")
	if err := VerifySource(good, src, vis); err != nil {
		t.Fatalf("udp same-group admit expected: %v", err)
	}
	bad := netip.MustParseAddr("10.42.0.5") // unregistered
	if err := VerifySource(bad, src, vis); !errors.Is(err, ErrVisibilityDenied) {
		t.Fatalf("udp spoofed source must reject: %v", err)
	}
	invalid := netip.Addr{}
	if err := VerifySource(invalid, src, vis); !errors.Is(err, ErrVisibilityDenied) {
		t.Fatalf("udp invalid IP must reject: %v", err)
	}
}

func TestVisibilityScopeExternalRejected(t *testing.T) {
	t.Parallel()
	v := ServiceVisibility{Scope: "external", Group: "x"}
	if v.Admits("x") {
		t.Fatal("external scope must reject (deferred per decision #17)")
	}
}

// TestVisibilityCrossClusterRejected exercises the cluster-boundary
// assertion added per audit minor #5. A bridge that resolves to a
// foreign cluster MUST be rejected even when the visibility scope is
// "cluster" (otherwise a stray packet from a peer cluster's data plane
// could be admitted by an in-cluster service that intended to admit
// "anyone in this cluster").
func TestVisibilityCrossClusterRejected(t *testing.T) {
	t.Parallel()
	src := newStaticSourceResolver().withLocalCluster("/falak/local")
	src.set(netip.MustParseAddr("10.42.0.50"), "/falak/foreign", "billing")
	vis := ServiceVisibility{Scope: "cluster"}
	conn := &fakeConnWithRemote{remote: mkTCPRemote(t, "10.42.0.50")}

	err := VerifyTCPSource(conn, src, vis)
	if !errors.Is(err, ErrVisibilityDenied) {
		t.Fatalf("expected ErrVisibilityDenied (cross-cluster), got %v", err)
	}
	if !errors.Is(err, ErrCrossClusterDenied) {
		t.Fatalf("expected ErrCrossClusterDenied specifically, got %v", err)
	}
}

// TestVisibilityLocalClusterAdmitted confirms that the boundary check
// does NOT reject the happy path: a source whose bridge belongs to the
// local cluster is admitted when the visibility scope allows it.
func TestVisibilityLocalClusterAdmitted(t *testing.T) {
	t.Parallel()
	src := newStaticSourceResolver().withLocalCluster("/falak/local")
	src.set(netip.MustParseAddr("10.42.0.51"), "/falak/local", "billing")
	vis := ServiceVisibility{Scope: "cluster"}
	conn := &fakeConnWithRemote{remote: mkTCPRemote(t, "10.42.0.51")}

	if err := VerifyTCPSource(conn, src, vis); err != nil {
		t.Fatalf("local-cluster admit expected, got %v", err)
	}
}

func TestVisibilityStatsCounter(t *testing.T) {
	t.Parallel()
	s := NewVisibilityStats()
	if s.Denied() != 0 {
		t.Fatalf("initial Denied = %d want 0", s.Denied())
	}
	s.IncDenied()
	s.IncDenied()
	if s.Denied() != 2 {
		t.Fatalf("Denied = %d want 2", s.Denied())
	}
}
