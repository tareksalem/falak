package capsule

import (
	"context"
	"testing"
	"time"

	enums "github.com/tareksalem/falak/capsule/enums"
)

// --- CapsuleStatus Enum Tests ---

func TestCapsuleStatusEnum(t *testing.T) {
	tests := []struct {
		name   string
		status enums.CapsuleStatus
		valid  bool
	}{
		{"created", enums.CapsuleStatusEnum.Created(), true},
		{"announced", enums.CapsuleStatusEnum.Announced(), true},
		{"electing", enums.CapsuleStatusEnum.Electing(), true},
		{"assigned", enums.CapsuleStatusEnum.Assigned(), true},
		{"executing", enums.CapsuleStatusEnum.Executing(), true},
		{"running", enums.CapsuleStatusEnum.Running(), true},
		{"stopping", enums.CapsuleStatusEnum.Stopping(), true},
		{"stopped", enums.CapsuleStatusEnum.Stopped(), true},
		{"invalid", enums.CapsuleStatus("invalid"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.status.Valid(); got != tt.valid {
				t.Errorf("CapsuleStatus(%q).Valid() = %v, want %v", tt.status, got, tt.valid)
			}
		})
	}
}

// --- Tier Enum Tests ---

func TestTierEnum(t *testing.T) {
	if enums.TierEnum.Critical().BaseMomentum() != 90 {
		t.Error("critical tier should have base momentum 90")
	}
	if enums.TierEnum.Standard().BaseMomentum() != 50 {
		t.Error("standard tier should have base momentum 50")
	}
	if enums.TierEnum.Background().BaseMomentum() != 20 {
		t.Error("background tier should have base momentum 20")
	}
	if !enums.TierEnum.Critical().Valid() {
		t.Error("critical tier should be valid")
	}
	if enums.Tier("invalid").Valid() {
		t.Error("invalid tier should not be valid")
	}
}

// --- Label Tests ---

func TestLabelsMatch(t *testing.T) {
	candidate := Labels{"region": "us-east", "gpu": "true", "tier": "production"}

	tests := []struct {
		name     string
		required Labels
		expected bool
	}{
		{"empty required matches all", Labels{}, true},
		{"exact match", Labels{"region": "us-east"}, true},
		{"multiple match", Labels{"region": "us-east", "gpu": "true"}, true},
		{"value mismatch", Labels{"region": "eu-west"}, false},
		{"missing key", Labels{"zone": "a"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LabelsMatch(candidate, tt.required); got != tt.expected {
				t.Errorf("LabelsMatch() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestLabelsShareMatch(t *testing.T) {
	source := Labels{"datacenter": "dc1", "region": "us-east"}
	target := Labels{"datacenter": "dc1", "region": "us-east"}
	ruleLabels := Labels{"datacenter": SameKeyword, "region": SameKeyword}

	if !LabelsShareMatch(source, target, ruleLabels) {
		t.Error("should match when labels share same values")
	}

	different := Labels{"datacenter": "dc2", "region": "us-east"}
	if LabelsShareMatch(source, different, ruleLabels) {
		t.Error("should not match when datacenter differs")
	}
}

func TestLabelsMatchAnyValue(t *testing.T) {
	labels := Labels{"region": "us-east"}
	if !LabelsMatchAnyValue(labels, "region", []string{"us-east", "us-west"}) {
		t.Error("should match one of the allowed values")
	}
	if LabelsMatchAnyValue(labels, "region", []string{"eu-west"}) {
		t.Error("should not match when value not in allowed list")
	}
}

func TestLabelsMerge(t *testing.T) {
	base := Labels{"a": "1", "b": "2"}
	overrides := Labels{"b": "3", "c": "4"}
	merged := LabelsMerge(base, overrides)

	if merged["a"] != "1" || merged["b"] != "3" || merged["c"] != "4" {
		t.Errorf("unexpected merge result: %v", merged)
	}
}

func TestLabelsEqual(t *testing.T) {
	a := Labels{"x": "1", "y": "2"}
	b := Labels{"x": "1", "y": "2"}
	c := Labels{"x": "1"}

	if !LabelsEqual(a, b) {
		t.Error("identical labels should be equal")
	}
	if LabelsEqual(a, c) {
		t.Error("different labels should not be equal")
	}
}

// --- Spec Validation Tests ---

func TestDefaultSpec(t *testing.T) {
	spec := CapsuleSpec{
		Name:  "test",
		Image: "test:latest",
		Orbit: "api",
	}
	DefaultSpec(&spec)

	if spec.Tier != enums.TierEnum.Standard() {
		t.Errorf("tier should default to standard, got %s", spec.Tier)
	}
	if spec.Replicas.Min != 1 || spec.Replicas.Max != 1 {
		t.Errorf("replicas should default to min:1 max:1, got min:%d max:%d", spec.Replicas.Min, spec.Replicas.Max)
	}
	if spec.MomentumConfig.Base != 50 {
		t.Errorf("momentum base should default to 50 (standard tier), got %d", spec.MomentumConfig.Base)
	}
	if spec.Runtime.Network.Mode != enums.NetworkModeEnum.Bridge() {
		t.Errorf("network mode should default to bridge, got %s", spec.Runtime.Network.Mode)
	}
	if spec.Runtime.FailurePolicy.RestartLimit != 3 {
		t.Errorf("restart limit should default to 3, got %d", spec.Runtime.FailurePolicy.RestartLimit)
	}
}

func TestValidateSpec_Valid(t *testing.T) {
	spec := CapsuleSpec{
		Name:  "test",
		Image: "test:latest",
		Orbit: "api",
		Tier:  enums.TierEnum.Standard(),
		Replicas: ReplicaConfig{
			Min: 1,
			Max: 3,
		},
	}
	DefaultSpec(&spec)

	if err := ValidateSpec(&spec); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestValidateSpec_MissingName(t *testing.T) {
	spec := CapsuleSpec{Image: "test:latest", Orbit: "api"}
	DefaultSpec(&spec)
	if err := ValidateSpec(&spec); err == nil {
		t.Error("expected error for missing name")
	}
}

func TestValidateSpec_MissingImage(t *testing.T) {
	spec := CapsuleSpec{Name: "test", Orbit: "api"}
	DefaultSpec(&spec)
	if err := ValidateSpec(&spec); err == nil {
		t.Error("expected error for missing image")
	}
}

func TestValidateSpec_MissingOrbit(t *testing.T) {
	spec := CapsuleSpec{Name: "test", Image: "test:latest"}
	DefaultSpec(&spec)
	if err := ValidateSpec(&spec); err == nil {
		t.Error("expected error for missing orbit")
	}
}

func TestValidateSpec_InvalidTier(t *testing.T) {
	spec := CapsuleSpec{Name: "test", Image: "test:latest", Orbit: "api", Tier: "invalid"}
	DefaultSpec(&spec)
	if err := ValidateSpec(&spec); err == nil {
		t.Error("expected error for invalid tier")
	}
}

func TestValidateSpec_ReplicaConflict(t *testing.T) {
	spec := CapsuleSpec{
		Name: "test", Image: "test:latest", Orbit: "api",
		Replicas: ReplicaConfig{Min: 1, Max: 3, Exact: 2},
	}
	DefaultSpec(&spec)
	if err := ValidateSpec(&spec); err == nil {
		t.Error("expected error for replica conflict")
	}
}

func TestValidateSpec_InvalidPlacement(t *testing.T) {
	spec := CapsuleSpec{
		Name: "test", Image: "test:latest", Orbit: "api",
		PlacementRules: []PlacementRule{
			{Type: enums.PlacementTypeEnum.Capsule(), Mode: ""},
		},
	}
	DefaultSpec(&spec)
	if err := ValidateSpec(&spec); err == nil {
		t.Error("expected error for capsule placement without mode")
	}
}

func TestValidateSpec_ScalingRule(t *testing.T) {
	spec := CapsuleSpec{
		Name: "test", Image: "test:latest", Orbit: "api",
		ScalingRules: []ScalingRule{
			{
				Name:       "high load",
				Trigger:    enums.TriggerModeEnum.All(),
				Conditions: []string{"cpu > 70%"},
				Action:     enums.ScalingActionEnum.ScaleUp(),
				Cooldown:   60 * time.Second,
			},
		},
	}
	DefaultSpec(&spec)
	if err := ValidateSpec(&spec); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

// --- Store Tests ---

func TestStoreCRUD(t *testing.T) {
	store := NewStore()

	spec := CapsuleSpec{Name: "test", Image: "test:latest", Orbit: "api"}
	DefaultSpec(&spec)

	c := &Capsule{
		ID:     NewCapsuleID(),
		Spec:   spec,
		Status: enums.CapsuleStatusEnum.Created(),
	}

	// Create
	if err := store.Create(c); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if store.Count() != 1 {
		t.Fatalf("expected count 1, got %d", store.Count())
	}

	// Duplicate create should fail
	if err := store.Create(c); err == nil {
		t.Error("expected error on duplicate create")
	}

	// Get
	got := store.Get(c.ID)
	if got == nil || got.Spec.Name != "test" {
		t.Error("Get returned nil or wrong capsule")
	}

	// GetByName
	got = store.GetByName("test")
	if got == nil || got.ID != c.ID {
		t.Error("GetByName returned nil or wrong capsule")
	}

	// Update
	c.Status = enums.CapsuleStatusEnum.Running()
	if err := store.Update(c); err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	got = store.Get(c.ID)
	if got.Status != enums.CapsuleStatusEnum.Running() {
		t.Error("status not updated")
	}

	// List
	list := store.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 capsule, got %d", len(list))
	}

	// Delete
	if err := store.Delete(c.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if store.Count() != 0 {
		t.Error("expected count 0 after delete")
	}
}

func TestStoreListByOrbit(t *testing.T) {
	store := NewStore()

	for _, orbit := range []string{"api", "api", "workers"} {
		spec := CapsuleSpec{Name: "cap-" + orbit, Image: "img", Orbit: orbit}
		DefaultSpec(&spec)
		c := &Capsule{ID: NewCapsuleID(), Spec: spec, Status: enums.CapsuleStatusEnum.Created()}
		store.Create(c)
	}

	apiCaps := store.ListByOrbit("api")
	if len(apiCaps) != 2 {
		t.Errorf("expected 2 api capsules, got %d", len(apiCaps))
	}

	workerCaps := store.ListByOrbit("workers")
	if len(workerCaps) != 1 {
		t.Errorf("expected 1 worker capsule, got %d", len(workerCaps))
	}
}

func TestStoreListByLabels(t *testing.T) {
	store := NewStore()

	spec1 := CapsuleSpec{Name: "a", Image: "img", Orbit: "api", Labels: Labels{"team": "backend", "tier": "prod"}}
	spec2 := CapsuleSpec{Name: "b", Image: "img", Orbit: "api", Labels: Labels{"team": "frontend", "tier": "prod"}}
	DefaultSpec(&spec1)
	DefaultSpec(&spec2)

	store.Create(&Capsule{ID: NewCapsuleID(), Spec: spec1, Status: enums.CapsuleStatusEnum.Created()})
	store.Create(&Capsule{ID: NewCapsuleID(), Spec: spec2, Status: enums.CapsuleStatusEnum.Created()})

	// Filter by team
	backend := store.ListByLabels(Labels{"team": "backend"})
	if len(backend) != 1 {
		t.Errorf("expected 1 backend capsule, got %d", len(backend))
	}

	// Filter by tier (both match)
	prod := store.ListByLabels(Labels{"tier": "prod"})
	if len(prod) != 2 {
		t.Errorf("expected 2 prod capsules, got %d", len(prod))
	}
}

// --- Manager Tests ---

func TestManagerCreate(t *testing.T) {
	var received []ManagerEvent
	mgr := NewManager(
		WithManagerEventHandler(func(event ManagerEvent) {
			received = append(received, event)
		}),
	)

	spec := CapsuleSpec{
		Name:  "my-api",
		Image: "my-api:v1",
		Orbit: "api",
	}

	c, err := mgr.Create(context.Background(), "test/dc1/cluster1", spec)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if c.ID == "" {
		t.Error("capsule should have an ID")
	}
	// Create auto-announces the capsule so the election subsystem picks it
	// up — so the post-Create status is Announced, not Created.
	if c.Status != enums.CapsuleStatusEnum.Announced() {
		t.Errorf("status should be announced, got %s", c.Status)
	}
	if c.Spec.Tier != enums.TierEnum.Standard() {
		t.Errorf("tier should default to standard, got %s", c.Spec.Tier)
	}
	if c.Momentum.Base != 50 {
		t.Errorf("momentum base should be 50 (standard), got %d", c.Momentum.Base)
	}
	if c.ClusterID != "test/dc1/cluster1" {
		t.Errorf("cluster ID should be test/dc1/cluster1, got %s", c.ClusterID)
	}

	// Create auto-announces BEFORE emitting EventCapsuleCreated so that
	// downstream subscribers (orbit announcer, election kickoff) observe
	// the capsule already at Announced. Therefore the manager emits
	// EventCapsuleAnnounced first, then EventCapsuleCreated.
	if len(received) < 2 {
		t.Fatalf("expected at least 2 events (announced, created), got %d", len(received))
	}
	if received[0].Type != EventCapsuleAnnounced {
		t.Errorf("first event should be %s, got %s", EventCapsuleAnnounced, received[0].Type)
	}
	if received[1].Type != EventCapsuleCreated {
		t.Errorf("second event should be %s, got %s", EventCapsuleCreated, received[1].Type)
	}
}

func TestManagerCRUD(t *testing.T) {
	mgr := NewManager()

	c, err := mgr.Create(context.Background(), "test/dc1/cluster1", CapsuleSpec{
		Name: "test", Image: "img", Orbit: "api",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Get
	got := mgr.Get(c.ID)
	if got == nil {
		t.Fatal("Get returned nil")
	}

	// GetByName
	got = mgr.GetByName("test")
	if got == nil {
		t.Fatal("GetByName returned nil")
	}

	// List
	list := mgr.List()
	if len(list) != 1 {
		t.Fatalf("expected 1, got %d", len(list))
	}

	// Update
	updated, err := mgr.Update(context.Background(), c.ID, CapsuleSpec{
		Name: "test-v2", Image: "img:v2", Orbit: "api",
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if updated.Spec.Name != "test-v2" {
		t.Error("name not updated")
	}

	// Delete
	if err := mgr.Delete(context.Background(), c.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if mgr.Count() != 0 {
		t.Error("count should be 0 after delete")
	}
}

func TestManagerReceive(t *testing.T) {
	var received []ManagerEvent
	mgr := NewManager(
		WithManagerEventHandler(func(event ManagerEvent) {
			received = append(received, event)
		}),
	)

	c := &Capsule{
		ID:     NewCapsuleID(),
		Spec:   CapsuleSpec{Name: "remote", Image: "img", Orbit: "api"},
		Status: enums.CapsuleStatusEnum.Announced(),
	}

	if err := mgr.Receive(c); err != nil {
		t.Fatalf("Receive failed: %v", err)
	}

	if mgr.Count() != 1 {
		t.Error("should have 1 capsule after receive")
	}

	// Verify event
	found := false
	for _, e := range received {
		if e.Type == EventCapsuleReceived {
			found = true
		}
	}
	if !found {
		t.Error("should have received CapsuleReceived event")
	}

	// Update existing via Receive
	c.Status = enums.CapsuleStatusEnum.Running()
	if err := mgr.Receive(c); err != nil {
		t.Fatalf("Receive update failed: %v", err)
	}

	got := mgr.Get(c.ID)
	if got.Status != enums.CapsuleStatusEnum.Running() {
		t.Error("status should be updated after second receive")
	}
}

func TestManagerCreateValidation(t *testing.T) {
	mgr := NewManager()

	// Missing name
	_, err := mgr.Create(context.Background(), "c", CapsuleSpec{Image: "img", Orbit: "api"})
	if err == nil {
		t.Error("expected error for missing name")
	}

	// Missing image
	_, err = mgr.Create(context.Background(), "c", CapsuleSpec{Name: "test", Orbit: "api"})
	if err == nil {
		t.Error("expected error for missing image")
	}

	// Missing cluster
	_, err = mgr.Create(context.Background(), "", CapsuleSpec{Name: "test", Image: "img", Orbit: "api"})
	if err == nil {
		t.Error("expected error for missing cluster")
	}
}
