// Package placement provides placement rule evaluation for capsule deployment decisions.
// Placement rules determine which nodes, clusters, or datacenters are eligible to run a capsule,
// and handle affinity/anti-affinity relative to other capsules.
package placement

import (
	"github.com/tareksalem/falak/capsule"
	enums "github.com/tareksalem/falak/capsule/enums"
)

// Rule is an evaluated placement rule with its parsed constraints.
type Rule struct {
	Name     string
	Type     enums.PlacementType
	Mode     enums.PlacementMode // only for Type=capsule
	Names    []string
	Labels   capsule.Labels
	Required bool
}

// FromSpec converts a capsule.PlacementRule into an evaluated Rule.
func FromSpec(spec capsule.PlacementRule) Rule {
	return Rule{
		Name:     spec.Name,
		Type:     spec.Type,
		Mode:     spec.Mode,
		Names:    spec.Names,
		Labels:   spec.Labels,
		Required: spec.Required,
	}
}

// FromSpecList converts a slice of capsule.PlacementRule into evaluated Rules.
func FromSpecList(specs []capsule.PlacementRule) []Rule {
	rules := make([]Rule, len(specs))
	for i, spec := range specs {
		rules[i] = FromSpec(spec)
	}
	return rules
}

// IsCapsuleRule returns true if this rule targets another capsule (affinity/anti-affinity).
func (r Rule) IsCapsuleRule() bool {
	return r.Type == enums.PlacementTypeEnum.Capsule()
}

// IsNear returns true if this is a near-affinity capsule rule.
func (r Rule) IsNear() bool {
	return r.IsCapsuleRule() && r.Mode == enums.PlacementModeEnum.Near()
}

// IsAway returns true if this is an anti-affinity capsule rule.
func (r Rule) IsAway() bool {
	return r.IsCapsuleRule() && r.Mode == enums.PlacementModeEnum.Away()
}

// SameKeys returns the label keys that use the "same" keyword for comparison.
func (r Rule) SameKeys() []string {
	var keys []string
	for key, val := range r.Labels {
		if val == capsule.SameKeyword {
			keys = append(keys, key)
		}
	}
	return keys
}
