package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// stubGroupView is an in-memory GroupView used by the tests below. It
// returns whatever was loaded into its members map. Sibling running
// states are mutable so tests can simulate dependency transitions.
type stubGroupView struct {
	mu       sync.Mutex
	groupID  string
	members  map[string]stubMember // member name -> details
	byID     map[string]string     // capsule ID -> member name
}

type stubMember struct {
	capsuleID string
	dependsOn []string
	running   bool
}

func newStubView(groupID string, members map[string]stubMember) *stubGroupView {
	view := &stubGroupView{
		groupID: groupID,
		members: members,
		byID:    make(map[string]string, len(members)),
	}
	for name, m := range members {
		view.byID[m.capsuleID] = name
	}
	return view
}

func (v *stubGroupView) MemberInfo(capsuleID string) (MemberInfo, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	name, ok := v.byID[capsuleID]
	if !ok {
		return MemberInfo{}, false
	}
	m := v.members[name]
	siblings := make(map[string]SiblingState, len(v.members))
	for sibName, sib := range v.members {
		siblings[sibName] = SiblingState{
			CapsuleID: sib.capsuleID,
			Running:   sib.running,
		}
	}
	return MemberInfo{
		GroupID:   v.groupID,
		Name:      name,
		DependsOn: append([]string(nil), m.dependsOn...),
		Siblings:  siblings,
	}, true
}

func (v *stubGroupView) CapsuleIDByMemberName(groupID, memberName string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if groupID != v.groupID {
		return "", false
	}
	m, ok := v.members[memberName]
	if !ok {
		return "", false
	}
	return m.capsuleID, true
}

// Colocation reports the test stub's group colocation mode. The stub
// always reports same-node so the runtime-side rollback path is
// exercised by the existing tests; tests that need a different mode
// override via a wrapper. Unknown group IDs return ("", false).
func (v *stubGroupView) Colocation(groupID string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if groupID != v.groupID {
		return "", false
	}
	return ColocationSameNode, true
}

func (v *stubGroupView) markRunning(name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	m := v.members[name]
	m.running = true
	v.members[name] = m
}

// TestGroupCoord_NoDeps_StartImmediately verifies that a member with no
// DependsOn is released right away.
func TestGroupCoord_NoDeps_StartImmediately(t *testing.T) {
	t.Parallel()

	view := newStubView("g1", map[string]stubMember{
		"web": {capsuleID: "c-web"},
	})
	g := newGroupCoord(view, time.Second, zap.NewNop(), nil, nil, nil)

	deps := g.shouldPark(ElectionWon{CapsuleID: "c-web"})
	if len(deps) != 0 {
		t.Fatalf("expected no deps to wait on, got %v", deps)
	}
	if !g.isReleased("c-web") {
		t.Fatal("expected capsule to be marked released past first boot")
	}
}

// TestGroupCoord_NotAMember_StartImmediately verifies that capsules
// outside any group skip the gating logic.
func TestGroupCoord_NotAMember_StartImmediately(t *testing.T) {
	t.Parallel()

	view := newStubView("g1", map[string]stubMember{
		"web": {capsuleID: "c-web"},
	})
	g := newGroupCoord(view, time.Second, zap.NewNop(), nil, nil, nil)

	deps := g.shouldPark(ElectionWon{CapsuleID: "c-other"})
	if len(deps) != 0 {
		t.Fatalf("standalone capsule should not be parked, got deps %v", deps)
	}
}

// TestGroupCoord_DepNotRunning_Parks verifies that an unmet dep returns
// the dep name in the wait list.
func TestGroupCoord_DepNotRunning_Parks(t *testing.T) {
	t.Parallel()

	view := newStubView("g1", map[string]stubMember{
		"db":  {capsuleID: "c-db", running: false},
		"web": {capsuleID: "c-web", dependsOn: []string{"db"}},
	})
	g := newGroupCoord(view, time.Second, zap.NewNop(), nil, nil, nil)

	deps := g.shouldPark(ElectionWon{CapsuleID: "c-web"})
	if len(deps) != 1 || deps[0] != "db" {
		t.Fatalf("expected to wait on [db], got %v", deps)
	}
	if g.isReleased("c-web") {
		t.Fatal("expected capsule to NOT be released yet")
	}
}

// TestGroupCoord_DepAlreadyRunning_StartImmediately verifies that a
// member whose deps are already Running starts without parking.
func TestGroupCoord_DepAlreadyRunning_StartImmediately(t *testing.T) {
	t.Parallel()

	view := newStubView("g1", map[string]stubMember{
		"db":  {capsuleID: "c-db", running: true},
		"web": {capsuleID: "c-web", dependsOn: []string{"db"}},
	})
	g := newGroupCoord(view, time.Second, zap.NewNop(), nil, nil, nil)

	deps := g.shouldPark(ElectionWon{CapsuleID: "c-web"})
	if len(deps) != 0 {
		t.Fatalf("expected no deps to wait on, got %v", deps)
	}
	if !g.isReleased("c-web") {
		t.Fatal("expected capsule to be released past first boot")
	}
}

// TestGroupCoord_ReleaseOnDependencyRunning verifies that a parked
// start is released when its dependency reports Running.
func TestGroupCoord_ReleaseOnDependencyRunning(t *testing.T) {
	t.Parallel()

	view := newStubView("g1", map[string]stubMember{
		"db":  {capsuleID: "c-db", running: false},
		"web": {capsuleID: "c-web", dependsOn: []string{"db"}},
	})

	released := make(chan ElectionWon, 1)
	startFn := func(ev ElectionWon) { released <- ev }
	wg := &sync.WaitGroup{}

	g := newGroupCoord(view, time.Second, zap.NewNop(), startFn, nil, wg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Park web waiting on db.
	g.park(ctx, ElectionWon{CapsuleID: "c-web", ReplicaID: "0"}, []string{"db"})
	if g.pendingCount() != 1 {
		t.Fatalf("expected 1 parked entry, got %d", g.pendingCount())
	}

	// Mark db running and notify.
	view.markRunning("db")
	g.onDependencyRunning("c-db")

	select {
	case ev := <-released:
		if ev.CapsuleID != "c-web" {
			t.Fatalf("released wrong capsule: %s", ev.CapsuleID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for release")
	}

	if g.pendingCount() != 0 {
		t.Fatalf("expected 0 parked entries after release, got %d", g.pendingCount())
	}
	if !g.isReleased("c-web") {
		t.Fatal("expected web to be marked released")
	}
}

// TestGroupCoord_ReleaseRequiresAllDeps verifies that a member with
// multiple deps does not release until ALL deps are running.
func TestGroupCoord_ReleaseRequiresAllDeps(t *testing.T) {
	t.Parallel()

	view := newStubView("g1", map[string]stubMember{
		"db":    {capsuleID: "c-db", running: false},
		"cache": {capsuleID: "c-cache", running: false},
		"api":   {capsuleID: "c-api", dependsOn: []string{"db", "cache"}},
	})

	released := make(chan ElectionWon, 1)
	startFn := func(ev ElectionWon) { released <- ev }
	wg := &sync.WaitGroup{}

	g := newGroupCoord(view, time.Second, zap.NewNop(), startFn, nil, wg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g.park(ctx, ElectionWon{CapsuleID: "c-api"}, []string{"db", "cache"})

	// First dep arrives — should NOT release.
	view.markRunning("db")
	g.onDependencyRunning("c-db")

	select {
	case <-released:
		t.Fatal("released after only one of two deps; should still wait")
	case <-time.After(50 * time.Millisecond):
	}

	// Second dep arrives — should release.
	view.markRunning("cache")
	g.onDependencyRunning("c-cache")

	select {
	case ev := <-released:
		if ev.CapsuleID != "c-api" {
			t.Fatalf("released wrong capsule: %s", ev.CapsuleID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for release after both deps running")
	}
}

// TestGroupCoord_DeadlineExpires verifies that a parked entry whose
// deadline expires fires MarkFailed.
func TestGroupCoord_DeadlineExpires(t *testing.T) {
	t.Parallel()

	view := newStubView("g1", map[string]stubMember{
		"db":  {capsuleID: "c-db", running: false},
		"web": {capsuleID: "c-web", dependsOn: []string{"db"}},
	})

	failed := make(chan string, 1)
	failFn := func(capsuleID, reason string) { failed <- capsuleID }
	wg := &sync.WaitGroup{}

	g := newGroupCoord(view, 50*time.Millisecond, zap.NewNop(), nil, failFn, wg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g.park(ctx, ElectionWon{CapsuleID: "c-web"}, []string{"db"})

	select {
	case id := <-failed:
		if id != "c-web" {
			t.Fatalf("wrong capsule failed: %s", id)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for deadline to fire")
	}
}

// TestGroupCoord_FirstBootOnlyRelease verifies that once a capsule has
// been released past first boot, subsequent ElectionWon events skip the
// dep gating regardless of current dep state.
func TestGroupCoord_FirstBootOnlyRelease(t *testing.T) {
	t.Parallel()

	view := newStubView("g1", map[string]stubMember{
		"db":  {capsuleID: "c-db", running: true},
		"web": {capsuleID: "c-web", dependsOn: []string{"db"}},
	})
	g := newGroupCoord(view, time.Second, zap.NewNop(), nil, nil, nil)

	// First boot — deps satisfied, released.
	deps := g.shouldPark(ElectionWon{CapsuleID: "c-web"})
	if len(deps) != 0 {
		t.Fatalf("first boot should not park, got deps %v", deps)
	}

	// Simulate db crashing (running=false). web is re-elected.
	view.mu.Lock()
	web := view.members["db"]
	web.running = false
	view.members["db"] = web
	view.mu.Unlock()

	deps = g.shouldPark(ElectionWon{CapsuleID: "c-web"})
	if len(deps) != 0 {
		t.Fatalf("post-release re-election should NOT park, got deps %v", deps)
	}
}

// TestGroupCoord_StopCancelsPending verifies that Stop cancels every
// pending parked-start without firing them.
func TestGroupCoord_StopCancelsPending(t *testing.T) {
	t.Parallel()

	view := newStubView("g1", map[string]stubMember{
		"db":  {capsuleID: "c-db", running: false},
		"web": {capsuleID: "c-web", dependsOn: []string{"db"}},
	})

	var startCount int32
	startFn := func(ev ElectionWon) { atomic.AddInt32(&startCount, 1) }
	var failCount int32
	failFn := func(capsuleID, reason string) { atomic.AddInt32(&failCount, 1) }
	wg := &sync.WaitGroup{}

	g := newGroupCoord(view, 200*time.Millisecond, zap.NewNop(), startFn, failFn, wg)

	ctx, cancel := context.WithCancel(context.Background())

	g.park(ctx, ElectionWon{CapsuleID: "c-web"}, []string{"db"})
	if g.pendingCount() != 1 {
		t.Fatalf("expected 1 parked entry, got %d", g.pendingCount())
	}

	cancel()    // simulate Handler.Stop cancelling its context
	g.stop()    // explicit coordinator stop

	wg.Wait() // park goroutine should exit promptly

	if atomic.LoadInt32(&startCount) != 0 {
		t.Fatal("startFn should not fire after Stop")
	}
	if atomic.LoadInt32(&failCount) != 0 {
		t.Fatal("failFn should not fire after Stop")
	}
	if g.pendingCount() != 0 {
		t.Fatalf("expected 0 parked entries after Stop, got %d", g.pendingCount())
	}
}

// TestGroupCoord_IdempotentPark verifies that re-parking the same
// capsule is a no-op.
func TestGroupCoord_IdempotentPark(t *testing.T) {
	t.Parallel()

	view := newStubView("g1", map[string]stubMember{
		"db":  {capsuleID: "c-db", running: false},
		"web": {capsuleID: "c-web", dependsOn: []string{"db"}},
	})
	wg := &sync.WaitGroup{}
	g := newGroupCoord(view, 50*time.Millisecond, zap.NewNop(), nil,
		func(capsuleID, reason string) {}, wg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g.park(ctx, ElectionWon{CapsuleID: "c-web"}, []string{"db"})
	g.park(ctx, ElectionWon{CapsuleID: "c-web"}, []string{"db"}) // duplicate
	if g.pendingCount() != 1 {
		t.Fatalf("expected 1 parked entry after duplicate park, got %d", g.pendingCount())
	}

	// Let the deadline fire so the wg drains, then verify we don't leak.
	wg.Wait()
}

// TestGroupCoord_OnDepRunning_NonGroupCapsule_Ignored verifies that a
// non-group capsule reaching Running does not affect parked entries.
func TestGroupCoord_OnDepRunning_NonGroupCapsule_Ignored(t *testing.T) {
	t.Parallel()

	view := newStubView("g1", map[string]stubMember{
		"db":  {capsuleID: "c-db", running: false},
		"web": {capsuleID: "c-web", dependsOn: []string{"db"}},
	})
	wg := &sync.WaitGroup{}
	g := newGroupCoord(view, time.Second, zap.NewNop(), nil, nil, wg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g.park(ctx, ElectionWon{CapsuleID: "c-web"}, []string{"db"})

	// A non-group capsule reports Running.
	g.onDependencyRunning("c-unrelated")

	if g.pendingCount() != 1 {
		t.Fatalf("non-group running event should not affect parked entries, got %d", g.pendingCount())
	}
	g.stop()
	wg.Wait()
}
