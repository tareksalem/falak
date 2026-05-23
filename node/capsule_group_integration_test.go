package node

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	miekg "github.com/miekg/dns"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/enums"
	netdns "github.com/tareksalem/falak/network/dns"
	"github.com/tareksalem/falak/network/endpoints"
	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
	"github.com/tareksalem/falak/node/internal/events"
	falakrt "github.com/tareksalem/falak/runtime"
	"github.com/tareksalem/falak/runtime/mock"
)

// waitForGroupOn polls a node's capsule store for a group capsule by
// name. Returns the *Capsule, or fails the test on timeout.
func waitForGroupOn(t *testing.T, n *Node, name string, timeout time.Duration) *capsule.Capsule {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c := n.Capsules().GetByName(name)
		if c != nil && c.Spec.Kind == capsule.CapsuleKindEnum.Group() {
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("group %q not found on node %s within %v", name, n.Name(), timeout)
	return nil
}

// waitForGroupGone polls until no capsule with the given name (any kind)
// exists on the node, or fails on timeout.
func waitForGroupGone(t *testing.T, n *Node, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n.Capsules().GetByName(name) == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("capsule %q still present on node %s after %v", name, n.Name(), timeout)
}

// setup3NodeClusterAndOrbits boots 3 nodes, joins them to the cluster,
// has every node subscribe to the given orbits, and waits for gossipsub
// mesh formation. Returns the 3 nodes.
func setup3NodeClusterAndOrbits(t *testing.T, clusterPath string, orbits ...string) (*Node, *Node, *Node) {
	t.Helper()
	n1 := testNode(t, clusterPath+"-n1", 0)
	n2 := testNode(t, clusterPath+"-n2", 0)
	n3 := testNode(t, clusterPath+"-n3", 0)

	joinCluster(t, n1, clusterPath)
	bootstrap := getBootstrapAddr(n1)
	if bootstrap == "" {
		t.Fatal("failed to get bootstrap address")
	}
	joinCluster(t, n2, clusterPath, bootstrap)
	joinCluster(t, n3, clusterPath, bootstrap)

	waitForPhonebookCount(t, n1, clusterPath, 3, 10*time.Second)
	waitForPhonebookCount(t, n2, clusterPath, 3, 10*time.Second)
	waitForPhonebookCount(t, n3, clusterPath, 3, 10*time.Second)

	for _, orbit := range orbits {
		joinOrbitOrFail(t, n1, clusterPath, orbit)
		joinOrbitOrFail(t, n2, clusterPath, orbit)
		joinOrbitOrFail(t, n3, clusterPath, orbit)
	}

	// Mesh formation buffer — gossipsub needs a few heartbeat ticks
	// after orbit join before grafts settle and a publish reaches every
	// peer reliably. There is no public observable for "mesh stable" so
	// this remains a documented timing buffer per the audit major #2
	// policy. Replacing it with an explicit predicate requires gossipsub
	// to expose its mesh state — out of scope for this bundle.
	waitForGossipMeshSettle(3)
	return n1, n2, n3
}

// TestCapsuleGroup_PropagatesAcross3Nodes (10.T13 base) — node1 creates a
// 2-member group with depends_on; nodes 2 and 3 must observe the group
// capsule AND every member via orbit gossip.
func TestCapsuleGroup_PropagatesAcross3Nodes(t *testing.T) {
	const clusterPath = "test/dc1/grp-prop"

	n1, n2, n3 := setup3NodeClusterAndOrbits(t, clusterPath, "data", "public")

	groupSpec := capsule.GroupSpec{
		Colocation:    capsule.ColocationModeEnum.SameOrbit(),
		CascadeDelete: true,
		Members: []capsule.MemberSpec{
			{
				Name: "db",
				Spec: capsule.CapsuleSpec{
					Name:  "db",
					Image: "registry.test/postgres:15",
					Orbit: "data",
				},
			},
			{
				Name: "api",
				Spec: capsule.CapsuleSpec{
					Name:  "api",
					Image: "registry.test/api:v1",
					Orbit: "public",
				},
				DependsOn: []string{"db"},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	group, members, err := n1.Capsules().CreateGroup(ctx, clusterPath, "my-stack", groupSpec, capsule.Labels{
		"app": "my-stack",
	})
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}

	// Node1 should already have the group + both members.
	if n1.Capsules().Get(group.ID) == nil {
		t.Fatal("node1 missing group locally after Create")
	}

	// Node2 and node3 must see the group + both members within the
	// gossip propagation window.
	for _, peer := range []*Node{n2, n3} {
		_ = waitForGroupOn(t, peer, "my-stack", 10*time.Second)
		_ = waitForCapsule(t, peer, "db", 10*time.Second)
		_ = waitForCapsule(t, peer, "api", 10*time.Second)

		// Verify group sub-spec was carried over the wire.
		gp := peer.Capsules().GetByName("my-stack")
		if gp == nil || gp.Spec.Group == nil {
			t.Fatalf("node %s: group spec not propagated", peer.Name())
		}
		if gp.Spec.Group.Colocation != capsule.ColocationModeEnum.SameOrbit() {
			t.Errorf("node %s: colocation not propagated, got %q", peer.Name(), gp.Spec.Group.Colocation)
		}
		if !gp.Spec.Group.CascadeDelete {
			t.Errorf("node %s: cascade_delete not propagated as true", peer.Name())
		}
		if len(gp.Spec.Group.MemberIDs) != 2 {
			t.Errorf("node %s: MemberIDs count: got %d, want 2", peer.Name(), len(gp.Spec.Group.MemberIDs))
		}

		// Verify members carry GroupID + GroupMember.
		dbMember := peer.Capsules().GetByName("db")
		if dbMember == nil {
			continue
		}
		if dbMember.Spec.GroupID != group.ID {
			t.Errorf("node %s: member db has wrong GroupID: got %s, want %s", peer.Name(), dbMember.Spec.GroupID, group.ID)
		}
		if !dbMember.Spec.GroupMember {
			t.Errorf("node %s: member db has GroupMember=false", peer.Name())
		}
	}
}

// TestCapsuleGroup_CascadeDelete_3Node (10.T14) — cascade delete on a
// 3-member group removes every member from every node within the gossip
// propagation window.
func TestCapsuleGroup_CascadeDelete_3Node(t *testing.T) {
	const clusterPath = "test/dc1/grp-cascade"

	n1, n2, n3 := setup3NodeClusterAndOrbits(t, clusterPath, "data", "public", "workers")

	groupSpec := capsule.GroupSpec{
		Colocation:    capsule.ColocationModeEnum.SameOrbit(),
		CascadeDelete: true,
		Members: []capsule.MemberSpec{
			{Name: "db", Spec: capsule.CapsuleSpec{Name: "db", Image: "img:1", Orbit: "data"}},
			{Name: "api", Spec: capsule.CapsuleSpec{Name: "api", Image: "img:2", Orbit: "public"}},
			{Name: "worker", Spec: capsule.CapsuleSpec{Name: "worker", Image: "img:3", Orbit: "workers"}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	group, _, err := n1.Capsules().CreateGroup(ctx, clusterPath, "stack", groupSpec, nil)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	// Wait for all 3 members + group to reach n2 and n3.
	for _, peer := range []*Node{n2, n3} {
		waitForGroupOn(t, peer, "stack", 10*time.Second)
		waitForCapsule(t, peer, "db", 10*time.Second)
		waitForCapsule(t, peer, "api", 10*time.Second)
		waitForCapsule(t, peer, "worker", 10*time.Second)
	}

	// Cascade delete on n1.
	if err := n1.Capsules().Delete(ctx, group.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Within the gossip propagation window, every name should disappear
	// from n2 and n3.
	for _, peer := range []*Node{n2, n3} {
		waitForGroupGone(t, peer, "stack", 10*time.Second)
		waitForGroupGone(t, peer, "db", 10*time.Second)
		waitForGroupGone(t, peer, "api", 10*time.Second)
		waitForGroupGone(t, peer, "worker", 10*time.Second)
	}

	// n1 is local-only; verify the cascade emptied the store there too.
	if n1.Capsules().GetByName("stack") != nil ||
		n1.Capsules().GetByName("db") != nil ||
		n1.Capsules().GetByName("api") != nil ||
		n1.Capsules().GetByName("worker") != nil {
		t.Error("n1: cascade did not clear all rows locally")
	}
}

// TestCapsuleGroup_NonCascadeDelete_3Node (10.T16 across the mesh) —
// non-cascade group delete leaves the members running on every node
// with GroupID cleared.
func TestCapsuleGroup_NonCascadeDelete_3Node(t *testing.T) {
	const clusterPath = "test/dc1/grp-noncascade"

	n1, n2, n3 := setup3NodeClusterAndOrbits(t, clusterPath, "data", "public")

	groupSpec := capsule.GroupSpec{
		Colocation:    capsule.ColocationModeEnum.SameOrbit(),
		CascadeDelete: false,
		Members: []capsule.MemberSpec{
			{Name: "db", Spec: capsule.CapsuleSpec{Name: "db", Image: "img:1", Orbit: "data"}},
			{Name: "api", Spec: capsule.CapsuleSpec{Name: "api", Image: "img:2", Orbit: "public"}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	group, _, err := n1.Capsules().CreateGroup(ctx, clusterPath, "stack-nc", groupSpec, nil)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	// Confirm propagation first.
	for _, peer := range []*Node{n2, n3} {
		waitForGroupOn(t, peer, "stack-nc", 10*time.Second)
		waitForCapsule(t, peer, "db", 10*time.Second)
		waitForCapsule(t, peer, "api", 10*time.Second)
	}

	// Non-cascade delete on n1.
	if err := n1.Capsules().Delete(ctx, group.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Group capsule should be gone everywhere.
	for _, peer := range []*Node{n1, n2, n3} {
		waitForGroupGone(t, peer, "stack-nc", 10*time.Second)
	}

	// Members should still exist on every node, with GroupID cleared on
	// the originator (n1 is the source of truth for cleared metadata;
	// the EventCapsuleGroupReleased re-announce propagates to peers).
	dbN1 := n1.Capsules().GetByName("db")
	apiN1 := n1.Capsules().GetByName("api")
	if dbN1 == nil || apiN1 == nil {
		t.Fatal("n1: members not preserved after non-cascade delete")
	}
	if dbN1.Spec.GroupID != "" {
		t.Errorf("n1: db still has GroupID %q after non-cascade delete", dbN1.Spec.GroupID)
	}
	if dbN1.Spec.GroupMember {
		t.Error("n1: db still has GroupMember=true after non-cascade delete")
	}

	// Members still observable on peers (group-released re-announcement
	// preserves the row; the peer view's GroupID may take an extra
	// gossip round to clear, so we just assert presence here, not the
	// flag state — that's covered by the originator-side asserts above
	// and the dormancy contract test).
	for _, peer := range []*Node{n2, n3} {
		if peer.Capsules().GetByName("db") == nil {
			t.Errorf("%s: db member missing after non-cascade group delete", peer.Name())
		}
		if peer.Capsules().GetByName("api") == nil {
			t.Errorf("%s: api member missing after non-cascade group delete", peer.Name())
		}
	}
}

// sameNodeGroupSpec builds a GroupSpec with same-node colocation and
// the given member names. The first member has no dependencies; each
// subsequent member depends on the previous one. This produces a deterministic
// topological order for assertions.
//
// Members declare a small but non-zero resource reservation so the
// gravity calculator produces a positive score; with score 0 the
// election protocol's wait timer equals the deadline timer and the
// round consistently times out in tests.
func sameNodeGroupSpec(memberNames ...string) capsule.GroupSpec {
	members := make([]capsule.MemberSpec, 0, len(memberNames))
	for i, name := range memberNames {
		ms := capsule.MemberSpec{
			Name: name,
			Spec: capsule.CapsuleSpec{
				Name:  name,
				Image: fmt.Sprintf("registry.test/%s:v1", name),
				Orbit: "api",
				Resources: capsule.ResourceRequirements{
					CPUCores: 1,
					MemoryMB: 64,
				},
			},
		}
		if i > 0 {
			ms.DependsOn = []string{memberNames[i-1]}
		}
		members = append(members, ms)
	}
	return capsule.GroupSpec{
		Colocation:    capsule.ColocationModeEnum.SameNode(),
		CascadeDelete: true,
		Members:       members,
	}
}

// waitForCapsuleStatus polls for the given capsule reaching the given
// status (on the node's local view). Fails on timeout.
func waitForCapsuleStatus(t *testing.T, n *Node, id capsule.CapsuleID, want enums.CapsuleStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c := n.Capsules().Get(id)
		if c != nil && c.Status == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c := n.Capsules().Get(id)
	got := enums.CapsuleStatus("missing")
	if c != nil {
		got = c.Status
	}
	t.Fatalf("node %s: capsule %s status=%q, want=%q (timeout %v)",
		n.Name(), id.String(), got, want, timeout)
}

// waitForContainersRunning polls a mock runtime until at least the
// expected number of containers report Running. Returns the set of
// container IDs that reached Running; fails on timeout.
func waitForContainersRunning(t *testing.T, rt *mock.Runtime, expectedCount int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rt.ContainerCount() >= expectedCount {
			return rt.ContainerCount()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("mock runtime: expected %d containers running, got %d (timeout %v)",
		expectedCount, rt.ContainerCount(), timeout)
	return 0
}

// TestCapsuleGroup_SameNode_PlacementSucceeds (10.T26) — single-node
// cluster, 2-member same-node group, both members must reach Running
// on the node and the group reservation must be cleared.
func TestCapsuleGroup_SameNode_PlacementSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	const cluster = "test/dc1/grp-sn-ok"

	rt := mock.New()
	n1 := testNodeWithRuntime(t, "sn-ok-n1", 0, rt)
	defer n1.Stop()

	joinCluster(t, n1, cluster)
	waitForPhonebookCount(t, n1, cluster, 1, 10*time.Second)
	joinOrbitOrFail(t, n1, cluster, "api")

	// Single-node orbit-loop spin-up buffer (no remote mesh to form,
	// but the per-orbit subscriber goroutine needs one tick to settle).
	// Documented per audit major #2 — no public mesh-state observable.
	waitForGossipMeshSettle(1)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	group, members, err := n1.Capsules().CreateGroup(ctx, cluster, "sn-ok", sameNodeGroupSpec("db", "api"), nil)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}

	// The runtime handler should start both member containers via the
	// GroupClaimWon path on this node.
	waitForContainersRunning(t, rt, 2, 30*time.Second)

	// Group reservation must be cleared once both members are Running.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !n1.Election().HasReservation(group.ID) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n1.Election().HasReservation(group.ID) {
		t.Errorf("group reservation NOT cleared after both members running")
	}
}

// TestCapsuleGroup_SameNode_NodeFailure_ReElects (10.T28) — 2-node
// cluster, same-node group lands on n1, n1 dies, n2 observes the
// failure and emits GroupReelectionRequested with ExcludeNodes=[n1],
// the group re-elects on n2.
func TestCapsuleGroup_SameNode_NodeFailure_ReElects(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	const cluster = "test/dc1/grp-sn-nodefail"

	rt1 := mock.New()
	rt2 := mock.New()
	n1 := testNodeWithRuntime(t, "sn-fail-n1", 0, rt1)
	n2 := testNodeWithRuntime(t, "sn-fail-n2", 0, rt2)
	defer func() { _ = n2.Stop() }()

	joinCluster(t, n1, cluster)
	bootstrap := getBootstrapAddr(n1)
	joinCluster(t, n2, cluster, bootstrap)
	waitForPhonebookCount(t, n1, cluster, 2, 10*time.Second)
	waitForPhonebookCount(t, n2, cluster, 2, 10*time.Second)

	joinOrbitOrFail(t, n1, cluster, "api")
	joinOrbitOrFail(t, n2, cluster, "api")

	waitForGossipMeshSettle(2) // documented per audit major #2

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	group, members, err := n1.Capsules().CreateGroup(ctx, cluster, "sn-fail", sameNodeGroupSpec("svc-a", "svc-b"), nil)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}

	// Wait for n2 to see the group via gossip so the re-election event
	// arrives with members locally resolvable.
	_ = waitForGroupOn(t, n2, "sn-fail", 15*time.Second)

	// Wait for placement on a node — the score should land everything
	// on n1 OR n2; both runtimes are equally capable.
	totalRunning := 0
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		totalRunning = rt1.ContainerCount() + rt2.ContainerCount()
		if totalRunning >= 2 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if totalRunning < 2 {
		t.Fatalf("expected 2 containers running on either node, got %d (rt1=%d, rt2=%d)",
			totalRunning, rt1.ContainerCount(), rt2.ContainerCount())
	}

	// Discover which node won the group election by checking which
	// runtime has the containers.
	var winner, survivor *Node
	if rt1.ContainerCount() >= 2 {
		winner, survivor = n1, n2
	} else {
		winner, survivor = n2, n1
	}
	winnerID := winner.ID().String()
	t.Logf("group placed on %s (initial winner)", winner.Name())

	// Subscribe on the survivor to verify the reelection event is emitted.
	reelectCh := survivor.EventBus().Subscribe(events.TypeGroupReelectionRequested)

	// Simulate node failure: publish NodeFailed on the survivor's bus.
	// In production this comes from SWIM; bypassing it here keeps the
	// test fast and focused on the group-failure-handling path.
	_ = winner.Stop()

	survivor.EventBus().Publish(events.NodeFailed{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      winnerID,
		ClusterPath: cluster,
	})

	// Wait for the reelection request to fire on the survivor.
	select {
	case ev := <-reelectCh:
		rev, ok := ev.(events.GroupReelectionRequested)
		if !ok {
			t.Fatalf("expected GroupReelectionRequested, got %T", ev)
		}
		if rev.GroupID != group.ID.String() {
			t.Fatalf("reelection: group_id=%s, want %s", rev.GroupID, group.ID.String())
		}
		if rev.FailedNodeID != winnerID {
			t.Fatalf("reelection: failed_node=%s, want %s", rev.FailedNodeID, winnerID)
		}
		if len(rev.MemberIDs) != 2 {
			t.Fatalf("reelection: members=%d, want 2", len(rev.MemberIDs))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for GroupReelectionRequested on survivor")
	}

	// Survivor's runtime now becomes the new winner; wait for both
	// containers to come up. The election round can take multiple
	// retries when the gravity score is zero (timer race in the
	// election protocol when wait == deadline), so the test is patient
	// here. If a round fails, the reservation watchdog/timeout
	// eventually emits another reelection event because the failed
	// node is still detected as down.
	survivorRT := rt1
	if survivor == n2 {
		survivorRT = rt2
	}
	waitForContainersRunning(t, survivorRT, 2, 30*time.Second)
}

// TestCapsuleGroup_SameNode_RollbackOnMemberFailure (10.T18) — 2-node
// cluster, same-node group with members svc-a + svc-b. Both runtimes
// initially fail to pull svc-b's image. The winner's runtime emits
// MemberPlacementFailed for svc-b; the capsule handler must:
//   - cancel any in-flight parked starts via the runtime rollback hook,
//   - stop any sibling that already started (svc-a),
//   - clear the election capacity reservation,
//   - emit GroupReelectionRequested with ExcludeNodes=[winner].
//
// The retry on the survivor succeeds only after the test clears the
// per-image pull error on the survivor's mock runtime, so the
// reelection lands without firing a second rollback. The test asserts
// every step of the rollback before relaxing the failure injection so
// the placement eventually succeeds.
func TestCapsuleGroup_SameNode_RollbackOnMemberFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	const cluster = "test/dc1/grp-sn-rollback"
	pullErr := fmt.Errorf("simulated pull failure for svc-b image")
	const failImage = "registry.test/svc-b:v1"

	rt1 := mock.New(mock.WithPullErrorForImage(failImage, pullErr))
	rt2 := mock.New(mock.WithPullErrorForImage(failImage, pullErr))

	n1 := testNodeWithRuntime(t, "sn-rb-n1", 0, rt1)
	n2 := testNodeWithRuntime(t, "sn-rb-n2", 0, rt2)
	defer func() { _ = n2.Stop() }()
	defer func() { _ = n1.Stop() }()

	joinCluster(t, n1, cluster)
	bootstrap := getBootstrapAddr(n1)
	joinCluster(t, n2, cluster, bootstrap)
	waitForPhonebookCount(t, n1, cluster, 2, 10*time.Second)
	waitForPhonebookCount(t, n2, cluster, 2, 10*time.Second)

	joinOrbitOrFail(t, n1, cluster, "api")
	joinOrbitOrFail(t, n2, cluster, "api")

	waitForGossipMeshSettle(2) // documented per audit major #2

	// Subscribe BEFORE creating the group so we cannot miss the
	// MemberPlacementFailed / GroupReelectionRequested events.
	mpfChN1 := n1.EventBus().Subscribe(events.TypeMemberPlacementFailed)
	mpfChN2 := n2.EventBus().Subscribe(events.TypeMemberPlacementFailed)
	reelectChN1 := n1.EventBus().Subscribe(events.TypeGroupReelectionRequested)
	reelectChN2 := n2.EventBus().Subscribe(events.TypeGroupReelectionRequested)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	group, members, err := n1.Capsules().CreateGroup(ctx, cluster, "sn-rb", sameNodeGroupSpec("svc-a", "svc-b"), nil)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}

	// Wait for n2 to see the group via gossip so reelection has a
	// locally-resolvable member set.
	_ = waitForGroupOn(t, n2, "sn-rb", 15*time.Second)

	// Wait for MemberPlacementFailed on either node — whichever wins
	// the initial election will emit it after svc-a runs and svc-b
	// fails its pull.
	var mpfEvent events.MemberPlacementFailed
	mpfDeadline := time.NewTimer(30 * time.Second)
	defer mpfDeadline.Stop()
	select {
	case ev := <-mpfChN1:
		mpfEvent = ev.(events.MemberPlacementFailed)
	case ev := <-mpfChN2:
		mpfEvent = ev.(events.MemberPlacementFailed)
	case <-mpfDeadline.C:
		t.Fatalf("timed out waiting for MemberPlacementFailed (rt1=%d rt2=%d)",
			rt1.ContainerCount(), rt2.ContainerCount())
	}

	if mpfEvent.GroupID != group.ID.String() {
		t.Fatalf("MemberPlacementFailed: group_id=%s, want %s", mpfEvent.GroupID, group.ID.String())
	}
	failedNodeID := mpfEvent.NodeID
	if failedNodeID == "" {
		t.Fatal("MemberPlacementFailed missing NodeID")
	}
	t.Logf("MemberPlacementFailed received: group=%s capsule=%s failed_node=%s reason=%q",
		mpfEvent.GroupID, mpfEvent.CapsuleID, failedNodeID, mpfEvent.Reason)

	// Identify which node won initially and which is the survivor.
	var winner, survivor *Node
	var survivorRT *mock.Runtime
	if failedNodeID == n1.ID().String() {
		winner, survivor = n1, n2
		survivorRT = rt2
	} else if failedNodeID == n2.ID().String() {
		winner, survivor = n2, n1
		survivorRT = rt1
	} else {
		t.Fatalf("MemberPlacementFailed.NodeID=%q matches neither n1 (%s) nor n2 (%s)",
			failedNodeID, n1.ID().String(), n2.ID().String())
	}
	t.Logf("rollback initiated on %s (winner), survivor=%s", winner.Name(), survivor.Name())

	// The reservation on the winner must be cleared by the rollback
	// path. Allow a brief window for the subscriber goroutine to run.
	reservationCleared := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !winner.Election().HasReservation(group.ID) {
			reservationCleared = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !reservationCleared {
		t.Errorf("winner %s: reservation NOT cleared after MemberPlacementFailed", winner.Name())
	}

	// The sibling that started before the failure (svc-a) must be
	// rolled back on the winner's RUNTIME — the container must be
	// stopped. Note: the capsule manager's Status field reflects the
	// global gossip view, so by the time we inspect it the survivor
	// may already have started its own svc-a and flipped the global
	// status back to Running. The local container state on the
	// failed winner is the source of truth for the rollback assertion.
	var winnerRT *mock.Runtime
	if winner == n1 {
		winnerRT = rt1
	} else {
		winnerRT = rt2
	}
	rolledBack := false
	rbDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(rbDeadline) {
		// Find any container on the winner's runtime whose status is
		// not Running. The container ID format is falak-<capsuleID>-<replicaID>.
		// After rollback either the container is stopped or removed
		// entirely; both satisfy the rollback contract.
		stopped := 0
		for _, m := range members {
			cid := "falak-" + m.ID.String() + "-0"
			st := winnerRT.ContainerStatus(cid)
			if st != falakrt.ContainerStatusEnum.Running() {
				stopped++
			}
		}
		if stopped == len(members) {
			rolledBack = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !rolledBack {
		t.Errorf("winner %s: at least one sibling container still Running after rollback (rt=%d)",
			winner.Name(), winnerRT.ContainerCount())
	}

	// GroupReelectionRequested must fire with ExcludeNodes (carried as
	// FailedNodeID on the event) set to the winner. Either subscriber
	// can observe it (publish hits every local subscriber on the same
	// bus). The winner's bus shut down for the survivor's path is not
	// in scope here — both nodes are alive.
	reelectDeadline := time.NewTimer(15 * time.Second)
	defer reelectDeadline.Stop()
	var reelect events.GroupReelectionRequested
	select {
	case ev := <-reelectChN1:
		reelect = ev.(events.GroupReelectionRequested)
	case ev := <-reelectChN2:
		reelect = ev.(events.GroupReelectionRequested)
	case <-reelectDeadline.C:
		t.Fatal("timed out waiting for GroupReelectionRequested after MemberPlacementFailed")
	}
	if reelect.GroupID != group.ID.String() {
		t.Errorf("reelection: group_id=%s, want %s", reelect.GroupID, group.ID.String())
	}
	if reelect.FailedNodeID != failedNodeID {
		t.Errorf("reelection: failed_node=%s, want %s", reelect.FailedNodeID, failedNodeID)
	}

	// Relax the pull failure on the survivor so the re-election
	// converges and the group lands successfully on the second node.
	survivorRT.SetPullErrorForImage(failImage, nil)

	// MemberPlacementFailed is emitted on the failing winner's local
	// bus only — there is no cluster-wide broadcast layer for it in
	// v1. The winner's HandleGroupClaimRequest short-circuits because
	// the failed node is in ExcludeNodes. To drive the re-election to
	// convergence on the survivor, re-publish the
	// GroupReelectionRequested event on the survivor's bus
	// (mirroring how TestCapsuleGroup_SameNode_NodeFailure_ReElects
	// manually drives NodeFailed on both buses).
	survivor.EventBus().Publish(reelect)

	// Wait for the survivor's runtime to start both containers.
	waitForContainersRunning(t, survivorRT, 2, 30*time.Second)
}

// TestCapsuleGroup_SameNode_StartedZeroOfThree (10.T27) — same-node
// group lands on one node; that node dies BEFORE starting any member.
// Survivor transitions Assigned siblings back to Announced and
// re-elects the group cleanly. Verified by asserting the surviving
// node sees a GroupReelectionRequested event AND every sibling
// capsule is reset out of any post-election state on the survivor's
// view.
//
// Both runtimes are wired with a Start error so whichever node wins,
// no members actually reach Running before the simulated failure.
// This deterministically exercises the "started zero" branch
// regardless of which node the election picks.
func TestCapsuleGroup_SameNode_StartedZeroOfThree(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	const cluster = "test/dc1/grp-sn-zero"

	startErr := fmt.Errorf("simulated start failure: 0 of N members started")
	rt1 := mock.New(mock.WithStartError(startErr))
	rt2 := mock.New(mock.WithStartError(startErr))

	n1 := testNodeWithRuntime(t, "sn-zero-n1", 0, rt1)
	n2 := testNodeWithRuntime(t, "sn-zero-n2", 0, rt2)
	defer func() { _ = n2.Stop() }()
	defer func() { _ = n1.Stop() }()

	joinCluster(t, n1, cluster)
	bootstrap := getBootstrapAddr(n1)
	joinCluster(t, n2, cluster, bootstrap)
	waitForPhonebookCount(t, n1, cluster, 2, 10*time.Second)
	waitForPhonebookCount(t, n2, cluster, 2, 10*time.Second)

	joinOrbitOrFail(t, n1, cluster, "api")
	joinOrbitOrFail(t, n2, cluster, "api")

	waitForGossipMeshSettle(2) // documented per audit major #2

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	group, members, err := n1.Capsules().CreateGroup(ctx, cluster, "sn-zero", sameNodeGroupSpec("a", "b", "c"), nil)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}

	// Wait for n2 to see the group.
	_ = waitForGroupOn(t, n2, "sn-zero", 15*time.Second)

	// Wait until a reservation has been recorded on either node — that
	// tells us which node won the group election. Phase 10.16
	// introduced a rollback path that clears the winner's reservation
	// when the runtime fails to start a member; with both runtimes
	// configured to fail Start, the winner's reservation may already
	// have been cleared by the time the test polls. The observer's
	// reservation persists, so polling either node converges
	// regardless of which one won.
	var winner, survivor *Node
	resDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(resDeadline) {
		if holder := n1.Election().ReservationNodeID(group.ID); holder != "" {
			if holder == n1.ID().String() {
				winner, survivor = n1, n2
			} else {
				winner, survivor = n2, n1
			}
			break
		}
		if holder := n2.Election().ReservationNodeID(group.ID); holder != "" {
			if holder == n1.ID().String() {
				winner, survivor = n1, n2
			} else {
				winner, survivor = n2, n1
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if winner == nil {
		t.Fatal("no reservation recorded on n1 or n2 within 30s — group election never converged")
	}
	winnerID := winner.ID().String()
	t.Logf("group placed on %s (winner per reservation)", winner.Name())

	// Subscribe BEFORE failing the node so we don't race the emission.
	reelectCh := survivor.EventBus().Subscribe(events.TypeGroupReelectionRequested)

	// Kill the winner. With Start errors set on both runtimes no
	// members ever reached Running.
	_ = winner.Stop()

	// Publish NodeFailed directly on the survivor's bus to drive
	// onNodeFailed without waiting for SWIM detection.
	survivor.EventBus().Publish(events.NodeFailed{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      winnerID,
		ClusterPath: cluster,
	})

	// Survivor must emit a single GroupReelectionRequested for the
	// orphaned group.
	select {
	case ev := <-reelectCh:
		rev, ok := ev.(events.GroupReelectionRequested)
		if !ok {
			t.Fatalf("expected GroupReelectionRequested, got %T", ev)
		}
		if rev.GroupID != group.ID.String() {
			t.Fatalf("reelection: group_id=%s, want %s", rev.GroupID, group.ID.String())
		}
		if rev.FailedNodeID != winnerID {
			t.Fatalf("reelection: failed_node=%s, want %s", rev.FailedNodeID, winnerID)
		}
		if len(rev.MemberIDs) != 3 {
			t.Fatalf("reelection: members=%d, want 3", len(rev.MemberIDs))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for GroupReelectionRequested")
	}

	// After the GroupReelectionRequested fires, no sibling should be
	// left Assigned to the failed node. The handler's reservation
	// path resyncs them to Announced before publishing the
	// reelection.
	for _, m := range members {
		c := survivor.Capsules().Get(m.ID)
		if c == nil {
			continue
		}
		for _, r := range c.Replicas {
			if r.NodeID == winnerID && c.Status == enums.CapsuleStatusEnum.Assigned() {
				t.Errorf("sibling %s left Assigned to failed node %s", m.ID, winnerID)
			}
		}
	}
}

// testNodeWithRuntimeAndOpts constructs a node with the given runtime
// and any extra options spliced into the standard test wiring. Used by
// the 10.17 recovery test to tune the placement retry cap so cap
// exhaustion can be driven with synthesized events.
func testNodeWithRuntimeAndOpts(t *testing.T, name string, port int, rt *mock.Runtime, extra ...Option) *Node {
	t.Helper()

	dataDir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}

	logger, _ := zap.NewDevelopment()

	opts := append([]Option{
		WithName(name),
		WithListenAddrs(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port)),
		WithDataDir(dataDir),
		WithLogger(logger.Named(name)),
		WithRuntime(rt),
	}, extra...)

	n := New(opts...)
	if err := n.Start(); err != nil {
		t.Fatalf("failed to start %s: %v", name, err)
	}
	return n
}

// TestCapsuleGroup_SameNode_RecoverOnNodeJoined (10.T22) — verifies
// the 10.17 NodeJoined recovery path on the capsule handler in
// isolation. A single-node cluster receives a same-node group; the
// test then drives the handler's per-event methods directly with
// synthetic MemberPlacementFailed inputs to deterministically exhaust
// the placement retry cap. The handler must:
//   - record the group in placementFailedGroups with capReached=true
//     and the cumulative ExcludeNodes drawn from each failure round,
//   - upon receiving a NewMemberReceived event (the node-bus signal
//     that a peer joined the cluster), emit a GroupReelectionRequested
//     with FailedNodeID empty and ExcludeNodes carrying the historical
//     failure set,
//   - drop the placement-failed entry once a GroupClaimWon arrives.
//
// Driving the handler's per-event methods directly (rather than
// pushing events onto the bus) avoids interfering with the real
// election manager's in-flight rounds for this same group, which
// would race with synthetic verdicts and panic the election manager
// (a pre-existing issue distinct from the 10.17 contract).
func TestCapsuleGroup_SameNode_RecoverOnNodeJoined(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	const cluster = "test/dc1/grp-sn-recover"
	const fakeFailedNode1 = "12D3KooW-failed-node-1"
	const fakeFailedNode2 = "12D3KooW-failed-node-2"
	const fakeJoinedNode = "12D3KooW-fresh-recovery-target"

	rt := mock.New()
	n1 := testNodeWithRuntimeAndOpts(t, "sn-rec-n1", 0, rt, WithPlacementRetryCap(1))
	defer func() { _ = n1.Stop() }()

	joinCluster(t, n1, cluster)
	waitForPhonebookCount(t, n1, cluster, 1, 10*time.Second)
	joinOrbitOrFail(t, n1, cluster, "api")

	waitForGossipMeshSettle(1) // documented per audit major #2

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	group, members, err := n1.Capsules().CreateGroup(ctx, cluster, "sn-rec", sameNodeGroupSpec("svc-a", "svc-b"), nil)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}

	// Wait for the original group election to drain — both containers
	// should reach Running on n1. Driving synthetic MPFs while the
	// original group election round is still in flight would replace
	// the topic's group listener and panic the running election
	// goroutine (a pre-existing election-layer bug). Letting the round
	// complete first cleans up the listener registration.
	waitForContainersRunning(t, rt, 2, 15*time.Second)
	// Brief settle for the post-Running reservation clear and any
	// in-flight election callbacks.
	time.Sleep(500 * time.Millisecond)

	// Subscribe to GroupClaimFailed BEFORE driving the synthetic MPFs
	// so the cap-exhaustion emission is observable. The election
	// manager may also emit GroupClaimFailed for the real election
	// round; we filter on the distinctive "placement retries
	// exhausted" reason the capsule handler attaches.
	claimFailedCh := n1.EventBus().Subscribe(events.TypeGroupClaimFailed)

	// Drive the handler's per-event method directly so the synthetic
	// rollback does not interleave with the real election manager's
	// in-flight round for the same group.
	handler := n1.CapsuleHandler()

	// First synthetic MPF: attempt=1, under cap=1. Entry upserted with
	// excludeNodes=[fakeFailedNode1] and capReached=false.
	handler.onMemberPlacementFailed(events.MemberPlacementFailed{
		BaseEvent:   events.NewBaseEvent(),
		GroupID:     group.ID.String(),
		CapsuleID:   members[1].ID.String(),
		NodeID:      fakeFailedNode1,
		ClusterPath: cluster,
		Reason:      "synthetic: pull failed on fakeFailedNode1",
	})

	// Second synthetic MPF: attempt=2 > cap=1, cap exhausts.
	// capReached flips to true, excludeNodes=[fakeFailedNode1, fakeFailedNode2].
	handler.onMemberPlacementFailed(events.MemberPlacementFailed{
		BaseEvent:   events.NewBaseEvent(),
		GroupID:     group.ID.String(),
		CapsuleID:   members[1].ID.String(),
		NodeID:      fakeFailedNode2,
		ClusterPath: cluster,
		Reason:      "synthetic: pull failed on fakeFailedNode2",
	})

	capDeadline := time.NewTimer(10 * time.Second)
	defer capDeadline.Stop()
	sawCapExhausted := false
	for !sawCapExhausted {
		select {
		case ev := <-claimFailedCh:
			gcf, ok := ev.(events.GroupClaimFailed)
			if !ok {
				continue
			}
			if gcf.GroupID != group.ID.String() {
				continue
			}
			if !strings.Contains(gcf.Reason, "placement retries exhausted") {
				continue
			}
			t.Logf("cap exhausted: %s", gcf.Reason)
			sawCapExhausted = true
		case <-capDeadline.C:
			t.Fatal("timed out waiting for cap-exhaustion GroupClaimFailed")
		}
	}

	if !handler.HasPlacementFailedEntry(group.ID) {
		t.Fatal("expected placement-failed entry to be recorded after cap exhaustion")
	}

	// Drive recovery: synthetic NewMemberReceived. The handler's
	// onNodeJoined must emit GroupReelectionRequested. Subscribe BEFORE
	// driving the event.
	reelectCh := n1.EventBus().Subscribe(events.TypeGroupReelectionRequested)

	handler.onNodeJoined(events.NewMemberReceived{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      fakeJoinedNode,
		ClusterPath: cluster,
	})

	recoveryDeadline := time.NewTimer(5 * time.Second)
	defer recoveryDeadline.Stop()
	var recovery events.GroupReelectionRequested
	gotRecovery := false
	for !gotRecovery {
		select {
		case ev := <-reelectCh:
			rev, ok := ev.(events.GroupReelectionRequested)
			if !ok {
				continue
			}
			if rev.GroupID == group.ID.String() && rev.FailedNodeID == "" {
				recovery = rev
				gotRecovery = true
			}
		case <-recoveryDeadline.C:
			t.Fatal("timed out waiting for recovery GroupReelectionRequested after NewMemberReceived")
		}
	}

	if recovery.ClusterPath != cluster {
		t.Errorf("recovery: cluster=%q, want %q", recovery.ClusterPath, cluster)
	}
	if len(recovery.MemberIDs) != 2 {
		t.Errorf("recovery: members=%d, want 2", len(recovery.MemberIDs))
	}
	excludeSet := make(map[string]struct{}, len(recovery.ExcludeNodes))
	for _, id := range recovery.ExcludeNodes {
		excludeSet[id] = struct{}{}
	}
	if _, ok := excludeSet[fakeFailedNode1]; !ok {
		t.Errorf("recovery: ExcludeNodes missing fakeFailedNode1; got %v", recovery.ExcludeNodes)
	}
	if _, ok := excludeSet[fakeFailedNode2]; !ok {
		t.Errorf("recovery: ExcludeNodes missing fakeFailedNode2; got %v", recovery.ExcludeNodes)
	}
	t.Logf("recovery emitted: group=%s exclude=%v members=%d",
		recovery.GroupID, recovery.ExcludeNodes, len(recovery.MemberIDs))

	// Drive GroupClaimWon directly through the subscriber goroutine via
	// the bus — the handleGroupClaimWon path is what clears the entry
	// in production, and we want to exercise it end-to-end.
	n1.EventBus().Publish(events.GroupClaimWon{
		BaseEvent:   events.NewBaseEvent(),
		GroupID:     group.ID.String(),
		ClusterPath: cluster,
		MemberIDs:   recovery.MemberIDs,
		NodeID:      fakeJoinedNode,
		Score:       1.0,
	})

	clearDeadline := time.Now().Add(5 * time.Second)
	cleared := false
	for time.Now().Before(clearDeadline) {
		if !handler.HasPlacementFailedEntry(group.ID) {
			cleared = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !cleared {
		t.Error("placement-failed entry not cleared after GroupClaimWon")
	}
}

// TestCapsuleGroup_SameNode_RecoverOnNodeJoined_DroppedWhenGroupGone
// (10.T22b) — when a placement-failed group has been deleted locally
// before a NodeJoined recovery tick, the recovery path must drop the
// stale entry instead of emitting a re-election for a non-existent
// group.
func TestCapsuleGroup_SameNode_RecoverOnNodeJoined_DroppedWhenGroupGone(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	const cluster = "test/dc1/grp-sn-recover-gone"
	const fakeFailedNode = "12D3KooW-failed-node-A"
	const fakeJoinedNode = "12D3KooW-fresh-recovery-target-A"

	rt := mock.New()
	n1 := testNodeWithRuntimeAndOpts(t, "sn-rec-gone-n1", 0, rt, WithPlacementRetryCap(1))
	defer func() { _ = n1.Stop() }()

	joinCluster(t, n1, cluster)
	waitForPhonebookCount(t, n1, cluster, 1, 10*time.Second)
	joinOrbitOrFail(t, n1, cluster, "api")

	waitForGossipMeshSettle(1) // documented per audit major #2

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	group, members, err := n1.Capsules().CreateGroup(ctx, cluster, "sn-rec-gone", sameNodeGroupSpec("svc-a", "svc-b"), nil)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	// Wait for the original group election to drain (see the sibling
	// test for the listener-replacement panic this guards against).
	waitForContainersRunning(t, rt, 2, 15*time.Second)
	time.Sleep(500 * time.Millisecond)

	handler := n1.CapsuleHandler()
	claimFailedCh := n1.EventBus().Subscribe(events.TypeGroupClaimFailed)

	// Drive two synthetic MPFs to exhaust cap=1.
	for i := 0; i < 2; i++ {
		handler.onMemberPlacementFailed(events.MemberPlacementFailed{
			BaseEvent:   events.NewBaseEvent(),
			GroupID:     group.ID.String(),
			CapsuleID:   members[1].ID.String(),
			NodeID:      fmt.Sprintf("%s-%d", fakeFailedNode, i),
			ClusterPath: cluster,
			Reason:      "synthetic",
		})
	}
	capDeadline := time.NewTimer(10 * time.Second)
	defer capDeadline.Stop()
	sawCap := false
	for !sawCap {
		select {
		case ev := <-claimFailedCh:
			gcf, ok := ev.(events.GroupClaimFailed)
			if !ok {
				continue
			}
			if gcf.GroupID != group.ID.String() {
				continue
			}
			if !strings.Contains(gcf.Reason, "placement retries exhausted") {
				continue
			}
			sawCap = true
		case <-capDeadline.C:
			t.Fatal("timed out waiting for cap-exhaustion GroupClaimFailed")
		}
	}
	if !handler.HasPlacementFailedEntry(group.ID) {
		t.Fatal("expected placement-failed entry after cap exhaustion")
	}

	// Delete the group locally — the cascade-delete bridge removes
	// both the group and its members. The placement-failed entry
	// becomes stale and should be dropped by the next NodeJoined tick.
	if err := n1.Capsules().Delete(ctx, group.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Drive the recovery tick directly. The recovery path must NOT
	// emit a GroupReelectionRequested for a missing group, and must
	// drop the entry.
	reelectCh := n1.EventBus().Subscribe(events.TypeGroupReelectionRequested)
	handler.onNodeJoined(events.NewMemberReceived{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      fakeJoinedNode,
		ClusterPath: cluster,
	})

	// Brief window to assert NO event fires AND the entry is gone.
	select {
	case ev := <-reelectCh:
		rev, ok := ev.(events.GroupReelectionRequested)
		if ok && rev.GroupID == group.ID.String() {
			t.Errorf("recovery emitted GroupReelectionRequested for deleted group: %+v", rev)
		}
	case <-time.After(1 * time.Second):
		// expected: no emission
	}
	if handler.HasPlacementFailedEntry(group.ID) {
		t.Error("placement-failed entry NOT dropped after recovery tick on deleted group")
	}
}

// ---------------------------------------------------------------------
// 11A.17 — 3-node network-foundation integration tests
//
// Per the plan, these validate the DNS + visibility + endpoint registry
// layers without requiring root or CAP_NET_ADMIN. The "3 nodes" are
// modeled as three independent endpoints.Registry instances driven by
// synthetic gossip (DispatchTo helper). A real netdns.Server runs on a
// loopback alias so miekg/dns client probes exercise the full responder
// path. Bridge IPs are synthetic addresses the test assigns.
//
// Note: real VXLAN/IPsec require Linux + root + kernel modules and are
// covered by the lower-level overlay tests; these integration tests
// intentionally exclude those paths to remain runnable on every
// developer host.
// ---------------------------------------------------------------------

// netFoundationNode is one synthetic node's view of the network layer:
// its own endpoint registry, its own DNS server bound on a loopback
// alias, and the bridge identity it serves. The fakeRegistry mirror is
// shared across the 3 nodes by netFoundationFixture so an Insert from
// any node lands on every node's registry — simulating fully-converged
// gossip.
type netFoundationNode struct {
	id          string
	bindAddr    netip.Addr
	dnsPort     int
	clusterPath string
	groupID     string
	registry    *endpoints.Registry
	server      *netdns.Server
}

// netFoundationFixture wires multiple netFoundationNodes plus the
// gossip simulator. Use newNetFoundationFixture to construct; defer
// Stop in the test.
type netFoundationFixture struct {
	t     *testing.T
	nodes []*netFoundationNode
}

// loopbackAddr returns 127.0.0.<offset+1> as a netip.Addr.
// Loopback aliasing in Linux exposes the entire 127.0.0.0/8 block, so
// every offset in 0..253 is bindable on the kernel without admin.
func loopbackAddr(offset int) netip.Addr {
	return netip.MustParseAddr(fmt.Sprintf("127.0.0.%d", offset+1))
}

// newNetFoundationFixture constructs n nodes. Each node gets a
// distinct loopback bind addr and a freshly built registry+DNS server.
// The bridgeMap closure tells the DNS server how to translate a listen
// address into (cluster, group); per-node identity is captured here so
// cross-group tests can assign different (cluster, group) to different
// bind addrs.
//
// Each node's registry has a short sweep cadence + injectable clock so
// tests can deterministically drive TTL eviction.
func newNetFoundationFixture(t *testing.T, identities []netFoundationNode, ttl time.Duration, predicate netdns.VisibilityPredicate) *netFoundationFixture {
	t.Helper()
	f := &netFoundationFixture{t: t}
	// Build addr -> identity map for the bridgeMap closure.
	addrIndex := make(map[netip.Addr]netFoundationNode, len(identities))
	bindAddrs := make([]netip.Addr, 0, len(identities))
	for _, id := range identities {
		addrIndex[id.bindAddr] = id
		bindAddrs = append(bindAddrs, id.bindAddr)
	}
	bridgeMap := func(addr netip.Addr) (string, string, bool) {
		got, ok := addrIndex[addr]
		if !ok {
			return "", "", false
		}
		return got.clusterPath, got.groupID, true
	}
	for i := range identities {
		ident := identities[i]
		reg := endpoints.NewRegistry(
			endpoints.WithSweepInterval(50*time.Millisecond),
		)
		t.Cleanup(reg.Stop)
		srv := netdns.NewServer(
			netdns.WithRegistry(reg),
			netdns.WithVisibility(predicate),
			netdns.WithBridgeMap(bridgeMap),
			netdns.WithBindAddrs([]netip.Addr{ident.bindAddr}),
			netdns.WithPort(0),
			netdns.WithTTLSeconds(2),
		)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := srv.Start(ctx); err != nil {
			cancel()
			t.Fatalf("dns Start (%s): %v", ident.id, err)
		}
		cancel()
		t.Cleanup(func() { _ = srv.Stop() })
		ua := srv.UDPListenAddr(ident.bindAddr)
		if ua == nil {
			t.Fatalf("dns UDPListenAddr(%s) returned nil", ident.bindAddr)
		}
		// Suppress unused-variable warning when ttl is irrelevant. ttl
		// is documented in the helper signature so call sites are clear
		// about the publisher hint they're emulating.
		_ = ttl
		f.nodes = append(f.nodes, &netFoundationNode{
			id:          ident.id,
			bindAddr:    ident.bindAddr,
			dnsPort:     ua.Port,
			clusterPath: ident.clusterPath,
			groupID:     ident.groupID,
			registry:    reg,
			server:      srv,
		})
	}
	return f
}

// publishTo inserts an endpoint record into the named node's registry
// only. publishToAll fans the same record to every registry — that's
// the convergence assumption the gossip layer delivers in steady
// state, expressed directly in the tests.
func (f *netFoundationFixture) publishTo(nodeID string, rec *endpointpb.EndpointRecord) {
	f.t.Helper()
	for _, n := range f.nodes {
		if n.id == nodeID {
			n.registry.Insert(rec)
			return
		}
	}
	f.t.Fatalf("publishTo: unknown node id %q", nodeID)
}

func (f *netFoundationFixture) publishToAll(rec *endpointpb.EndpointRecord) {
	for _, n := range f.nodes {
		n.registry.Insert(rec)
	}
}

// queryFrom issues an A query from the named node against its local
// DNS server (on the bind addr the BridgeMapFunc resolves into that
// node's bridge identity). Returns the response Rcode + answer IPs.
func (f *netFoundationFixture) queryFrom(nodeID, name string) (int, []string) {
	f.t.Helper()
	var node *netFoundationNode
	for _, n := range f.nodes {
		if n.id == nodeID {
			node = n
			break
		}
	}
	if node == nil {
		f.t.Fatalf("queryFrom: unknown node id %q", nodeID)
	}
	c := &miekg.Client{Net: "udp", Timeout: 2 * time.Second}
	m := new(miekg.Msg)
	m.SetQuestion(miekg.Fqdn(name), miekg.TypeA)
	addr := fmt.Sprintf("%s:%d", node.bindAddr, node.dnsPort)
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		f.t.Fatalf("queryFrom(%s, %s) exchange %s: %v", nodeID, name, addr, err)
	}
	ips := make([]string, 0, len(resp.Answer))
	for _, rr := range resp.Answer {
		if a, ok := rr.(*miekg.A); ok {
			ips = append(ips, a.A.String())
		}
	}
	return resp.Rcode, ips
}

// newRecord builds a fully-populated EndpointRecord with the supplied
// SwimState + emittedAt + ttl.
func newRecord(cluster, groupID, capsuleName, replicaID, nodeID, bridgeIP, swim string, emittedAt time.Time, ttl time.Duration) *endpointpb.EndpointRecord {
	return &endpointpb.EndpointRecord{
		ClusterPath: cluster,
		GroupId:     groupID,
		CapsuleName: capsuleName,
		ReplicaId:   replicaID,
		NodeId:      nodeID,
		BridgeIp:    bridgeIP,
		SwimState:   swim,
		EmittedAt:   timestamppb.New(emittedAt),
		TtlSeconds:  int32(ttl.Seconds()),
	}
}

// TestNetworkFoundation_DNS_BareCapsuleResolution (11A.T1) — verifies
// that with a 2-member same-orbit group (api on node1, db on node2),
// the endpoint registry on every node holds records for both members
// and a DNS query for "db" issued from node3 (in the same group) returns
// db's bridge IP on node2.
func TestNetworkFoundation_DNS_BareCapsuleResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/netfound-bare"
	const groupID = "g-bare-1"
	fix := newNetFoundationFixture(t, []netFoundationNode{
		{id: "node1", bindAddr: loopbackAddr(10), clusterPath: cluster, groupID: groupID},
		{id: "node2", bindAddr: loopbackAddr(11), clusterPath: cluster, groupID: groupID},
		{id: "node3", bindAddr: loopbackAddr(12), clusterPath: cluster, groupID: groupID},
	}, 30*time.Second, netdns.DefaultPredicate)

	now := time.Now()
	fix.publishToAll(newRecord(cluster, groupID, "api", "r1", "node1", "10.88.1.10", endpoints.SwimStateAlive, now, 30*time.Second))
	fix.publishToAll(newRecord(cluster, groupID, "db", "r1", "node2", "10.88.1.11", endpoints.SwimStateAlive, now, 30*time.Second))

	// node3 resolves db: should yield 10.88.1.11.
	rcode, ips := fix.queryFrom("node3", "db")
	if rcode != miekg.RcodeSuccess {
		t.Fatalf("rcode = %d want NOERROR", rcode)
	}
	if len(ips) != 1 || ips[0] != "10.88.1.11" {
		t.Fatalf("ips = %v want [10.88.1.11]", ips)
	}

	// node3 resolves api: should yield 10.88.1.10.
	rcode, ips = fix.queryFrom("node3", "api")
	if rcode != miekg.RcodeSuccess {
		t.Fatalf("rcode = %d want NOERROR", rcode)
	}
	if len(ips) != 1 || ips[0] != "10.88.1.10" {
		t.Fatalf("ips = %v want [10.88.1.10]", ips)
	}

	// Registry mirror is gossip-fed; every node sees both records.
	for _, n := range fix.nodes {
		got := n.registry.Snapshot()
		if len(got) != 2 {
			t.Errorf("node %s: registry has %d records, want 2", n.id, len(got))
		}
	}
}

// TestNetworkFoundation_NodeFailure_DNSDropsDeadReplica (11A.T2) — once
// node2's db replica is marked dead, the registry no longer returns it
// from an alive-filtered Lookup, so a subsequent DNS query for "db"
// returns NXDOMAIN.
//
// The publisher TTL is shortened (2s) so the test runs in < 6s — twice
// the TTL is the registry's sweep boundary.
func TestNetworkFoundation_NodeFailure_DNSDropsDeadReplica(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/netfound-fail"
	const groupID = "g-fail-1"
	fix := newNetFoundationFixture(t, []netFoundationNode{
		{id: "node1", bindAddr: loopbackAddr(20), clusterPath: cluster, groupID: groupID},
		{id: "node2", bindAddr: loopbackAddr(21), clusterPath: cluster, groupID: groupID},
		{id: "node3", bindAddr: loopbackAddr(22), clusterPath: cluster, groupID: groupID},
	}, 1*time.Second, netdns.DefaultPredicate)

	now := time.Now()
	fix.publishToAll(newRecord(cluster, groupID, "db", "r1", "node2", "10.88.2.11", endpoints.SwimStateAlive, now, 1*time.Second))

	// Sanity: initial Lookup succeeds.
	rcode, ips := fix.queryFrom("node3", "db")
	if rcode != miekg.RcodeSuccess || len(ips) != 1 {
		t.Fatalf("pre-failure rcode=%d ips=%v want NOERROR with one IP", rcode, ips)
	}

	// Simulate node2 failure via two parallel mechanisms (each
	// representative of a production path):
	//   1. The publisher re-emits the record with SwimState = "dead"
	//      (mirroring the SWIM failure-detector fan-out we emit on
	//      NodeFailed; the alive-only filter then drops it from Lookup).
	//   2. After 2 × TTL the sweeper evicts the record entirely.
	deadAt := now.Add(2 * time.Second)
	fix.publishToAll(newRecord(cluster, groupID, "db", "r1", "node2", "10.88.2.11", "dead", deadAt, 1*time.Second))

	// Within the 2 × TTL window the DNS responder returns NXDOMAIN
	// (alive-filtered registry is empty).
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		rcode, ips = fix.queryFrom("node3", "db")
		if rcode == miekg.RcodeNameError {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rcode != miekg.RcodeNameError {
		t.Fatalf("post-failure rcode=%d ips=%v want NXDOMAIN", rcode, ips)
	}
}

// TestNetworkFoundation_MultiReplicaDistribution (11A.T3) — 3 nodes
// each hosting a replica of "db". DNS query returns all 3 A records.
// 30 sequential queries with randomized shuffle hit every bridge IP at
// least once.
func TestNetworkFoundation_MultiReplicaDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/netfound-multi"
	const groupID = "g-multi-1"
	fix := newNetFoundationFixture(t, []netFoundationNode{
		{id: "node1", bindAddr: loopbackAddr(30), clusterPath: cluster, groupID: groupID},
		{id: "node2", bindAddr: loopbackAddr(31), clusterPath: cluster, groupID: groupID},
		{id: "node3", bindAddr: loopbackAddr(32), clusterPath: cluster, groupID: groupID},
	}, 30*time.Second, netdns.DefaultPredicate)

	now := time.Now()
	want := map[string]struct{}{"10.88.3.10": {}, "10.88.3.11": {}, "10.88.3.12": {}}
	fix.publishToAll(newRecord(cluster, groupID, "db", "r1", "node1", "10.88.3.10", endpoints.SwimStateAlive, now, 30*time.Second))
	fix.publishToAll(newRecord(cluster, groupID, "db", "r2", "node2", "10.88.3.11", endpoints.SwimStateAlive, now, 30*time.Second))
	fix.publishToAll(newRecord(cluster, groupID, "db", "r3", "node3", "10.88.3.12", endpoints.SwimStateAlive, now, 30*time.Second))

	// Single query returns all 3 A records.
	rcode, ips := fix.queryFrom("node1", "db")
	if rcode != miekg.RcodeSuccess || len(ips) != 3 {
		t.Fatalf("rcode=%d ips=%v want NOERROR with 3 IPs", rcode, ips)
	}
	for _, ip := range ips {
		if _, ok := want[ip]; !ok {
			t.Errorf("unexpected ip %s in answer", ip)
		}
	}

	// 30 sequential queries: hit every bridge IP at least once. The
	// responder shuffles A-record order per response, so the first
	// answer's distribution stands in for the connect-the-first-IP
	// behavior most resolvers use.
	hits := map[string]int{}
	for i := 0; i < 30; i++ {
		_, got := fix.queryFrom("node1", "db")
		if len(got) == 0 {
			continue
		}
		hits[got[0]]++
	}
	for ip := range want {
		if hits[ip] == 0 {
			t.Errorf("ip %s never appeared as first answer across 30 queries: hits=%v", ip, hits)
		}
	}
}

// TestNetworkFoundation_CrossGroupIsolation (11A.T4) — two groups (A
// and B) each have a "db" member with different bridge IPs. From group
// A's bridge, DNS returns A's db only. A caller forced to look up B's
// db is denied by the visibility predicate.
//
// The test wires three nodes: two in group A (one of them serves as
// the caller), one in group B. A's caller resolves "db" → A's bridge
// IP. The cross-group attempt uses an instrumented predicate that
// shows visibility denies the query even though the record exists in
// the same registry.
func TestNetworkFoundation_CrossGroupIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const cluster = "test/dc1/netfound-iso"
	const groupA = "g-a"
	const groupB = "g-b"
	fix := newNetFoundationFixture(t, []netFoundationNode{
		{id: "a-caller", bindAddr: loopbackAddr(40), clusterPath: cluster, groupID: groupA},
		{id: "a-host", bindAddr: loopbackAddr(41), clusterPath: cluster, groupID: groupA},
		{id: "b-host", bindAddr: loopbackAddr(42), clusterPath: cluster, groupID: groupB},
	}, 30*time.Second, netdns.DefaultPredicate)

	now := time.Now()
	// Both groups have a "db" member with distinct bridge IPs.
	fix.publishToAll(newRecord(cluster, groupA, "db", "r1", "a-host", "10.88.40.10", endpoints.SwimStateAlive, now, 30*time.Second))
	fix.publishToAll(newRecord(cluster, groupB, "db", "r1", "b-host", "10.88.41.10", endpoints.SwimStateAlive, now, 30*time.Second))

	// Group A's caller resolves "db" → A's bridge IP only. The
	// responder's Lookup is scoped to caller.GroupID; B's record is
	// in the same registry but a different group, so it is invisible.
	rcode, ips := fix.queryFrom("a-caller", "db")
	if rcode != miekg.RcodeSuccess {
		t.Fatalf("a-caller rcode=%d want NOERROR", rcode)
	}
	if len(ips) != 1 || ips[0] != "10.88.40.10" {
		t.Fatalf("a-caller ips=%v want [10.88.40.10] (must NEVER see B's 10.88.41.10)", ips)
	}

	// b-host's caller resolves "db" → B's bridge IP only.
	rcode, ips = fix.queryFrom("b-host", "db")
	if rcode != miekg.RcodeSuccess {
		t.Fatalf("b-host rcode=%d want NOERROR", rcode)
	}
	if len(ips) != 1 || ips[0] != "10.88.41.10" {
		t.Fatalf("b-host ips=%v want [10.88.41.10]", ips)
	}

	// Direct visibility-predicate check: caller in group A, target in
	// group B → false. Documents the surface the server applies.
	if netdns.DefaultPredicate(netdns.BridgeInfo{ClusterPath: cluster, GroupID: groupA}, groupB) {
		t.Error("DefaultPredicate must deny cross-group queries (A → B)")
	}
}
