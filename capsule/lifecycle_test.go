package capsule

import (
	"context"
	"testing"

	"github.com/tareksalem/falak/capsule/enums"
)

func TestLifecycleHappyPath(t *testing.T) {
	id := NewCapsuleID()
	var transitions []LifecycleEvent
	lc := NewLifecycle(id, WithLifecycleHandler(func(e LifecycleEvent) {
		transitions = append(transitions, e)
	}))

	if lc.State() != enums.CapsuleStatusEnum.Created() {
		t.Fatalf("initial state should be created, got %s", lc.State())
	}

	steps := []struct {
		trigger  string
		expected enums.CapsuleStatus
	}{
		{TriggerAnnounce, enums.CapsuleStatusEnum.Announced()},
		{TriggerElectionStarted, enums.CapsuleStatusEnum.Electing()},
		{TriggerElectionWon, enums.CapsuleStatusEnum.Assigned()},
		{TriggerExecutionStart, enums.CapsuleStatusEnum.Executing()},
		{TriggerContainerReady, enums.CapsuleStatusEnum.Running()},
		{TriggerStopRequested, enums.CapsuleStatusEnum.Stopping()},
		{TriggerContainerStopped, enums.CapsuleStatusEnum.Stopped()},
	}

	for _, step := range steps {
		if err := lc.Fire(step.trigger); err != nil {
			t.Fatalf("Fire(%s) failed: %v", step.trigger, err)
		}
		if lc.State() != step.expected {
			t.Fatalf("after %s: expected %s, got %s", step.trigger, step.expected, lc.State())
		}
	}

	if len(transitions) == 0 {
		t.Error("should have received transition events")
	}
}

func TestLifecycleReElectionOnNodeFailure(t *testing.T) {
	id := NewCapsuleID()
	lc := NewLifecycle(id)

	// Advance to running
	lc.Fire(TriggerAnnounce)
	lc.Fire(TriggerElectionStarted)
	lc.Fire(TriggerElectionWon)
	lc.Fire(TriggerExecutionStart)
	lc.Fire(TriggerContainerReady)

	if lc.State() != enums.CapsuleStatusEnum.Running() {
		t.Fatal("should be running")
	}

	if err := lc.Fire(TriggerNodeFailed); err != nil {
		t.Fatalf("Fire(NodeFailed) failed: %v", err)
	}

	if lc.State() != enums.CapsuleStatusEnum.Announced() {
		t.Errorf("should be announced after node failure, got %s", lc.State())
	}
}

func TestLifecycleElectionTimeout(t *testing.T) {
	id := NewCapsuleID()
	lc := NewLifecycle(id)

	lc.Fire(TriggerAnnounce)
	lc.Fire(TriggerElectionStarted)

	if err := lc.Fire(TriggerElectionTimeout); err != nil {
		t.Fatalf("Fire(ElectionTimeout) failed: %v", err)
	}

	if lc.State() != enums.CapsuleStatusEnum.Announced() {
		t.Errorf("should be back to announced, got %s", lc.State())
	}
}

func TestLifecycleInvalidTransition(t *testing.T) {
	id := NewCapsuleID()
	lc := NewLifecycle(id)

	if err := lc.Fire(TriggerElectionWon); err == nil {
		t.Error("expected error for invalid transition")
	}
}

func TestLifecycleCanFire(t *testing.T) {
	id := NewCapsuleID()
	lc := NewLifecycle(id)

	if !lc.CanFire(TriggerAnnounce) {
		t.Error("should be able to fire announce from created")
	}
	if lc.CanFire(TriggerElectionWon) {
		t.Error("should not be able to fire election_won from created")
	}
}

func TestLifecycleRestartFromStopped(t *testing.T) {
	id := NewCapsuleID()
	lc := NewLifecycle(id)

	lc.Fire(TriggerAnnounce)
	lc.Fire(TriggerElectionStarted)
	lc.Fire(TriggerElectionWon)
	lc.Fire(TriggerExecutionStart)
	lc.Fire(TriggerContainerReady)
	lc.Fire(TriggerStopRequested)
	lc.Fire(TriggerContainerStopped)

	if lc.State() != enums.CapsuleStatusEnum.Stopped() {
		t.Fatal("should be stopped")
	}

	if err := lc.Fire(TriggerAnnounce); err != nil {
		t.Fatalf("restart failed: %v", err)
	}
	if lc.State() != enums.CapsuleStatusEnum.Announced() {
		t.Errorf("should be announced after restart, got %s", lc.State())
	}
}

func TestLifecycleInitialState(t *testing.T) {
	// A lifecycle restored from persistence should start at the given state.
	id := NewCapsuleID()
	lc := NewLifecycle(id, WithLifecycleInitialState(enums.CapsuleStatusEnum.Running()))

	if lc.State() != enums.CapsuleStatusEnum.Running() {
		t.Errorf("expected Running, got %s", lc.State())
	}
	// From Running we can fire StopRequested
	if err := lc.Fire(TriggerStopRequested); err != nil {
		t.Errorf("Fire(StopRequested) failed: %v", err)
	}
	if lc.State() != enums.CapsuleStatusEnum.Stopping() {
		t.Errorf("expected Stopping, got %s", lc.State())
	}
}

// --- MembersAdmitted (group) trigger ---

// TestLifecycle_MembersAdmittedTrigger_FromAnnounced verifies the FSM accepts
// MembersAdmitted from Announced and transitions to Running. The FSM itself
// is uniform across kinds; the Manager seam enforces kind gating.
func TestLifecycle_MembersAdmittedTrigger_FromAnnounced(t *testing.T) {
	id := NewCapsuleID()
	lc := NewLifecycle(id)

	if err := lc.Fire(TriggerAnnounce); err != nil {
		t.Fatalf("Fire(Announce) failed: %v", err)
	}
	if lc.State() != enums.CapsuleStatusEnum.Announced() {
		t.Fatalf("expected Announced before MembersAdmitted, got %s", lc.State())
	}

	if !lc.CanFire(TriggerMembersAdmitted) {
		t.Fatal("CanFire(MembersAdmitted) should be true from Announced")
	}
	if err := lc.Fire(TriggerMembersAdmitted); err != nil {
		t.Fatalf("Fire(MembersAdmitted) from Announced failed: %v", err)
	}
	if lc.State() != enums.CapsuleStatusEnum.Running() {
		t.Errorf("expected Running after MembersAdmitted, got %s", lc.State())
	}
}

// TestLifecycle_MembersAdmittedTrigger_FromRunning verifies the FSM rejects
// MembersAdmitted when the lifecycle is already Running.
func TestLifecycle_MembersAdmittedTrigger_FromRunning(t *testing.T) {
	id := NewCapsuleID()
	lc := NewLifecycle(id, WithLifecycleInitialState(enums.CapsuleStatusEnum.Running()))

	if lc.CanFire(TriggerMembersAdmitted) {
		t.Error("CanFire(MembersAdmitted) should be false from Running")
	}
	if err := lc.Fire(TriggerMembersAdmitted); err == nil {
		t.Error("expected error firing MembersAdmitted from Running")
	}
	if lc.State() != enums.CapsuleStatusEnum.Running() {
		t.Errorf("state should remain Running, got %s", lc.State())
	}
}

// TestLifecycle_MembersAdmittedTrigger_FromCreated verifies the FSM rejects
// MembersAdmitted from Created — the trigger is only valid from Announced.
func TestLifecycle_MembersAdmittedTrigger_FromCreated(t *testing.T) {
	id := NewCapsuleID()
	lc := NewLifecycle(id)

	if lc.State() != enums.CapsuleStatusEnum.Created() {
		t.Fatalf("expected initial state Created, got %s", lc.State())
	}
	if lc.CanFire(TriggerMembersAdmitted) {
		t.Error("CanFire(MembersAdmitted) should be false from Created")
	}
	if err := lc.Fire(TriggerMembersAdmitted); err == nil {
		t.Error("expected error firing MembersAdmitted from Created")
	}
	if lc.State() != enums.CapsuleStatusEnum.Created() {
		t.Errorf("state should remain Created, got %s", lc.State())
	}
}

// --- Manager + Lifecycle integration ---

func TestManagerLifecycleTransitions(t *testing.T) {
	var events []ManagerEvent
	mgr := NewManager(
		WithManagerEventHandler(func(e ManagerEvent) {
			events = append(events, e)
		}),
	)

	c, err := mgr.Create(context.Background(), "test/dc1/cluster", CapsuleSpec{
		Name: "test", Image: "img", Orbit: "api",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Create auto-announces so the post-Create status is Announced, not
	// Created. The Created → Announced transition has already fired
	// before Create returns.
	status, err := mgr.Status(c.ID)
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if status != enums.CapsuleStatusEnum.Announced() {
		t.Errorf("expected Announced (auto-announce), got %s", status)
	}

	// Full happy path
	steps := []func(CapsuleID) error{
		mgr.StartElection,
		mgr.WinElection,
		mgr.StartExecution,
		mgr.MarkRunning,
		mgr.StopCapsule,
		mgr.MarkStopped,
	}
	for i, step := range steps {
		if err := step(c.ID); err != nil {
			t.Fatalf("step %d failed: %v", i, err)
		}
	}
	if s, _ := mgr.Status(c.ID); s != enums.CapsuleStatusEnum.Stopped() {
		t.Errorf("expected Stopped, got %s", s)
	}

	// Verify the store's persisted status also reflects the final state
	stored := mgr.Get(c.ID)
	if stored.Status != enums.CapsuleStatusEnum.Stopped() {
		t.Errorf("store status mismatch: %s", stored.Status)
	}

	// Verify events were emitted for status transitions
	seenRunning := false
	seenStopped := false
	for _, e := range events {
		if e.Type == EventCapsuleRunning {
			seenRunning = true
		}
		if e.Type == EventCapsuleStopped {
			seenStopped = true
		}
	}
	if !seenRunning {
		t.Error("expected EventCapsuleRunning to be emitted")
	}
	if !seenStopped {
		t.Error("expected EventCapsuleStopped to be emitted")
	}
}

func TestManagerInvalidTransition(t *testing.T) {
	mgr := NewManager()
	c, err := mgr.Create(context.Background(), "test/dc1/cluster", CapsuleSpec{
		Name: "test", Image: "img", Orbit: "api",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Can't start execution from Created
	if err := mgr.StartExecution(c.ID); err == nil {
		t.Error("expected error for invalid transition")
	}
}

func TestManagerSyncStatusFromMesh(t *testing.T) {
	mgr := NewManager()

	// Receive a capsule from the mesh (simulating another node announcing it)
	received := &Capsule{
		ID:        NewCapsuleID(),
		ClusterID: "test/dc1/cluster",
		Spec:      CapsuleSpec{Name: "remote", Image: "img", Orbit: "api"},
		Status:    enums.CapsuleStatusEnum.Announced(),
	}
	DefaultSpec(&received.Spec)

	if err := mgr.Receive(received); err != nil {
		t.Fatalf("Receive failed: %v", err)
	}

	// SyncStatus should jump directly to Running even though it's not a valid local transition
	if err := mgr.SyncStatus(received.ID, enums.CapsuleStatusEnum.Running()); err != nil {
		t.Fatalf("SyncStatus failed: %v", err)
	}

	if s, _ := mgr.Status(received.ID); s != enums.CapsuleStatusEnum.Running() {
		t.Errorf("expected Running after sync, got %s", s)
	}

	// Local transitions should now work from the synced state
	if err := mgr.StopCapsule(received.ID); err != nil {
		t.Errorf("StopCapsule after sync failed: %v", err)
	}
}

func TestManagerLifecycleCleanedUpOnDelete(t *testing.T) {
	mgr := NewManager()
	c, _ := mgr.Create(context.Background(), "test/dc1/cluster", CapsuleSpec{
		Name: "test", Image: "img", Orbit: "api",
	})

	if mgr.getLifecycle(c.ID) == nil {
		t.Fatal("lifecycle should exist after Create")
	}

	mgr.Delete(context.Background(), c.ID)

	if mgr.getLifecycle(c.ID) != nil {
		t.Error("lifecycle should be removed after Delete")
	}
}
