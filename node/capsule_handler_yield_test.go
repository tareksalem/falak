package node

import (
	"context"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/enums"
	"github.com/tareksalem/falak/node/internal/events"
)

// O14c node-side yield handling (HARD INVARIANT #2 — re-point the binding
// to the real winner). The container teardown (INVARIANT #1) is the
// RuntimeBridge's job through StopContainer / the O2 ignore-set and is
// covered in the runtime package; these tests assert the capsule-side
// re-point + FSM mirror and, critically, that a yield fires NO re-election.

// TestOnElectionYielded_RepointsBindingToWinner is the single-replica
// yield: a node that briefly held the replica un-wins. The stale local
// binding is cleared, the winner is recorded (so NodesRunningCapsule
// converges on the winner, not merely on nobody), and the FSM mirrors the
// winner as remote (Assigned). No re-election fires.
func TestOnElectionYielded_RepointsBindingToWinner(t *testing.T) {
	h, bus := newReelectionHandler(t)

	const (
		cluster   = "test/dc1/c"
		capsName  = "yield-app"
		replicaID = "0"
	)
	// The local node briefly held the replica (it reported Won, then yielded).
	c := seedCapsuleWithReplica(t, h, cluster, capsName, replicaID, "local-node")

	// Guard: a yield must NOT request a re-election (the winner is known).
	electSub := bus.Subscribe(events.TypeElectionRequested)

	h.onElectionYielded(events.ElectionYielded{
		BaseEvent:    events.NewBaseEvent(),
		CapsuleID:    c.ID.String(),
		ReplicaID:    replicaID,
		ClusterPath:  cluster,
		WinnerNodeID: "winner-node",
	})

	// The replica binding must now point at the WINNER — HARD INVARIANT #2.
	after := h.manager.Get(c.ID)
	if after == nil {
		t.Fatal("capsule vanished after yield")
	}
	if len(after.Replicas) != 1 {
		t.Fatalf("expected 1 replica, got %d: %+v", len(after.Replicas), after.Replicas)
	}
	if after.Replicas[0].NodeID != "winner-node" {
		t.Fatalf("replica NodeID = %q, want re-pointed to winner-node", after.Replicas[0].NodeID)
	}
	// FSM mirrors the winner as remote: Assigned (a clean loser view).
	if after.Status != enums.CapsuleStatusEnum.Assigned() {
		t.Fatalf("capsule status = %q, want Assigned (winner mirrored as remote)", after.Status)
	}

	// O2/re-election guard: no ElectionRequested may fire from a yield.
	assertNoElectionRequested(t, electSub)
}

// TestOnElectionYielded_EmptyWinnerClearsBinding covers the defensive path
// where WinnerNodeID is empty (should not happen in production, but the
// subscriber must not record an empty binding). The stale local binding is
// still cleared; no winner is recorded.
func TestOnElectionYielded_EmptyWinnerClearsBinding(t *testing.T) {
	h, _ := newReelectionHandler(t)
	c := seedCapsuleWithReplica(t, h, "test/dc1/c", "yield-empty", "0", "local-node")

	h.onElectionYielded(events.ElectionYielded{
		BaseEvent:   events.NewBaseEvent(),
		CapsuleID:   c.ID.String(),
		ReplicaID:   "0",
		ClusterPath: "test/dc1/c",
		// WinnerNodeID intentionally empty.
	})

	after := h.manager.Get(c.ID)
	if after == nil {
		t.Fatal("capsule vanished after yield")
	}
	for _, r := range after.Replicas {
		if r.NodeID == "local-node" {
			t.Fatalf("stale local binding must be cleared on yield, got %+v", after.Replicas)
		}
	}
}

// TestOnGroupClaimYielded_StopsMembersAndRepoints is the group twin: a node
// that briefly won a same-node group election yields. Every member it
// started must be stopped through the runtime rollback hook (the O2
// ignore-set path — StopContainer), each member's binding re-pointed to the
// winner, and NO re-election (GroupReelectionRequested) fired.
func TestOnGroupClaimYielded_StopsMembersAndRepoints(t *testing.T) {
	h, bus := newReelectionHandler(t)
	rb := &fakeRollback{}
	h.SetRuntimeRollback(rb)

	const cluster = "test/dc1/c"
	group, members := seedSameNodeGroup(t, h, cluster, "yield-grp", []string{"db", "api"}, "local-node")

	// Guards: a group yield must NOT re-elect.
	reelectSub := bus.Subscribe(events.TypeGroupReelectionRequested)
	electSub := bus.Subscribe(events.TypeElectionRequested)

	memberIDs := make([]string, 0, len(members))
	for _, m := range members {
		memberIDs = append(memberIDs, m.ID.String())
	}

	h.onGroupClaimYielded(events.GroupClaimYielded{
		BaseEvent:    events.NewBaseEvent(),
		GroupID:      group.ID.String(),
		ClusterPath:  cluster,
		MemberIDs:    memberIDs,
		WinnerNodeID: "winner-node",
	})

	// Every member must have been stopped through the ignore-set rollback
	// (HARD INVARIANT #1 routes through StopContainer).
	calls := rb.calls()
	stopped := make(map[string]bool)
	for _, c := range calls {
		stopped[c.capsuleID] = true
		if c.replicaID != "0" {
			t.Errorf("group member stop replicaID = %q, want 0", c.replicaID)
		}
	}
	for _, m := range members {
		if !stopped[m.ID.String()] {
			t.Errorf("member %s was not stopped through the rollback hook", m.ID.String())
		}
	}

	// Every member binding must be re-pointed to the winner — HARD INVARIANT #2.
	for _, m := range members {
		after := h.manager.Get(m.ID)
		if after == nil {
			t.Fatalf("member %s vanished after group yield", m.ID.String())
		}
		if len(after.Replicas) != 1 || after.Replicas[0].NodeID != "winner-node" {
			t.Fatalf("member %s binding = %+v, want re-pointed to winner-node", m.ID.String(), after.Replicas)
		}
		if after.Status != enums.CapsuleStatusEnum.Assigned() {
			t.Fatalf("member %s status = %q, want Assigned", m.ID.String(), after.Status)
		}
	}

	// No re-election of any kind.
	assertNoGroupReelectionOrElection(t, reelectSub, electSub)
}

// --- helpers ------------------------------------------------------------

// seedSameNodeGroup materializes a same-node CapsuleGroup via CreateGroup
// (which sets each member's GroupID/GroupMember so GetGroup resolves
// siblings), then binds each member's replica to nodeID and drives it to
// Running so a yield has real state to roll back. Returns the group capsule
// and its materialized members.
func seedSameNodeGroup(t *testing.T, h *CapsuleHandler, cluster, groupName string, memberNames []string, nodeID string) (*capsule.Capsule, []*capsule.Capsule) {
	t.Helper()
	memberSpecs := make([]capsule.MemberSpec, 0, len(memberNames))
	for _, name := range memberNames {
		spec := capsule.CapsuleSpec{Name: name, Image: name + ":v1", Orbit: "api"}
		capsule.DefaultSpec(&spec)
		memberSpecs = append(memberSpecs, capsule.MemberSpec{Name: name, Spec: spec})
	}
	groupSpec := capsule.GroupSpec{
		Colocation:    capsule.ColocationModeEnum.SameNode(),
		Members:       memberSpecs,
		CascadeDelete: true,
	}
	group, members, err := h.manager.CreateGroup(context.Background(), cluster, groupName, groupSpec, nil)
	if err != nil {
		t.Fatalf("seed group: CreateGroup failed: %v", err)
	}

	// Bind each member's replica to nodeID and drive it to Running so
	// rollbackGroupContainers exercises the Running → StopCapsule branch
	// AND has a durable binding to re-point.
	for _, m := range members {
		if err := h.manager.AssignReplica(m.ID, capsule.ReplicaID("0"), nodeID); err != nil {
			t.Fatalf("seed group: AssignReplica(%s) failed: %v", m.ID, err)
		}
		if err := h.manager.SyncStatus(m.ID, enums.CapsuleStatusEnum.Running()); err != nil {
			t.Fatalf("seed group: sync member %s to Running failed: %v", m.ID, err)
		}
	}
	time.Sleep(30 * time.Millisecond)
	return group, members
}

// assertNoElectionRequested fails the test if an ElectionRequested event
// arrives within a short window.
func assertNoElectionRequested(t *testing.T, electSub <-chan events.Event) {
	t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case ev := <-electSub:
			if _, ok := ev.(events.ElectionRequested); ok {
				t.Fatal("a yield must not request a re-election (winner is already known)")
			}
		case <-deadline:
			return
		}
	}
}

// assertNoGroupReelectionOrElection fails the test if a
// GroupReelectionRequested or ElectionRequested arrives within a short
// window.
func assertNoGroupReelectionOrElection(t *testing.T, reelectSub, electSub <-chan events.Event) {
	t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case ev := <-reelectSub:
			if _, ok := ev.(events.GroupReelectionRequested); ok {
				t.Fatal("a group yield must not fire GroupReelectionRequested (winner is already known)")
			}
		case ev := <-electSub:
			if _, ok := ev.(events.ElectionRequested); ok {
				t.Fatal("a group yield must not fire ElectionRequested")
			}
		case <-deadline:
			return
		}
	}
}
