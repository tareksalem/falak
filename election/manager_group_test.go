package election

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/election/gravity"
)

// --- Test helpers ----------------------------------------------------------

// fakeGroupSink records every emitted group-claim outcome so tests can
// assert on it without a real event bus.
type fakeGroupSink struct {
	mu     sync.Mutex
	won    []fakeGroupEvent
	lost   []fakeGroupEvent
	failed []fakeGroupEvent
}

type fakeGroupEvent struct {
	GroupID capsule.CapsuleID
	NodeID  string
	Score   float64
	Reason  string
}

func (s *fakeGroupSink) EmitGroupWon(req GroupClaimRequest, nodeID string, score float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.won = append(s.won, fakeGroupEvent{GroupID: req.GroupID, NodeID: nodeID, Score: score})
}

func (s *fakeGroupSink) EmitGroupLost(req GroupClaimRequest, nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lost = append(s.lost, fakeGroupEvent{GroupID: req.GroupID, NodeID: nodeID})
}

func (s *fakeGroupSink) EmitGroupFailed(req GroupClaimRequest, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = append(s.failed, fakeGroupEvent{GroupID: req.GroupID, Reason: reason})
}

func (s *fakeGroupSink) snapshot() (won, lost, failed []fakeGroupEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := append([]fakeGroupEvent(nil), s.won...)
	l := append([]fakeGroupEvent(nil), s.lost...)
	f := append([]fakeGroupEvent(nil), s.failed...)
	return w, l, f
}

// fakeStore is an in-memory CapsuleStore for tests.
type fakeStore struct {
	mu     sync.Mutex
	byID   map[capsule.CapsuleID]*capsule.Capsule
}

func newFakeStore() *fakeStore {
	return &fakeStore{byID: make(map[capsule.CapsuleID]*capsule.Capsule)}
}

func (s *fakeStore) put(c *capsule.Capsule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[c.ID] = c
}

func (s *fakeStore) Get(id capsule.CapsuleID) *capsule.Capsule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id]
}

// fakeProvider returns a fixed NodeState for tests.
type fakeProvider struct {
	state gravity.NodeState
}

func (p *fakeProvider) LocalNode(string) (gravity.NodeState, error) {
	return p.state, nil
}

// fakeLifecycle satisfies LifecycleController without driving real state.
type fakeLifecycle struct{}

func (fakeLifecycle) StartElection(capsule.CapsuleID) error   { return nil }
func (fakeLifecycle) WinElection(capsule.CapsuleID) error     { return nil }
func (fakeLifecycle) ElectionTimeout(capsule.CapsuleID) error { return nil }

// memberCapsule builds a minimal group-member capsule with the given
// resources. Mirrors the helper in gravity/combined_test.go but defined
// here because it cannot be imported across the package boundary.
func memberCapsule(t *testing.T, name string, cpu int32, memMB int64) *capsule.Capsule {
	t.Helper()
	c := &capsule.Capsule{
		ID:        capsule.NewCapsuleID(),
		ClusterID: "test/dc1/c",
		Spec: capsule.CapsuleSpec{
			Name:  name,
			Image: name + ":v1",
			Orbit: "api",
			Resources: capsule.ResourceRequirements{
				CPUCores: cpu,
				MemoryMB: memMB,
			},
		},
	}
	capsule.DefaultSpec(&c.Spec)
	return c
}

func healthyNodeState(id string, cpuFree int32, memFree int64) gravity.NodeState {
	return gravity.NodeState{
		NodeID:      id,
		ClusterPath: "test/dc1/c",
		Datacenter:  "dc1",
		Region:      "us-east",
		Labels:      capsule.Labels{},
		Resources: gravity.Resources{
			CPUCoresTotal: 8, CPUCoresFree: cpuFree,
			MemoryMBTotal: 16384, MemoryMBFree: memFree,
			DiskMBTotal: 100000, DiskMBFree: 90000,
		},
		Status:           gravity.NodeStatusEnum.Active(),
		ReliabilityScore: 1.0,
	}
}

// newTestPubSub starts a single in-process libp2p host with gossipsub
// attached. The host listens on no addresses (NoListenAddrs) so the test
// stays self-contained. The returned host/pubsub are torn down via
// t.Cleanup.
func newTestPubSub(t *testing.T) (host.Host, *pubsub.PubSub, string) {
	t.Helper()
	h, err := libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	ps, err := pubsub.NewGossipSub(context.Background(), h)
	if err != nil {
		t.Fatalf("NewGossipSub: %v", err)
	}
	return h, ps, h.ID().String()
}

// makeGroupManager wires a Manager ready for HandleGroupClaimRequest
// against a real pubsub. Returns the manager, the fake sink it publishes
// to, and the fake store it reads from. Manager is Stopped via Cleanup.
func makeGroupManager(t *testing.T, nodeState gravity.NodeState, opts ...ManagerOption) (*Manager, *fakeGroupSink, *fakeStore) {
	t.Helper()
	_, ps, nodeID := newTestPubSub(t)
	nodeState.NodeID = nodeID

	store := newFakeStore()
	sink := &fakeGroupSink{}
	provider := &fakeProvider{state: nodeState}
	calc := gravity.NewCalculator()

	combined := []ManagerOption{
		WithNodeID(nodeID),
		WithCapsuleStore(store),
		WithLifecycleController(fakeLifecycle{}),
		WithCalculator(calc),
		WithStateProvider(provider),
		WithPubSub(ps),
		WithGroupClaimSink(sink),
		WithElectionTimeout(2 * time.Second),
		WithTiebreakWindow(50 * time.Millisecond),
		WithPublishTimeout(1 * time.Second),
	}
	combined = append(combined, opts...)

	mgr := NewManager(&stubStrategy{name: "delay"}, combined...)
	mgr.Start(context.Background())
	t.Cleanup(mgr.Stop)

	if err := mgr.JoinCluster(nodeState.ClusterPath); err != nil {
		t.Fatalf("JoinCluster: %v", err)
	}
	return mgr, sink, store
}

// waitFor blocks up to d for the predicate to return true; otherwise the
// test is failed. Reduces sleep-driven flakes by polling on a tight loop.
func waitFor(t *testing.T, d time.Duration, msg string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitFor timed out: %s", msg)
}

// --- Group election tests --------------------------------------------------

// T_eligible_local_self_win: a single-node cluster running an eligible
// group converges on GroupClaimWon for the local node.
func TestGroupElection_EligibleLocalSelfWin(t *testing.T) {
	state := healthyNodeState("placeholder", 8, 16000)
	mgr, sink, store := makeGroupManager(t, state)

	members := []*capsule.Capsule{
		memberCapsule(t, "db", 2, 1024),
		memberCapsule(t, "api", 1, 512),
	}
	for _, m := range members {
		store.put(m)
	}

	groupID := capsule.NewCapsuleID()
	memberIDs := []capsule.CapsuleID{members[0].ID, members[1].ID}
	req := GroupClaimRequest{
		GroupID:     groupID,
		MemberIDs:   memberIDs,
		ClusterPath: state.ClusterPath,
		Reason:      ReasonEnum.Initial(),
		CreatedAt:   time.Now(),
	}

	if err := mgr.HandleGroupClaimRequest(req); err != nil {
		t.Fatalf("HandleGroupClaimRequest: %v", err)
	}

	waitFor(t, 3*time.Second, "GroupClaimWon not emitted", func() bool {
		won, _, _ := sink.snapshot()
		return len(won) >= 1
	})

	won, _, failed := sink.snapshot()
	if len(failed) > 0 {
		t.Fatalf("unexpected failure events: %+v", failed)
	}
	if won[0].GroupID != groupID {
		t.Errorf("won.GroupID = %s, want %s", won[0].GroupID, groupID)
	}
	if won[0].NodeID != mgr.nodeID {
		t.Errorf("won.NodeID = %s, want %s", won[0].NodeID, mgr.nodeID)
	}
	if won[0].Score <= 0 {
		t.Errorf("won.Score = %v, want > 0", won[0].Score)
	}
}

// T_ineligible_member: when a member's individual resources do not fit
// the local node, HandleGroupClaimRequest emits GroupClaimFailed with
// the ineligible member's name in the reason.
func TestGroupElection_IneligibleMember(t *testing.T) {
	state := healthyNodeState("placeholder", 8, 16000)
	mgr, sink, store := makeGroupManager(t, state)

	// First member requires 99 CPU — far more than the node has, so its
	// individual eligibility check fails. The combined-fit logic short-
	// circuits with IneligibleMember=<that name>.
	bad := memberCapsule(t, "huge", 99, 1024)
	good := memberCapsule(t, "fine", 1, 512)
	store.put(bad)
	store.put(good)

	groupID := capsule.NewCapsuleID()
	req := GroupClaimRequest{
		GroupID:     groupID,
		MemberIDs:   []capsule.CapsuleID{bad.ID, good.ID},
		ClusterPath: state.ClusterPath,
		Reason:      ReasonEnum.Initial(),
		CreatedAt:   time.Now(),
	}

	if err := mgr.HandleGroupClaimRequest(req); err != nil {
		t.Fatalf("HandleGroupClaimRequest: %v", err)
	}

	waitFor(t, 2*time.Second, "GroupClaimFailed not emitted", func() bool {
		_, _, failed := sink.snapshot()
		return len(failed) >= 1
	})

	won, _, failed := sink.snapshot()
	if len(won) > 0 {
		t.Fatalf("expected no won events, got %+v", won)
	}
	if failed[0].GroupID != groupID {
		t.Errorf("failed.GroupID = %s, want %s", failed[0].GroupID, groupID)
	}
	if !strings.Contains(failed[0].Reason, "huge") {
		t.Errorf("failure reason %q should mention ineligible member 'huge'", failed[0].Reason)
	}
}

// T_reservation_recorded: after a Won outcome the manager records a
// reservation keyed on the group ID with a deadline matching the
// configured per-member image-pull timeout.
func TestGroupElection_ReservationRecorded(t *testing.T) {
	state := healthyNodeState("placeholder", 8, 16000)
	imagePull := 200 * time.Millisecond
	mgr, sink, store := makeGroupManager(t, state, WithGroupImagePullTimeout(imagePull))

	a := memberCapsule(t, "a", 1, 256)
	b := memberCapsule(t, "b", 1, 256)
	store.put(a)
	store.put(b)

	groupID := capsule.NewCapsuleID()
	req := GroupClaimRequest{
		GroupID:     groupID,
		MemberIDs:   []capsule.CapsuleID{a.ID, b.ID},
		ClusterPath: state.ClusterPath,
		Reason:      ReasonEnum.Initial(),
		CreatedAt:   time.Now(),
	}

	before := time.Now()
	if err := mgr.HandleGroupClaimRequest(req); err != nil {
		t.Fatalf("HandleGroupClaimRequest: %v", err)
	}

	waitFor(t, 2*time.Second, "GroupClaimWon not emitted", func() bool {
		won, _, _ := sink.snapshot()
		return len(won) >= 1
	})

	if !mgr.HasReservation(groupID) {
		// HasReservation may be racing with fireReservationTimeout if
		// the test runs slowly enough — give a small grace and re-check
		// before asserting.
		time.Sleep(10 * time.Millisecond)
		if !mgr.HasReservation(groupID) {
			t.Fatalf("expected pendingReservations to contain %s", groupID)
		}
	}

	deadline := mgr.ReservationDeadline(groupID)
	// Deadline = recordReservation_time + N*imagePull + 30s slack. The
	// record time is sometime AFTER `before` (the call had to wait for
	// PublishAt + tiebreak window before the manager called record).
	// Assert the deadline is in the right ballpark:
	//   lower bound: before + N*imagePull + slack (record happened at
	//     or after `before`)
	//   upper bound: before + 5s + N*imagePull + slack (generous cap on
	//     how long the election round can take)
	minExpected := before.Add(time.Duration(len(req.MemberIDs)) * imagePull).Add(30 * time.Second)
	maxExpected := minExpected.Add(5 * time.Second)
	if deadline.Before(minExpected) || deadline.After(maxExpected) {
		t.Errorf("reservation deadline %v out of expected range [%v, %v]",
			deadline, minExpected, maxExpected)
	}

	// Cancel the reservation so the watchdog goroutine exits before
	// the test ends; otherwise Stop blocks on the wg.
	mgr.clearReservation(groupID)
}

// T_reservation_timeout: with a tiny image-pull timeout the reservation
// fires GroupClaimFailed shortly after the deadline.
func TestGroupElection_ReservationTimeout(t *testing.T) {
	state := healthyNodeState("placeholder", 8, 16000)
	// 50ms per member × 1 member + 30s slack would normally be way too
	// long — so call recordReservation directly with a minimal request
	// AND set the configured timeout small enough that the watchdog
	// fires within a test-friendly window.
	mgr, sink, _ := makeGroupManager(t, state, WithGroupImagePullTimeout(50*time.Millisecond))

	groupID := capsule.NewCapsuleID()
	req := GroupClaimRequest{
		GroupID:     groupID,
		MemberIDs:   []capsule.CapsuleID{capsule.NewCapsuleID()},
		ClusterPath: state.ClusterPath,
		Reason:      ReasonEnum.Initial(),
		CreatedAt:   time.Now(),
	}

	// Manually install the reservation with a 100ms deadline by
	// overriding the watchdog's deadline computation. We bypass the
	// public API because recordReservation always recomputes the
	// deadline from N*imagePull+slack — a value too coarse for tests.
	// Instead we install a reservation directly under the lock and
	// kick off the watchdog with the short deadline.
	shortDeadline := time.Now().Add(100 * time.Millisecond)
	watchCtx, watchCancel := context.WithCancel(mgr.ctx)
	mgr.pendingReservationsMu.Lock()
	mgr.pendingReservations[groupID] = &groupReservation{
		GroupID:   groupID,
		MemberIDs: req.MemberIDs,
		NodeID:    mgr.nodeID,
		Deadline:  shortDeadline,
		cancel:    watchCancel,
	}
	mgr.pendingReservationsMu.Unlock()
	mgr.wg.Add(1)
	go func() {
		defer mgr.wg.Done()
		mgr.watchReservation(watchCtx, req, shortDeadline)
	}()

	waitFor(t, 2*time.Second, "GroupClaimFailed not emitted on timeout", func() bool {
		_, _, failed := sink.snapshot()
		return len(failed) >= 1
	})

	_, _, failed := sink.snapshot()
	if failed[0].GroupID != groupID {
		t.Errorf("failed.GroupID = %s, want %s", failed[0].GroupID, groupID)
	}
	if !strings.Contains(failed[0].Reason, "reservation timeout") {
		t.Errorf("failure reason %q should mention reservation timeout", failed[0].Reason)
	}

	if mgr.HasReservation(groupID) {
		t.Errorf("reservation should be cleared after timeout fired")
	}
}

// TestIsBetterGroup_NilRival regression-tests the 10.18 fix: a nil rival
// (observed when a group-claim listener channel is closed mid-flight by
// a replacement registration) must NOT cause isBetterGroup to panic
// dereferencing the rival. The expected behaviour is "local is at least
// as good" — return false so the round stays on the local-wins path.
func TestIsBetterGroup_NilRival(t *testing.T) {
	now := time.Now()
	if isBetterGroup(nil, 50.0, now, "node-a") {
		t.Fatalf("isBetterGroup(nil, ...) returned true; want false")
	}
}

// TestGroupListener_ReplaceConcurrentReceive_NoPanic regression-tests
// the 10.18 fix in the underlying channel-receive path. A concurrent
// reader that pulls from a listener whose channel has been closed
// mid-flight by a second ListenGroup registration on the same group_id
// must observe a closed channel (!ok) without panicking. Run under
// `go test -race` this surfaces any data race introduced by the
// listener-replacement pattern.
func TestGroupListener_ReplaceConcurrentReceive_NoPanic(t *testing.T) {
	state := healthyNodeState("placeholder", 8, 16000)
	_, ps, nodeID := newTestPubSub(t)
	state.NodeID = nodeID

	topic, err := NewClusterTopic(context.Background(), state.ClusterPath, nodeID, ps, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewClusterTopic: %v", err)
	}
	t.Cleanup(topic.Stop)

	groupID := capsule.NewCapsuleID().String()

	// First listener — start a reader goroutine that, on receiving from
	// a closed channel, asserts the second return is false and exits
	// without dereferencing the value. Without the fix in place, code
	// reading rival.GravityScore would panic on the nil zero-value.
	claims1, cancel1 := topic.ListenGroup(groupID, 4)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			rival, ok := <-claims1
			if !ok {
				return
			}
			// Defense-in-depth: pass the (possibly nil) claim through
			// isBetterGroup the same way runGroupElection does.
			_ = isBetterGroup(rival, 50.0, time.Now(), nodeID)
		}
	}()

	// Register a second listener for the SAME group. This closes claims1
	// and replaces it inside the topic's listener map. The reader
	// goroutine above should observe ok=false on the closed channel and
	// exit cleanly — not panic on a nil deref.
	_, cancel2 := topic.ListenGroup(groupID, 4)

	select {
	case <-readerDone:
		// Reader exited cleanly through the !ok path.
	case <-time.After(2 * time.Second):
		t.Fatalf("reader did not observe closed channel within 2s")
	}

	cancel1()
	cancel2()
}
