package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/runtime"
	"github.com/tareksalem/falak/runtime/mock"
)

// groupStubCapsuleStore returns per-capsule specs keyed by ID. Unknown
// capsules return ErrNotFound via a stub error.
type groupStubCapsuleStore struct {
	mu    sync.Mutex
	specs map[string]*runtime.CapsuleSpec
}

func newGroupStubStore() *groupStubCapsuleStore {
	return &groupStubCapsuleStore{specs: map[string]*runtime.CapsuleSpec{}}
}

func (s *groupStubCapsuleStore) put(id, image string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs[id] = &runtime.CapsuleSpec{
		Name:        id,
		Image:       image,
		ImageDigest: "sha256:" + id,
		NetworkMode: runtime.NetworkModeEnum.Bridge(),
	}
}

func (s *groupStubCapsuleStore) GetSpec(capsuleID string) (*runtime.CapsuleSpec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, ok := s.specs[capsuleID]
	if !ok {
		return nil, errSpecNotFound{id: capsuleID}
	}
	return spec, nil
}

type errSpecNotFound struct{ id string }

func (e errSpecNotFound) Error() string { return "spec not found: " + e.id }

// groupStubView mirrors stubGroupView from group_test.go but lives in
// the _test package so it can be used by external (_test) tests. It
// implements runtime.GroupView in terms of an in-memory members map.
type groupStubView struct {
	mu      sync.Mutex
	groupID string
	members map[string]groupStubMember
	byID    map[string]string
}

type groupStubMember struct {
	capsuleID string
	dependsOn []string
	running   bool
}

func newGroupStubView(groupID string, members map[string]groupStubMember) *groupStubView {
	v := &groupStubView{
		groupID: groupID,
		members: members,
		byID:    make(map[string]string, len(members)),
	}
	for name, m := range members {
		v.byID[m.capsuleID] = name
	}
	return v
}

func (v *groupStubView) MemberInfo(capsuleID string) (runtime.MemberInfo, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	name, ok := v.byID[capsuleID]
	if !ok {
		return runtime.MemberInfo{}, false
	}
	m := v.members[name]
	siblings := make(map[string]runtime.SiblingState, len(v.members))
	for sibName, sib := range v.members {
		siblings[sibName] = runtime.SiblingState{
			CapsuleID: sib.capsuleID,
			Running:   sib.running,
		}
	}
	return runtime.MemberInfo{
		GroupID:   v.groupID,
		Name:      name,
		DependsOn: append([]string(nil), m.dependsOn...),
		Siblings:  siblings,
	}, true
}

func (v *groupStubView) CapsuleIDByMemberName(groupID, memberName string) (string, bool) {
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

// Colocation reports the stub view's group colocation mode. Always
// same-node for tests that exercise the runtime rollback path; the
// constant matches runtime.ColocationSameNode.
func (v *groupStubView) Colocation(groupID string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if groupID != v.groupID {
		return "", false
	}
	return runtime.ColocationSameNode, true
}

// TestStartGroup_StartsAllMembers verifies that StartGroup walks the
// MemberIDs list and dispatches every member through the standard
// container-start pipeline.
func TestStartGroup_StartsAllMembers(t *testing.T) {
	rt := mock.New()
	store := newGroupStubStore()
	store.put("c-db", "db:1")
	store.put("c-web", "web:1")

	view := newGroupStubView("g1", map[string]groupStubMember{
		"db":  {capsuleID: "c-db"},
		"web": {capsuleID: "c-web", dependsOn: []string{"db"}},
	})

	lc := &stubLifecycle{}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithLifecycleNotifier(lc),
		runtime.WithGroupView(view),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.StartGroup(runtime.GroupClaimWon{
		GroupID:     "g1",
		ClusterPath: "test/dc1/c",
		MemberIDs:   []string{"c-db", "c-web"},
		NodeID:      "node-a",
		Score:       95,
	})

	// db has no deps; it should reach Running quickly.
	lc.waitRunning(t, "c-db", 5*time.Second)

	// web depends on db; it parks until db is reported running. Mark db
	// running on the view + tell the coordinator so web releases.
	view.mu.Lock()
	dbMember := view.members["db"]
	dbMember.running = true
	view.members["db"] = dbMember
	view.mu.Unlock()
	h.OnDependencyRunning("c-db")

	lc.waitRunning(t, "c-web", 5*time.Second)

	if rt.ContainerStatus("falak-c-db-0") != runtime.ContainerStatusEnum.Running() {
		t.Fatal("db container should be running")
	}
	if rt.ContainerStatus("falak-c-web-0") != runtime.ContainerStatusEnum.Running() {
		t.Fatal("web container should be running")
	}
}

// TestStartGroup_EmptyMemberList logs and returns without touching the
// runtime; verifies the defensive guard does not panic.
func TestStartGroup_EmptyMemberList(t *testing.T) {
	rt := mock.New()
	store := newGroupStubStore()
	lc := &stubLifecycle{}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithLifecycleNotifier(lc),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.StartGroup(runtime.GroupClaimWon{
		GroupID:     "g-empty",
		ClusterPath: "test/dc1/c",
		MemberIDs:   nil,
		NodeID:      "node-a",
	})

	if rt.ContainerCount() != 0 {
		t.Fatalf("expected 0 containers, got %d", rt.ContainerCount())
	}
}

// TestStartGroup_NoGroupView_StartsImmediately verifies the no-GroupView
// fast path: every member starts unconditionally, in the order given.
func TestStartGroup_NoGroupView_StartsImmediately(t *testing.T) {
	rt := mock.New()
	store := newGroupStubStore()
	store.put("c-a", "a:1")
	store.put("c-b", "b:1")
	lc := &stubLifecycle{}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithLifecycleNotifier(lc),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.StartGroup(runtime.GroupClaimWon{
		GroupID:   "g2",
		MemberIDs: []string{"c-a", "c-b"},
		NodeID:    "node-a",
	})

	lc.waitRunning(t, "c-a", 5*time.Second)
	lc.waitRunning(t, "c-b", 5*time.Second)
}
