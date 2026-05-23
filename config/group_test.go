package config

import (
	"errors"
	"testing"

	"github.com/tareksalem/falak/capsule"
)

// boolPtr is a tiny helper for the *bool CascadeDelete field.
func boolPtr(b bool) *bool { return &b }

// validMemberConfig returns a minimal valid member spec for use in tests.
func validMemberConfig(name, image, orbit string) CapsuleMemberConfig {
	return CapsuleMemberConfig{
		Name:  name,
		Image: image,
		Orbit: orbit,
	}
}

// TestToGroupSpec_HappyPath converts a complete kind=group config and
// asserts every field landed in the resulting GroupSpec.
func TestToGroupSpec_HappyPath(t *testing.T) {
	t.Parallel()

	cfg := &CapsuleConfig{
		Name:   "my-stack",
		Kind:   "group",
		Labels: map[string]string{"app": "my-stack", "team": "backend"},
		Group: &GroupConfig{
			Colocation:    "same-orbit",
			CascadeDelete: boolPtr(true),
			Members: map[string]CapsuleMemberConfig{
				"db":  validMemberConfig("db", "postgres:15", "data"),
				"api": validMemberConfig("api", "registry/api:v1", "public"),
				"web": validMemberConfig("web", "registry/web:v1", "public"),
			},
		},
	}
	cfg.Group.Members["api"] = CapsuleMemberConfig{
		Name:      "api",
		Image:     "registry/api:v1",
		Orbit:     "public",
		DependsOn: []string{"db"},
	}
	cfg.Group.Members["web"] = CapsuleMemberConfig{
		Name:      "web",
		Image:     "registry/web:v1",
		Orbit:     "public",
		DependsOn: []string{"api"},
	}

	name, spec, base, err := cfg.ToGroupSpec()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "my-stack" {
		t.Errorf("group name: got %q, want my-stack", name)
	}
	if spec.Colocation != capsule.ColocationModeEnum.SameOrbit() {
		t.Errorf("colocation: got %q, want same-orbit", spec.Colocation)
	}
	if !spec.CascadeDelete {
		t.Errorf("cascade_delete: got false, want true")
	}
	if len(spec.Members) != 3 {
		t.Fatalf("member count: got %d, want 3", len(spec.Members))
	}
	if base["app"] != "my-stack" || base["team"] != "backend" {
		t.Errorf("base labels not propagated: %v", base)
	}
}

// TestToGroupSpec_DefaultColocation_Empty defaults empty colocation
// to "same-orbit".
func TestToGroupSpec_DefaultColocation_Empty(t *testing.T) {
	t.Parallel()

	cfg := &CapsuleConfig{
		Name: "g",
		Kind: "group",
		Group: &GroupConfig{
			Colocation: "",
			Members: map[string]CapsuleMemberConfig{
				"db": validMemberConfig("db", "postgres:15", "data"),
			},
		},
	}
	_, spec, _, err := cfg.ToGroupSpec()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.Colocation != capsule.ColocationModeEnum.SameOrbit() {
		t.Errorf("default colocation: got %q, want same-orbit", spec.Colocation)
	}
}

// TestToGroupSpec_DefaultCascadeDelete defaults missing CascadeDelete to true.
func TestToGroupSpec_DefaultCascadeDelete(t *testing.T) {
	t.Parallel()

	cfg := &CapsuleConfig{
		Name: "g",
		Kind: "group",
		Group: &GroupConfig{
			// CascadeDelete not set — defaults to true.
			Members: map[string]CapsuleMemberConfig{
				"db": validMemberConfig("db", "postgres:15", "data"),
			},
		},
	}
	_, spec, _, err := cfg.ToGroupSpec()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !spec.CascadeDelete {
		t.Error("default cascade_delete: got false, want true")
	}
}

// TestToGroupSpec_ExplicitCascadeDeleteFalse honours an explicit false.
func TestToGroupSpec_ExplicitCascadeDeleteFalse(t *testing.T) {
	t.Parallel()

	cfg := &CapsuleConfig{
		Name: "g",
		Kind: "group",
		Group: &GroupConfig{
			CascadeDelete: boolPtr(false),
			Members: map[string]CapsuleMemberConfig{
				"db": validMemberConfig("db", "postgres:15", "data"),
			},
		},
	}
	_, spec, _, err := cfg.ToGroupSpec()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.CascadeDelete {
		t.Error("explicit cascade_delete=false not honoured")
	}
}

// TestToGroupSpec_RejectReplicaLabels rejects Phase 11 dead field.
func TestToGroupSpec_RejectReplicaLabels(t *testing.T) {
	t.Parallel()

	cfg := &CapsuleConfig{
		Name: "g",
		Kind: "group",
		Group: &GroupConfig{
			Members: map[string]CapsuleMemberConfig{
				"db": {
					Name:  "db",
					Image: "postgres:15",
					Orbit: "data",
					ReplicaLabels: []map[string]string{
						{"role": "primary"},
						{"role": "secondary"},
					},
				},
			},
		},
	}
	_, _, _, err := cfg.ToGroupSpec()
	if err == nil {
		t.Fatal("expected error for replica_labels but got nil")
	}
	if !errors.Is(err, ErrConfigPhase11FieldReplicaLabels) {
		t.Errorf("expected ErrConfigPhase11FieldReplicaLabels, got: %v", err)
	}
}

// TestToGroupSpec_RejectDiscovers rejects Phase 11 dead field.
func TestToGroupSpec_RejectDiscovers(t *testing.T) {
	t.Parallel()

	cfg := &CapsuleConfig{
		Name: "g",
		Kind: "group",
		Group: &GroupConfig{
			Members: map[string]CapsuleMemberConfig{
				"api": {
					Name:      "api",
					Image:     "registry/api:v1",
					Orbit:     "public",
					Discovers: []string{"billing.other-group"},
				},
			},
		},
	}
	_, _, _, err := cfg.ToGroupSpec()
	if err == nil {
		t.Fatal("expected error for discovers but got nil")
	}
	if !errors.Is(err, ErrConfigPhase11FieldDiscovers) {
		t.Errorf("expected ErrConfigPhase11FieldDiscovers, got: %v", err)
	}
}

// TestToGroupSpec_RejectBothPhase11Fields_JoinsErrors confirms multiple
// Phase 11 violations are joined into a single error.
func TestToGroupSpec_RejectBothPhase11Fields_JoinsErrors(t *testing.T) {
	t.Parallel()

	cfg := &CapsuleConfig{
		Name: "g",
		Kind: "group",
		Group: &GroupConfig{
			Members: map[string]CapsuleMemberConfig{
				"api": {
					Name:      "api",
					Image:     "registry/api:v1",
					Orbit:     "public",
					Discovers: []string{"x.y"},
					ReplicaLabels: []map[string]string{
						{"shard": "0"},
					},
				},
			},
		},
	}
	_, _, _, err := cfg.ToGroupSpec()
	if err == nil {
		t.Fatal("expected error but got nil")
	}
	if !errors.Is(err, ErrConfigPhase11FieldReplicaLabels) {
		t.Error("expected ErrConfigPhase11FieldReplicaLabels in join")
	}
	if !errors.Is(err, ErrConfigPhase11FieldDiscovers) {
		t.Error("expected ErrConfigPhase11FieldDiscovers in join")
	}
}

// TestToGroupSpec_NotKindGroup rejects when called on a kind=capsule config.
func TestToGroupSpec_NotKindGroup(t *testing.T) {
	t.Parallel()

	cfg := &CapsuleConfig{
		Name:  "standalone",
		Kind:  "", // defaults to capsule
		Image: "x",
		Orbit: "y",
	}
	_, _, _, err := cfg.ToGroupSpec()
	if err == nil {
		t.Fatal("expected error for kind=capsule, got nil")
	}
}

// TestToGroupSpec_GroupNil rejects when the group block is nil.
func TestToGroupSpec_GroupNil(t *testing.T) {
	t.Parallel()

	cfg := &CapsuleConfig{
		Name: "g",
		Kind: "group",
		// Group: nil
	}
	_, _, _, err := cfg.ToGroupSpec()
	if err == nil {
		t.Fatal("expected error for nil Group, got nil")
	}
}

// TestToGroupSpec_InvalidColocation rejects unknown colocation strings.
func TestToGroupSpec_InvalidColocation(t *testing.T) {
	t.Parallel()

	cfg := &CapsuleConfig{
		Name: "g",
		Kind: "group",
		Group: &GroupConfig{
			Colocation: "bogus",
			Members: map[string]CapsuleMemberConfig{
				"db": validMemberConfig("db", "postgres:15", "data"),
			},
		},
	}
	_, _, _, err := cfg.ToGroupSpec()
	if err == nil {
		t.Fatal("expected error for invalid colocation, got nil")
	}
}

// TestIsCapsuleKindGroup is the lightweight discriminator helper.
func TestIsCapsuleKindGroup(t *testing.T) {
	t.Parallel()

	tcases := []struct {
		kind string
		want bool
	}{
		{"", false},
		{"capsule", false},
		{"group", true},
		{"GROUP", false}, // case-sensitive — must match the const
	}
	for _, tc := range tcases {
		c := &CapsuleConfig{Kind: tc.kind}
		if got := c.IsCapsuleKindGroup(); got != tc.want {
			t.Errorf("kind=%q: got %v, want %v", tc.kind, got, tc.want)
		}
	}
}
