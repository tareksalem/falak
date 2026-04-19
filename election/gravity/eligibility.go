package gravity

import (
	"github.com/tareksalem/falak/capsule"
)

// Ineligibility describes why a node was rejected by IsEligible. It is the
// pair of (rejection reason, supplementary detail) so callers can log or
// surface the failure to operators.
type Ineligibility struct {
	Reason IneligibilityReason
	Detail string
}

// IneligibilityReason categorizes the eligibility check that failed.
// Concrete reasons mirror the three filters documented in the election plan.
type IneligibilityReason string

// Private backing constants for the IneligibilityReason enum. External
// callers access them through the IneligibilityReasonEnum accessor.
const (
	ineligibilityOK              IneligibilityReason = ""
	ineligibilityResourcesCPU    IneligibilityReason = "resources.cpu"
	ineligibilityResourcesMemory IneligibilityReason = "resources.memory"
	ineligibilityResourcesDisk   IneligibilityReason = "resources.disk"
	ineligibilityNotHealthy      IneligibilityReason = "node.unhealthy"
	ineligibilityPlacementRule   IneligibilityReason = "placement.rule"
	ineligibilityExcluded        IneligibilityReason = "excluded"
)

// ineligibilityReasonEnum is the unexported carrier struct used to expose
// the valid IneligibilityReason values as methods on a single
// package-level accessor.
type ineligibilityReasonEnum struct{}

// IneligibilityReasonEnum is the public accessor for IneligibilityReason
// values. Use gravity.IneligibilityReasonEnum.OK() instead of a bare
// constant.
var IneligibilityReasonEnum ineligibilityReasonEnum

// OK returns the zero-value reason — the node passed every check.
func (ineligibilityReasonEnum) OK() IneligibilityReason { return ineligibilityOK }

// ResourcesCPU returns the reason "node has insufficient free CPU".
func (ineligibilityReasonEnum) ResourcesCPU() IneligibilityReason {
	return ineligibilityResourcesCPU
}

// ResourcesMemory returns the reason "node has insufficient free memory".
func (ineligibilityReasonEnum) ResourcesMemory() IneligibilityReason {
	return ineligibilityResourcesMemory
}

// ResourcesDisk returns the reason "node has insufficient free disk".
func (ineligibilityReasonEnum) ResourcesDisk() IneligibilityReason {
	return ineligibilityResourcesDisk
}

// NotHealthy returns the reason "node is not in the Active health state".
func (ineligibilityReasonEnum) NotHealthy() IneligibilityReason { return ineligibilityNotHealthy }

// PlacementRule returns the reason "a hard placement rule failed".
func (ineligibilityReasonEnum) PlacementRule() IneligibilityReason {
	return ineligibilityPlacementRule
}

// Excluded returns the reason "the node is on the request's exclude list".
func (ineligibilityReasonEnum) Excluded() IneligibilityReason { return ineligibilityExcluded }

// PassedEligibility returns whether the node passed all eligibility checks.
func (i Ineligibility) PassedEligibility() bool {
	return i.Reason == ineligibilityOK
}

// IsEligible runs the binary eligibility filter for a node against a capsule.
//
// The filter is intentionally fast and pure: it checks only conditions that
// can disqualify a node entirely (resources, hard placement, health,
// anti-affinity to self). Soft preferences and gradual penalties belong in
// the scoring layer (gravity.go), not here.
//
// targetLookup is used to evaluate capsule affinity/anti-affinity placement
// rules AND the implicit self-anti-affinity rule (a node never runs two
// replicas of the same capsule). It returns the list of nodes currently
// hosting a given capsule, in the same cluster. Pass nil if the calculator
// does not need to evaluate capsule-typed rules; rules of that type (and
// the self-anti-affinity check) will then be skipped so eligibility never
// fails for missing data.
func IsEligible(
	c *capsule.Capsule,
	node NodeState,
	targetLookup CapsuleTargetLookup,
) Ineligibility {
	// Self-anti-affinity: a node never runs two replicas of the same
	// capsule. When a lookup is available, the election skips any node
	// that is already hosting this capsule. This replaces the previous
	// ExcludeNodes protocol field — anti-affinity is now a local,
	// capsule-driven rule rather than an externally-passed list.
	if targetLookup != nil {
		if containsString(targetLookup.NodesRunningCapsule(node.ClusterPath, c.Spec.Name), node.NodeID) {
			return Ineligibility{
				Reason: IneligibilityReasonEnum.Excluded(),
				Detail: "node " + node.NodeID + " is already running capsule " + c.Spec.Name,
			}
		}
	}

	// Health is the cheapest check; do it first.
	if !node.Status.IsHealthy() {
		return Ineligibility{
			Reason: IneligibilityReasonEnum.NotHealthy(),
			Detail: "node status is " + string(node.Status),
		}
	}

	// Resource availability — every required resource must fit in the
	// node's free pool. Tolerate zero/unset values: a capsule that does
	// not declare a resource requirement passes the check trivially.
	if c.Spec.Resources.CPUCores > 0 && node.Resources.CPUCoresFree < c.Spec.Resources.CPUCores {
		return Ineligibility{
			Reason: IneligibilityReasonEnum.ResourcesCPU(),
			Detail: "insufficient free CPU",
		}
	}
	if c.Spec.Resources.MemoryMB > 0 && node.Resources.MemoryMBFree < c.Spec.Resources.MemoryMB {
		return Ineligibility{
			Reason: IneligibilityReasonEnum.ResourcesMemory(),
			Detail: "insufficient free memory",
		}
	}
	if c.Spec.Resources.DiskMB > 0 && node.Resources.DiskMBFree < c.Spec.Resources.DiskMB {
		return Ineligibility{
			Reason: IneligibilityReasonEnum.ResourcesDisk(),
			Detail: "insufficient free disk",
		}
	}

	// Hard placement rules — every required: true rule must pass.
	for _, rule := range c.Spec.PlacementRules {
		if !rule.Required {
			continue
		}
		if !ruleMatchesNode(rule, node, targetLookup) {
			return Ineligibility{
				Reason: IneligibilityReasonEnum.PlacementRule(),
				Detail: "hard rule failed: " + rule.Name,
			}
		}
	}

	return Ineligibility{Reason: IneligibilityReasonEnum.OK()}
}

// CapsuleTargetLookup is the minimal interface the eligibility (and gravity)
// functions need to evaluate capsule-typed placement rules. It returns the
// nodes currently running the named capsule.
//
// Implementations are typically a thin adapter over the capsule store and
// the local view of replica placements (gossiped via capsule status updates).
type CapsuleTargetLookup interface {
	// NodesRunningCapsule returns the IDs of nodes currently hosting any
	// replica of the named capsule, in the given cluster. Returns an empty
	// slice (not nil error) when the capsule is unknown or has no replicas.
	NodesRunningCapsule(clusterPath, capsuleName string) []string
}

// ruleMatchesNode returns whether a single PlacementRule is satisfied by
// the given node. The function understands the four placement types
// (node, cluster, datacenter, capsule) and the two affinity modes
// (near, away) for capsule rules.
//
// For node/cluster/datacenter rules with no targets and no labels, the
// rule is treated as a pass: the spec validator already rejects empty
// rules at create time, but defending against it here keeps the function
// total.
func ruleMatchesNode(
	rule capsule.PlacementRule,
	node NodeState,
	lookup CapsuleTargetLookup,
) bool {
	switch rule.Type {
	case capsule.PlacementTypeEnum.Node():
		return matchNodeRule(rule, node)

	case capsule.PlacementTypeEnum.Cluster():
		return matchClusterRule(rule, node)

	case capsule.PlacementTypeEnum.Datacenter():
		return matchDatacenterRule(rule, node)

	case capsule.PlacementTypeEnum.Capsule():
		return matchCapsuleRule(rule, node, lookup)
	}
	// Unknown placement type — treat as failing so we never accidentally
	// silently allow placements with malformed rules. Validation should
	// have caught this earlier.
	return false
}

// matchNodeRule matches the node's name and labels against the rule.
func matchNodeRule(rule capsule.PlacementRule, node NodeState) bool {
	if len(rule.Names) > 0 && !containsString(rule.Names, node.NodeID) {
		return false
	}
	return labelsMatchAll(node.Labels, rule.Labels)
}

// matchClusterRule matches the node's cluster path against the rule.
func matchClusterRule(rule capsule.PlacementRule, node NodeState) bool {
	if len(rule.Names) > 0 && !containsString(rule.Names, node.ClusterPath) {
		return false
	}
	// Cluster-level labels are not currently propagated to nodes, so any
	// label requirement on a cluster rule is treated as failing. This
	// will be revisited when cluster metadata gossip is added.
	return len(rule.Labels) == 0
}

// matchDatacenterRule matches the node's datacenter against the rule.
func matchDatacenterRule(rule capsule.PlacementRule, node NodeState) bool {
	if len(rule.Names) > 0 && !containsString(rule.Names, node.Datacenter) {
		return false
	}
	return len(rule.Labels) == 0
}

// matchCapsuleRule evaluates affinity/anti-affinity against another capsule.
//
//   - mode "near": the node must be co-located with at least one runner of
//     the target capsule, sharing the labels listed under labels[key]="same".
//   - mode "away": the node must NOT share those labels with any runner of
//     the target capsule.
//
// If the target capsule is unknown (returns no runners), near rules cannot
// be satisfied (false) and away rules are trivially satisfied (true).
func matchCapsuleRule(rule capsule.PlacementRule, node NodeState, lookup CapsuleTargetLookup) bool {
	if lookup == nil || len(rule.Names) == 0 {
		// We cannot evaluate the rule. Treat near as failing (cannot
		// guarantee proximity) and away as passing (cannot prove conflict).
		return rule.Mode == capsule.PlacementModeEnum.Away()
	}

	// Collect the set of node IDs running any of the target capsules.
	var targetIDs []string
	for _, name := range rule.Names {
		targetIDs = append(targetIDs, lookup.NodesRunningCapsule(node.ClusterPath, name)...)
	}
	if len(targetIDs) == 0 {
		return rule.Mode == capsule.PlacementModeEnum.Away()
	}

	// For "same"-keyword labels we need to look up each target node's
	// labels to compare. The state provider does not give us labels for
	// arbitrary node IDs from inside this function — instead the
	// CapsuleTargetLookup adapter is expected to provide them via
	// LabelsForNode below. To avoid expanding the interface here, we
	// fall back to a simple "is the local node also a runner?" check
	// for near rules, which covers the common case (same node).
	//
	// A richer comparison (same datacenter, same region, …) is delegated
	// to gravity scoring where we have full StateProvider access.
	isAlsoRunner := containsString(targetIDs, node.NodeID)
	if rule.Mode == capsule.PlacementModeEnum.Near() {
		return isAlsoRunner
	}
	if rule.Mode == capsule.PlacementModeEnum.Away() {
		return !isAlsoRunner
	}
	return false
}

// labelsMatchAll returns true when every (key, value) in required is
// present in candidate with an equal value.
func labelsMatchAll(candidate, required capsule.Labels) bool {
	for k, v := range required {
		if cv, ok := candidate[k]; !ok || cv != v {
			return false
		}
	}
	return true
}

// containsString returns true when haystack contains needle.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
