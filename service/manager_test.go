package service

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// stubCapsuleLookup is a hand-tuned CapsuleLookup used in manager tests.
type stubCapsuleLookup struct {
	mu  sync.Mutex
	ids map[string]string
}

func newStubCapsules(initial map[string]string) *stubCapsuleLookup {
	out := &stubCapsuleLookup{ids: make(map[string]string)}
	for k, v := range initial {
		out.ids[k] = v
	}
	return out
}

func (s *stubCapsuleLookup) Lookup(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.ids[name]
	return id, ok
}

func (s *stubCapsuleLookup) set(name, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		delete(s.ids, name)
		return
	}
	s.ids[name] = id
}

// eventRecorder captures every ManagerEvent emitted by the manager.
type eventRecorder struct {
	mu     sync.Mutex
	events []ManagerEvent
}

func (r *eventRecorder) handler() EventHandler {
	return func(e ManagerEvent) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, e)
	}
}

func (r *eventRecorder) typesFor(svcID ServiceID) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for _, e := range r.events {
		if e.ServiceID == svcID {
			out = append(out, e.Type)
		}
	}
	return out
}

func (r *eventRecorder) lastOfType(t string) *ManagerEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].Type == t {
			e := r.events[i]
			return &e
		}
	}
	return nil
}

func managerSpec() ServiceSpec {
	return ServiceSpec{
		Name:       "payments",
		Visibility: VisibilityEnum.Cluster(),
		Ports:      []ServicePort{{Name: "http", Port: 8080, Protocol: ProtocolEnum.TCP()}},
		Backends: []ServiceBackend{
			{Capsule: "payments-v1", Weight: 90},
			{Capsule: "payments-v2", Weight: 10},
		},
		Strategy: &Strategy{Type: StrategyTypeEnum.Static()},
	}
}

func newManagerForTest(caps CapsuleLookup, rec *eventRecorder) *Manager {
	m := NewManager(WithCapsuleLookup(caps))
	if rec != nil {
		m.SetEventHandler(rec.handler())
	}
	return m
}

func TestManager_Create_ResolvesBackends(t *testing.T) {
	caps := newStubCapsules(map[string]string{"payments-v1": "cap-001"})
	rec := &eventRecorder{}
	m := newManagerForTest(caps, rec)

	svc, err := m.Create(context.Background(), "c1", managerSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if svc.Spec.Backends[0].CapturedCapsuleID != "cap-001" {
		t.Errorf("v1 captured id: got %q", svc.Spec.Backends[0].CapturedCapsuleID)
	}
	if svc.Spec.Backends[1].CapturedCapsuleID != "" {
		t.Errorf("v2 should be unresolved")
	}
	if svc.BackendStates[0].Resolution != BackendResolutionEnum.Resolved() {
		t.Errorf("v1 state: got %q", svc.BackendStates[0].Resolution)
	}
	if svc.BackendStates[1].Resolution != BackendResolutionEnum.Unresolved() {
		t.Errorf("v2 state: got %q", svc.BackendStates[1].Resolution)
	}
	if got := m.Get(svc.ID); got == nil || got.ID != svc.ID {
		t.Error("Get failed after Create")
	}
	// Lifecycle should be Active after Create's auto-Activate.
	if got, _ := m.Status(svc.ID); got != ServiceStatusEnum.Active() {
		t.Errorf("lifecycle: got %q want active", got)
	}
}

// Status exposes the lifecycle state for test inspection.
func (m *Manager) Status(id ServiceID) (ServiceStatus, error) {
	lc := m.getLifecycle(id)
	if lc == nil {
		return "", errors.New("no lifecycle")
	}
	return lc.State(), nil
}

func TestManager_Create_DuplicateName_Rejected(t *testing.T) {
	m := newManagerForTest(newStubCapsules(nil), nil)
	if _, err := m.Create(context.Background(), "c1", managerSpec()); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := m.Create(context.Background(), "c1", managerSpec())
	if err == nil || !errors.Is(err, ErrServiceNameExists) {
		t.Fatalf("expected ErrServiceNameExists, got %v", err)
	}
}

func TestManager_OnCapsuleReceived_ResolvesUnresolved(t *testing.T) {
	caps := newStubCapsules(nil)
	rec := &eventRecorder{}
	m := newManagerForTest(caps, rec)
	svc, _ := m.Create(context.Background(), "c1", managerSpec())

	caps.set("payments-v2", "cap-200")
	m.OnCapsuleReceived("payments-v2", "cap-200")

	got := m.Get(svc.ID)
	state := findBackendState(got.BackendStates, "payments-v2")
	if state == nil || state.Resolution != BackendResolutionEnum.Resolved() {
		t.Fatalf("v2 not resolved: %+v", state)
	}
	if state.CapturedCapsuleID != "cap-200" {
		t.Errorf("v2 captured id: got %q want cap-200", state.CapturedCapsuleID)
	}
	if rec.lastOfType(EventServiceBackendResolved) == nil {
		t.Error("EventServiceBackendResolved not emitted")
	}
}

func TestManager_OnCapsuleReceived_SameIDNoOp(t *testing.T) {
	caps := newStubCapsules(map[string]string{"payments-v1": "cap-001"})
	rec := &eventRecorder{}
	m := newManagerForTest(caps, rec)
	svc, _ := m.Create(context.Background(), "c1", managerSpec())

	beforeEvents := len(rec.events)
	m.OnCapsuleReceived("payments-v1", "cap-001")
	if len(rec.events) > beforeEvents {
		// Allow benign no-op behaviour: but identity-changed/resolved should not fire.
		for _, e := range rec.events[beforeEvents:] {
			if e.Type == EventServiceBackendIdentityChanged ||
				e.Type == EventServiceBackendResolved {
				t.Errorf("unexpected event on identical re-resolve: %s", e.Type)
			}
		}
	}
	got := m.Get(svc.ID)
	state := findBackendState(got.BackendStates, "payments-v1")
	if state.Resolution != BackendResolutionEnum.Resolved() {
		t.Errorf("v1 resolution lost: %q", state.Resolution)
	}
}

func TestManager_OnCapsuleReceived_IdentityChange(t *testing.T) {
	caps := newStubCapsules(map[string]string{"payments-v1": "cap-001"})
	rec := &eventRecorder{}
	m := newManagerForTest(caps, rec)
	svc, _ := m.Create(context.Background(), "c1", managerSpec())

	caps.set("payments-v1", "cap-999")
	m.OnCapsuleReceived("payments-v1", "cap-999")

	got := m.Get(svc.ID)
	state := findBackendState(got.BackendStates, "payments-v1")
	if state == nil || state.Resolution != BackendResolutionEnum.UnresolvedIdentityChanged() {
		t.Fatalf("expected UnresolvedIdentityChanged, got %+v", state)
	}
	if state.CapturedCapsuleID != "cap-001" {
		t.Errorf("captured id should be preserved as cap-001, got %q", state.CapturedCapsuleID)
	}
	ev := rec.lastOfType(EventServiceBackendIdentityChanged)
	if ev == nil {
		t.Fatal("EventServiceBackendIdentityChanged not emitted")
	}
	if ev.Meta[MetaPreviousCapsuleID] != "cap-001" {
		t.Errorf("event missing previous id: %v", ev.Meta)
	}
	if ev.Meta[MetaNewCapsuleID] != "cap-999" {
		t.Errorf("event missing new id: %v", ev.Meta)
	}
	_ = svc
}

// TestManager_OnCapsuleReceived_IdentityChange_ZeroesWeight verifies
// Decision #14: identity-changed backends have their spec weight
// zeroed so downstream strategy engines (canary auto-abort, static
// drop) treat them as inadmissible without a separate signal. A
// follow-up EventServiceUpdated fires so subscribers re-read the
// spec on the same event-bus path.
func TestManager_OnCapsuleReceived_IdentityChange_ZeroesWeight(t *testing.T) {
	caps := newStubCapsules(map[string]string{"payments-v1": "cap-001"})
	rec := &eventRecorder{}
	m := newManagerForTest(caps, rec)
	svc, _ := m.Create(context.Background(), "c1", managerSpec())

	caps.set("payments-v1", "cap-999")
	m.OnCapsuleReceived("payments-v1", "cap-999")

	got := m.Get(svc.ID)
	var zeroed bool
	for _, b := range got.Spec.Backends {
		if b.Capsule == "payments-v1" {
			if b.Weight != 0 {
				t.Fatalf("expected payments-v1 weight zeroed on identity change, got %d", b.Weight)
			}
			zeroed = true
		}
	}
	if !zeroed {
		t.Fatal("payments-v1 missing from spec after identity change")
	}
	if rec.lastOfType(EventServiceBackendIdentityChanged) == nil {
		t.Error("EventServiceBackendIdentityChanged not emitted")
	}
	if rec.lastOfType(EventServiceUpdated) == nil {
		t.Error("EventServiceUpdated must follow identity change so engines re-read spec")
	}
}

func TestManager_OnCapsuleDeleted_MarksUnresolvedCapsuleDeleted(t *testing.T) {
	caps := newStubCapsules(map[string]string{"payments-v1": "cap-001"})
	rec := &eventRecorder{}
	m := newManagerForTest(caps, rec)
	svc, _ := m.Create(context.Background(), "c1", managerSpec())

	m.OnCapsuleDeleted("payments-v1")

	got := m.Get(svc.ID)
	state := findBackendState(got.BackendStates, "payments-v1")
	if state.Resolution != BackendResolutionEnum.UnresolvedCapsuleDeleted() {
		t.Fatalf("expected UnresolvedCapsuleDeleted, got %q", state.Resolution)
	}
	if state.CapturedCapsuleID != "cap-001" {
		t.Errorf("captured id must be preserved for rebind comparison, got %q",
			state.CapturedCapsuleID)
	}
	if rec.lastOfType(EventServiceBackendUnresolved) == nil {
		t.Error("EventServiceBackendUnresolved not emitted")
	}
}

func TestManager_Rebind_ClearsAndResolves(t *testing.T) {
	caps := newStubCapsules(map[string]string{"payments-v1": "cap-001"})
	rec := &eventRecorder{}
	m := newManagerForTest(caps, rec)
	svc, _ := m.Create(context.Background(), "c1", managerSpec())

	// Identity change → backend unresolved-identity-changed.
	caps.set("payments-v1", "cap-999")
	m.OnCapsuleReceived("payments-v1", "cap-999")

	// Operator rebinds.
	if err := m.Rebind(context.Background(), svc.ID, "payments-v1"); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	got := m.Get(svc.ID)
	state := findBackendState(got.BackendStates, "payments-v1")
	if state.Resolution != BackendResolutionEnum.Resolved() {
		t.Fatalf("expected Resolved after Rebind, got %q", state.Resolution)
	}
	if state.CapturedCapsuleID != "cap-999" {
		t.Errorf("Rebind should capture new id, got %q", state.CapturedCapsuleID)
	}
}

// recordingPublisher captures Publisher API calls for the Delete-publishes
// test below.
type recordingPublisher struct {
	mu          sync.Mutex
	updates     []*Service
	withdrawals []ServiceID
}

func (r *recordingPublisher) PublishUpdate(_ context.Context, svc *Service) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, svc)
	return nil
}

func (r *recordingPublisher) PublishWithdrawal(_ context.Context, id ServiceID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.withdrawals = append(r.withdrawals, id)
	return nil
}

func newManagerWithPublisher(caps CapsuleLookup, rec *eventRecorder, pub publisher) *Manager {
	m := NewManager(WithCapsuleLookup(caps))
	if rec != nil {
		m.SetEventHandler(rec.handler())
	}
	m.publisher = pub
	return m
}

func TestManager_Delete_PublishesWithdrawal(t *testing.T) {
	pub := &recordingPublisher{}
	rec := &eventRecorder{}
	m := newManagerWithPublisher(newStubCapsules(nil), rec, pub)

	svc, err := m.Create(context.Background(), "c1", managerSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Delete(context.Background(), svc.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	pub.mu.Lock()
	wdLen := len(pub.withdrawals)
	updLen := len(pub.updates)
	pub.mu.Unlock()
	if wdLen != 1 || pub.withdrawals[0] != svc.ID {
		t.Errorf("expected 1 withdrawal for %s, got %+v", svc.ID, pub.withdrawals)
	}
	if updLen != 1 {
		t.Errorf("expected 1 update from Create, got %d", updLen)
	}

	// Second Delete should report not-found (idempotent for reapers).
	err = m.Delete(context.Background(), svc.ID)
	if err == nil || !errors.Is(err, ErrServiceNotFound) {
		t.Errorf("second Delete: expected ErrServiceNotFound, got %v", err)
	}
}

func TestManager_Receive_StoresGossipArrivedService(t *testing.T) {
	rec := &eventRecorder{}
	m := newManagerForTest(newStubCapsules(nil), rec)

	incoming := &Service{
		ID: "remote-1", ClusterID: "c1", Status: ServiceStatusEnum.Active(),
		Spec: managerSpec(),
	}
	if err := m.Receive(incoming); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if got := m.Get("remote-1"); got == nil || got.Spec.Name != "payments" {
		t.Fatal("Receive did not persist Service")
	}
	if rec.lastOfType(EventServiceReceived) == nil {
		t.Error("EventServiceReceived not emitted")
	}
}

func TestManager_Apply_CreatesThenUpdates(t *testing.T) {
	rec := &eventRecorder{}
	m := newManagerForTest(newStubCapsules(nil), rec)

	spec := managerSpec()
	first, err := m.Apply(context.Background(), "c1", spec)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if rec.lastOfType(EventServiceCreated) == nil {
		t.Error("Apply absent: expected EventServiceCreated")
	}

	// Bump weight; Apply must Update, not Create.
	spec.Backends[0].Weight = 70
	spec.Backends[1].Weight = 30
	second, err := m.Apply(context.Background(), "c1", spec)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("Apply created new Service: first=%s second=%s", first.ID, second.ID)
	}
	if rec.lastOfType(EventServiceUpdated) == nil {
		t.Error("Apply present: expected EventServiceUpdated")
	}
	got := m.Get(second.ID)
	if got.Spec.Backends[0].Weight != 70 || got.Spec.Backends[1].Weight != 30 {
		t.Errorf("weights not applied: %+v", got.Spec.Backends)
	}
}

func TestManager_HandleWithdrawal_RemovesService(t *testing.T) {
	rec := &eventRecorder{}
	m := newManagerForTest(newStubCapsules(nil), rec)
	svc, _ := m.Create(context.Background(), "c1", managerSpec())

	if err := m.HandleWithdrawal(svc.ID); err != nil {
		t.Fatalf("HandleWithdrawal: %v", err)
	}
	if got := m.Get(svc.ID); got != nil {
		t.Fatal("HandleWithdrawal did not remove Service")
	}
	// Second call must be a no-op (idempotent).
	if err := m.HandleWithdrawal(svc.ID); err != nil {
		t.Fatalf("second HandleWithdrawal: %v", err)
	}
}
