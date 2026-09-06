package capsule

import (
	"context"
	"testing"
)

// TestRecordReplicaNetwork covers the O15 observability path: after a replica
// is assigned, recording its resolved IP + host-port bindings updates that
// replica's state, survives a Get snapshot, and is idempotent for unknown
// capsules/replicas.
func TestRecordReplicaNetwork(t *testing.T) {
	t.Parallel()

	const (
		cluster   = "test/dc1/cluster"
		replicaID = ReplicaID("0")
		nodeID    = "node-a"
	)

	newAssigned := func(t *testing.T) (*Manager, CapsuleID) {
		t.Helper()
		mgr := NewManager()
		c, err := mgr.Create(context.Background(), cluster, CapsuleSpec{
			Name:  "web",
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

	t.Run("records ip and ports on the replica", func(t *testing.T) {
		t.Parallel()
		mgr, id := newAssigned(t)

		bindings := []PortBinding{
			{Name: "http", ContainerPort: 80, HostPort: 32768},
			{Name: "metrics", ContainerPort: 9090, HostPort: 0},
		}
		if err := mgr.RecordReplicaNetwork(id, replicaID, "10.0.0.5", bindings); err != nil {
			t.Fatalf("RecordReplicaNetwork failed: %v", err)
		}

		got := mgr.Get(id)
		if len(got.Replicas) != 1 {
			t.Fatalf("expected 1 replica, got %d", len(got.Replicas))
		}
		r := got.Replicas[0]
		if r.IP != "10.0.0.5" {
			t.Errorf("IP = %q, want 10.0.0.5", r.IP)
		}
		if len(r.Ports) != 2 || r.Ports[0].HostPort != 32768 || r.Ports[1].HostPort != 0 {
			t.Fatalf("ports = %+v, want http:32768 + metrics:0", r.Ports)
		}

		// The recorded slice must be a copy — mutating the caller's slice
		// afterwards must not change stored state.
		bindings[0].HostPort = 1
		if again := mgr.Get(id); again.Replicas[0].Ports[0].HostPort != 32768 {
			t.Errorf("stored port mutated via caller slice: %d", again.Replicas[0].Ports[0].HostPort)
		}
	})

	t.Run("idempotent for unknown capsule and replica", func(t *testing.T) {
		t.Parallel()
		mgr, id := newAssigned(t)

		if err := mgr.RecordReplicaNetwork(CapsuleID("nope"), replicaID, "1.2.3.4", nil); err != nil {
			t.Errorf("unknown capsule should be a no-op, got %v", err)
		}
		if err := mgr.RecordReplicaNetwork(id, ReplicaID("99"), "1.2.3.4", nil); err != nil {
			t.Errorf("unknown replica should be a no-op, got %v", err)
		}
		// The known replica must be untouched.
		if got := mgr.Get(id); got.Replicas[0].IP != "" {
			t.Errorf("unrelated replica IP should stay empty, got %q", got.Replicas[0].IP)
		}
	})
}
