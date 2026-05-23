package gravity

import (
	"testing"

	"github.com/tareksalem/falak/capsule"
)

// memberCapsule returns a minimal group-member capsule with the given
// resources. Mirrors minimalCapsule() but lets the test set CPUCores
// and MemoryMB inline.
func memberCapsule(name string, cpu int32, memMB int64) *capsule.Capsule {
	c := &capsule.Capsule{
		ID:        capsule.NewCapsuleID(),
		ClusterID: "test/dc1/cluster",
		Spec: capsule.CapsuleSpec{
			Name:  name,
			Image: name + ":v1",
			Orbit: "api",
			Resources: capsule.ResourceRequirements{
				CPUCores: cpu,
				MemoryMB: memMB,
			},
		},
	}
	capsule.DefaultSpec(&c.Spec)
	// DefaultSpec doesn't touch Resources, but it does set Labels and other
	// defaults that downstream helpers rely on.
	return c
}

// TestCalculateCombinedFit_EmptyMembers returns a non-eligible result.
func TestCalculateCombinedFit_EmptyMembers(t *testing.T) {
	calc := NewCalculator()
	node := healthyNode("n1")

	got := calc.CalculateCombinedFit(nil, node)
	if got.Result.Eligible {
		t.Error("expected ineligible for empty members slice")
	}
	if got.IneligibleMember != "<none>" {
		t.Errorf("expected IneligibleMember=<none>, got %q", got.IneligibleMember)
	}
}

// TestCalculateCombinedFit_AllFit verifies that a node with enough
// summed capacity for every member returns an Eligible result.
func TestCalculateCombinedFit_AllFit(t *testing.T) {
	calc := NewCalculator()
	// Node has 8 CPU / 16384 MB total = enough for 2+3+2 CPU and
	// 512+1024+2048 MB combined demand.
	node := healthyNode("n1")

	members := []*capsule.Capsule{
		memberCapsule("db", 3, 2048),
		memberCapsule("api", 2, 1024),
		memberCapsule("web", 2, 512),
	}

	got := calc.CalculateCombinedFit(members, node)
	if !got.Result.Eligible {
		t.Fatalf("expected eligible, got ineligible: %+v", got)
	}
	if got.IneligibleMember != "" {
		t.Errorf("expected no ineligible member, got %q", got.IneligibleMember)
	}
	if got.Result.Score <= 0 || got.Result.Score > 100 {
		t.Errorf("score out of range: %v", got.Result.Score)
	}
}

// TestCalculateCombinedFit_SumExceedsNode verifies that when summed
// resource demand exceeds the node's capacity, the result is ineligible
// — driven by the per-member eligibility check on whichever member's
// individual demand is also unmet, OR by zero score from the combined
// calculation when individual fits pass.
func TestCalculateCombinedFit_SumExceedsNode(t *testing.T) {
	calc := NewCalculator()
	// Node has 8 CPU. One 6-CPU member fits alone but together with a
	// second 5-CPU member the sum (11) exceeds the node.
	node := healthyNode("n1")

	members := []*capsule.Capsule{
		memberCapsule("a", 6, 1024),
		memberCapsule("b", 5, 1024),
	}

	got := calc.CalculateCombinedFit(members, node)
	// Either individual eligibility rejects b (5 CPU > 8 alone is fine,
	// so b passes individually) and the combined Calculate scores low
	// for headroom, OR the synthetic 11-CPU capsule fails its own
	// eligibility check. Both paths result in Eligible=false.
	if got.Result.Eligible {
		t.Errorf("expected ineligible when summed demand exceeds capacity, got %+v", got)
	}
}

// TestCalculateCombinedFit_OneIneligibleRejectsAll verifies the AND
// semantics: if any single member's individual eligibility fails, the
// combined result is ineligible regardless of how the others score.
func TestCalculateCombinedFit_OneIneligibleRejectsAll(t *testing.T) {
	calc := NewCalculator()
	node := healthyNode("n1") // 8 CPU total

	members := []*capsule.Capsule{
		memberCapsule("api", 2, 512),  // fits
		memberCapsule("hog", 16, 512), // exceeds individually
		memberCapsule("web", 2, 512),  // fits
	}

	got := calc.CalculateCombinedFit(members, node)
	if got.Result.Eligible {
		t.Fatal("expected ineligible due to hog member")
	}
	if got.IneligibleMember != "hog" {
		t.Errorf("IneligibleMember: got %q, want hog", got.IneligibleMember)
	}
	if len(got.IneligibleMembers) == 0 || got.IneligibleMembers[0] != "hog" {
		t.Errorf("IneligibleMembers: got %v, want [hog]", got.IneligibleMembers)
	}
}

// TestCalculateCombinedFit_NilMembersSkipped verifies that nil entries
// in the members slice are silently skipped rather than panicking.
func TestCalculateCombinedFit_NilMembersSkipped(t *testing.T) {
	calc := NewCalculator()
	node := healthyNode("n1")

	members := []*capsule.Capsule{
		memberCapsule("db", 2, 1024),
		nil,
		memberCapsule("api", 2, 1024),
	}

	got := calc.CalculateCombinedFit(members, node)
	if !got.Result.Eligible {
		t.Errorf("expected eligible (nil should be skipped), got: %+v", got)
	}
}

// TestCalculateCombinedFit_UnhealthyNodeRejected confirms node-level
// eligibility (e.g. unhealthy / quarantined) is honoured for each
// member's check.
func TestCalculateCombinedFit_UnhealthyNodeRejected(t *testing.T) {
	calc := NewCalculator()
	node := healthyNode("n1", func(n *NodeState) {
		n.Status = NodeStatusEnum.Quarantined()
	})

	members := []*capsule.Capsule{
		memberCapsule("db", 1, 256),
		memberCapsule("api", 1, 256),
	}

	got := calc.CalculateCombinedFit(members, node)
	if got.Result.Eligible {
		t.Fatal("expected ineligible on quarantined node")
	}
	// Every member should fail node health → every name in
	// IneligibleMembers.
	if len(got.IneligibleMembers) != 2 {
		t.Errorf("expected both members ineligible, got %v", got.IneligibleMembers)
	}
}
