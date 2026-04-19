package momentum

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
)

// AdjustmentEvent is emitted when momentum changes.
type AdjustmentEvent struct {
	CapsuleID capsule.CapsuleID
	OldValue  int32
	NewValue  int32
	Reason    string // "traffic_boost", "idle_reduce", "failure_reduce", "dependency_boost"
	Timestamp time.Time
}

// AdjustmentHandler is called when a momentum adjustment occurs.
type AdjustmentHandler func(event AdjustmentEvent)

// Adjuster monitors capsule activity and adjusts momentum dynamically.
// It subscribes to traffic, idle, and failure signals and adjusts momentum accordingly.
type Adjuster struct {
	mu       sync.RWMutex
	trackers map[capsule.CapsuleID]*Tracker
	handler  AdjustmentHandler
	logger   *zap.Logger

	idleCheckInterval time.Duration
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
}

// AdjusterOption configures an Adjuster.
type AdjusterOption func(*Adjuster)

// WithAdjusterLogger sets the logger.
func WithAdjusterLogger(logger *zap.Logger) AdjusterOption {
	return func(a *Adjuster) {
		a.logger = logger
	}
}

// WithAdjustmentHandler sets the handler for momentum change events.
func WithAdjustmentHandler(h AdjustmentHandler) AdjusterOption {
	return func(a *Adjuster) {
		a.handler = h
	}
}

// WithIdleCheckInterval sets how often to check for idle capsules.
func WithIdleCheckInterval(d time.Duration) AdjusterOption {
	return func(a *Adjuster) {
		a.idleCheckInterval = d
	}
}

// NewAdjuster creates a new momentum adjuster.
func NewAdjuster(opts ...AdjusterOption) *Adjuster {
	a := &Adjuster{
		trackers:          make(map[capsule.CapsuleID]*Tracker),
		handler:           func(AdjustmentEvent) {},
		logger:            zap.NewNop(),
		idleCheckInterval: 30 * time.Second,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Register adds a capsule's momentum tracker to the adjuster.
func (a *Adjuster) Register(id capsule.CapsuleID, tracker *Tracker) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.trackers[id] = tracker
}

// Unregister removes a capsule from the adjuster.
func (a *Adjuster) Unregister(id capsule.CapsuleID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.trackers, id)
}

// GetTracker returns the tracker for a capsule.
func (a *Adjuster) GetTracker(id capsule.CapsuleID) *Tracker {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.trackers[id]
}

// OnTraffic should be called when a capsule receives traffic.
// This boosts its momentum.
func (a *Adjuster) OnTraffic(id capsule.CapsuleID) {
	a.mu.RLock()
	tracker, ok := a.trackers[id]
	a.mu.RUnlock()
	if !ok {
		return
	}

	old := tracker.Current()
	if tracker.Boost() {
		a.handler(AdjustmentEvent{
			CapsuleID: id,
			OldValue:  old,
			NewValue:  tracker.Current(),
			Reason:    "traffic_boost",
			Timestamp: time.Now(),
		})
	}
}

// OnFailure should be called when a capsule experiences a failure.
// This reduces its momentum.
func (a *Adjuster) OnFailure(id capsule.CapsuleID) {
	a.mu.RLock()
	tracker, ok := a.trackers[id]
	a.mu.RUnlock()
	if !ok {
		return
	}

	old := tracker.Current()
	if tracker.Reduce() {
		a.handler(AdjustmentEvent{
			CapsuleID: id,
			OldValue:  old,
			NewValue:  tracker.Current(),
			Reason:    "failure_reduce",
			Timestamp: time.Now(),
		})
	}
}

// Start begins the idle check loop.
func (a *Adjuster) Start(ctx context.Context) {
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.idleLoop()
	}()
	a.logger.Info("momentum adjuster started",
		zap.Duration("idle_check_interval", a.idleCheckInterval))
}

// Stop halts the adjuster.
func (a *Adjuster) Stop() {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	a.logger.Info("momentum adjuster stopped")
}

func (a *Adjuster) idleLoop() {
	ticker := time.NewTicker(a.idleCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.checkIdle()
		}
	}
}

func (a *Adjuster) checkIdle() {
	now := time.Now()

	a.mu.RLock()
	// Snapshot trackers
	snapshot := make(map[capsule.CapsuleID]*Tracker, len(a.trackers))
	for id, t := range a.trackers {
		snapshot[id] = t
	}
	a.mu.RUnlock()

	for id, tracker := range snapshot {
		if tracker.IsIdle(now) {
			old := tracker.Current()
			if tracker.Reduce() {
				a.handler(AdjustmentEvent{
					CapsuleID: id,
					OldValue:  old,
					NewValue:  tracker.Current(),
					Reason:    "idle_reduce",
					Timestamp: now,
				})
			}
		}
	}
}
