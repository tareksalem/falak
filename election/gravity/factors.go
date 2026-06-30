package gravity

import (
	"math"
	"time"

	"github.com/tareksalem/falak/capsule"
	capsuleEnums "github.com/tareksalem/falak/capsule/enums"
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
// The factor is request-independent (O9-A): when the capsule does not
// request CPU (required == 0) the formula degenerates to free/total,
// which still rewards emptier nodes by their relative free fraction.
// This is what differentiates two otherwise-identical bare capsules
// across a busy and an idle node. The only guard is total <= 0, which
// means the node has not yet reported its capacity.
func factorCPUHeadroom(c *capsule.Capsule, node NodeState) factorContribution {
	required := c.Spec.Resources.CPUCores
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
// Request-independent (O9-A): required == 0 yields free/total.
func factorMemoryHeadroom(c *capsule.Capsule, node NodeState) factorContribution {
	required := c.Spec.Resources.MemoryMB
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
// Request-independent (O9-A): required == 0 yields free/total.
func factorDiskHeadroom(c *capsule.Capsule, node NodeState) factorContribution {
	required := c.Spec.Resources.DiskMB
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
		if rule.Type != capsuleEnums.PlacementTypeEnum.Capsule() {
			continue
		}
		if rule.Mode != capsuleEnums.PlacementModeEnum.Near() {
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

// factorLoadPenalty rewards nodes with free committed-capsule capacity,
// discouraging stacking many capsules on the same node. The score is
// `1 - utilization` where utilization is the running capsule count
// expressed as a fraction of a soft cap; above the soft cap the score
// saturates at 0 (no free capacity). Despite the historical name, this
// is now a positive free-capacity factor routed through the same
// add-to-numerator-and-denominator path as every other factor — it no
// longer subtracts. Keeping the count-based proxy (rather than live CPU
// utilization) matters because idle capsules consume ~0 CPU, so resource
// headroom alone cannot spread them; the committed count can.
//
// The factor is *always* applied — every node has some committed load,
// so it is meaningful for every election.
func factorLoadPenalty(node NodeState) factorContribution {
	const softCap = 20.0 // soft cap on capsules per node before saturating
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

// factorExecutionReliability rewards nodes that have historically been
// able to actually START and RUN capsules — as distinct from the
// connection-level ReliabilityScore. It is sourced from the node-local
// execution-reliability tracker (decayed Bayesian-smoothed success/
// failure counts over container starts), surfaced on NodeState by the
// metrics provider. The score is in [0, 1]; a node with no history
// scores at the optimistic prior (0.8) so a freshly-joined node is not
// starved before it has placed anything.
//
// The factor is always applied because every node has an execution
// reliability value (the prior, until it accrues history).
func factorExecutionReliability(node NodeState) factorContribution {
	return applied(node.ExecutionReliability)
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

// Snapshot-locality age-decay defaults. The bonus a snapshot-holding node
// earns is not flat: it decays from ~1.0 (just captured) toward 0 as the
// snapshot ages. A stale CRIU image can restore to a worse state than a
// clean cold start, and decaying the bonus also damps the self-reinforcing
// loop where a holder keeps winning re-elections and re-refreshing its own
// snapshot (which would otherwise pin a workload to one node forever).
const (
	// defaultSnapshotDecayHorizon is the fallback age at which the
	// snapshot-locality bonus reaches 0, used when a snapshot record carries
	// no TTL. Tied to the typical snapshot TTL (72h).
	defaultSnapshotDecayHorizon = 72 * time.Hour

	// defaultSnapshotDecayExponent shapes the decay curve. 1.0 is a straight
	// linear decay from full bonus at age 0 to zero at the horizon.
	defaultSnapshotDecayExponent = 1.0
)

// snapshotDecayConfig parameterizes how the snapshot-locality bonus decays
// with the held snapshot's age. It is configured on the Calculator via the
// WithSnapshotDecay* options and consumed by factorSnapshotLocality.
type snapshotDecayConfig struct {
	// fallbackHorizon is the decay horizon used when the snapshot record
	// reports no TTL (TTL <= 0). A snapshot whose age reaches the horizon
	// contributes zero locality bonus.
	fallbackHorizon time.Duration

	// exponent shapes the decay curve applied to the remaining-life
	// fraction (see snapshotAgeDecay). 1.0 is linear.
	exponent float64
}

// defaultSnapshotDecay returns the built-in snapshot-locality decay config.
func defaultSnapshotDecay() snapshotDecayConfig {
	return snapshotDecayConfig{
		fallbackHorizon: defaultSnapshotDecayHorizon,
		exponent:        defaultSnapshotDecayExponent,
	}
}

// snapshotAgeDecay returns the age-decayed snapshot-locality bonus in
// [0, 1]. It yields 1.0 for a fresh snapshot (age <= 0), 0 for a snapshot at
// or beyond the horizon, and a monotonically decreasing value in between
// following (1 - age/horizon)^exponent. A non-positive horizon disables
// decay (returns 1.0) — used for snapshots with no TTL only after the caller
// has already substituted the configured fallback horizon.
func snapshotAgeDecay(age, horizon time.Duration, exponent float64) float64 {
	if horizon <= 0 {
		return 1
	}
	if age <= 0 {
		return 1
	}
	if age >= horizon {
		return 0
	}
	remaining := 1 - float64(age)/float64(horizon)
	if exponent == 1 {
		return remaining
	}
	return math.Pow(remaining, exponent)
}

// factorSnapshotLocality rewards nodes that already have a local snapshot
// for the capsule. Restoring from a local snapshot avoids the transfer
// latency, making startup significantly faster.
//
// The bonus is age-decayed rather than flat: a freshly captured snapshot
// scores ~1.0 and the value falls toward 0 as the snapshot approaches its
// decay horizon (its TTL when set, otherwise the calculator's configured
// fallback horizon). See snapshotDecayConfig for why staleness is penalized.
//
// The tag is derived identically to the runtime restore path (ImageDigest,
// falling back to Image) so a snapshot the runtime would actually restore
// from is the same one this factor credits.
//
// The weight (default 0.6) is intentionally capped below the health/
// headroom factors so a heavily loaded or degraded node holding a snapshot
// still loses to a healthy empty node without one — preventing
// snapshot-holders from becoming overloaded. Returns notApplicable when no
// SnapshotLookup is wired (e.g. a calculator built without snapshot
// support), leaving other calculators unaffected.
func factorSnapshotLocality(c *capsule.Capsule, snapLookup SnapshotLookup, decay snapshotDecayConfig) factorContribution {
	if snapLookup == nil {
		return notApplicable
	}
	tag := c.Spec.ImageDigest
	if tag == "" {
		tag = c.Spec.Image // fall back to image ref if no digest
	}
	info, ok := snapLookup.LocalSnapshot(c.ID.String(), tag)
	if !ok {
		return applied(0)
	}
	horizon := info.TTL
	if horizon <= 0 {
		horizon = decay.fallbackHorizon
	}
	return applied(snapshotAgeDecay(info.Age, horizon, decay.exponent))
}
