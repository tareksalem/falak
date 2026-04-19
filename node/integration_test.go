package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/node/phonebook"
)

// testPSK is a 32-byte PSK for testing.
var testPSK = []byte("integration-test-psk-32bytes!!")

func init() {
	// Pad to 32 bytes
	for len(testPSK) < 32 {
		testPSK = append(testPSK, '0')
	}
}

// testNode creates and starts a node with fast timing for integration testing.
func testNode(t *testing.T, name string, port int) *Node {
	t.Helper()

	dataDir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}

	logger, _ := zap.NewDevelopment()

	n := New(
		WithName(name),
		WithListenAddrs(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port)),
		WithDataDir(dataDir),
		WithLogger(logger.Named(name)),
	)

	if err := n.Start(); err != nil {
		t.Fatalf("failed to start %s: %v", name, err)
	}

	t.Cleanup(func() {
		n.Stop()
	})

	return n
}

// joinCluster joins a node to a cluster, optionally with bootstrap peers.
func joinCluster(t *testing.T, n *Node, cluster string, bootstrapAddrs ...string) {
	t.Helper()

	cfg := ClusterConfig{
		Path:           cluster,
		PSK:            append([]byte{}, testPSK...), // copy since PSK gets zeroed
		BootstrapPeers: bootstrapAddrs,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := n.Join(ctx, cfg); err != nil {
		t.Fatalf("node %s failed to join %s: %v", n.Name(), cluster, err)
	}
}

// waitForPhonebookCount waits until a node's phonebook has the expected number of entries for a cluster.
func waitForPhonebookCount(t *testing.T, n *Node, cluster string, expected int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := n.Phonebook().GetByCluster(cluster)
		if err == nil && len(entries) == expected {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	entries, _ := n.Phonebook().GetByCluster(cluster)
	t.Fatalf("node %s: expected %d phonebook entries for %s, got %d (timeout %v)",
		n.Name(), expected, cluster, len(entries), timeout)
}

// waitForNodeRemoved waits until a specific node is removed from another node's phonebook.
func waitForNodeRemoved(t *testing.T, observer *Node, targetNodeID, cluster string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		exists, err := observer.Phonebook().Exists(targetNodeID, cluster)
		if err == nil && !exists {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("node %s: expected node %s to be removed from phonebook for %s (timeout %v)",
		observer.Name(), targetNodeID, cluster, timeout)
}

// waitForNodeStatus waits until a node has a specific status in another node's phonebook.
func waitForNodeStatus(t *testing.T, observer *Node, targetNodeID, cluster string, status phonebook.NodeStatus, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entry, err := observer.Phonebook().Get(targetNodeID, cluster)
		if err == nil && entry != nil && entry.Status == status {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	entry, _ := observer.Phonebook().Get(targetNodeID, cluster)
	var got phonebook.NodeStatus
	if entry != nil {
		got = entry.Status
	}
	t.Fatalf("node %s: expected node %s status=%s, got=%s (timeout %v)",
		observer.Name(), targetNodeID, status, got, timeout)
}

// getBootstrapAddr returns the first full multiaddr for a node.
func getBootstrapAddr(n *Node) string {
	addrs := n.Addrs()
	if len(addrs) == 0 {
		return ""
	}
	return addrs[0]
}

// --- Tests ---

func TestSingleNodeCluster(t *testing.T) {
	n1 := testNode(t, "solo1", 0)
	joinCluster(t, n1, "test/solo")

	// Single node should have itself in phonebook
	waitForPhonebookCount(t, n1, "test/solo", 1, 5*time.Second)

	// Verify it's us
	entries, _ := n1.Phonebook().GetByCluster("test/solo")
	if entries[0].NodeID != n1.ID().String() {
		t.Errorf("expected own node ID in phonebook, got %s", entries[0].NodeID)
	}

	// Verify joined clusters
	clusters := n1.JoinedClusters()
	if len(clusters) != 1 || clusters[0] != "test/solo" {
		t.Errorf("expected [test/solo], got %v", clusters)
	}
}

func TestTwoNodeCluster_JoinAndDrop(t *testing.T) {
	n1 := testNode(t, "pair1", 0)
	joinCluster(t, n1, "test/pair")

	n2 := testNode(t, "pair2", 0)
	joinCluster(t, n2, "test/pair", getBootstrapAddr(n1))

	// Both nodes should see 2 entries
	waitForPhonebookCount(t, n1, "test/pair", 2, 15*time.Second)
	waitForPhonebookCount(t, n2, "test/pair", 2, 15*time.Second)

	// Verify both see each other
	exists, _ := n1.Phonebook().Exists(n2.ID().String(), "test/pair")
	if !exists {
		t.Error("n1 doesn't see n2 in phonebook")
	}
	exists, _ = n2.Phonebook().Exists(n1.ID().String(), "test/pair")
	if !exists {
		t.Error("n2 doesn't see n1 in phonebook")
	}

	// Kill n2
	n2ID := n2.ID().String()
	n2.Stop()

	// n1 should detect n2 is gone and remove it
	// With fast timing (2-node cluster, 0.3 scale): ~22s worst case
	waitForNodeRemoved(t, n1, n2ID, "test/pair", 45*time.Second)

	// n1 phonebook should now have only itself
	entries, _ := n1.Phonebook().GetByCluster("test/pair")
	if len(entries) != 1 {
		t.Errorf("expected 1 entry after drop, got %d", len(entries))
	}
}

func TestThreeNodeCluster_DropOne(t *testing.T) {
	n1 := testNode(t, "tri1", 0)
	joinCluster(t, n1, "test/tri")

	n2 := testNode(t, "tri2", 0)
	joinCluster(t, n2, "test/tri", getBootstrapAddr(n1))

	// Wait for n2 to be visible before n3 joins
	waitForPhonebookCount(t, n1, "test/tri", 2, 15*time.Second)

	n3 := testNode(t, "tri3", 0)
	joinCluster(t, n3, "test/tri", getBootstrapAddr(n1))

	// All should see 3 entries
	waitForPhonebookCount(t, n1, "test/tri", 3, 15*time.Second)
	waitForPhonebookCount(t, n2, "test/tri", 3, 15*time.Second)
	waitForPhonebookCount(t, n3, "test/tri", 3, 15*time.Second)

	// Kill n3
	n3ID := n3.ID().String()
	n3.Stop()

	// n1 and n2 should detect n3 gone
	waitForNodeRemoved(t, n1, n3ID, "test/tri", 60*time.Second)
	waitForNodeRemoved(t, n2, n3ID, "test/tri", 60*time.Second)

	// Should have 2 entries remaining
	entries1, _ := n1.Phonebook().GetByCluster("test/tri")
	entries2, _ := n2.Phonebook().GetByCluster("test/tri")
	if len(entries1) != 2 {
		t.Errorf("n1: expected 2 entries, got %d", len(entries1))
	}
	if len(entries2) != 2 {
		t.Errorf("n2: expected 2 entries, got %d", len(entries2))
	}
}

func TestFiveNodeCluster_DropTwo(t *testing.T) {
	nodes := make([]*Node, 5)
	cluster := "test/five"

	// Start first node
	nodes[0] = testNode(t, "five1", 0)
	joinCluster(t, nodes[0], cluster)

	// Start remaining nodes
	bootstrap := getBootstrapAddr(nodes[0])
	for i := 1; i < 5; i++ {
		nodes[i] = testNode(t, fmt.Sprintf("five%d", i+1), 0)
		joinCluster(t, nodes[i], cluster, bootstrap)
		// Wait a bit between joins to avoid overwhelming
		time.Sleep(500 * time.Millisecond)
	}

	// All should see 5 entries
	for i, n := range nodes {
		waitForPhonebookCount(t, n, cluster, 5, 30*time.Second)
		t.Logf("node %d (%s) sees 5 members", i+1, n.Name())
	}

	// Kill nodes 4 and 5
	n4ID := nodes[3].ID().String()
	n5ID := nodes[4].ID().String()
	nodes[3].Stop()
	nodes[4].Stop()

	// Remaining 3 nodes should detect both are gone
	for i := 0; i < 3; i++ {
		waitForNodeRemoved(t, nodes[i], n4ID, cluster, 90*time.Second)
		waitForNodeRemoved(t, nodes[i], n5ID, cluster, 90*time.Second)
		t.Logf("node %d confirmed removal of nodes 4,5", i+1)
	}

	// Should have 3 entries remaining
	for i := 0; i < 3; i++ {
		entries, _ := nodes[i].Phonebook().GetByCluster(cluster)
		if len(entries) != 3 {
			t.Errorf("node %d: expected 3 entries, got %d", i+1, len(entries))
		}
	}
}

func TestTenNodeCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 10-node test in short mode")
	}

	nodes := make([]*Node, 10)
	cluster := "test/ten"

	nodes[0] = testNode(t, "ten1", 0)
	joinCluster(t, nodes[0], cluster)

	bootstrap := getBootstrapAddr(nodes[0])
	for i := 1; i < 10; i++ {
		nodes[i] = testNode(t, fmt.Sprintf("ten%d", i+1), 0)
		joinCluster(t, nodes[i], cluster, bootstrap)
		time.Sleep(300 * time.Millisecond)
	}

	// All should see 10 entries (give more time for larger cluster)
	for i, n := range nodes {
		waitForPhonebookCount(t, n, cluster, 10, 60*time.Second)
		t.Logf("node %d sees 10 members", i+1)
	}

	// Kill 3 nodes (7, 8, 9)
	droppedIDs := make([]string, 3)
	for i := 7; i < 10; i++ {
		droppedIDs[i-7] = nodes[i].ID().String()
		nodes[i].Stop()
	}

	// Remaining 7 nodes should detect all 3 are gone
	for i := 0; i < 7; i++ {
		for _, droppedID := range droppedIDs {
			waitForNodeRemoved(t, nodes[i], droppedID, cluster, 120*time.Second)
		}
		t.Logf("node %d confirmed all removals", i+1)
	}

	// Should have 7 entries remaining
	for i := 0; i < 7; i++ {
		entries, _ := nodes[i].Phonebook().GetByCluster(cluster)
		if len(entries) != 7 {
			t.Errorf("node %d: expected 7 entries, got %d", i+1, len(entries))
		}
	}
}

func TestMultiCluster_Isolation(t *testing.T) {
	// Node1 joins cluster A and cluster B
	// Node2 joins cluster A only
	// Node3 joins cluster B only
	n1 := testNode(t, "multi1", 0)
	joinCluster(t, n1, "test/clusterA")
	joinCluster(t, n1, "test/clusterB")

	n2 := testNode(t, "multi2", 0)
	joinCluster(t, n2, "test/clusterA", getBootstrapAddr(n1))

	n3 := testNode(t, "multi3", 0)
	joinCluster(t, n3, "test/clusterB", getBootstrapAddr(n1))

	// n1 should see 2 in clusterA (self + n2), 2 in clusterB (self + n3)
	waitForPhonebookCount(t, n1, "test/clusterA", 2, 15*time.Second)
	waitForPhonebookCount(t, n1, "test/clusterB", 2, 15*time.Second)

	// n2 should see 2 in clusterA, nothing in clusterB
	waitForPhonebookCount(t, n2, "test/clusterA", 2, 15*time.Second)
	entriesB, _ := n2.Phonebook().GetByCluster("test/clusterB")
	if len(entriesB) != 0 {
		t.Errorf("n2 should not see clusterB entries, got %d", len(entriesB))
	}

	// n3 should see 2 in clusterB, nothing in clusterA
	waitForPhonebookCount(t, n3, "test/clusterB", 2, 15*time.Second)
	entriesA, _ := n3.Phonebook().GetByCluster("test/clusterA")
	if len(entriesA) != 0 {
		t.Errorf("n3 should not see clusterA entries, got %d", len(entriesA))
	}

	// n1 joined clusters should be both
	clusters := n1.JoinedClusters()
	if len(clusters) != 2 {
		t.Errorf("n1 expected 2 joined clusters, got %d", len(clusters))
	}

	// Leave clusterA — should NOT affect clusterB
	if err := n1.Leave("test/clusterA"); err != nil {
		t.Fatalf("n1 failed to leave clusterA: %v", err)
	}

	// n1 should still be in clusterB
	clusters = n1.JoinedClusters()
	if len(clusters) != 1 {
		t.Fatalf("n1 expected 1 joined cluster after leave, got %d: %v", len(clusters), clusters)
	}
	if clusters[0] != "test/clusterB" {
		t.Errorf("n1 expected clusterB, got %s", clusters[0])
	}

	// n1's clusterB phonebook should still have 2 entries
	entriesB, _ = n1.Phonebook().GetByCluster("test/clusterB")
	if len(entriesB) != 2 {
		t.Errorf("n1 clusterB should still have 2 entries after leaving clusterA, got %d", len(entriesB))
	}

	// Kill n3 — n1 should detect it gone from clusterB
	n3ID := n3.ID().String()
	n3.Stop()

	waitForNodeRemoved(t, n1, n3ID, "test/clusterB", 45*time.Second)
	t.Log("n1 detected n3 removal from clusterB after leaving clusterA — isolation confirmed")
}
