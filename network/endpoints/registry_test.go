package endpoints

import (
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
)

// pbRecord builds a wire EndpointRecord for tests.
func pbRecord(cluster, group, capsule, replica, state string, emit time.Time, ttl time.Duration) *endpointpb.EndpointRecord {
	return &endpointpb.EndpointRecord{
		ClusterPath: cluster,
		GroupId:     group,
		CapsuleName: capsule,
		ReplicaId:   replica,
		NodeId:      "node-" + replica,
		BridgeIp:    "10.0.0." + replica[len(replica)-1:],
		SwimState:   state,
		NamedPorts: []*endpointpb.NamedPort{
			{Name: "http", ContainerPort: 8080, Protocol: "tcp"},
			{Name: "grpc", ContainerPort: 9090, Protocol: "tcp"},
		},
		EmittedAt:  timestamppb.New(emit),
		TtlSeconds: int32(ttl.Seconds()),
	}
}

func TestRegistry_InsertAndLookup(t *testing.T) {
	r := NewRegistry(WithSweepInterval(time.Hour))
	defer r.Stop()

	r.Insert(pbRecord("c1", "g1", "api", "r1", SwimStateAlive, time.Now(), 30*time.Second))
	r.Insert(pbRecord("c1", "g1", "api", "r2", SwimStateAlive, time.Now(), 30*time.Second))
	r.Insert(pbRecord("c1", "g1", "worker", "r3", SwimStateAlive, time.Now(), 30*time.Second))

	got := r.Lookup("c1", "g1", "api")
	if len(got) != 2 {
		t.Fatalf("expected 2 api endpoints, got %d", len(got))
	}
	if r.Len() != 3 {
		t.Errorf("registry len = %d, want 3", r.Len())
	}
}

func TestRegistry_LookupFiltersSwimState(t *testing.T) {
	r := NewRegistry(WithSweepInterval(time.Hour))
	defer r.Stop()

	r.Insert(pbRecord("c1", "g1", "api", "r1", SwimStateAlive, time.Now(), 30*time.Second))
	r.Insert(pbRecord("c1", "g1", "api", "r2", "suspect", time.Now(), 30*time.Second))
	r.Insert(pbRecord("c1", "g1", "api", "r3", "dead", time.Now(), 30*time.Second))

	alive := r.Lookup("c1", "g1", "api")
	if len(alive) != 1 {
		t.Errorf("alive lookup returned %d, want 1", len(alive))
	}
	all := r.LookupAll("c1", "g1", "api")
	if len(all) != 3 {
		t.Errorf("all lookup returned %d, want 3", len(all))
	}
}

func TestRegistry_LookupOrderedByEmittedAtDesc(t *testing.T) {
	r := NewRegistry(WithSweepInterval(time.Hour))
	defer r.Stop()

	now := time.Now()
	r.Insert(pbRecord("c1", "g1", "api", "r1", SwimStateAlive, now.Add(-time.Minute), 30*time.Second))
	r.Insert(pbRecord("c1", "g1", "api", "r2", SwimStateAlive, now, 30*time.Second))
	r.Insert(pbRecord("c1", "g1", "api", "r3", SwimStateAlive, now.Add(-30*time.Second), 30*time.Second))

	got := r.Lookup("c1", "g1", "api")
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d", len(got))
	}
	if got[0].ReplicaID != "r2" || got[1].ReplicaID != "r3" || got[2].ReplicaID != "r1" {
		t.Errorf("unexpected order: %v / %v / %v", got[0].ReplicaID, got[1].ReplicaID, got[2].ReplicaID)
	}
}

func TestRegistry_WithdrawIdempotent(t *testing.T) {
	r := NewRegistry(WithSweepInterval(time.Hour))
	defer r.Stop()

	r.Insert(pbRecord("c1", "g1", "api", "r1", SwimStateAlive, time.Now(), 30*time.Second))
	w := &endpointpb.EndpointWithdrawal{
		ClusterPath: "c1", GroupId: "g1", CapsuleName: "api", ReplicaId: "r1",
	}
	r.Withdraw(w)
	if got := len(r.Lookup("c1", "g1", "api")); got != 0 {
		t.Errorf("after withdraw expected 0, got %d", got)
	}
	// Second withdraw should be a no-op.
	r.Withdraw(w)
	if got := r.Len(); got != 0 {
		t.Errorf("len after double withdraw = %d", got)
	}

	r.WithdrawKey("c1", "g1", "api", "ghost") // unknown — must not panic
}

func TestRegistry_TTLEvictsStaleEntries(t *testing.T) {
	// Use a fake clock so we can fast-forward.
	mu := &sync.Mutex{}
	current := time.Unix(1_700_000_000, 0)
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	r := NewRegistry(
		WithSweepInterval(10*time.Millisecond),
		WithNowFunc(nowFn),
	)
	defer r.Stop()

	// TTL=1s — eviction window is 2s.
	r.Insert(pbRecord("c1", "g1", "api", "r1", SwimStateAlive, current, time.Second))

	// At t+1.5s nothing should be evicted yet.
	mu.Lock()
	current = current.Add(1500 * time.Millisecond)
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	if r.Len() != 1 {
		t.Errorf("premature eviction: len=%d", r.Len())
	}

	// At t+3s the entry is past 2 × TTL → evicted.
	mu.Lock()
	current = current.Add(1500 * time.Millisecond)
	mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r.Len() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected eviction, registry still has %d entries", r.Len())
}

func TestRegistry_LookupNamedPort(t *testing.T) {
	r := NewRegistry(WithSweepInterval(time.Hour))
	defer r.Stop()

	r.Insert(pbRecord("c1", "g1", "api", "r1", SwimStateAlive, time.Now(), 30*time.Second))
	r.Insert(pbRecord("c1", "g1", "api", "r2", SwimStateAlive, time.Now(), 30*time.Second))
	// One dead replica — should not show up.
	r.Insert(pbRecord("c1", "g1", "api", "r3", "dead", time.Now(), 30*time.Second))

	got := r.LookupNamedPort("c1", "g1", "api", "http")
	if len(got) != 2 {
		t.Fatalf("expected 2 http addresses, got %d (%+v)", len(got), got)
	}
	for _, addr := range got {
		if addr.Port != 8080 || addr.Protocol != "tcp" {
			t.Errorf("unexpected addr: %+v", addr)
		}
	}

	grpc := r.LookupNamedPort("c1", "g1", "api", "grpc")
	if len(grpc) != 2 {
		t.Errorf("expected 2 grpc addresses, got %d", len(grpc))
	}

	none := r.LookupNamedPort("c1", "g1", "api", "ws")
	if len(none) != 0 {
		t.Errorf("unknown port returned %d entries", len(none))
	}
}

func TestRegistry_InsertOverwritesByKey(t *testing.T) {
	r := NewRegistry(WithSweepInterval(time.Hour))
	defer r.Stop()

	rec := pbRecord("c1", "g1", "api", "r1", SwimStateAlive, time.Now(), 30*time.Second)
	r.Insert(rec)
	rec.BridgeIp = "10.99.99.99"
	rec.EmittedAt = timestamppb.New(time.Now().Add(time.Second))
	r.Insert(rec)

	got := r.Lookup("c1", "g1", "api")
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	if got[0].BridgeIP != "10.99.99.99" {
		t.Errorf("expected overwrite, got %+v", got[0])
	}
}

func TestRegistry_Snapshot(t *testing.T) {
	r := NewRegistry(WithSweepInterval(time.Hour))
	defer r.Stop()

	r.Insert(pbRecord("c1", "g1", "api", "r1", SwimStateAlive, time.Now(), 30*time.Second))
	r.Insert(pbRecord("c1", "g1", "api", "r2", "dead", time.Now(), 30*time.Second))
	if len(r.Snapshot()) != 2 {
		t.Errorf("snapshot len = %d, want 2", len(r.Snapshot()))
	}
}

func TestRegistry_StopIdempotent(t *testing.T) {
	r := NewRegistry(WithSweepInterval(10 * time.Millisecond))
	r.Stop()
	r.Stop() // second call must not panic or hang
}

func TestRegistry_InsertNilSafe(t *testing.T) {
	r := NewRegistry(WithSweepInterval(time.Hour))
	defer r.Stop()
	r.Insert(nil)
	r.Withdraw(nil)
	if r.Len() != 0 {
		t.Errorf("nil insert/withdraw should be no-op")
	}
}
