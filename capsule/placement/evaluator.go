package placement

import (
	"github.com/tareksalem/falak/capsule"
	enums "github.com/tareksalem/falak/capsule/enums"
)

// Result holds the evaluation outcome for a single node against all placement rules.
type Result struct {
	NodeName    string
	Eligible    bool    // true if all hard rules pass
	Score       float64 // soft rules contribute to score (higher = better fit)
	FailedRules []string
	MatchedSoft []string
}

// Evaluator evaluates placement rules against the current mesh state.
type Evaluator struct {
	provider EntityProvider
}

// EvaluatorOption configures an Evaluator.
type EvaluatorOption func(*Evaluator)

// WithProvider sets the entity provider.
func WithProvider(p EntityProvider) EvaluatorOption {
	return func(e *Evaluator) {
		e.provider = p
	}
}

// NewEvaluator creates a new placement rule evaluator.
func NewEvaluator(opts ...EvaluatorOption) *Evaluator {
	e := &Evaluator{}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// EvaluateNode checks if a single node satisfies the given placement rules.
// Returns the evaluation result with eligibility and score.
func (e *Evaluator) EvaluateNode(node Entity, rules []Rule) Result {
	result := Result{
		NodeName: node.Name,
		Eligible: true,
	}

	for _, rule := range rules {
		passed := e.evaluateRule(node, rule)

		if !passed && rule.Required {
			result.Eligible = false
			result.FailedRules = append(result.FailedRules, rule.Name)
		}
		if passed && !rule.Required {
			result.Score += 1.0
			result.MatchedSoft = append(result.MatchedSoft, rule.Name)
		}
	}

	return result
}

// EvaluateAll checks all available nodes against the placement rules.
// Returns results for every node, sorted by eligibility then score.
func (e *Evaluator) EvaluateAll(rules []Rule) []Result {
	nodes := e.provider.Nodes()
	results := make([]Result, 0, len(nodes))

	for _, node := range nodes {
		result := e.EvaluateNode(node, rules)
		results = append(results, result)
	}

	return results
}

// EligibleNodes returns only the nodes that pass all hard placement rules.
func (e *Evaluator) EligibleNodes(rules []Rule) []Result {
	all := e.EvaluateAll(rules)
	var eligible []Result
	for _, r := range all {
		if r.Eligible {
			eligible = append(eligible, r)
		}
	}
	return eligible
}

func (e *Evaluator) evaluateRule(node Entity, rule Rule) bool {
	switch rule.Type {
	case enums.PlacementTypeEnum.Node():
		return e.evaluateDirectRule(node, rule)
	case enums.PlacementTypeEnum.Cluster():
		return e.evaluateClusterRule(node, rule)
	case enums.PlacementTypeEnum.Datacenter():
		return e.evaluateDatacenterRule(node, rule)
	case enums.PlacementTypeEnum.Capsule():
		return e.evaluateCapsuleRule(node, rule)
	default:
		return false
	}
}

// evaluateDirectRule checks if the node directly matches the rule's names and labels.
func (e *Evaluator) evaluateDirectRule(node Entity, rule Rule) bool {
	if len(rule.Names) > 0 {
		found := false
		for _, name := range rule.Names {
			if node.Name == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	// For direct rules, labels are exact match (key=value)
	return capsule.LabelsMatch(node.Labels, rule.Labels)
}

// evaluateClusterRule checks if the node belongs to a cluster matching the rule.
func (e *Evaluator) evaluateClusterRule(node Entity, rule Rule) bool {
	clusters := e.provider.Clusters()
	matching := Select(clusters, rule.Names, rule.Labels)

	// Check if the node's cluster label matches any selected cluster
	nodeCluster, ok := node.Labels["cluster"]
	if !ok {
		return false
	}
	for _, c := range matching {
		if c.Name == nodeCluster {
			return true
		}
	}
	return false
}

// evaluateDatacenterRule checks if the node is in a datacenter matching the rule.
func (e *Evaluator) evaluateDatacenterRule(node Entity, rule Rule) bool {
	datacenters := e.provider.Datacenters()
	matching := Select(datacenters, rule.Names, rule.Labels)

	nodeDC, ok := node.Labels["datacenter"]
	if !ok {
		return false
	}
	for _, dc := range matching {
		if dc.Name == nodeDC {
			return true
		}
	}
	return false
}

// evaluateCapsuleRule checks capsule affinity/anti-affinity.
func (e *Evaluator) evaluateCapsuleRule(node Entity, rule Rule) bool {
	// Find nodes where the target capsule is running
	targetNodes := e.provider.CapsuleNodes(rule.Names, nil)
	if len(targetNodes) == 0 {
		// Target capsule not running anywhere.
		// - Near affinity: can't satisfy — fail (no place to be near).
		// - Away anti-affinity: trivially satisfied — pass (nothing to avoid).
		if rule.IsAway() {
			return true
		}
		return false
	}

	sameKeys := rule.SameKeys()
	if len(sameKeys) == 0 {
		return true
	}

	// Check if node shares the same label values as any target capsule node
	sharesLabels := false
	for _, targetNode := range targetNodes {
		allMatch := true
		for _, key := range sameKeys {
			nodeVal, nodeOK := node.Labels[key]
			targetVal, targetOK := targetNode.Labels[key]
			if !nodeOK || !targetOK || nodeVal != targetVal {
				allMatch = false
				break
			}
		}
		if allMatch {
			sharesLabels = true
			break
		}
	}

	if rule.IsNear() {
		return sharesLabels
	}
	if rule.IsAway() {
		return !sharesLabels
	}

	return false
}
