package election

import (
	"context"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/election/gravity"
	electionpb "github.com/tareksalem/falak/election/proto/electionpb"
)

// stubStrategy is a minimal Strategy used by manager tests that only need
// to exercise registry lookups and validation paths — it never actually
// runs an election.
type stubStrategy struct{ name string }

func (s *stubStrategy) Name() string { return s.name }
func (s *stubStrategy) Decide(context.Context, Request, *capsule.Capsule, *gravity.Calculator, gravity.StateProvider) Decision {
	return Decision{}
}

// testCtx returns a context for tests that don't need cancellation.
func testCtx() context.Context { return context.Background() }

// --- isBetter tiebreak tests --------------------------------------------
//
// The middle tiebreak key is the deterministic priority OFFSET (O14b), not
// a wall-clock timestamp: SMALLER offset wins. timestamp_micros is set on
// the rivals below only to prove it is IGNORED — these fixtures use a
// wildly different (and adversarial) timestamp than the offset would imply,
// so a tiebreak that mistakenly consulted timestamp_micros would flip.

func TestIsBetter_HigherScoreWins(t *testing.T) {
	ours := Decision{
		Score:  50,
		Offset: 100 * time.Microsecond,
	}
	rival := &electionpb.Claim{
		GravityScore:    60,
		OffsetMicros:    200, // larger offset, but higher score wins outright
		TimestampMicros: 999, // ignored
		NodeId:          "z-later-id",
	}
	if !isBetter(rival, ours, "a-local") {
		t.Error("rival with higher score should win")
	}
}

func TestIsBetter_LowerScoreLoses(t *testing.T) {
	ours := Decision{
		Score:  60,
		Offset: 200 * time.Microsecond,
	}
	rival := &electionpb.Claim{
		GravityScore:    50,
		OffsetMicros:    100, // smaller offset, but lower score loses outright
		TimestampMicros: 1,   // ignored
		NodeId:          "a-earlier-id",
	}
	if isBetter(rival, ours, "z-local") {
		t.Error("rival with lower score should lose even with smaller offset")
	}
}

func TestIsBetter_SmallerOffsetWinsOnScoreTie(t *testing.T) {
	ours := Decision{
		Score:  50,
		Offset: 200 * time.Microsecond,
	}
	rival := &electionpb.Claim{
		GravityScore:    50,
		OffsetMicros:    100, // smaller offset wins on score tie
		TimestampMicros: 999, // LATER timestamp: proves timestamp is ignored
		NodeId:          "z-later-id",
	}
	if !isBetter(rival, ours, "a-local") {
		t.Error("rival with smaller offset should win on score tie")
	}
}

func TestIsBetter_SmallerNodeIDWinsOnFullTie(t *testing.T) {
	ours := Decision{
		Score:  50,
		Offset: 100 * time.Microsecond,
	}
	rival := &electionpb.Claim{
		GravityScore:    50,
		OffsetMicros:    100,
		TimestampMicros: 100,
		NodeId:          "a-rival",
	}
	if !isBetter(rival, ours, "z-local") {
		t.Error("rival with smaller node ID should win on full tie")
	}
}

func TestIsBetter_LargerNodeIDLosesOnFullTie(t *testing.T) {
	ours := Decision{
		Score:  50,
		Offset: 100 * time.Microsecond,
	}
	rival := &electionpb.Claim{
		GravityScore:    50,
		OffsetMicros:    100,
		TimestampMicros: 100,
		NodeId:          "z-rival",
	}
	if isBetter(rival, ours, "a-local") {
		t.Error("rival with larger node ID should lose on full tie")
	}
}

// --- StrategyRegistry tests ---------------------------------------------

func TestRegistry_DefaultUsedWhenNoOverride(t *testing.T) {
	def := &stubStrategy{name: "default"}
	reg := NewStrategyRegistry(def)
	if reg.Get("any/cluster").Name() != "default" {
		t.Error("default strategy should be returned when no override")
	}
}

func TestRegistry_PerClusterOverride(t *testing.T) {
	def := &stubStrategy{name: "default"}
	override := &stubStrategy{name: "special"}
	reg := NewStrategyRegistry(def)
	reg.Set("special/cluster", override)

	if reg.Get("special/cluster").Name() != "special" {
		t.Error("override should be returned for matching cluster")
	}
	if reg.Get("other/cluster").Name() != "default" {
		t.Error("default should be returned for unmatched cluster")
	}
}

func TestRegistry_ReplaceExistingOverride(t *testing.T) {
	reg := NewStrategyRegistry(&stubStrategy{name: "default"})
	reg.Set("c", &stubStrategy{name: "first"})
	reg.Set("c", &stubStrategy{name: "second"})
	if reg.Get("c").Name() != "second" {
		t.Error("second Set should replace first")
	}
}

// --- HandleRequest validation tests -------------------------------------

func TestHandleRequest_ErrorsWithoutStart(t *testing.T) {
	mgr := NewManager(&stubStrategy{name: "s"})
	if err := mgr.HandleRequest(Request{}); err == nil {
		t.Error("HandleRequest before Start should error")
	}
}

func TestHandleRequest_ErrorsWithoutStore(t *testing.T) {
	mgr := NewManager(&stubStrategy{name: "s"})
	mgr.Start(testCtx())
	defer mgr.Stop()

	if err := mgr.HandleRequest(Request{}); err == nil {
		t.Error("HandleRequest without store should error")
	}
}

// --- Local claim guard tests -------------------------------------------
//
// These cover the Manager's in-memory "this node has already published a
// claim for this capsule" guard, which is what prevents the best-fit
// node from winning every replica of a multi-replica capsule in a
// parallel fire.

func TestLocalClaim_FirstCallerWins(t *testing.T) {
	mgr := NewManager(&stubStrategy{name: "s"})
	id := capsule.CapsuleID("cap-1")

	if mgr.hasLocalClaim(id) {
		t.Fatal("no claim should exist for a fresh manager")
	}
	if !mgr.tryClaimCapsule(id) {
		t.Error("first tryClaimCapsule should succeed")
	}
	if !mgr.hasLocalClaim(id) {
		t.Error("hasLocalClaim should be true after a successful try")
	}
}

func TestLocalClaim_SecondCallerFails(t *testing.T) {
	mgr := NewManager(&stubStrategy{name: "s"})
	id := capsule.CapsuleID("cap-1")

	mgr.tryClaimCapsule(id)
	if mgr.tryClaimCapsule(id) {
		t.Error("second tryClaimCapsule for the same capsule should fail")
	}
}

func TestLocalClaim_ReleaseAllowsRetry(t *testing.T) {
	mgr := NewManager(&stubStrategy{name: "s"})
	id := capsule.CapsuleID("cap-1")

	mgr.tryClaimCapsule(id)
	mgr.releaseCapsuleClaim(id)

	if mgr.hasLocalClaim(id) {
		t.Error("release should clear the claim flag")
	}
	if !mgr.tryClaimCapsule(id) {
		t.Error("tryClaimCapsule after release should succeed")
	}
}

func TestLocalClaim_ForgetCapsuleClears(t *testing.T) {
	mgr := NewManager(&stubStrategy{name: "s"})
	id := capsule.CapsuleID("cap-1")

	mgr.tryClaimCapsule(id)
	mgr.ForgetCapsule(id)

	if mgr.hasLocalClaim(id) {
		t.Error("ForgetCapsule should clear the claim flag")
	}
}

func TestLocalClaim_IndependentCapsules(t *testing.T) {
	mgr := NewManager(&stubStrategy{name: "s"})
	idA := capsule.CapsuleID("cap-a")
	idB := capsule.CapsuleID("cap-b")

	if !mgr.tryClaimCapsule(idA) {
		t.Error("claim on capsule A should succeed")
	}
	if !mgr.tryClaimCapsule(idB) {
		t.Error("claim on capsule B should succeed independently")
	}
	if mgr.tryClaimCapsule(idA) {
		t.Error("second claim on capsule A should still fail")
	}
}

func TestLocalClaim_Concurrent(t *testing.T) {
	// Fire many goroutines contending for the same capsule claim and
	// verify exactly one wins. Catches missing mutex or ABA bugs in the
	// guard logic.
	mgr := NewManager(&stubStrategy{name: "s"})
	id := capsule.CapsuleID("cap-concurrent")

	const workers = 64
	results := make(chan bool, workers)
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		go func() {
			<-start
			results <- mgr.tryClaimCapsule(id)
		}()
	}
	close(start)

	wins := 0
	for i := 0; i < workers; i++ {
		if <-results {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("expected exactly one winning claim among %d workers, got %d", workers, wins)
	}
}

func TestLocalClaim_ForgetUnknownIsNoop(t *testing.T) {
	mgr := NewManager(&stubStrategy{name: "s"})
	// Must not panic, must not affect unrelated state.
	mgr.ForgetCapsule(capsule.CapsuleID("never-claimed"))
	if !mgr.tryClaimCapsule(capsule.CapsuleID("cap-1")) {
		t.Error("unrelated capsule should still be claimable")
	}
}
