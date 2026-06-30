package node

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
)

// fakePruner records PruneNode calls and signals each on a channel.
type fakePruner struct {
	mu     sync.Mutex
	pruned []string
	fired  chan string
}

func newFakePruner() *fakePruner { return &fakePruner{fired: make(chan string, 8)} }

func (f *fakePruner) PruneNode(nodeID string) int {
	f.mu.Lock()
	f.pruned = append(f.pruned, nodeID)
	f.mu.Unlock()
	f.fired <- nodeID
	return 1
}

// TestSnapshotReconciler_PrunesOnMembershipLoss is the load-bearing node-level
// wiring test: a NodeFailed and a NodeDeparting event must each drive a
// PruneNode for the lost node so the puller stops targeting a dead holder.
func TestSnapshotReconciler_PrunesOnMembershipLoss(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	pr := newFakePruner()

	rec := newSnapshotReconciler(bus, pr, zap.NewNop())
	rec.Start(context.Background())
	defer rec.Stop()

	bus.Publish(events.NodeFailed{BaseEvent: events.NewBaseEvent(), NodeID: "deadNode", ClusterPath: "c"})
	select {
	case id := <-pr.fired:
		if id != "deadNode" {
			t.Fatalf("pruned %q, want deadNode", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("NodeFailed did not trigger PruneNode")
	}

	bus.Publish(events.NodeDeparting{BaseEvent: events.NewBaseEvent(), NodeID: "leavingNode"})
	select {
	case id := <-pr.fired:
		if id != "leavingNode" {
			t.Fatalf("pruned %q, want leavingNode", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("NodeDeparting did not trigger PruneNode")
	}
}

func mustPeerID(t *testing.T) string {
	t.Helper()
	_, pub, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	id, err := peer.IDFromPublicKey(pub)
	if err != nil {
		t.Fatalf("peer id: %v", err)
	}
	return id.String()
}

// TestSnapshotCandidateProvider_MapsPhonebook proves the provider maps
// phonebook entries to candidates: excludes self, surfaces Active status,
// failure domain, and capabilities-derived free disk.
func TestSnapshotCandidateProvider_MapsPhonebook(t *testing.T) {
	pb, err := phonebook.Open(filepath.Join(t.TempDir(), "pb.db"))
	if err != nil {
		t.Fatalf("open phonebook: %v", err)
	}
	defer pb.Close()

	const cluster = "test/dc/prod"
	self := mustPeerID(t)
	peerA := mustPeerID(t)
	peerB := mustPeerID(t)

	add := func(id, dc string, diskGB int64, status phonebook.NodeStatus) {
		e := &phonebook.Entry{
			NodeID:       id,
			ClusterPath:  cluster,
			Datacenter:   dc,
			Status:       status,
			Capabilities: &phonebook.Capabilities{DiskGB: diskGB},
		}
		if err := pb.Add(e); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	add(self, "dc-self", 100, phonebook.NodeStatusEnum.Active())
	add(peerA, "dc-a", 50, phonebook.NodeStatusEnum.Active())
	add(peerB, "dc-b", 10, phonebook.NodeStatusEnum.Quarantined())

	p := &snapshotCandidateProvider{
		phonebook:   pb,
		clusterPath: cluster,
		selfID:      self,
		localDC:     "dc-self",
		logger:      zap.NewNop(),
	}

	cands := p.Candidates("cap1", "v1")
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates (self excluded), got %d", len(cands))
	}
	byID := map[string]struct {
		dc     string
		disk   int64
		active bool
	}{}
	for _, c := range cands {
		byID[c.NodeID] = struct {
			dc     string
			disk   int64
			active bool
		}{c.Datacenter, c.DiskMBFree, c.Active}
	}
	if _, ok := byID[self]; ok {
		t.Error("self must be excluded from candidates")
	}
	if a := byID[peerA]; a.dc != "dc-a" || a.disk != 50*1024 || !a.active {
		t.Errorf("peerA mapped wrong: %+v", a)
	}
	if b := byID[peerB]; b.active {
		t.Error("quarantined peerB should map Active=false")
	}
	if p.LocalDatacenter() != "dc-self" {
		t.Errorf("LocalDatacenter = %q", p.LocalDatacenter())
	}
}
