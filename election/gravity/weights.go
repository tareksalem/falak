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

	// LoadPenalty weights the free committed-capsule-capacity factor
	// (despite the historical name, it is now a positive reward, not a
	// subtraction — see factorLoadPenalty). The field name is retained
	// because it is the CUE config key operators tune. Default 1.0.
	LoadPenalty float64

	// Reliability rewards nodes with high historical connection success
	// rates (from the phonebook). Default 0.8.
	Reliability float64

	// ExecutionReliability rewards nodes with a high historical ability to
	// actually start and run capsules (from the node-local execution
	// reliability tracker). Distinct from Reliability (connection health).
	// Default 0.6.
	ExecutionReliability float64

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
		CPUHeadroom:          1.0,
		MemoryHeadroom:       1.0,
		DiskHeadroom:         0.5,
		SoftPlacementMatch:   0.3,
		AffinityProximity:    0.8,
		HardwareLabelMatch:   0.2,
		LoadPenalty:          1.0,
		Reliability:          0.8,
		ExecutionReliability: 0.6,
		Diversity:            0.2,
		SnapshotLocality:     0.6,
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
	if override.ExecutionReliability != 0 {
		out.ExecutionReliability = override.ExecutionReliability
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
// the sum of every factor's weight (each factor contributes at most
// 1.0 × its weight). Load and execution reliability are now positive
// factors routed through the standard add path, so they count toward the
// maximum exactly like the others — the old "load only subtracts" carve-out
// no longer holds.
//
// This is a diagnostics helper: the Calculator normalizes against the sum
// of *applicable* factor weights per (capsule, node) pair, which is a
// subset of this theoretical maximum. Real scores rarely reach it because
// no node satisfies every factor perfectly.
func (w Weights) MaxPossibleScore() float64 {
	return w.CPUHeadroom +
		w.MemoryHeadroom +
		w.DiskHeadroom +
		w.SoftPlacementMatch +
		w.AffinityProximity +
		w.HardwareLabelMatch +
		w.LoadPenalty +
		w.Reliability +
		w.ExecutionReliability +
		w.Diversity +
		w.SnapshotLocality
}
