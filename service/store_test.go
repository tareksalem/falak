package service

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func makeService(t *testing.T, name string) *Service {
	t.Helper()
	spec := ServiceSpec{
		Name:       name,
		Visibility: VisibilityEnum.Cluster(),
		Group:      "checkout",
		Ports:      []ServicePort{{Name: "http", Port: 8080, Protocol: ProtocolEnum.TCP()}},
		Backends: []ServiceBackend{
			{Capsule: "payments-v1", Weight: 90},
			{Capsule: "payments-v2", Weight: 10},
		},
		Strategy: &Strategy{
			Type: StrategyTypeEnum.Canary(),
			Canary: &CanaryStrategy{
				Target: "payments-v2", From: "payments-v1",
				Step: 10, Interval: 30 * time.Second,
				SuccessCriteria: []string{"error_rate<1"},
				AbortOn:         []string{"error_rate>5"},
			},
		},
	}
	DefaultSpec(&spec)
	return &Service{
		ID:        NewServiceID(),
		ClusterID: "us-east/dc1/prod",
		Spec:      spec,
		Status:    ServiceStatusEnum.Created(),
		Version:   "1",
		BackendStates: []BackendState{
			{Name: "payments-v1", Resolution: BackendResolutionEnum.Resolved(), CapturedCapsuleID: "cap-1"},
			{Name: "payments-v2", Resolution: BackendResolutionEnum.Unresolved()},
		},
	}
}

func TestStore_CreateAndGet(t *testing.T) {
	store := NewStore()
	svc := makeService(t, "payments")

	if err := store.Create(svc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := store.Get(svc.ID)
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.Spec.Name != "payments" {
		t.Errorf("name = %q, want payments", got.Spec.Name)
	}
}

func TestStore_CreateDuplicateRejected(t *testing.T) {
	store := NewStore()
	svc := makeService(t, "payments")
	if err := store.Create(svc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Create(svc); !errors.Is(err, ErrServiceExists) {
		t.Errorf("duplicate Create err = %v, want ErrServiceExists", err)
	}
}

func TestStore_GetByName(t *testing.T) {
	store := NewStore()
	svc := makeService(t, "payments")
	if err := store.Create(svc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := store.GetByName("payments")
	if got == nil || got.ID != svc.ID {
		t.Errorf("GetByName: got %v, want %s", got, svc.ID)
	}
	if store.GetByName("ghost") != nil {
		t.Error("GetByName(ghost) = non-nil, want nil")
	}
}

func TestStore_UpdateAndDelete(t *testing.T) {
	store := NewStore()
	svc := makeService(t, "payments")
	if err := store.Create(svc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	svc.Status = ServiceStatusEnum.Active()
	if err := store.Update(svc); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := store.Get(svc.ID); got.Status != ServiceStatusEnum.Active() {
		t.Errorf("after Update: status = %s, want active", got.Status)
	}

	if err := store.Delete(svc.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := store.Get(svc.ID); got != nil {
		t.Errorf("Get after Delete = %v, want nil", got)
	}
	if err := store.Delete(svc.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Errorf("Delete second time err = %v, want ErrServiceNotFound", err)
	}
}

func TestStore_UpdateNotFound(t *testing.T) {
	store := NewStore()
	svc := makeService(t, "payments")
	if err := store.Update(svc); !errors.Is(err, ErrServiceNotFound) {
		t.Errorf("Update unknown err = %v, want ErrServiceNotFound", err)
	}
}

func TestStore_NilArgs(t *testing.T) {
	store := NewStore()
	if err := store.Create(nil); err == nil {
		t.Error("Create(nil) = nil, want error")
	}
	if err := store.Update(nil); err == nil {
		t.Error("Update(nil) = nil, want error")
	}
}

func TestStore_List_Sorted(t *testing.T) {
	store := NewStore()
	for _, name := range []string{"zeta", "alpha", "mu"} {
		if err := store.Create(makeService(t, name)); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	list := store.List()
	want := []string{"alpha", "mu", "zeta"}
	if len(list) != 3 {
		t.Fatalf("List len = %d, want 3", len(list))
	}
	for i, n := range want {
		if list[i].Spec.Name != n {
			t.Errorf("List[%d] = %s, want %s", i, list[i].Spec.Name, n)
		}
	}
}

func TestStore_ListByVisibility(t *testing.T) {
	store := NewStore()
	c := makeService(t, "cluster-svc")
	c.Spec.Visibility = VisibilityEnum.Cluster()
	g := makeService(t, "group-svc")
	g.Spec.Visibility = VisibilityEnum.Group()
	for _, svc := range []*Service{c, g} {
		if err := store.Create(svc); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	got := store.ListByVisibility(VisibilityEnum.Group())
	if len(got) != 1 || got[0].Spec.Name != "group-svc" {
		t.Errorf("ListByVisibility(group) = %v, want [group-svc]", got)
	}
}

func TestStore_ListByGroup(t *testing.T) {
	store := NewStore()
	a := makeService(t, "a")
	a.Spec.Group = "checkout"
	b := makeService(t, "b")
	b.Spec.Group = "shipping"
	c := makeService(t, "c")
	c.Spec.Group = "checkout"
	for _, svc := range []*Service{a, b, c} {
		if err := store.Create(svc); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	got := store.ListByGroup("checkout")
	if len(got) != 2 {
		t.Fatalf("ListByGroup(checkout) len = %d, want 2", len(got))
	}
	if got[0].Spec.Name != "a" || got[1].Spec.Name != "c" {
		t.Errorf("ListByGroup unsorted: %v", []string{got[0].Spec.Name, got[1].Spec.Name})
	}
	if store.ListByGroup("") != nil {
		t.Error("ListByGroup(\"\") != nil")
	}
}

func TestStore_ListReferencingCapsule(t *testing.T) {
	store := NewStore()
	a := makeService(t, "a")
	a.Spec.Backends = []ServiceBackend{{Capsule: "payments-v1", Weight: 100}}
	b := makeService(t, "b")
	b.Spec.Backends = []ServiceBackend{{Capsule: "billing", Weight: 100}}
	c := makeService(t, "c")
	c.Spec.Backends = []ServiceBackend{{Capsule: "payments-v1", Weight: 100}}
	for _, svc := range []*Service{a, b, c} {
		if err := store.Create(svc); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	got := store.ListReferencingCapsule("payments-v1")
	if len(got) != 2 || got[0].Spec.Name != "a" || got[1].Spec.Name != "c" {
		t.Errorf("ListReferencingCapsule unexpected result: %v", got)
	}
	if store.ListReferencingCapsule("") != nil {
		t.Error("ListReferencingCapsule(\"\") != nil")
	}
}

func TestStore_ListReferencingCapsule_IndexUpdatedOnUpdate(t *testing.T) {
	// Update must drop the previous backend set from the secondary
	// index and register the new one. Otherwise the index would
	// over-report references after a Service swaps backends.
	store := NewStore()
	svc := makeService(t, "svc")
	svc.Spec.Backends = []ServiceBackend{{Capsule: "old-backend", Weight: 100}}
	if err := store.Create(svc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := store.ListReferencingCapsule("old-backend"); len(got) != 1 {
		t.Fatalf("initial old-backend index = %d, want 1", len(got))
	}

	// Swap to a new backend.
	svc.Spec.Backends = []ServiceBackend{{Capsule: "new-backend", Weight: 100}}
	if err := store.Update(svc); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := store.ListReferencingCapsule("old-backend"); len(got) != 0 {
		t.Errorf("post-Update old-backend index = %d, want 0 (stale entry leaked)", len(got))
	}
	if got := store.ListReferencingCapsule("new-backend"); len(got) != 1 {
		t.Errorf("post-Update new-backend index = %d, want 1", len(got))
	}
}

func TestStore_ListReferencingCapsule_IndexUpdatedOnDelete(t *testing.T) {
	// Delete must remove the Service from every capsule-name bucket it
	// referenced. The index then reports the remaining services
	// faithfully.
	store := NewStore()
	a := makeService(t, "a")
	a.Spec.Backends = []ServiceBackend{{Capsule: "shared", Weight: 100}}
	b := makeService(t, "b")
	b.Spec.Backends = []ServiceBackend{{Capsule: "shared", Weight: 100}}
	for _, svc := range []*Service{a, b} {
		if err := store.Create(svc); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if got := store.ListReferencingCapsule("shared"); len(got) != 2 {
		t.Fatalf("pre-Delete shared index = %d, want 2", len(got))
	}
	if err := store.Delete(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got := store.ListReferencingCapsule("shared")
	if len(got) != 1 || got[0].ID != b.ID {
		t.Errorf("post-Delete shared index = %+v, want exactly [b]", got)
	}
}

func TestStore_ListReferencingCapsule_MultiBackendIndex(t *testing.T) {
	// A Service with multiple backends must appear under EACH backend's
	// capsule name in the secondary index — no over-counting, no
	// under-counting.
	store := NewStore()
	svc := makeService(t, "multi")
	svc.Spec.Backends = []ServiceBackend{
		{Capsule: "db-primary", Weight: 80},
		{Capsule: "db-replica", Weight: 20},
	}
	if err := store.Create(svc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := store.ListReferencingCapsule("db-primary"); len(got) != 1 {
		t.Errorf("db-primary index = %d, want 1", len(got))
	}
	if got := store.ListReferencingCapsule("db-replica"); len(got) != 1 {
		t.Errorf("db-replica index = %d, want 1", len(got))
	}
}

func TestStore_Count(t *testing.T) {
	store := NewStore()
	if store.Count() != 0 {
		t.Errorf("empty Count = %d, want 0", store.Count())
	}
	if err := store.Create(makeService(t, "a")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if store.Count() != 1 {
		t.Errorf("Count after Create = %d, want 1", store.Count())
	}
}

func TestStore_CloseInMemoryNoop(t *testing.T) {
	store := NewStore()
	if err := store.Close(); err != nil {
		t.Errorf("Close on in-memory: %v", err)
	}
}

// --- SQLite-backed tests ---

func TestSQLiteStore_RoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "services.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	svc := makeService(t, "payments")
	if err := store.Create(svc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Re-open and verify reload preserves the full spec, including the
	// nested Canary strategy and the backend states.
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	store, err = OpenStore(dbPath)
	if err != nil {
		t.Fatalf("re-open OpenStore: %v", err)
	}
	defer store.Close()

	got := store.Get(svc.ID)
	if got == nil {
		t.Fatal("Get after reload = nil")
	}
	if got.Spec.Name != svc.Spec.Name {
		t.Errorf("name mismatch: got %s, want %s", got.Spec.Name, svc.Spec.Name)
	}
	if got.Spec.Strategy == nil || got.Spec.Strategy.Type != StrategyTypeEnum.Canary() {
		t.Fatalf("strategy after reload = %+v", got.Spec.Strategy)
	}
	if got.Spec.Strategy.Canary == nil || got.Spec.Strategy.Canary.Target != "payments-v2" {
		t.Errorf("canary target lost: %+v", got.Spec.Strategy.Canary)
	}
	if len(got.BackendStates) != 2 {
		t.Fatalf("backend states len = %d, want 2", len(got.BackendStates))
	}
	if got.BackendStates[0].CapturedCapsuleID != "cap-1" {
		t.Errorf("captured ID lost: %+v", got.BackendStates[0])
	}
}

func TestSQLiteStore_DeleteRemovesRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "services.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	svc := makeService(t, "payments")
	if err := store.Create(svc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Delete(svc.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Re-open and confirm the row is gone.
	store.Close()
	store, err = OpenStore(dbPath)
	if err != nil {
		t.Fatalf("re-open OpenStore: %v", err)
	}
	defer store.Close()
	if got := store.Get(svc.ID); got != nil {
		t.Errorf("Get after Delete + reload = %v, want nil", got)
	}
}

func TestSQLiteStore_BackendStatesPreservedAcrossUpdate(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "services.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	svc := makeService(t, "payments")
	if err := store.Create(svc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Mutate the backend state and re-persist.
	svc.BackendStates[1].Resolution = BackendResolutionEnum.UnresolvedCapsuleDeleted()
	svc.BackendStates[1].CapturedCapsuleID = "cap-prev"
	if err := store.Update(svc); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Reload.
	store.Close()
	store, err = OpenStore(dbPath)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer store.Close()
	got := store.Get(svc.ID)
	if got == nil {
		t.Fatal("Get after reload = nil")
	}
	if got.BackendStates[1].Resolution != BackendResolutionEnum.UnresolvedCapsuleDeleted() {
		t.Errorf("backend state lost: %+v", got.BackendStates[1])
	}
	if got.BackendStates[1].CapturedCapsuleID != "cap-prev" {
		t.Errorf("captured ID lost: %s", got.BackendStates[1].CapturedCapsuleID)
	}
}
