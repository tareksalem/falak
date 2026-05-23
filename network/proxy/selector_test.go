package proxy

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/network/endpoints"
	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
)

// fakeClock is a monotonic-advancing test clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// insertReplica inserts an alive replica with the given identity into
// the registry, using the supplied clock for EmittedAt so the entry
// won't be swept during the test.
func insertReplica(r *endpoints.Registry, clk *fakeClock, capsule, replicaID, bridgeIP string) {
	r.Insert(&endpointpb.EndpointRecord{
		ClusterPath: "c1",
		GroupId:     "g1",
		CapsuleName: capsule,
		ReplicaId:   replicaID,
		BridgeIp:    bridgeIP,
		SwimState:   endpoints.SwimStateAlive,
		NamedPorts: []*endpointpb.NamedPort{
			{Name: "http", ContainerPort: 8080, Protocol: "tcp"},
		},
		EmittedAt:  timestamppb.New(clk.Now()),
		TtlSeconds: 3600,
	})
}

func newTestSelector(t *testing.T, clk *fakeClock, reg *endpoints.Registry, seed int64, opts ...SelectorOption) *Selector {
	t.Helper()
	base := []SelectorOption{
		WithRegistry(reg),
		WithSelectorRand(rand.New(rand.NewSource(seed))),
		WithSelectorNowFunc(clk.Now),
		WithEjectInitial(30 * time.Second),
		WithEjectMax(5 * time.Minute),
		WithBackoffFactor(2),
		WithDialFailureThreshold(3),
		WithDialWindow(30 * time.Second),
	}
	return NewSelector(append(base, opts...)...)
}

func TestSelector_PickReturnsHealthy(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour), endpoints.WithNowFunc(clk.Now))
	defer reg.Stop()

	insertReplica(reg, clk, "api", "r1", "10.0.0.5")
	insertReplica(reg, clk, "api", "r2", "10.0.0.6")

	s := newTestSelector(t, clk, reg, 1)
	for i := 0; i < 50; i++ {
		ep, err := s.Pick("c1", "g1", "api")
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		if ep.ReplicaID != "r1" && ep.ReplicaID != "r2" {
			t.Fatalf("unexpected replica %q", ep.ReplicaID)
		}
	}
}

func TestSelector_NoReplicaWhenEmpty(t *testing.T) {
	clk := newFakeClock(time.Now())
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour), endpoints.WithNowFunc(clk.Now))
	defer reg.Stop()
	s := newTestSelector(t, clk, reg, 1)
	if _, err := s.Pick("c1", "g1", "api"); err != ErrNoHealthyReplica {
		t.Fatalf("expected ErrNoHealthyReplica, got %v", err)
	}
}

func TestSelector_DialFailureDownWeightsThenEjects(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour), endpoints.WithNowFunc(clk.Now))
	defer reg.Stop()
	insertReplica(reg, clk, "api", "r1", "10.0.0.5")
	insertReplica(reg, clk, "api", "r2", "10.0.0.6")
	s := newTestSelector(t, clk, reg, 1)

	// Three consecutive dial failures within the dial window must eject.
	s.RecordDialFailure("api", "r1")
	clk.Advance(5 * time.Second)
	s.RecordDialFailure("api", "r1")
	clk.Advance(5 * time.Second)
	s.RecordDialFailure("api", "r1")

	until := s.EjectedUntil("api", "r1")
	if until.IsZero() {
		t.Fatalf("r1 should be ejected after 3 dial failures")
	}
	// Pick must never return r1 while ejected.
	for i := 0; i < 100; i++ {
		ep, err := s.Pick("c1", "g1", "api")
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		if ep.ReplicaID == "r1" {
			t.Fatalf("ejected r1 was returned")
		}
	}
}

func TestSelector_ForwardFailureEjectsImmediately(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour), endpoints.WithNowFunc(clk.Now))
	defer reg.Stop()
	insertReplica(reg, clk, "api", "r1", "10.0.0.5")
	insertReplica(reg, clk, "api", "r2", "10.0.0.6")
	s := newTestSelector(t, clk, reg, 1)

	s.RecordForwardFailure("api", "r1")
	if s.EjectedUntil("api", "r1").IsZero() {
		t.Fatalf("forward failure must eject immediately")
	}
}

func TestSelector_EjectWindowExpiresReadmits(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour), endpoints.WithNowFunc(clk.Now))
	defer reg.Stop()
	insertReplica(reg, clk, "api", "r1", "10.0.0.5")
	s := newTestSelector(t, clk, reg, 1)

	s.RecordForwardFailure("api", "r1")
	// During the eject window — no replica.
	if _, err := s.Pick("c1", "g1", "api"); err != ErrNoHealthyReplica {
		t.Fatalf("expected ErrNoHealthyReplica during eject, got %v", err)
	}
	// After window — re-admitted.
	clk.Advance(31 * time.Second)
	ep, err := s.Pick("c1", "g1", "api")
	if err != nil {
		t.Fatalf("Pick after eject: %v", err)
	}
	if ep.ReplicaID != "r1" {
		t.Fatalf("expected r1, got %q", ep.ReplicaID)
	}
}

func TestSelector_BackoffDoublesAndCaps(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour), endpoints.WithNowFunc(clk.Now))
	defer reg.Stop()
	insertReplica(reg, clk, "api", "r1", "10.0.0.5")
	s := newTestSelector(t, clk, reg, 1,
		WithEjectInitial(time.Second),
		WithEjectMax(8*time.Second),
		WithBackoffFactor(2),
	)

	expectedDurations := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, want := range expectedDurations {
		s.RecordForwardFailure("api", "r1")
		until := s.EjectedUntil("api", "r1")
		got := until.Sub(clk.Now())
		if got != want {
			t.Fatalf("iteration %d: expected eject duration %v, got %v", i, want, got)
		}
		clk.Advance(want + time.Millisecond)
	}
}

func TestSelector_AllEjectedYieldsErr(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour), endpoints.WithNowFunc(clk.Now))
	defer reg.Stop()
	insertReplica(reg, clk, "api", "r1", "10.0.0.5")
	insertReplica(reg, clk, "api", "r2", "10.0.0.6")
	s := newTestSelector(t, clk, reg, 1)

	s.RecordForwardFailure("api", "r1")
	s.RecordForwardFailure("api", "r2")
	if _, err := s.Pick("c1", "g1", "api"); err != ErrNoHealthyReplica {
		t.Fatalf("expected ErrNoHealthyReplica, got %v", err)
	}
}

func TestSelector_DialFailureWindowResets(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour), endpoints.WithNowFunc(clk.Now))
	defer reg.Stop()
	insertReplica(reg, clk, "api", "r1", "10.0.0.5")
	s := newTestSelector(t, clk, reg, 1, WithDialWindow(10*time.Second))

	s.RecordDialFailure("api", "r1")
	s.RecordDialFailure("api", "r1")
	// Window elapses — counter resets.
	clk.Advance(20 * time.Second)
	s.RecordDialFailure("api", "r1")
	if !s.EjectedUntil("api", "r1").IsZero() {
		t.Fatalf("single failure after window reset must not eject")
	}
}

func TestSelector_RecordSuccessClearsAfterWindow(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour), endpoints.WithNowFunc(clk.Now))
	defer reg.Stop()
	insertReplica(reg, clk, "api", "r1", "10.0.0.5")
	s := newTestSelector(t, clk, reg, 1)

	s.RecordDialFailure("api", "r1")
	s.RecordDialFailure("api", "r1")
	s.RecordSuccess("api", "r1")
	// Counter cleared — a single subsequent failure must not eject.
	s.RecordDialFailure("api", "r1")
	if !s.EjectedUntil("api", "r1").IsZero() {
		t.Fatalf("ejected after success-reset path; should require 3 fresh failures")
	}
}

func TestSelector_ConcurrentRecord(t *testing.T) {
	clk := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	reg := endpoints.NewRegistry(endpoints.WithSweepInterval(time.Hour), endpoints.WithNowFunc(clk.Now))
	defer reg.Stop()
	for i := 0; i < 4; i++ {
		insertReplica(reg, clk, "api", string(rune('a'+i)), "10.0.0.5")
	}
	s := newTestSelector(t, clk, reg, 1)

	var wg sync.WaitGroup
	var picks atomic.Int64
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, err := s.Pick("c1", "g1", "api")
				if err == nil {
					picks.Add(1)
				}
				s.RecordDialFailure("api", "a")
				s.RecordSuccess("api", "a")
			}
		}()
	}
	wg.Wait()
	if picks.Load() == 0 {
		t.Fatalf("no successful picks under concurrent load")
	}
}
