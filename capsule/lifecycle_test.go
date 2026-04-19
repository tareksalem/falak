package capsule

import (
	"context"
	"testing"
)

func TestLifecycleHappyPath(t *testing.T) {
	id := NewCapsuleID()
	var transitions []LifecycleEvent
	lc := NewLifecycle(id, WithLifecycleHandler(func(e LifecycleEvent) {
		transitions = append(transitions, e)
	}))

	if lc.State() != CapsuleStatusEnum.Created() {
		t.Fatalf("initial state should be created, got %s", lc.State())
	}

	steps := []struct {
		trigger  string
		expected CapsuleStatus
	}{
		{TriggerAnnounce, CapsuleStatusEnum.Announced()},
		{TriggerElectionStarted, CapsuleStatusEnum.Electing()},
		{TriggerElectionWon, CapsuleStatusEnum.Assigned()},
		{TriggerExecutionStart, CapsuleStatusEnum.Executing()},
		{TriggerContainerReady, CapsuleStatusEnum.Running()},
		{TriggerStopRequested, CapsuleStatusEnum.Stopping()},
		{TriggerContainerStopped, CapsuleStatusEnum.Stopped()},
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

	if lc.State() != CapsuleStatusEnum.Running() {
		t.Fatal("should be running")
	}

	if err := lc.Fire(TriggerNodeFailed); err != nil {
		t.Fatalf("Fire(NodeFailed) failed: %v", err)
	}

	if lc.State() != CapsuleStatusEnum.Announced() {
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

	if lc.State() != CapsuleStatusEnum.Announced() {
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

	if lc.State() != CapsuleStatusEnum.Stopped() {
		t.Fatal("should be stopped")
	}

	if err := lc.Fire(TriggerAnnounce); err != nil {
		t.Fatalf("restart failed: %v", err)
	}
	if lc.State() != CapsuleStatusEnum.Announced() {
		t.Errorf("should be announced after restart, got %s", lc.State())
	}
}

func TestLifecycleInitialState(t *testing.T) {
	// A lifecycle restored from persistence should start at the given state.
	id := NewCapsuleID()
	lc := NewLifecycle(id, WithLifecycleInitialState(CapsuleStatusEnum.Running()))

	if lc.State() != CapsuleStatusEnum.Running() {
		t.Errorf("expected Running, got %s", lc.State())
	}
	// From Running we can fire StopRequested
	if err := lc.Fire(TriggerStopRequested); err != nil {
		t.Errorf("Fire(StopRequested) failed: %v", err)
	}
	if lc.State() != CapsuleStatusEnum.Stopping() {
		t.Errorf("expected Stopping, got %s", lc.State())
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
	if status != CapsuleStatusEnum.Announced() {
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
	if s, _ := mgr.Status(c.ID); s != CapsuleStatusEnum.Stopped() {
		t.Errorf("expected Stopped, got %s", s)
	}

	// Verify the store's persisted status also reflects the final state
	stored := mgr.Get(c.ID)
	if stored.Status != CapsuleStatusEnum.Stopped() {
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
		Status:    CapsuleStatusEnum.Announced(),
	}
	DefaultSpec(&received.Spec)

	if err := mgr.Receive(received); err != nil {
		t.Fatalf("Receive failed: %v", err)
	}

	// SyncStatus should jump directly to Running even though it's not a valid local transition
	if err := mgr.SyncStatus(received.ID, CapsuleStatusEnum.Running()); err != nil {
		t.Fatalf("SyncStatus failed: %v", err)
	}

	if s, _ := mgr.Status(received.ID); s != CapsuleStatusEnum.Running() {
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
