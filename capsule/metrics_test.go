package capsule

import (
	"context"
	"testing"
)

func TestCountingMetrics_CreateEmitsCounter(t *testing.T) {
	m := NewCountingMetrics()
	mgr := NewManager(WithManagerMetrics(m))

	if _, err := mgr.Create(context.Background(), "test/dc1", CapsuleSpec{
		Name: "test", Image: "img", Orbit: "api",
	}); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if m.Created.Load() != 1 {
		t.Errorf("expected Created=1, got %d", m.Created.Load())
	}
}

func TestCountingMetrics_TransitionIncrementsCounter(t *testing.T) {
	m := NewCountingMetrics()
	mgr := NewManager(WithManagerMetrics(m))

	// Create auto-announces so the Created → Announced transition fires
	// inside Create itself and increments the counter.
	_, _ = mgr.Create(context.Background(), "test/dc1", CapsuleSpec{
		Name: "test", Image: "img", Orbit: "api",
	})

	if m.Transitions.Load() == 0 {
		t.Error("expected Transitions > 0 after Create (auto-announce)")
	}
}

func TestCountingMetrics_ReceiveEmitsCounter(t *testing.T) {
	m := NewCountingMetrics()
	mgr := NewManager(WithManagerMetrics(m))

	c := &Capsule{
		ID:        NewCapsuleID(),
		ClusterID: "test/dc1",
		Spec:      CapsuleSpec{Name: "test", Image: "img", Orbit: "api"},
		Status:    CapsuleStatusEnum.Announced(),
	}
	DefaultSpec(&c.Spec)
	if err := mgr.Receive(c); err != nil {
		t.Fatalf("Receive failed: %v", err)
	}

	if m.Received.Load() != 1 {
		t.Errorf("expected Received=1, got %d", m.Received.Load())
	}
}

func TestNoopMetrics_CompileCheck(t *testing.T) {
	// Just verify NoopMetrics implements Metrics.
	var _ Metrics = NoopMetrics{}
	var _ Metrics = NewCountingMetrics()
}
