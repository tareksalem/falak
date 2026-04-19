package node

import (
	"testing"
	"time"
)

// TestMetrics_LocalSamplingFlows verifies that a single node samples its
// own host metrics, persists them, and produces a non-empty local
// snapshot within a few sampling intervals.
func TestMetrics_LocalSamplingFlows(t *testing.T) {
	const cluster = "test/dc1/metrics-local"

	n := testNode(t, "m1", 0)
	joinCluster(t, n, cluster)

	mgr := n.Metrics()
	if mgr == nil {
		t.Fatal("metrics manager should be initialized after Start")
	}

	// Default sampling interval is 1s; give it a couple of cycles.
	deadline := time.Now().Add(5 * time.Second)
	var snap interface{}
	for time.Now().Before(deadline) {
		s, err := mgr.LocalLatest()
		if err != nil {
			t.Fatalf("LocalLatest failed: %v", err)
		}
		if !s.CapturedAt.IsZero() {
			snap = s
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if snap == nil {
		t.Fatal("metrics manager produced no local snapshots within 5s")
	}

	// History should grow but stay within the rolling window.
	history, err := mgr.LocalHistory()
	if err != nil {
		t.Fatalf("LocalHistory failed: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("LocalHistory returned empty after sampling")
	}
}

// TestMetrics_GossipPropagatesAcross3Nodes verifies that three nodes
// in the same cluster see each other's resource snapshots in their
// peer_latest tables within a few sampling intervals.
func TestMetrics_GossipPropagatesAcross3Nodes(t *testing.T) {
	const cluster = "test/dc1/metrics-gossip"

	n1 := testNode(t, "mg1", 0)
	n2 := testNode(t, "mg2", 0)
	n3 := testNode(t, "mg3", 0)

	joinCluster(t, n1, cluster)
	bootstrap := getBootstrapAddr(n1)
	joinCluster(t, n2, cluster, bootstrap)
	joinCluster(t, n3, cluster, bootstrap)

	waitForPhonebookCount(t, n1, cluster, 3, 10*time.Second)
	waitForPhonebookCount(t, n2, cluster, 3, 10*time.Second)
	waitForPhonebookCount(t, n3, cluster, 3, 10*time.Second)

	// Each node samples every 1s and publishes immediately. Give the
	// gossip mesh time to form (the existing health topic) and at
	// least a couple of sample cycles.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		all1, _ := n1.Metrics().AllPeers()
		all2, _ := n2.Metrics().AllPeers()
		all3, _ := n3.Metrics().AllPeers()
		if len(all1) >= 2 && len(all2) >= 2 && len(all3) >= 2 {
			return // success
		}
		time.Sleep(500 * time.Millisecond)
	}

	all1, _ := n1.Metrics().AllPeers()
	all2, _ := n2.Metrics().AllPeers()
	all3, _ := n3.Metrics().AllPeers()
	t.Fatalf("metrics gossip did not propagate within 15s: n1 sees %d peers, n2 sees %d, n3 sees %d",
		len(all1), len(all2), len(all3))
}
