// Package reliability tracks a node's execution reliability — its historical
// ability to actually START and RUN capsules, as distinct from connection
// health. It listens on the node event bus for capsule lifecycle outcomes
// (CapsuleRunning = success, CapsuleExecutionFailed = failure) and maintains
// time-decayed, Bayesian-smoothed success/failure counts. The election
// gravity calculator reads the resulting score so that nodes which keep
// failing to place workloads gradually lose election priority while idle
// nodes recover toward an optimistic prior as their failures decay away.
//
// Design notes:
//   - Decay is TIME-based (half-life), not event-based, so an idle node that
//     had a bad streak recovers without needing new placements.
//   - Failures are only counted when they are plausibly the node's fault.
//     Failures categorised CapsuleGlobal (bad image ref, invalid spec) are
//     excluded — they would fail on every node, so blaming this node is wrong.
//   - The clock is injectable for deterministic tests.
//   - Counters persist node-global in SQLite; on restart they reload and the
//     lazy-decay on the next read applies decay for the downtime gap.
package reliability

import (
	"context"
	"math"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/node/internal/events"
)

const (
	// DefaultHalfLife is how long it takes decayed counters to halve. 30m
	// lets a node that had a bad streak recover within a few half-lives
	// even while idle.
	DefaultHalfLife = 30 * time.Minute

	// DefaultPriorA / DefaultPriorB are the Beta-prior pseudo-counts. With
	// a=4, b=1 a zero-history node scores 4/5 = 0.8 — optimistic, just under
	// a proven node, so freshly joined nodes are not starved.
	DefaultPriorA = 4.0
	DefaultPriorB = 1.0
)

// Clock supplies the current time. Production uses the wall clock; tests
// inject a controllable fake for deterministic decay assertions.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Tracker maintains a single node's decayed execution-reliability counters
// and exposes the current score to the gravity state provider.
type Tracker struct {
	mu         sync.Mutex
	successes  float64
	failures   float64
	lastUpdate time.Time

	halfLife time.Duration
	priorA   float64
	priorB   float64
	clock    Clock
	nodeID   string

	store  *Store
	bus    events.Bus
	logger *zap.Logger

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool

	// onProcessed, when set, is invoked after each bus event is applied.
	// Test-only hook so bus-delivery tests can synchronise deterministically
	// without sleeping. nil in production.
	onProcessed func()
}

// TrackerOption configures a Tracker.
type TrackerOption func(*Tracker)

// WithHalfLife overrides the decay half-life.
func WithHalfLife(d time.Duration) TrackerOption {
	return func(t *Tracker) {
		if d > 0 {
			t.halfLife = d
		}
	}
}

// WithPrior overrides the Beta-prior pseudo-counts (a successes, b failures).
// The zero-history score is a/(a+b).
func WithPrior(a, b float64) TrackerOption {
	return func(t *Tracker) {
		if a > 0 {
			t.priorA = a
		}
		if b > 0 {
			t.priorB = b
		}
	}
}

// WithClock injects a clock (defaults to the wall clock).
func WithClock(c Clock) TrackerOption {
	return func(t *Tracker) {
		if c != nil {
			t.clock = c
		}
	}
}

// WithLogger sets the logger.
func WithLogger(l *zap.Logger) TrackerOption {
	return func(t *Tracker) {
		if l != nil {
			t.logger = l
		}
	}
}

// withOnProcessed is a test-only hook (unexported; only the in-package test
// can set it) invoked after each bus event is applied.
func withOnProcessed(fn func()) TrackerOption {
	return func(t *Tracker) {
		t.onProcessed = fn
	}
}

// NewTracker constructs a Tracker for nodeID. The store provides persistence
// (may be nil to run purely in-memory). Persisted counters, if any, are
// loaded immediately so Current is correct before Start.
func NewTracker(nodeID string, store *Store, bus events.Bus, opts ...TrackerOption) *Tracker {
	t := &Tracker{
		halfLife: DefaultHalfLife,
		priorA:   DefaultPriorA,
		priorB:   DefaultPriorB,
		clock:    realClock{},
		nodeID:   nodeID,
		store:    store,
		bus:      bus,
		logger:   zap.NewNop(),
	}
	for _, opt := range opts {
		opt(t)
	}
	t.loadPersisted()
	return t
}

// loadPersisted reloads counters from the store. A missing row or a load
// error both start from a clean slate (the latter is logged) — execution
// reliability is a soft signal and must never block node startup.
func (t *Tracker) loadPersisted() {
	if t.store == nil {
		return
	}
	c, found, err := t.store.Load(t.nodeID)
	if err != nil {
		t.logger.Warn("execution reliability: load persisted counters failed; starting fresh",
			zap.String("node", t.nodeID), zap.Error(err))
		return
	}
	if !found {
		return
	}
	t.mu.Lock()
	t.successes = c.successes
	t.failures = c.failures
	t.lastUpdate = c.lastUpdate
	t.mu.Unlock()
	t.logger.Debug("execution reliability: reloaded persisted counters",
		zap.String("node", t.nodeID),
		zap.Float64("successes", c.successes),
		zap.Float64("failures", c.failures))
}

// Start subscribes to the event bus and launches the consume loop. Idempotent.
func (t *Tracker) Start(parent context.Context) {
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return
	}
	t.started = true
	t.ctx, t.cancel = context.WithCancel(parent)
	t.mu.Unlock()

	// Subscribe synchronously BEFORE launching the consume goroutine so the
	// subscription is live by the time Start returns. Subscribing inside the
	// goroutine races with the caller's first Publish, silently dropping
	// early lifecycle events (and flaking the bus-subscription test).
	successCh := t.bus.Subscribe(events.TypeCapsuleRunning)
	failCh := t.bus.Subscribe(events.TypeCapsuleFailed)

	t.wg.Add(1)
	go t.loop(successCh, failCh)
	t.logger.Info("execution reliability tracker started",
		zap.String("node", t.nodeID),
		zap.Duration("half_life", t.halfLife))
}

// Stop cancels the consume loop, waits for it to exit, and persists the
// final decayed counters. Safe to call multiple times.
func (t *Tracker) Stop() {
	t.mu.Lock()
	if !t.started {
		t.mu.Unlock()
		return
	}
	t.started = false
	cancel := t.cancel
	t.cancel = nil
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	t.wg.Wait()
	t.persist()
	if t.store != nil {
		if err := t.store.Close(); err != nil {
			t.logger.Warn("execution reliability: store close failed",
				zap.String("node", t.nodeID), zap.Error(err))
		}
	}
	t.logger.Info("execution reliability tracker stopped", zap.String("node", t.nodeID))
}

// loop consumes lifecycle events until the context is cancelled. The
// subscription channels are created synchronously in Start (before the
// goroutine launches) so no early event can be missed.
func (t *Tracker) loop(successCh, failCh <-chan events.Event) {
	defer t.wg.Done()

	defer t.bus.Unsubscribe(events.TypeCapsuleRunning, successCh)
	defer t.bus.Unsubscribe(events.TypeCapsuleFailed, failCh)

	for {
		select {
		case <-t.ctx.Done():
			return
		case ev, ok := <-successCh:
			if !ok {
				return
			}
			if _, ok := ev.(events.CapsuleRunning); ok {
				t.recordSuccess()
			}
			t.signalProcessed()
		case ev, ok := <-failCh:
			if !ok {
				return
			}
			if failed, ok := ev.(events.CapsuleExecutionFailed); ok {
				t.recordFailureEvent(failed.Category)
			}
			t.signalProcessed()
		}
	}
}

func (t *Tracker) signalProcessed() {
	if t.onProcessed != nil {
		t.onProcessed()
	}
}

// recordSuccess decays to now and adds one success.
func (t *Tracker) recordSuccess() {
	t.mu.Lock()
	t.decayLocked(t.clock.Now())
	t.successes++
	c := counters{t.successes, t.failures, t.lastUpdate}
	t.mu.Unlock()

	t.savePersist(c)
	t.logger.Debug("execution reliability: success recorded",
		zap.String("node", t.nodeID),
		zap.Float64("successes", c.successes),
		zap.Float64("failures", c.failures))
}

// recordFailureEvent applies a categorised failure. CapsuleGlobal failures
// are ignored (they are not the node's fault); NodeAttributable and Ambiguous
// (and the empty/zero default) are counted.
func (t *Tracker) recordFailureEvent(category events.FailureCategory) {
	if category == events.FailureCategoryEnum.CapsuleGlobal() {
		t.logger.Debug("execution reliability: failure excluded (capsule-global)",
			zap.String("node", t.nodeID))
		return
	}
	t.mu.Lock()
	t.decayLocked(t.clock.Now())
	t.failures++
	c := counters{t.successes, t.failures, t.lastUpdate}
	t.mu.Unlock()

	t.savePersist(c)
	t.logger.Debug("execution reliability: failure recorded",
		zap.String("node", t.nodeID),
		zap.String("category", string(category)),
		zap.Float64("successes", c.successes),
		zap.Float64("failures", c.failures))
}

// decayLocked multiplies both counters by 0.5^(elapsed/halfLife) and advances
// lastUpdate. The first observation (zero lastUpdate) seeds the timestamp
// without decaying. Decay is composable, so reloading after downtime and
// decaying the whole gap yields the same result as continuous decay.
func (t *Tracker) decayLocked(now time.Time) {
	if t.lastUpdate.IsZero() {
		t.lastUpdate = now
		return
	}
	dt := now.Sub(t.lastUpdate)
	if dt <= 0 {
		return
	}
	decay := math.Pow(0.5, dt.Seconds()/t.halfLife.Seconds())
	t.successes *= decay
	t.failures *= decay
	t.lastUpdate = now
}

// Current returns the current execution-reliability score in (0, 1). It
// lazy-decays to the clock's current time first, so an idle node's score
// climbs back toward the prior a/(a+b) as its accumulated failures decay.
func (t *Tracker) Current() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.decayLocked(t.clock.Now())
	return (t.successes + t.priorA) / (t.successes + t.failures + t.priorA + t.priorB)
}

// savePersist writes counters to the store if persistence is enabled,
// logging (not propagating) any error — reliability data is best-effort.
func (t *Tracker) savePersist(c counters) {
	if t.store == nil {
		return
	}
	if err := t.store.Save(t.nodeID, c); err != nil {
		t.logger.Warn("execution reliability: persist failed",
			zap.String("node", t.nodeID), zap.Error(err))
	}
}

// persist snapshots and saves the current (lazy-decayed) counters.
func (t *Tracker) persist() {
	t.mu.Lock()
	t.decayLocked(t.clock.Now())
	c := counters{t.successes, t.failures, t.lastUpdate}
	t.mu.Unlock()
	t.savePersist(c)
}
