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

// resetGroupForSameNodeReelection reproduces the full pre-state that a
// production same-node group re-election trigger MUST leave a group in
// before a re-election can win on the same node, isolating the O5 slot
// deadlock these tests gate:
//
//   - every member replica binding is cleared (UnassignReplica sets
//     NodeID="") so member self-anti-affinity (electionCapsuleLookup.
//     NodesRunningCapsule) stops excluding the local node;
//   - every member FSM is resynced back to Announced so the re-election's
//     forward transitions are valid;
//   - the round-1 group-inflight entry is drained so the re-fire is not
//     deduped.
//
// TRIPWIRE (O5b): the production same-node group re-election triggers
// (emitGroupReelectionForGroup / maybeEmitGroupReelection / the
// onMemberPlacementFailed rollback) currently resync sibling FSM state and
// clear the reservation but do NOT clear member bindings — so a same-node
// group re-election is refused by member self-anti-affinity in production
// today. That is a latent bug tracked separately as O5b (mirrors O3's
// onContainerCrash → UnassignReplica clear). When O5b lands, this manual
// binding-clear should be deleted and the tests driven through the real
// trigger. Until then this helper clears bindings so the tests exercise the
// O5 slot-reservation path in isolation.
//
// clearReservation controls whether the group's capacity reservation is
// also cleared (mirrors the reservation-cleared "group was Running, member
// later crashed" case vs the reservation-live "mid-fanout" case).
func resetGroupForSameNodeReelection(t *testing.T, n *Node, group *capsule.Capsule, members []*capsule.Capsule, clearReservation bool) {
	t.Helper()

	for _, m := range members {
		// Clear the binding (O5b's owed step) so member self-anti-affinity
		// (electionCapsuleLookup.NodesRunningCapsule) no longer excludes the
		// local node when the group re-election runs CalculateCombinedFit.
		//
		// NOTE: we deliberately do NOT resync each member's FSM back to
		// Announced here. In this single-node harness a member resynced to
		// Announced auto-fires a fresh PER-MEMBER election, which re-binds
		// the member to the local node and re-arms self-anti-affinity —
		// re-poisoning the very state we just cleared. The GROUP election's
		// eligibility (runGroupElection → CalculateCombinedFit) depends only
		// on member bindings and resources, not on member FSM status, so
		// clearing bindings alone isolates the O5 group slot mechanic. The
		// FSM resync is part of the production trigger's contract (and the
		// member-election re-bind it causes is exactly the O5b binding-clear
		// gap), but is orthogonal to the group slot deadlock under test.
		if err := n.Capsules().UnassignReplica(m.ID, capsule.ReplicaID("0")); err != nil {
			t.Fatalf("reset: UnassignReplica(%s) failed: %v", m.ID, err)
		}
	}

	if clearReservation {
		n.Election().ClearGroupReservation(group.ID)
	}

	// Drain the round-1 group-inflight entry so the re-fire is not deduped.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !n.Election().HasGroupInFlight(group.ID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Force-cancel any lingering in-flight round so the re-fire is admitted.
	n.Election().CancelGroupInFlight(group.ID)
}

// TestGroupReElection_SameNode_AfterWin is the direct O5 repro. A 1-node
// cluster wins a same-node group; every member reaches Running (so the
// reservation is cleared by the runningLoop). The group is then reset to its
// pre-re-election state (see resetGroupForSameNodeReelection) and a same-node
// re-election is re-fired on the node bus with an EMPTY exclude list — the
// faithful wire representation of a same-node-eligible group re-election
// (the exclude list is the only discriminator between a move-off-this-node
// rollback and a re-place-anywhere re-election).
//
// Before the O5 fix the group slot (localGroupClaims[group]) retained from
// the first Won made the second round dead-lock: tryLocalGroupClaim fails,
// the round jumps to waitForRemoteGroupVerdict and times out with "group
// election timeout (no claim heard)". After the fix the slot is released on
// Won and the park-wake-redecide loop re-acquires it, so the group wins
// again on the only node.
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

	// The reservation clears once every member is Running (runningLoop).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !n1.Election().HasReservation(group.ID) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n1.Election().HasReservation(group.ID) {
		t.Fatalf("reservation not cleared after all members running")
	}

	// Reset the group to the pre-re-election state (reservation already
	// cleared above; clear bindings + resync FSM + drain in-flight).
	resetGroupForSameNodeReelection(t, n1, group, members, true /*clearReservation*/)

	// Round 2: same-node re-election with EMPTY exclude / failed node. On
	// the unfixed code this dead-locks (slot retained from round 1); on the
	// fixed code the released slot + park-loop re-acquire wins again.
	round2Start := time.Now()
	memberIDs := make([]string, 0, len(members))
	for _, m := range members {
		memberIDs = append(memberIDs, m.ID.String())
	}
	n1.EventBus().Publish(events.GroupReelectionRequested{
		BaseEvent:   events.NewBaseEvent(),
		GroupID:     group.ID.String(),
		ClusterPath: cluster,
		MemberIDs:   memberIDs,
	})

	won2 := waitForGroupClaimWon(t, wonCh, group.ID.String(), round2Start, 20*time.Second)
	if won2.NodeID != n1.ID().String() {
		t.Fatalf("round 2 winner = %s, want %s (same-node re-election must win again)", won2.NodeID, n1.ID().String())
	}
}

// NOTE on the mid-fanout variant: the reservation-still-live re-election
// property (a same-node group re-election wins WHILE the group's own
// reservation is live) is asserted deterministically at the election-manager
// level in election/manager_group_o5_test.go
// (TestGroupReElection_SameNode_MidFanout). It is not repeated as a node
// integration test because a node-level mid-fanout re-run re-drives the
// runtime StartGroup against containers created by round 1, which fails with
// "container already exists" -> MemberPlacementFailed -> an auto excluded
// re-election that occupies the group-inflight slot and dedupes the manual
// re-fire (an O5b/runtime-teardown interaction orthogonal to the O5 slot
// deadlock). The manager-level test isolates the slot/reservation mechanic
// without that interference; the node-level AfterWin test above proves the
// event-bus + dispatch + reservation wiring end-to-end.
