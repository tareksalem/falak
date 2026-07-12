package sync

import (
	"context"
	"testing"
	"time"

	"github.com/tareksalem/falak/node/internal/events"
)

// TestBurstBackstop_ConvergesViaAntiEntropy is the Layer-2-only test: Layer 1
// is disabled, and node2 never received node3's Step-2 announcement. node2's
// convergence burst (driven deterministically by a mock clock — no real 1s
// sleeps) must sync node3 from node1. We advance the mock clock past exactly
// one burst interval and assert convergence via the SyncCompleted event.
func TestBurstBackstop_ConvergesViaAntiEntropy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := newMockClock(time.Unix(0, 0))

	// node1 holds the complete roster (self, node2, node3).
	n1 := newSyncNode(t, ctx, "node1", WithMemberPushEnabled(false))
	// node2 uses the mock clock and knows only itself + node1 (missed node3).
	n2 := newSyncNode(t, ctx, "node2",
		WithMemberPushEnabled(false),
		WithClock(clock),
		WithBurstInterval(1*time.Second),
		WithBurstDuration(30*time.Second),
		WithBurstJitter(0), // exact timing for determinism
	)
	n3 := newSyncNode(t, ctx, "node3", WithMemberPushEnabled(false))

	n1.addSelf()
	n1.addMember(n2)
	n1.addMember(n3)

	n2.addSelf()
	n2.addMember(n1)

	// node1 must accept node2's sync request: node2 is in node1's phonebook
	// (the receiver-auth gate), and node2 can dial node1.
	connect(t, ctx, n2, n1)

	if n2.exists(n3.host.ID().String()) {
		t.Fatal("precondition failed: node2 already knows node3")
	}

	// Arm the burst as a membership change would. StartPeriodicSync wires the
	// loop; armBurst puts it in the fast window.
	n2.syncer.StartPeriodicSync(testCluster)
	n2.syncer.armBurst(testCluster)

	// Observe convergence deterministically via the sync-completed event.
	syncCh := n2.bus.Subscribe(events.TypeSyncCompleted)

	// Give the loop a moment to consume the arm signal and register its
	// clock.After(interval) waiter, then advance past one burst interval.
	// We poll the mock clock's waiter registration rather than sleeping on
	// wall time for the convergence assertion itself.
	waitForWaiter(t, clock)
	clock.Advance(1 * time.Second)

	ev := waitEvent(t, syncCh, 10*time.Second, func(e events.Event) bool {
		c, ok := e.(events.SyncCompleted)
		return ok && c.NewMembers >= 1
	}).(events.SyncCompleted)

	if ev.SyncedFrom != n1.host.ID().String() {
		t.Fatalf("expected sync from node1, got %s", ev.SyncedFrom)
	}
	if !n2.exists(n3.host.ID().String()) {
		t.Fatal("node2 did not converge to node3 via burst anti-entropy")
	}
	if got := n2.count(); got != 3 {
		t.Fatalf("expected node2 phonebook == 3 after burst, got %d", got)
	}
}

// TestBurst_SettlesToSteady asserts that after the burst window elapses with
// no re-arm the loop falls back to the steady interval (it does not keep
// firing at the fast rate forever).
func TestBurst_SettlesToSteady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := newMockClock(time.Unix(0, 0))

	n1 := newSyncNode(t, ctx, "node1", WithMemberPushEnabled(false))
	n2 := newSyncNode(t, ctx, "node2",
		WithMemberPushEnabled(false),
		WithClock(clock),
		WithBurstInterval(1*time.Second),
		WithBurstDuration(2*time.Second),
		WithBurstJitter(0),
		WithSyncInterval(90*time.Second),
	)

	n1.addSelf()
	n1.addMember(n2)
	n2.addSelf()
	n2.addMember(n1)
	connect(t, ctx, n2, n1)

	n2.syncer.StartPeriodicSync(testCluster)
	n2.syncer.armBurst(testCluster)

	syncCh := n2.bus.Subscribe(events.TypeSyncCompleted)

	// Burst duration 2s at 1s interval → ~2 fast ticks, then steady (90s).
	// Fire the two burst ticks.
	for i := 0; i < 2; i++ {
		waitForWaiter(t, clock)
		clock.Advance(1 * time.Second)
		// Each tick performs a sync (node1 has node2 already, so NewMembers
		// may be 0 — we only need the loop to keep advancing, so drain).
		drainSyncEvents(syncCh, 2*time.Second)
	}

	// After the window elapsed, the next waiter must be for the steady
	// interval (90s), not the burst interval (1s). Advancing 1s must NOT
	// fire another tick.
	waitForWaiter(t, clock)
	clock.Advance(1 * time.Second)
	select {
	case <-syncCh:
		t.Fatal("loop kept firing at burst rate after window elapsed")
	case <-time.After(500 * time.Millisecond):
		// Expected: settled to steady, 1s advance is not enough to fire.
	}
}

// waitForWaiter blocks until the mock clock has at least one pending After
// waiter registered by the burst loop. This lets the test advance the clock
// exactly when the loop is ready, keeping timing deterministic without
// coupling to the loop's internal scheduling via sleeps for the assertion.
func waitForWaiter(t *testing.T, c *mockClock) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.waiters)
		c.mu.Unlock()
		if n > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("no clock waiter registered by burst loop")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func drainSyncEvents(ch <-chan events.Event, within time.Duration) {
	deadline := time.After(within)
	for {
		select {
		case <-ch:
			return
		case <-deadline:
			return
		}
	}
}
