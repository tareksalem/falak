package node

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/enums"
	"github.com/tareksalem/falak/node/internal/events"
	falakrt "github.com/tareksalem/falak/runtime"
	"github.com/tareksalem/falak/runtime/mock"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// assignedReplica returns the first replica of the capsule that is bound to a
// non-empty node, along with whether one was found. Used to observe election
// outcomes on a node's local view.
func assignedReplica(n *Node, id capsule.CapsuleID) (capsule.ReplicaState, bool) {
	c := n.Capsules().Get(id)
	if c == nil {
		return capsule.ReplicaState{}, false
	}
	for _, r := range c.Replicas {
		if r.NodeID != "" {
			return r, true
		}
	}
	return capsule.ReplicaState{}, false
}

// waitForAssignedReplica polls a node's local view until the capsule has a
// replica bound to some node, returning that replica. Fails on timeout.
func waitForAssignedReplica(t *testing.T, n *Node, id capsule.CapsuleID, timeout time.Duration) capsule.ReplicaState {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r, ok := assignedReplica(n, id); ok {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("node %s: capsule %s never got an assigned replica within %v", n.Name(), id.String(), timeout)
	return capsule.ReplicaState{}
}

// electionWinCounter counts ElectionWon events per capsule ID seen on a node's
// event bus. Used to detect that a *second* election round completed after a
// crash (the recovery), which is the signal that the lost replica was
// re-placed rather than left stuck.
type electionWinCounter struct {
	mu    sync.Mutex
	count map[string]int
}

func (c *electionWinCounter) winsFor(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count[id]
}

func watchElectionWins(n *Node) (*electionWinCounter, func()) {
	c := &electionWinCounter{count: map[string]int{}}
	ch := n.EventBus().Subscribe(events.TypeElectionWon)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if won, ok := ev.(events.ElectionWon); ok {
					c.mu.Lock()
					c.count[won.CapsuleID]++
					c.mu.Unlock()
				}
			}
		}
	}()
	return c, cancel
}

// TestSingleNode_CrashRecovery_Replaces reproduces the exact scenario that
// dead-locked live: a single-node cluster runs a capsule, its container is lost
// out-of-band (the runtime backend emits a Removed event — the O2-detected
// path), and the node MUST clear the stale replica binding, re-win the
// re-election, and re-place the replica on itself.
//
// O3 (the binding-clear: UnassignReplica + onContainerCrash) is necessary but
// NOT sufficient for this end-to-end recovery. Live tracing confirmed O3
// restores eligibility — after the crash the election strategy logs
// "delay strategy: eligible" (self-anti-affinity no longer excludes the node).
// But the round still ends "no claim heard" because of a SECOND, independent
// defect tracked as O4: election/manager.go retains the per-capsule local claim
// slot (localClaims) across a Won election and only releases it on Lost/Failed,
// so a re-election for the same capsule on the same node finds hasLocalClaim
// true and steps aside forever. The correct fix for O4 is an election-core
// change (make the winner binding synchronous in the LifecycleController and
// release the claim slot on Won), out of O3 scope. See docs/BUGS.md O4.
//
// This test is the regression gate for O4. With O4 landed (the manager
// releases the local claim slot on a Won election after binding the winner
// durably) the full crash → re-placement loop (O2 event → O3 unbind → O4
// release → re-win → re-assign → mock re-start) closes end-to-end.
func TestSingleNode_CrashRecovery_Replaces(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	const cluster = "test/dc1/crash-recover"

	rt := mock.New()
	// Capture lifecycle logs so the test can assert ZERO rejected FSM
	// transitions during recovery (O6). Before the fix the crash → re-election
	// cycle logged "WinElectionWithBinding failed" (Warn) and "MarkRunning
	// failed" (Debug) because the capsule FSM was left in 'running'.
	obsCore, capturedLogs := observer.New(zapcore.DebugLevel)
	n1 := testNodeWithRuntimeAndOpts(t, "crash-rec-n1", 0, rt,
		WithPlacementRetryCap(1), WithLogger(zap.New(obsCore)))
	defer func() { _ = n1.Stop() }()

	wins, stopWins := watchElectionWins(n1)
	defer stopWins()

	joinCluster(t, n1, cluster)
	waitForPhonebookCount(t, n1, cluster, 1, 10*time.Second)
	joinOrbitOrFail(t, n1, cluster, "api")

	// Single-node orbit-loop spin-up buffer (no remote mesh to form).
	waitForGossipMeshSettle(1)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	created, err := n1.Capsules().Create(ctx, cluster, capsule.CapsuleSpec{
		Name:  "crash-app",
		Image: "registry.test/crash-app:v1",
		Orbit: "api",
		Resources: capsule.ResourceRequirements{
			CPUCores: 1,
			MemoryMB: 64,
		},
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	capsuleID := created.ID

	// Phase 1: the node wins the initial election and starts the container.
	first := waitForAssignedReplica(t, n1, capsuleID, 15*time.Second)
	if first.NodeID != n1.ID().String() {
		t.Fatalf("initial winner = %q, want local node %q", first.NodeID, n1.ID().String())
	}
	waitForContainersRunning(t, rt, 1, 15*time.Second)

	// Record the win count after the initial election so we can require a
	// strictly-greater count after the crash.
	deadlineW := time.Now().Add(5 * time.Second)
	for wins.winsFor(capsuleID.String()) < 1 && time.Now().Before(deadlineW) {
		time.Sleep(20 * time.Millisecond)
	}
	winsBefore := wins.winsFor(capsuleID.String())
	if winsBefore < 1 {
		t.Fatalf("expected at least one ElectionWon for the initial placement, got %d", winsBefore)
	}

	cID := fmt.Sprintf("falak-%s-%s", capsuleID.String(), string(first.ReplicaID))

	// Phase 2: lose the container out-of-band. The mock backend emits a
	// Removed event (the same shape the Podman event stream produces on a
	// `podman rm -f`); the runtime handler reports failure, which drives the
	// O3 unassign-then-re-elect path on the node.
	rt.EmitContainerEvent(falakrt.ContainerEvent{
		ContainerID: cID,
		Action:      falakrt.ContainerEventActionEnum.Removed(),
	})

	// Phase 3: the node must re-win the re-election. Before the fix this
	// never happened (self-anti-affinity excluded the only node, election
	// timed out, no claim). A strictly-higher ElectionWon count proves a
	// fresh round completed — i.e. the replica was re-placed.
	winDeadline := time.Now().Add(20 * time.Second)
	reWon := false
	for time.Now().Before(winDeadline) {
		if wins.winsFor(capsuleID.String()) > winsBefore {
			reWon = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !reWon {
		c := n1.Capsules().Get(capsuleID)
		var status enums.CapsuleStatus = "missing"
		var bound string
		if c != nil {
			status = c.Status
			for _, r := range c.Replicas {
				bound += fmt.Sprintf("[replica=%s node=%q status=%s]", r.ReplicaID, r.NodeID, r.Status)
			}
		}
		t.Fatalf("capsule did not re-win election after container loss (deadlock): wins=%d (was %d) status=%s replicas=%s",
			wins.winsFor(capsuleID.String()), winsBefore, status, bound)
	}

	// After the re-election the replica must be bound back to the local node.
	final := waitForAssignedReplica(t, n1, capsuleID, 10*time.Second)
	if final.NodeID != n1.ID().String() {
		t.Errorf("after recovery, replica bound to %q, want local node %q", final.NodeID, n1.ID().String())
	}

	// O6: recovery must drive the capsule back to Running via the valid forward
	// chain (announced → electing → assigned → executing → running). Wait for
	// the FSM to settle at Running, then assert ZERO rejected lifecycle
	// transitions were logged during the crash → re-election → running cycle.
	runDeadline := time.Now().Add(10 * time.Second)
	var lastStatus enums.CapsuleStatus
	for time.Now().Before(runDeadline) {
		if c := n1.Capsules().Get(capsuleID); c != nil {
			lastStatus = c.Status
			if lastStatus == enums.CapsuleStatusEnum.Running() {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastStatus != enums.CapsuleStatusEnum.Running() {
		t.Fatalf("capsule did not reach Running after recovery, last status=%s", lastStatus)
	}

	// The fix's core guarantee: the recovery loop is state-correct. None of the
	// from-state-'running' rejections that limped recovery along before O6 may
	// appear.
	for _, msg := range []string{"WinElectionWithBinding failed", "MarkRunning failed"} {
		if rejected := capturedLogs.FilterMessage(msg).Len(); rejected > 0 {
			t.Errorf("recovery logged %d rejected transition(s) %q; O6 requires zero", rejected, msg)
		}
	}
}
