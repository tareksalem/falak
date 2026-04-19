package node

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/node/internal/events"
)

func TestFourNodePubSubDelivery(t *testing.T) {
	cluster := "test/pubsub-delivery"
	nodes := make([]*Node, 4)

	nodes[0] = testNode(t, "pd1", 0)
	joinCluster(t, nodes[0], cluster)

	bootstrap := getBootstrapAddr(nodes[0])

	for i := 1; i < 4; i++ {
		nodes[i] = testNode(t, fmt.Sprintf("pd%d", i+1), 0)
		joinCluster(t, nodes[i], cluster, bootstrap)
		time.Sleep(500 * time.Millisecond)
	}

	for i, n := range nodes {
		waitForPhonebookCount(t, n, cluster, 4, 30*time.Second)
		t.Logf("node %d sees 4 members", i+1)
	}

	time.Sleep(3 * time.Second)

	var mu sync.Mutex
	type probeEvent struct {
		TargetNodeID string
		Success      bool
	}
	probeResults := make(map[string][]probeEvent)

	for i, n := range nodes {
		nodeName := fmt.Sprintf("pd%d", i+1)
		ch := n.eventBus.Subscribe(events.TypeNodeProbeResult)
		go func(name string, ch <-chan events.Event) {
			for evt := range ch {
				pr, ok := evt.(events.NodeProbeResult)
				if !ok {
					continue
				}
				mu.Lock()
				probeResults[name] = append(probeResults[name], probeEvent{
					TargetNodeID: pr.NodeID,
					Success:      pr.Success,
				})
				mu.Unlock()
			}
		}(nodeName, ch)
	}

	time.Sleep(12 * time.Second)

	mu.Lock()
	t.Logf("\n=== Probe Results ===")
	for nodeName, probes := range probeResults {
		successCount := 0
		targets := make(map[string]int)
		for _, p := range probes {
			if p.Success {
				successCount++
			}
			targets[p.TargetNodeID]++
		}
		t.Logf("Node %s: %d probes, %d successful, targets: %v", nodeName, len(probes), successCount, targets)

		if len(probes) == 0 {
			t.Errorf("node %s emitted zero probe results", nodeName)
		}
	}
	mu.Unlock()

	// Kill node4
	n4ID := nodes[3].ID().String()
	nodes[3].Stop()

	t.Logf("\nKilled node4 (%s), waiting for removal...", n4ID[:12])

	for i := 0; i < 3; i++ {
		waitForNodeRemoved(t, nodes[i], n4ID, cluster, 45*time.Second)
		t.Logf("node pd%d confirmed node4 removal", i+1)
	}

	for i := 0; i < 3; i++ {
		entries, _ := nodes[i].Phonebook().GetByCluster(cluster)
		nodeIDs := make([]string, len(entries))
		for j, e := range entries {
			nodeIDs[j] = e.NodeID[:12]
		}
		t.Logf("node pd%d phonebook: %v", i+1, nodeIDs)
		if len(entries) != 3 {
			t.Errorf("node pd%d: expected 3 entries, got %d", i+1, len(entries))
		}
	}

	t.Log("\nAll 3 remaining nodes have consistent phonebooks — PubSub delivery confirmed")
}

func TestFourNode_DropTwoSimultaneously(t *testing.T) {
	cluster := "test/drop-two"
	nodes := make([]*Node, 4)

	nodes[0] = testNode(t, "dt1", 0)
	joinCluster(t, nodes[0], cluster)

	bootstrap := getBootstrapAddr(nodes[0])

	for i := 1; i < 4; i++ {
		nodes[i] = testNode(t, fmt.Sprintf("dt%d", i+1), 0)
		joinCluster(t, nodes[i], cluster, bootstrap)
		time.Sleep(500 * time.Millisecond)
	}

	// All 4 should see each other
	for i, n := range nodes {
		waitForPhonebookCount(t, n, cluster, 4, 30*time.Second)
		t.Logf("node %d sees 4 members", i+1)
	}

	// Let health stabilize
	time.Sleep(5 * time.Second)

	// Kill nodes 3 and 4 simultaneously
	n3ID := nodes[2].ID().String()
	n4ID := nodes[3].ID().String()
	t.Logf("\nKilling node3 (%s) and node4 (%s) simultaneously...", n3ID[:12], n4ID[:12])
	nodes[2].Stop()
	nodes[3].Stop()

	// Remaining nodes (1 and 2) should detect both are gone
	waitForNodeRemoved(t, nodes[0], n3ID, cluster, 60*time.Second)
	t.Log("node dt1 confirmed node3 removal")
	waitForNodeRemoved(t, nodes[0], n4ID, cluster, 60*time.Second)
	t.Log("node dt1 confirmed node4 removal")

	waitForNodeRemoved(t, nodes[1], n3ID, cluster, 60*time.Second)
	t.Log("node dt2 confirmed node3 removal")
	waitForNodeRemoved(t, nodes[1], n4ID, cluster, 60*time.Second)
	t.Log("node dt2 confirmed node4 removal")

	// Both remaining nodes should have exactly 2 entries
	for i := 0; i < 2; i++ {
		entries, _ := nodes[i].Phonebook().GetByCluster(cluster)
		nodeIDs := make([]string, len(entries))
		for j, e := range entries {
			nodeIDs[j] = e.NodeID[:12]
		}
		t.Logf("node dt%d phonebook: %v", i+1, nodeIDs)
		if len(entries) != 2 {
			t.Errorf("node dt%d: expected 2 entries, got %d", i+1, len(entries))
		}

		// Verify dropped nodes are NOT in phonebook
		for _, e := range entries {
			if e.NodeID == n3ID || e.NodeID == n4ID {
				t.Errorf("node dt%d: dropped node still in phonebook: %s", i+1, e.NodeID[:12])
			}
		}
	}

	// Verify the two remaining nodes still see each other
	exists, _ := nodes[0].Phonebook().Exists(nodes[1].ID().String(), cluster)
	if !exists {
		t.Error("dt1 doesn't see dt2")
	}
	exists, _ = nodes[1].Phonebook().Exists(nodes[0].ID().String(), cluster)
	if !exists {
		t.Error("dt2 doesn't see dt1")
	}

	t.Log("\nBoth remaining nodes have consistent phonebooks after simultaneous drop")
}
