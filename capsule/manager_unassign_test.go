package capsule

import (
	"context"
	"path/filepath"
	"testing"

	enums "github.com/tareksalem/falak/capsule/enums"
)

// TestUnassignReplica covers the crash/removal re-placement path: assigning a
// replica then unassigning it clears the node binding and resets the replica
// status to Announced, the operation is idempotent across the unknown-capsule,
// unknown-replica and already-unbound cases, and the cleared binding survives
// a store reload.
func TestUnassignReplica(t *testing.T) {
	t.Parallel()

	const (
		cluster   = "test/dc1/cluster"
		capsName  = "crash-app"
		replicaID = ReplicaID("0")
		nodeID    = "node-a"
	)

	newAssigned := func(t *testing.T) (*Manager, CapsuleID) {
		t.Helper()
		mgr := NewManager()
		c, err := mgr.Create(context.Background(), cluster, CapsuleSpec{
			Name:  capsName,
			Image: "img:v1",
			Orbit: "api",
		})
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}
		if err := mgr.AssignReplica(c.ID, replicaID, nodeID); err != nil {
			t.Fatalf("AssignReplica failed: %v", err)
		}
		return mgr, c.ID
	}

	t.Run("clears binding and resets status", func(t *testing.T) {
		t.Parallel()
		mgr, id := newAssigned(t)

		// Sanity: the replica is bound before unassign.
		before := mgr.Get(id)
		if len(before.Replicas) != 1 || before.Replicas[0].NodeID != nodeID {
			t.Fatalf("precondition: expected replica bound to %q, got %+v", nodeID, before.Replicas)
		}

		if err := mgr.UnassignReplica(id, replicaID); err != nil {
			t.Fatalf("UnassignReplica failed: %v", err)
		}

		after := mgr.Get(id)
		if len(after.Replicas) != 1 {
			t.Fatalf("replica slot should be preserved, got %d replicas", len(after.Replicas))
		}
		if after.Replicas[0].ReplicaID != replicaID {
			t.Errorf("ReplicaID should be stable: got %q, want %q", after.Replicas[0].ReplicaID, replicaID)
		}
		if after.Replicas[0].NodeID != "" {
			t.Errorf("NodeID should be cleared, got %q", after.Replicas[0].NodeID)
		}
		if after.Replicas[0].Status != enums.CapsuleStatusEnum.Announced() {
			t.Errorf("Status should reset to Announced, got %q", after.Replicas[0].Status)
		}
	})

	t.Run("idempotent on unknown capsule", func(t *testing.T) {
		t.Parallel()
		mgr := NewManager()
		if err := mgr.UnassignReplica(NewCapsuleID(), replicaID); err != nil {
			t.Errorf("UnassignReplica on unknown capsule should be a no-op, got %v", err)
		}
	})

	t.Run("idempotent on unknown replica", func(t *testing.T) {
		t.Parallel()
		mgr, id := newAssigned(t)
		if err := mgr.UnassignReplica(id, ReplicaID("does-not-exist")); err != nil {
			t.Errorf("UnassignReplica on unknown replica should be a no-op, got %v", err)
		}
		// The real replica must remain untouched.
		c := mgr.Get(id)
		if c.Replicas[0].NodeID != nodeID {
			t.Errorf("unrelated replica should remain bound, got NodeID %q", c.Replicas[0].NodeID)
		}
	})

	t.Run("idempotent on double call", func(t *testing.T) {
		t.Parallel()
		mgr, id := newAssigned(t)
		if err := mgr.UnassignReplica(id, replicaID); err != nil {
			t.Fatalf("first UnassignReplica failed: %v", err)
		}
		if err := mgr.UnassignReplica(id, replicaID); err != nil {
			t.Errorf("second UnassignReplica should be a no-op, got %v", err)
		}
		c := mgr.Get(id)
		if c.Replicas[0].NodeID != "" {
			t.Errorf("NodeID should remain cleared, got %q", c.Replicas[0].NodeID)
		}
	})

	t.Run("persists across store reload", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "capsules.db")

		store1, err := OpenStore(dbPath)
		if err != nil {
			t.Fatalf("OpenStore failed: %v", err)
		}
		mgr1 := NewManager(WithManagerStore(store1))

		c, err := mgr1.Create(context.Background(), cluster, CapsuleSpec{
			Name:  capsName,
			Image: "img:v1",
			Orbit: "api",
		})
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}
		if err := mgr1.AssignReplica(c.ID, replicaID, nodeID); err != nil {
			t.Fatalf("AssignReplica failed: %v", err)
		}
		if err := mgr1.UnassignReplica(c.ID, replicaID); err != nil {
			t.Fatalf("UnassignReplica failed: %v", err)
		}
		if err := store1.Close(); err != nil {
			t.Fatalf("store close failed: %v", err)
		}

		store2, err := OpenStore(dbPath)
		if err != nil {
			t.Fatalf("OpenStore (reopen) failed: %v", err)
		}
		defer store2.Close()
		mgr2 := NewManager(WithManagerStore(store2))

		reloaded := mgr2.Get(c.ID)
		if reloaded == nil {
			t.Fatal("capsule missing after reopen")
		}
		if len(reloaded.Replicas) != 1 {
			t.Fatalf("expected 1 replica after reopen, got %d", len(reloaded.Replicas))
		}
		if reloaded.Replicas[0].NodeID != "" {
			t.Errorf("cleared binding should persist, got NodeID %q", reloaded.Replicas[0].NodeID)
		}
	})
}
