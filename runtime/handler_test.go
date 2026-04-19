package runtime_test

import (
	"context"
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
	mu      sync.Mutex
	running []string
	failed  []string
	stopped []string
}

func (l *stubLifecycle) MarkRunning(capsuleID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running = append(l.running, capsuleID)
	return nil
}

func (l *stubLifecycle) MarkFailed(capsuleID, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed = append(l.failed, capsuleID)
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

func TestHandler_CrashTriggersMarkFailed(t *testing.T) {
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

	// Simulate container crash.
	rt.SimulateCrash("falak-cap1-0", 137)

	// Wait for the watcher to detect the crash (polls every 2s).
	deadline := time.After(10 * time.Second)
	for {
		lc.mu.Lock()
		failedCount := len(lc.failed)
		lc.mu.Unlock()
		if failedCount > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for MarkFailed after crash")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
