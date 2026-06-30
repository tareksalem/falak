package runtime_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/runtime"
	"github.com/tareksalem/falak/runtime/mock"
)

// stubCapsuleStore returns a fixed spec for any capsule ID.
type stubCapsuleStore struct {
	spec *runtime.CapsuleSpec
}

func (s *stubCapsuleStore) GetSpec(capsuleID string) (*runtime.CapsuleSpec, error) {
	return s.spec, nil
}

// stubSnapshotStore tracks "has" calls and records writes.
type stubSnapshotStore struct {
	mu       sync.Mutex
	has      map[string]bool
	recorded []string
}

func newStubSnapshotStore() *stubSnapshotStore {
	return &stubSnapshotStore{has: make(map[string]bool)}
}

func (s *stubSnapshotStore) HasLocal(capsuleID, tag string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.has[capsuleID+"/"+tag]
}

func (s *stubSnapshotStore) SnapshotPath(capsuleID, tag string) string {
	return "/tmp/snap/" + capsuleID + "/" + tag
}

func (s *stubSnapshotStore) RecordSnapshot(capsuleID, tag, checksum, path string, size int64, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recorded = append(s.recorded, capsuleID+"/"+tag)
	s.has[capsuleID+"/"+tag] = true
	return nil
}

func (s *stubSnapshotStore) MarkInUse(capsuleID, tag string, inUse bool) error { return nil }
func (s *stubSnapshotStore) TouchAccess(capsuleID, tag string) error           { return nil }

func (s *stubSnapshotStore) setLocal(capsuleID, tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.has[capsuleID+"/"+tag] = true
}

// stubLifecycle records lifecycle calls.
type stubLifecycle struct {
	mu             sync.Mutex
	running        []string
	failed         []string
	failedCategory []runtime.FailureCategory
	stopped        []string
}

func (l *stubLifecycle) MarkRunning(capsuleID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running = append(l.running, capsuleID)
	return nil
}

func (l *stubLifecycle) MarkFailed(capsuleID, reason string, category runtime.FailureCategory) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed = append(l.failed, capsuleID)
	l.failedCategory = append(l.failedCategory, category)
	return nil
}

func (l *stubLifecycle) MarkStopped(capsuleID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopped = append(l.stopped, capsuleID)
	return nil
}

func (l *stubLifecycle) waitRunning(t *testing.T, capsuleID string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		l.mu.Lock()
		for _, id := range l.running {
			if id == capsuleID {
				l.mu.Unlock()
				return
			}
		}
		l.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for MarkRunning(%s)", capsuleID)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func defaultSpec() *runtime.CapsuleSpec {
	return &runtime.CapsuleSpec{
		Name:        "test-app",
		Image:       "img:v1",
		ImageDigest: "sha256:abc",
		Env:         map[string]string{"APP": "test"},
		NetworkMode: runtime.NetworkModeEnum.Bridge(),
	}
}

func TestHandler_ColdStart(t *testing.T) {
	rt := mock.New()
	lc := &stubLifecycle{}
	store := &stubCapsuleStore{spec: defaultSpec()}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithLifecycleNotifier(lc),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{
		CapsuleID: "cap1",
		ReplicaID: "0",
	})

	lc.waitRunning(t, "cap1", 5*time.Second)

	if !rt.HasPulled("img:v1") {
		t.Error("image should be pulled on cold start")
	}
	if rt.ContainerStatus("falak-cap1-0") != runtime.ContainerStatusEnum.Running() {
		t.Error("container should be running")
	}
}

func TestHandler_RestoreFromSnapshot(t *testing.T) {
	rt := mock.New()
	lc := &stubLifecycle{}
	snapStore := newStubSnapshotStore()
	snapStore.setLocal("cap1", "sha256:abc")
	store := &stubCapsuleStore{spec: defaultSpec()}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithSnapshotStore(snapStore),
		runtime.WithLifecycleNotifier(lc),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{
		CapsuleID: "cap1",
		ReplicaID: "0",
	})

	lc.waitRunning(t, "cap1", 5*time.Second)

	// Should have restored, not pulled.
	if rt.HasPulled("img:v1") {
		t.Error("image should NOT be pulled on snapshot restore")
	}
	if rt.ContainerStatus("falak-cap1-0") != runtime.ContainerStatusEnum.Running() {
		t.Error("restored container should be running")
	}
}

func TestHandler_ColdStartCapturesSnapshot(t *testing.T) {
	rt := mock.New()
	lc := &stubLifecycle{}
	snapStore := newStubSnapshotStore()
	store := &stubCapsuleStore{spec: defaultSpec()}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithSnapshotStore(snapStore),
		runtime.WithLifecycleNotifier(lc),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{
		CapsuleID: "cap1",
		ReplicaID: "0",
	})

	lc.waitRunning(t, "cap1", 5*time.Second)

	// Give the background snapshot goroutine time to run.
	time.Sleep(500 * time.Millisecond)

	snapStore.mu.Lock()
	recorded := len(snapStore.recorded)
	snapStore.mu.Unlock()

	if recorded == 0 {
		t.Error("cold start should capture a snapshot")
	}
}

func TestHandler_StopContainer(t *testing.T) {
	rt := mock.New()
	lc := &stubLifecycle{}
	store := &stubCapsuleStore{spec: defaultSpec()}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithLifecycleNotifier(lc),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{
		CapsuleID: "cap1",
		ReplicaID: "0",
	})
	lc.waitRunning(t, "cap1", 5*time.Second)

	if err := h.StopContainer("cap1", "0", 5*time.Second); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}

	lc.mu.Lock()
	stoppedCount := len(lc.stopped)
	lc.mu.Unlock()

	if stoppedCount == 0 {
		t.Error("MarkStopped should have been called")
	}
}

// streamingRuntimeStub embeds a mock.Runtime to satisfy the rest of the
// runtime.Runtime interface and adds a PullStreaming method that drives
// onProgress with a canned sequence of (current, total) tuples. The
// stub is local to this test so the mock package keeps its narrow
// runtime.Runtime contract.
type streamingRuntimeStub struct {
	*mock.Runtime
	progress []struct{ current, total int64 }
}

// PullStreaming implements runtime.StreamingPuller. The canned sequence
// is delivered synchronously with a short sleep between calls so the
// rate-limit window in pullWithHeartbeat trips on a subset of records,
// exercising both the emission path AND the throttle.
func (s *streamingRuntimeStub) PullStreaming(ctx context.Context, image string, onProgress func(current, total int64), opts ...runtime.PullOption) error {
	for _, p := range s.progress {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		onProgress(p.current, p.total)
		time.Sleep(1100 * time.Millisecond) // > pullProgressMinEmitInterval so every record passes the throttle
	}
	return s.Runtime.Pull(ctx, image, opts...)
}

// recordingEmitter captures every EmitPullProgress / EmitMemberPlacementFailed
// call so the test can assert non-zero bytesRemaining emission.
type recordingPullEmitter struct {
	mu      sync.Mutex
	pulls   []pullProgressCall
	mpfs    int
}

type pullProgressCall struct {
	groupID, capsuleID string
	bytesRemaining     int64
	bytesPerSecond     int64
}

func (e *recordingPullEmitter) EmitMemberPlacementFailed(_, _, _ string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mpfs++
}

func (e *recordingPullEmitter) EmitPullProgress(groupID, capsuleID string, bytesRemaining, bytesPerSecond int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pulls = append(e.pulls, pullProgressCall{groupID, capsuleID, bytesRemaining, bytesPerSecond})
}

func (e *recordingPullEmitter) snapshot() []pullProgressCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]pullProgressCall, len(e.pulls))
	copy(out, e.pulls)
	return out
}

// TestHandler_PullWithHeartbeat_StreamingDeriverNonZeroBytes drives a
// streaming-capable runtime through the runtime handler's cold-start
// path and asserts that at least one PullProgress emission carries
// non-zero bytesRemaining. Together with the periodic-tick "-1, 0"
// liveness emissions, this is the contract the election manager
// depends on for reservation extension.
func TestHandler_PullWithHeartbeat_StreamingDeriverNonZeroBytes(t *testing.T) {
	stub := &streamingRuntimeStub{
		Runtime: mock.New(),
		progress: []struct{ current, total int64 }{
			{1024, 10240},
			{5120, 10240},
			{10240, 10240},
		},
	}
	emitter := &recordingPullEmitter{}
	lc := &stubLifecycle{}
	store := &stubCapsuleStore{spec: defaultSpec()}

	h := runtime.NewHandler(stub,
		runtime.WithCapsuleStore(store),
		runtime.WithLifecycleNotifier(lc),
		runtime.WithGroupEventEmitter(emitter),
		runtime.WithPullHeartbeatInterval(0), // disable periodic ticker; only streaming-derived emissions count
	)
	h.Start(context.Background())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{
		CapsuleID: "cap-stream",
		ReplicaID: "0",
	})
	lc.waitRunning(t, "cap-stream", 30*time.Second)

	// Confirm at least one emission carried real bytes-remaining (i.e.
	// the streaming path fired, not just the start/finish bookends).
	sawNonZero := false
	for _, p := range emitter.snapshot() {
		if p.bytesRemaining > 0 {
			sawNonZero = true
			break
		}
	}
	if !sawNonZero {
		t.Fatalf("expected at least one non-zero bytesRemaining emission, got %+v", emitter.snapshot())
	}
}

// waitFailed blocks until MarkFailed has been called for capsuleID or the
// timeout elapses (fatal on timeout).
func (l *stubLifecycle) waitFailed(t *testing.T, capsuleID string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		l.mu.Lock()
		for _, id := range l.failed {
			if id == capsuleID {
				l.mu.Unlock()
				return
			}
		}
		l.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for MarkFailed(%s)", capsuleID)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// failedCount returns how many times MarkFailed was called for capsuleID.
func (l *stubLifecycle) failedCount(capsuleID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, id := range l.failed {
		if id == capsuleID {
			n++
		}
	}
	return n
}

func TestHandler_CrashTriggersMarkFailed(t *testing.T) {
	rt := mock.New()
	lc := &stubLifecycle{}
	store := &stubCapsuleStore{spec: defaultSpec()}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithLifecycleNotifier(lc),
		runtime.WithReconcileInterval(10*time.Millisecond),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{
		CapsuleID: "cap1",
		ReplicaID: "0",
	})
	lc.waitRunning(t, "cap1", 5*time.Second)

	// Simulate container crash via the backend status (no event emitted);
	// the reconcile sweep must catch the Failed status.
	rt.SimulateCrash("falak-cap1-0", 137)

	lc.waitFailed(t, "cap1", 5*time.Second)
}

// TestHandler_DiedEventTriggersRecovery exercises the event-driven crash
// path: a Died event with restart_limit=0 goes straight to MarkFailed,
// while restart_limit>0 attempts local restarts first.
func TestHandler_DiedEventTriggersRecovery(t *testing.T) {
	t.Run("no_restart_limit_marks_failed", func(t *testing.T) {
		rt := mock.New()
		lc := &stubLifecycle{}
		spec := defaultSpec()
		spec.FailurePolicy.RestartLimit = 0
		store := &stubCapsuleStore{spec: spec}

		h := runtime.NewHandler(rt,
			runtime.WithCapsuleStore(store),
			runtime.WithLifecycleNotifier(lc),
		)
		h.Start(context.Background())
		defer h.Stop()

		h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})
		lc.waitRunning(t, "cap1", 5*time.Second)

		rt.EmitContainerEvent(runtime.ContainerEvent{
			ContainerID: "falak-cap1-0",
			Action:      runtime.ContainerEventActionEnum.Died(),
			ExitCode:    137,
		})

		lc.waitFailed(t, "cap1", 5*time.Second)
	})

	t.Run("restart_limit_restarts_before_failing", func(t *testing.T) {
		rt := mock.New()
		lc := &stubLifecycle{}
		spec := defaultSpec()
		spec.FailurePolicy.RestartLimit = 2
		store := &stubCapsuleStore{spec: spec}

		h := runtime.NewHandler(rt,
			runtime.WithCapsuleStore(store),
			runtime.WithLifecycleNotifier(lc),
		)
		h.Start(context.Background())
		defer h.Stop()

		h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})
		lc.waitRunning(t, "cap1", 5*time.Second)

		died := func() {
			rt.EmitContainerEvent(runtime.ContainerEvent{
				ContainerID: "falak-cap1-0",
				Action:      runtime.ContainerEventActionEnum.Died(),
				ExitCode:    1,
			})
		}

		// First two crashes consume the restart budget (local restart),
		// not MarkFailed. We poll the backend status to confirm it is
		// running again after each restart.
		waitRunning := func() {
			deadline := time.After(2 * time.Second)
			for rt.ContainerStatus("falak-cap1-0") != runtime.ContainerStatusEnum.Running() {
				select {
				case <-deadline:
					t.Fatal("container not restarted to Running")
				case <-time.After(5 * time.Millisecond):
				}
			}
		}

		died()
		waitRunning()
		if lc.failedCount("cap1") != 0 {
			t.Fatalf("first crash should restart, not MarkFailed; failures=%d", lc.failedCount("cap1"))
		}
		died()
		waitRunning()
		if lc.failedCount("cap1") != 0 {
			t.Fatalf("second crash should restart, not MarkFailed; failures=%d", lc.failedCount("cap1"))
		}

		// Third crash exhausts the budget → MarkFailed.
		died()
		lc.waitFailed(t, "cap1", 5*time.Second)
	})
}

// TestHandler_RemoveEventTriggersMarkFailed is the O2 regression: a
// removed container is terminal and must MarkFailed → re-election even
// though it can no longer be restarted locally.
func TestHandler_RemoveEventTriggersMarkFailed(t *testing.T) {
	rt := mock.New()
	lc := &stubLifecycle{}
	spec := defaultSpec()
	spec.FailurePolicy.RestartLimit = 5 // irrelevant: removal cannot restart
	store := &stubCapsuleStore{spec: spec}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithLifecycleNotifier(lc),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})
	lc.waitRunning(t, "cap1", 5*time.Second)

	rt.EmitContainerEvent(runtime.ContainerEvent{
		ContainerID: "falak-cap1-0",
		Action:      runtime.ContainerEventActionEnum.Removed(),
	})

	lc.waitFailed(t, "cap1", 5*time.Second)
}

// TestHandler_IntentionalRemoveIgnored confirms that a handler-initiated
// Stop/Remove (StopContainer) does NOT MarkFailed even though the backend
// would emit died+remove events for the teardown.
func TestHandler_IntentionalRemoveIgnored(t *testing.T) {
	rt := mock.New()
	lc := &stubLifecycle{}
	store := &stubCapsuleStore{spec: defaultSpec()}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithLifecycleNotifier(lc),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})
	lc.waitRunning(t, "cap1", 5*time.Second)

	if err := h.StopContainer("cap1", "0", time.Second); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}

	// Now emit the died+remove events the backend would have produced for
	// the teardown. They must be suppressed by the ignore set.
	rt.EmitContainerEvent(runtime.ContainerEvent{
		ContainerID: "falak-cap1-0",
		Action:      runtime.ContainerEventActionEnum.Died(),
		ExitCode:    0,
	})
	rt.EmitContainerEvent(runtime.ContainerEvent{
		ContainerID: "falak-cap1-0",
		Action:      runtime.ContainerEventActionEnum.Removed(),
	})

	// Give the consumer a moment to (not) act.
	time.Sleep(50 * time.Millisecond)

	if lc.failedCount("cap1") != 0 {
		t.Fatalf("intentional teardown must not MarkFailed; failures=%d", lc.failedCount("cap1"))
	}
}

// TestHandler_ReconcileCatchesMissedRemoval proves the reconcile backstop:
// the container is removed at the backend WITHOUT emitting an event, and
// the sweep detects the not-found Inspect → MarkFailed.
func TestHandler_ReconcileCatchesMissedRemoval(t *testing.T) {
	rt := mock.New()
	lc := &stubLifecycle{}
	store := &stubCapsuleStore{spec: defaultSpec()}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithLifecycleNotifier(lc),
		runtime.WithReconcileInterval(10*time.Millisecond),
	)
	h.Start(context.Background())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})
	lc.waitRunning(t, "cap1", 5*time.Second)

	// Remove via the backend without emitting any event.
	if err := rt.Remove(context.Background(), "falak-cap1-0"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	lc.waitFailed(t, "cap1", 5*time.Second)
}

// TestHandler_TransientInspectEscalation confirms the runtime-unreachable
// escalation: consecutive transient Inspect errors escalate only after
// maxInspectErrors, while a single blip followed by recovery does not.
func TestHandler_TransientInspectEscalation(t *testing.T) {
	t.Run("escalates_after_max_errors", func(t *testing.T) {
		rt := mock.New()
		lc := &stubLifecycle{}
		store := &stubCapsuleStore{spec: defaultSpec()}

		h := runtime.NewHandler(rt,
			runtime.WithCapsuleStore(store),
			runtime.WithLifecycleNotifier(lc),
			runtime.WithReconcileInterval(5*time.Millisecond),
			runtime.WithMaxInspectErrors(3),
		)
		h.Start(context.Background())
		defer h.Stop()

		h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})
		lc.waitRunning(t, "cap1", 5*time.Second)

		// Inject a persistent transient (non-sentinel) error.
		rt.SetInspectError("falak-cap1-0", errors.New("dial unix: connection refused"))

		lc.waitFailed(t, "cap1", 5*time.Second)
	})

	t.Run("single_blip_does_not_escalate", func(t *testing.T) {
		rt := mock.New()
		lc := &stubLifecycle{}
		store := &stubCapsuleStore{spec: defaultSpec()}

		h := runtime.NewHandler(rt,
			runtime.WithCapsuleStore(store),
			runtime.WithLifecycleNotifier(lc),
			runtime.WithReconcileInterval(5*time.Millisecond),
			runtime.WithMaxInspectErrors(5),
		)
		h.Start(context.Background())
		defer h.Stop()

		h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})
		lc.waitRunning(t, "cap1", 5*time.Second)

		// One blip, then clear it before maxInspectErrors is reached.
		rt.SetInspectError("falak-cap1-0", errors.New("temporary blip"))
		time.Sleep(8 * time.Millisecond) // ~1 reconcile tick
		rt.SetInspectError("falak-cap1-0", nil)

		// Let several reconcile ticks run after recovery.
		time.Sleep(60 * time.Millisecond)

		if lc.failedCount("cap1") != 0 {
			t.Fatalf("single blip must not escalate; failures=%d", lc.failedCount("cap1"))
		}
	})
}
