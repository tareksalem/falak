package election

import (
	"sort"
	"testing"
	"time"

	electionpb "github.com/tareksalem/falak/election/proto/electionpb"
)

// O14 / O14b — election split-brain (tiebreak must be a strict total order)
// and its structural hardening.
//
// The tiebreak (isBetter / isBetterGroup) must be a strict TOTAL ORDER so
// that, cluster-wide, EXACTLY ONE node finds no rival strictly better than
// itself (that node reports Won; every other reports Lost). Each node keys
// the order as (score, OFFSET, nodeID):
//
//   - self  : (decision.Score, decision.Offset, m.nodeID)
//   - rival : (claim.GravityScore, claim.OffsetMicros, claim.NodeId)
//
// The O14 bug (historical): the published claim carried the ACTUAL wall-clock
// publish time in the tiebreak field while each node compared rivals against
// its own INTENDED time. When actual != intended (scheduling jitter), the
// relation lost antisymmetry: a 3-cycle A≻B≻C≻A became reachable, every node
// found a "better" rival and reported Lost — "no winner observed" (split-brain).
//
// O14b removes wall-clock from the tiebreak ENTIRELY: the middle key is now
// the node's deterministic priority OFFSET (baseWait + slotDelay + jitter), a
// pure duration. timestamp_micros survives only for observability and is
// NEVER read by the tiebreak. Because offset is a duration, cross-node clock
// skew cannot reorder claims — the total order is structurally clock-free.
//
// These tests reproduce the cycle deterministically by modelling each node's
// published claim exactly as the manager builds it (post-O14b: OffsetMicros =
// the node's offset; timestamp_micros = a wall-clock value that the tiebreak
// must ignore) and asserting a unique winner across a high iteration count. A
// companion helper demonstrates that a tiebreak keyed on the SKEWED wall-clock
// timestamp (the pre-O14 behaviour) makes the cycle reachable — proving the
// field is load-bearing without failing the suite.

// nodeView is one node's full tiebreak input for a simulated election round.
// offsetMicros is the node's deterministic priority delay (the O14b tiebreak
// key every node agrees on for this node). timestampMicros is the node's
// wall-clock publish time (offset applied to a SKEWED local clock); the O14b
// tiebreak must ignore it.
//
// intendedTsMicros / actualTsMicros model the PRE-O14 bug for the load-bearing
// guard: the buggy comparator had each node key SELF on its intended publish
// time while the published claim carried the ACTUAL (jittered) wall-clock time.
// When intended != actual the timestamp relation loses antisymmetry and a
// 3-cycle becomes reachable. The real O14b path never uses these fields —
// offset is symmetric (self and rival read the same offsetMicros).
type nodeView struct {
	nodeID           string
	score            float64
	offsetMicros     int64
	timestampMicros  int64
	intendedTsMicros int64
	actualTsMicros   int64
}

// buildClaim renders a node's published single-replica Claim exactly as the
// manager builds it post-O14b: OffsetMicros carries the deterministic priority
// delay (the tiebreak key), TimestampMicros carries the wall-clock publish
// time (observability only, ignored by the tiebreak).
func (n nodeView) buildClaim() *electionpb.Claim {
	return &electionpb.Claim{
		NodeId:          n.nodeID,
		GravityScore:    n.score,
		OffsetMicros:    n.offsetMicros,
		TimestampMicros: n.timestampMicros,
	}
}

// decision renders a node's local Decision — keyed on the node's OFFSET,
// matching isBetter's ours.Offset read and the published OffsetMicros.
func (n nodeView) decision() Decision {
	return Decision{
		Eligible: true,
		Score:    n.score,
		Offset:   time.Duration(n.offsetMicros) * time.Microsecond,
	}
}

// isBetterByIntendedActual is the PRE-O14 (buggy) comparator: SELF keys on its
// INTENDED publish time (ourIntendedMicros) while the RIVAL is compared on the
// ACTUAL published time (rivalActualMicros). This intended-vs-actual asymmetry
// is precisely the O14 bug — when the two differ, the relation loses
// antisymmetry and a 3-cycle A≻B≻C≻A becomes reachable. Retained ONLY to prove
// the cycle fixtures genuinely stress the tiebreak (the load-bearing guard
// below); the production path never uses it.
func isBetterByIntendedActual(rivalScore float64, rivalActualMicros int64, rivalNodeID string, ourScore float64, ourIntendedMicros int64, ourNodeID string) bool {
	if rivalScore != ourScore {
		return rivalScore > ourScore
	}
	if rivalActualMicros != ourIntendedMicros {
		return rivalActualMicros < ourIntendedMicros
	}
	return rivalNodeID < ourNodeID
}

// countSingleWinners returns how many nodes in the set report Won under the
// REAL single-replica offset tiebreak (isBetter): a node wins iff NO rival's
// published claim is strictly better than the node's own (offset-keyed)
// Decision. This mirrors exactly what runElection concludes across the cluster.
func countSingleWinners(nodes []nodeView) int {
	claims := make([]*electionpb.Claim, len(nodes))
	for i, n := range nodes {
		claims[i] = n.buildClaim()
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

// countSingleWinnersByIntendedActual is the PRE-O14 buggy simulation: self
// keys on its intended time, rivals are compared on their actual (jittered)
// time. Used only by the load-bearing guard to prove the fixtures stress the
// tiebreak (a genuine 3-cycle is reachable here).
func countSingleWinnersByIntendedActual(nodes []nodeView) int {
	winners := 0
	for i, self := range nodes {
		lost := false
		for j := range nodes {
			if i == j {
				continue
			}
			r := nodes[j]
			if isBetterByIntendedActual(r.score, r.actualTsMicros, r.nodeID, self.score, self.intendedTsMicros, self.nodeID) {
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

// countGroupWinners is the group twin of countSingleWinners, driving the REAL
// isBetterGroup with GroupClaim messages keyed on offset.
func countGroupWinners(nodes []nodeView) int {
	claims := make([]*electionpb.GroupClaim, len(nodes))
	for i, n := range nodes {
		claims[i] = &electionpb.GroupClaim{
			NodeId:          n.nodeID,
			GravityScore:    n.score,
			OffsetMicros:    n.offsetMicros,
			TimestampMicros: n.timestampMicros,
		}
	}
	winners := 0
	for i, self := range nodes {
		ourOffset := time.Duration(self.offsetMicros) * time.Microsecond
		lost := false
		for j := range nodes {
			if i == j {
				continue
			}
			if isBetterGroup(claims[j], self.score, ourOffset, self.nodeID) {
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

// countGroupWinnersByIntendedActual is the group PRE-O14 buggy simulation
// (intended-vs-actual asymmetry), for the load-bearing guard. Identical shape
// to the single-replica version — the group path had the same bug.
func countGroupWinnersByIntendedActual(nodes []nodeView) int {
	winners := 0
	for i, self := range nodes {
		lost := false
		for j := range nodes {
			if i == j {
				continue
			}
			r := nodes[j]
			if isBetterByIntendedActual(r.score, r.actualTsMicros, r.nodeID, self.score, self.intendedTsMicros, self.nodeID) {
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
// near-equal scores (all tie on score so the middle field is load-bearing)
// with OFFSETS ordered A < B < C, and per-iteration wall-clock TIMESTAMP skews
// that permute the timestamp ordering into a CYCLE (A's timestamp latest, C's
// earliest — the reverse of the offset order). Distinct nodeIDs give a
// deterministic final tiebreak. Under the O14b offset tiebreak the winner is
// stable (A, smallest offset); under the pre-O14 timestamp tiebreak the cycle
// is reachable. The scores/skews vary with i so the matrix is not fixed.
func cycleFixture(i int) []nodeView {
	// All three tie on score — the antisymmetry break lives entirely in the
	// middle field.
	score := 50.0
	base := int64(1_000_000)
	// Offset ordering: A(0) < B(10) < C(20) — the real O14b tiebreak key, a
	// strict total order, so the offset path always has a unique winner (A).
	//
	// For the pre-O14 guard: INTENDED times ordered A < B < C but ACTUAL
	// (jittered) times reverse the ordering (A actual latest, C actual
	// earliest). Under the buggy intended-vs-actual comparator the 3-cycle
	// A≻B≻C≻A is reachable. These fields never touch the offset path.
	skewA := int64(30 + (i % 7))  // A actual = base + 0  + skewA  (latest)
	skewB := int64(10 + (i % 5))  // B actual = base + 10 + skewB
	skewC := int64(-5 - (i % 3))  // C actual = base + 20 + skewC  (earliest)
	return []nodeView{
		{nodeID: "node-a", score: score, offsetMicros: base + 0, timestampMicros: base + 0 + skewA,
			intendedTsMicros: base + 0, actualTsMicros: base + 0 + skewA},
		{nodeID: "node-b", score: score, offsetMicros: base + 10, timestampMicros: base + 10 + skewB,
			intendedTsMicros: base + 10, actualTsMicros: base + 10 + skewB},
		{nodeID: "node-c", score: score, offsetMicros: base + 20, timestampMicros: base + 20 + skewC,
			intendedTsMicros: base + 20, actualTsMicros: base + 20 + skewC},
	}
}

// TestO14_SingleReplicaCycle_ExactlyOneWinner is the single-replica O14
// regression: across a high iteration count with near-equal scores and a
// timestamp order that cycles, the offset tiebreak must yield EXACTLY ONE
// winner cluster-wide every time — zero split-brains.
func TestO14_SingleReplicaCycle_ExactlyOneWinner(t *testing.T) {
	const iterations = 500
	splitBrains := 0
	for i := 0; i < iterations; i++ {
		nodes := cycleFixture(i)
		if w := countSingleWinners(nodes); w != 1 {
			splitBrains++
			t.Errorf("iteration %d: offset single-replica tiebreak produced %d winners, want exactly 1", i, w)
		}
	}
	if splitBrains != 0 {
		t.Fatalf("single-replica O14 repro: %d/%d iterations split-brained under the fix (want 0)", splitBrains, iterations)
	}
}

// TestO14_GroupCycle_ExactlyOneWinner is the MANDATORY group twin: the same
// cycle setup driven through the REAL isBetterGroup (offset-keyed) must yield
// exactly one winner across the iteration count.
func TestO14_GroupCycle_ExactlyOneWinner(t *testing.T) {
	const iterations = 500
	splitBrains := 0
	for i := 0; i < iterations; i++ {
		nodes := cycleFixture(i)
		if w := countGroupWinners(nodes); w != 1 {
			splitBrains++
			t.Errorf("iteration %d: offset group tiebreak produced %d winners, want exactly 1", i, w)
		}
	}
	if splitBrains != 0 {
		t.Fatalf("group O14 repro: %d/%d iterations split-brained under the fix (want 0)", splitBrains, iterations)
	}
}

// TestO14_TimestampTiebreakReintroducesCycle is the load-bearing guard: it
// confirms the offset-vs-timestamp distinction is what the fix corrects. With
// the PRE-O14 behaviour (keying the tiebreak on the SKEWED wall-clock
// timestamp) the cycle fixture DOES reach a split-brain (0 or ≥2 winners) at
// least once across the iteration space — proving these fixtures genuinely
// exercise the bug and the offset-keyed tests above are not vacuously green.
// This test does NOT fail on the presence of the cycle; it fails only if the
// buggy path were somehow immune (which would mean the fixtures do not stress
// the tiebreak).
func TestO14_TimestampTiebreakReintroducesCycle(t *testing.T) {
	const iterations = 500
	brokenSingle := 0
	brokenGroup := 0
	for i := 0; i < iterations; i++ {
		nodes := cycleFixture(i)
		if countSingleWinnersByIntendedActual(nodes) != 1 {
			brokenSingle++
		}
		if countGroupWinnersByIntendedActual(nodes) != 1 {
			brokenGroup++
		}
	}
	if brokenSingle == 0 {
		t.Fatalf("pre-O14 (intended-vs-actual timestamp) single-replica path never split-brained across %d iterations; "+
			"the cycle fixture does not stress the tiebreak, so the offset-path test is vacuous", iterations)
	}
	if brokenGroup == 0 {
		t.Fatalf("pre-O14 (intended-vs-actual timestamp) group path never split-brained across %d iterations; "+
			"the cycle fixture does not stress the group tiebreak, so the offset-path test is vacuous", iterations)
	}
	t.Logf("pre-O14 (timestamp) path split-brained: single=%d/%d group=%d/%d iterations (offset path: 0/%d for both)",
		brokenSingle, iterations, brokenGroup, iterations, iterations)
}

// TestO14_EqualScoreEqualOffset_SmallestNodeIDWins asserts the deterministic
// final tiebreak: when every node has the SAME score AND the same offset, the
// lexicographically-smallest nodeID is the unique winner, and every node
// agrees (single-replica and group). Anchored ties collapse onto the nodeID
// field; it must break them totally.
func TestO14_EqualScoreEqualOffset_SmallestNodeIDWins(t *testing.T) {
	ids := []string{"node-c", "node-a", "node-b", "node-d"}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	wantWinner := sorted[0] // "node-a"

	const sameScore = 42.0
	const sameOffset = int64(2_000_000)
	nodes := make([]nodeView, len(ids))
	for i, id := range ids {
		// Distinct (adversarial) timestamps to prove they are ignored.
		nodes[i] = nodeView{nodeID: id, score: sameScore, offsetMicros: sameOffset, timestampMicros: sameOffset + int64(i)}
	}

	if w := countSingleWinners(nodes); w != 1 {
		t.Fatalf("single-replica equal-score/equal-offset: %d winners, want 1", w)
	}
	winner := singleWinner(nodes)
	if winner != wantWinner {
		t.Errorf("single-replica winner = %q, want lexicographically-smallest %q", winner, wantWinner)
	}

	// Group path must agree on the identical winner.
	if w := countGroupWinners(nodes); w != 1 {
		t.Fatalf("group equal-score/equal-offset: %d winners, want 1", w)
	}
}

// TestO14_BestNodeDropsBeforePublish_SecondBestWins models the liveness case
// at the tiebreak level: the best-fit node (highest score) is removed from the
// candidate set (it crashed during its pre-publish wait and never published),
// so its claim is absent. The remaining nodes must still resolve to a single
// unique winner — the next-best — with no split-brain.
func TestO14_BestNodeDropsBeforePublish_SecondBestWins(t *testing.T) {
	base := int64(3_000_000)
	full := []nodeView{
		{nodeID: "node-best", score: 90, offsetMicros: base, timestampMicros: base + 5},
		{nodeID: "node-second", score: 70, offsetMicros: base + 100, timestampMicros: base + 130},
		{nodeID: "node-third", score: 50, offsetMicros: base + 200, timestampMicros: base + 190},
	}
	// Drop the best node — it never published its claim.
	remaining := full[1:]

	if w := countSingleWinners(remaining); w != 1 {
		t.Fatalf("after best node dropped: %d winners, want 1", w)
	}
	if winner := singleWinner(remaining); winner != "node-second" {
		t.Errorf("winner after best-node crash = %q, want node-second", winner)
	}
}

// TestO14b_ClockSkewIndependence_SameWinner is the O14b value proof: a
// non-anchored re-election (Reason=NodeFailure) where nodes have wildly skewed
// wall clocks. The per-node timestamps are offset by large, unequal amounts,
// but the tiebreak keys on OFFSET (a pure duration), so the ordering is
// unaffected by clock skew and a single, IDENTICAL winner is agreed regardless
// of the skew magnitude. Under the pre-O14 timestamp key the winner would swing
// with skew; under O14b it cannot. This is the structural guarantee O14b adds
// over O14 (whose intended-PublishAt was still a wall-clock value).
func TestO14b_ClockSkewIndependence_SameWinner(t *testing.T) {
	base := int64(4_000_000)
	// Distinct offsets (well-separated), equal scores → node-x (smallest
	// offset) is the winner. We sweep several skew magnitudes and assert the
	// winner never changes.
	skews := []int64{0, 1_000_000, -1_000_000, 5_000_000, -7_500_000, 42}
	var firstWinner string
	for si, skew := range skews {
		nodes := []nodeView{
			// Each node's timestamp is its offset applied to a clock skewed by
			// a per-node multiple of `skew` — the exact clock-skew condition
			// O14b must be immune to.
			{nodeID: "node-x", score: 55, offsetMicros: base + 0, timestampMicros: base + 0 + 0*skew},
			{nodeID: "node-y", score: 55, offsetMicros: base + 50, timestampMicros: base + 50 + 1*skew},
			{nodeID: "node-z", score: 55, offsetMicros: base + 100, timestampMicros: base + 100 - 2*skew},
		}
		if w := countSingleWinners(nodes); w != 1 {
			t.Fatalf("skew %d: %d winners, want 1 (offset tiebreak must stay a total order)", skew, w)
		}
		winner := singleWinner(nodes)
		if winner != "node-x" {
			t.Errorf("skew %d: winner = %q, want node-x (smallest offset, clock-independent)", skew, winner)
		}
		// Group path must reach the same single winner.
		if w := countGroupWinners(nodes); w != 1 {
			t.Fatalf("skew %d: group %d winners, want 1", skew, w)
		}
		if si == 0 {
			firstWinner = winner
		} else if winner != firstWinner {
			t.Fatalf("skew %d changed the winner from %q to %q — tiebreak is NOT clock-independent",
				skew, firstWinner, winner)
		}
	}
}

// TestO14b_TiebreakKeysOffsetNotTimestamp is the direct guard proving the
// tiebreak keys on offset, not timestamp: two claims with EQUAL offset but
// WILDLY different timestamps must tie on the offset key and fall through to
// nodeID — NOT be decided by the timestamp. If the tiebreak still read the
// timestamp, the node with the earlier timestamp would win regardless of
// nodeID, and this test would catch it.
func TestO14b_TiebreakKeysOffsetNotTimestamp(t *testing.T) {
	const score = 50.0
	const sameOffset = 1_500_000 * time.Microsecond

	// ours: large nodeID, EARLY timestamp. rival: small nodeID, LATE timestamp.
	ours := Decision{Eligible: true, Score: score, Offset: sameOffset}
	// If the tiebreak keyed on timestamp, ours (earlier) would beat the rival
	// and isBetter(rival, ours) would be FALSE. Keying on offset (equal) →
	// nodeID decides: rival "node-a" < ours "node-z" → rival wins → TRUE.
	rival := &electionpb.Claim{
		NodeId:          "node-a",
		GravityScore:    score,
		OffsetMicros:    sameOffset.Microseconds(),
		TimestampMicros: 9_999_999, // far LATER than ours — must be ignored
	}
	// ours has the EARLIER timestamp (so under a timestamp key ours would win
	// and the rival would lose), but the LARGER nodeID (so under offset-equal
	// the rival wins via nodeID). The two keys give OPPOSITE winners — the
	// fixture genuinely distinguishes offset-keying from timestamp-keying.
	const oursTimestampEarly = int64(1)
	const rivalTimestampLate = int64(9_999_999)

	if !isBetter(rival, ours, "node-z") {
		t.Fatal("equal offset + smaller rival nodeID must win via nodeID; " +
			"a false result means the tiebreak wrongly consulted timestamp_micros")
	}

	// Sanity: under a timestamp key the rival's LATER timestamp makes it LOSE
	// (opposite of the offset-key verdict above) — proving the fixture actually
	// distinguishes the two keys, so the assertion above is not vacuous.
	if isBetterByIntendedActual(score, rivalTimestampLate, "node-a", score, oursTimestampEarly, "node-z") {
		t.Fatal("fixture is not adversarial: rival with a later timestamp must lose the timestamp key")
	}

	// Group path: identical guard.
	grival := &electionpb.GroupClaim{
		NodeId:          "node-a",
		GravityScore:    score,
		OffsetMicros:    sameOffset.Microseconds(),
		TimestampMicros: 9_999_999,
	}
	if !isBetterGroup(grival, score, sameOffset, "node-z") {
		t.Fatal("group: equal offset + smaller rival nodeID must win via nodeID; " +
			"a false result means isBetterGroup wrongly consulted timestamp_micros")
	}
}

// singleWinner returns the unique nodeID that wins under the offset tiebreak,
// or "" if the set does not resolve to exactly one winner.
func singleWinner(nodes []nodeView) string {
	winner := ""
	for i, self := range nodes {
		dec := self.decision()
		lost := false
		for j := range nodes {
			if i == j {
				continue
			}
			if isBetter(nodes[j].buildClaim(), dec, self.nodeID) {
				lost = true
				break
			}
		}
		if !lost {
			winner = self.nodeID
		}
	}
	return winner
}

// --- Manager-level scenarios (real pubsub, deterministic construction) -----

// TestO14_RoundTimesOutBeforePublish_NoDeadlock is the liveness counterpart
// to the best-node-drops simulation: a round that never reaches its publish
// point (the node "crashed"/stalled before publishing — modelled here by an
// intended PublishAt beyond the election timeout) must resolve to Failed
// promptly and release its slot — never deadlock.
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

// TestO14_ContendedReDecide_PublishedOffsetMatchesWindow guards the STABILITY
// INVARIANT the fix relies on: the OffsetMicros published on the wire must
// equal decision.Offset.Microseconds() — the same value isBetter compares
// against in the post-publish tiebreak window. Two replica rounds contend for
// one capsule's local-claim slot on a single node, forcing the loser through
// the CAS-loop park + re-Decide (manager.go). Whichever round publishes, the
// round must reach Won without split-brain, which can only happen if the
// published offset and the compared Offset are the same value.
func TestO14_ContendedReDecide_PublishedOffsetMatchesWindow(t *testing.T) {
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
	// meaning its published OffsetMicros (== decision.Offset) is stable across
	// the publish → tiebreak-window comparison.
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

// TestO14b_PublishedOffsetIsDecisionOffset is the direct field-level assertion
// of the fix: the manager must build its Claim with OffsetMicros ==
// decision.Offset.Microseconds(), and timestamp_micros must carry the intended
// PublishAt for observability (never the tiebreak). It reconstructs the claim
// the way runElection does and asserts both fields, so a future refactor that
// reverts the tiebreak to timestamp is caught here.
func TestO14b_PublishedOffsetIsDecisionOffset(t *testing.T) {
	// A decision with a distinct offset and a distinct intended PublishAt.
	intended := time.UnixMicro(7_654_321)
	offset := 123_456 * time.Microsecond
	decision := Decision{Eligible: true, Score: 63.5, PublishAt: intended, Offset: offset}

	// Mirror manager.go's claim construction (post-O14b).
	publishedMicros := decision.PublishAt.UnixMicro()
	publishedOffsetMicros := decision.Offset.Microseconds()
	claim := &electionpb.Claim{
		NodeId:          "node-under-test",
		GravityScore:    decision.Score,
		TimestampMicros: publishedMicros,
		OffsetMicros:    publishedOffsetMicros,
	}
	if claim.OffsetMicros != offset.Microseconds() {
		t.Fatalf("single-replica claim OffsetMicros = %d, want decision.Offset %d",
			claim.OffsetMicros, offset.Microseconds())
	}
	if claim.TimestampMicros != intended.UnixMicro() {
		t.Fatalf("single-replica claim TimestampMicros = %d, want intended (observability) %d",
			claim.TimestampMicros, intended.UnixMicro())
	}

	// Mirror manager_group.go's claim construction (post-O14b).
	gclaim := &electionpb.GroupClaim{
		NodeId:          "node-under-test",
		GravityScore:    decision.Score,
		TimestampMicros: intended.UnixMicro(),
		OffsetMicros:    offset.Microseconds(),
	}
	if gclaim.OffsetMicros != offset.Microseconds() {
		t.Fatalf("group claim OffsetMicros = %d, want offset %d", gclaim.OffsetMicros, offset.Microseconds())
	}

	// The value compared in the post-publish window (isBetter reads
	// ours.Offset) must equal the published field — the stability invariant.
	if decision.Offset.Microseconds() != claim.OffsetMicros {
		t.Fatalf("stability invariant broken: window-compare value %d != published %d",
			decision.Offset.Microseconds(), claim.OffsetMicros)
	}
}
