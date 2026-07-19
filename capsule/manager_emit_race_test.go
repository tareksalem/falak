package capsule

import (
	"context"
	"fmt"
	"sync"
	"testing"

	enums "github.com/tareksalem/falak/capsule/enums"
)

// TestManager_EmitCarriesRaceSafeSnapshot proves the event emitters hand
// subscribers a race-safe snapshot, not the live store pointer. A handler
// reads the mutable Replicas slice off every event while AssignReplica
// concurrently mutates that same slice under m.mu. Before the fix (emit
// carried the live *Capsule) this was a data race the -race detector caught;
// with emit snapshotting under m.mu.RLock, the handler reads an immutable
// copy. Run under `go test -race`.
func TestManager_EmitCarriesRaceSafeSnapshot(t *testing.T) {
	mgr := NewManager()
	c, err := mgr.Create(context.Background(), "test/dc1/cluster", CapsuleSpec{
		Name: "emit-race", Image: "img", Orbit: "api",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The lockless read that used to race with AssignReplica.
	mgr.SetEventHandler(func(ev ManagerEvent) {
		if ev.Capsule == nil {
			return
		}
		for _, r := range ev.Capsule.Replicas {
			_ = r.NodeID
		}
	})

	const iters = 3000
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: mutates Replicas under m.mu.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = mgr.AssignReplica(c.ID, ReplicaID(fmt.Sprintf("r%d", i%4)), fmt.Sprintf("node%d", i))
		}
	}()

	// Emitter: SyncStatus emits, and emit snapshots + invokes the handler.
	go func() {
		defer wg.Done()
		statuses := []enums.CapsuleStatus{
			enums.CapsuleStatusEnum.Announced(),
			enums.CapsuleStatusEnum.Electing(),
		}
		for i := 0; i < iters; i++ {
			_ = mgr.SyncStatus(c.ID, statuses[i%len(statuses)])
		}
	}()

	wg.Wait()
}
