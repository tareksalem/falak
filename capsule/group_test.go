package capsule

import "testing"

// --- CapsuleKind Enum Tests ---

func TestCapsuleKindEnum(t *testing.T) {
	if got := CapsuleKindEnum.Capsule(); got != CapsuleKind("capsule") {
		t.Errorf("CapsuleKindEnum.Capsule() = %q, want %q", got, "capsule")
	}
	if got := CapsuleKindEnum.Group(); got != CapsuleKind("group") {
		t.Errorf("CapsuleKindEnum.Group() = %q, want %q", got, "group")
	}

	tests := []struct {
		name  string
		kind  CapsuleKind
		valid bool
	}{
		{"capsule", CapsuleKindEnum.Capsule(), true},
		{"group", CapsuleKindEnum.Group(), true},
		{"empty", CapsuleKind(""), false},
		{"garbage", CapsuleKind("not-a-kind"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.kind.Valid(); got != tt.valid {
				t.Errorf("CapsuleKind(%q).Valid() = %v, want %v", tt.kind, got, tt.valid)
			}
		})
	}
}

// --- ColocationMode Enum Tests ---

func TestColocationModeEnum(t *testing.T) {
	if got := ColocationModeEnum.SameNode(); got != ColocationMode("same-node") {
		t.Errorf("ColocationModeEnum.SameNode() = %q, want %q", got, "same-node")
	}
	if got := ColocationModeEnum.SameOrbit(); got != ColocationMode("same-orbit") {
		t.Errorf("ColocationModeEnum.SameOrbit() = %q, want %q", got, "same-orbit")
	}

	tests := []struct {
		name  string
		mode  ColocationMode
		valid bool
	}{
		{"same-node", ColocationModeEnum.SameNode(), true},
		{"same-orbit", ColocationModeEnum.SameOrbit(), true},
		{"empty", ColocationMode(""), false},
		{"garbage", ColocationMode("anywhere"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.mode.Valid(); got != tt.valid {
				t.Errorf("ColocationMode(%q).Valid() = %v, want %v", tt.mode, got, tt.valid)
			}
		})
	}
}

// --- Zero Value Tests (shape sentinels) ---

func TestMemberSpec_Zero(t *testing.T) {
	var m MemberSpec

	if m.Name != "" {
		t.Errorf("zero MemberSpec.Name = %q, want empty", m.Name)
	}
	if m.DependsOn != nil {
		t.Errorf("zero MemberSpec.DependsOn = %v, want nil", m.DependsOn)
	}

	// Spec is a zero CapsuleSpec; lock its key zero fields so future
	// CapsuleSpec changes don't silently flip member-spec defaults.
	if m.Spec.Name != "" {
		t.Errorf("zero MemberSpec.Spec.Name = %q, want empty", m.Spec.Name)
	}
	if m.Spec.Image != "" {
		t.Errorf("zero MemberSpec.Spec.Image = %q, want empty", m.Spec.Image)
	}
	if m.Spec.Group != nil {
		t.Errorf("zero MemberSpec.Spec.Group = %v, want nil", m.Spec.Group)
	}
}

func TestGroupSpec_Zero(t *testing.T) {
	var g GroupSpec

	if g.Colocation != "" {
		t.Errorf("zero GroupSpec.Colocation = %q, want empty (defaults applied later)", g.Colocation)
	}
	if g.Members != nil {
		t.Errorf("zero GroupSpec.Members = %v, want nil", g.Members)
	}
	if g.MemberIDs != nil {
		t.Errorf("zero GroupSpec.MemberIDs = %v, want nil", g.MemberIDs)
	}
	if g.CascadeDelete {
		t.Errorf("zero GroupSpec.CascadeDelete = true, want false (default true is applied by DefaultSpec, not the type)")
	}
}

// --- CapsuleSpec Zero Tests for Group Fields ---

func TestCapsuleSpec_GroupFields_Zero(t *testing.T) {
	t.Run("kind zero is empty (no auto-default at type level)", func(t *testing.T) {
		var s CapsuleSpec
		if s.Kind != "" {
			t.Errorf("zero CapsuleSpec.Kind = %q, want empty (DefaultSpec sets default in task 10.5)", s.Kind)
		}
	})

	t.Run("group-id zero is empty CapsuleID", func(t *testing.T) {
		var s CapsuleSpec
		if s.GroupID != "" {
			t.Errorf("zero CapsuleSpec.GroupID = %q, want empty", s.GroupID)
		}
	})

	t.Run("group-member zero is false", func(t *testing.T) {
		var s CapsuleSpec
		if s.GroupMember {
			t.Error("zero CapsuleSpec.GroupMember = true, want false")
		}
	})

	t.Run("group pointer zero is nil", func(t *testing.T) {
		var s CapsuleSpec
		if s.Group != nil {
			t.Errorf("zero CapsuleSpec.Group = %v, want nil", s.Group)
		}
	})
}
