package gravity

import (
	"github.com/tareksalem/falak/capsule"
)

// factorContribution is the unweighted result of a single factor function.
// It carries the per-factor score in [0, 1] and a flag telling the calculator
// whether the factor actually applied to this (capsule, node) pair. If the
// factor did not apply (e.g. CPU headroom for a capsule that does not request
// CPU), Applied is false and Value is meaningless — the calculator skips
// the factor and does not contribute its weight to the maximum.
type factorContribution struct {
	Value   float64
	Applied bool
}

// applied is a small constructor for an applied factor.
func applied(v float64) factorContribution {
	return factorContribution{Value: clamp01(v), Applied: true}
}

// notApplicable is the zero contribution for factors that do not apply to
// the current capsule.
var notApplicable = factorContribution{Applied: false}

// clamp01 clamps a value to the [0, 1] range.
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// factorCPUHeadroom rewards nodes with spare CPU capacity beyond the
// capsule's requirement. The score is the fraction of CPU that would
// remain free *after* the capsule starts, divided by the node's total
// capacity. A node with abundant headroom scores higher than a node
// already running near its limit.
//
// Returns notApplicable when the capsule does not request CPU.
func factorCPUHeadroom(c *capsule.Capsule, node NodeState) factorContribution {
	required := c.Spec.Resources.CPUCores
	if required <= 0 {
		return notApplicable
	}
	if node.Resources.CPUCoresTotal <= 0 {
		return notApplicable
	}
	remaining := node.Resources.CPUCoresFree - required
	if remaining < 0 {
		return applied(0)
	}
	return applied(float64(remaining) / float64(node.Resources.CPUCoresTotal))
}

// factorMemoryHeadroom mirrors factorCPUHeadroom for memory in megabytes.
func factorMemoryHeadroom(c *capsule.Capsule, node NodeState) factorContribution {
	required := c.Spec.Resources.MemoryMB
	if required <= 0 {
		return notApplicable
	}
	if node.Resources.MemoryMBTotal <= 0 {
		return notApplicable
	}
	remaining := node.Resources.MemoryMBFree - required
	if remaining < 0 {
		return applied(0)
	}
	return applied(float64(remaining) / float64(node.Resources.MemoryMBTotal))
}

// factorDiskHeadroom mirrors factorCPUHeadroom for disk in megabytes.
func factorDiskHeadroom(c *capsule.Capsule, node NodeState) factorContribution {
	required := c.Spec.Resources.DiskMB
	if required <= 0 {
		return notApplicable
	}
	if node.Resources.DiskMBTotal <= 0 {
		return notApplicable
	}
	remaining := node.Resources.DiskMBFree - required
	if remaining < 0 {
		return applied(0)
	}
	return applied(float64(remaining) / float64(node.Resources.DiskMBTotal))
}

// factorSoftPlacementMatch rewards nodes that satisfy soft placement rules
// (required: false). The score is the fraction of soft rules the node
// satisfies, in [0, 1]. Hard rules are not counted here — they are
// already enforced by the eligibility filter.
//
// Returns notApplicable when the capsule has no soft placement rules.
func factorSoftPlacementMatch(c *capsule.Capsule, node NodeState, lookup CapsuleTargetLookup) factorContribution {
	var total, matched int
	for _, rule := range c.Spec.PlacementRules {
		if rule.Required {
			continue
		}
		total++
		if ruleMatchesNode(rule, node, lookup) {
			matched++
		}
	}
	if total == 0 {
		return notApplicable
	}
	return applied(float64(matched) / float64(total))
}

// factorAffinityProximity rewards nodes that are co-located with the
// capsules referenced by mode: near placement rules. Co-location is
// defined as running on the same node as a target replica — the strongest
// form of affinity available in v1.
//
// Returns notApplicable when the capsule has no near rules.
func factorAffinityProximity(c *capsule.Capsule, node NodeState, lookup CapsuleTargetLookup) factorContribution {
	if lookup == nil {
		return notApplicable
	}
	var total, satisfied int
	for _, rule := range c.Spec.PlacementRules {
		if rule.Type != capsule.PlacementTypeEnum.Capsule() {
			continue
		}
		if rule.Mode != capsule.PlacementModeEnum.Near() {
			continue
		}
		total++
		for _, name := range rule.Names {
			runners := lookup.NodesRunningCapsule(node.ClusterPath, name)
			if containsString(runners, node.NodeID) {
				satisfied++
				break
			}
		}
	}
	if total == 0 {
		return notApplicable
	}
	return applied(float64(satisfied) / float64(total))
}

// factorHardwareLabelMatch rewards nodes whose labels include capsule-spec
// labels (used as soft hardware preferences). The score is the fraction of
// capsule labels found on the node, in [0, 1].
//
// Returns notApplicable when the capsule has no labels.
func factorHardwareLabelMatch(c *capsule.Capsule, node NodeState) factorContribution {
	if len(c.Spec.Labels) == 0 {
		return notApplicable
	}
	var matched int
	for k, v := range c.Spec.Labels {
		if nv, ok := node.Labels[k]; ok && nv == v {
			matched++
		}
	}
	return applied(float64(matched) / float64(len(c.Spec.Labels)))
}

// factorLoadPenalty discourages stacking many capsules on the same node.
// The score is `1 - utilization` where utilization is the running capsule
// count expressed as a fraction of a soft cap. Above the soft cap the
// penalty saturates at 0.
//
// The penalty is *always* applied — every node has some load, so the
// factor is meaningful for every election.
func factorLoadPenalty(node NodeState) factorContribution {
	const softCap = 50.0 // soft cap on capsules per node before saturating
	utilization := float64(node.RunningCapsuleCount) / softCap
	if utilization > 1 {
		utilization = 1
	}
	return applied(1 - utilization)
}

// factorReliability rewards nodes with high historical reliability scores
// from the phonebook. The score is the reliability value clamped to [0, 1].
//
// The factor is always applied because every node has a reliability
// history (defaulting to 1.0 for newly-joined nodes).
func factorReliability(node NodeState) factorContribution {
	return applied(node.ReliabilityScore)
}

// factorDiversity rewards spreading replicas across distinct datacenters.
// It only applies when the capsule asks for more than one replica AND the
// node has a datacenter label. The score is binary in v1: 1.0 if the
// node's datacenter is non-empty, 0 otherwise. A future enhancement can
// inspect the actual placement of other replicas via the lookup interface.
//
// Returns notApplicable when the capsule does not request multiple replicas.
func factorDiversity(c *capsule.Capsule, node NodeState) factorContribution {
	if c.Spec.Replicas.Min < 2 && c.Spec.Replicas.Exact < 2 {
		return notApplicable
	}
	if node.Datacenter == "" {
		return applied(0)
	}
	return applied(1)
}

// factorSnapshotLocality rewards nodes that already have a local
// snapshot for the capsule. Restoring from a local snapshot avoids
// the transfer latency, making startup significantly faster.
//
// The factor is binary: 1.0 if the node has the snapshot, 0.0 if not.
// The weight (default 0.6) is intentionally capped below resource
// factors so a heavily loaded node with a snapshot still loses to an
// empty node without one — preventing snapshot-holders from becoming
// overloaded. Returns notApplicable when no SnapshotLookup is wired.
func factorSnapshotLocality(c *capsule.Capsule, snapLookup SnapshotLookup) factorContribution {
	if snapLookup == nil {
		return notApplicable
	}
	tag := c.Spec.ImageDigest
	if tag == "" {
		tag = c.Spec.Image // fall back to image ref if no digest
	}
	if snapLookup.HasLocalSnapshot(c.ID.String(), tag) {
		return applied(1)
	}
	return applied(0)
}
