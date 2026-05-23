package node

import (
	"context"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/enums"
	"github.com/tareksalem/falak/capsule/scaling"
	"github.com/tareksalem/falak/node/internal/events"
)

// newReelectionHandler constructs a CapsuleHandler with the minimum
// dependencies needed to exercise the election-request paths. It owns a
// fresh in-memory capsule.Manager and an unbuffered event bus.
func newReelectionHandler(t *testing.T) (*CapsuleHandler, events.Bus) {
	t.Helper()
	bus := events.NewBus()
	h := NewCapsuleHandler(
		capsule.NewManager(),
		WithCapsuleHandlerEventBus(bus),
		WithCapsuleHandlerNodeID("local-node"),
	)
	// We intentionally do NOT call h.Start — it would also spin up the
	// scaling monitor and orbit loops which these focused tests don't
	// need. onNodeFailed and onScaleEvent are both safe to invoke
	// directly against a not-started handler.
	return h, bus
}

// seedCapsuleWithReplica inserts a capsule into the handler's manager
// with a single already-assigned replica on the given node ID. Used by
// the re-election tests to simulate a state where the mesh had
// previously placed the replica before failure or scale trigger.
//
// Create() on the capsule manager auto-announces, which walks through
// the handler's onManagerEvent bridge and fires an initial
// ElectionRequested. Tests that assert on election events must subscribe
// AFTER calling this helper so the auto-fired initial request does not
// pollute the channel.
func seedCapsuleWithReplica(t *testing.T, h *CapsuleHandler, cluster, name, replicaID, nodeID string) *capsule.Capsule {
	t.Helper()
	ctx := context.Background()
	created, err := h.manager.Create(ctx, cluster, capsule.CapsuleSpec{
		Name:  name,
		Image: "img:v1",
		Orbit: "api",
	})
	if err != nil {
		t.Fatalf("seed: Create failed: %v", err)
	}
	if err := h.manager.AssignReplica(created.ID, capsule.ReplicaID(replicaID), nodeID); err != nil {
		t.Fatalf("seed: AssignReplica failed: %v", err)
	}
	// The async initial election request fires via goroutines from the
	// event bus; give it a beat to land before the test subscribes.
	time.Sleep(50 * time.Millisecond)
	return created
}

// waitForElectionRequested drains the event bus subscription channel
// until an ElectionRequested arrives or the timeout expires. Returns
// the event or calls t.Fatalf on timeout.
func waitForElectionRequested(t *testing.T, ch <-chan events.Event, timeout time.Duration) events.ElectionRequested {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("event bus closed before ElectionRequested arrived")
			}
			if req, ok := ev.(events.ElectionRequested); ok {
				return req
			}
			// Ignore other event types that might share the stream.
		case <-deadline:
			t.Fatal("timeout waiting for ElectionRequested")
		}
	}
}

// --- Node failure re-election -----------------------------------------

func TestOnNodeFailed_FiresElectionForOrphanedReplica(t *testing.T) {
	h, bus := newReelectionHandler(t)

	const (
		cluster   = "test/dc1/c"
		capsName  = "orphan-app"
		replicaID = "0"
		failedID  = "dead-node"
	)
	c := seedCapsuleWithReplica(t, h, cluster, capsName, replicaID, failedID)

	sub := bus.Subscribe(events.TypeElectionRequested)

	h.onNodeFailed(context.Background(), events.NodeFailed{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      failedID,
		ClusterPath: cluster,
	})

	req := waitForElectionRequested(t, sub, 2*time.Second)

	if req.CapsuleID != c.ID.String() {
		t.Errorf("CapsuleID = %q, want %q", req.CapsuleID, c.ID.String())
	}
	if req.ReplicaID != replicaID {
		t.Errorf("ReplicaID = %q, want %q", req.ReplicaID, replicaID)
	}
	if req.Reason != events.ElectionReasonEnum.NodeFailure() {
		t.Errorf("Reason = %q, want %q", req.Reason, events.ElectionReasonEnum.NodeFailure())
	}
	if req.PreviousNodeID != failedID {
		t.Errorf("PreviousNodeID = %q, want %q", req.PreviousNodeID, failedID)
	}
	if req.Priority != priorityNodeFailure {
		t.Errorf("Priority = %d, want %d", req.Priority, priorityNodeFailure)
	}
	if req.ClusterPath != cluster {
		t.Errorf("ClusterPath = %q, want %q", req.ClusterPath, cluster)
	}
}

func TestOnNodeFailed_SkipsUnaffectedCapsules(t *testing.T) {
	h, bus := newReelectionHandler(t)
	// Seed a capsule whose replica lives on a surviving node. The
	// failure of some other node should not trigger a re-election.
	seedCapsuleWithReplica(t, h, "test/dc1/c", "alive-app", "0", "healthy-node")

	sub := bus.Subscribe(events.TypeElectionRequested)

	h.onNodeFailed(context.Background(), events.NodeFailed{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      "unrelated-dead-node",
		ClusterPath: "test/dc1/c",
	})

	select {
	case ev := <-sub:
		if _, ok := ev.(events.ElectionRequested); ok {
			t.Error("unrelated node failure should not fire an election request")
		}
	case <-time.After(200 * time.Millisecond):
		// Expected path: no event.
	}
}

func TestOnNodeFailed_FiresOneEventPerOrphanedReplica(t *testing.T) {
	h, bus := newReelectionHandler(t)

	// Two distinct capsules both pinned to the same failing node.
	c1 := seedCapsuleWithReplica(t, h, "test/dc1/c", "app-a", "0", "dead")
	c2 := seedCapsuleWithReplica(t, h, "test/dc1/c", "app-b", "0", "dead")

	sub := bus.Subscribe(events.TypeElectionRequested)

	h.onNodeFailed(context.Background(), events.NodeFailed{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      "dead",
		ClusterPath: "test/dc1/c",
	})

	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case ev := <-sub:
			req, ok := ev.(events.ElectionRequested)
			if !ok {
				continue
			}
			seen[req.CapsuleID] = true
		case <-deadline:
			t.Fatalf("timeout: saw %d of 2 expected election requests", len(seen))
		}
	}
	if !seen[c1.ID.String()] || !seen[c2.ID.String()] {
		t.Errorf("expected election requests for both capsules, got %+v", seen)
	}
}

// --- Scale-up re-election ---------------------------------------------

func TestOnScaleEvent_ScaleUpFiresElection(t *testing.T) {
	h, bus := newReelectionHandler(t)

	c := seedCapsuleWithReplica(t, h, "test/dc1/c", "scaled", "0", "node-a")

	sub := bus.Subscribe(events.TypeElectionRequested)

	h.onScaleEvent(scaling.ScaleEvent{
		CapsuleID: c.ID,
		RuleName:  "cpu>70",
		Action:    enums.ScalingActionEnum.ScaleUp(),
	})

	req := waitForElectionRequested(t, sub, 2*time.Second)

	if req.CapsuleID != string(c.ID) {
		t.Errorf("CapsuleID = %q, want %q", req.CapsuleID, string(c.ID))
	}
	if req.Reason != events.ElectionReasonEnum.ScaleUp() {
		t.Errorf("Reason = %q, want %q", req.Reason, events.ElectionReasonEnum.ScaleUp())
	}
	if req.Priority != priorityScaleUp {
		t.Errorf("Priority = %d, want %d", req.Priority, priorityScaleUp)
	}
	// Next replica slot should be one past the highest existing.
	if req.ReplicaID != "1" {
		t.Errorf("ReplicaID = %q, want %q (next slot)", req.ReplicaID, "1")
	}
}

func TestOnScaleEvent_ScaleDownDoesNotFireElection(t *testing.T) {
	h, bus := newReelectionHandler(t)

	c := seedCapsuleWithReplica(t, h, "test/dc1/c", "scaled", "0", "node-a")

	sub := bus.Subscribe(events.TypeElectionRequested)

	h.onScaleEvent(scaling.ScaleEvent{
		CapsuleID: c.ID,
		RuleName:  "cpu<10",
		Action:    enums.ScalingActionEnum.ScaleDown(),
	})

	select {
	case ev := <-sub:
		if _, ok := ev.(events.ElectionRequested); ok {
			t.Error("scale-down should not fire an election request")
		}
	case <-time.After(200 * time.Millisecond):
		// Expected.
	}
}
