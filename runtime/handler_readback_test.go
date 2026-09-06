package runtime_test

import (
	"testing"
	"time"

	"github.com/tareksalem/falak/runtime"
	"github.com/tareksalem/falak/runtime/mock"
)

// closedTickerFactory returns a ticker factory whose channel is already
// closed, so the readback poll advances immediately on every iteration
// without real sleeps — deterministic and race-free.
func closedTickerFactory() func(d time.Duration) (<-chan time.Time, func()) {
	return func(d time.Duration) (<-chan time.Time, func()) {
		ch := make(chan time.Time)
		close(ch)
		return ch, func() {}
	}
}

// recordingLifecycle records the ORDER of lifecycle callbacks so the readback
// tests can assert that the network record lands before the Running announce.
// It implements runtime.ReplicaNetworkRecorder in addition to
// runtime.LifecycleNotifier.
type recordingLifecycle struct {
	mu        chan struct{} // used as a 1-slot mutex to keep the stub tiny
	events    []string
	lastPorts []runtime.PortBinding
	lastIP    string
	failedCat runtime.FailureCategory
}

func newRecordingLifecycle() *recordingLifecycle {
	l := &recordingLifecycle{mu: make(chan struct{}, 1)}
	l.mu <- struct{}{}
	return l
}

func (l *recordingLifecycle) lock()   { <-l.mu }
func (l *recordingLifecycle) unlock() { l.mu <- struct{}{} }

func (l *recordingLifecycle) MarkRunning(capsuleID string) error {
	l.lock()
	defer l.unlock()
	l.events = append(l.events, "running")
	return nil
}

func (l *recordingLifecycle) MarkFailed(capsuleID, reason string, category runtime.FailureCategory) error {
	l.lock()
	defer l.unlock()
	l.events = append(l.events, "failed")
	l.failedCat = category
	return nil
}

func (l *recordingLifecycle) MarkStopped(capsuleID string) error {
	l.lock()
	defer l.unlock()
	l.events = append(l.events, "stopped")
	return nil
}

func (l *recordingLifecycle) RecordReplicaNetwork(capsuleID, replicaID, ip string, ports []runtime.PortBinding) error {
	l.lock()
	defer l.unlock()
	l.events = append(l.events, "record")
	l.lastIP = ip
	l.lastPorts = append([]runtime.PortBinding(nil), ports...)
	return nil
}

func (l *recordingLifecycle) snapshot() (events []string, ip string, ports []runtime.PortBinding, cat runtime.FailureCategory) {
	l.lock()
	defer l.unlock()
	return append([]string(nil), l.events...), l.lastIP, append([]runtime.PortBinding(nil), l.lastPorts...), l.failedCat
}

func (l *recordingLifecycle) waitFor(t *testing.T, event string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		evs, _, _, _ := l.snapshot()
		for _, e := range evs {
			if e == event {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for lifecycle event %q; saw %v", event, evs)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func portSpec(name string, host, container uint16, proto string) runtime.PortMapping {
	return runtime.PortMapping{Name: name, HostPort: host, ContainerPort: container, Protocol: proto}
}

func newReadbackHandler(t *testing.T, rt *mock.Runtime, spec *runtime.CapsuleSpec, lc runtime.LifecycleNotifier) *runtime.Handler {
	t.Helper()
	h := runtime.NewHandler(rt,
		runtime.WithCapsuleStore(&stubCapsuleStore{spec: spec}),
		runtime.WithLifecycleNotifier(lc),
		runtime.WithPortReadbackTicker(closedTickerFactory()),
		runtime.WithPortReadbackBudget(200*time.Millisecond),
		runtime.WithPortReadbackInterval(10*time.Millisecond),
	)
	return h
}

// TestReadback_RecordsHostPortAfterItPopulates covers the empty-then-populated
// sequence: the host-port DNAT lands a moment after the IP, so early Inspects
// see an empty host port. The handler must keep polling and record the ACTUAL
// host port (not 0), and must not announce Running before the record.
func TestReadback_RecordsHostPortAfterItPopulates(t *testing.T) {
	t.Parallel()

	rt := mock.New()
	cID := "falak-cap1-0"
	// IP appears at tick 2, host port only at tick 3.
	rt.SetInspectNetworkSequence(cID,
		[]string{"", "10.0.0.5", "10.0.0.5"},
		[][]runtime.PortMapping{
			nil,
			{{ContainerPort: 80, HostPort: 0}},
			{{ContainerPort: 80, HostPort: 32768}},
		},
	)

	spec := &runtime.CapsuleSpec{
		Name:        "web",
		Image:       "img:v1",
		NetworkMode: runtime.NetworkModeEnum.Bridge(),
		Ports:       []runtime.PortMapping{portSpec("http", 0, 80, "tcp")}, // auto
	}
	lc := newRecordingLifecycle()
	h := newReadbackHandler(t, rt, spec, lc)
	h.Start(t.Context())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})

	lc.waitFor(t, "running", 2*time.Second)
	events, ip, ports, _ := lc.snapshot()

	// Record must precede the Running announce.
	recordIdx, runningIdx := -1, -1
	for i, e := range events {
		if e == "record" && recordIdx == -1 {
			recordIdx = i
		}
		if e == "running" && runningIdx == -1 {
			runningIdx = i
		}
	}
	if recordIdx == -1 || runningIdx == -1 || recordIdx > runningIdx {
		t.Fatalf("expected record before running; events=%v", events)
	}
	if ip != "10.0.0.5" {
		t.Errorf("recorded ip = %q, want 10.0.0.5", ip)
	}
	if len(ports) != 1 || ports[0].HostPort != 32768 || ports[0].ContainerPort != 80 {
		t.Fatalf("recorded ports = %+v, want [{http 80 32768}]", ports)
	}
}

// TestReadback_FixedPortUnboundFailsStart covers a user-specified fixed host
// port that never binds within the budget (e.g. a cross-node collision): the
// handler must fail the start (MarkFailed, NodeAttributable) and tear the
// container down, NOT announce Running.
func TestReadback_FixedPortUnboundFailsStart(t *testing.T) {
	t.Parallel()

	rt := mock.New()
	cID := "falak-cap1-0"
	// IP present but the fixed host port never appears.
	rt.SetInspectNetworkSequence(cID,
		[]string{"10.0.0.5"},
		[][]runtime.PortMapping{nil},
	)

	spec := &runtime.CapsuleSpec{
		Name:        "web",
		Image:       "img:v1",
		NetworkMode: runtime.NetworkModeEnum.Bridge(),
		Ports:       []runtime.PortMapping{portSpec("http", 8080, 80, "tcp")}, // FIXED
	}
	lc := newRecordingLifecycle()
	h := newReadbackHandler(t, rt, spec, lc)
	h.Start(t.Context())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})

	lc.waitFor(t, "failed", 2*time.Second)
	events, _, _, cat := lc.snapshot()
	for _, e := range events {
		if e == "running" {
			t.Fatalf("fixed-port-unbound must NOT announce Running; events=%v", events)
		}
	}
	if cat != runtime.FailureCategoryEnum.NodeAttributable() {
		t.Errorf("failure category = %v, want NodeAttributable", cat)
	}
	// Container must have been torn down.
	if rt.ContainerCount() != 0 {
		t.Errorf("container not removed after fixed-port failure; count=%d", rt.ContainerCount())
	}
}

// TestReadback_AutoPortUnboundWarnsAndContinues covers an auto host port that
// never resolves within the budget: the container is mesh-reachable regardless,
// so the handler records 0 and continues to Running — it must NOT MarkFailed.
func TestReadback_AutoPortUnboundWarnsAndContinues(t *testing.T) {
	t.Parallel()

	rt := mock.New()
	cID := "falak-cap1-0"
	rt.SetInspectNetworkSequence(cID,
		[]string{"10.0.0.5"},
		[][]runtime.PortMapping{nil},
	)

	spec := &runtime.CapsuleSpec{
		Name:        "web",
		Image:       "img:v1",
		NetworkMode: runtime.NetworkModeEnum.Bridge(),
		Ports:       []runtime.PortMapping{portSpec("metrics", 0, 9090, "tcp")}, // AUTO
	}
	lc := newRecordingLifecycle()
	h := newReadbackHandler(t, rt, spec, lc)
	h.Start(t.Context())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})

	lc.waitFor(t, "running", 2*time.Second)
	events, _, ports, _ := lc.snapshot()
	for _, e := range events {
		if e == "failed" {
			t.Fatalf("auto-port-unbound must NOT MarkFailed; events=%v", events)
		}
	}
	if len(ports) != 1 || ports[0].HostPort != 0 || ports[0].ContainerPort != 9090 {
		t.Fatalf("recorded ports = %+v, want [{metrics 9090 0}]", ports)
	}
	if rt.ContainerCount() != 1 {
		t.Errorf("working container must not be torn down; count=%d", rt.ContainerCount())
	}
}

// TestReadback_HappyPathBoundImmediately confirms a port bound on the first
// Inspect converges without extra polling and records the concrete host port.
func TestReadback_HappyPathBoundImmediately(t *testing.T) {
	t.Parallel()

	rt := mock.New()
	cID := "falak-cap1-0"
	rt.SetInspectNetworkSequence(cID,
		[]string{"10.0.0.9"},
		[][]runtime.PortMapping{{{ContainerPort: 80, HostPort: 8080}}},
	)

	spec := &runtime.CapsuleSpec{
		Name:        "web",
		Image:       "img:v1",
		NetworkMode: runtime.NetworkModeEnum.Bridge(),
		Ports:       []runtime.PortMapping{portSpec("http", 8080, 80, "tcp")}, // fixed, binds
	}
	lc := newRecordingLifecycle()
	h := newReadbackHandler(t, rt, spec, lc)
	h.Start(t.Context())
	defer h.Stop()

	h.HandleElectionWon(runtime.ElectionWon{CapsuleID: "cap1", ReplicaID: "0"})

	lc.waitFor(t, "running", 2*time.Second)
	_, ip, ports, _ := lc.snapshot()
	if ip != "10.0.0.9" {
		t.Errorf("recorded ip = %q, want 10.0.0.9", ip)
	}
	if len(ports) != 1 || ports[0].HostPort != 8080 {
		t.Fatalf("recorded ports = %+v, want host 8080", ports)
	}
}
