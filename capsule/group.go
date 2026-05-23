package capsule

// SystemGroupOrbit is the reserved gossipsub topic on which group-kind
// capsules are announced. Members are still announced on their own
// declared orbits; the system orbit only carries the group coordinator
// row so peers learn the membership graph + cascade-delete contract.
//
// Every cluster member auto-subscribes to this orbit at cluster setup
// time. The "__" prefix marks it as a reserved system topic — DNS-label
// validation on user orbit names excludes leading underscores so there
// is no collision risk.
const SystemGroupOrbit = "__falak/groups"

// --- CapsuleKind Enum ---

// CapsuleKind discriminates standalone capsules from group-kind capsules.
// A capsule with Kind == Group has no workload of its own — it carries a
// GroupSpec describing its members. A capsule with Kind == Capsule is a
// standard standalone workload (the default).
type CapsuleKind string

const (
	capsuleKindCapsule CapsuleKind = "capsule"
	capsuleKindGroup   CapsuleKind = "group"
)

type capsuleKindEnum struct{}

// CapsuleKindEnum provides access to CapsuleKind values.
var CapsuleKindEnum capsuleKindEnum

// Capsule returns the CapsuleKind value for standalone capsules.
func (capsuleKindEnum) Capsule() CapsuleKind { return capsuleKindCapsule }

// Group returns the CapsuleKind value for group-kind capsules.
func (capsuleKindEnum) Group() CapsuleKind { return capsuleKindGroup }

// Valid returns true if the kind is a known CapsuleKind value.
func (k CapsuleKind) Valid() bool {
	switch k {
	case capsuleKindCapsule, capsuleKindGroup:
		return true
	}
	return false
}

// --- ColocationMode Enum ---

// ColocationMode controls whether group members must share a node.
// SameNode is atomic — every member of the group lands on a single node
// together. SameOrbit places members independently by gravity within the
// same orbit; members may end up on different nodes.
type ColocationMode string

const (
	colocationSameNode  ColocationMode = "same-node"
	colocationSameOrbit ColocationMode = "same-orbit"
)

type colocationModeEnum struct{}

// ColocationModeEnum provides access to ColocationMode values.
var ColocationModeEnum colocationModeEnum

// SameNode returns the ColocationMode value for atomic single-node placement.
func (colocationModeEnum) SameNode() ColocationMode { return colocationSameNode }

// SameOrbit returns the ColocationMode value for independent per-member placement.
func (colocationModeEnum) SameOrbit() ColocationMode { return colocationSameOrbit }

// Valid returns true if the mode is a known ColocationMode value.
func (m ColocationMode) Valid() bool {
	switch m {
	case colocationSameNode, colocationSameOrbit:
		return true
	}
	return false
}

// --- Group Types ---

// MemberSpec is a capsule spec intended to live inside a group.
//
// At admission, each MemberSpec is materialized to a full CapsuleSpec with
// Kind = Capsule, GroupID = <group ID>, GroupMember = true. Phase 10
// deliberately omits the `Discovers` and `ReplicaLabels` fields — those will
// be added in Phase 11 alongside their consumers. Inputs setting them are
// rejected at admission with a clear "feature not yet supported" error so
// users do not silently rely on no-op fields.
type MemberSpec struct {
	// Name is the member's name within the group (DNS-friendly).
	Name string

	// Spec is the full capsule spec for this member. The Kind, GroupID, and
	// GroupMember fields are set by the admission path; values supplied by
	// the user are ignored.
	Spec CapsuleSpec

	// DependsOn lists names of other members in the same group that must
	// reach Running before this member is started. Cycles are rejected at
	// admission.
	DependsOn []string
}

// GroupSpec lives on a CapsuleSpec when Kind == Group. It carries the
// member list, colocation mode, and cascade-delete behaviour. After
// admission, MemberIDs is populated with the materialized member capsule
// IDs in the same order as Members.
type GroupSpec struct {
	// Colocation controls whether members must share a node (SameNode) or
	// may be placed independently within the orbit (SameOrbit).
	Colocation ColocationMode

	// Members is the canonical list of member specs used for materialization
	// and (when CascadeDelete is true) cascade teardown.
	Members []MemberSpec

	// MemberIDs holds the IDs of materialized member capsules. Empty before
	// admission; populated by Manager.CreateGroup in the same order as
	// Members.
	MemberIDs []CapsuleID

	// CascadeDelete controls what happens to members when the group is
	// deleted. True (the default applied by DefaultSpec) deletes every
	// member. False clears GroupID/GroupMember on each member, leaving them
	// running as standalone capsules.
	CascadeDelete bool
}
