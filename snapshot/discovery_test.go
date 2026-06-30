package snapshot

import (
	"testing"

	"go.uber.org/zap"
)

// newIndexOnlyDiscovery builds a Discovery with just the in-memory index
// populated, bypassing the host/pubsub wiring. Sufficient for testing the
// index-mutating methods (addToIndex, FindHolders, PruneNode) in isolation.
func newIndexOnlyDiscovery() *Discovery {
	return &Discovery{
		logger: zap.NewNop(),
		index:  make(map[indexKey][]indexEntry),
	}
}

func TestDiscovery_AddToIndexGainsHolder(t *testing.T) {
	d := newIndexOnlyDiscovery()
	d.addToIndex(SnapshotAvailable{CapsuleID: "cap1", Tag: "v1", NodeID: "nodeB", Size: 10, Checksum: "c"})

	holders := d.FindHolders("cap1", "v1")
	if len(holders) != 1 || holders[0] != "nodeB" {
		t.Fatalf("index should contain nodeB after a re-broadcast, got %v", holders)
	}
}

// TestDiscovery_PruneNode is the load-bearing reconciliation test: when a
// holder fails or departs, PruneNode must remove every index entry it held
// across all keys so the puller never targets the dead node and cold-starts
// despite replication.
func TestDiscovery_PruneNode(t *testing.T) {
	d := newIndexOnlyDiscovery()

	// Two snapshots, each held by nodeA and nodeB. nodeA also holds a third.
	d.addToIndex(SnapshotAvailable{CapsuleID: "cap1", Tag: "v1", NodeID: "nodeA"})
	d.addToIndex(SnapshotAvailable{CapsuleID: "cap1", Tag: "v1", NodeID: "nodeB"})
	d.addToIndex(SnapshotAvailable{CapsuleID: "cap2", Tag: "v1", NodeID: "nodeA"})
	d.addToIndex(SnapshotAvailable{CapsuleID: "cap2", Tag: "v1", NodeID: "nodeB"})
	d.addToIndex(SnapshotAvailable{CapsuleID: "cap3", Tag: "v1", NodeID: "nodeA"})

	pruned := d.PruneNode("nodeA")
	if pruned != 3 {
		t.Fatalf("expected 3 entries pruned for nodeA, got %d", pruned)
	}

	// cap1 and cap2 must now only show nodeB; nodeA must be gone everywhere.
	for _, c := range []string{"cap1", "cap2"} {
		holders := d.FindHolders(c, "v1")
		if len(holders) != 1 || holders[0] != "nodeB" {
			t.Errorf("%s holders = %v, want [nodeB]", c, holders)
		}
	}
	// cap3 had only nodeA — the key must be dropped entirely.
	if h := d.FindHolders("cap3", "v1"); len(h) != 0 {
		t.Errorf("cap3 should have no holders after pruning nodeA, got %v", h)
	}

	// Idempotent: pruning again removes nothing.
	if again := d.PruneNode("nodeA"); again != 0 {
		t.Errorf("re-pruning nodeA should remove 0, got %d", again)
	}
	// Pruning a never-seen node is a no-op.
	if z := d.PruneNode("ghost"); z != 0 {
		t.Errorf("pruning unknown node should remove 0, got %d", z)
	}
}
