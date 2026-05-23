package gravity

import (
	"testing"

	"github.com/tareksalem/falak/capsule"
	capsuleEnums "github.com/tareksalem/falak/capsule/enums"
)

// --- Test fixtures -----------------------------------------------------------

// staticLookup is a CapsuleTargetLookup that returns a hard-coded mapping
// from capsule name to a list of node IDs running it. Used by tests that
// exercise affinity / anti-affinity factors.
type staticLookup map[string][]string

func (s staticLookup) NodesRunningCapsule(_ /*clusterPath*/, name string) []string {
	return s[name]
}

// healthyNode returns a NodeState that passes the health check and has
// generous resources, ready to be customized further by tests.
func healthyNode(id string, opts ...func(*NodeState)) NodeState {
	n := NodeState{
		NodeID:      id,
		ClusterPath: "test/dc1/cluster",
		Datacenter:  "dc1",
		Region:      "us-east",
		Labels:      capsule.Labels{},
		Resources: Resources{
			CPUCoresTotal: 8, CPUCoresFree: 8,
			MemoryMBTotal: 16384, MemoryMBFree: 16384,
			DiskMBTotal: 100000, DiskMBFree: 100000,
		},
		Status:           NodeStatusEnum.Active(),
		ReliabilityScore: 1.0,
	}
	for _, o := range opts {
		o(&n)
	}
	return n
}

// minimalCapsule returns a default capsule used by tests.
func minimalCapsule(name string, opts ...func(*capsule.Capsule)) *capsule.Capsule {
	c := &capsule.Capsule{
		ID:        capsule.NewCapsuleID(),
		ClusterID: "test/dc1/cluster",
		Spec: capsule.CapsuleSpec{
			Name:  name,
			Image: "img:v1",
			Orbit: "api",
		},
	}
	capsule.DefaultSpec(&c.Spec)
	for _, o := range opts {
		o(c)
	}
	return c
}

// --- Eligibility tests -------------------------------------------------------

func TestIsEligible_HealthyNodePasses(t *testing.T) {
	c := minimalCapsule("test")
	node := healthyNode("n1")

	got := IsEligible(c, node, nil)
	if !got.PassedEligibility() {
		t.Errorf("expected eligible, got %+v", got)
	}
}

func TestIsEligible_UnhealthyNodeRejected(t *testing.T) {
	c := minimalCapsule("test")
	node := healthyNode("n1", func(n *NodeState) {
		n.Status = NodeStatusEnum.Quarantined()
	})

	got := IsEligible(c, node, nil)
	if got.PassedEligibility() {
		t.Error("expected ineligible for quarantined node")
	}
	if got.Reason != IneligibilityReasonEnum.NotHealthy() {
		t.Errorf("expected reason %q, got %q", IneligibilityReasonEnum.NotHealthy(), got.Reason)
	}
}

func TestIsEligible_InsufficientCPU(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.Resources.CPUCores = 16
	})
	node := healthyNode("n1") // has 8 cores

	got := IsEligible(c, node, nil)
	if got.Reason != IneligibilityReasonEnum.ResourcesCPU() {
		t.Errorf("expected CPU rejection, got %+v", got)
	}
}

func TestIsEligible_InsufficientMemory(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.Resources.MemoryMB = 32000
	})
	node := healthyNode("n1") // has 16384

	got := IsEligible(c, node, nil)
	if got.Reason != IneligibilityReasonEnum.ResourcesMemory() {
		t.Errorf("expected memory rejection, got %+v", got)
	}
}

func TestIsEligible_HardPlacementRuleFails(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.PlacementRules = []capsule.PlacementRule{
			{
				Name:     "gpu required",
				Type:     capsuleEnums.PlacementTypeEnum.Node(),
				Labels:   capsule.Labels{"gpu": "true"},
				Required: true,
			},
		}
	})
	node := healthyNode("n1") // no gpu label

	got := IsEligible(c, node, nil)
	if got.Reason != IneligibilityReasonEnum.PlacementRule() {
		t.Errorf("expected placement rejection, got %+v", got)
	}
}

func TestIsEligible_HardPlacementRulePasses(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.PlacementRules = []capsule.PlacementRule{
			{
				Name:     "gpu required",
				Type:     capsuleEnums.PlacementTypeEnum.Node(),
				Labels:   capsule.Labels{"gpu": "true"},
				Required: true,
			},
		}
	})
	node := healthyNode("n1", func(n *NodeState) {
		n.Labels = capsule.Labels{"gpu": "true"}
	})

	got := IsEligible(c, node, nil)
	if !got.PassedEligibility() {
		t.Errorf("expected eligible, got %+v", got)
	}
}

func TestIsEligible_SoftRulesIgnoredByEligibility(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.PlacementRules = []capsule.PlacementRule{
			{
				Name:     "prefer gpu",
				Type:     capsuleEnums.PlacementTypeEnum.Node(),
				Labels:   capsule.Labels{"gpu": "true"},
				Required: false,
			},
		}
	})
	node := healthyNode("n1") // no gpu

	got := IsEligible(c, node, nil)
	if !got.PassedEligibility() {
		t.Error("soft rules must not affect eligibility")
	}
}

// TestIsEligible_SelfAntiAffinity asserts that a node which is already
// running a replica of the target capsule is rejected by the implicit
// self-anti-affinity rule. This rule replaces the old ExcludeNodes
// protocol field.
func TestIsEligible_SelfAntiAffinity(t *testing.T) {
	c := minimalCapsule("test")
	node := healthyNode("n1")

	got := IsEligible(c, node, staticLookup{"test": {"n1"}})
	if got.Reason != IneligibilityReasonEnum.Excluded() {
		t.Errorf("expected excluded rejection, got %+v", got)
	}
}

// --- Weights tests -----------------------------------------------------------

func TestWeights_DefaultsAreSane(t *testing.T) {
	w := DefaultWeights()
	if w.CPUHeadroom <= 0 || w.MemoryHeadroom <= 0 {
		t.Errorf("default weights for CPU and memory must be positive: %+v", w)
	}
	if w.MaxPossibleScore() <= 0 {
		t.Error("MaxPossibleScore should be positive for default weights")
	}
}

func TestWeights_MergeOverridesNonZeroFields(t *testing.T) {
	base := DefaultWeights()
	override := Weights{CPUHeadroom: 5.0}

	merged := base.Merge(override)
	if merged.CPUHeadroom != 5.0 {
		t.Errorf("CPU should be overridden to 5.0, got %v", merged.CPUHeadroom)
	}
	if merged.MemoryHeadroom != base.MemoryHeadroom {
		t.Error("memory weight should be unchanged when not in override")
	}
}

func TestWeights_MergeKeepsBaseForZeroOverrides(t *testing.T) {
	base := DefaultWeights()
	merged := base.Merge(Weights{}) // all zero
	if merged != base {
		t.Errorf("zero override should leave base unchanged: got %+v", merged)
	}
}

// --- Factor tests ------------------------------------------------------------

func TestFactorCPUHeadroom_Applied(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.Resources.CPUCores = 2
	})
	node := healthyNode("n1") // 8 cores free
	got := factorCPUHeadroom(c, node)
	if !got.Applied {
		t.Fatal("expected applied")
	}
	// (8 - 2) / 8 = 0.75
	if got.Value != 0.75 {
		t.Errorf("expected 0.75, got %v", got.Value)
	}
}

func TestFactorCPUHeadroom_NotApplicableWhenCapsuleHasNoCPU(t *testing.T) {
	c := minimalCapsule("test")
	node := healthyNode("n1")
	got := factorCPUHeadroom(c, node)
	if got.Applied {
		t.Error("should not apply when capsule has no CPU requirement")
	}
}

func TestFactorCPUHeadroom_NodeTooFullScoresZero(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.Resources.CPUCores = 16
	})
	node := healthyNode("n1") // only 8 cores total
	got := factorCPUHeadroom(c, node)
	if !got.Applied {
		t.Fatal("expected applied")
	}
	if got.Value != 0 {
		t.Errorf("expected 0 for over-utilized node, got %v", got.Value)
	}
}

func TestFactorMemoryHeadroom_Applied(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.Resources.MemoryMB = 4096
	})
	node := healthyNode("n1") // 16384 MB free, 16384 total
	got := factorMemoryHeadroom(c, node)
	if !got.Applied {
		t.Fatal("expected applied")
	}
	// (16384 - 4096) / 16384 = 0.75
	if got.Value != 0.75 {
		t.Errorf("expected 0.75, got %v", got.Value)
	}
}

func TestFactorSoftPlacementMatch_PartialMatch(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.PlacementRules = []capsule.PlacementRule{
			{
				Name:     "prefer gpu",
				Type:     capsuleEnums.PlacementTypeEnum.Node(),
				Labels:   capsule.Labels{"gpu": "true"},
				Required: false,
			},
			{
				Name:     "prefer ssd",
				Type:     capsuleEnums.PlacementTypeEnum.Node(),
				Labels:   capsule.Labels{"ssd": "true"},
				Required: false,
			},
		}
	})
	node := healthyNode("n1", func(n *NodeState) {
		n.Labels = capsule.Labels{"gpu": "true"}
	})
	got := factorSoftPlacementMatch(c, node, nil)
	if !got.Applied {
		t.Fatal("expected applied")
	}
	if got.Value != 0.5 {
		t.Errorf("expected 0.5 (1 of 2 matches), got %v", got.Value)
	}
}

func TestFactorAffinityProximity(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.PlacementRules = []capsule.PlacementRule{
			{
				Name:   "near db",
				Type:   capsuleEnums.PlacementTypeEnum.Capsule(),
				Mode:   capsuleEnums.PlacementModeEnum.Near(),
				Names:  []string{"db"},
				Labels: capsule.Labels{"node": "same"},
			},
		}
	})

	lookup := staticLookup{"db": []string{"n1", "n2"}}

	// Node n1 also runs db -> proximity 1.0
	node := healthyNode("n1")
	got := factorAffinityProximity(c, node, lookup)
	if !got.Applied || got.Value != 1.0 {
		t.Errorf("expected 1.0 affinity, got %+v", got)
	}

	// Node n3 does not run db -> proximity 0
	node3 := healthyNode("n3")
	got = factorAffinityProximity(c, node3, lookup)
	if !got.Applied || got.Value != 0 {
		t.Errorf("expected 0 affinity, got %+v", got)
	}
}

func TestFactorLoadPenalty(t *testing.T) {
	// Empty node: penalty value 1.0 (max free capacity)
	empty := healthyNode("n1")
	got := factorLoadPenalty(empty)
	if !got.Applied || got.Value != 1.0 {
		t.Errorf("empty node should score 1.0, got %+v", got)
	}

	// 25 capsules running: 25/50 = 0.5 utilization, penalty value 0.5
	loaded := healthyNode("n1", func(n *NodeState) {
		n.RunningCapsuleCount = 25
	})
	got = factorLoadPenalty(loaded)
	if got.Value != 0.5 {
		t.Errorf("loaded node should score 0.5, got %v", got.Value)
	}

	// At and beyond saturation: 0
	full := healthyNode("n1", func(n *NodeState) {
		n.RunningCapsuleCount = 100
	})
	got = factorLoadPenalty(full)
	if got.Value != 0 {
		t.Errorf("over-loaded node should score 0, got %v", got.Value)
	}
}

func TestFactorReliability(t *testing.T) {
	node := healthyNode("n1", func(n *NodeState) {
		n.ReliabilityScore = 0.7
	})
	got := factorReliability(node)
	if !got.Applied || got.Value != 0.7 {
		t.Errorf("expected 0.7, got %+v", got)
	}
}

func TestFactorDiversity_AppliesWhenMultiReplica(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.Replicas.Min = 3
		c.Spec.Replicas.Max = 5
	})
	node := healthyNode("n1") // has dc1
	got := factorDiversity(c, node)
	if !got.Applied || got.Value != 1.0 {
		t.Errorf("expected applied 1.0, got %+v", got)
	}
}

func TestFactorDiversity_NotApplicableWhenSingleReplica(t *testing.T) {
	c := minimalCapsule("test") // default replicas min:1 max:1
	node := healthyNode("n1")
	got := factorDiversity(c, node)
	if got.Applied {
		t.Error("should not apply for single replica")
	}
}

// --- Calculator tests --------------------------------------------------------

func TestCalculator_IneligibleNodeReturnsZero(t *testing.T) {
	c := minimalCapsule("test")
	node := healthyNode("n1", func(n *NodeState) {
		n.Status = NodeStatusEnum.Failed()
	})

	calc := NewCalculator()
	res := calc.Calculate(c, node)
	if res.Eligible {
		t.Error("should be ineligible")
	}
	if res.Score != 0 {
		t.Errorf("ineligible score should be 0, got %v", res.Score)
	}
}

func TestCalculator_HealthyNodeProducesScore(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.Resources.CPUCores = 2
		c.Spec.Resources.MemoryMB = 4096
	})
	node := healthyNode("n1")

	calc := NewCalculator()
	res := calc.Calculate(c, node)
	if !res.Eligible {
		t.Fatalf("should be eligible: %+v", res)
	}
	if res.Score <= 0 || res.Score > 100 {
		t.Errorf("score should be in (0, 100], got %v", res.Score)
	}
	// CPU and memory factors should appear in the breakdown.
	if _, ok := res.Factors["cpu_headroom"]; !ok {
		t.Error("expected cpu_headroom in factors")
	}
	if _, ok := res.Factors["memory_headroom"]; !ok {
		t.Error("expected memory_headroom in factors")
	}
}

func TestCalculator_HighFitNodeBeatsLowFit(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.Resources.CPUCores = 2
		c.Spec.Resources.MemoryMB = 1024
	})

	bigEmpty := healthyNode("big") // lots of headroom
	smallLoaded := healthyNode("small", func(n *NodeState) {
		n.Resources = Resources{
			CPUCoresTotal: 4, CPUCoresFree: 2,
			MemoryMBTotal: 4096, MemoryMBFree: 2048,
			DiskMBTotal: 50000, DiskMBFree: 25000,
		}
		n.RunningCapsuleCount = 30
		n.ReliabilityScore = 0.7
	})

	calc := NewCalculator()
	bigRes := calc.Calculate(c, bigEmpty)
	smallRes := calc.Calculate(c, smallLoaded)

	if bigRes.Score <= smallRes.Score {
		t.Errorf("big empty node should score higher: big=%v, small=%v",
			bigRes.Score, smallRes.Score)
	}
}

func TestCalculator_CalculateAllReturnsAllNodes(t *testing.T) {
	c := minimalCapsule("test")
	nodes := []NodeState{
		healthyNode("n1"),
		healthyNode("n2", func(n *NodeState) { n.Status = NodeStatusEnum.Quarantined() }),
		healthyNode("n3"),
	}
	calc := NewCalculator()
	results := calc.CalculateAll(c, nodes)
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[1].Eligible {
		t.Error("quarantined node should be ineligible")
	}
	if !results[0].Eligible || !results[2].Eligible {
		t.Error("healthy nodes should be eligible")
	}
}

func TestCalculator_CustomWeightsAffectScore(t *testing.T) {
	c := minimalCapsule("test", func(c *capsule.Capsule) {
		c.Spec.Resources.CPUCores = 2
	})
	node := healthyNode("n1")

	low := NewCalculator(WithWeights(DefaultWeights().Merge(Weights{CPUHeadroom: 0.01})))
	high := NewCalculator(WithWeights(DefaultWeights().Merge(Weights{CPUHeadroom: 100})))

	lowRes := low.Calculate(c, node)
	highRes := high.Calculate(c, node)

	if lowRes.Score == highRes.Score {
		t.Errorf("different CPU weights should produce different scores: low=%v, high=%v",
			lowRes.Score, highRes.Score)
	}
}

func TestCalculator_ScoreClampedTo100(t *testing.T) {
	c := minimalCapsule("test")
	node := healthyNode("n1")
	calc := NewCalculator()
	res := calc.Calculate(c, node)
	if res.Score > 100 {
		t.Errorf("score should be clamped to 100, got %v", res.Score)
	}
	if res.Score < 0 {
		t.Errorf("score should be clamped to 0, got %v", res.Score)
	}
}
