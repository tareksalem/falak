package gravity

import (
	"github.com/tareksalem/falak/capsule"
)

// CombinedFitResult is the aggregated gravity verdict for a same-node
// CapsuleGroup election. It mirrors the per-capsule Result shape but
// adds two group-specific fields so callers can act on them.
type CombinedFitResult struct {
	// Result is the gravity verdict computed against the synthetic
	// "combined" capsule built from the members' summed resources.
	// Score, Eligible, and the per-factor breakdown all live here.
	Result Result

	// IneligibleMember is the name of the first member that failed
	// its individual eligibility check, or empty when every member
	// was eligible. Populated only when Result.Eligible is false.
	IneligibleMember string

	// IneligibleMembers lists every member that failed eligibility
	// (in spec order). Useful for diagnostics; the election manager
	// uses IneligibleMember alone for the rejection reason.
	IneligibleMembers []string
}

// CalculateCombinedFit computes a single gravity result for a same-node
// CapsuleGroup against the local node. The semantics:
//
//   - Every member's individual eligibility filter must pass. If any
//     member fails (e.g. placement rule mismatch, missing hardware
//     label), the combined result is ineligible. The first ineligible
//     member is captured for the rejection reason.
//
//   - The combined score is computed against a SYNTHETIC capsule whose
//     resources are the SUM of every member's. This makes cpu/memory/
//     disk headroom factors reflect the total demand correctly — a
//     node that fits one member but not all should score low or be
//     ruled out by the eligibility filter.
//
//   - Per-member soft factors (affinity, snapshot locality) and label
//     matches are computed against the first member's spec for now.
//     This is conservative — it favours nodes that fit the first
//     member's preferences. A future enhancement can take the mean
//     across members; the contract here is "AND of eligibility, summed
//     resources for headroom".
//
// The receiver is unchanged; CalculateCombinedFit is pure like
// Calculate.
//
// members must contain at least one capsule. An empty slice returns a
// zero-score result with Eligible=false and IneligibleMember="<none>".
func (c *Calculator) CalculateCombinedFit(
	members []*capsule.Capsule,
	node NodeState,
) CombinedFitResult {
	if len(members) == 0 {
		return CombinedFitResult{
			Result: Result{
				Score:    0,
				Factors:  map[string]float64{},
				Weights:  c.weights,
				Eligible: false,
			},
			IneligibleMember: "<none>",
		}
	}

	// Phase 1: per-member eligibility. Any failure stops the score.
	var ineligibleNames []string
	var firstFail string
	for _, m := range members {
		if m == nil {
			continue
		}
		inelig := IsEligible(m, node, c.lookup)
		if !inelig.PassedEligibility() {
			ineligibleNames = append(ineligibleNames, m.Spec.Name)
			if firstFail == "" {
				firstFail = m.Spec.Name
			}
		}
	}
	if firstFail != "" {
		return CombinedFitResult{
			Result: Result{
				Score:    0,
				Factors:  map[string]float64{},
				Weights:  c.weights,
				Eligible: false,
			},
			IneligibleMember:  firstFail,
			IneligibleMembers: ineligibleNames,
		}
	}

	// Phase 2: build a synthetic capsule with summed resources and run
	// the standard Calculate() against it. The synthetic capsule
	// inherits the first member's identity + placement rules for soft
	// factor purposes.
	synth := *members[0]
	synth.Spec = members[0].Spec
	for _, m := range members[1:] {
		if m == nil {
			continue
		}
		synth.Spec.Resources.CPUCores += m.Spec.Resources.CPUCores
		if m.Spec.Resources.CPUCoresMax > 0 {
			if synth.Spec.Resources.CPUCoresMax == 0 {
				synth.Spec.Resources.CPUCoresMax = synth.Spec.Resources.CPUCores
			}
			synth.Spec.Resources.CPUCoresMax += m.Spec.Resources.CPUCoresMax
		}
		synth.Spec.Resources.MemoryMB += m.Spec.Resources.MemoryMB
		if m.Spec.Resources.MemoryMBMax > 0 {
			if synth.Spec.Resources.MemoryMBMax == 0 {
				synth.Spec.Resources.MemoryMBMax = synth.Spec.Resources.MemoryMB
			}
			synth.Spec.Resources.MemoryMBMax += m.Spec.Resources.MemoryMBMax
		}
		synth.Spec.Resources.DiskMB += m.Spec.Resources.DiskMB
	}

	r := c.Calculate(&synth, node)
	return CombinedFitResult{
		Result: r,
	}
}
