package node

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/network"
	"github.com/tareksalem/falak/node/internal/events"
)

// fakeNetworkManager implements NetworkManagerLike and records every
// callback the EventSource fires so tests can assert translation
// correctness. starts/stops are atomic so the race detector covers the
// concurrent Stop path.
type fakeNetworkManager struct {
	mu sync.Mutex

	starts int32
	stops  int32

	startErr error
	stopErr  error

	received []network.CapsuleEvent
	running  []network.CapsuleEvent
	stopped  []network.CapsuleEvent
	deleted  []network.CapsuleEvent
	joined   []network.CapsuleEvent
	left     []network.CapsuleEvent

	cancels []func()
}

func (f *fakeNetworkManager) Start(ctx context.Context) error {
	atomic.AddInt32(&f.starts, 1)
	return f.startErr
}

func (f *fakeNetworkManager) Stop() error {
	atomic.AddInt32(&f.stops, 1)
	return f.stopErr
}

// wireSource captures every callback so the adapter is exercised
// end-to-end (Subscribe → goroutine → fanout → callback).
func (f *fakeNetworkManager) wireSource(src network.EventSource) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels,
		src.OnCapsuleReceived(func(ev network.CapsuleEvent) { f.appendTo(&f.received, ev) }),
		src.OnCapsuleRunning(func(ev network.CapsuleEvent) { f.appendTo(&f.running, ev) }),
		src.OnCapsuleStopped(func(ev network.CapsuleEvent) { f.appendTo(&f.stopped, ev) }),
		src.OnCapsuleDeleted(func(ev network.CapsuleEvent) { f.appendTo(&f.deleted, ev) }),
		src.OnPeerJoined(func(ev network.CapsuleEvent) { f.appendTo(&f.joined, ev) }),
		src.OnPeerLeft(func(ev network.CapsuleEvent) { f.appendTo(&f.left, ev) }),
	)
}

func (f *fakeNetworkManager) appendTo(list *[]network.CapsuleEvent, ev network.CapsuleEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*list = append(*list, ev)
}

func (f *fakeNetworkManager) snapshot(name string) []network.CapsuleEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch name {
	case "received":
		return append([]network.CapsuleEvent(nil), f.received...)
	case "running":
		return append([]network.CapsuleEvent(nil), f.running...)
	case "stopped":
		return append([]network.CapsuleEvent(nil), f.stopped...)
	case "deleted":
		return append([]network.CapsuleEvent(nil), f.deleted...)
	case "joined":
		return append([]network.CapsuleEvent(nil), f.joined...)
	case "left":
		return append([]network.CapsuleEvent(nil), f.left...)
	}
	return nil
}

// validHandlerConfig returns a NetworkConfig whose required fields are
// populated so validateConfig accepts it without modification.
func validHandlerConfig() NetworkConfig {
	return NetworkConfig{
		Enabled:           true,
		ClusterPath:       "test/dc/cluster",
		LocalNodeID:       "node-A",
		LocalIP:           "10.0.0.1",
		ClusterRootKey:    []byte("0123456789abcdef0123456789abcdef"),
		StatePath:         "/tmp/test-network",
		BridgeSubnetPool:  DefaultBridgeSubnetPool,
		DependencyTimeout: DefaultGroupDependencyTimeout,
		RetryCap:          1,
	}
}

// waitFor polls fn until it returns true or the timeout expires.
func waitFor(t *testing.T, fn func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("waitFor: condition did not become true")
}

// TestNetworkHandler_DisabledByConfig verifies that Enabled=false skips
// the entire subsystem cleanly: no factory call, no manager start, no
// errors out of Start/Stop.
func TestNetworkHandler_DisabledByConfig(t *testing.T) {
	factoryCalls := int32(0)
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	h := NewNetworkHandler(
		WithNetworkEnabled(false),
		WithNetworkHandlerEventBus(bus),
		WithNetworkManagerFactory(func(cfg NetworkConfig, src network.EventSource, l *zap.Logger) (NetworkManagerLike, error) {
			atomic.AddInt32(&factoryCalls, 1)
			return &fakeNetworkManager{}, nil
		}),
	)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start (disabled): %v", err)
	}
	if h.Enabled() {
		t.Fatal("Enabled() must be false when Enabled config is false")
	}
	if atomic.LoadInt32(&factoryCalls) != 0 {
		t.Fatalf("factory must not be called when disabled, got %d", factoryCalls)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop (disabled): %v", err)
	}
}

// TestNetworkHandler_NoFactoryWarns verifies the factory-missing path
// logs and skips without erroring.
func TestNetworkHandler_NoFactoryWarns(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	h := NewNetworkHandler(
		WithNetworkEnabled(true),
		WithNetworkHandlerEventBus(bus),
	)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start (no factory): %v", err)
	}
	if h.Enabled() {
		t.Fatal("Enabled must be false when factory missing")
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestNetworkHandler_InvalidConfigSkipped exercises every required
// field: when missing, the handler logs and skips without returning
// error.
func TestNetworkHandler_InvalidConfigSkipped(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	cases := []struct {
		name string
		mut  func(*NetworkConfig)
	}{
		{"no cluster path", func(c *NetworkConfig) { c.ClusterPath = "" }},
		{"no node id", func(c *NetworkConfig) { c.LocalNodeID = "" }},
		{"no local ip", func(c *NetworkConfig) { c.LocalIP = "" }},
		{"no cluster root key", func(c *NetworkConfig) { c.ClusterRootKey = nil }},
		{"no state path", func(c *NetworkConfig) { c.StatePath = "" }},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := validHandlerConfig()
			tc.mut(&cfg)

			factoryCalls := int32(0)
			h := NewNetworkHandler(
				WithNetworkEnabled(cfg.Enabled),
				WithNetworkClusterPath(cfg.ClusterPath),
				WithNetworkLocalNodeID(cfg.LocalNodeID),
				WithNetworkLocalIP(cfg.LocalIP),
				WithNetworkClusterRootKey(cfg.ClusterRootKey),
				WithNetworkStatePath(cfg.StatePath),
				WithNetworkHandlerEventBus(bus),
				WithNetworkManagerFactory(func(_ NetworkConfig, _ network.EventSource, _ *zap.Logger) (NetworkManagerLike, error) {
					atomic.AddInt32(&factoryCalls, 1)
					return &fakeNetworkManager{}, nil
				}),
			)
			if err := h.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if atomic.LoadInt32(&factoryCalls) != 0 {
				t.Fatalf("factory must not be invoked on invalid config (%s)", tc.name)
			}
			_ = h.Stop()
		})
	}
}

// TestNetworkHandler_TranslatesCapsuleReceived verifies that a
// CapsuleReceived bus event fires the manager's OnCapsuleReceived
// callback with the matching cluster + capsule name.
func TestNetworkHandler_TranslatesCapsuleReceived(t *testing.T) {
	fake := &fakeNetworkManager{}
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	cfg := validHandlerConfig()
	h := NewNetworkHandler(
		WithNetworkEnabled(cfg.Enabled),
		WithNetworkClusterPath(cfg.ClusterPath),
		WithNetworkLocalNodeID(cfg.LocalNodeID),
		WithNetworkLocalIP(cfg.LocalIP),
		WithNetworkClusterRootKey(cfg.ClusterRootKey),
		WithNetworkStatePath(cfg.StatePath),
		WithNetworkHandlerEventBus(bus),
		WithNetworkManagerFactory(func(_ NetworkConfig, src network.EventSource, _ *zap.Logger) (NetworkManagerLike, error) {
			fake.wireSource(src)
			return fake, nil
		}),
	)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop() })

	bus.Publish(events.CapsuleReceived{
		BaseEvent:   events.NewBaseEvent(),
		CapsuleID:   "cap-1",
		CapsuleName: "api",
		ClusterPath: cfg.ClusterPath,
		Orbit:       "api",
	})

	waitFor(t, func() bool { return len(fake.snapshot("received")) == 1 }, 2*time.Second)
	got := fake.snapshot("received")[0]
	if got.ClusterPath != cfg.ClusterPath {
		t.Fatalf("cluster: want %q, got %q", cfg.ClusterPath, got.ClusterPath)
	}
	if got.CapsuleName != "api" {
		t.Fatalf("capsule name: want api, got %q", got.CapsuleName)
	}
}

// TestNetworkHandler_TranslatesCapsuleDeleted verifies that the
// withdrawal-path event fires the manager's OnCapsuleDeleted callback.
func TestNetworkHandler_TranslatesCapsuleDeleted(t *testing.T) {
	fake := &fakeNetworkManager{}
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	cfg := validHandlerConfig()
	h := NewNetworkHandler(
		WithNetworkEnabled(cfg.Enabled),
		WithNetworkClusterPath(cfg.ClusterPath),
		WithNetworkLocalNodeID(cfg.LocalNodeID),
		WithNetworkLocalIP(cfg.LocalIP),
		WithNetworkClusterRootKey(cfg.ClusterRootKey),
		WithNetworkStatePath(cfg.StatePath),
		WithNetworkHandlerEventBus(bus),
		WithNetworkManagerFactory(func(_ NetworkConfig, src network.EventSource, _ *zap.Logger) (NetworkManagerLike, error) {
			fake.wireSource(src)
			return fake, nil
		}),
	)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop() })

	bus.Publish(events.CapsuleWithdrawn{
		BaseEvent:   events.NewBaseEvent(),
		CapsuleID:   "cap-1",
		ClusterPath: cfg.ClusterPath,
		Reason:      "operator delete",
	})

	waitFor(t, func() bool { return len(fake.snapshot("deleted")) == 1 }, 2*time.Second)
}

// TestNetworkHandler_TranslatesPeerJoinedAndLeft verifies that
// NewMemberReceived translates to OnPeerJoined (with peer != self) and
// NodeFailed translates to OnPeerLeft, while self-events are filtered.
func TestNetworkHandler_TranslatesPeerJoinedAndLeft(t *testing.T) {
	fake := &fakeNetworkManager{}
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	cfg := validHandlerConfig()
	h := NewNetworkHandler(
		WithNetworkEnabled(cfg.Enabled),
		WithNetworkClusterPath(cfg.ClusterPath),
		WithNetworkLocalNodeID(cfg.LocalNodeID),
		WithNetworkLocalIP(cfg.LocalIP),
		WithNetworkClusterRootKey(cfg.ClusterRootKey),
		WithNetworkStatePath(cfg.StatePath),
		WithNetworkHandlerEventBus(bus),
		WithNetworkManagerFactory(func(_ NetworkConfig, src network.EventSource, _ *zap.Logger) (NetworkManagerLike, error) {
			fake.wireSource(src)
			return fake, nil
		}),
	)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop() })

	// Peer-join event from a remote node.
	bus.Publish(events.NewMemberReceived{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      "node-B",
		ClusterPath: cfg.ClusterPath,
		Addresses:   []string{"/ip4/10.0.0.2/tcp/4001"},
	})
	waitFor(t, func() bool { return len(fake.snapshot("joined")) == 1 }, 2*time.Second)
	got := fake.snapshot("joined")[0]
	if got.NodeID != "node-B" {
		t.Fatalf("peer joined node id: want node-B, got %q", got.NodeID)
	}
	if got.NodeIP != "10.0.0.2" {
		t.Fatalf("peer joined node ip: want 10.0.0.2, got %q", got.NodeIP)
	}

	// Self-event should be filtered.
	bus.Publish(events.NewMemberReceived{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      cfg.LocalNodeID, // self
		ClusterPath: cfg.ClusterPath,
		Addresses:   []string{"/ip4/10.0.0.1/tcp/4001"},
	})
	// Wait briefly to ensure event drained without firing again.
	time.Sleep(100 * time.Millisecond)
	if got := fake.snapshot("joined"); len(got) != 1 {
		t.Fatalf("self peer joined leaked: want 1 entry, got %d", len(got))
	}

	// Peer-failed event.
	bus.Publish(events.NodeFailed{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      "node-B",
		ClusterPath: cfg.ClusterPath,
	})
	waitFor(t, func() bool { return len(fake.snapshot("left")) == 1 }, 2*time.Second)
}

// TestNetworkHandler_StopDrainsAdapter verifies that Stop blocks until
// the per-event goroutines exit AND then the manager.Stop runs. After
// Stop, further bus publishes do not call the manager again.
func TestNetworkHandler_StopDrainsAdapter(t *testing.T) {
	fake := &fakeNetworkManager{}
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	cfg := validHandlerConfig()
	h := NewNetworkHandler(
		WithNetworkEnabled(cfg.Enabled),
		WithNetworkClusterPath(cfg.ClusterPath),
		WithNetworkLocalNodeID(cfg.LocalNodeID),
		WithNetworkLocalIP(cfg.LocalIP),
		WithNetworkClusterRootKey(cfg.ClusterRootKey),
		WithNetworkStatePath(cfg.StatePath),
		WithNetworkHandlerEventBus(bus),
		WithNetworkManagerFactory(func(_ NetworkConfig, src network.EventSource, _ *zap.Logger) (NetworkManagerLike, error) {
			fake.wireSource(src)
			return fake, nil
		}),
	)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if atomic.LoadInt32(&fake.stops) != 1 {
		t.Fatalf("manager.Stop call count: want 1, got %d", fake.stops)
	}

	// Publish after Stop — must not reach the callbacks.
	bus.Publish(events.CapsuleReceived{
		BaseEvent:   events.NewBaseEvent(),
		CapsuleID:   "cap-z",
		CapsuleName: "post-stop",
		ClusterPath: cfg.ClusterPath,
	})
	time.Sleep(50 * time.Millisecond)
	if got := fake.snapshot("received"); len(got) != 0 {
		t.Fatalf("post-stop event leaked: %+v", got)
	}

	// Second Stop is a no-op.
	if err := h.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestNetworkHandler_FactoryErrorReturned exercises the path where the
// factory itself fails; Start must return the wrapped error and Stop
// after a failed Start must be a clean no-op (no manager to stop).
func TestNetworkHandler_FactoryErrorReturned(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	want := errors.New("factory boom")
	cfg := validHandlerConfig()
	h := NewNetworkHandler(
		WithNetworkEnabled(cfg.Enabled),
		WithNetworkClusterPath(cfg.ClusterPath),
		WithNetworkLocalNodeID(cfg.LocalNodeID),
		WithNetworkLocalIP(cfg.LocalIP),
		WithNetworkClusterRootKey(cfg.ClusterRootKey),
		WithNetworkStatePath(cfg.StatePath),
		WithNetworkHandlerEventBus(bus),
		WithNetworkManagerFactory(func(_ NetworkConfig, _ network.EventSource, _ *zap.Logger) (NetworkManagerLike, error) {
			return nil, want
		}),
	)
	if err := h.Start(context.Background()); err == nil || !errors.Is(err, want) {
		t.Fatalf("Start: want wrap of %v, got %v", want, err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop after failed Start: %v", err)
	}
}

// TestNetworkHandler_ManagerStartRetriesThenSucceeds verifies that the
// retry loop honors RetryCap and reaches eventual success.
func TestNetworkHandler_ManagerStartRetriesThenSucceeds(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(bus.Close)

	fake := &retryingFakeManager{failCount: 2}
	cfg := validHandlerConfig()
	cfg.RetryCap = 3
	h := NewNetworkHandler(
		WithNetworkEnabled(cfg.Enabled),
		WithNetworkClusterPath(cfg.ClusterPath),
		WithNetworkLocalNodeID(cfg.LocalNodeID),
		WithNetworkLocalIP(cfg.LocalIP),
		WithNetworkClusterRootKey(cfg.ClusterRootKey),
		WithNetworkStatePath(cfg.StatePath),
		WithNetworkRetryCap(cfg.RetryCap),
		WithNetworkHandlerEventBus(bus),
		WithNetworkManagerFactory(func(_ NetworkConfig, _ network.EventSource, _ *zap.Logger) (NetworkManagerLike, error) {
			return fake, nil
		}),
	)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := atomic.LoadInt32(&fake.attempts); got != 3 {
		t.Fatalf("start attempts: want 3, got %d", got)
	}
	_ = h.Stop()
}

// retryingFakeManager is a NetworkManagerLike that fails the first
// failCount Start calls then succeeds.
type retryingFakeManager struct {
	attempts  int32
	failCount int32
}

func (r *retryingFakeManager) Start(ctx context.Context) error {
	n := atomic.AddInt32(&r.attempts, 1)
	if n <= r.failCount {
		return errors.New("transient start failure")
	}
	return nil
}

func (r *retryingFakeManager) Stop() error { return nil }
