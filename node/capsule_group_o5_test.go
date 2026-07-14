package node

import (
	"context"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/runtime/mock"
)

// waitForGroupClaimWon polls the given subscription channel for a
// GroupClaimWon for the given group ID emitted AT OR AFTER `since`, failing
// the test on timeout. Filtering by timestamp prevents matching a stale
// GroupClaimWon buffered from an earlier round. Returns the matching event.
func waitForGroupClaimWon(t *testing.T, ch <-chan events.Event, groupID string, since time.Time, timeout time.Duration) events.GroupClaimWon {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("event bus closed before GroupClaimWon arrived")
			}
			won, ok := ev.(events.GroupClaimWon)
			if !ok || won.GroupID != groupID {
				continue
			}
			if won.OccurredAt.Before(since) {
				// Stale Won from a prior round; keep waiting.
				continue
			}
			return won
		case <-deadline:
			t.Fatalf("timeout waiting for GroupClaimWon for group %s", groupID)
		}
	}
}

// waitForReservationCleared polls until the group's capacity reservation is
// released (the runningLoop clears it once every member reaches Running),
// failing the test on timeout.
func waitForReservationCleared(t *testing.T, n *Node, groupID capsule.CapsuleID, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !n.Election().HasReservation(groupID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n.Election().HasReservation(groupID) {
		t.Fatalf("reservation not cleared for group %s after %v", groupID.String(), timeout)
	}
}

// TestGroupReElection_SameNode_AfterWin is the direct O5 repro, now driven
// through the PRODUCTION trigger (onMemberPlacementFailed) rather than a
// manual UnassignReplica + raw GroupReelectionRequested publish.
//
// A 1-node cluster wins a same-node group; every member reaches Running (so
// the reservation is cleared by the runningLoop). A member placement failure
// is then driven through the real trigger — onMemberPlacementFailed rolls the
// siblings back (SyncStatus/StopCapsule/StopContainer), O5b clears every
// sibling replica binding, the reservation is released, and a single
// GroupReelectionRequested is refired. Because the bindings are now cleared,
// member self-anti-affinity no longer excludes the only node, so the group
// wins AGAIN on the same node.
//
// Before the O5 fix the retained group slot (localGroupClaims[group]) made the
// second round dead-lock ("no claim heard"). Before the O5b fix the still-bound
// member replicas made the round fail eligibility ("member does not fit" ->
// GroupClaimFailed). With both fixes the round re-wins on the only node.
func TestGroupReElection_SameNode_AfterWin(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	const cluster = "test/dc1/grp-o5-afterwin"

	rt := mock.New()
	n1 := testNodeWithRuntime(t, "o5-aw-n1", 0, rt)
	defer func() { _ = n1.Stop() }()

	joinCluster(t, n1, cluster)
	waitForPhonebookCount(t, n1, cluster, 1, 10*time.Second)
	joinOrbitOrFail(t, n1, cluster, "api")
	waitForGossipMeshSettle(1)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wonCh := n1.EventBus().Subscribe(events.TypeGroupClaimWon)

	group, members, err := n1.Capsules().CreateGroup(ctx, cluster, "o5-aw", sameNodeGroupSpec("db", "api"), nil)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}

	// Round 1: the group wins on the only node and both members run.
	round1Start := time.Now()
	won1 := waitForGroupClaimWon(t, wonCh, group.ID.String(), round1Start, 20*time.Second)
	if won1.NodeID != n1.ID().String() {
		t.Fatalf("round 1 winner = %s, want %s", won1.NodeID, n1.ID().String())
	}
	waitForContainersRunning(t, rt, 2, 20*time.Second)

	// The reservation clears once every member is Running (runningLoop),
	// and every member replica is bound to n1 (advanceGroupMembersToAssigned).
	waitForReservationCleared(t, n1, group.ID, 10*time.Second)
	// Brief settle so any in-flight round-1 election callbacks drain before
	// the synthetic failure is driven (mirrors the recovery-test discipline).
	time.Sleep(500 * time.Millisecond)

	// Round 2: drive the PRODUCTION member-placement-failure trigger directly
	// (as the recovery test does, so the synthetic rollback does not interleave
	// with the real election manager's in-flight round for this group). The
	// onMemberPlacementFailed body performs the full production rollback —
	// including the O5b binding clear — and refires GroupReelectionRequested.
	// The empty NodeID here means "no node excluded": the faithful same-node
	// re-election case where the only home must remain eligible.
	round2Start := time.Now()
	n1.CapsuleHandler().onMemberPlacementFailed(events.MemberPlacementFailed{
		BaseEvent:   events.NewBaseEvent(),
		GroupID:     group.ID.String(),
		CapsuleID:   members[1].ID.String(),
		ClusterPath: cluster,
		Reason:      "synthetic: member start failed after group was running",
	})

	won2 := waitForGroupClaimWon(t, wonCh, group.ID.String(), round2Start, 20*time.Second)
	if won2.NodeID != n1.ID().String() {
		t.Fatalf("round 2 winner = %s, want %s (same-node re-election must win again)", won2.NodeID, n1.ID().String())
	}
}

// TestGroupReElection_SameNode_O5b_ClearsBindingsThenReWins is the direct O5b
// repro: it proves that the PRODUCTION member-placement-failure trigger clears
// every sibling replica binding (NodeID reset to empty) so a same-node group
// re-election finds the only node ELIGIBLE and wins again — with NO manual
// UnassignReplica.
//
// Root cause (O5b): each member's group-claim binding
// (advanceGroupMembersToAssigned -> AssignReplica(id,"0",winner)) is durable.
// Left in place, member self-anti-affinity (electionCapsuleLookup.
// NodesRunningCapsule) counts the still-bound member and self-excludes the
// only node, so the same-node re-election is refused ("member does not fit")
// and emits GroupClaimFailed. Fatal on a single node.
//
// The test asserts BOTH halves of the fix in one flow:
//  1. after onMemberPlacementFailed, every member replica binding is cleared
//     (NodeID == "") — the unit-level binding-clear assertion (mirrors O3);
//  2. the refired GroupReelectionRequested re-wins on the same node
//     (GroupClaimWon with NodeID == n1) — proving eligibility was restored.
func TestGroupReElection_SameNode_O5b_ClearsBindingsThenReWins(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	const cluster = "test/dc1/grp-o5b-clearbind"

	rt := mock.New()
	n1 := testNodeWithRuntime(t, "o5b-cb-n1", 0, rt)
	defer func() { _ = n1.Stop() }()

	joinCluster(t, n1, cluster)
	waitForPhonebookCount(t, n1, cluster, 1, 10*time.Second)
	joinOrbitOrFail(t, n1, cluster, "api")
	waitForGossipMeshSettle(1)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	wonCh := n1.EventBus().Subscribe(events.TypeGroupClaimWon)

	group, members, err := n1.Capsules().CreateGroup(ctx, cluster, "o5b-cb", sameNodeGroupSpec("svc-a", "svc-b"), nil)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}

	// Round 1: the group wins on the only node; both members run and every
	// member replica is bound to n1.
	round1Start := time.Now()
	won1 := waitForGroupClaimWon(t, wonCh, group.ID.String(), round1Start, 20*time.Second)
	if won1.NodeID != n1.ID().String() {
		t.Fatalf("round 1 winner = %s, want %s", won1.NodeID, n1.ID().String())
	}
	waitForContainersRunning(t, rt, 2, 20*time.Second)
	waitForReservationCleared(t, n1, group.ID, 10*time.Second)

	// Precondition: every member replica IS bound to n1 before the trigger.
	// If this fails the test is not exercising the O5b binding-clear path.
	for _, m := range members {
		c := n1.Capsules().Get(m.ID)
		if c == nil {
			t.Fatalf("member %s missing before trigger", m.ID.String())
		}
		bound := false
		for _, r := range c.Replicas {
			if r.NodeID == n1.ID().String() {
				bound = true
				break
			}
		}
		if !bound {
			t.Fatalf("precondition: member %s replica not bound to n1 (replicas=%+v)", m.ID.String(), c.Replicas)
		}
	}

	time.Sleep(500 * time.Millisecond)

	// Drive the production trigger. onMemberPlacementFailed must clear every
	// sibling replica binding (O5b) BEFORE it refires GroupReelectionRequested.
	round2Start := time.Now()
	n1.CapsuleHandler().onMemberPlacementFailed(events.MemberPlacementFailed{
		BaseEvent:   events.NewBaseEvent(),
		GroupID:     group.ID.String(),
		CapsuleID:   members[1].ID.String(),
		ClusterPath: cluster,
		Reason:      "synthetic: member start failed",
	})

	// Assertion 1 (unit-level, mirrors O3's crash-path binding-clear test):
	// every member replica binding is cleared. onMemberPlacementFailed is
	// synchronous through the unbind + publish, so by the time it returns the
	// bindings must already be empty.
	for _, m := range members {
		c := n1.Capsules().Get(m.ID)
		if c == nil {
			t.Fatalf("member %s missing after trigger", m.ID.String())
		}
		for _, r := range c.Replicas {
			if r.NodeID != "" {
				t.Fatalf("O5b: member %s replica %s still bound to %q after onMemberPlacementFailed (expected cleared)",
					m.ID.String(), string(r.ReplicaID), r.NodeID)
			}
		}
	}

	// Assertion 2: the refired same-node re-election re-wins on n1. With the
	// bindings cleared the only node is eligible again; without the O5b clear
	// this round would emit GroupClaimFailed ("member does not fit") and this
	// wait would time out.
	won2 := waitForGroupClaimWon(t, wonCh, group.ID.String(), round2Start, 20*time.Second)
	if won2.NodeID != n1.ID().String() {
		t.Fatalf("round 2 winner = %s, want %s (same-node re-election must win again after binding clear)",
			won2.NodeID, n1.ID().String())
	}
}

// NOTE on the mid-fanout variant: the reservation-still-live re-election
// property (a same-node group re-election wins WHILE the group's own
// reservation is live) is asserted deterministically at the election-manager
// level in election/manager_group_o5_test.go
// (TestGroupReElection_SameNode_MidFanout). It is not repeated as a node
// integration test because a node-level mid-fanout re-run re-drives the
// runtime StartGroup against containers created by round 1 while the
// reservation is still live; the manager-level test isolates the
// slot/reservation mechanic without that interference.
