package gravity

// Weights controls how much each gravity factor contributes to the final
// score. A factor only participates in the calculation if the capsule has
// a corresponding rule or requirement (e.g. CPU is only weighted when
// capsule.Resources.CPUCores > 0). Weights determine *how much* each
// participating factor matters relative to the others.
//
// Weights are applied in three layers:
//
//  1. Built-in defaults (DefaultWeights below).
//  2. Cluster-level overrides loaded from CUE configuration. Operators tune
//     these for their environment.
//  3. Per-capsule overrides set in the capsule spec. Capsule authors tune
//     these for special workloads.
//
// Each subsequent layer overrides only the fields it explicitly sets.
// Use Merge to combine two layers in priority order.
//
// All weights are unitless multipliers. The Calculator normalizes the
// weighted sum into the 0–100 percentage scale at the end.
type Weights struct {
	// CPUHeadroom rewards nodes with spare CPU capacity beyond what the
	// capsule requires. Default 1.0.
	CPUHeadroom float64

	// MemoryHeadroom rewards nodes with spare memory capacity. Default 1.0.
	MemoryHeadroom float64

	// DiskHeadroom rewards nodes with spare disk capacity. Default 0.5.
	DiskHeadroom float64

	// SoftPlacementMatch rewards each soft placement rule (required: false)
	// the node satisfies. Multiplied by the number of matching rules.
	// Default 0.3.
	SoftPlacementMatch float64

	// AffinityProximity rewards nodes that are co-located with the
	// capsules referenced by mode: near placement rules. Default 0.8.
	AffinityProximity float64

	// HardwareLabelMatch rewards each hardware/soft label match.
	// Default 0.2.
	HardwareLabelMatch float64

	// LoadPenalty discourages stacking too many capsules on the same node.
	// Subtracted from the score. Default 0.5 (treated as -0.5 in the sum).
	LoadPenalty float64

	// Reliability rewards nodes with high historical success rates.
	// Default 0.4.
	Reliability float64

	// Diversity rewards spreading replicas across distinct datacenters or
	// regions. Default 0.2. Only applies when the capsule asks for more
	// than one replica.
	Diversity float64

	// SnapshotLocality rewards nodes that already have a local snapshot
	// for the capsule, avoiding the transfer latency on restore. Default
	// 0.6. Capped so new nodes without snapshots are not permanently
	// starved — they can still win under load.
	SnapshotLocality float64
}

// DefaultWeights returns the built-in default weights. These are tuned to
// produce sensible behavior for typical workloads without operator
// intervention.
func DefaultWeights() Weights {
	return Weights{
		CPUHeadroom:        1.0,
		MemoryHeadroom:     1.0,
		DiskHeadroom:       0.5,
		SoftPlacementMatch: 0.3,
		AffinityProximity:  0.8,
		HardwareLabelMatch: 0.2,
		LoadPenalty:        0.5,
		Reliability:        0.4,
		Diversity:          0.2,
		SnapshotLocality:   0.6,
	}
}

// Merge returns a new Weights where every field set on override replaces
// the corresponding field on base. A field is considered "set" if it is
// non-zero — gravity factors with a true zero weight are equivalent to
// disabling them, which the override layer can express by passing a tiny
// non-zero number such as 1e-9 if needed.
//
// This makes the override pattern explicit: callers only specify the
// fields they want to change; everything else inherits from the base.
func (w Weights) Merge(override Weights) Weights {
	out := w
	if override.CPUHeadroom != 0 {
		out.CPUHeadroom = override.CPUHeadroom
	}
	if override.MemoryHeadroom != 0 {
		out.MemoryHeadroom = override.MemoryHeadroom
	}
	if override.DiskHeadroom != 0 {
		out.DiskHeadroom = override.DiskHeadroom
	}
	if override.SoftPlacementMatch != 0 {
		out.SoftPlacementMatch = override.SoftPlacementMatch
	}
	if override.AffinityProximity != 0 {
		out.AffinityProximity = override.AffinityProximity
	}
	if override.HardwareLabelMatch != 0 {
		out.HardwareLabelMatch = override.HardwareLabelMatch
	}
	if override.LoadPenalty != 0 {
		out.LoadPenalty = override.LoadPenalty
	}
	if override.Reliability != 0 {
		out.Reliability = override.Reliability
	}
	if override.Diversity != 0 {
		out.Diversity = override.Diversity
	}
	if override.SnapshotLocality != 0 {
		out.SnapshotLocality = override.SnapshotLocality
	}
	return out
}

// MaxPossibleScore is the upper bound of the unnormalized weighted sum,
// computed as the sum of all positive weights plus the maximum positive
// contribution from the load penalty (which is zero — the load penalty
// only ever subtracts).
//
// The Calculator divides the unnormalized sum by this value to produce a
// 0–100 score, so the same set of weights always normalizes consistently.
//
// Note: this is the *theoretical* max where every factor returns its
// maximum per-factor contribution (1.0). Real scores rarely reach this
// because no node satisfies every factor perfectly.
func (w Weights) MaxPossibleScore() float64 {
	// Each factor contributes at most 1.0 * its weight to the unnormalized sum.
	// The load penalty is the only factor that subtracts; we exclude it from
	// the maximum because subtraction never increases the score.
	return w.CPUHeadroom +
		w.MemoryHeadroom +
		w.DiskHeadroom +
		w.SoftPlacementMatch +
		w.AffinityProximity +
		w.HardwareLabelMatch +
		w.Reliability +
		w.Diversity +
		w.SnapshotLocality
}
