package proxy

import (
	"sync"
	"testing"
)

func TestStats_CountersIncrement(t *testing.T) {
	s := newStats()
	s.ConnectionsOpened.Add(1)
	s.BytesSent.Add(123)
	s.BytesReceived.Add(456)
	s.ConnectErrors.Add(2)
	s.AddPick("a")
	s.AddPick("a")
	s.AddPick("b")

	snap := s.Snapshot()
	if snap.ConnectionsOpened != 1 {
		t.Fatalf("ConnectionsOpened = %d", snap.ConnectionsOpened)
	}
	if snap.BytesSent != 123 || snap.BytesReceived != 456 {
		t.Fatalf("bytes mismatch: %+v", snap)
	}
	if snap.ConnectErrors != 2 {
		t.Fatalf("ConnectErrors = %d", snap.ConnectErrors)
	}
	if snap.PicksPerBackend["a"] != 2 || snap.PicksPerBackend["b"] != 1 {
		t.Fatalf("picks mismatch: %+v", snap.PicksPerBackend)
	}
}

func TestStatsRegistry_EvictionOnDelete(t *testing.T) {
	r := NewStatsRegistry()
	st := r.GetOrCreate("svc-1")
	st.ConnectionsOpened.Add(5)
	if r.Len() != 1 {
		t.Fatalf("Len after GetOrCreate = %d", r.Len())
	}
	r.Delete("svc-1")
	if r.Len() != 0 {
		t.Fatalf("Len after Delete = %d", r.Len())
	}
	if r.Get("svc-1") != nil {
		t.Fatalf("Get after Delete should be nil")
	}
	if r.Snapshot("svc-1") != nil {
		t.Fatalf("Snapshot after Delete should be nil")
	}
}

func TestStatsRegistry_SnapshotConsistency(t *testing.T) {
	r := NewStatsRegistry()
	st := r.GetOrCreate("svc-x")
	st.BytesSent.Add(10)
	st.AddPick("a")

	snap1 := r.Snapshot("svc-x")
	if snap1 == nil {
		t.Fatalf("expected snapshot")
	}
	// Mutate after snapshot; the snapshot must be unchanged.
	st.BytesSent.Add(100)
	st.AddPick("a")
	if snap1.BytesSent != 10 {
		t.Fatalf("snapshot mutated: BytesSent=%d", snap1.BytesSent)
	}
	if snap1.PicksPerBackend["a"] != 1 {
		t.Fatalf("snapshot picks mutated: %d", snap1.PicksPerBackend["a"])
	}
}

func TestStatsRegistry_Reset(t *testing.T) {
	r := NewStatsRegistry()
	st := r.GetOrCreate("svc-r")
	st.BytesSent.Add(99)
	st.AddPick("x")
	r.Reset("svc-r")
	snap := r.Snapshot("svc-r")
	if snap.BytesSent != 0 || len(snap.PicksPerBackend) != 0 {
		t.Fatalf("Reset did not clear stats: %+v", snap)
	}
}

func TestStats_ConcurrentUpdates(t *testing.T) {
	r := NewStatsRegistry()
	st := r.GetOrCreate("svc-c")
	const workers = 16
	const iters = 5000

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				st.ConnectionsOpened.Add(1)
				st.BytesSent.Add(1)
				st.AddPick("a")
			}
		}()
	}
	wg.Wait()
	snap := st.Snapshot()
	want := int64(workers * iters)
	if snap.ConnectionsOpened != want {
		t.Fatalf("ConnectionsOpened = %d want %d", snap.ConnectionsOpened, want)
	}
	if snap.BytesSent != want {
		t.Fatalf("BytesSent = %d want %d", snap.BytesSent, want)
	}
	if snap.PicksPerBackend["a"] != want {
		t.Fatalf("Picks[a] = %d want %d", snap.PicksPerBackend["a"], want)
	}
}

func TestStatsRegistry_GetOrCreateIdempotent(t *testing.T) {
	r := NewStatsRegistry()
	a := r.GetOrCreate("svc")
	b := r.GetOrCreate("svc")
	if a != b {
		t.Fatalf("GetOrCreate returned distinct pointers for same key")
	}
}
