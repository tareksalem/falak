package election_test

import (
	"context"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/election"
	"github.com/tareksalem/falak/election/delay"
	"github.com/tareksalem/falak/election/gravity"
)

// staticProvider is a deterministic in-memory StateProvider for tests.
// It returns the local node state from a fixed map keyed on node ID.
type staticProvider struct {
	local string
	nodes map[string]gravity.NodeState
}

func newStaticProvider(localID string) *staticProvider {
	return &staticProvider{
		local: localID,
		nodes: map[string]gravity.NodeState{},
	}
}

func (p *staticProvider) add(state gravity.NodeState) {
	p.nodes[state.NodeID] = state
}

func (p *staticProvider) LocalNode(string) (gravity.NodeState, error) {
	return p.nodes[p.local], nil
}

func makeNode(id string, cpuFree int32) gravity.NodeState {
	return gravity.NodeState{
		NodeID:      id,
		ClusterPath: "test/dc1/c",
		Datacenter:  "dc1",
		Region:      "us-east",
		Labels:      capsule.Labels{},
		Resources: gravity.Resources{
			CPUCoresTotal: 8, CPUCoresFree: cpuFree,
			MemoryMBTotal: 16384, MemoryMBFree: 16000,
			DiskMBTotal: 100000, DiskMBFree: 90000,
		},
		Status:           gravity.NodeStatusEnum.Active(),
		ReliabilityScore: 1.0,
	}
}

func makeCapsule(needCPU int32) *capsule.Capsule {
	c := &capsule.Capsule{
		ID:        capsule.NewCapsuleID(),
		ClusterID: "test/dc1/c",
		Spec: capsule.CapsuleSpec{
			Name:      "test",
			Image:     "img:v1",
			Orbit:     "api",
			Resources: capsule.ResourceRequirements{CPUCores: needCPU},
		},
	}
	capsule.DefaultSpec(&c.Spec)
	return c
}

// --- DelayStrategy --------------------------------------------------------

func TestDelayStrategy_EligibleProducesPublishAt(t *testing.T) {
	provider := newStaticProvider("n1")
	provider.add(makeNode("n1", 8))
	calc := gravity.NewCalculator()

	s := delay.New()
	c := makeCapsule(2)
	req := election.Request{
		CapsuleID:   c.ID,
		ReplicaID:   "0",
		ClusterPath: c.ClusterID,
	}

	dec := s.Decide(context.Background(), req, c, calc, provider)
	if !dec.Eligible {
		t.Fatalf("expected eligible: %+v", dec)
	}
	if dec.Score <= 0 {
		t.Errorf("expected positive score, got %v", dec.Score)
	}
	if !dec.PublishAt.After(time.Now().Add(-1 * time.Second)) {
		t.Errorf("PublishAt should be near now, got %v", dec.PublishAt)
	}
	// O14b: the delay strategy must carry the deterministic priority Offset
	// (baseWait + slotDelay + jitter) — the tiebreak key the manager publishes.
	if dec.Offset <= 0 {
		t.Errorf("eligible decision must carry a positive Offset (the O14b tiebreak key), got %v", dec.Offset)
	}
}

func TestDelayStrategy_HigherScoreShorterWait(t *testing.T) {
	calc := gravity.NewCalculator()
	c := makeCapsule(2)
	req := election.Request{CapsuleID: c.ID, ReplicaID: "0", ClusterPath: c.ClusterID}

	bigProvider := newStaticProvider("big")
	bigProvider.add(makeNode("big", 8)) // generous headroom

	smallProvider := newStaticProvider("small")
	small := makeNode("small", 3) // tighter
	small.RunningCapsuleCount = 30
	small.ReliabilityScore = 0.7
	smallProvider.add(small)

	s := delay.New(delay.WithMaxWait(500*time.Millisecond), delay.WithMaxJitter(0))

	bigDec := s.Decide(context.Background(), req, c, calc, bigProvider)
	smallDec := s.Decide(context.Background(), req, c, calc, smallProvider)

	if !bigDec.Eligible || !smallDec.Eligible {
		t.Fatalf("both should be eligible")
	}
	if !bigDec.PublishAt.Before(smallDec.PublishAt) {
		t.Errorf("higher gravity should publish earlier: big=%v small=%v",
			bigDec.PublishAt, smallDec.PublishAt)
	}
	// O14b: the Offset (the clock-independent tiebreak key) must order the
	// same way — better fit → smaller offset → wins the tiebreak. Jitter is
	// disabled here (WithMaxJitter(0)) so the comparison is exact.
	if !(bigDec.Offset < smallDec.Offset) {
		t.Errorf("higher gravity should yield a smaller Offset: big=%v small=%v",
			bigDec.Offset, smallDec.Offset)
	}
}

func TestDelayStrategy_IneligibleSkips(t *testing.T) {
	provider := newStaticProvider("n1")
	provider.add(makeNode("n1", 8))
	calc := gravity.NewCalculator()

	c := makeCapsule(99) // requires more CPU than the node has
	req := election.Request{CapsuleID: c.ID, ReplicaID: "0", ClusterPath: c.ClusterID}

	s := delay.New()
	dec := s.Decide(context.Background(), req, c, calc, provider)
	if dec.Eligible {
		t.Errorf("should be ineligible for over-spec capsule, got %+v", dec)
	}
}
