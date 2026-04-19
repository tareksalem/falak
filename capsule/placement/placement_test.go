package placement

import (
	"testing"

	"github.com/tareksalem/falak/capsule"
)

// --- Mock EntityProvider ---

type mockProvider struct {
	nodes       []Entity
	clusters    []Entity
	datacenters []Entity
	capsuleNodes map[string][]Entity // capsuleName -> nodes
}

func (m *mockProvider) Nodes() []Entity       { return m.nodes }
func (m *mockProvider) Clusters() []Entity    { return m.clusters }
func (m *mockProvider) Datacenters() []Entity { return m.datacenters }
func (m *mockProvider) CapsuleNodes(names []string, _ capsule.Labels) []Entity {
	var result []Entity
	for _, name := range names {
		if nodes, ok := m.capsuleNodes[name]; ok {
			result = append(result, nodes...)
		}
	}
	return result
}

// --- Selector Tests ---

func TestSelectByName(t *testing.T) {
	entities := []Entity{
		{Name: "node1"},
		{Name: "node2"},
		{Name: "node3"},
	}

	result := SelectByName(entities, []string{"node1", "node3"})
	if len(result) != 2 {
		t.Fatalf("expected 2, got %d", len(result))
	}
}

func TestSelectByLabels(t *testing.T) {
	entities := []Entity{
		{Name: "n1", Labels: capsule.Labels{"region": "us-east", "gpu": "true"}},
		{Name: "n2", Labels: capsule.Labels{"region": "us-east", "gpu": "false"}},
		{Name: "n3", Labels: capsule.Labels{"region": "eu-west", "gpu": "true"}},
	}

	result := SelectByLabels(entities, capsule.Labels{"gpu": "true"})
	if len(result) != 2 {
		t.Fatalf("expected 2 gpu nodes, got %d", len(result))
	}
}

// --- Evaluator Tests ---

func TestEvaluateDirectNodeRule(t *testing.T) {
	provider := &mockProvider{
		nodes: []Entity{
			{Name: "node1", Type: capsule.PlacementTypeEnum.Node(), Labels: capsule.Labels{"gpu": "true", "region": "us-east"}},
			{Name: "node2", Type: capsule.PlacementTypeEnum.Node(), Labels: capsule.Labels{"gpu": "false", "region": "us-east"}},
			{Name: "node3", Type: capsule.PlacementTypeEnum.Node(), Labels: capsule.Labels{"gpu": "true", "region": "eu-west"}},
		},
	}

	eval := NewEvaluator(WithProvider(provider))

	rules := []Rule{
		{
			Name:     "gpu nodes",
			Type:     capsule.PlacementTypeEnum.Node(),
			Labels:   capsule.Labels{"gpu": "true"},
			Required: true,
		},
	}

	eligible := eval.EligibleNodes(rules)
	if len(eligible) != 2 {
		t.Fatalf("expected 2 eligible nodes, got %d", len(eligible))
	}

	for _, r := range eligible {
		if r.NodeName != "node1" && r.NodeName != "node3" {
			t.Errorf("unexpected eligible node: %s", r.NodeName)
		}
	}
}

func TestEvaluateSoftRule(t *testing.T) {
	provider := &mockProvider{
		nodes: []Entity{
			{Name: "n1", Labels: capsule.Labels{"gpu": "true", "ssd": "true"}},
			{Name: "n2", Labels: capsule.Labels{"gpu": "true", "ssd": "false"}},
		},
	}

	eval := NewEvaluator(WithProvider(provider))

	rules := []Rule{
		{
			Name:     "gpu required",
			Type:     capsule.PlacementTypeEnum.Node(),
			Labels:   capsule.Labels{"gpu": "true"},
			Required: true,
		},
		{
			Name:     "prefer ssd",
			Type:     capsule.PlacementTypeEnum.Node(),
			Labels:   capsule.Labels{"ssd": "true"},
			Required: false,
		},
	}

	results := eval.EligibleNodes(rules)
	if len(results) != 2 {
		t.Fatalf("both nodes should be eligible, got %d", len(results))
	}

	// Find scores
	var n1Score, n2Score float64
	for _, r := range results {
		if r.NodeName == "n1" {
			n1Score = r.Score
		}
		if r.NodeName == "n2" {
			n2Score = r.Score
		}
	}

	if n1Score <= n2Score {
		t.Error("n1 (with ssd) should have higher score than n2")
	}
}

func TestEvaluateCapsuleAffinity(t *testing.T) {
	provider := &mockProvider{
		nodes: []Entity{
			{Name: "n1", Labels: capsule.Labels{"datacenter": "dc1", "region": "us"}},
			{Name: "n2", Labels: capsule.Labels{"datacenter": "dc2", "region": "us"}},
			{Name: "n3", Labels: capsule.Labels{"datacenter": "dc1", "region": "eu"}},
		},
		capsuleNodes: map[string][]Entity{
			"capsule-db": {
				{Name: "n1", Labels: capsule.Labels{"datacenter": "dc1", "region": "us"}},
			},
		},
	}

	eval := NewEvaluator(WithProvider(provider))

	rules := []Rule{
		{
			Name:     "near db",
			Type:     capsule.PlacementTypeEnum.Capsule(),
			Mode:     capsule.PlacementModeEnum.Near(),
			Names:    []string{"capsule-db"},
			Labels:   capsule.Labels{"datacenter": capsule.SameKeyword},
			Required: true,
		},
	}

	eligible := eval.EligibleNodes(rules)

	// Only n1 shares datacenter "dc1" with capsule-db's node (n1)
	// n3 also has dc1 but different region — but we only match on datacenter
	eligibleNames := make(map[string]bool)
	for _, r := range eligible {
		eligibleNames[r.NodeName] = true
	}

	if !eligibleNames["n1"] {
		t.Error("n1 should be eligible (same datacenter as capsule-db)")
	}
	if !eligibleNames["n3"] {
		t.Error("n3 should be eligible (same datacenter dc1)")
	}
	if eligibleNames["n2"] {
		t.Error("n2 should NOT be eligible (different datacenter)")
	}
}

func TestEvaluateCapsuleAntiAffinity(t *testing.T) {
	provider := &mockProvider{
		nodes: []Entity{
			{Name: "n1", Labels: capsule.Labels{"node": "n1"}},
			{Name: "n2", Labels: capsule.Labels{"node": "n2"}},
		},
		capsuleNodes: map[string][]Entity{
			"capsule-old": {
				{Name: "n1", Labels: capsule.Labels{"node": "n1"}},
			},
		},
	}

	eval := NewEvaluator(WithProvider(provider))

	rules := []Rule{
		{
			Name:     "away from old",
			Type:     capsule.PlacementTypeEnum.Capsule(),
			Mode:     capsule.PlacementModeEnum.Away(),
			Names:    []string{"capsule-old"},
			Labels:   capsule.Labels{"node": capsule.SameKeyword},
			Required: true,
		},
	}

	eligible := eval.EligibleNodes(rules)
	if len(eligible) != 1 {
		t.Fatalf("expected 1 eligible node, got %d", len(eligible))
	}
	if eligible[0].NodeName != "n2" {
		t.Errorf("expected n2, got %s", eligible[0].NodeName)
	}
}

func TestEvaluateClusterRule(t *testing.T) {
	provider := &mockProvider{
		nodes: []Entity{
			{Name: "n1", Labels: capsule.Labels{"cluster": "prod-1"}},
			{Name: "n2", Labels: capsule.Labels{"cluster": "prod-2"}},
			{Name: "n3", Labels: capsule.Labels{"cluster": "staging"}},
		},
		clusters: []Entity{
			{Name: "prod-1"},
			{Name: "prod-2"},
			{Name: "staging"},
		},
	}

	eval := NewEvaluator(WithProvider(provider))

	rules := []Rule{
		{
			Name:     "prod clusters only",
			Type:     capsule.PlacementTypeEnum.Cluster(),
			Names:    []string{"prod-1", "prod-2"},
			Labels:   capsule.Labels{},
			Required: true,
		},
	}

	eligible := eval.EligibleNodes(rules)
	if len(eligible) != 2 {
		t.Fatalf("expected 2 eligible nodes, got %d", len(eligible))
	}
}
