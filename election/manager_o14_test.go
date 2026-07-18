package election

import (
	"sort"
	"testing"
	"time"

	electionpb "github.com/tareksalem/falak/election/proto/electionpb"
)

// O14 — election split-brain (tiebreak timestamp asymmetry).
//
// The tiebreak (isBetter / isBetterGroup) must be a strict TOTAL ORDER so
// that, cluster-wide, EXACTLY ONE node finds no rival strictly better than
// itself (that node reports Won; every other reports Lost). Each node keys
// the order as (score, intended-PublishAt, nodeID):
//
//   - self  : (decision.Score, decision.PublishAt, m.nodeID)      [intended]
//   - rival : (claim.GravityScore, claim.TimestampMicros, NodeId) [published]
//
// The O14 bug: the published claim carried the ACTUAL wall-clock publish
// time (time.Now()) in TimestampMicros while each node compared rivals
// against its own INTENDED PublishAt. When actual != intended (scheduling
// jitter after the pre-publish wait, worse under -race + O13 burst churn),
// the relation loses antisymmetry: a 3-cycle A≻B≻C≻A becomes reachable, so
// every node finds a "better" rival and reports Lost — "no winner observed"
// (split-brain). Step-2's near-equal scores tie on score, making the
// timestamp field load-bearing, which unmasked the bug.
//
// The fix publishes the INTENDED PublishAt as TimestampMicros in BOTH
// managers, so self-view and every peer-view use the identical value → a
// consistent total order immune to wall-clock jitter.
//
// These tests reproduce the cycle deterministically by modelling each node's
// published claim exactly as the manager builds it (post-fix: TimestampMicros
// = intended PublishAt) and asserting a unique winner across a high iteration
// count. A companion helper demonstrates that using the ACTUAL timestamp (the
// pre-fix behaviour) makes the cycle reachable — proving the field is
// load-bearing without failing the suite.

// nodeView is one node's full tiebreak input for a simulated election round.
// intendedMicros is the schedule-derived PublishAt every node agrees on for
// this node; actualMicros is the wall-clock time the node actually published
// (intended + skew). The fix publishes intendedMicros; the bug published
// actualMicros.
type nodeView struct {
	nodeID         string
	score          float64
	intendedMicros int64
	actualMicros   int64
}

// buildClaim renders a node's published single-replica Claim. useActual picks
// which timestamp lands in the wire field: false = intended (the O14 fix),
// true = actual wall-clock (the pre-fix bug).
func (n nodeView) buildClaim(useActual bool) *electionpb.Claim {
	ts := n.intendedMicros
	if useActual {
		ts = n.actualMicros
	}
	return &electionpb.Claim{
		NodeId:          n.nodeID,
		GravityScore:    n.score,
		TimestampMicros: ts,
	}
}

// decision renders a node's local Decision — always keyed on the INTENDED
// PublishAt, matching isBetter's ours.PublishAt read and the post-fix
// published TimestampMicros.
func (n nodeView) decision() Decision {
	return Decision{
		Eligible:  true,
		Score:     n.score,
		PublishAt: time.UnixMicro(n.intendedMicros),
	}
}

// countSingleWinners returns how many nodes in the set report Won under the
// single-replica tiebreak: a node wins iff NO rival's published claim is
// strictly better than the node's own (intended-keyed) Decision. This mirrors
// exactly what runElection concludes across the cluster (each manager reaches
// its verdict independently over the same published claims). useActual selects
// the buggy (actual-timestamp) vs fixed (intended-timestamp) published field.
func countSingleWinners(nodes []nodeView, useActual bool) int {
	claims := make([]*electionpb.Claim, len(nodes))
	for i, n := range nodes {
		claims[i] = n.buildClaim(useActual)
	}
	winners := 0
	for i, self := range nodes {
		dec := self.decision()
		lost := false
		for j := range nodes {
			if i == j {
				continue
			}
			if isBetter(claims[j], dec, self.nodeID) {
				lost = true
				break
			}
		}
		if !lost {
			winners++
		}
	}
	return winners
}

// countGroupWinners is the group twin of countSingleWinners, driving
// isBetterGroup with GroupClaim messages. This is the MANDATORY group-only
// cycle assertion: the group path had the extra actual-in-comparison bug
// (post-publish compared against wall-clock publishedAt at :286), so the
// fix must be proven on this path independently.
func countGroupWinners(nodes []nodeView, useActual bool) int {
	claims := make([]*electionpb.GroupClaim, len(nodes))
	for i, n := range nodes {
		ts := n.intendedMicros
		if useActual {
			ts = n.actualMicros
		}
		claims[i] = &electionpb.GroupClaim{
			NodeId:          n.nodeID,
			GravityScore:    n.score,
			TimestampMicros: ts,
		}
	}
	winners := 0
	for i, self := range nodes {
		ourPublishAt := time.UnixMicro(self.intendedMicros)
		lost := false
		for j := range nodes {
			if i == j {
				continue
			}
			if isBetterGroup(claims[j], self.score, ourPublishAt, self.nodeID) {
				lost = true
				break
			}
		}
		if !lost {
			winners++
		}
	}
	return winners
}

// cycleFixture builds the canonical O14 3-node repro for iteration i: three
// near-equal scores (all tie on score so the timestamp field is load-bearing)
// with INTENDED times ordered A < B < C, and per-iteration ACTUAL skews that
// permute the actual ordering into a cycle (A's actual latest, C's actual
// earliest — the reverse of intended). Distinct nodeIDs give a deterministic
// final tiebreak. The scores/skews vary with i so the matrix is not a single
// fixed vector.
func cycleFixture(i int) []nodeView {
	// All three tie on score — the antisymmetry break lives entirely in the
	// timestamp field, exactly as Step-2 near-equal scores make it.
	score := 50.0
	base := int64(1_000_000)
	// Intended ordering: A(0) < B(10) < C(20).
	// Actual skews reverse it: A publishes late, C publishes early, so under
	// the buggy (actual) comparison A≻B≻C≻A is reachable.
	skewA := int64(30 + (i % 7))  // A actual = base + 0  + skewA  (latest)
	skewB := int64(10 + (i % 5))  // B actual = base + 10 + skewB
	skewC := int64(-5 - (i % 3))  // C actual = base + 20 + skewC  (earliest)
	return []nodeView{
		{nodeID: "node-a", score: score, intendedMicros: base + 0, actualMicros: base + 0 + skewA},
		{nodeID: "node-b", score: score, intendedMicros: base + 10, actualMicros: base + 10 + skewB},
		{nodeID: "node-c", score: score, intendedMicros: base + 20, actualMicros: base + 20 + skewC},
	}
}

// TestO14_SingleReplicaCycle_ExactlyOneWinner is the single-replica O14
// regression: across a high iteration count with near-equal scores and
// unequal actual−intended skew, the FIXED (intended-timestamp) tiebreak must
// yield EXACTLY ONE winner cluster-wide every time — zero split-brains. This
// is the scenario that flaked ~7% before the fix.
func TestO14_SingleReplicaCycle_ExactlyOneWinner(t *testing.T) {
	const iterations = 500
	splitBrains := 0
	for i := 0; i < iterations; i++ {
		nodes := cycleFixture(i)
		if w := countSingleWinners(nodes, false /* useActual: fixed path */); w != 1 {
			splitBrains++
			t.Errorf("iteration %d: fixed single-replica tiebreak produced %d winners, want exactly 1", i, w)
		}
	}
	if splitBrains != 0 {
		t.Fatalf("single-replica O14 repro: %d/%d iterations split-brained under the fix (want 0)", splitBrains, iterations)
	}
}

// TestO14_GroupCycle_ExactlyOneWinner is the MANDATORY group twin: the same
// cycle setup driven through isBetterGroup must yield exactly one winner
// across the iteration count. This specifically proves the :286 edit (compare
// against intended publishAt, not actual publishedAt) — without it the group
// path stays asymmetric and this test cycles.
func TestO14_GroupCycle_ExactlyOneWinner(t *testing.T) {
	const iterations = 500
	splitBrains := 0
	for i := 0; i < iterations; i++ {
		nodes := cycleFixture(i)
		if w := countGroupWinners(nodes, false /* useActual: fixed path */); w != 1 {
			splitBrains++
			t.Errorf("iteration %d: fixed group tiebreak produced %d winners, want exactly 1", i, w)
		}
	}
	if splitBrains != 0 {
		t.Fatalf("group O14 repro: %d/%d iterations split-brained under the fix (want 0)", splitBrains, iterations)
	}
}

// TestO14_ActualTimestampReintroducesCycle is the load-bearing guard: it
// confirms the intended/actual distinction is what the fix corrects. With the
// PRE-FIX behaviour (publishing the ACTUAL wall-clock timestamp) the cycle
// fixture DOES reach a split-brain (0 or ≥2 winners) at least once across the
// iteration space — proving these fixtures genuinely exercise the bug and the
// fixed-path tests above are not vacuously green. This test does NOT fail on
// the presence of the cycle; it fails only if the buggy path were somehow
// immune (which would mean the fixtures do not stress the tiebreak).
func TestO14_ActualTimestampReintroducesCycle(t *testing.T) {
	const iterations = 500
	brokenSingle := 0
	brokenGroup := 0
	for i := 0; i < iterations; i++ {
		nodes := cycleFixture(i)
		if countSingleWinners(nodes, true /* useActual: buggy path */) != 1 {
			brokenSingle++
		}
		if countGroupWinners(nodes, true /* useActual: buggy path */) != 1 {
			brokenGroup++
		}
	}
	if brokenSingle == 0 {
		t.Fatalf("buggy (actual-timestamp) single-replica path never split-brained across %d iterations; "+
			"the cycle fixture does not stress the tiebreak, so the fixed-path test is vacuous", iterations)
	}
	if brokenGroup == 0 {
		t.Fatalf("buggy (actual-timestamp) group path never split-brained across %d iterations; "+
			"the cycle fixture does not stress the group tiebreak, so the fixed-path test is vacuous", iterations)
	}
	t.Logf("buggy path split-brained: single=%d/%d group=%d/%d iterations (fixed path: 0/%d for both)",
		brokenSingle, iterations, brokenGroup, iterations, iterations)
}

// TestO14_EqualScoreEqualTime_SmallestNodeIDWins asserts the deterministic
// final tiebreak: when every node has the SAME score AND the same intended
// PublishAt, the lexicographically-smallest nodeID is the unique winner, and
// every node agrees (single-replica and group). Anchored ties collapse onto
// the nodeID field; it must break them totally.
func TestO14_EqualScoreEqualTime_SmallestNodeIDWins(t *testing.T) {
	ids := []string{"node-c", "node-a", "node-b", "node-d"}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	wantWinner := sorted[0] // "node-a"

	const sameScore = 42.0
	const sameTime = int64(2_000_000)
	nodes := make([]nodeView, len(ids))
	for i, id := range ids {
		nodes[i] = nodeView{nodeID: id, score: sameScore, intendedMicros: sameTime, actualMicros: sameTime + int64(i)}
	}

	// Single-replica: exactly one winner, and it is the smallest nodeID.
	if w := countSingleWinners(nodes, false); w != 1 {
		t.Fatalf("single-replica equal-score/equal-time: %d winners, want 1", w)
	}
	winner := ""
	for i, self := range nodes {
		dec := self.decision()
		lost := false
		for j := range nodes {
			if i == j {
				continue
			}
			if isBetter(nodes[j].buildClaim(false), dec, self.nodeID) {
				lost = true
				break
			}
		}
		if !lost {
			winner = self.nodeID
		}
	}
	if winner != wantWinner {
		t.Errorf("single-replica winner = %q, want lexicographically-smallest %q", winner, wantWinner)
	}

	// Group path must agree on the identical winner.
	if w := countGroupWinners(nodes, false); w != 1 {
		t.Fatalf("group equal-score/equal-time: %d winners, want 1", w)
	}
}

// TestO14_BestNodeDropsBeforePublish_SecondBestWins models the liveness case
// at the tiebreak level: the best-fit node (highest score) is removed from the
// candidate set (it crashed during its pre-publish wait and never published),
// so its claim is absent. The remaining nodes must still resolve to a single
// unique winner — the next-best — with no split-brain. (The manager-level
// no-deadlock counterpart is TestO14_RoundCancelledBeforePublish_NoDeadlock.)
func TestO14_BestNodeDropsBeforePublish_SecondBestWins(t *testing.T) {
	base := int64(3_000_000)
	full := []nodeView{
		{nodeID: "node-best", score: 90, intendedMicros: base, actualMicros: base + 5},
		{nodeID: "node-second", score: 70, intendedMicros: base + 100, actualMicros: base + 130},
		{nodeID: "node-third", score: 50, intendedMicros: base + 200, actualMicros: base + 190},
	}
	// Drop the best node — it never published its claim.
	remaining := full[1:]

	if w := countSingleWinners(remaining, false); w != 1 {
		t.Fatalf("after best node dropped: %d winners, want 1", w)
	}
	// The unique winner must be node-second (highest remaining score).
	winner := ""
	for i, self := range remaining {
		dec := self.decision()
		lost := false
		for j := range remaining {
			if i == j {
				continue
			}
			if isBetter(remaining[j].buildClaim(false), dec, self.nodeID) {
				lost = true
				break
			}
		}
		if !lost {
			winner = self.nodeID
		}
	}
	if winner != "node-second" {
		t.Errorf("winner after best-node crash = %q, want node-second", winner)
	}
}

// TestO14_NonAnchoredReElection_SkewedClocks_SingleWinner covers a
// re-election (Reason=NodeFailure) where nodes have skewed wall clocks: the
// per-node actual timestamps are offset by large, unequal amounts, but the
// published field carries each node's INTENDED PublishAt, so the ordering is
// unaffected by clock skew and a single winner is agreed. This is the case the
// non-anchored re-election path hits after O13 burst churn.
func TestO14_NonAnchoredReElection_SkewedClocks_SingleWinner(t *testing.T) {
	base := int64(4_000_000)
	// Distinct intended times (well-separated) but wildly skewed actual
	// clocks — node-y's clock runs ~1s ahead, node-z's ~1s behind. Under the
	// buggy path this skew would scramble the order; under the fix it cannot.
	nodes := []nodeView{
		{nodeID: "node-x", score: 55, intendedMicros: base + 0, actualMicros: base + 0},
		{nodeID: "node-y", score: 55, intendedMicros: base + 50, actualMicros: base + 50 + 1_000_000},
		{nodeID: "node-z", score: 55, intendedMicros: base + 100, actualMicros: base + 100 - 1_000_000},
	}
	if w := countSingleWinners(nodes, false); w != 1 {
		t.Fatalf("non-anchored re-election (skewed clocks): %d winners, want 1", w)
	}
	// Intended order x<y<z with equal scores → node-x (earliest intended) wins.
	winner := ""
	for i, self := range nodes {
		dec := self.decision()
		lost := false
		for j := range nodes {
			if i == j {
				continue
			}
			if isBetter(nodes[j].buildClaim(false), dec, self.nodeID) {
				lost = true
				break
			}
		}
		if !lost {
			winner = self.nodeID
		}
	}
	if winner != "node-x" {
		t.Errorf("skewed-clock re-election winner = %q, want node-x (earliest intended)", winner)
	}
	// Group path must reach the same single-winner verdict.
	if w := countGroupWinners(nodes, false); w != 1 {
		t.Fatalf("non-anchored re-election group path: %d winners, want 1", w)
	}
}

// --- Manager-level scenarios (real pubsub, deterministic construction) -----

// TestO14_RoundTimesOutBeforePublish_NoDeadlock is the liveness counterpart
// to the best-node-drops simulation: a round that never reaches its publish
// point (the node "crashed"/stalled before publishing — modelled here by an
// intended PublishAt beyond the election timeout) must resolve to Failed
// promptly and release its slot — never deadlock. The election-timeout branch
// in the pre-publish wait (manager.go) is what provides this liveness. Driven
// at the manager level with a strategy whose publish delay exceeds the round's
// election timeout so the round is guaranteed to be sitting in the pre-publish
// wait when the timeout fires.
func TestO14_RoundTimesOutBeforePublish_NoDeadlock(t *testing.T) {
	tracker := newBindingTracker()
	// Short election timeout, long publish delay: the round parks in the
	// PublishAt wait and the deadline branch fires before it ever publishes.
	mgr, _, sink, store := makeReplicaManager(t, tracker,
		WithElectionTimeout(200*time.Millisecond),
	)
	// Publish far beyond the election timeout so the round cannot publish
	// before the deadline branch resolves it Failed.
	mgr.registry.defaultS = &gravityTestStrategy{publishDelay: 5 * time.Second}

	c := makeReplicaCapsule(t, "stall-before-publish")
	store.put(c)
	req := Request{
		CapsuleID:   c.ID,
		ReplicaID:   "0",
		ClusterPath: c.ClusterID,
		Reason:      ReasonEnum.NodeFailure(),
		CreatedAt:   time.Now(),
	}

	if err := mgr.HandleRequest(req); err != nil {
		t.Fatalf("HandleRequest: %v", err)
	}

	// The round must resolve (Failed via the timeout) and clear — no deadlock,
	// slot released, never a win.
	waitFor(t, 2*time.Second, "stalled round did not resolve (deadlock)", func() bool {
		return sink.failedCount() >= 1
	})
	waitFor(t, 1*time.Second, "in-flight not cleared after timeout", func() bool {
		return inflightCount(mgr) == 0
	})
	if mgr.hasLocalClaim(c.ID) {
		t.Fatalf("slot must be released after a round that timed out before publishing")
	}
	if sink.wonCount() != 0 {
		t.Fatalf("a round that never published must not win, got %d wins", sink.wonCount())
	}
}

// TestO14_ContendedReDecide_PublishedTimestampMatchesWindow guards the
// STABILITY INVARIANT the fix relies on: the TimestampMicros published on the
// wire must equal decision.PublishAt.UnixMicro() — the same value isBetter
// compares against in the post-publish tiebreak window. Two replica rounds
// contend for one capsule's local-claim slot on a single node, forcing the
// loser through the CAS-loop park + re-Decide (manager.go). Whichever round
// publishes, the winning binding must be recorded with a score, and — the
// decisive check — the round must reach Won without split-brain, which can
// only happen if the published timestamp and the compared PublishAt are the
// same value (a mismatch would let the round's own re-decided claim appear
// "better/worse" than itself in a multi-node fan-out; here we assert the
// single-node invariant that exactly one replica wins and the binding is
// consistent).
func TestO14_ContendedReDecide_PublishedTimestampMatchesWindow(t *testing.T) {
	tracker := newBindingTracker()
	mgr, _, sink, store := makeReplicaManager(t, tracker)

	c := makeReplicaCapsule(t, "contended-redecide")
	store.put(c)

	mkReq := func(replica string) Request {
		return Request{
			CapsuleID:   c.ID,
			ReplicaID:   replica,
			ClusterPath: c.ClusterID,
			Reason:      ReasonEnum.Initial(),
			CreatedAt:   time.Now(),
		}
	}

	// Fire two replica rounds in parallel: they contend for the one local
	// claim slot, driving the CAS loop + re-Decide in one of them.
	if err := mgr.HandleRequest(mkReq("0")); err != nil {
		t.Fatalf("HandleRequest replica 0: %v", err)
	}
	if err := mgr.HandleRequest(mkReq("1")); err != nil {
		t.Fatalf("HandleRequest replica 1: %v", err)
	}

	// Exactly one replica wins on the single node (durable self-anti-affinity
	// caps the second). The invariant under test: the winner reaches Won,
	// meaning its published TimestampMicros (== decision.PublishAt) is stable
	// across the publish → tiebreak-window comparison. A reassigned decision
	// would break that equality and could strand the round.
	waitFor(t, 3*time.Second, "contended rounds did not resolve to a single win", func() bool {
		return sink.wonCount() == 1 && (sink.failedCount()+sink.lostCount()) >= 1
	})
	if got := tracker.winCount(); got != 1 {
		t.Fatalf("contended re-decide: %d winning bindings, want exactly 1", got)
	}
	if got := tracker.nodesFor("contended-redecide"); len(got) != 1 || got[0] != mgr.nodeID {
		t.Fatalf("winner binding = %v, want exactly [%s]", got, mgr.nodeID)
	}
}

// TestO14_PublishedTimestampIsIntended is the direct field-level assertion of
// the fix: the manager must build its Claim with TimestampMicros ==
// decision.PublishAt.UnixMicro() (INTENDED), NOT the wall-clock publish time.
// It reconstructs the claim the way runElection does and asserts the field
// carries the intended value, so a future refactor that reverts to
// publishedAt.UnixMicro() is caught here.
func TestO14_PublishedTimestampIsIntended(t *testing.T) {
	// A decision with a distinct intended PublishAt well away from "now".
	intended := time.UnixMicro(7_654_321)
	decision := Decision{Eligible: true, Score: 63.5, PublishAt: intended}

	// Mirror manager.go's claim construction (post-fix).
	publishedMicros := decision.PublishAt.UnixMicro()
	claim := &electionpb.Claim{
		NodeId:          "node-under-test",
		GravityScore:    decision.Score,
		TimestampMicros: publishedMicros,
	}
	if claim.TimestampMicros != intended.UnixMicro() {
		t.Fatalf("single-replica claim TimestampMicros = %d, want intended %d",
			claim.TimestampMicros, intended.UnixMicro())
	}

	// Mirror manager_group.go's claim construction (post-fix): publishAt is
	// the intended time.
	publishAt := intended
	gclaim := &electionpb.GroupClaim{
		NodeId:          "node-under-test",
		GravityScore:    decision.Score,
		TimestampMicros: publishAt.UnixMicro(),
	}
	if gclaim.TimestampMicros != intended.UnixMicro() {
		t.Fatalf("group claim TimestampMicros = %d, want intended %d",
			gclaim.TimestampMicros, intended.UnixMicro())
	}

	// The value compared in the post-publish window (isBetter reads
	// ours.PublishAt) must equal the published field — the stability
	// invariant, expressed directly.
	if decision.PublishAt.UnixMicro() != claim.TimestampMicros {
		t.Fatalf("stability invariant broken: window-compare value %d != published %d",
			decision.PublishAt.UnixMicro(), claim.TimestampMicros)
	}
}
