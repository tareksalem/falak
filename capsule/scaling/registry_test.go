package scaling

import (
	"testing"

	"github.com/tareksalem/falak/capsule"
)

// fakeProvider is a test MetricsProvider returning fixed values.
type fakeProvider struct {
	values map[string]float64
}

func (f *fakeProvider) GetMetric(name string) (float64, bool) {
	v, ok := f.values[name]
	return v, ok
}

func TestInMemoryRegistry_RegisterAndGet(t *testing.T) {
	reg := NewInMemoryRegistry()
	id := capsule.CapsuleID("abc")

	if reg.Get(id) != nil {
		t.Error("Get should return nil for unregistered capsule")
	}

	p := &fakeProvider{values: map[string]float64{"cpu": 50}}
	reg.Register(id, p)

	got := reg.Get(id)
	if got == nil {
		t.Fatal("Get should return the registered provider")
	}

	val, ok := got.GetMetric("cpu")
	if !ok || val != 50 {
		t.Errorf("expected cpu=50, got %f (ok=%v)", val, ok)
	}
}

func TestInMemoryRegistry_Replace(t *testing.T) {
	reg := NewInMemoryRegistry()
	id := capsule.CapsuleID("abc")

	reg.Register(id, &fakeProvider{values: map[string]float64{"cpu": 10}})
	reg.Register(id, &fakeProvider{values: map[string]float64{"cpu": 90}})

	val, _ := reg.Get(id).GetMetric("cpu")
	if val != 90 {
		t.Errorf("expected cpu=90 after replace, got %f", val)
	}
}

func TestInMemoryRegistry_Unregister(t *testing.T) {
	reg := NewInMemoryRegistry()
	id := capsule.CapsuleID("abc")

	reg.Register(id, &fakeProvider{})
	if reg.Count() != 1 {
		t.Errorf("expected count 1, got %d", reg.Count())
	}

	reg.Unregister(id)
	if reg.Count() != 0 {
		t.Errorf("expected count 0, got %d", reg.Count())
	}
	if reg.Get(id) != nil {
		t.Error("Get should return nil after Unregister")
	}
}

func TestInMemoryRegistry_UnregisterMissing(t *testing.T) {
	reg := NewInMemoryRegistry()
	// Should not panic or error
	reg.Unregister(capsule.CapsuleID("never-registered"))
}

func TestInMemoryRegistry_Concurrent(t *testing.T) {
	reg := NewInMemoryRegistry()

	done := make(chan struct{})
	const iterations = 100

	// Concurrent writes
	go func() {
		for i := 0; i < iterations; i++ {
			reg.Register(capsule.CapsuleID("a"), &fakeProvider{})
		}
		done <- struct{}{}
	}()

	// Concurrent reads
	go func() {
		for i := 0; i < iterations; i++ {
			_ = reg.Get(capsule.CapsuleID("a"))
		}
		done <- struct{}{}
	}()

	// Concurrent deletes
	go func() {
		for i := 0; i < iterations; i++ {
			reg.Unregister(capsule.CapsuleID("a"))
		}
		done <- struct{}{}
	}()

	<-done
	<-done
	<-done
}

// --- Monitor + Registry integration ---

func TestMonitor_UsesRegistry(t *testing.T) {
	reg := NewInMemoryRegistry()
	var lastEvent *ScaleEvent
	mon := NewMonitor(
		WithMonitorInterval(50_000_000), // 50ms
		WithMonitorRegistry(reg),
		WithMonitorHandler(func(e ScaleEvent) {
			lastEvent = &e
		}),
	)

	id := capsule.CapsuleID("test-capsule")
	rules := []Rule{
		{
			Name:    "high cpu",
			Trigger: capsule.TriggerModeEnum.All(),
			Conditions: []Condition{
				{Metric: "cpu", Operator: ">", Value: 70},
			},
			Action: capsule.ScalingActionEnum.ScaleUp(),
		},
	}

	mon.Register(&CapsuleMetrics{CapsuleID: id, Rules: rules})

	// Before provider is registered, the monitor should not fire
	// because the registry returns nil.
	mon.evaluate()
	if lastEvent != nil {
		t.Error("monitor should not fire without a provider")
	}

	// Register a provider with high CPU
	reg.Register(id, &fakeProvider{values: map[string]float64{"cpu": 95}})

	mon.evaluate()
	if lastEvent == nil {
		t.Fatal("monitor should fire after provider registered")
	}
	if lastEvent.Action != capsule.ScalingActionEnum.ScaleUp() {
		t.Errorf("expected ScaleUp, got %s", lastEvent.Action)
	}
}
