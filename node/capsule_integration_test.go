package node

import (
	"context"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/enums"
)

// joinOrbitOrFail subscribes the node to the named orbit on a cluster, failing the test on error.
// Uses a long-lived context so the orbit message loop continues for the duration of the test.
func joinOrbitOrFail(t *testing.T, n *Node, cluster, orbit string) {
	t.Helper()
	// Use context.Background so the orbit message loop is not tied to a short timeout.
	// The handler's own Stop() will cancel it when the node shuts down.
	if err := n.CapsuleHandler().JoinOrbit(context.Background(), cluster, orbit); err != nil {
		t.Fatalf("node %s failed to join orbit %s: %v", n.Name(), orbit, err)
	}
}

// waitForCapsule polls until a capsule with the given name appears, or fails the test.
func waitForCapsule(t *testing.T, n *Node, name string, timeout time.Duration) *capsule.Capsule {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c := n.Capsules().GetByName(name); c != nil {
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("capsule %q not found on node %s within %v", name, n.Name(), timeout)
	return nil
}

// TestCapsuleLifecycle_3Node tests capsule creation and propagation across a 3-node cluster.
// Node1 creates a capsule; nodes 2 and 3 should receive it via orbit PubSub.
func TestCapsuleLifecycle_3Node(t *testing.T) {
	const clusterPath = "test/dc1/capsule-3node"

	n1 := testNode(t, "cap-n1", 0)
	n2 := testNode(t, "cap-n2", 0)
	n3 := testNode(t, "cap-n3", 0)

	joinCluster(t, n1, clusterPath)

	bootstrap := getBootstrapAddr(n1)
	if bootstrap == "" {
		t.Fatal("failed to get bootstrap address")
	}

	joinCluster(t, n2, clusterPath, bootstrap)
	joinCluster(t, n3, clusterPath, bootstrap)

	// Wait for phonebook propagation
	waitForPhonebookCount(t, n1, clusterPath, 3, 10*time.Second)
	waitForPhonebookCount(t, n2, clusterPath, 3, 10*time.Second)
	waitForPhonebookCount(t, n3, clusterPath, 3, 10*time.Second)

	// All nodes subscribe to the "api" orbit
	joinOrbitOrFail(t, n1, clusterPath, "api")
	joinOrbitOrFail(t, n2, clusterPath, "api")
	joinOrbitOrFail(t, n3, clusterPath, "api")

	// Give GossipSub mesh time to form for the orbit topic.
	// Mesh formation in libp2p takes a few heartbeat cycles after subscribing.
	time.Sleep(3 * time.Second)

	// Create a capsule on node1
	spec := capsule.CapsuleSpec{
		Name:  "test-api",
		Image: "registry.test/api:v1",
		Orbit: "api",
		Tier:  enums.TierEnum.Standard(),
		Labels: capsule.Labels{
			"app":  "test-api",
			"team": "backend",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	created, err := n1.Capsules().Create(ctx, clusterPath, spec)
	if err != nil {
		t.Fatalf("failed to create capsule: %v", err)
	}

	// Verify node1 has it locally
	if n1.Capsules().GetByName("test-api") == nil {
		t.Error("node1 should have the capsule locally")
	}

	// Wait for nodes 2 and 3 to receive it via orbit announcement
	c2 := waitForCapsule(t, n2, "test-api", 10*time.Second)
	c3 := waitForCapsule(t, n3, "test-api", 10*time.Second)

	if c2.Spec.Name != "test-api" || c2.Spec.Image != "registry.test/api:v1" {
		t.Errorf("node2 received wrong capsule spec: %+v", c2.Spec)
	}
	if c3.Spec.Name != "test-api" || c3.Spec.Image != "registry.test/api:v1" {
		t.Errorf("node3 received wrong capsule spec: %+v", c3.Spec)
	}
	if c2.Spec.Labels["team"] != "backend" {
		t.Errorf("node2 missing labels: %+v", c2.Spec.Labels)
	}
	if string(c2.ID) != string(created.ID) {
		t.Errorf("node2 ID mismatch: got %s, want %s", c2.ID, created.ID)
	}
	if c2.ClusterID != clusterPath {
		t.Errorf("node2 cluster ID mismatch: got %s, want %s", c2.ClusterID, clusterPath)
	}
}

// TestCapsuleWithdrawal_3Node tests that capsule deletion propagates across the cluster.
func TestCapsuleWithdrawal_3Node(t *testing.T) {
	const clusterPath = "test/dc1/capsule-withdraw"

	n1 := testNode(t, "w-n1", 0)
	n2 := testNode(t, "w-n2", 0)
	n3 := testNode(t, "w-n3", 0)

	joinCluster(t, n1, clusterPath)
	bootstrap := getBootstrapAddr(n1)
	joinCluster(t, n2, clusterPath, bootstrap)
	joinCluster(t, n3, clusterPath, bootstrap)

	waitForPhonebookCount(t, n1, clusterPath, 3, 10*time.Second)
	waitForPhonebookCount(t, n2, clusterPath, 3, 10*time.Second)
	waitForPhonebookCount(t, n3, clusterPath, 3, 10*time.Second)

	joinOrbitOrFail(t, n1, clusterPath, "api")
	joinOrbitOrFail(t, n2, clusterPath, "api")
	joinOrbitOrFail(t, n3, clusterPath, "api")

	// Give GossipSub mesh time to form for the orbit topic.
	time.Sleep(3 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	created, err := n1.Capsules().Create(ctx, clusterPath, capsule.CapsuleSpec{
		Name:  "temp-capsule",
		Image: "img:v1",
		Orbit: "api",
	})
	if err != nil {
		t.Fatalf("failed to create capsule: %v", err)
	}

	waitForCapsule(t, n2, "temp-capsule", 10*time.Second)
	waitForCapsule(t, n3, "temp-capsule", 10*time.Second)

	if err := n1.Capsules().Delete(ctx, created.ID); err != nil {
		t.Fatalf("failed to delete capsule: %v", err)
	}

	// Wait for withdrawal to propagate
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if n2.Capsules().GetByName("temp-capsule") == nil &&
			n3.Capsules().GetByName("temp-capsule") == nil {
			return // Success
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Error("capsule not removed from nodes 2 and 3 after withdrawal")
}
