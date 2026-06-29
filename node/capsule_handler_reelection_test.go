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

// --- Container crash re-election (O3) ----------------------------------

// TestOnContainerCrash_ClearsBindingThenFiresElection verifies the O3 fix:
// when a container bound to the local node crashes/is removed, the handler
// clears the stale replica binding (so self-anti-affinity stops excluding the
// node) AND fires a re-election for the same replica. Order matters — the
// binding must already be cleared by the time the election round evaluates
// eligibility — so this test asserts the binding is empty after onContainerCrash
// returns (it is cleared synchronously) and an ElectionRequested was fired.
func TestOnContainerCrash_ClearsBindingThenFiresElection(t *testing.T) {
	h, bus := newReelectionHandler(t)

	const (
		cluster   = "test/dc1/c"
		capsName  = "crash-app"
		replicaID = "0"
	)
	// Bind the replica to the LOCAL node — the crash path only acts on
	// replicas this node was running.
	c := seedCapsuleWithReplica(t, h, cluster, capsName, replicaID, "local-node")

	// Precondition: the replica is bound to the local node.
	before := h.manager.Get(c.ID)
	if len(before.Replicas) != 1 || before.Replicas[0].NodeID != "local-node" {
		t.Fatalf("precondition: expected replica bound to local-node, got %+v", before.Replicas)
	}

	// Drive the capsule-level FSM all the way to Running, mirroring a placed
	// capsule, so the crash path's MarkNodeFailed downgrade (O6) is exercised
	// against the real Running → Announced transition (not a no-op rejection).
	for _, step := range []struct {
		name string
		fn   func(capsule.CapsuleID) error
	}{
		{"StartElection", h.manager.StartElection},
		{"WinElection", h.manager.WinElection},
		{"StartExecution", h.manager.StartExecution},
		{"MarkRunning", h.manager.MarkRunning},
	} {
		if err := step.fn(c.ID); err != nil {
			t.Fatalf("setup: %s failed: %v", step.name, err)
		}
	}
	if got, _ := h.manager.Status(c.ID); got != enums.CapsuleStatusEnum.Running() {
		t.Fatalf("setup: expected Running before crash, got %s", got)
	}

	sub := bus.Subscribe(events.TypeElectionRequested)

	h.onContainerCrash(events.CapsuleExecutionFailed{
		BaseEvent: events.NewBaseEvent(),
		CapsuleID: c.ID.String(),
		Reason:    "container exited",
	})

	// UnassignReplica is synchronous, so by the time onContainerCrash
	// returns the binding is already cleared.
	after := h.manager.Get(c.ID)
	if len(after.Replicas) != 1 {
		t.Fatalf("replica slot should be preserved, got %d replicas", len(after.Replicas))
	}
	if after.Replicas[0].NodeID != "" {
		t.Errorf("binding should be cleared before re-election, got NodeID %q", after.Replicas[0].NodeID)
	}

	// O6: the capsule-level FSM must be downgraded Running → Announced so the
	// re-election round's forward transitions are valid.
	if got, _ := h.manager.Status(c.ID); got != enums.CapsuleStatusEnum.Announced() {
		t.Errorf("FSM should be downgraded to Announced after crash, got %s", got)
	}

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
	if req.Priority != priorityNodeFailure {
		t.Errorf("Priority = %d, want %d", req.Priority, priorityNodeFailure)
	}
}

// TestOnContainerCrash_SkipsReplicaOnOtherNode verifies the crash handler
// ignores replicas hosted on a different node — only the local node's own
// containers crash through this path, and a remote-bound replica must keep its
// binding and not trigger a local re-election.
func TestOnContainerCrash_SkipsReplicaOnOtherNode(t *testing.T) {
	h, bus := newReelectionHandler(t)

	c := seedCapsuleWithReplica(t, h, "test/dc1/c", "remote-app", "0", "other-node")

	sub := bus.Subscribe(events.TypeElectionRequested)

	h.onContainerCrash(events.CapsuleExecutionFailed{
		BaseEvent: events.NewBaseEvent(),
		CapsuleID: c.ID.String(),
		Reason:    "container exited",
	})

	// Remote-bound replica must remain bound.
	after := h.manager.Get(c.ID)
	if after.Replicas[0].NodeID != "other-node" {
		t.Errorf("remote replica binding should be untouched, got %q", after.Replicas[0].NodeID)
	}

	select {
	case ev := <-sub:
		if _, ok := ev.(events.ElectionRequested); ok {
			t.Error("crash of a remote-bound replica should not fire a local election")
		}
	case <-time.After(200 * time.Millisecond):
		// Expected: no event.
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
