// Package momentum provides momentum tracking, dynamic adjustment, and lifecycle
// state machine for capsules. Momentum is a priority/weight value that affects
// election priority, eviction order, and scaling decisions.
package momentum

import (
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
)

// Tracker manages the momentum state for a capsule.
type Tracker struct {
	mu        sync.RWMutex
	capsuleID capsule.CapsuleID
	state     capsule.MomentumState
	config    capsule.MomentumConfig
	logger    *zap.Logger

	// Bounds
	minMomentum int32
	maxMomentum int32

	// Boost/reduce amounts (configurable)
	boostAmount  int32
	reduceAmount int32
}

// TrackerOption configures a Tracker.
type TrackerOption func(*Tracker)

// WithTrackerLogger sets the logger.
func WithTrackerLogger(logger *zap.Logger) TrackerOption {
	return func(t *Tracker) {
		t.logger = logger
	}
}

// WithTrackerCapsuleID attaches the owning capsule ID for observability.
func WithTrackerCapsuleID(id capsule.CapsuleID) TrackerOption {
	return func(t *Tracker) {
		t.capsuleID = id
	}
}

// WithMinMomentum sets the minimum momentum value.
func WithMinMomentum(min int32) TrackerOption {
	return func(t *Tracker) {
		t.minMomentum = min
	}
}

// WithMaxMomentum sets the maximum momentum value.
func WithMaxMomentum(max int32) TrackerOption {
	return func(t *Tracker) {
		t.maxMomentum = max
	}
}

// WithBoostAmount sets how much momentum increases on boost.
func WithBoostAmount(amount int32) TrackerOption {
	return func(t *Tracker) {
		t.boostAmount = amount
	}
}

// WithReduceAmount sets how much momentum decreases on reduce.
func WithReduceAmount(amount int32) TrackerOption {
	return func(t *Tracker) {
		t.reduceAmount = amount
	}
}

// NewTracker creates a new momentum tracker for a capsule.
func NewTracker(config capsule.MomentumConfig, opts ...TrackerOption) *Tracker {
	now := time.Now()
	t := &Tracker{
		config: config,
		state: capsule.MomentumState{
			Current:      config.Base,
			Base:         config.Base,
			LastAdjusted: now,
		},
		logger:       zap.NewNop(),
		minMomentum:  0,
		maxMomentum:  100,
		boostAmount:  5,
		reduceAmount: 5,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// NewTrackerFromTier creates a tracker using tier defaults.
func NewTrackerFromTier(tier capsule.Tier, opts ...TrackerOption) *Tracker {
	return NewTracker(capsule.MomentumConfig{
		Base:           tier.BaseMomentum(),
		BoostOnTraffic: true,
		ReduceOnIdle:   true,
		IdleTimeout:    5 * time.Minute,
	}, opts...)
}

// State returns the current momentum state.
func (t *Tracker) State() capsule.MomentumState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state
}

// Current returns the current momentum value.
func (t *Tracker) Current() int32 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state.Current
}

// Boost increases momentum (e.g., due to traffic).
// Returns true if momentum actually changed.
func (t *Tracker) Boost() bool {
	if !t.config.BoostOnTraffic {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	old := t.state.Current
	t.state.Current = t.clamp(t.state.Current + t.boostAmount)
	t.state.LastAdjusted = time.Now()

	changed := t.state.Current != old
	if changed {
		t.logger.Debug("momentum boosted",
			zap.String("capsule_id", string(t.capsuleID)),
			zap.Int32("old", old),
			zap.Int32("new", t.state.Current))
	}
	return changed
}

// Reduce decreases momentum (e.g., due to idle or failure).
// Returns true if momentum actually changed.
func (t *Tracker) Reduce() bool {
	if !t.config.ReduceOnIdle {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	old := t.state.Current
	t.state.Current = t.clamp(t.state.Current - t.reduceAmount)
	t.state.LastAdjusted = time.Now()

	changed := t.state.Current != old
	if changed {
		t.logger.Debug("momentum reduced",
			zap.String("capsule_id", string(t.capsuleID)),
			zap.Int32("old", old),
			zap.Int32("new", t.state.Current))
	}
	return changed
}

// Set directly sets the momentum to a specific value.
func (t *Tracker) Set(value int32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.Current = t.clamp(value)
	t.state.LastAdjusted = time.Now()
}

// Reset restores momentum to the base value.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.Current = t.state.Base
	t.state.LastAdjusted = time.Now()
}

// IsIdle returns true if the capsule has been idle longer than the configured timeout.
func (t *Tracker) IsIdle(now time.Time) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.config.IdleTimeout == 0 {
		return false
	}
	return now.Sub(t.state.LastAdjusted) > t.config.IdleTimeout
}

// Config returns the momentum configuration.
func (t *Tracker) Config() capsule.MomentumConfig {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.config
}

func (t *Tracker) clamp(value int32) int32 {
	if value < t.minMomentum {
		return t.minMomentum
	}
	if value > t.maxMomentum {
		return t.maxMomentum
	}
	return value
}
