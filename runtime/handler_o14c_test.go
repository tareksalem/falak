package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/tareksalem/falak/runtime"
	"github.com/tareksalem/falak/runtime/mock"
)

// O14c start-vs-stop race (runtime side).
//
// A post-hoc election yield's StopContainer may race AHEAD of the async
// ElectionWon → start. The guarantee: StopContainer on a not-yet-created
// container is a clean no-op that still plants the ignore-set entry, so
// the SUBSEQUENT start (if it lands) is suppressed — no duplicate survives
// and MarkRunning never fires for the yielded replica.

// notMarkedRunning asserts MarkRunning was not called for capsuleID within
// the window (a bounded negative check — we allow the async paths time to
// run and confirm they did NOT reach MarkRunning).
func notMarkedRunning(t *testing.T, lc *stubLifecycle, capsuleID string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case <-deadline:
			return
		case <-time.After(20 * time.Millisecond):
			lc.mu.Lock()
			for _, id := range lc.running {
				if id == capsuleID {
					lc.mu.Unlock()
					t.Fatalf("MarkRunning(%s) fired but the container was yielded/stopped and must NOT run", capsuleID)
				}
			}
			lc.mu.Unlock()
		}
	}
}

// TestHandler_O14c_StopBeforeStart_Suppressed covers "stop landed before
// start began": StopContainer plants the ignore-set entry first, then
// HandleElectionWon fires. startContainer's top-of-function isIgnored guard
// must suppress the start — no image pull, no container, no MarkRunning.
func TestHandler_O14c_StopBeforeStart_Suppressed(t *testing.T) {
	rt := mock.New()
	lc := &stubLifecycle{}
	store := &stubCapsuleStore{spec: defaultSpec()}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithLifecycleNotifier(lc),
	)
	h.Start(context.Background())
	defer h.Stop()

	// Yield stop lands FIRST — plants the ignore-set entry for the container.
	// The container does not exist yet: StopContainer is a clean no-op
	// (Stop/Remove on an unknown container error, tolerated).
	_ = h.StopContainer("cap1", "0", time.Second)

	// Now the delayed ElectionWon-driven start lands.
	h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})

	// The start must be suppressed: no MarkRunning, no running container,
	// and (because startContainer bailed before pull) no image pulled.
	notMarkedRunning(t, lc, "cap1", 500*time.Millisecond)
	if rt.HasPulled("img:v1") {
		t.Error("image must NOT be pulled: the ignore-set entry should suppress the start before pull")
	}
	if rt.ContainerStatus("falak-cap1-0") == runtime.ContainerStatusEnum.Running() {
		t.Error("no container must be running after a suppressed start")
	}
}

// TestHandler_O14c_StopDuringStart_TornDown covers "stop landed DURING an
// in-flight start": the start passes the top guard and blocks in the image
// pull; StopContainer then plants the ignore-set entry; the pull completes
// and onContainerRunning must honor the live ignore entry by tearing the
// just-created container back down — NOT registering a watcher, NOT calling
// MarkRunning.
func TestHandler_O14c_StopDuringStart_TornDown(t *testing.T) {
	// Block the cold-start pull long enough to inject the stop mid-start.
	rt := mock.New(mock.WithPullDelayForImage("img:v1", 300*time.Millisecond))
	lc := &stubLifecycle{}
	store := &stubCapsuleStore{spec: defaultSpec()}

	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(store),
		runtime.WithLifecycleNotifier(lc),
	)
	h.Start(context.Background())
	defer h.Stop()

	// Start the container — it enters the pull and blocks (passes the top
	// isIgnored guard because nothing is ignored yet).
	h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})

	// While the pull is in flight, the yield stop lands: plant the ignore
	// entry. Give the start goroutine a moment to get past the top guard
	// and into the pull first.
	time.Sleep(60 * time.Millisecond)
	_ = h.StopContainer("cap1", "0", time.Second)

	// After the pull completes, onContainerRunning honors the ignore entry
	// and tears the container back down: MarkRunning must NOT fire, and the
	// container must not be left running.
	notMarkedRunning(t, lc, "cap1", 800*time.Millisecond)
	if rt.ContainerStatus("falak-cap1-0") == runtime.ContainerStatusEnum.Running() {
		t.Error("container created during the race must be torn down, not left running")
	}
}
