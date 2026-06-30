package reliability

import (
	"context"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/node/internal/events"
)

// fakeClock is a deterministic, race-safe clock for decay assertions.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var testBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func approx(t *testing.T, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("got %.4f, want %.4f (±%.4f)", got, want, tol)
	}
}

// TestTrackerS9_MinConfidencePrior pins the Bayesian prior: a node with no
// history scores at a/(a+b) = 0.8 (optimistic but below a proven node), and a
// single node-attributable failure moves it to 4/6 = 0.667 — never to 0.
func TestTrackerS9_MinConfidencePrior(t *testing.T) {
	clk := &fakeClock{now: testBase}
	tr := NewTracker("n", nil, nil, WithClock(clk))

	approx(t, tr.Current(), 0.8, 1e-9)

	tr.recordFailureEvent(events.FailureCategoryEnum.NodeAttributable())
	approx(t, tr.Current(), 4.0/6.0, 1e-9)
}

// TestTrackerS8_Attribution verifies failure attribution: five CapsuleGlobal
// failures (bad image/spec — would fail everywhere) leave the score UNCHANGED,
// while five NodeAttributable failures DROP it.
func TestTrackerS8_Attribution(t *testing.T) {
	clk := &fakeClock{now: testBase}
	tr := NewTracker("n", nil, nil, WithClock(clk))

	for i := 0; i < 5; i++ {
		tr.recordFailureEvent(events.FailureCategoryEnum.CapsuleGlobal())
	}
	approx(t, tr.Current(), 0.8, 1e-9) // unchanged: capsule-global excluded

	for i := 0; i < 5; i++ {
		tr.recordFailureEvent(events.FailureCategoryEnum.NodeAttributable())
	}
	got := tr.Current()
	if got >= 0.8 {
		t.Fatalf("node-attributable failures should drop the score below 0.8, got %.4f", got)
	}
	approx(t, got, 4.0/10.0, 1e-9) // (0+4)/(0+5+4+1) = 0.4
}

// TestTrackerS7_RecoveryClimb is the core decay test. A node earns a high
// score, its successes decay away, a burst of node-attributable failures tanks
// it (~0.3), then — as the failures decay AND the node successfully runs
// capsules again — it climbs back above 0.85.
//
// NOTE ON THE SPEC: the plan's S7 phrasing ("advance clock 2×halfLife → climbs
// back ≥.85") is not reachable by clock advance ALONE: with prior a=4,b=1 the
// pure-time-decay asymptote is the prior 4/5 = 0.8, so a score above 0.8
// requires live successes. The recovery phase therefore models real recovery
// (time passes AND the node places workloads successfully again), which is the
// design's stated intent (idle decay recovers toward 0.8; active success
// recovery climbs higher). The prior is left at 4/1 because S8/S9 pin it.
func TestTrackerS7_RecoveryClimb(t *testing.T) {
	const hl = 30 * time.Minute
	clk := &fakeClock{now: testBase}
	tr := NewTracker("n", nil, nil, WithClock(clk), WithHalfLife(hl))

	// Phase 1: 10 successes → high score (14/15 ≈ 0.933).
	for i := 0; i < 10; i++ {
		tr.recordSuccess()
	}
	high := tr.Current()
	if high < 0.9 {
		t.Fatalf("phase 1: expected high score ≈0.93, got %.4f", high)
	}

	// Phase 2: let the successes decay away, then 10 node-attributable
	// failures tank the score toward ≈0.3.
	clk.advance(7 * hl) // 0.5^7 ≈ 0.0078 → successes ≈ 0.078
	for i := 0; i < 10; i++ {
		tr.recordFailureEvent(events.FailureCategoryEnum.NodeAttributable())
	}
	low := tr.Current()
	if low > 0.35 {
		t.Fatalf("phase 2: expected tanked score ≈0.3, got %.4f", low)
	}

	// Phase 3: failures decay over 2 half-lives and the node runs capsules
	// successfully again → climbs back ≥ 0.85.
	clk.advance(2 * hl)
	for i := 0; i < 20; i++ {
		tr.recordSuccess()
	}
	recovered := tr.Current()
	if recovered < 0.85 {
		t.Fatalf("phase 3: expected recovery ≥0.85, got %.4f", recovered)
	}
}

// TestTrackerIdleDecayRecoversTowardPrior confirms the pure-time-decay
// recovery property: after a failure streak, an IDLE node (no new events)
// climbs back toward the 0.8 prior as its failures decay.
func TestTrackerIdleDecayRecoversTowardPrior(t *testing.T) {
	const hl = 30 * time.Minute
	clk := &fakeClock{now: testBase}
	tr := NewTracker("n", nil, nil, WithClock(clk), WithHalfLife(hl))

	for i := 0; i < 10; i++ {
		tr.recordFailureEvent(events.FailureCategoryEnum.NodeAttributable())
	}
	tanked := tr.Current() // (0+4)/(0+10+5) ≈ 0.267
	if tanked > 0.3 {
		t.Fatalf("expected tanked ≈0.27, got %.4f", tanked)
	}

	clk.advance(10 * hl) // failures decay to ≈0.0098
	recovered := tr.Current()
	if recovered <= tanked || recovered > 0.8 {
		t.Fatalf("idle decay should climb toward (but not exceed) 0.8: tanked=%.4f recovered=%.4f", tanked, recovered)
	}
	approx(t, recovered, 0.8, 0.02)
}

// TestTrackerPersistAndReloadAppliesDowntimeDecay verifies the SQLite
// round-trip and that decay is applied for the downtime gap on reload.
func TestTrackerPersistAndReloadAppliesDowntimeDecay(t *testing.T) {
	const hl = 30 * time.Minute
	path := filepath.Join(t.TempDir(), "reliability.db")
	clk := &fakeClock{now: testBase}

	store1, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open store1: %v", err)
	}
	tr1 := NewTracker("node-x", store1, nil, WithClock(clk), WithHalfLife(hl))
	for i := 0; i < 10; i++ {
		tr1.recordFailureEvent(events.FailureCategoryEnum.NodeAttributable())
	}
	tr1.persist() // S=0, F=10, lastUpdate=testBase
	if err := store1.Close(); err != nil {
		t.Fatalf("close store1: %v", err)
	}

	// Simulate downtime, then reload.
	clk.advance(2 * hl)
	store2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open store2: %v", err)
	}
	defer store2.Close()
	tr2 := NewTracker("node-x", store2, nil, WithClock(clk), WithHalfLife(hl))

	// Reloaded F=10 at testBase; reading 2 half-lives later decays F to 2.5.
	got := tr2.Current() // (0+4)/(0+2.5+5) = 0.5333
	approx(t, got, 4.0/7.5, 1e-6)
}

// TestTrackerBusSubscriptionRoutesEvents verifies the event-bus wiring:
// CapsuleRunning increments successes, CapsuleExecutionFailed is filtered by
// category. Synchronisation is via the onProcessed hook (no sleeps).
func TestTrackerBusSubscriptionRoutesEvents(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()
	clk := &fakeClock{now: testBase}
	processed := make(chan struct{}, 8)
	tr := NewTracker("node-y", nil, bus, WithClock(clk),
		withOnProcessed(func() { processed <- struct{}{} }))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.Start(ctx)
	defer tr.Stop()

	waitProcessed := func() {
		select {
		case <-processed:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for event to be processed")
		}
	}

	bus.Publish(events.CapsuleRunning{BaseEvent: events.NewBaseEvent(), CapsuleID: "c1"})
	waitProcessed()
	approx(t, tr.Current(), 5.0/6.0, 1e-9) // S=1 → (1+4)/(1+5)

	// CapsuleGlobal failure is excluded — score unchanged.
	bus.Publish(events.CapsuleExecutionFailed{
		BaseEvent: events.NewBaseEvent(),
		CapsuleID: "c1",
		Category:  events.FailureCategoryEnum.CapsuleGlobal(),
	})
	waitProcessed()
	approx(t, tr.Current(), 5.0/6.0, 1e-9)

	// NodeAttributable failure is counted.
	bus.Publish(events.CapsuleExecutionFailed{
		BaseEvent: events.NewBaseEvent(),
		CapsuleID: "c1",
		Category:  events.FailureCategoryEnum.NodeAttributable(),
	})
	waitProcessed()
	approx(t, tr.Current(), 5.0/7.0, 1e-9) // S=1,F=1 → (1+4)/(1+1+5)
}
