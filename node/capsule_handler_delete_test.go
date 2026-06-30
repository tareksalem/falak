package node

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/node/internal/events"
)

// stopCall records a single StopContainer invocation routed through the
// runtimeGroupRollback hook by the capsule-delete teardown.
type stopCall struct {
	capsuleID string
	replicaID string
	grace     time.Duration
}

// fakeRollback is a test double for runtimeGroupRollback. It records
// every StopContainer call so the delete tests can assert the local
// container teardown was routed through the intentional-stop (O2
// ignore-set) path rather than a raw runtime Remove. The recorded calls
// are guarded by a mutex so the test is race-detector clean even though
// the handler invokes StopContainer from the manager-event callback.
type fakeRollback struct {
	mu     sync.Mutex
	stops  []stopCall
	err    error // optional error returned by StopContainer
	cancel int   // value returned by CancelGroupStarts
}

func (f *fakeRollback) CancelGroupStarts(_ string) int { return f.cancel }

func (f *fakeRollback) StopContainer(capsuleID, replicaID string, grace time.Duration) error {
	f.mu.Lock()
	f.stops = append(f.stops, stopCall{capsuleID: capsuleID, replicaID: replicaID, grace: grace})
	f.mu.Unlock()
	return f.err
}

func (f *fakeRollback) calls() []stopCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]stopCall, len(f.stops))
	copy(out, f.stops)
	return out
}

// assertNoFailureOrReelection drains the bus subscriptions for a short
// window and fails the test if a CapsuleExecutionFailed or
// ElectionRequested event was observed. This is the O2-interaction
// regression guard: an intentional delete teardown must NOT look like a
// crash, so it must neither publish a failure nor request a re-election.
func assertNoFailureOrReelection(t *testing.T, failSub, electSub <-chan events.Event) {
	t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case ev := <-failSub:
			if _, ok := ev.(events.CapsuleExecutionFailed); ok {
				t.Fatal("capsule delete must not publish CapsuleExecutionFailed (would self-trigger O2 re-election)")
			}
		case ev := <-electSub:
			if _, ok := ev.(events.ElectionRequested); ok {
				t.Fatal("capsule delete must not request a re-election")
			}
		case <-deadline:
			return
		}
	}
}

// TestEventCapsuleDeleted_StopsLocalContainer is the O8 regression test:
// deleting a capsule whose replica this node hosts must stop+remove the
// local container via the runtime, routed through the intentional-stop
// (ignore-set) path so it does NOT self-trigger the O2 crash/re-election
// flow.
func TestEventCapsuleDeleted_StopsLocalContainer(t *testing.T) {
	h, bus := newReelectionHandler(t)
	rb := &fakeRollback{}
	h.SetRuntimeRollback(rb)

	const (
		cluster   = "test/dc1/c"
		capsName  = "del-app"
		replicaID = "0"
	)
	// Bind the replica to the LOCAL node so the delete path treats it as
	// a locally-hosted container that must be torn down.
	c := seedCapsuleWithReplica(t, h, cluster, capsName, replicaID, "local-node")

	failSub := bus.Subscribe(events.TypeCapsuleFailed)
	electSub := bus.Subscribe(events.TypeElectionRequested)

	if err := h.manager.Delete(context.Background(), c.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	calls := rb.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 StopContainer call, got %d: %+v", len(calls), calls)
	}
	if calls[0].capsuleID != c.ID.String() {
		t.Errorf("StopContainer capsuleID = %q, want %q", calls[0].capsuleID, c.ID.String())
	}
	if calls[0].replicaID != replicaID {
		t.Errorf("StopContainer replicaID = %q, want %q", calls[0].replicaID, replicaID)
	}
	if calls[0].grace != defaultDeleteStopGrace {
		t.Errorf("StopContainer grace = %v, want %v", calls[0].grace, defaultDeleteStopGrace)
	}

	// O2 interaction guard: no failure event, no re-election.
	assertNoFailureOrReelection(t, failSub, electSub)
}

// TestEventCapsuleDeleted_SkipsRemoteReplica verifies a node only tears
// down the replicas IT hosts. This is what makes the
// withdrawal-received path correct: every peer processes the same
// EventCapsuleDeleted, but each stops only its own local container. A
// replica bound to another node must not be stopped here.
func TestEventCapsuleDeleted_SkipsRemoteReplica(t *testing.T) {
	h, _ := newReelectionHandler(t)
	rb := &fakeRollback{}
	h.SetRuntimeRollback(rb)

	c := seedCapsuleWithReplica(t, h, "test/dc1/c", "remote-del", "0", "other-node")

	if err := h.manager.Delete(context.Background(), c.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if calls := rb.calls(); len(calls) != 0 {
		t.Fatalf("remote-bound replica must not be stopped locally, got %+v", calls)
	}
}

// TestEventCapsuleDeleted_IdempotentWhenNoLocalContainer verifies the
// delete path tolerates a missing container: deleting a capsule with no
// replica bound to this node (or whose StopContainer reports the
// container is already gone) completes without error and without panic.
func TestEventCapsuleDeleted_IdempotentWhenNoLocalContainer(t *testing.T) {
	t.Run("no local replica", func(t *testing.T) {
		h, _ := newReelectionHandler(t)
		rb := &fakeRollback{}
		h.SetRuntimeRollback(rb)

		// Capsule created but never assigned anywhere.
		ctx := context.Background()
		c, err := h.manager.Create(ctx, "test/dc1/c", capsule.CapsuleSpec{
			Name:  "unplaced",
			Image: "img:v1",
			Orbit: "api",
		})
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)

		if err := h.manager.Delete(ctx, c.ID); err != nil {
			t.Fatalf("Delete of unplaced capsule failed: %v", err)
		}
		if calls := rb.calls(); len(calls) != 0 {
			t.Fatalf("unplaced capsule must not stop any container, got %+v", calls)
		}
	})

	t.Run("container already gone", func(t *testing.T) {
		h, _ := newReelectionHandler(t)
		// StopContainer reports the container is already gone — the
		// delete path must tolerate this (log Warn, continue) and
		// complete the deletion.
		rb := &fakeRollback{err: errContainerGone}
		h.SetRuntimeRollback(rb)

		c := seedCapsuleWithReplica(t, h, "test/dc1/c", "gone-app", "0", "local-node")

		if err := h.manager.Delete(context.Background(), c.ID); err != nil {
			t.Fatalf("Delete must tolerate StopContainer error, got: %v", err)
		}
		if calls := rb.calls(); len(calls) != 1 {
			t.Fatalf("expected the stop to be attempted once, got %+v", calls)
		}
		// Capsule metadata must be gone regardless of the stop error.
		if h.manager.Get(c.ID) != nil {
			t.Fatal("capsule must be deleted even when StopContainer errors")
		}
	})
}

// TestEventCapsuleDeleted_NoRollbackHookNoPanic verifies the delete path
// is safe when no runtime rollback hook is installed (tests,
// runtime-disabled deployments): it must not panic and the deletion must
// still complete.
func TestEventCapsuleDeleted_NoRollbackHookNoPanic(t *testing.T) {
	h, _ := newReelectionHandler(t)
	// Deliberately do NOT install a rollback hook.

	c := seedCapsuleWithReplica(t, h, "test/dc1/c", "no-hook", "0", "local-node")

	if err := h.manager.Delete(context.Background(), c.ID); err != nil {
		t.Fatalf("Delete without rollback hook failed: %v", err)
	}
	if h.manager.Get(c.ID) != nil {
		t.Fatal("capsule must be deleted even without a rollback hook")
	}
}

// errContainerGone simulates the runtime reporting that a container is
// already gone — the idempotent-teardown case the delete path tolerates.
var errContainerGone = &containerGoneError{}

type containerGoneError struct{}

func (*containerGoneError) Error() string { return "runtime: container not found" }
