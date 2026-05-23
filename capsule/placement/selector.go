package placement

import (
	"github.com/tareksalem/falak/capsule"
	enums "github.com/tareksalem/falak/capsule/enums"
)

// Entity represents any entity in the mesh that can be targeted by placement rules.
type Entity struct {
	Name   string
	Type   enums.PlacementType
	Labels capsule.Labels
}

// EntityProvider supplies the current state of entities in the mesh for placement evaluation.
type EntityProvider interface {
	// Nodes returns all known nodes with their labels.
	Nodes() []Entity

	// Clusters returns all known clusters with their labels.
	Clusters() []Entity

	// Datacenters returns all known datacenters with their labels.
	Datacenters() []Entity

	// CapsuleNodes returns the entities (nodes) where a capsule is currently running.
	// The capsule is identified by name or labels.
	CapsuleNodes(names []string, labels capsule.Labels) []Entity
}

// SelectByName filters entities that match any of the given names.
func SelectByName(entities []Entity, names []string) []Entity {
	if len(names) == 0 {
		return entities
	}
	nameSet := make(map[string]struct{}, len(names))
	for _, n := range names {
		nameSet[n] = struct{}{}
	}

	var result []Entity
	for _, e := range entities {
		if _, ok := nameSet[e.Name]; ok {
			result = append(result, e)
		}
	}
	return result
}

// SelectByLabels filters entities whose labels match all the required labels.
func SelectByLabels(entities []Entity, required capsule.Labels) []Entity {
	if len(required) == 0 {
		return entities
	}

	var result []Entity
	for _, e := range entities {
		if capsule.LabelsMatch(e.Labels, required) {
			result = append(result, e)
		}
	}
	return result
}

// Select filters entities by both name and labels. Both filters apply (AND logic).
func Select(entities []Entity, names []string, labels capsule.Labels) []Entity {
	result := entities
	if len(names) > 0 {
		result = SelectByName(result, names)
	}
	if len(labels) > 0 {
		result = SelectByLabels(result, labels)
	}
	return result
}
