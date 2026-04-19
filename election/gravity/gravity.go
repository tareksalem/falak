package gravity

import (
	"github.com/tareksalem/falak/capsule"
)

// Score is a per-(capsule, node) gravity result on the 0–100 percentage
// scale. Larger values indicate a better fit. A score of 0 means the node
// scored poorly on every applicable factor or was clamped at the lower
// bound; a score of 100 means the node scored maximally on every applicable
// factor.
//
// Score is intentionally a float64 (not int) so the calculator can express
// fine-grained differences without aliasing — election strategies use the
// raw value for comparisons and rounding only happens at display time.
type Score float64

// Result is the full output of a gravity calculation: the final Score plus
// the per-factor breakdown that produced it. The breakdown is invaluable
// for debugging "why did this node score so low?" questions and for
// surfacing election decisions to operators.
type Result struct {
	Score      Score
	Factors    map[string]float64 // per-factor unweighted contribution in [0, 1]
	Weights    Weights
	Eligible   bool
	Inelig     Ineligibility // populated when Eligible is false
}

// SnapshotLookup checks whether the local node has a snapshot cached
// for a given capsule. The gravity calculator uses this to reward nodes
// that can restore instantly (avoiding transfer latency).
type SnapshotLookup interface {
	// HasLocalSnapshot returns true if the local node has a snapshot for
	// the given (capsuleID, tag). The tag is typically the image digest.
	HasLocalSnapshot(capsuleID, tag string) bool
}

// Calculator computes gravity scores for capsule/node pairs.
//
// The calculator is stateless apart from the weights and the optional
// lookups; one Calculator can score many capsules and many nodes
// concurrently. Construct one Calculator per cluster (because weights
// are per-cluster) and reuse it for the cluster's lifetime.
type Calculator struct {
	weights  Weights
	lookup   CapsuleTargetLookup
	snapLookup SnapshotLookup
}

// CalculatorOption configures a Calculator.
type CalculatorOption func(*Calculator)

// WithWeights overrides the default weights. Apply per-cluster overrides
// before constructing the calculator by merging them onto DefaultWeights.
func WithWeights(w Weights) CalculatorOption {
	return func(c *Calculator) {
		c.weights = w
	}
}

// WithCapsuleTargetLookup wires in a lookup for evaluating capsule
// affinity / anti-affinity rules. Without it the affinity factor is
// skipped (returns notApplicable) and capsule-typed eligibility checks
// degrade to the conservative defaults documented on matchCapsuleRule.
func WithCapsuleTargetLookup(l CapsuleTargetLookup) CalculatorOption {
	return func(c *Calculator) {
		c.lookup = l
	}
}

// WithSnapshotLookup wires in a lookup for evaluating snapshot locality.
// Without it the snapshot_locality factor is skipped.
func WithSnapshotLookup(l SnapshotLookup) CalculatorOption {
	return func(c *Calculator) {
		c.snapLookup = l
	}
}

// NewCalculator constructs a Calculator with the given options. Defaults:
//   - Weights: DefaultWeights()
//   - Lookup:  nil (capsule-typed factors will be skipped)
func NewCalculator(opts ...CalculatorOption) *Calculator {
	c := &Calculator{
		weights: DefaultWeights(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Weights returns the calculator's current weights. Useful for tests and
// for surfacing the active configuration in observability outputs.
func (c *Calculator) Weights() Weights {
	return c.weights
}

// Calculate computes the gravity Result for a single (capsule, node) pair.
//
// The flow is:
//  1. Run the eligibility filter. If the node is ineligible, return a
//     zero-score Result with Eligible=false and the failure reason set.
//  2. Compute every factor that applies to the (capsule, node) pair.
//  3. Sum applicable factors weighted by their respective weights.
//  4. Subtract the load penalty (always applied).
//  5. Normalize the weighted sum into the 0–100 scale and clamp.
//  6. Return the final Score plus the per-factor breakdown.
//
// Calculate is pure: given the same inputs it always returns the same
// output. It performs no I/O and no logging.
func (c *Calculator) Calculate(
	capsuleObj *capsule.Capsule,
	node NodeState,
) Result {
	inelig := IsEligible(capsuleObj, node, c.lookup)
	if !inelig.PassedEligibility() {
		return Result{
			Score:    0,
			Factors:  map[string]float64{},
			Weights:  c.weights,
			Eligible: false,
			Inelig:   inelig,
		}
	}

	factors := map[string]float64{}
	var weightedSum float64
	var maxWeightedSum float64

	addFactor := func(name string, contribution factorContribution, weight float64) {
		if !contribution.Applied {
			return
		}
		factors[name] = contribution.Value
		weightedSum += contribution.Value * weight
		maxWeightedSum += weight
	}

	addFactor("cpu_headroom", factorCPUHeadroom(capsuleObj, node), c.weights.CPUHeadroom)
	addFactor("memory_headroom", factorMemoryHeadroom(capsuleObj, node), c.weights.MemoryHeadroom)
	addFactor("disk_headroom", factorDiskHeadroom(capsuleObj, node), c.weights.DiskHeadroom)
	addFactor("soft_placement_match", factorSoftPlacementMatch(capsuleObj, node, c.lookup), c.weights.SoftPlacementMatch)
	addFactor("affinity_proximity", factorAffinityProximity(capsuleObj, node, c.lookup), c.weights.AffinityProximity)
	addFactor("hardware_label_match", factorHardwareLabelMatch(capsuleObj, node), c.weights.HardwareLabelMatch)
	addFactor("reliability", factorReliability(node), c.weights.Reliability)
	addFactor("diversity", factorDiversity(capsuleObj, node), c.weights.Diversity)
	addFactor("snapshot_locality", factorSnapshotLocality(capsuleObj, c.snapLookup), c.weights.SnapshotLocality)

	// Load penalty is always applied (and subtracted, not added).
	loadFactor := factorLoadPenalty(node)
	if loadFactor.Applied {
		factors["load_penalty"] = loadFactor.Value
		// Higher loadFactor.Value means MORE free capacity. We invert so
		// the *penalty* grows as the node gets fuller.
		penalty := (1 - loadFactor.Value) * c.weights.LoadPenalty
		weightedSum -= penalty
		// The penalty's contribution to the maximum is its full weight,
		// because in the best case (node empty) the penalty is zero.
		maxWeightedSum += c.weights.LoadPenalty
	}

	if maxWeightedSum <= 0 {
		// No factor applied — degenerate case (shouldn't happen because
		// reliability is always applied). Return a neutral 50.
		return Result{
			Score:    50,
			Factors:  factors,
			Weights:  c.weights,
			Eligible: true,
		}
	}

	// Normalize to 0..1 then scale to 0..100.
	normalized := weightedSum / maxWeightedSum
	if normalized < 0 {
		normalized = 0
	}
	if normalized > 1 {
		normalized = 1
	}

	return Result{
		Score:    Score(normalized * 100),
		Factors:  factors,
		Weights:  c.weights,
		Eligible: true,
	}
}

// CalculateAll computes gravity for every node in the supplied slice and
// returns the results in the same order. Ineligible nodes are included as
// zero-score Results so callers can present the full picture (e.g. "node
// excluded: insufficient memory").
//
// This is primarily a diagnostic helper — the election hot path only
// ever scores the local node.
func (c *Calculator) CalculateAll(
	capsuleObj *capsule.Capsule,
	nodes []NodeState,
) []Result {
	out := make([]Result, len(nodes))
	for i, node := range nodes {
		out[i] = c.Calculate(capsuleObj, node)
	}
	return out
}
