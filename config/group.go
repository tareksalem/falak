package config

import (
	"errors"
	"fmt"

	"github.com/tareksalem/falak/capsule"
)

// Phase 10 dead-field rejection sentinels. These mirror the rules in
// `.claude/plans/capsule-groups.md` "V1 cut" — fields whose runtime
// behaviour ships in Phase 11+ MUST be rejected at admission so users
// don't silently rely on no-op spec fields.
var (
	// ErrConfigPhase11FieldReplicaLabels fires when a #CapsuleMember has
	// a non-empty replica_labels list. The runtime injection lands in
	// Phase 11.
	ErrConfigPhase11FieldReplicaLabels = errors.New("config: member field 'replica_labels' is reserved for Phase 11 and is not yet supported")

	// ErrConfigPhase11FieldDiscovers fires when a #CapsuleMember has a
	// non-empty discovers list. Cross-group eager subscription lands in
	// Phase 11.
	ErrConfigPhase11FieldDiscovers = errors.New("config: member field 'discovers' is reserved for Phase 11 and is not yet supported")
)

// IsCapsuleKindGroup returns true when the config refers to a CapsuleGroup
// (kind == "group"). Empty kind defaults to "capsule".
func (c *CapsuleConfig) IsCapsuleKindGroup() bool {
	return c.Kind == string(capsule.CapsuleKindEnum.Group())
}

// ToGroupSpec converts a group-kind CapsuleConfig into a group name plus
// `capsule.GroupSpec` suitable for `capsule.Manager.CreateGroup`. It
// also returns the merged base labels for the group (used as the seed
// for member-label inheritance).
//
// Returns an error when:
//   - The CapsuleConfig is not kind=group.
//   - The Group sub-block is nil.
//   - Any member sets a Phase 11+ field (replica_labels, discovers).
//   - Any member's nested spec fails to convert.
//
// Validation of the group-level invariants (DAG cycles, name format,
// member spec contents) is performed by `capsule.ValidateGroupSpec` at
// the Manager seam — this function only handles the config→Go shape
// translation and the dead-field gate.
func (c *CapsuleConfig) ToGroupSpec() (groupName string, spec capsule.GroupSpec, baseLabels capsule.Labels, err error) {
	if !c.IsCapsuleKindGroup() {
		return "", capsule.GroupSpec{}, nil, fmt.Errorf("config: capsule %q is not kind=group", c.Name)
	}
	if c.Group == nil {
		return "", capsule.GroupSpec{}, nil, fmt.Errorf("config: capsule %q has kind=group but no group block", c.Name)
	}

	colocation := capsule.ColocationModeEnum.SameOrbit()
	switch c.Group.Colocation {
	case "", string(capsule.ColocationModeEnum.SameOrbit()):
		colocation = capsule.ColocationModeEnum.SameOrbit()
	case string(capsule.ColocationModeEnum.SameNode()):
		colocation = capsule.ColocationModeEnum.SameNode()
	default:
		return "", capsule.GroupSpec{}, nil, fmt.Errorf("config: capsule %q has invalid group.colocation %q", c.Name, c.Group.Colocation)
	}

	cascade := true
	if c.Group.CascadeDelete != nil {
		cascade = *c.Group.CascadeDelete
	}

	var memberErrs []error
	members := make([]capsule.MemberSpec, 0, len(c.Group.Members))
	for memberName, m := range c.Group.Members {
		// The map key is authoritative; reject inconsistent name fields.
		if m.Name != "" && m.Name != memberName {
			memberErrs = append(memberErrs, fmt.Errorf("config: group %q member %q has mismatched inner name %q", c.Name, memberName, m.Name))
			continue
		}
		m.Name = memberName

		if len(m.ReplicaLabels) > 0 {
			memberErrs = append(memberErrs, fmt.Errorf("group %q member %q: %w", c.Name, memberName, ErrConfigPhase11FieldReplicaLabels))
		}
		if len(m.Discovers) > 0 {
			memberErrs = append(memberErrs, fmt.Errorf("group %q member %q: %w", c.Name, memberName, ErrConfigPhase11FieldDiscovers))
		}

		nested, convErr := m.toCapsuleSpec()
		if convErr != nil {
			memberErrs = append(memberErrs, fmt.Errorf("group %q member %q: %w", c.Name, memberName, convErr))
			continue
		}

		members = append(members, capsule.MemberSpec{
			Name:      memberName,
			Spec:      nested,
			DependsOn: append([]string(nil), m.DependsOn...),
		})
	}

	if joined := errors.Join(memberErrs...); joined != nil {
		return "", capsule.GroupSpec{}, nil, joined
	}

	spec = capsule.GroupSpec{
		Colocation:    colocation,
		Members:       members,
		CascadeDelete: cascade,
	}

	baseLabels = capsule.Labels{}
	for k, v := range c.Labels {
		baseLabels[k] = v
	}

	return c.Name, spec, baseLabels, nil
}

// toCapsuleSpec converts a CapsuleMemberConfig into a capsule.CapsuleSpec
// suitable for embedding inside a MemberSpec. Reuses the existing
// CapsuleConfig→CapsuleSpec converter by funneling through a synthetic
// CapsuleConfig (kind=capsule).
//
// Phase 11+ fields are NOT carried into the inner spec — they're caught
// at the group-level dead-field gate above, before this runs.
func (m *CapsuleMemberConfig) toCapsuleSpec() (capsule.CapsuleSpec, error) {
	syn := &CapsuleConfig{
		Name:        m.Name,
		Kind:        string(capsule.CapsuleKindEnum.Capsule()),
		Image:       m.Image,
		ImageAlias:  m.ImageAlias,
		ImageDigest: m.ImageDigest,
		Orbit:       m.Orbit,
		Tier:        m.Tier,
		Labels:      m.Labels,
		Command:     m.Command,
		Resources:   m.Resources,
		Replicas:    m.Replicas,
		Scaling:     m.Scaling,
		Placement:   m.Placement,
		Runtime:     m.Runtime,
		Advanced:    m.Advanced,
	}
	return syn.ToCapsuleSpec()
}
