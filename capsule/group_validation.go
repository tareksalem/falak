package capsule

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Group spec validation errors.
//
// These sentinels mirror the style of the validation errors in spec.go so
// callers can use errors.Is to test for specific failures within an
// errors.Join'd result.
var (
	// ErrGroupColocationRequired is returned when a GroupSpec has no Colocation set.
	ErrGroupColocationRequired = errors.New("group colocation is required")

	// ErrGroupColocationInvalid is returned when a GroupSpec has an unknown
	// Colocation value (anything other than same-node or same-orbit).
	ErrGroupColocationInvalid = errors.New("group colocation must be same-node or same-orbit")

	// ErrGroupMembersRequired is returned when a GroupSpec has no members.
	ErrGroupMembersRequired = errors.New("group must have at least one member")

	// ErrGroupMemberNameRequired is returned when a member has no Name.
	ErrGroupMemberNameRequired = errors.New("group member name is required")

	// ErrGroupMemberNameDuplicate is returned when two members within a
	// group share the same Name.
	ErrGroupMemberNameDuplicate = errors.New("group member name is duplicated")

	// ErrGroupMemberNameNotDNS is returned when a member Name is not a
	// DNS-friendly label (lowercase letters, digits, hyphens; 1-63 chars;
	// must not start or end with a hyphen).
	ErrGroupMemberNameNotDNS = errors.New("group member name must be a DNS-friendly label (lowercase letters, digits, hyphens; 1-63 chars; cannot start or end with hyphen)")

	// ErrGroupDependsOnUnknown is returned when a member's DependsOn entry
	// references a name that is not present in the group.
	ErrGroupDependsOnUnknown = errors.New("group member depends_on references an unknown member")

	// ErrGroupDependsOnSelf is returned when a member declares itself as a
	// dependency.
	ErrGroupDependsOnSelf = errors.New("group member depends_on cannot reference itself")

	// ErrGroupDependsOnCycle is returned when the depends_on graph contains
	// a cycle.
	ErrGroupDependsOnCycle = errors.New("group depends_on graph contains a cycle")

	// ErrGroupMemberSpecInvalid is returned when a member's embedded
	// CapsuleSpec fails ValidateSpec. The inner errors are wrapped via %w so
	// callers can unwrap to inspect them.
	ErrGroupMemberSpecInvalid = errors.New("group member spec is invalid")

	// ErrPhase11FieldNotSupported is returned when a Phase 11+ field is set
	// on a Phase 10 spec. Phase 10 deliberately rejects Phase 11 fields
	// (replica_labels, discovers, ...) at admission instead of silently
	// dropping them; this preserves the contract that any field accepted by
	// the validator has working semantics behind it.
	ErrPhase11FieldNotSupported = errors.New("feature not yet supported (Phase 11)")

	// ErrGroupKindRequiresGroup is returned when CapsuleSpec.Kind == Group
	// but CapsuleSpec.Group is nil.
	ErrGroupKindRequiresGroup = errors.New("kind=group requires a non-nil Group sub-spec")

	// ErrCapsuleKindForbidsGroup is returned when CapsuleSpec.Kind != Group
	// but CapsuleSpec.Group is non-nil.
	ErrCapsuleKindForbidsGroup = errors.New("non-group capsule must not carry a Group sub-spec")

	// ErrGroupKindForbidsWorkloadFields is returned when a group-kind
	// capsule sets workload-only fields (Image, Resources, Replicas,
	// ScalingRules, PlacementRules, Runtime, MomentumConfig). Group capsules
	// have no workload of their own.
	ErrGroupKindForbidsWorkloadFields = errors.New("kind=group must not set workload fields (image, resources, replicas, scaling rules, placement rules, runtime, momentum)")

	// ErrGroupMemberPairing is returned when GroupMember and GroupID are
	// not paired correctly: GroupMember=true requires a non-empty GroupID,
	// and a non-empty GroupID requires GroupMember=true.
	ErrGroupMemberPairing = errors.New("GroupMember and GroupID must be paired (both set or both empty)")
)

// ValidateGroupSpec validates a GroupSpec and returns all violations joined
// via errors.Join. Each violation is one of the sentinel errors declared in
// this file (possibly wrapped with %w to add context such as the offending
// member name or cycle path), so callers can use errors.Is to check for a
// specific failure.
//
// The validator does not mutate the input. Callers expecting defaults
// applied should run DefaultSpec / DefaultGroupSpec first.
//
// Validation covers:
//   - Colocation must be a known ColocationMode.
//   - The group must have at least one member.
//   - Each member name must be present, DNS-friendly, and unique within the
//     group.
//   - Each member's embedded CapsuleSpec must pass ValidateSpec (errors are
//     wrapped via ErrGroupMemberSpecInvalid with the member index).
//   - Each DependsOn entry must reference a known member name and must not
//     be a self-reference.
//   - The depends_on graph must be a DAG (no cycles). A single cycle path is
//     reported via ErrGroupDependsOnCycle.
//   - Phase 11+ fields must not be set; rejectPhase11Fields enforces this
//     seam (no-op today on the Go struct, future-proof for CUE/proto).
func ValidateGroupSpec(g *GroupSpec) error {
	if g == nil {
		return ErrGroupMembersRequired
	}

	var errs []error

	// Colocation.
	switch {
	case g.Colocation == "":
		errs = append(errs, ErrGroupColocationRequired)
	case !g.Colocation.Valid():
		errs = append(errs, fmt.Errorf("%w: %q", ErrGroupColocationInvalid, g.Colocation))
	}

	// Members.
	if len(g.Members) == 0 {
		errs = append(errs, ErrGroupMembersRequired)
	}

	// Build name set up front so depends_on validation has it. Names are
	// validated for shape and duplicates here too.
	names := make(map[string]struct{}, len(g.Members))
	seenForDup := make(map[string]struct{}, len(g.Members))
	for i, m := range g.Members {
		if m.Name == "" {
			errs = append(errs, fmt.Errorf("%w: member index %d", ErrGroupMemberNameRequired, i))
			continue
		}
		if !isDNSLabel(m.Name) {
			errs = append(errs, fmt.Errorf("%w: %q", ErrGroupMemberNameNotDNS, m.Name))
			// Continue so we still surface duplicate names. Don't add to
			// `names` set; depends_on validation will then flag references.
			continue
		}
		if _, dup := seenForDup[m.Name]; dup {
			errs = append(errs, fmt.Errorf("%w: %q", ErrGroupMemberNameDuplicate, m.Name))
			// Don't re-record; the first occurrence is in `names` already.
			continue
		}
		seenForDup[m.Name] = struct{}{}
		names[m.Name] = struct{}{}
	}

	// Member spec validation.
	for i, m := range g.Members {
		spec := m.Spec
		if err := ValidateSpec(&spec); err != nil {
			errs = append(errs, fmt.Errorf("%w: member %d (%q): %w", ErrGroupMemberSpecInvalid, i, m.Name, err))
		}
	}

	// DependsOn references and self-reference.
	for _, m := range g.Members {
		if m.Name == "" {
			continue
		}
		for _, dep := range m.DependsOn {
			if dep == m.Name {
				errs = append(errs, fmt.Errorf("%w: %q", ErrGroupDependsOnSelf, m.Name))
				continue
			}
			if _, ok := names[dep]; !ok {
				errs = append(errs, fmt.Errorf("%w: %q -> %q", ErrGroupDependsOnUnknown, m.Name, dep))
			}
		}
	}

	// Cycle detection. Build the graph from valid (known-name) members
	// only; unknown / self-edges have already been reported above and are
	// excluded so we report the cycle error at most once and avoid spurious
	// cycles caused by edges that won't exist in a corrected spec.
	deps := make(map[string][]string, len(g.Members))
	for _, m := range g.Members {
		if m.Name == "" {
			continue
		}
		if _, ok := names[m.Name]; !ok {
			continue
		}
		filtered := make([]string, 0, len(m.DependsOn))
		for _, dep := range m.DependsOn {
			if dep == m.Name {
				continue
			}
			if _, ok := names[dep]; !ok {
				continue
			}
			filtered = append(filtered, dep)
		}
		deps[m.Name] = filtered
	}
	if cycle := detectCycle(deps); cycle != nil {
		errs = append(errs, fmt.Errorf("%w: %s", ErrGroupDependsOnCycle, strings.Join(cycle, " -> ")))
	}

	// Phase 11 dead-field rejection seam — no-op on the Go struct today,
	// but invoked here so future CUE/proto-level rejections plug in.
	if err := rejectPhase11FieldsGroup(g); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// ValidateCapsuleSpecGroupFields enforces the structural invariants on the
// group-related fields of a CapsuleSpec without revalidating the entire
// workload spec. Call ValidateSpec separately for full workload validation.
//
// Invariants:
//   - Kind == Group requires a non-nil Group and forbids workload fields
//     (Image, non-zero Resources/Replicas/Runtime/MomentumConfig,
//     ScalingRules, PlacementRules).
//   - Kind != Group (including the empty default) forbids a non-nil Group.
//   - GroupMember and GroupID must be paired: both set or both empty.
//   - Phase 11+ fields must not be set.
//
// All violations are joined via errors.Join.
func ValidateCapsuleSpecGroupFields(s *CapsuleSpec) error {
	if s == nil {
		return nil
	}

	var errs []error

	isGroup := s.Kind == CapsuleKindEnum.Group()

	switch {
	case isGroup && s.Group == nil:
		errs = append(errs, ErrGroupKindRequiresGroup)
	case !isGroup && s.Group != nil:
		errs = append(errs, ErrCapsuleKindForbidsGroup)
	}

	if isGroup && hasWorkloadFields(s) {
		errs = append(errs, ErrGroupKindForbidsWorkloadFields)
	}

	// Pairing of GroupMember <-> GroupID.
	if s.GroupMember && s.GroupID == "" {
		errs = append(errs, fmt.Errorf("%w: GroupMember=true with empty GroupID", ErrGroupMemberPairing))
	}
	if s.GroupID != "" && !s.GroupMember {
		errs = append(errs, fmt.Errorf("%w: GroupID=%q with GroupMember=false", ErrGroupMemberPairing, s.GroupID))
	}

	if err := rejectPhase11Fields(s); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// hasWorkloadFields returns true if any workload-only field on the spec is
// non-zero. Group capsules carry no workload of their own; this guard keeps
// admission from accepting a group spec that quietly mixes workload fields.
func hasWorkloadFields(s *CapsuleSpec) bool {
	if s.Image != "" {
		return true
	}
	if s.Resources != (ResourceRequirements{}) {
		return true
	}
	if s.Replicas != (ReplicaConfig{}) {
		return true
	}
	if len(s.ScalingRules) > 0 {
		return true
	}
	if len(s.PlacementRules) > 0 {
		return true
	}
	if !runtimeIsZero(s.Runtime) {
		return true
	}
	if s.MomentumConfig != (MomentumConfig{}) {
		return true
	}
	return false
}

// runtimeIsZero reports whether a RuntimeConfig is the zero value.
// RuntimeConfig contains a pointer (HealthCheck) and maps/slices, so direct
// struct equality won't compile. Each field is compared explicitly.
func runtimeIsZero(r RuntimeConfig) bool {
	if len(r.Env) != 0 {
		return false
	}
	if r.Network.Mode != "" || len(r.Network.Ports) != 0 {
		return false
	}
	if r.HealthCheck != nil {
		return false
	}
	if r.FailurePolicy != (FailurePolicy{}) {
		return false
	}
	if r.LogRetention != (LogRetention{}) {
		return false
	}
	if r.StatsInterval != 0 {
		return false
	}
	if r.Registry != nil {
		return false
	}
	if r.SnapshotConfig != (SnapshotConfig{}) {
		return false
	}
	return true
}

// rejectPhase11Fields is the seam for rejecting Phase 11+ fields on a
// CapsuleSpec. Today the Go struct does not declare any Phase 11 fields, so
// this helper is a deliberate no-op. It exists so that:
//
//  1. The CUE and proto layers (tasks 10.10 and 10.18) plug their rejection
//     here rather than scattering "phase 11 not supported" branches across
//     the codebase.
//  2. Both validators (ValidateGroupSpec via member specs and
//     ValidateCapsuleSpecGroupFields directly) call this single helper, so
//     adding a new dead field means editing exactly one place.
//
// When Phase 11 fields land on the Go struct, they will be checked here and
// any non-zero value will return a wrapped ErrPhase11FieldNotSupported.
func rejectPhase11Fields(s *CapsuleSpec) error {
	if s == nil {
		return nil
	}
	// No Phase 11 fields exist on CapsuleSpec yet. Future fields land here.
	return nil
}

// rejectPhase11FieldsGroup is the group-spec equivalent of
// rejectPhase11Fields. It iterates members and applies the same seam to
// each member's CapsuleSpec. Same rationale: one place to extend when
// Phase 11 fields are introduced.
func rejectPhase11FieldsGroup(g *GroupSpec) error {
	if g == nil {
		return nil
	}
	var errs []error
	for i, m := range g.Members {
		spec := m.Spec
		if err := rejectPhase11Fields(&spec); err != nil {
			errs = append(errs, fmt.Errorf("member %d (%q): %w", i, m.Name, err))
		}
	}
	return errors.Join(errs...)
}

// detectCycle runs a DFS-based cycle detection on the depends_on graph and
// returns the cycle path (in traversal order, starting and ending at the
// cycle's entry node) or nil if the graph is a DAG. Iteration is over
// sorted node names so the reported path is deterministic across runs.
func detectCycle(deps map[string][]string) []string {
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS stack
		black = 2 // fully explored
	)

	color := make(map[string]int, len(deps))
	stack := make([]string, 0, len(deps))

	// Local DFS that returns a cycle path (slice of names) or nil.
	var dfs func(node string) []string
	dfs = func(node string) []string {
		color[node] = gray
		stack = append(stack, node)

		// Iterate dependencies in sorted order for determinism.
		neighbors := append([]string(nil), deps[node]...)
		sort.Strings(neighbors)

		for _, next := range neighbors {
			switch color[next] {
			case white:
				if cyc := dfs(next); cyc != nil {
					return cyc
				}
			case gray:
				// Found a back-edge to `next`. The cycle is the suffix of
				// the stack starting at `next`, plus `next` to close the
				// loop visually.
				start := -1
				for i, n := range stack {
					if n == next {
						start = i
						break
					}
				}
				if start < 0 {
					// Defensive: the gray node must be on the stack.
					return append([]string{next}, next)
				}
				cyc := append([]string(nil), stack[start:]...)
				cyc = append(cyc, next)
				return cyc
			}
		}

		// Pop and finalize.
		color[node] = black
		stack = stack[:len(stack)-1]
		return nil
	}

	// Seed nodes in sorted order so the first-discovered cycle is stable.
	keys := make([]string, 0, len(deps))
	for k := range deps {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if color[k] == white {
			if cyc := dfs(k); cyc != nil {
				return cyc
			}
		}
	}
	return nil
}

// isDNSLabel reports whether s is a valid DNS label per RFC 1123 (lowercase
// letters, digits, hyphens; 1-63 chars; cannot start or end with a hyphen).
// Used to validate group member names so they can be embedded in DNS
// records by Phase 11 service discovery without further escaping.
func isDNSLabel(s string) bool {
	n := len(s)
	if n == 0 || n > 63 {
		return false
	}
	if s[0] == '-' || s[n-1] == '-' {
		return false
	}
	for i := 0; i < n; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}
