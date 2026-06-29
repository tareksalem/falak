package gravity

import "testing"

// mutableLookup is a CapsuleTargetLookup whose mapping can be changed between
// calls, used to simulate a replica being unbound (UnassignReplica) so the
// node it ran on is no longer reported as hosting the capsule.
type mutableLookup struct {
	byName map[string][]string
}

func (m *mutableLookup) NodesRunningCapsule(_ /*clusterPath*/, name string) []string {
	return m.byName[name]
}

// TestIsEligible_SelfAntiAffinity_ClearedAfterUnbind verifies the exact O3
// transition: while the lookup reports the node as running the capsule, the
// node is Excluded by self-anti-affinity; once the lookup no longer returns
// the node (the binding was cleared via UnassignReplica), the same node is
// eligible to re-place the replica it just lost.
func TestIsEligible_SelfAntiAffinity_ClearedAfterUnbind(t *testing.T) {
	t.Parallel()

	c := minimalCapsule("crash-app")
	node := healthyNode("n1")
	lookup := &mutableLookup{byName: map[string][]string{"crash-app": {"n1"}}}

	// Phase 1: node n1 is reported as running the capsule → Excluded.
	got := IsEligible(c, node, lookup)
	if got.Reason != IneligibilityReasonEnum.Excluded() {
		t.Fatalf("phase 1: expected Excluded while node hosts capsule, got %+v", got)
	}

	// Phase 2: simulate UnassignReplica clearing the binding — the lookup no
	// longer returns n1 for this capsule.
	lookup.byName["crash-app"] = nil

	got = IsEligible(c, node, lookup)
	if !got.PassedEligibility() {
		t.Fatalf("phase 2: expected eligible after unbind, got %+v", got)
	}
	if got.Reason != IneligibilityReasonEnum.OK() {
		t.Errorf("phase 2: expected OK reason, got %q", got.Reason)
	}
}
