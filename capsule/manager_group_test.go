package capsule

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	enums "github.com/tareksalem/falak/capsule/enums"
)

// memberSpecForTest returns a CapsuleSpec usable as a group member's embedded
// spec — name + image + orbit are populated, defaults are applied so the
// shape mirrors what CreateGroup will hand to ValidateGroupSpec.
func memberSpecForTest(name string) CapsuleSpec {
	s := CapsuleSpec{
		Name:  name,
		Image: name + ":latest",
		Orbit: "api",
	}
	DefaultSpec(&s)
	return s
}

// captureEvents returns an EventHandler that records every ManagerEvent into
// the returned slice (under a mutex), and the slice itself. Tests use this
// to assert that CreateGroup emits CapsuleCreated/CapsuleAnnounced for both
// the group and every member without leaking the recorder mutex.
func captureEvents() (EventHandler, *[]ManagerEvent, *sync.Mutex) {
	var (
		mu     sync.Mutex
		events []ManagerEvent
	)
	h := func(ev ManagerEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}
	return h, &events, &mu
}

// countEventsByType returns the number of events with the given type.
func countEventsByType(mu *sync.Mutex, events *[]ManagerEvent, eventType string) int {
	mu.Lock()
	defer mu.Unlock()
	count := 0
	for _, ev := range *events {
		if ev.Type == eventType {
			count++
		}
	}
	return count
}

// TestManager_CreateGroup_HappyPath (10.T4) verifies CreateGroup materializes
// every member, sets GroupID/GroupMember/Kind correctly, populates
// Spec.Group.MemberIDs in spec order, and auto-announces the group plus
// every member. GetGroup is exercised after creation.
func TestManager_CreateGroup_HappyPath(t *testing.T) {
	t.Parallel()

	handler, events, mu := captureEvents()
	mgr := NewManager(WithManagerEventHandler(handler))

	groupSpec := GroupSpec{
		Colocation: ColocationModeEnum.SameOrbit(),
		Members: []MemberSpec{
			{Name: "api", Spec: memberSpecForTest("api")},
			{Name: "worker", Spec: memberSpecForTest("worker")},
			{Name: "cache", Spec: memberSpecForTest("cache")},
		},
		CascadeDelete: true,
	}
	baseLabels := Labels{"app": "demo"}

	group, members, err := mgr.CreateGroup(context.Background(), "test/dc1/cluster", "demo-group", groupSpec, baseLabels)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	if group == nil {
		t.Fatal("CreateGroup returned nil group")
	}
	if group.Spec.Kind != CapsuleKindEnum.Group() {
		t.Errorf("group Kind = %q, want %q", group.Spec.Kind, CapsuleKindEnum.Group())
	}
	if group.Spec.Name != "demo-group" {
		t.Errorf("group Name = %q, want %q", group.Spec.Name, "demo-group")
	}
	if group.Spec.Group == nil {
		t.Fatal("group.Spec.Group is nil")
	}
	if got := len(group.Spec.Group.MemberIDs); got != 3 {
		t.Fatalf("MemberIDs length = %d, want 3", got)
	}

	// MemberIDs must match the order of Members and the returned slice.
	if len(members) != 3 {
		t.Fatalf("returned members length = %d, want 3", len(members))
	}
	for i, mc := range members {
		if mc.Spec.Kind != CapsuleKindEnum.Capsule() {
			t.Errorf("member[%d] Kind = %q, want %q", i, mc.Spec.Kind, CapsuleKindEnum.Capsule())
		}
		if mc.Spec.GroupID != group.ID {
			t.Errorf("member[%d] GroupID = %q, want %q", i, mc.Spec.GroupID, group.ID)
		}
		if !mc.Spec.GroupMember {
			t.Errorf("member[%d] GroupMember = false, want true", i)
		}
		if mc.Spec.Group != nil {
			t.Errorf("member[%d] Spec.Group should be nil, got %+v", i, mc.Spec.Group)
		}
		if group.Spec.Group.MemberIDs[i] != mc.ID {
			t.Errorf("MemberIDs[%d] = %q, want %q", i, group.Spec.Group.MemberIDs[i], mc.ID)
		}
	}

	// Group lifecycle must be Announced after auto-announce.
	if got, _ := mgr.Status(group.ID); got != enums.CapsuleStatusEnum.Announced() {
		t.Errorf("group status = %q, want Announced", got)
	}

	// Each member auto-announced via Manager.Create.
	for _, mc := range members {
		if got, _ := mgr.Status(mc.ID); got != enums.CapsuleStatusEnum.Announced() {
			t.Errorf("member %q status = %q, want Announced", mc.Spec.Name, got)
		}
	}

	// Events: 1 created + 1 announced for the group, plus 1 each for every
	// member — 4 created events total, 4 announced.
	if got := countEventsByType(mu, events, EventCapsuleCreated); got != 4 {
		t.Errorf("EventCapsuleCreated count = %d, want 4", got)
	}
	if got := countEventsByType(mu, events, EventCapsuleAnnounced); got != 4 {
		t.Errorf("EventCapsuleAnnounced count = %d, want 4", got)
	}

	// GetGroup returns the group + members sorted by name.
	gotGroup, gotMembers := mgr.GetGroup(group.ID)
	if gotGroup == nil || gotGroup.ID != group.ID {
		t.Fatalf("GetGroup returned wrong group: %+v", gotGroup)
	}
	if len(gotMembers) != 3 {
		t.Fatalf("GetGroup members length = %d, want 3", len(gotMembers))
	}
	wantOrder := []string{"api", "cache", "worker"} // sorted by name
	for i, mc := range gotMembers {
		if mc.Spec.Name != wantOrder[i] {
			t.Errorf("GetGroup members[%d].Name = %q, want %q", i, mc.Spec.Name, wantOrder[i])
		}
	}
}

// TestManager_CreateGroup_CycleRejected verifies that a group whose
// depends_on graph contains a cycle is rejected at validation; no group
// capsule and no member capsules are persisted.
func TestManager_CreateGroup_CycleRejected(t *testing.T) {
	t.Parallel()

	mgr := NewManager()

	groupSpec := GroupSpec{
		Colocation: ColocationModeEnum.SameOrbit(),
		Members: []MemberSpec{
			{Name: "a", Spec: memberSpecForTest("a"), DependsOn: []string{"b"}},
			{Name: "b", Spec: memberSpecForTest("b"), DependsOn: []string{"a"}},
		},
	}

	_, _, err := mgr.CreateGroup(context.Background(), "test/dc1/cluster", "cyclic", groupSpec, nil)
	if err == nil {
		t.Fatal("expected cycle error from CreateGroup")
	}
	if !errors.Is(err, ErrGroupDependsOnCycle) {
		t.Errorf("expected ErrGroupDependsOnCycle, got %v", err)
	}

	if got := mgr.Count(); got != 0 {
		t.Errorf("store count = %d, want 0 after rejected create", got)
	}
	if mgr.GetByName("cyclic") != nil {
		t.Error("cyclic group should not be in store")
	}
	if mgr.GetByName("a") != nil || mgr.GetByName("b") != nil {
		t.Error("no member capsules should be created when group is rejected")
	}
}

// TestManager_CreateGroup_InvalidMemberRejected verifies that a 3-member
// group whose second member fails ValidateSpec (empty Image) is rejected
// at up-front validation — no group, no first member, nothing touched.
func TestManager_CreateGroup_InvalidMemberRejected(t *testing.T) {
	t.Parallel()

	mgr := NewManager()

	bad := memberSpecForTest("middle")
	bad.Image = "" // invalidates ValidateSpec

	groupSpec := GroupSpec{
		Colocation: ColocationModeEnum.SameOrbit(),
		Members: []MemberSpec{
			{Name: "first", Spec: memberSpecForTest("first")},
			{Name: "middle", Spec: bad},
			{Name: "third", Spec: memberSpecForTest("third")},
		},
	}

	_, _, err := mgr.CreateGroup(context.Background(), "test/dc1/cluster", "with-bad-member", groupSpec, nil)
	if err == nil {
		t.Fatal("expected error from CreateGroup with invalid member")
	}
	if !errors.Is(err, ErrGroupMemberSpecInvalid) {
		t.Errorf("expected ErrGroupMemberSpecInvalid, got %v", err)
	}
	if !errors.Is(err, ErrImageRequired) {
		t.Errorf("expected ErrImageRequired (wrapped), got %v", err)
	}

	// Up-front rejection: nothing in the store.
	if got := mgr.Count(); got != 0 {
		t.Errorf("store count = %d, want 0", got)
	}
	if mgr.GetByName("with-bad-member") != nil {
		t.Error("group should not be in store after validation failure")
	}
	if mgr.GetByName("first") != nil {
		t.Error("first member should not be created when group validation fails")
	}
}

// TestManager_CreateGroup_DNSInvalidName verifies that a group name failing
// the DNS-label rule is rejected before any store write happens.
func TestManager_CreateGroup_DNSInvalidName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		groupName string
	}{
		{"empty", ""},
		{"uppercase", "MyGroup"},
		{"underscores", "my_group"},
		{"leading-hyphen", "-leading"},
		{"trailing-hyphen", "trailing-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mgr := NewManager()

			groupSpec := GroupSpec{
				Colocation: ColocationModeEnum.SameOrbit(),
				Members: []MemberSpec{
					{Name: "a", Spec: memberSpecForTest("a")},
				},
			}

			_, _, err := mgr.CreateGroup(context.Background(), "test/dc1/cluster", tt.groupName, groupSpec, nil)
			if err == nil {
				t.Fatalf("expected error for group name %q", tt.groupName)
			}
			if !strings.Contains(err.Error(), "DNS-friendly") {
				t.Errorf("error should mention DNS-friendly, got %q", err.Error())
			}

			if got := mgr.Count(); got != 0 {
				t.Errorf("store count = %d, want 0", got)
			}
		})
	}
}

// TestManager_CreateGroup_LabelMerge verifies group-level baseLabels are
// inherited by members, and that member-level labels win on key collision.
func TestManager_CreateGroup_LabelMerge(t *testing.T) {
	t.Parallel()

	mgr := NewManager()

	memberWithLabel := memberSpecForTest("worker")
	memberWithLabel.Labels = Labels{
		"role":  "worker", // unique to this member
		"tier":  "high",   // collides with baseLabels — member must win
	}

	memberNoLabels := memberSpecForTest("api")
	// Leave api's labels as the default (empty map after DefaultSpec).

	groupSpec := GroupSpec{
		Colocation: ColocationModeEnum.SameOrbit(),
		Members: []MemberSpec{
			{Name: "api", Spec: memberNoLabels},
			{Name: "worker", Spec: memberWithLabel},
		},
	}
	baseLabels := Labels{
		"app":  "demo", // inherited by every member
		"tier": "low",  // overridden on worker, kept on api
	}

	_, members, err := mgr.CreateGroup(context.Background(), "test/dc1/cluster", "merge-test", groupSpec, baseLabels)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	byName := make(map[string]*Capsule, len(members))
	for _, mc := range members {
		byName[mc.Spec.Name] = mc
	}

	// api: inherits baseLabels untouched (no member labels set).
	api := byName["api"]
	if api == nil {
		t.Fatal("api member missing")
	}
	if api.Spec.Labels["app"] != "demo" {
		t.Errorf("api Labels[app] = %q, want demo", api.Spec.Labels["app"])
	}
	if api.Spec.Labels["tier"] != "low" {
		t.Errorf("api Labels[tier] = %q, want low (inherited)", api.Spec.Labels["tier"])
	}

	// worker: inherits base, but member-level tier wins; role added.
	worker := byName["worker"]
	if worker == nil {
		t.Fatal("worker member missing")
	}
	if worker.Spec.Labels["app"] != "demo" {
		t.Errorf("worker Labels[app] = %q, want demo (inherited)", worker.Spec.Labels["app"])
	}
	if worker.Spec.Labels["tier"] != "high" {
		t.Errorf("worker Labels[tier] = %q, want high (member override)", worker.Spec.Labels["tier"])
	}
	if worker.Spec.Labels["role"] != "worker" {
		t.Errorf("worker Labels[role] = %q, want worker", worker.Spec.Labels["role"])
	}
}

// TestManager_CreateGroup_MemberIDsOrder verifies Spec.Group.MemberIDs is
// stored in the same order as the input groupSpec.Members slice — not the
// alphabetical Spec.Name order returned by Store.ListByGroup.
func TestManager_CreateGroup_MemberIDsOrder(t *testing.T) {
	t.Parallel()

	mgr := NewManager()

	// Names chosen so creation order != alphabetical order.
	groupSpec := GroupSpec{
		Colocation: ColocationModeEnum.SameOrbit(),
		Members: []MemberSpec{
			{Name: "zebra", Spec: memberSpecForTest("zebra")},
			{Name: "alpha", Spec: memberSpecForTest("alpha")},
			{Name: "mike", Spec: memberSpecForTest("mike")},
		},
	}

	group, members, err := mgr.CreateGroup(context.Background(), "test/dc1/cluster", "order-test", groupSpec, nil)
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	wantNames := []string{"zebra", "alpha", "mike"}
	for i, want := range wantNames {
		if members[i].Spec.Name != want {
			t.Errorf("members[%d].Name = %q, want %q", i, members[i].Spec.Name, want)
		}
		if group.Spec.Group.MemberIDs[i] != members[i].ID {
			t.Errorf("MemberIDs[%d] = %q, want %q", i, group.Spec.Group.MemberIDs[i], members[i].ID)
		}
	}
}

// TestManager_GetGroup_NotGroupReturnsNil verifies GetGroup refuses to
// return a non-group capsule even when the ID matches.
func TestManager_GetGroup_NotGroupReturnsNil(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	c, err := mgr.Create(context.Background(), "test/dc1/cluster", CapsuleSpec{
		Name: "regular", Image: "img", Orbit: "api",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	g, members := mgr.GetGroup(c.ID)
	if g != nil {
		t.Errorf("GetGroup on a non-group capsule = %+v, want nil", g)
	}
	if members != nil {
		t.Errorf("GetGroup members on non-group = %+v, want nil", members)
	}
}

// TestManager_GetGroup_Missing verifies GetGroup returns nil/nil when the
// capsule does not exist.
func TestManager_GetGroup_Missing(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	g, members := mgr.GetGroup(NewCapsuleID())
	if g != nil || members != nil {
		t.Errorf("GetGroup on missing id = (%+v, %+v), want (nil, nil)", g, members)
	}
}

// TestManager_CreateGroup_RejectsDuplicateGroupName verifies that two
// groups in the same cluster cannot share a name. The second CreateGroup
// must return ErrCapsuleNameConflict (the same sentinel Manager.Create
// uses) so the API layer surfaces gRPC AlreadyExists.
func TestManager_CreateGroup_RejectsDuplicateGroupName(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	build := func() GroupSpec {
		return GroupSpec{
			Colocation:    ColocationModeEnum.SameOrbit(),
			CascadeDelete: true,
			Members: []MemberSpec{
				{Name: "api", Spec: memberSpecForTest("api")},
			},
		}
	}

	if _, _, err := mgr.CreateGroup(context.Background(), "prod/dc1", "stack", build(), nil); err != nil {
		t.Fatalf("first CreateGroup failed: %v", err)
	}

	// Second create with the same name + cluster must reject. Use a
	// distinct member name to make sure the failure is on the group, not
	// on a member's Create-time uniqueness check.
	conflict := build()
	conflict.Members[0] = MemberSpec{Name: "api2", Spec: memberSpecForTest("api2")}
	_, _, err := mgr.CreateGroup(context.Background(), "prod/dc1", "stack", conflict, nil)
	if err == nil {
		t.Fatal("expected duplicate-name CreateGroup to fail")
	}
	if !errors.Is(err, ErrCapsuleNameConflict) {
		t.Errorf("expected ErrCapsuleNameConflict, got %T %v", err, err)
	}
}

// TestManager_CreateGroup_RejectsGroupNameMatchingStandalone verifies a
// group name cannot collide with an existing standalone capsule's name
// in the same cluster. Names live in one namespace because DNS, Service
// resolution, and self-anti-affinity all key off the name.
func TestManager_CreateGroup_RejectsGroupNameMatchingStandalone(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	if _, err := mgr.Create(context.Background(), "prod/dc1", CapsuleSpec{
		Name: "shared", Image: "nginx:alpine", Orbit: "default",
	}); err != nil {
		t.Fatalf("standalone Create failed: %v", err)
	}

	_, _, err := mgr.CreateGroup(context.Background(), "prod/dc1", "shared", GroupSpec{
		Colocation:    ColocationModeEnum.SameOrbit(),
		CascadeDelete: true,
		Members:       []MemberSpec{{Name: "m1", Spec: memberSpecForTest("m1")}},
	}, nil)
	if err == nil {
		t.Fatal("expected CreateGroup to reject group name colliding with standalone")
	}
	if !errors.Is(err, ErrCapsuleNameConflict) {
		t.Errorf("expected ErrCapsuleNameConflict, got %T %v", err, err)
	}
}

// TestManager_CreateGroup_MemberNameCollisionRollsBack verifies that if
// a member name collides with an existing standalone capsule, the group
// create rolls back atomically: no group row, no orphan members, just
// the original standalone.
func TestManager_CreateGroup_MemberNameCollisionRollsBack(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	if _, err := mgr.Create(context.Background(), "prod/dc1", CapsuleSpec{
		Name: "db", Image: "postgres:15", Orbit: "data",
	}); err != nil {
		t.Fatalf("standalone Create failed: %v", err)
	}

	startCount := mgr.Count()

	_, _, err := mgr.CreateGroup(context.Background(), "prod/dc1", "my-group", GroupSpec{
		Colocation:    ColocationModeEnum.SameOrbit(),
		CascadeDelete: true,
		Members: []MemberSpec{
			{Name: "api", Spec: memberSpecForTest("api")},
			// "db" collides with the pre-existing standalone capsule.
			{Name: "db", Spec: memberSpecForTest("db")},
		},
	}, nil)
	if err == nil {
		t.Fatal("expected CreateGroup to fail on member name collision")
	}
	if !errors.Is(err, ErrCapsuleNameConflict) {
		t.Errorf("expected ErrCapsuleNameConflict, got %T %v", err, err)
	}

	// Rollback contract: store must hold exactly the original standalone
	// — no group row, no orphan "api" member.
	if got := mgr.Count(); got != startCount {
		t.Errorf("store count after failed CreateGroup = %d, want %d (no leftovers)", got, startCount)
	}
	if mgr.GetByName("my-group") != nil {
		t.Error("group capsule should not exist after rollback")
	}
	if mgr.GetByName("api") != nil {
		t.Error("partial member 'api' should have been rolled back")
	}
}

// TestManager_CreateGroup_EmptyClusterID verifies CreateGroup rejects an
// empty clusterID before any work happens.
func TestManager_CreateGroup_EmptyClusterID(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	_, _, err := mgr.CreateGroup(context.Background(), "", "g", GroupSpec{
		Colocation: ColocationModeEnum.SameOrbit(),
		Members:    []MemberSpec{{Name: "a", Spec: memberSpecForTest("a")}},
	}, nil)
	if err == nil {
		t.Fatal("expected error for empty clusterID")
	}
	if !strings.Contains(err.Error(), "clusterID") {
		t.Errorf("error should mention clusterID, got %q", err.Error())
	}
	if mgr.Count() != 0 {
		t.Error("no rows should be written when clusterID is empty")
	}
}

// buildGroupForDeleteTest creates a 3-member group with the given
// CascadeDelete flag, then resets the captured event slice so the caller can
// observe only the events emitted by the subsequent Delete.
func buildGroupForDeleteTest(t *testing.T, cascade bool) (*Manager, *Capsule, []*Capsule, *[]ManagerEvent, *sync.Mutex) {
	t.Helper()

	handler, events, mu := captureEvents()
	mgr := NewManager(WithManagerEventHandler(handler))

	groupSpec := GroupSpec{
		Colocation: ColocationModeEnum.SameOrbit(),
		Members: []MemberSpec{
			{Name: "api", Spec: memberSpecForTest("api")},
			{Name: "worker", Spec: memberSpecForTest("worker")},
			{Name: "cache", Spec: memberSpecForTest("cache")},
		},
		CascadeDelete: cascade,
	}

	group, members, err := mgr.CreateGroup(context.Background(), "test/dc1/cluster", "delete-test", groupSpec, Labels{"app": "demo"})
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}

	// Reset captured events so the Delete-phase assertions only see the
	// post-Delete emissions.
	mu.Lock()
	*events = (*events)[:0]
	mu.Unlock()

	return mgr, group, members, events, mu
}

// TestManager_DeleteGroup_Cascade verifies CascadeDelete=true removes the
// group and every member from the store and emits EventCapsuleDeleted for
// each one (3 members + 1 group = 4 deletion events).
func TestManager_DeleteGroup_Cascade(t *testing.T) {
	t.Parallel()

	mgr, group, members, events, mu := buildGroupForDeleteTest(t, true)

	if err := mgr.Delete(context.Background(), group.ID); err != nil {
		t.Fatalf("Delete(group) failed: %v", err)
	}

	if got := mgr.Get(group.ID); got != nil {
		t.Errorf("group still in store after cascade delete: %+v", got)
	}
	for _, mc := range members {
		if got := mgr.Get(mc.ID); got != nil {
			t.Errorf("member %q still in store after cascade delete: %+v", mc.Spec.Name, got)
		}
	}
	if got := mgr.Count(); got != 0 {
		t.Errorf("store count = %d, want 0 after cascade delete", got)
	}

	if got := countEventsByType(mu, events, EventCapsuleDeleted); got != 4 {
		t.Errorf("EventCapsuleDeleted count = %d, want 4 (3 members + group)", got)
	}
	// No member updates should fire on the cascade path.
	if got := countEventsByType(mu, events, EventCapsuleUpdated); got != 0 {
		t.Errorf("EventCapsuleUpdated count = %d, want 0 on cascade path", got)
	}
}

// TestManager_DeleteGroup_NonCascade (10.T16) verifies CascadeDelete=false
// detaches members instead of deleting them: GroupID/GroupMember cleared,
// Kind preserved as Capsule, members still in the store. The group capsule
// itself is removed and EventCapsuleUpdated fires for each member while
// EventCapsuleDeleted fires once for the group.
func TestManager_DeleteGroup_NonCascade(t *testing.T) {
	t.Parallel()

	mgr, group, members, events, mu := buildGroupForDeleteTest(t, false)

	if err := mgr.Delete(context.Background(), group.ID); err != nil {
		t.Fatalf("Delete(group) failed: %v", err)
	}

	if got := mgr.Get(group.ID); got != nil {
		t.Errorf("group still in store after non-cascade delete: %+v", got)
	}

	for _, mc := range members {
		got := mgr.Get(mc.ID)
		if got == nil {
			t.Fatalf("member %q removed by non-cascade delete; should remain as standalone", mc.Spec.Name)
		}
		if got.Spec.GroupID != "" {
			t.Errorf("member %q GroupID = %q, want empty", mc.Spec.Name, got.Spec.GroupID)
		}
		if got.Spec.GroupMember {
			t.Errorf("member %q GroupMember = true, want false", mc.Spec.Name)
		}
		if got.Spec.Kind != CapsuleKindEnum.Capsule() {
			t.Errorf("member %q Kind = %q, want Capsule", mc.Spec.Name, got.Spec.Kind)
		}
	}

	if got := mgr.Count(); got != 3 {
		t.Errorf("store count = %d, want 3 (members survive)", got)
	}

	if got := countEventsByType(mu, events, EventCapsuleUpdated); got != 3 {
		t.Errorf("EventCapsuleUpdated count = %d, want 3 (one per detached member)", got)
	}
	if got := countEventsByType(mu, events, EventCapsuleDeleted); got != 1 {
		t.Errorf("EventCapsuleDeleted count = %d, want 1 (only the group)", got)
	}
}

// TestManager_DeleteGroup_MissingMemberIsSuccess verifies reaper idempotence:
// if a member is already gone (deleted directly before the group), the
// cascade path returns nil rather than surfacing ErrNotFound.
func TestManager_DeleteGroup_MissingMemberIsSuccess(t *testing.T) {
	t.Parallel()

	mgr, group, members, _, _ := buildGroupForDeleteTest(t, true)

	// Pre-delete one member directly. The cascade should treat the
	// resulting ErrNotFound as success.
	if err := mgr.Delete(context.Background(), members[1].ID); err != nil {
		t.Fatalf("pre-delete member failed: %v", err)
	}

	if err := mgr.Delete(context.Background(), group.ID); err != nil {
		t.Fatalf("Delete(group) returned %v on missing member; want nil", err)
	}

	for _, mc := range members {
		if got := mgr.Get(mc.ID); got != nil {
			t.Errorf("member %q still in store: %+v", mc.Spec.Name, got)
		}
	}
	if got := mgr.Get(group.ID); got != nil {
		t.Error("group still in store after cascade delete")
	}
}

// TestManager_DeleteGroup_NotAGroup verifies that calling Delete on a
// standalone Kind=Capsule capsule still flows through the original
// non-group path: the capsule is removed, EventCapsuleDeleted fires once,
// and no group-specific behaviour kicks in.
func TestManager_DeleteGroup_NotAGroup(t *testing.T) {
	t.Parallel()

	handler, events, mu := captureEvents()
	mgr := NewManager(WithManagerEventHandler(handler))

	c, err := mgr.Create(context.Background(), "test/dc1/cluster", CapsuleSpec{
		Name: "lonely", Image: "img", Orbit: "api",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	mu.Lock()
	*events = (*events)[:0]
	mu.Unlock()

	if err := mgr.Delete(context.Background(), c.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if got := mgr.Get(c.ID); got != nil {
		t.Error("standalone capsule still in store after delete")
	}
	if got := countEventsByType(mu, events, EventCapsuleDeleted); got != 1 {
		t.Errorf("EventCapsuleDeleted = %d, want 1", got)
	}
	if got := countEventsByType(mu, events, EventCapsuleUpdated); got != 0 {
		t.Errorf("EventCapsuleUpdated = %d, want 0", got)
	}
}

// TestManager_DeleteGroup_NilGroupSpec verifies that a Kind=Group capsule
// missing its GroupSpec returns a clear error rather than panicking. We
// install the malformed capsule via Receive (the same path the mesh would
// use) since CreateGroup never produces this shape.
func TestManager_DeleteGroup_NilGroupSpec(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	bad := &Capsule{
		ID:        NewCapsuleID(),
		ClusterID: "test/dc1/cluster",
		Spec: CapsuleSpec{
			Name:  "broken",
			Orbit: "groups",
			Kind:  CapsuleKindEnum.Group(),
			Group: nil,
		},
		Status: enums.CapsuleStatusEnum.Announced(),
	}
	if err := mgr.Receive(bad); err != nil {
		t.Fatalf("Receive(group with nil spec) failed: %v", err)
	}

	err := mgr.Delete(context.Background(), bad.ID)
	if err == nil {
		t.Fatal("expected error deleting group with nil GroupSpec")
	}
	if !strings.Contains(err.Error(), "nil GroupSpec") {
		t.Errorf("error should mention nil GroupSpec, got %q", err.Error())
	}

	// The malformed capsule must remain in the store: deleteGroup refused
	// to touch it.
	if got := mgr.Get(bad.ID); got == nil {
		t.Error("malformed group capsule was removed despite the error")
	}
}

// TestManager_DeleteGroup_ReturnsErrNotFound verifies Manager.Delete returns
// the ErrNotFound sentinel (not a wrapped fmt error) for missing IDs so
// cascade callers can match it via errors.Is.
func TestManager_DeleteGroup_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()

	mgr := NewManager()
	err := mgr.Delete(context.Background(), NewCapsuleID())
	if err == nil {
		t.Fatal("expected ErrNotFound for missing capsule")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// TestManager_CreateGroup_PersistsAcrossReopen (10.T15) verifies that a
// SQLite-backed manager preserves a group capsule and all its members
// across a close/reopen cycle, with the group→members linkage intact via
// GroupID.
//
// The test:
//  1. Opens a SQLite-backed Store at a tempfile path.
//  2. Creates a 3-member group with depends_on (db → api → web).
//  3. Captures (groupID, MemberIDs) from the in-memory view.
//  4. Closes the store and opens a fresh one at the same path.
//  5. Builds a fresh Manager pointed at the reopened store.
//  6. Verifies the group capsule round-tripped: Kind=Group, MemberIDs and
//     Group.Members intact, members' GroupID = groupID, GroupMember=true.
//
// This locks the contract that node restart does not orphan group rows
// and members remain re-linkable via GroupID without any in-memory
// reconciliation pass.
func TestManager_CreateGroup_PersistsAcrossReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "capsules.db")

	// --- Phase 1: open store, create the group ---
	store1, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}

	mgr1 := NewManager(WithManagerStore(store1))

	groupSpec := GroupSpec{
		Colocation:    ColocationModeEnum.SameOrbit(),
		CascadeDelete: true,
		Members: []MemberSpec{
			{Name: "db", Spec: memberSpecForTest("db")},
			{Name: "api", Spec: memberSpecForTest("api"), DependsOn: []string{"db"}},
			{Name: "web", Spec: memberSpecForTest("web"), DependsOn: []string{"api"}},
		},
	}

	ctx := context.Background()
	group, members, err := mgr1.CreateGroup(ctx, "test-cluster", "my-stack", groupSpec, Labels{
		"app": "my-stack",
	})
	if err != nil {
		t.Fatalf("CreateGroup failed: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}

	groupID := group.ID
	originalMemberIDs := make(map[string]CapsuleID, 3)
	for _, m := range members {
		originalMemberIDs[m.Spec.Name] = m.ID
	}

	if err := store1.Close(); err != nil {
		t.Fatalf("store close failed: %v", err)
	}

	// --- Phase 2: reopen the same SQLite file with a fresh manager ---
	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore (reopen) failed: %v", err)
	}
	defer store2.Close()

	mgr2 := NewManager(WithManagerStore(store2))

	// Verify the group capsule round-tripped with Kind/Group intact.
	g := mgr2.Get(groupID)
	if g == nil {
		t.Fatal("group capsule missing after reopen")
	}
	if g.Spec.Kind != CapsuleKindEnum.Group() {
		t.Errorf("group Kind: got %q, want group", g.Spec.Kind)
	}
	if g.Spec.Group == nil {
		t.Fatal("group spec missing after reopen")
	}
	if g.Spec.Group.Colocation != ColocationModeEnum.SameOrbit() {
		t.Errorf("group colocation: got %q, want same-orbit", g.Spec.Group.Colocation)
	}
	if !g.Spec.Group.CascadeDelete {
		t.Error("group cascade_delete: got false, want true")
	}
	if len(g.Spec.Group.MemberIDs) != 3 {
		t.Fatalf("group MemberIDs count: got %d, want 3", len(g.Spec.Group.MemberIDs))
	}
	if len(g.Spec.Group.Members) != 3 {
		t.Fatalf("group Members count: got %d, want 3", len(g.Spec.Group.Members))
	}

	// Verify the dependency map persisted.
	gotDeps := make(map[string][]string, 3)
	for _, m := range g.Spec.Group.Members {
		gotDeps[m.Name] = append([]string(nil), m.DependsOn...)
	}
	if len(gotDeps["db"]) != 0 {
		t.Errorf("db deps: got %v, want []", gotDeps["db"])
	}
	if len(gotDeps["api"]) != 1 || gotDeps["api"][0] != "db" {
		t.Errorf("api deps: got %v, want [db]", gotDeps["api"])
	}
	if len(gotDeps["web"]) != 1 || gotDeps["web"][0] != "api" {
		t.Errorf("web deps: got %v, want [api]", gotDeps["web"])
	}

	// Verify each member round-tripped with the correct GroupID/GroupMember.
	for name, originalID := range originalMemberIDs {
		mc := mgr2.Get(originalID)
		if mc == nil {
			t.Errorf("member %q (id %s) missing after reopen", name, originalID)
			continue
		}
		if mc.Spec.Kind != CapsuleKindEnum.Capsule() {
			t.Errorf("member %q Kind: got %q, want capsule", name, mc.Spec.Kind)
		}
		if mc.Spec.GroupID != groupID {
			t.Errorf("member %q GroupID: got %s, want %s", name, mc.Spec.GroupID, groupID)
		}
		if !mc.Spec.GroupMember {
			t.Errorf("member %q GroupMember: got false, want true", name)
		}
	}

	// Verify GetGroup returns the group + its members from the reopened state.
	rg, rmembers := mgr2.GetGroup(groupID)
	if rg == nil {
		t.Fatal("GetGroup returned nil after reopen")
	}
	if len(rmembers) != 3 {
		t.Errorf("GetGroup members count after reopen: got %d, want 3", len(rmembers))
	}

	// Verify lifecycle was re-installed at the persisted status. After Create
	// the group transitioned Created → Announced via auto-announce; the
	// re-opened manager must reflect that state, not reset to Created.
	if g.Status != enums.CapsuleStatusEnum.Announced() {
		t.Errorf("group status after reopen: got %q, want announced", g.Status)
	}
	status, err := mgr2.Status(groupID)
	if err != nil {
		t.Errorf("Status() after reopen: %v", err)
	}
	if status != enums.CapsuleStatusEnum.Announced() {
		t.Errorf("Status() returned %q, want announced", status)
	}
}
