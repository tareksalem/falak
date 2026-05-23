package capsule

import (
	"context"
	"strings"
	"testing"

	enums "github.com/tareksalem/falak/capsule/enums"
)

// installGroupCapsule materialises a group-kind Capsule directly into the
// manager's store and registers a fresh lifecycle at Announced. Manager.Create
// deliberately refuses Kind=Group (that path is owned by CreateGroup, task
// 10.6); the kind-gating tests need a group capsule on disk without going
// through that path, so they install it via Receive — the same code path the
// mesh receive handler uses.
func installGroupCapsule(t *testing.T, mgr *Manager, name string) *Capsule {
	t.Helper()
	c := &Capsule{
		ID:        NewCapsuleID(),
		ClusterID: "test/dc1/cluster",
		Spec: CapsuleSpec{
			Name:  name,
			Orbit: "groups",
			Kind:  CapsuleKindEnum.Group(),
			Group: &GroupSpec{
				Colocation: ColocationModeEnum.SameOrbit(),
				Members: []MemberSpec{{
					Name: "member-a",
					Spec: CapsuleSpec{Name: "member-a", Image: "img", Orbit: "groups"},
				}},
				CascadeDelete: true,
			},
		},
		Status: enums.CapsuleStatusEnum.Announced(),
	}
	if err := mgr.Receive(c); err != nil {
		t.Fatalf("Receive(group) failed: %v", err)
	}
	return c
}

// TestManager_Fire_GroupRejectsElectionTriggers verifies that every
// election/execution/scaling trigger is rejected on a group capsule and the
// error message names the offending trigger.
func TestManager_Fire_GroupRejectsElectionTriggers(t *testing.T) {
	mgr := NewManager()
	c := installGroupCapsule(t, mgr, "rejects-elections")

	rejected := []string{
		TriggerElectionStarted,
		TriggerElectionWon,
		TriggerElectionTimeout,
		TriggerExecutionStart,
		TriggerContainerReady,
		TriggerNodeFailed,
		TriggerScaleUpNeeded,
		TriggerScaleDownNeeded,
	}

	for _, trig := range rejected {
		t.Run(trig, func(t *testing.T) {
			err := mgr.Fire(c.ID, trig)
			if err == nil {
				t.Fatalf("expected error firing %q on group capsule, got nil", trig)
			}
			if !strings.Contains(err.Error(), trig) {
				t.Errorf("error should mention trigger %q: got %q", trig, err.Error())
			}
			if !strings.Contains(err.Error(), "group capsules") {
				t.Errorf("error should mention group capsules: got %q", err.Error())
			}
		})
	}

	// Status must remain Announced — no transition fired.
	got, err := mgr.Status(c.ID)
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if got != enums.CapsuleStatusEnum.Announced() {
		t.Errorf("status should remain Announced, got %s", got)
	}
}

// TestManager_Fire_NonGroupRejectsMembersAdmitted verifies a regular capsule
// rejects MembersAdmitted with a clear error.
func TestManager_Fire_NonGroupRejectsMembersAdmitted(t *testing.T) {
	mgr := NewManager()
	c, err := mgr.Create(context.Background(), "test/dc1/cluster", CapsuleSpec{
		Name: "regular", Image: "img", Orbit: "api",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	err = mgr.Fire(c.ID, TriggerMembersAdmitted)
	if err == nil {
		t.Fatal("expected error firing MembersAdmitted on non-group capsule")
	}
	if !strings.Contains(err.Error(), TriggerMembersAdmitted) {
		t.Errorf("error should mention trigger name: got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "group capsules") {
		t.Errorf("error should mention group capsules: got %q", err.Error())
	}

	// Also verify MarkGroupRunning rejects the same way.
	if err := mgr.MarkGroupRunning(c.ID); err == nil {
		t.Error("MarkGroupRunning on non-group capsule should error")
	}
}

// TestManager_MarkGroupRunning verifies the group convenience helper:
// Announce → MarkGroupRunning lands the group in Running.
func TestManager_MarkGroupRunning(t *testing.T) {
	mgr := NewManager()
	c := installGroupCapsule(t, mgr, "mark-running")

	// installGroupCapsule places the lifecycle at Announced.
	if got, _ := mgr.Status(c.ID); got != enums.CapsuleStatusEnum.Announced() {
		t.Fatalf("setup: expected Announced, got %s", got)
	}

	if err := mgr.MarkGroupRunning(c.ID); err != nil {
		t.Fatalf("MarkGroupRunning failed: %v", err)
	}
	if got, _ := mgr.Status(c.ID); got != enums.CapsuleStatusEnum.Running() {
		t.Errorf("expected Running after MarkGroupRunning, got %s", got)
	}

	// Persisted status must match.
	if stored := mgr.Get(c.ID); stored.Status != enums.CapsuleStatusEnum.Running() {
		t.Errorf("store status mismatch: got %s, want Running", stored.Status)
	}
}

// TestManager_Fire_GroupAllowsAnnounceAndStop walks a group through the
// triggers it IS allowed to fire: Announce, MembersAdmitted, StopRequested,
// ContainerStopped. None of these are on the group-rejection list.
//
// Note: installGroupCapsule places the group at Announced via Receive (which
// mirrors the mesh path). To exercise TriggerAnnounce we restart from
// Stopped at the end and verify the loop closes.
func TestManager_Fire_GroupAllowsAnnounceAndStop(t *testing.T) {
	mgr := NewManager()
	c := installGroupCapsule(t, mgr, "happy-group")

	// Announced → Running via MembersAdmitted.
	if err := mgr.MarkGroupRunning(c.ID); err != nil {
		t.Fatalf("MarkGroupRunning failed: %v", err)
	}
	if got, _ := mgr.Status(c.ID); got != enums.CapsuleStatusEnum.Running() {
		t.Fatalf("expected Running, got %s", got)
	}

	// Running → Stopping via StopRequested.
	if err := mgr.StopCapsule(c.ID); err != nil {
		t.Fatalf("StopCapsule on group failed: %v", err)
	}
	if got, _ := mgr.Status(c.ID); got != enums.CapsuleStatusEnum.Stopping() {
		t.Fatalf("expected Stopping, got %s", got)
	}

	// Stopping → Stopped via ContainerStopped.
	if err := mgr.MarkStopped(c.ID); err != nil {
		t.Fatalf("MarkStopped on group failed: %v", err)
	}
	if got, _ := mgr.Status(c.ID); got != enums.CapsuleStatusEnum.Stopped() {
		t.Fatalf("expected Stopped, got %s", got)
	}

	// Stopped → Announced via Announce closes the loop and proves Announce
	// is on the allow list for groups.
	if err := mgr.Announce(c.ID); err != nil {
		t.Fatalf("Announce on group failed: %v", err)
	}
	if got, _ := mgr.Status(c.ID); got != enums.CapsuleStatusEnum.Announced() {
		t.Errorf("expected Announced after restart, got %s", got)
	}
}

// TestManager_Create_RejectsGroupKind verifies the guard added to Create
// pointing callers at CreateGroup.
func TestManager_Create_RejectsGroupKind(t *testing.T) {
	mgr := NewManager()
	_, err := mgr.Create(context.Background(), "test/dc1/cluster", CapsuleSpec{
		Name:  "should-not-create",
		Orbit: "groups",
		Kind:  CapsuleKindEnum.Group(),
		Group: &GroupSpec{
			Colocation: ColocationModeEnum.SameOrbit(),
			Members: []MemberSpec{{
				Name: "m",
				Spec: CapsuleSpec{Name: "m", Image: "img", Orbit: "groups"},
			}},
		},
	})
	if err == nil {
		t.Fatal("expected error from Create on Kind=Group")
	}
	if !strings.Contains(err.Error(), "CreateGroup") {
		t.Errorf("error should point at CreateGroup, got %q", err.Error())
	}
}
