package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/runtime"
	"github.com/tareksalem/falak/runtime/mock"
)

// recordingReplicator records Replicate calls for assertion.
type recordingReplicator struct {
	mu    sync.Mutex
	calls []replicateCall
	fired chan struct{}
}

type replicateCall struct {
	capsuleID string
	tag       string
	checksum  string
	size      int64
}

func newRecordingReplicator() *recordingReplicator {
	return &recordingReplicator{fired: make(chan struct{}, 4)}
}

func (r *recordingReplicator) Replicate(capsuleID, tag, checksum string, size int64) {
	r.mu.Lock()
	r.calls = append(r.calls, replicateCall{capsuleID, tag, checksum, size})
	r.mu.Unlock()
	select {
	case r.fired <- struct{}{}:
	default:
	}
}

// noopBroadcaster satisfies runtime.SnapshotBroadcaster.
type noopBroadcaster struct{}

func (noopBroadcaster) BroadcastAvailable(capsuleID, tag, checksum string, size int64) error {
	return nil
}

// TestHandler_CaptureTriggersReplication proves that a successful cold-start
// capture invokes the wired SnapshotReplicator exactly once for the
// captured (capsule, tag) — the O11 hook in captureSnapshot.
func TestHandler_CaptureTriggersReplication(t *testing.T) {
	rt := mock.New()
	lc := &stubLifecycle{}
	snapStore := newStubSnapshotStore()
	store := &stubCapsuleStore{spec: defaultSpec()}
	rep := newRecordingReplicator()

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithSnapshotStore(snapStore),
		runtime.WithSnapshotBroadcaster(noopBroadcaster{}),
		runtime.WithSnapshotReplicator(rep),
		runtime.WithLifecycleNotifier(lc),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})
	lc.waitRunning(t, "cap1", 5*time.Second)

	select {
	case <-rep.fired:
	case <-time.After(5 * time.Second):
		t.Fatal("replicator was not invoked after capture")
	}

	rep.mu.Lock()
	defer rep.mu.Unlock()
	if len(rep.calls) != 1 {
		t.Fatalf("expected exactly 1 Replicate call, got %d", len(rep.calls))
	}
	c := rep.calls[0]
	if c.capsuleID != "cap1" || c.tag != "sha256:abc" {
		t.Errorf("Replicate called with (%s, %s), want (cap1, sha256:abc)", c.capsuleID, c.tag)
	}
}

// TestHandler_SetSnapshotReplicatorPostStart proves the post-construction
// setter is honoured (the node wires the replicator on cluster join, after
// the handler is built at node start).
func TestHandler_SetSnapshotReplicatorPostStart(t *testing.T) {
	rt := mock.New()
	lc := &stubLifecycle{}
	snapStore := newStubSnapshotStore()
	store := &stubCapsuleStore{spec: defaultSpec()}
	rep := newRecordingReplicator()

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithSnapshotStore(snapStore),
		runtime.WithLifecycleNotifier(lc),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.SetSnapshotReplicator(rep) // wired after Start

	h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})
	lc.waitRunning(t, "cap1", 5*time.Second)

	select {
	case <-rep.fired:
	case <-time.After(5 * time.Second):
		t.Fatal("replicator wired post-start was not invoked")
	}
}
