package capsule

import (
	"errors"
	"strings"
	"testing"
)

// validMemberSpec returns a CapsuleSpec that passes ValidateSpec, suitable
// for embedding inside a MemberSpec without polluting unrelated assertions.
func validMemberSpec(name string) CapsuleSpec {
	spec := CapsuleSpec{
		Name:  name,
		Image: name + ":latest",
		Orbit: "api",
	}
	DefaultSpec(&spec)
	return spec
}

// validGroupSpec returns a minimal but valid GroupSpec with two members
// (a, b) that can be tweaked per-test.
func validGroupSpec() *GroupSpec {
	return &GroupSpec{
		Colocation: ColocationModeEnum.SameNode(),
		Members: []MemberSpec{
			{Name: "a", Spec: validMemberSpec("a")},
			{Name: "b", Spec: validMemberSpec("b")},
		},
	}
}

// --- 10.T1 — sentinel errors fire on the corresponding bad input ---

func TestValidateGroupSpec_Sentinels(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(g *GroupSpec)
		wantErr error
	}{
		{
			name:    "missing colocation",
			mutate:  func(g *GroupSpec) { g.Colocation = "" },
			wantErr: ErrGroupColocationRequired,
		},
		{
			name:    "invalid colocation",
			mutate:  func(g *GroupSpec) { g.Colocation = "moon" },
			wantErr: ErrGroupColocationInvalid,
		},
		{
			name:    "no members",
			mutate:  func(g *GroupSpec) { g.Members = nil },
			wantErr: ErrGroupMembersRequired,
		},
		{
			name: "empty member name",
			mutate: func(g *GroupSpec) {
				g.Members[0].Name = ""
			},
			wantErr: ErrGroupMemberNameRequired,
		},
		{
			name: "duplicate member name",
			mutate: func(g *GroupSpec) {
				g.Members = append(g.Members, MemberSpec{Name: "a", Spec: validMemberSpec("a")})
			},
			wantErr: ErrGroupMemberNameDuplicate,
		},
		{
			name: "non-DNS member name (uppercase)",
			mutate: func(g *GroupSpec) {
				g.Members[0].Name = "API"
			},
			wantErr: ErrGroupMemberNameNotDNS,
		},
		{
			name: "depends_on unknown",
			mutate: func(g *GroupSpec) {
				g.Members[0].DependsOn = []string{"ghost"}
			},
			wantErr: ErrGroupDependsOnUnknown,
		},
		{
			name: "depends_on self",
			mutate: func(g *GroupSpec) {
				g.Members[0].DependsOn = []string{"a"}
			},
			wantErr: ErrGroupDependsOnSelf,
		},
		{
			name: "member spec invalid (missing image)",
			mutate: func(g *GroupSpec) {
				bad := validMemberSpec("a")
				bad.Image = ""
				g.Members[0].Spec = bad
			},
			wantErr: ErrGroupMemberSpecInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := validGroupSpec()
			tt.mutate(g)
			err := ValidateGroupSpec(g)
			if err == nil {
				t.Fatalf("expected error %v, got nil", tt.wantErr)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected errors.Is(err, %v) = true; err = %v", tt.wantErr, err)
			}
		})
	}
}

// All sentinels fire together when fed maximally bad input. errors.Is must
// resolve each individually because errors.Join preserves all branches.
func TestValidateGroupSpec_MultipleViolations(t *testing.T) {
	g := &GroupSpec{
		// no colocation, no members → triggers two errors.
	}
	err := ValidateGroupSpec(g)
	if err == nil {
		t.Fatal("expected errors, got nil")
	}
	for _, want := range []error{ErrGroupColocationRequired, ErrGroupMembersRequired} {
		if !errors.Is(err, want) {
			t.Errorf("expected errors.Is(err, %v) = true; err = %v", want, err)
		}
	}
}

// --- 10.T2 — DAG cycle detection ---

func TestValidateGroupSpec_CycleDetection(t *testing.T) {
	tests := []struct {
		name string
		deps map[string][]string
	}{
		{
			name: "self loop A->A (also exercises ErrGroupDependsOnSelf)",
			deps: map[string][]string{"a": {"a"}, "b": nil},
		},
		{
			name: "two-cycle A->B->A",
			deps: map[string][]string{"a": {"b"}, "b": {"a"}},
		},
		{
			name: "three-cycle A->B->C->A",
			deps: map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"a"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := &GroupSpec{
				Colocation: ColocationModeEnum.SameOrbit(),
				Members:    membersFromDeps(tt.deps),
			}
			err := ValidateGroupSpec(g)
			if err == nil {
				t.Fatalf("expected cycle error, got nil")
			}
			// Self-loop case reports ErrGroupDependsOnSelf and skips cycle
			// graph construction; the other cases must report
			// ErrGroupDependsOnCycle.
			if tt.name == "self loop A->A (also exercises ErrGroupDependsOnSelf)" {
				if !errors.Is(err, ErrGroupDependsOnSelf) {
					t.Fatalf("expected ErrGroupDependsOnSelf; got %v", err)
				}
				return
			}
			if !errors.Is(err, ErrGroupDependsOnCycle) {
				t.Fatalf("expected ErrGroupDependsOnCycle; got %v", err)
			}
		})
	}
}

// --- 10.T3 — DAG with no cycles passes; cycle detection is deterministic ---

func TestValidateGroupSpec_DAGAccepted(t *testing.T) {
	tests := []struct {
		name string
		deps map[string][]string
	}{
		{
			name: "linear A->B, B->C",
			deps: map[string][]string{"a": {"b"}, "b": {"c"}, "c": nil},
		},
		{
			name: "diamond A->B, A->C, B->D, C->D",
			deps: map[string][]string{
				"a": {"b", "c"},
				"b": {"d"},
				"c": {"d"},
				"d": nil,
			},
		},
		{
			name: "no edges at all",
			deps: map[string][]string{"a": nil, "b": nil, "c": nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := &GroupSpec{
				Colocation: ColocationModeEnum.SameNode(),
				Members:    membersFromDeps(tt.deps),
			}
			if err := ValidateGroupSpec(g); err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestDetectCycle_Deterministic(t *testing.T) {
	// Same cyclic graph, run many times. The cycle path must be identical
	// across runs because detectCycle iterates sorted keys and sorted
	// neighbors.
	deps := map[string][]string{
		"a": {"b", "c"},
		"b": {"d"},
		"c": {"d"},
		"d": {"a"}, // creates cycle back to a
	}
	first := detectCycle(deps)
	if first == nil {
		t.Fatal("expected a cycle, got nil")
	}
	for i := 0; i < 100; i++ {
		got := detectCycle(deps)
		if !equalStrings(got, first) {
			t.Fatalf("non-deterministic cycle path: first=%v iter=%d=%v", first, i, got)
		}
	}
}

// --- 10.T23 — Phase 11 dead-field rejection seam ---

func TestRejectPhase11Fields_NoOpToday(t *testing.T) {
	// The Go struct does not declare any Phase 11 fields yet, so the seam
	// must return nil. CUE/proto-level rejection is task 10.10 / 10.18.
	if err := rejectPhase11Fields(nil); err != nil {
		t.Errorf("rejectPhase11Fields(nil) = %v, want nil", err)
	}
	var spec CapsuleSpec
	if err := rejectPhase11Fields(&spec); err != nil {
		t.Errorf("rejectPhase11Fields(zero) = %v, want nil", err)
	}
}

func TestRejectPhase11FieldsGroup_NoOpToday(t *testing.T) {
	if err := rejectPhase11FieldsGroup(nil); err != nil {
		t.Errorf("rejectPhase11FieldsGroup(nil) = %v, want nil", err)
	}
	g := validGroupSpec()
	if err := rejectPhase11FieldsGroup(g); err != nil {
		t.Errorf("rejectPhase11FieldsGroup(valid) = %v, want nil", err)
	}
}

// --- DNS label validation ---

func TestIsDNSLabel(t *testing.T) {
	tests := []struct {
		name string
		s    string
		ok   bool
	}{
		{"simple lowercase", "api", true},
		{"with digit", "web-1", true},
		{"two letters", "db", true},
		{"single char", "a", true},
		{"63 chars", strings.Repeat("a", 63), true},
		{"empty", "", false},
		{"uppercase", "API", false},
		{"starts with digit ok", "1web", true}, // RFC 1123: digits OK at start
		{"starts with hyphen", "-name", false},
		{"ends with hyphen", "name-", false},
		{"contains dot", "ab.cd", false},
		{"contains underscore", "ab_cd", false},
		{"64 chars", strings.Repeat("a", 64), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isDNSLabel(tt.s); got != tt.ok {
				t.Errorf("isDNSLabel(%q) = %v, want %v", tt.s, got, tt.ok)
			}
		})
	}
}

func TestValidateGroupSpec_DNSNameRules(t *testing.T) {
	// Exercise the validator end-to-end on each of the bad-name cases that
	// isDNSLabel rejects, to confirm the error surfaces as ErrGroupMemberNameNotDNS.
	bad := []string{"API", "-name", "name-", "ab.cd", "ab_cd", strings.Repeat("a", 64)}
	for _, name := range bad {
		t.Run("bad/"+name, func(t *testing.T) {
			t.Parallel()
			g := validGroupSpec()
			g.Members[0].Name = name
			err := ValidateGroupSpec(g)
			if err == nil {
				t.Fatalf("expected error for member name %q, got nil", name)
			}
			if !errors.Is(err, ErrGroupMemberNameNotDNS) {
				t.Fatalf("expected ErrGroupMemberNameNotDNS for %q; got %v", name, err)
			}
		})
	}

	good := []string{"api", "web-1", "db", "1web", "a"}
	for _, name := range good {
		t.Run("good/"+name, func(t *testing.T) {
			t.Parallel()
			g := &GroupSpec{
				Colocation: ColocationModeEnum.SameNode(),
				Members: []MemberSpec{
					{Name: name, Spec: validMemberSpec(name)},
				},
			}
			if err := ValidateGroupSpec(g); err != nil {
				t.Fatalf("expected no error for member name %q, got %v", name, err)
			}
		})
	}
}

// --- ValidateCapsuleSpecGroupFields ---

func TestValidateCapsuleSpecGroupFields(t *testing.T) {
	tests := []struct {
		name    string
		spec    CapsuleSpec
		wantErr error // nil = expect success
	}{
		{
			name: "all zero",
			spec: CapsuleSpec{},
		},
		{
			name: "kind=group with image",
			spec: CapsuleSpec{
				Kind:  CapsuleKindEnum.Group(),
				Group: &GroupSpec{},
				Image: "nginx",
			},
			wantErr: ErrGroupKindForbidsWorkloadFields,
		},
		{
			name: "kind=group with non-zero replicas",
			spec: CapsuleSpec{
				Kind:     CapsuleKindEnum.Group(),
				Group:    &GroupSpec{},
				Replicas: ReplicaConfig{Min: 1, Max: 1},
			},
			wantErr: ErrGroupKindForbidsWorkloadFields,
		},
		{
			name: "kind=group with placement rules",
			spec: CapsuleSpec{
				Kind:           CapsuleKindEnum.Group(),
				Group:          &GroupSpec{},
				PlacementRules: []PlacementRule{{Name: "r"}},
			},
			wantErr: ErrGroupKindForbidsWorkloadFields,
		},
		{
			name: "kind=group with momentum",
			spec: CapsuleSpec{
				Kind:           CapsuleKindEnum.Group(),
				Group:          &GroupSpec{},
				MomentumConfig: MomentumConfig{Base: 50},
			},
			wantErr: ErrGroupKindForbidsWorkloadFields,
		},
		{
			name: "kind=group with runtime env",
			spec: CapsuleSpec{
				Kind:    CapsuleKindEnum.Group(),
				Group:   &GroupSpec{},
				Runtime: RuntimeConfig{Env: map[string]string{"K": "V"}},
			},
			wantErr: ErrGroupKindForbidsWorkloadFields,
		},
		{
			name: "kind=group group nil",
			spec: CapsuleSpec{
				Kind: CapsuleKindEnum.Group(),
			},
			wantErr: ErrGroupKindRequiresGroup,
		},
		{
			name: "kind=capsule group non-nil",
			spec: CapsuleSpec{
				Kind:  CapsuleKindEnum.Capsule(),
				Group: &GroupSpec{},
			},
			wantErr: ErrCapsuleKindForbidsGroup,
		},
		{
			name: "empty kind with group non-nil",
			spec: CapsuleSpec{
				Group: &GroupSpec{},
			},
			wantErr: ErrCapsuleKindForbidsGroup,
		},
		{
			name: "GroupMember=true GroupID empty",
			spec: CapsuleSpec{
				GroupMember: true,
			},
			wantErr: ErrGroupMemberPairing,
		},
		{
			name: "GroupID set GroupMember=false",
			spec: CapsuleSpec{
				GroupID: "abc123",
			},
			wantErr: ErrGroupMemberPairing,
		},
		{
			name: "GroupID set GroupMember=true",
			spec: CapsuleSpec{
				GroupID:     "abc123",
				GroupMember: true,
			},
		},
		{
			name: "kind=group group set, no workload, no pairing",
			spec: CapsuleSpec{
				Kind:  CapsuleKindEnum.Group(),
				Group: &GroupSpec{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := tt.spec
			err := ValidateCapsuleSpecGroupFields(&spec)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %v, got nil", tt.wantErr)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected errors.Is(err, %v) = true; err = %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateCapsuleSpecGroupFields_Nil(t *testing.T) {
	if err := ValidateCapsuleSpecGroupFields(nil); err != nil {
		t.Errorf("ValidateCapsuleSpecGroupFields(nil) = %v, want nil", err)
	}
}

// --- helpers ---

// membersFromDeps builds a deterministic slice of MemberSpec from a
// dependency map. Each member gets a valid embedded CapsuleSpec.
func membersFromDeps(deps map[string][]string) []MemberSpec {
	names := make([]string, 0, len(deps))
	for k := range deps {
		names = append(names, k)
	}
	// Sorted iteration so the slice order is deterministic for table-driven
	// assertions.
	sortStrings(names)

	members := make([]MemberSpec, 0, len(names))
	for _, n := range names {
		m := MemberSpec{
			Name: n,
			Spec: validMemberSpec(n),
		}
		if len(deps[n]) > 0 {
			m.DependsOn = append([]string(nil), deps[n]...)
		}
		members = append(members, m)
	}
	return members
}

// equalStrings is a tiny helper to avoid pulling reflect into every test.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sortStrings is a thin wrapper so the test file does not introduce a
// "sort" import alongside the validator's own.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
