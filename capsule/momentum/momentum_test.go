package momentum

import (
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
)

func TestTrackerDefaults(t *testing.T) {
	tracker := NewTrackerFromTier(capsule.TierEnum.Critical())
	if tracker.Current() != 90 {
		t.Errorf("critical tier should start at 90, got %d", tracker.Current())
	}

	tracker = NewTrackerFromTier(capsule.TierEnum.Standard())
	if tracker.Current() != 50 {
		t.Errorf("standard tier should start at 50, got %d", tracker.Current())
	}

	tracker = NewTrackerFromTier(capsule.TierEnum.Background())
	if tracker.Current() != 20 {
		t.Errorf("background tier should start at 20, got %d", tracker.Current())
	}
}

func TestTrackerBoost(t *testing.T) {
	tracker := NewTracker(capsule.MomentumConfig{
		Base:           50,
		BoostOnTraffic: true,
	}, WithBoostAmount(10))

	old := tracker.Current()
	changed := tracker.Boost()
	if !changed {
		t.Error("boost should change momentum")
	}
	if tracker.Current() != old+10 {
		t.Errorf("expected %d, got %d", old+10, tracker.Current())
	}
}

func TestTrackerBoostDisabled(t *testing.T) {
	tracker := NewTracker(capsule.MomentumConfig{
		Base:           50,
		BoostOnTraffic: false,
	})

	changed := tracker.Boost()
	if changed {
		t.Error("boost should not change when disabled")
	}
}

func TestTrackerReduce(t *testing.T) {
	tracker := NewTracker(capsule.MomentumConfig{
		Base:         50,
		ReduceOnIdle: true,
	}, WithReduceAmount(10))

	old := tracker.Current()
	changed := tracker.Reduce()
	if !changed {
		t.Error("reduce should change momentum")
	}
	if tracker.Current() != old-10 {
		t.Errorf("expected %d, got %d", old-10, tracker.Current())
	}
}

func TestTrackerClamp(t *testing.T) {
	tracker := NewTracker(capsule.MomentumConfig{
		Base:           95,
		BoostOnTraffic: true,
	}, WithBoostAmount(10), WithMaxMomentum(100))

	tracker.Boost()
	if tracker.Current() != 100 {
		t.Errorf("should clamp at 100, got %d", tracker.Current())
	}

	tracker2 := NewTracker(capsule.MomentumConfig{
		Base:         5,
		ReduceOnIdle: true,
	}, WithReduceAmount(10), WithMinMomentum(0))

	tracker2.Reduce()
	if tracker2.Current() != 0 {
		t.Errorf("should clamp at 0, got %d", tracker2.Current())
	}
}

func TestTrackerReset(t *testing.T) {
	tracker := NewTracker(capsule.MomentumConfig{
		Base:           50,
		BoostOnTraffic: true,
	}, WithBoostAmount(10))

	tracker.Boost()
	tracker.Boost()
	if tracker.Current() == 50 {
		t.Error("should have increased after boosts")
	}

	tracker.Reset()
	if tracker.Current() != 50 {
		t.Errorf("should reset to base 50, got %d", tracker.Current())
	}
}

func TestTrackerIsIdle(t *testing.T) {
	tracker := NewTracker(capsule.MomentumConfig{
		Base:        50,
		IdleTimeout: 100 * time.Millisecond,
	})

	if tracker.IsIdle(time.Now()) {
		t.Error("should not be idle immediately")
	}

	time.Sleep(150 * time.Millisecond)
	if !tracker.IsIdle(time.Now()) {
		t.Error("should be idle after timeout")
	}
}

func TestTrackerSet(t *testing.T) {
	tracker := NewTracker(capsule.MomentumConfig{Base: 50})
	tracker.Set(75)
	if tracker.Current() != 75 {
		t.Errorf("expected 75, got %d", tracker.Current())
	}
}
