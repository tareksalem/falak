package node

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/enums"
	"github.com/tareksalem/falak/node/internal/events"
)

// newGroupHandler builds a CapsuleHandler wired to a fresh in-memory
// capsule.Manager and a fresh event bus. The handler is NOT started; the
// group/orphan tests drive their bodies directly (onMemberRunning,
// scheduleOrphanReap) so timing is deterministic.
//
// orphanGrace is configurable so reap tests can use a short window.
func newGroupHandler(t *testing.T, orphanGrace time.Duration) (*CapsuleHandler, events.Bus) {
	t.Helper()
	bus := events.NewBus()
	opts := []CapsuleHandlerOption{
		WithCapsuleHandlerEventBus(bus),
		WithCapsuleHandlerNodeID("local-node"),
	}
	if orphanGrace > 0 {
		opts = append(opts, WithCapsuleHandlerOrphanGrace(orphanGrace))
	}
	h := NewCapsuleHandler(capsule.NewManager(), opts...)
	return h, bus
}

// startHandler starts the handler so the subscriber goroutines and the
// reaper timers run. The returned cleanup function stops the handler.
func startHandler(t *testing.T, h *CapsuleHandler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.Start(ctx)
	return func() {
		cancel()
		h.Stop()
	}
}

// createTestGroup creates a group + members via the manager and returns
// the materialized group and member slice. Useful for setup steps.
func createTestGroup(t *testing.T, h *CapsuleHandler, cluster, name string, memberNames ...string) (*capsule.Capsule, []*capsule.Capsule) {
	t.Helper()
	members := make([]capsule.MemberSpec, 0, len(memberNames))
	for _, mn := range memberNames {
		spec := capsule.CapsuleSpec{
			Name:  mn,
			Image: mn + ":latest",
			Orbit: "api",
		}
		capsule.DefaultSpec(&spec)
		members = append(members, capsule.MemberSpec{Name: mn, Spec: spec})
	}
	groupSpec := capsule.GroupSpec{
		Colocation:    capsule.ColocationModeEnum.SameOrbit(),
		Members:       members,
		CascadeDelete: true,
	}
	group, created, err := h.manager.CreateGroup(context.Background(), cluster, name, groupSpec, nil)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return group, created
}

// markMemberRunning advances a member capsule through the FSM transitions
// required to reach Running. Standalone capsules normally walk
// Announced -> Electing -> Assigned -> Executing -> Running; for the
// purposes of the group-running coordination tests we drive that walk
// directly.
func markMemberRunning(t *testing.T, mgr *capsule.Manager, id capsule.CapsuleID) {
	t.Helper()
	if err := mgr.StartElection(id); err != nil {
		t.Fatalf("StartElection(%s): %v", id, err)
	}
	if err := mgr.WinElection(id); err != nil {
		t.Fatalf("WinElection(%s): %v", id, err)
	}
	if err := mgr.StartExecution(id); err != nil {
		t.Fatalf("StartExecution(%s): %v", id, err)
	}
	if err := mgr.MarkRunning(id); err != nil {
		t.Fatalf("MarkRunning(%s): %v", id, err)
	}
}

// --- handleMemberRunning -----------------------------------------------

// TestCapsuleHandler_MarkGroupRunning_AllMembersRunning verifies that
// when every member of a group reaches Running, the handler fires
// MarkGroupRunning and the group lifecycle advances.
func TestCapsuleHandler_MarkGroupRunning_AllMembersRunning(t *testing.T) {
	h, _ := newGroupHandler(t, 0)

	const cluster = "test/dc1/c"
	group, members := createTestGroup(t, h, cluster, "demo", "a", "b")
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}

	// Drive both members to Running and call onMemberRunning per event.
	for _, m := range members {
		markMemberRunning(t, h.manager, m.ID)
		h.onMemberRunning(m.ID)
	}

	// Group should be Running now.
	status, err := h.manager.Status(group.ID)
	if err != nil {
		t.Fatalf("Status(group): %v", err)
	}
	if status != enums.CapsuleStatusEnum.Running() {
		t.Errorf("group status = %q, want Running", status)
	}
}

// TestCapsuleHandler_MarkGroupRunning_PartialRunning verifies that when
// only one of N members has reached Running, the group stays at
// Announced — MarkGroupRunning must NOT fire.
func TestCapsuleHandler_MarkGroupRunning_PartialRunning(t *testing.T) {
	h, _ := newGroupHandler(t, 0)

	const cluster = "test/dc1/c"
	group, members := createTestGroup(t, h, cluster, "partial", "a", "b")

	// Only the first member transitions to Running.
	markMemberRunning(t, h.manager, members[0].ID)
	h.onMemberRunning(members[0].ID)

	status, err := h.manager.Status(group.ID)
	if err != nil {
		t.Fatalf("Status(group): %v", err)
	}
	if status == enums.CapsuleStatusEnum.Running() {
		t.Errorf("group prematurely Running with only 1/2 members at Running")
	}
	if status != enums.CapsuleStatusEnum.Announced() {
		t.Errorf("group status = %q, want Announced", status)
	}
}

// --- handleOrphanReaper ------------------------------------------------

// TestCapsuleHandler_OrphanReaper_DeletesAfterGrace verifies a member
// capsule whose GroupID references a non-existent group is deleted
// after the grace window elapses.
func TestCapsuleHandler_OrphanReaper_DeletesAfterGrace(t *testing.T) {
	h, _ := newGroupHandler(t, 100*time.Millisecond)
	cleanup := startHandler(t, h)
	defer cleanup()

	const cluster = "test/dc1/c"
	// Insert a member capsule with a GroupID that points nowhere.
	orphanGroupID := capsule.NewCapsuleID()
	memberSpec := capsule.CapsuleSpec{
		Name:        "orphan",
		Image:       "orphan:latest",
		Orbit:       "api",
		GroupID:     orphanGroupID,
		GroupMember: true,
	}
	capsule.DefaultSpec(&memberSpec)
	member, err := h.manager.Create(context.Background(), cluster, memberSpec)
	if err != nil {
		t.Fatalf("Create orphan member: %v", err)
	}

	// Drive the reaper logic.
	h.onCapsuleReceivedForReaper(member.ID)

	// Wait long enough for the grace window plus the delete RPC.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.manager.Get(member.ID) == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("orphan member %s still present after grace window", member.ID)
}

// TestCapsuleHandler_OrphanReaper_CancelsOnGroupArrival verifies that
// when the parent group arrives during the grace window, the pending
// reap is cancelled and the member is NOT deleted.
func TestCapsuleHandler_OrphanReaper_CancelsOnGroupArrival(t *testing.T) {
	h, _ := newGroupHandler(t, 500*time.Millisecond)
	cleanup := startHandler(t, h)
	defer cleanup()

	const cluster = "test/dc1/c"

	// First create a real group so we can take its ID. We then DELETE
	// the group from the store directly to simulate the "member
	// announcement arrived before group" ordering — this leaves the
	// member dangling at the moment onCapsuleReceivedForReaper runs.
	groupSpec := capsule.GroupSpec{
		Colocation: capsule.ColocationModeEnum.SameOrbit(),
		Members: []capsule.MemberSpec{
			{Name: "m1", Spec: func() capsule.CapsuleSpec {
				s := capsule.CapsuleSpec{Name: "m1", Image: "m1:v1", Orbit: "api"}
				capsule.DefaultSpec(&s)
				return s
			}()},
		},
		CascadeDelete: true,
	}
	group, members, err := h.manager.CreateGroup(context.Background(), cluster, "g1", groupSpec, nil)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	member := members[0]

	// Simulate "group not yet known": remove the group entry so the
	// reaper schedules a delete for the member.
	if err := h.manager.Delete(context.Background(), group.ID); err != nil {
		// Delete cascade will reap the member; re-insert it.
		_ = err
	}
	// Recreate just the member capsule with the now-dangling GroupID.
	memberSpec := member.Spec
	memberSpec.GroupID = group.ID
	memberSpec.GroupMember = true
	recreated, err := h.manager.Create(context.Background(), cluster, memberSpec)
	if err != nil {
		t.Fatalf("recreate member: %v", err)
	}

	// Arm the reaper.
	h.onCapsuleReceivedForReaper(recreated.ID)

	// Confirm a pending reap is registered.
	h.mu.RLock()
	_, pending := h.pendingReaps[recreated.ID]
	h.mu.RUnlock()
	if !pending {
		t.Fatal("expected pending reap for orphan member")
	}

	// Simulate the parent group arriving: recreate the group capsule
	// in the store (use the manager's CreateGroup again with a new
	// member won't reuse the same group ID, so insert directly via
	// store) — for this test it's cleaner to use Receive with a synthetic
	// group capsule.
	groupCapsule := &capsule.Capsule{
		ID:        group.ID,
		ClusterID: cluster,
		Spec: capsule.CapsuleSpec{
			Name:  "g1",
			Kind:  capsule.CapsuleKindEnum.Group(),
			Group: &capsule.GroupSpec{Colocation: capsule.ColocationModeEnum.SameOrbit(), MemberIDs: []capsule.CapsuleID{recreated.ID}, CascadeDelete: true},
		},
		Status:  enums.CapsuleStatusEnum.Announced(),
		Version: "1",
	}
	if err := h.manager.Receive(groupCapsule); err != nil {
		t.Fatalf("Receive group: %v", err)
	}
	// Drive the reaper to observe the group's arrival.
	h.onCapsuleReceivedForReaper(group.ID)

	// The pending reap should now be cancelled.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		h.mu.RLock()
		_, stillPending := h.pendingReaps[recreated.ID]
		h.mu.RUnlock()
		if !stillPending {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Wait past the original grace window — the member must remain.
	time.Sleep(700 * time.Millisecond)
	if h.manager.Get(recreated.ID) == nil {
		t.Fatal("member was reaped despite parent group arrival")
	}
}

// TestCapsuleHandler_OrphanReaper_StopCancelsPending verifies that
// calling Stop cancels every pending reap so no Delete fires after
// shutdown.
func TestCapsuleHandler_OrphanReaper_StopCancelsPending(t *testing.T) {
	h, _ := newGroupHandler(t, 2*time.Second) // long grace so we hit Stop first

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Start(ctx)

	const cluster = "test/dc1/c"
	orphanGroupID := capsule.NewCapsuleID()
	memberSpec := capsule.CapsuleSpec{
		Name:        "orphan",
		Image:       "orphan:latest",
		Orbit:       "api",
		GroupID:     orphanGroupID,
		GroupMember: true,
	}
	capsule.DefaultSpec(&memberSpec)
	member, err := h.manager.Create(context.Background(), cluster, memberSpec)
	if err != nil {
		t.Fatalf("Create orphan: %v", err)
	}

	h.onCapsuleReceivedForReaper(member.ID)

	// Confirm a pending reap exists.
	h.mu.RLock()
	_, pending := h.pendingReaps[member.ID]
	h.mu.RUnlock()
	if !pending {
		t.Fatal("expected pending reap before Stop")
	}

	// Stop must wake the timer goroutine and clear the map.
	h.Stop()

	h.mu.RLock()
	remaining := len(h.pendingReaps)
	h.mu.RUnlock()
	if remaining != 0 {
		t.Errorf("pendingReaps after Stop = %d, want 0", remaining)
	}

	// Member must still exist (no Delete fired).
	if h.manager.Get(member.ID) == nil {
		t.Error("member was reaped after Stop; reap should have been cancelled")
	}
}

// --- TypeCapsuleGroupReleased ------------------------------------------

// TestCapsuleHandler_GroupReleased_ReannouncesMembers verifies that a
// non-cascade group delete causes each detached member to be re-announced
// on its orbit AND publishes a CapsuleGroupReleased event with the
// correct previous group ID.
func TestCapsuleHandler_GroupReleased_ReannouncesMembers(t *testing.T) {
	h, bus := newGroupHandler(t, 0)

	const cluster = "test/dc1/c"

	// Use an observing recorder hooked through a fake announcer
	// equivalent: count the times announceCapsule's path triggers a
	// CapsuleUpdated event on the node bus. Without a real announcer
	// the announce itself is a no-op (no announcer registered for the
	// cluster), but the node-bus event bridge fires regardless and is
	// what downstream consumers (and tests) actually observe.
	updatedCh := bus.Subscribe(events.TypeCapsuleUpdated)
	releasedCh := bus.Subscribe(events.TypeCapsuleGroupReleased)

	// Create a non-cascade group with 2 members.
	memberSpec := func(name string) capsule.CapsuleSpec {
		s := capsule.CapsuleSpec{Name: name, Image: name + ":v1", Orbit: "api"}
		capsule.DefaultSpec(&s)
		return s
	}
	groupSpec := capsule.GroupSpec{
		Colocation: capsule.ColocationModeEnum.SameOrbit(),
		Members: []capsule.MemberSpec{
			{Name: "a", Spec: memberSpec("a")},
			{Name: "b", Spec: memberSpec("b")},
		},
		CascadeDelete: false,
	}
	group, members, err := h.manager.CreateGroup(context.Background(), cluster, "non-cascade", groupSpec, nil)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	memberIDs := map[string]bool{}
	for _, m := range members {
		memberIDs[m.ID.String()] = true
	}

	// Delete the group non-cascade.
	if err := h.manager.Delete(context.Background(), group.ID); err != nil {
		t.Fatalf("Delete(group): %v", err)
	}

	// Collect events with a deadline.
	var (
		updatedForMembers   int32
		releasedForMembers  int32
		releasedWithPrevID  int32
	)
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&updatedForMembers) < 2 || atomic.LoadInt32(&releasedForMembers) < 2 {
		select {
		case ev := <-updatedCh:
			u, ok := ev.(events.CapsuleUpdated)
			if !ok {
				continue
			}
			if memberIDs[u.CapsuleID] {
				atomic.AddInt32(&updatedForMembers, 1)
			}
		case ev := <-releasedCh:
			r, ok := ev.(events.CapsuleGroupReleased)
			if !ok {
				continue
			}
			if memberIDs[r.CapsuleID] {
				atomic.AddInt32(&releasedForMembers, 1)
				if r.PreviousGroupID == group.ID.String() {
					atomic.AddInt32(&releasedWithPrevID, 1)
				}
			}
		case <-deadline:
			t.Fatalf("timeout: updated=%d released=%d (want 2 each)",
				atomic.LoadInt32(&updatedForMembers), atomic.LoadInt32(&releasedForMembers))
		}
	}

	if got := atomic.LoadInt32(&releasedWithPrevID); got != 2 {
		t.Errorf("CapsuleGroupReleased with correct PreviousGroupID = %d, want 2", got)
	}

	// Each member's GroupID has been cleared on the manager side.
	for _, m := range members {
		c := h.manager.Get(m.ID)
		if c == nil {
			t.Errorf("member %s missing after non-cascade delete", m.ID)
			continue
		}
		if c.Spec.GroupID != "" {
			t.Errorf("member %s GroupID = %q, want empty", m.ID, c.Spec.GroupID)
		}
		if c.Spec.GroupMember {
			t.Errorf("member %s GroupMember = true, want false", m.ID)
		}
	}
}
