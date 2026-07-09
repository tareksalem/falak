package election

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/election/gravity"
)

// --- O5 test helpers -------------------------------------------------------

// reservationAwareProvider models the intended anti-over-commit contract:
// the local node's free capacity is its base free capacity MINUS the summed
// member resources of every group reservation the manager currently holds.
// This is what lets a second group's CalculateCombinedFit observe a node as
// over-committed once a first group has won and recorded its reservation —
// the exact "different concurrent group" branch part (b)'s park loop guards.
//
// It reads the manager's live pendingReservations plus a snapshot of member
// resources supplied by the test (the manager does not retain member specs
// beyond IDs in the reservation record).
type reservationAwareProvider struct {
	base    gravity.NodeState
	mgr     *Manager
	store   *fakeStore
}

func (p *reservationAwareProvider) LocalNode(string) (gravity.NodeState, error) {
	st := p.base
	if p.mgr == nil {
		return st, nil
	}
	var cpu int32
	var mem int64
	p.mgr.pendingReservationsMu.Lock()
	for _, res := range p.mgr.pendingReservations {
		for _, mid := range res.MemberIDs {
			if p.store == nil {
				continue
			}
			if c := p.store.Get(mid); c != nil {
				cpu += c.Spec.Resources.CPUCores
				mem += c.Spec.Resources.MemoryMB
			}
		}
	}
	p.mgr.pendingReservationsMu.Unlock()
	st.Resources.CPUCoresFree = st.Resources.CPUCoresFree - cpu
	st.Resources.MemoryMBFree = st.Resources.MemoryMBFree - mem
	return st, nil
}

// makeGroupManagerWithProvider wires a Manager like makeGroupManager but
// lets the caller install a custom StateProvider AFTER the manager is
// constructed (needed by reservationAwareProvider, which references the
// manager it feeds). Returns the manager, sink, store, and node ID.
func makeGroupManagerWithProvider(
	t *testing.T,
	base gravity.NodeState,
	makeProvider func(mgr *Manager, store *fakeStore) gravity.StateProvider,
	opts ...ManagerOption,
) (*Manager, *fakeGroupSink, *fakeStore) {
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
	nodeID := h.ID().String()
	base.NodeID = nodeID

	store := newFakeStore()
	sink := &fakeGroupSink{}
	calc := gravity.NewCalculator()

	// Provider is constructed lazily so it can close over the manager.
	var mgr *Manager
	provider := gravity.StateProvider(&fakeProvider{state: base})

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

	mgr = NewManager(&stubStrategy{name: "delay"}, combined...)
	if makeProvider != nil {
		mgr.provider = makeProvider(mgr, store)
	}
	mgr.Start(context.Background())
	t.Cleanup(mgr.Stop)

	if err := mgr.JoinCluster(base.ClusterPath); err != nil {
		t.Fatalf("JoinCluster: %v", err)
	}
	return mgr, sink, store
}

// --- O5 manager-level tests ------------------------------------------------

// TestRunGroupElection_WonReleasesGroupSlot asserts the O5 ordering
// invariant at the manager level: after a group election resolves Won, the
// local group-claim slot (localGroupClaims[group]) is RELEASED and the
// capacity reservation is PRESENT. The release-after-record ordering is what
// lets a same-node re-election re-acquire the slot instead of dead-locking.
func TestRunGroupElection_WonReleasesGroupSlot(t *testing.T) {
	state := healthyNodeState("placeholder", 8, 16000)
	mgr, sink, store := makeGroupManager(t, state, WithGroupImagePullTimeout(500*time.Millisecond))

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

	if err := mgr.HandleGroupClaimRequest(req); err != nil {
		t.Fatalf("HandleGroupClaimRequest: %v", err)
	}

	waitFor(t, 3*time.Second, "GroupClaimWon not emitted", func() bool {
		won, _, _ := sink.snapshot()
		return len(won) >= 1
	})

	// The reservation must be present the instant the win is reported.
	if !mgr.HasReservation(groupID) {
		time.Sleep(10 * time.Millisecond)
		if !mgr.HasReservation(groupID) {
			t.Fatalf("reservation must be present after Won")
		}
	}

	// The slot must be released after the win — this is the O5 fix.
	waitFor(t, 1*time.Second, "group slot not released after win", func() bool {
		mgr.localGroupClaimsMu.Lock()
		defer mgr.localGroupClaimsMu.Unlock()
		return !mgr.localGroupClaims[groupID]
	})

	// The release channel must also be dropped (mirror releaseCapsuleClaim).
	mgr.localGroupClaimsMu.Lock()
	_, chPresent := mgr.groupClaimReleased[groupID]
	mgr.localGroupClaimsMu.Unlock()
	if chPresent {
		t.Fatalf("group release channel must be dropped after release")
	}

	_, _, failed := sink.snapshot()
	if len(failed) > 0 {
		t.Fatalf("unexpected failure events: %+v", failed)
	}

	// Clear the reservation so the watchdog goroutine exits before Stop.
	mgr.clearReservation(groupID)
}

// TestGroupReElection_SameNode_MidFanout is the manager-level assertion of
// the reservation-still-live same-node re-election: a group wins (records a
// reservation, releases the slot on Won), and WITHOUT the reservation being
// cleared, a re-election for the SAME group is dispatched and must win again.
// This is the "member crashed mid-fanout, reservation still live" case — the
// park-loop's re-decide treats the group's OWN live reservation as
// "re-place it," not "step aside," and the slot released on the first Won is
// what lets the second round acquire it.
//
// Before the O5 fix the slot retained from the first Won made the second
// round dead-lock (tryLocalGroupClaim fails → waitForRemoteGroupVerdict →
// "no claim heard"). This runs at the manager level (with the default
// reservation-blind provider) so the runtime StartGroup / member-binding
// machinery does not interfere — that interference is the O5b concern
// tracked separately; here the slot/reservation mechanic is isolated.
func TestGroupReElection_SameNode_MidFanout(t *testing.T) {
	state := healthyNodeState("placeholder", 8, 16000)
	mgr, sink, store := makeGroupManager(t, state, WithGroupImagePullTimeout(2*time.Second))

	a := memberCapsule(t, "a", 1, 256)
	b := memberCapsule(t, "b", 1, 256)
	store.put(a)
	store.put(b)

	groupID := capsule.NewCapsuleID()
	mkReq := func() GroupClaimRequest {
		return GroupClaimRequest{
			GroupID:     groupID,
			MemberIDs:   []capsule.CapsuleID{a.ID, b.ID},
			ClusterPath: state.ClusterPath,
			Reason:      ReasonEnum.Initial(),
			CreatedAt:   time.Now(),
		}
	}

	// Round 1: win the group. Reservation is recorded; the slot is released
	// on Won (O5 part a).
	if err := mgr.HandleGroupClaimRequest(mkReq()); err != nil {
		t.Fatalf("HandleGroupClaimRequest (round 1): %v", err)
	}
	waitFor(t, 3*time.Second, "round 1 did not win", func() bool {
		won, _, _ := sink.snapshot()
		return len(won) >= 1
	})

	// The reservation must be LIVE (mid-fanout: members not all Running) and
	// the slot released.
	if !mgr.HasReservation(groupID) {
		t.Fatalf("reservation must be live after the first win (mid-fanout)")
	}
	waitFor(t, 1*time.Second, "group slot not released after win", func() bool {
		mgr.localGroupClaimsMu.Lock()
		defer mgr.localGroupClaimsMu.Unlock()
		return !mgr.localGroupClaims[groupID]
	})

	// Round 1 must fully drain from group-inflight so the re-fire is not
	// deduped.
	waitFor(t, 2*time.Second, "round 1 group-inflight not drained", func() bool {
		return !mgr.HasGroupInFlight(groupID)
	})

	// Round 2: re-fire the SAME group re-election WITH the reservation still
	// live. Must win again — the released slot + the park-loop's
	// own-reservation-eligible rule carry it (no reservation clear needed).
	winsBefore := func() int { w, _, _ := sink.snapshot(); return len(w) }()
	if err := mgr.HandleGroupClaimRequest(mkReq()); err != nil {
		t.Fatalf("HandleGroupClaimRequest (round 2): %v", err)
	}
	waitFor(t, 3*time.Second, "mid-fanout re-election did not win again (deadlock)", func() bool {
		w, _, _ := sink.snapshot()
		return len(w) > winsBefore
	})

	_, _, failed := sink.snapshot()
	for _, f := range failed {
		if f.GroupID == groupID {
			t.Fatalf("mid-fanout re-election emitted GroupClaimFailed (%q); slot deadlock not fixed", f.Reason)
		}
	}
	if !mgr.HasReservation(groupID) {
		t.Fatalf("reservation should still be present after the re-win")
	}
	mgr.clearReservation(groupID)
}

// TestGroupClaim_ConcurrentSameGroup_NoDoubleClaim fires two group elections
// for the SAME group through the real HandleGroupClaimRequest entry point and
// asserts exactly one round runs to a Won (records the reservation); the
// second request is deduped by the groupInflight guard (returns nil, no
// second round). Both production callers (dispatchGroup / dispatchGroupreelection)
// funnel through HandleGroupClaimRequest, so this is the real invariant — the
// manager never publishes two claims for one group.
func TestGroupClaim_ConcurrentSameGroup_NoDoubleClaim(t *testing.T) {
	state := healthyNodeState("placeholder", 8, 16000)
	mgr, sink, store := makeGroupManager(t, state, WithGroupImagePullTimeout(500*time.Millisecond))

	a := memberCapsule(t, "a", 1, 256)
	b := memberCapsule(t, "b", 1, 256)
	store.put(a)
	store.put(b)

	groupID := capsule.NewCapsuleID()
	mkReq := func() GroupClaimRequest {
		return GroupClaimRequest{
			GroupID:     groupID,
			MemberIDs:   []capsule.CapsuleID{a.ID, b.ID},
			ClusterPath: state.ClusterPath,
			Reason:      ReasonEnum.Initial(),
			CreatedAt:   time.Now(),
		}
	}

	// Fire two concurrent requests for the same group. The groupInflight
	// dedup (HandleGroupClaimRequest, under m.mu) admits exactly one round;
	// the loser returns nil without spawning.
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			if err := mgr.HandleGroupClaimRequest(mkReq()); err != nil {
				t.Errorf("HandleGroupClaimRequest: %v", err)
			}
		}()
	}
	wg.Wait()

	waitFor(t, 3*time.Second, "group did not win", func() bool {
		won, _, _ := sink.snapshot()
		return len(won) >= 1
	})
	// Give any (impossible) second round a window to double-publish before
	// asserting exactly one Won.
	time.Sleep(200 * time.Millisecond)

	won, lost, failed := sink.snapshot()
	if len(won) != 1 {
		t.Fatalf("expected exactly 1 Won, got %d (lost=%d failed=%d)", len(won), len(lost), len(failed))
	}
	if won[0].GroupID != groupID {
		t.Fatalf("Won.GroupID = %s, want %s", won[0].GroupID, groupID)
	}
	if !mgr.HasReservation(groupID) {
		t.Fatalf("expected a reservation after the single Won")
	}
	mgr.clearReservation(groupID)
}

// TestGroupClaim_ConcurrentDifferentGroups_ReservationRefuses wins group A on
// a node, then runs group B (whose combined resources fit ONLY if group A's
// reservation is ignored) on the same node. With the reservation-aware
// provider modelling committed capacity, group B's CalculateCombinedFit must
// see the node as over-committed and refuse — anti-over-commit still holds
// after the O5 slot-release change.
func TestGroupClaim_ConcurrentDifferentGroups_ReservationRefuses(t *testing.T) {
	// Node has 4 free CPU. Group A needs 3, group B needs 3. Individually
	// each fits; together they over-commit. Once A's reservation is
	// recorded, the reservation-aware provider reports 1 free CPU, so B's
	// synthetic (summed) 3-CPU demand no longer fits.
	base := healthyNodeState("placeholder", 4, 8192)
	mgr, sink, store := makeGroupManagerWithProvider(t, base,
		func(m *Manager, s *fakeStore) gravity.StateProvider {
			return &reservationAwareProvider{base: base, mgr: m, store: s}
		},
		WithGroupImagePullTimeout(2*time.Second),
	)
	// Rewrite base with the resolved node ID (makeGroupManagerWithProvider
	// stamped it into the manager, but our provider closed over the pre-stamp
	// copy). Re-point the provider's base to carry the node ID.
	if p, ok := mgr.provider.(*reservationAwareProvider); ok {
		p.base.NodeID = mgr.nodeID
	}

	// Group A: two members summing to 3 CPU.
	a1 := memberCapsule(t, "a1", 2, 512)
	a2 := memberCapsule(t, "a2", 1, 512)
	store.put(a1)
	store.put(a2)
	groupA := capsule.NewCapsuleID()
	reqA := GroupClaimRequest{
		GroupID:     groupA,
		MemberIDs:   []capsule.CapsuleID{a1.ID, a2.ID},
		ClusterPath: base.ClusterPath,
		Reason:      ReasonEnum.Initial(),
		CreatedAt:   time.Now(),
	}
	if err := mgr.HandleGroupClaimRequest(reqA); err != nil {
		t.Fatalf("HandleGroupClaimRequest A: %v", err)
	}
	waitFor(t, 3*time.Second, "group A did not win", func() bool {
		won, _, _ := sink.snapshot()
		return len(won) >= 1
	})
	if !mgr.HasReservation(groupA) {
		t.Fatalf("group A reservation missing after win")
	}

	// Group B: two members summing to 3 CPU — fits only if A's reservation
	// is ignored.
	b1 := memberCapsule(t, "b1", 2, 512)
	b2 := memberCapsule(t, "b2", 1, 512)
	store.put(b1)
	store.put(b2)
	groupB := capsule.NewCapsuleID()
	reqB := GroupClaimRequest{
		GroupID:     groupB,
		MemberIDs:   []capsule.CapsuleID{b1.ID, b2.ID},
		ClusterPath: base.ClusterPath,
		Reason:      ReasonEnum.Initial(),
		CreatedAt:   time.Now(),
	}
	if err := mgr.HandleGroupClaimRequest(reqB); err != nil {
		t.Fatalf("HandleGroupClaimRequest B: %v", err)
	}

	// Group B must fail (ineligible: over-committed) — never win, never
	// record a reservation.
	waitFor(t, 3*time.Second, "group B did not resolve", func() bool {
		_, _, failed := sink.snapshot()
		for _, f := range failed {
			if f.GroupID == groupB {
				return true
			}
		}
		return false
	})

	won, _, _ := sink.snapshot()
	for _, w := range won {
		if w.GroupID == groupB {
			t.Fatalf("group B won despite group A's reservation over-committing the node")
		}
	}
	if mgr.HasReservation(groupB) {
		t.Fatalf("group B must not record a reservation when refused")
	}

	mgr.clearReservation(groupA)
}
