package election

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/election/gravity"
	electionpb "github.com/tareksalem/falak/election/proto/electionpb"
)

// isClosed reports whether a signalling channel has been closed.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// hostSigner signs claim content with a libp2p host's private key.
type hostSigner struct{ h host.Host }

func (s *hostSigner) Sign(content []byte) ([]byte, error) {
	return s.h.Peerstore().PrivKey(s.h.ID()).Sign(content)
}

// testPairVerifier verifies signed claims against a fixed set of node
// public keys. Models the phonebook-backed production verifier.
type testPairVerifier struct {
	keys map[string]crypto.PubKey
}

func (v *testPairVerifier) Verify(senderID string, content, signature []byte) bool {
	pk, ok := v.keys[senderID]
	if !ok || pk == nil {
		return false
	}
	valid, err := pk.Verify(content, signature)
	return err == nil && valid
}

// fixedScoreStrategy is a Strategy that always reports eligible with a
// fixed gravity score, publishing after publishDelay. Two nodes using it
// tie on score so the O14 timestamp tiebreak decides the winner — the
// exact near-equal-score condition under which O14c's yield matters.
type fixedScoreStrategy struct {
	score        float64
	publishDelay time.Duration
}

func (s *fixedScoreStrategy) Name() string { return "fixed-score" }

func (s *fixedScoreStrategy) Decide(
	_ context.Context,
	_ Request,
	_ *capsule.Capsule,
	_ *gravity.Calculator,
	_ gravity.StateProvider,
) Decision {
	return Decision{
		Eligible:  true,
		Score:     s.score,
		PublishAt: time.Now().Add(s.publishDelay),
		Reason:    "fixed-score: eligible",
	}
}

// O14c — double-winner safety via post-hoc yield.
//
// Even with O14's strict total order, if gossip propagation delay exceeds
// the tiebreak window both nodes can report Won and both start the
// container (duplicate execution). Layer 2 fixes this with a bounded
// reconcile window after report(Won): a node that observes a strictly-
// better rival during reconcile YIELDS (un-wins). Under the O14 total
// order EXACTLY ONE of two rivals yields — never both, never neither.
//
// These tests exercise the reconcile decision deterministically (hand-fed
// claims channel, injectable timing), prove the A/B symmetry invariant
// through the REAL isBetter, and reproduce the double-winner on two real
// managers connected over gossipsub with a controllable propagation delay.

// --- Test 1: reconcile decision (strictly-better → yield; strictly-worse → durable) ---

// makeReconcileManager builds a single-node manager wired for direct
// reconcileAfterWin driving. The reconcile window is generous so the
// hand-fed claim, not a timeout, drives the outcome.
func makeReconcileManager(t *testing.T, reconcileWindow time.Duration) (*Manager, *recordingSink) {
	t.Helper()
	sink := &recordingSink{}
	mgr := NewManager(&stubStrategy{name: "s"},
		WithNodeID("node-self"),
		WithEventSink(sink),
		WithReconcileWindow(reconcileWindow),
	)
	mgr.Start(context.Background())
	t.Cleanup(mgr.Stop)
	return mgr, sink
}

// TestReconcileAfterWin_StrictlyBetterRivalYields drives reconcileAfterWin
// with a rival claim that is strictly better than the local decision under
// isBetter. The node must yield: EmitYielded fires exactly once, naming the
// rival as winner, and the function returns before the window elapses.
func TestReconcileAfterWin_StrictlyBetterRivalYields(t *testing.T) {
	mgr, sink := makeReconcileManager(t, 5*time.Second)

	// Local decision: score 50, intended publish at t=200.
	decision := Decision{Eligible: true, Score: 50, PublishAt: time.UnixMicro(200)}
	req := Request{CapsuleID: capsule.CapsuleID("cap-y"), ReplicaID: "0", ClusterPath: "test/dc1/c"}

	// Rival is strictly better: higher score.
	claimsCh := make(chan *electionpb.Claim, 1)
	claimsCh <- &electionpb.Claim{NodeId: "node-rival", GravityScore: 90, TimestampMicros: 200}

	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.reconcileAfterWin(context.Background(), req, decision, claimsCh)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcileAfterWin did not return after a strictly-better rival (should yield immediately)")
	}

	if got := sink.yieldedCount(); got != 1 {
		t.Fatalf("expected exactly 1 yield, got %d", got)
	}
	if winners := sink.yieldWinnerList(); len(winners) != 1 || winners[0] != "node-rival" {
		t.Fatalf("yield winner = %v, want [node-rival]", winners)
	}
}

// TestReconcileAfterWin_StrictlyWorseRivalDurableWinner drives
// reconcileAfterWin with a rival that is strictly WORSE. The node must NOT
// yield: it drains the window and returns as the durable winner.
func TestReconcileAfterWin_StrictlyWorseRivalDurableWinner(t *testing.T) {
	mgr, sink := makeReconcileManager(t, 60*time.Millisecond)

	decision := Decision{Eligible: true, Score: 90, PublishAt: time.UnixMicro(200)}
	req := Request{CapsuleID: capsule.CapsuleID("cap-w"), ReplicaID: "0", ClusterPath: "test/dc1/c"}

	// Rival is strictly worse: lower score. Feed several to ensure the loop
	// keeps draining without yielding.
	claimsCh := make(chan *electionpb.Claim, 4)
	claimsCh <- &electionpb.Claim{NodeId: "node-rival", GravityScore: 10, TimestampMicros: 100}
	claimsCh <- &electionpb.Claim{NodeId: "node-rival2", GravityScore: 20, TimestampMicros: 50}

	start := time.Now()
	mgr.reconcileAfterWin(context.Background(), req, decision, claimsCh)
	elapsed := time.Since(start)

	if got := sink.yieldedCount(); got != 0 {
		t.Fatalf("strictly-worse rivals must not cause a yield, got %d yields", got)
	}
	// The window (60ms) must have elapsed — the durable winner path is the
	// timeout branch, not an early return.
	if elapsed < 50*time.Millisecond {
		t.Fatalf("reconcile returned in %v; expected it to drain the ~60ms window", elapsed)
	}
}

// TestReconcileAfterWin_ContextCancelReturns confirms a cancelled context
// ends the reconcile promptly without yielding.
func TestReconcileAfterWin_ContextCancelReturns(t *testing.T) {
	mgr, sink := makeReconcileManager(t, 10*time.Second)

	decision := Decision{Eligible: true, Score: 50, PublishAt: time.UnixMicro(200)}
	req := Request{CapsuleID: capsule.CapsuleID("cap-c"), ReplicaID: "0", ClusterPath: "test/dc1/c"}
	claimsCh := make(chan *electionpb.Claim) // never fed

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.reconcileAfterWin(ctx, req, decision, claimsCh)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("reconcileAfterWin did not return on context cancel")
	}
	if got := sink.yieldedCount(); got != 0 {
		t.Fatalf("context cancel must not yield, got %d", got)
	}
}

// --- Test 2: A/B symmetry invariant (both-yield-impossible / nobody-yield-impossible) ---

// TestReconcile_ABSymmetry_ExactlyOneYields is the liveness-crux proof:
// two decisions with equal score but different intended PublishAt (A
// earlier, B later). Running A's reconcile fed B's published claim and B's
// reconcile fed A's published claim — through the REAL isBetter — must
// yield EXACTLY ONE outcome: A does NOT yield (it is the true winner) AND
// B DOES yield (it is the true loser). Both-yield and nobody-yield are
// impossible under the O14 strict total order; this test encodes that.
func TestReconcile_ABSymmetry_ExactlyOneYields(t *testing.T) {
	// A has a smaller offset (higher priority) → A is strictly better on the
	// O14b offset tiebreak; scores equal.
	const score = 50.0
	aDecision := Decision{Eligible: true, Score: score, Offset: 1000 * time.Microsecond}
	bDecision := Decision{Eligible: true, Score: score, Offset: 2000 * time.Microsecond}

	// The wire claims each node PUBLISHED (OffsetMicros == its own offset, O14b).
	aClaim := &electionpb.Claim{NodeId: "node-a", GravityScore: score, OffsetMicros: 1000}
	bClaim := &electionpb.Claim{NodeId: "node-b", GravityScore: score, OffsetMicros: 2000}

	reqA := Request{CapsuleID: capsule.CapsuleID("cap"), ReplicaID: "0", ClusterPath: "test/dc1/c"}
	reqB := reqA

	// A's reconcile fed B's claim.
	mgrA, sinkA := makeReconcileManager(t, 200*time.Millisecond)
	mgrA.nodeID = "node-a"
	chA := make(chan *electionpb.Claim, 1)
	chA <- bClaim
	mgrA.reconcileAfterWin(context.Background(), reqA, aDecision, chA)

	// B's reconcile fed A's claim.
	mgrB, sinkB := makeReconcileManager(t, 200*time.Millisecond)
	mgrB.nodeID = "node-b"
	chB := make(chan *electionpb.Claim, 1)
	chB <- aClaim
	mgrB.reconcileAfterWin(context.Background(), reqB, bDecision, chB)

	// The decisive invariant: A did NOT yield, B DID yield.
	if sinkA.yieldedCount() != 0 {
		t.Fatalf("true winner A must NOT yield, got %d yields", sinkA.yieldedCount())
	}
	if sinkB.yieldedCount() != 1 {
		t.Fatalf("true loser B must yield exactly once, got %d", sinkB.yieldedCount())
	}
	if w := sinkB.yieldWinnerList(); len(w) != 1 || w[0] != "node-a" {
		t.Fatalf("B must yield to A, got winners %v", w)
	}

	// Confirm the both-yield-impossible property holds via the raw comparator
	// too: exactly one direction of isBetter is true for distinct claims.
	aBetterThanB := isBetter(aClaim, bDecision, "node-b")
	bBetterThanA := isBetter(bClaim, aDecision, "node-a")
	if aBetterThanB == bBetterThanA {
		t.Fatalf("isBetter is not antisymmetric here: A>B=%v B>A=%v (must differ)",
			aBetterThanB, bBetterThanA)
	}
}

// --- Test 3: integration — inject propagation delay > window → double-winner reproduced ---

// connectedPubSubPair starts two loopback libp2p hosts with gossipsub,
// connects them, and returns both. Used to reproduce the real double-winner
// with a controllable per-message publish delay.
func connectedPubSubPair(t *testing.T) (host.Host, *pubsub.PubSub, host.Host, *pubsub.PubSub) {
	t.Helper()
	h1, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("libp2p.New h1: %v", err)
	}
	t.Cleanup(func() { _ = h1.Close() })
	h2, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("libp2p.New h2: %v", err)
	}
	t.Cleanup(func() { _ = h2.Close() })

	ps1, err := pubsub.NewGossipSub(context.Background(), h1)
	if err != nil {
		t.Fatalf("NewGossipSub h1: %v", err)
	}
	ps2, err := pubsub.NewGossipSub(context.Background(), h2)
	if err != nil {
		t.Fatalf("NewGossipSub h2: %v", err)
	}

	if err := h1.Connect(context.Background(), peer.AddrInfo{ID: h2.ID(), Addrs: h2.Addrs()}); err != nil {
		t.Fatalf("connect h1->h2: %v", err)
	}
	return h1, ps1, h2, ps2
}

// TestO14c_InjectDelay_DoubleWinner_ExactlyOneYields is the integration
// repro. Two managers on connected gossipsub hosts both run an election for
// the same (capsule, replica) with the SAME score. The tiebreak window is
// tiny (1ms) so BOTH report Won before either hears the other (double-
// winner reproduced), and the reconcile window is generous (2s) so the
// cross-delivered claim lands DURING reconcile. Assertions:
//   - both momentarily Won (double-winner),
//   - after reconcile EXACTLY ONE is a durable winner and the other yielded,
//   - the yield names the isBetter winner.
func TestO14c_InjectDelay_DoubleWinner_ExactlyOneYields(t *testing.T) {
	h1, ps1, h2, ps2 := connectedPubSubPair(t)
	id1, id2 := h1.ID().String(), h2.ID().String()

	// Cross-trust verifier: each manager verifies the other's claims by
	// looking up the peer's public key. A tiny in-test verifier keyed by
	// the two node IDs.
	pubKeys := map[string]crypto.PubKey{
		id1: h1.Peerstore().PubKey(h1.ID()),
		id2: h2.Peerstore().PubKey(h2.ID()),
	}
	verifier := &testPairVerifier{keys: pubKeys}

	newMgr := func(ps *pubsub.PubSub, h host.Host, tracker *bindingTracker) (*Manager, *recordingSink) {
		nodeID := h.ID().String()
		sink := &recordingSink{}
		store := newFakeStore()
		state := healthyNodeState(nodeID, 8, 16000)
		provider := &fakeProvider{state: state}
		// Fixed-score strategy so both nodes tie on score → the timestamp
		// tiebreak decides, and both publish quickly.
		calc := gravity.NewCalculator(gravity.WithCapsuleTargetLookup(tracker))
		lc := &recordingLifecycle{tracker: tracker, store: store}
		mgr := NewManager(&fixedScoreStrategy{score: 50, publishDelay: 0},
			WithNodeID(nodeID),
			WithCapsuleStore(store),
			WithLifecycleController(lc),
			WithCalculator(calc),
			WithStateProvider(provider),
			WithPubSub(ps),
			WithEventSink(sink),
			WithSigner(&hostSigner{h: h}),
			WithVerifier(verifier),
			WithElectionTimeout(3*time.Second),
			WithTiebreakWindow(1*time.Millisecond),  // tiny → both win before hearing each other
			WithReconcileWindow(2*time.Second),       // generous → cross-claim lands in reconcile
			WithPublishTimeout(1*time.Second),
		)
		mgr.Start(context.Background())
		t.Cleanup(mgr.Stop)
		return mgr, sink
	}

	tracker1 := newBindingTracker()
	tracker2 := newBindingTracker()
	mgr1, sink1 := newMgr(ps1, h1, tracker1)
	mgr2, sink2 := newMgr(ps2, h2, tracker2)

	clusterPath := "test/dc1/c"
	if err := mgr1.JoinCluster(clusterPath); err != nil {
		t.Fatalf("mgr1 JoinCluster: %v", err)
	}
	if err := mgr2.JoinCluster(clusterPath); err != nil {
		t.Fatalf("mgr2 JoinCluster: %v", err)
	}

	// Both stores must know the capsule (each manager reads its own store).
	c := makeReplicaCapsule(t, "dup-app")
	c.ClusterID = clusterPath
	mgr1.store.(*fakeStore).put(c)
	mgr2.store.(*fakeStore).put(c)

	// Let gossipsub form the topic mesh before publishing so claims
	// actually cross. Peer connectivity alone is not sufficient — the
	// per-topic mesh grafts on a gossipsub heartbeat (~1s). Probe actual
	// cross-delivery: publish a warm-up claim from mgr1 on a throwaway
	// (capsule, replica) and wait until mgr2's listener receives it. This
	// deterministically gates on the transport being live, no blind sleep.
	waitFor(t, 3*time.Second, "gossipsub peers not connected", func() bool {
		return len(h1.Network().Peers()) > 0 && len(h2.Network().Peers()) > 0
	})
	warmClaims2, _, warmCancel2 := mgr2.topics[clusterPath].Listen("warmup", "0", 4)
	crossed := make(chan struct{})
	go func() {
		for range warmClaims2 {
			select {
			case <-crossed:
			default:
				close(crossed)
			}
			return
		}
	}()
	warmDeadline := time.Now().Add(5 * time.Second)
	for {
		_ = mgr1.topics[clusterPath].PublishClaim(context.Background(), &electionpb.Claim{
			CapsuleId: "warmup", ReplicaId: "0", ClusterPath: clusterPath,
			NodeId: id1, GravityScore: 1, TimestampMicros: 1,
		})
		select {
		case <-crossed:
		case <-time.After(200 * time.Millisecond):
		}
		if isClosed(crossed) {
			break
		}
		if time.Now().After(warmDeadline) {
			warmCancel2()
			t.Fatal("gossipsub topic mesh never delivered a cross-node claim")
		}
	}
	warmCancel2()

	req := Request{CapsuleID: c.ID, ReplicaID: "0", ClusterPath: clusterPath, Reason: ReasonEnum.Initial(), CreatedAt: time.Now()}
	if err := mgr1.HandleRequest(req); err != nil {
		t.Fatalf("mgr1 HandleRequest: %v", err)
	}
	if err := mgr2.HandleRequest(req); err != nil {
		t.Fatalf("mgr2 HandleRequest: %v", err)
	}

	// Both momentarily reach Won (double-winner reproduced): the tiny
	// tiebreak window means neither hears the other before reporting Won.
	waitFor(t, 3*time.Second, "double-winner not reproduced (both must report Won)", func() bool {
		return sink1.wonCount() >= 1 && sink2.wonCount() >= 1
	})

	// After reconcile: EXACTLY ONE yields (the isBetter loser), the other is
	// the durable winner. The winner is whichever published the earlier
	// intended timestamp (or, on a tie, the smaller nodeID).
	waitFor(t, 4*time.Second, "exactly one node did not yield after reconcile", func() bool {
		return sink1.yieldedCount()+sink2.yieldedCount() == 1
	})

	// Give the (durable) winner's reconcile window room to close so a late
	// spurious second yield would surface. Poll that the total stays 1.
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if total := sink1.yieldedCount() + sink2.yieldedCount(); total != 1 {
			t.Fatalf("exactly one node must yield; got %d total yields", total)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The yield winner (as reported by whichever node yielded) must be the
	// other node — the durable winner.
	var yielder, durable *recordingSink
	var durableID string
	if sink1.yieldedCount() == 1 {
		yielder, durable, durableID = sink1, sink2, id2
	} else {
		yielder, durable, durableID = sink2, sink1, id1
	}
	if w := yielder.yieldWinnerList(); len(w) != 1 || w[0] != durableID {
		t.Fatalf("yielder must name the durable winner %s, got %v", durableID, w)
	}
	if durable.yieldedCount() != 0 {
		t.Fatalf("durable winner must not yield, got %d", durable.yieldedCount())
	}
}

// TestO14c_DelayBeyondReconcileWindow_BothRemainWinners encodes the
// documented boundary: when propagation delay exceeds the reconcile window,
// neither node observes the other's claim in time, so BOTH remain winners
// until membership (SWIM) heals — election cannot fix this. We model the
// delay by running each reconcile with a channel that never delivers within
// the (short) reconcile window; both must close their windows as durable
// winners, zero yields.
func TestO14c_DelayBeyondReconcileWindow_BothRemainWinners(t *testing.T) {
	const score = 50.0
	aDecision := Decision{Eligible: true, Score: score, PublishAt: time.UnixMicro(1000)}
	bDecision := Decision{Eligible: true, Score: score, PublishAt: time.UnixMicro(2000)}
	req := Request{CapsuleID: capsule.CapsuleID("cap"), ReplicaID: "0", ClusterPath: "test/dc1/c"}

	// Short reconcile window; feed no claim within it (delay > window).
	mgrA, sinkA := makeReconcileManager(t, 40*time.Millisecond)
	mgrA.nodeID = "node-a"
	mgrB, sinkB := makeReconcileManager(t, 40*time.Millisecond)
	mgrB.nodeID = "node-b"

	// Channels that would deliver the rival — but only AFTER the window.
	chA := make(chan *electionpb.Claim, 1)
	chB := make(chan *electionpb.Claim, 1)

	mgrA.reconcileAfterWin(context.Background(), req, aDecision, chA)
	mgrB.reconcileAfterWin(context.Background(), req, bDecision, chB)

	// Both closed their windows without a rival → both remain winners.
	if sinkA.yieldedCount() != 0 || sinkB.yieldedCount() != 0 {
		t.Fatalf("delay > reconcile window: neither node may yield (both remain winners); got A=%d B=%d",
			sinkA.yieldedCount(), sinkB.yieldedCount())
	}
}

// --- Test 4: group twin ---

// TestReconcileGroupAfterWin_StrictlyBetterRivalYields is the group unit
// twin of Test 1: a strictly-better rival group claim during the reconcile
// window makes the local node yield (EmitGroupYielded once, naming the
// rival) AND re-point the group's capacity reservation to the winner via
// the manager's own recordReservation — the reservation node ID must flip
// to the rival, and the reservation must survive (not be cleared).
func TestReconcileGroupAfterWin_StrictlyBetterRivalYields(t *testing.T) {
	state := healthyNodeState("placeholder", 8, 16000)
	mgr, sink, store := makeGroupManager(t, state, WithReconcileWindow(5*time.Second))

	members := []*capsule.Capsule{
		memberCapsule(t, "db", 2, 1024),
		memberCapsule(t, "api", 1, 512),
	}
	for _, m := range members {
		store.put(m)
	}
	groupID := capsule.NewCapsuleID()
	req := GroupClaimRequest{
		GroupID:     groupID,
		MemberIDs:   []capsule.CapsuleID{members[0].ID, members[1].ID},
		ClusterPath: state.ClusterPath,
		Reason:      ReasonEnum.Initial(),
	}

	// Pre-condition: the local node holds the reservation (it just Won).
	mgr.recordReservation(req, mgr.nodeID, 50)
	if mgr.ReservationNodeID(groupID) != mgr.nodeID {
		t.Fatalf("precondition: reservation should be held by local node")
	}

	// Local offset larger than the rival's → rival is strictly better on the
	// O14b offset tiebreak (equal score). TimestampMicros is set to the
	// OPPOSITE ordering to prove the tiebreak ignores it.
	offset := 2000 * time.Microsecond
	claimsCh := make(chan *electionpb.GroupClaim, 1)
	claimsCh <- &electionpb.GroupClaim{
		GroupId: string(groupID), NodeId: "node-rival", GravityScore: 50,
		OffsetMicros: 1000, TimestampMicros: 9999, // smaller offset wins; timestamp ignored
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.reconcileGroupAfterWin(context.Background(), req, 50, offset, claimsCh)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcileGroupAfterWin did not return after a strictly-better rival")
	}

	yielded := sink.yieldedEvents()
	if len(yielded) != 1 || yielded[0].NodeID != "node-rival" {
		t.Fatalf("expected exactly 1 group yield to node-rival, got %v", yielded)
	}
	// The reservation must be re-pointed to the winner, and still present
	// (single atomic re-record, no spurious clear → no watchdog GroupClaimFailed).
	if got := mgr.ReservationNodeID(groupID); got != "node-rival" {
		t.Fatalf("reservation node = %q, want re-pointed to winner node-rival", got)
	}
	if !mgr.HasReservation(groupID) {
		t.Fatal("reservation must survive the yield (re-pointed, not cleared)")
	}
}

// TestReconcileGroupAfterWin_StrictlyWorseRivalDurable is the group twin of
// Test 1's durable-winner arm: a strictly-worse rival does NOT cause a
// yield; the reservation stays on the local node.
func TestReconcileGroupAfterWin_StrictlyWorseRivalDurable(t *testing.T) {
	state := healthyNodeState("placeholder", 8, 16000)
	mgr, sink, store := makeGroupManager(t, state, WithReconcileWindow(60*time.Millisecond))

	members := []*capsule.Capsule{memberCapsule(t, "db", 2, 1024)}
	store.put(members[0])
	groupID := capsule.NewCapsuleID()
	req := GroupClaimRequest{
		GroupID: groupID, MemberIDs: []capsule.CapsuleID{members[0].ID},
		ClusterPath: state.ClusterPath, Reason: ReasonEnum.Initial(),
	}
	mgr.recordReservation(req, mgr.nodeID, 90)

	offset := 1000 * time.Microsecond
	claimsCh := make(chan *electionpb.GroupClaim, 1)
	// Rival is strictly worse: lower score (offset/timestamp irrelevant).
	claimsCh <- &electionpb.GroupClaim{GroupId: string(groupID), NodeId: "node-rival", GravityScore: 10, OffsetMicros: 100, TimestampMicros: 500}

	mgr.reconcileGroupAfterWin(context.Background(), req, 90, offset, claimsCh)

	if got := len(sink.yieldedEvents()); got != 0 {
		t.Fatalf("strictly-worse rival must not cause a group yield, got %d", got)
	}
	if got := mgr.ReservationNodeID(groupID); got != mgr.nodeID {
		t.Fatalf("durable winner: reservation must stay on local node, got %q", got)
	}
}
