package capsule

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStore_CreateAndGet(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "capsules.db")

	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	spec := CapsuleSpec{
		Name:  "test-api",
		Image: "test:v1",
		Orbit: "api",
		Tier:  TierEnum.Critical(),
		Labels: Labels{
			"team": "backend",
			"env":  "production",
		},
		Resources: ResourceRequirements{CPUCores: 2, MemoryMB: 512, DiskMB: 1024},
		Replicas:  ReplicaConfig{Min: 2, Max: 5},
		ScalingRules: []ScalingRule{
			{
				Name:       "high cpu",
				Trigger:    TriggerModeEnum.All(),
				Conditions: []string{"cpu > 70%"},
				Action:     ScalingActionEnum.ScaleUp(),
				Cooldown:   60 * time.Second,
			},
		},
		PlacementRules: []PlacementRule{
			{
				Name:     "gpu nodes",
				Type:     PlacementTypeEnum.Node(),
				Labels:   Labels{"gpu": "true"},
				Required: true,
			},
		},
		MomentumConfig: MomentumConfig{
			Base:           90,
			BoostOnTraffic: true,
			ReduceOnIdle:   true,
			IdleTimeout:    5 * time.Minute,
		},
	}
	DefaultSpec(&spec)

	c := &Capsule{
		ID:        NewCapsuleID(),
		ClusterID: "us-east/dc1/prod",
		Spec:      spec,
		Status:    CapsuleStatusEnum.Created(),
		Momentum: MomentumState{
			Current:      90,
			Base:         90,
			LastAdjusted: time.Now(),
		},
		Version: "1",
	}

	if err := store.Create(c); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Verify in-memory
	got := store.Get(c.ID)
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.Spec.Name != "test-api" {
		t.Errorf("name: got %q, want %q", got.Spec.Name, "test-api")
	}

	// Close and reopen to verify persistence
	store.Close()

	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore (reopen) failed: %v", err)
	}
	defer store2.Close()

	if store2.Count() != 1 {
		t.Fatalf("expected 1 capsule after reopen, got %d", store2.Count())
	}

	reloaded := store2.Get(c.ID)
	if reloaded == nil {
		t.Fatal("Get after reopen returned nil")
	}

	// Verify full spec survived serialization
	if reloaded.Spec.Name != "test-api" {
		t.Errorf("name: got %q, want %q", reloaded.Spec.Name, "test-api")
	}
	if reloaded.Spec.Tier != TierEnum.Critical() {
		t.Errorf("tier: got %q, want %q", reloaded.Spec.Tier, TierEnum.Critical())
	}
	if reloaded.Spec.Labels["team"] != "backend" {
		t.Errorf("labels: team should be backend")
	}
	if reloaded.Spec.Resources.CPUCores != 2 {
		t.Errorf("cpu: got %d, want 2", reloaded.Spec.Resources.CPUCores)
	}
	if reloaded.Spec.Replicas.Min != 2 || reloaded.Spec.Replicas.Max != 5 {
		t.Errorf("replicas: got min=%d max=%d", reloaded.Spec.Replicas.Min, reloaded.Spec.Replicas.Max)
	}
	if len(reloaded.Spec.ScalingRules) != 1 {
		t.Fatalf("scaling rules: got %d, want 1", len(reloaded.Spec.ScalingRules))
	}
	if reloaded.Spec.ScalingRules[0].Name != "high cpu" {
		t.Errorf("scaling rule name: got %q", reloaded.Spec.ScalingRules[0].Name)
	}
	if len(reloaded.Spec.PlacementRules) != 1 {
		t.Fatalf("placement rules: got %d, want 1", len(reloaded.Spec.PlacementRules))
	}
	if reloaded.Spec.MomentumConfig.Base != 90 {
		t.Errorf("momentum base: got %d, want 90", reloaded.Spec.MomentumConfig.Base)
	}
	if reloaded.Momentum.Current != 90 {
		t.Errorf("momentum current: got %d, want 90", reloaded.Momentum.Current)
	}
	if reloaded.ClusterID != "us-east/dc1/prod" {
		t.Errorf("cluster_id: got %q", reloaded.ClusterID)
	}
	if string(reloaded.Status) != string(CapsuleStatusEnum.Created()) {
		t.Errorf("status: got %q", reloaded.Status)
	}
}

func TestSQLiteStore_Update(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	spec := CapsuleSpec{Name: "test", Image: "img", Orbit: "api"}
	DefaultSpec(&spec)
	c := &Capsule{ID: NewCapsuleID(), Spec: spec, Status: CapsuleStatusEnum.Created()}

	store.Create(c)

	c.Status = CapsuleStatusEnum.Running()
	c.Spec.Image = "img:v2"
	if err := store.Update(c); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	// Close and reopen
	store.Close()
	store2, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore (reopen) failed: %v", err)
	}
	defer store2.Close()

	got := store2.Get(c.ID)
	if got.Status != CapsuleStatusEnum.Running() {
		t.Errorf("status should be running, got %s", got.Status)
	}
	if got.Spec.Image != "img:v2" {
		t.Errorf("image should be img:v2, got %s", got.Spec.Image)
	}
}

func TestSQLiteStore_Delete(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	spec := CapsuleSpec{Name: "test", Image: "img", Orbit: "api"}
	DefaultSpec(&spec)
	c := &Capsule{ID: NewCapsuleID(), Spec: spec, Status: CapsuleStatusEnum.Created()}

	store.Create(c)
	store.Delete(c.ID)

	// Close and reopen
	store.Close()
	store2, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore (reopen) failed: %v", err)
	}
	defer store2.Close()

	if store2.Count() != 0 {
		t.Errorf("should have 0 capsules after delete, got %d", store2.Count())
	}
}

func TestSQLiteStore_MultipleCapsules(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	for _, name := range []string{"api", "db", "worker", "cache"} {
		spec := CapsuleSpec{Name: name, Image: name + ":latest", Orbit: "default"}
		DefaultSpec(&spec)
		c := &Capsule{ID: NewCapsuleID(), Spec: spec, Status: CapsuleStatusEnum.Created()}
		store.Create(c)
	}

	if store.Count() != 4 {
		t.Fatalf("expected 4, got %d", store.Count())
	}

	// Close and reopen
	store.Close()
	store2, err := OpenStore(filepath.Join(dir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore (reopen) failed: %v", err)
	}
	defer store2.Close()

	if store2.Count() != 4 {
		t.Errorf("expected 4 after reopen, got %d", store2.Count())
	}

	// Verify all names present
	names := map[string]bool{}
	for _, c := range store2.List() {
		names[c.Spec.Name] = true
	}
	for _, expected := range []string{"api", "db", "worker", "cache"} {
		if !names[expected] {
			t.Errorf("missing capsule %q after reopen", expected)
		}
	}
}

func TestSQLiteStore_InMemoryFallback(t *testing.T) {
	// NewStore (no path) should work without SQLite
	store := NewStore()
	spec := CapsuleSpec{Name: "test", Image: "img", Orbit: "api"}
	DefaultSpec(&spec)
	c := &Capsule{ID: NewCapsuleID(), Spec: spec, Status: CapsuleStatusEnum.Created()}

	if err := store.Create(c); err != nil {
		t.Fatalf("in-memory Create failed: %v", err)
	}
	if store.Count() != 1 {
		t.Errorf("expected 1, got %d", store.Count())
	}
}

func TestSQLiteStore_NonExistentDir(t *testing.T) {
	// Opening with a nonexistent parent dir should still work (sqlite creates the file)
	dir := t.TempDir()
	subdir := filepath.Join(dir, "sub", "dir")
	os.MkdirAll(subdir, 0755)

	store, err := OpenStore(filepath.Join(subdir, "capsules.db"))
	if err != nil {
		t.Fatalf("OpenStore in nested dir failed: %v", err)
	}
	defer store.Close()
}
