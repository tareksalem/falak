package strategy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tareksalem/falak/service"
)

// blueGreenFullWeight is the post-flip weight on the active backend.
const blueGreenFullWeight = int32(100)

// BlueGreen is the atomic-flip strategy with a configurable drain
// window (default 30s). The proxy never force-closes;
// the engine moves all new traffic to the new active immediately on
// flip and uses the drain timer as a bookkeeping signal — when it
// fires, the engine transitions back to Active phase.
type BlueGreen struct {
	serviceID                             service.ServiceID
	emitter                               EventEmitter
	now                                   func() time.Time
	afterFunc                             func(time.Duration, func()) timer
	mu                                    sync.Mutex
	active, pendingFrom, pendingTo, phase string
	pendingDrain                          timer
	weights                               LiveWeights
	lastChanged                           time.Time
	drain                                 time.Duration
	closed                                bool
}

// timer abstracts time.AfterFunc so tests can inject a scheduler.
type timer interface{ Stop() bool }

type realTimer struct{ t *time.Timer }

func (r realTimer) Stop() bool { return r.t.Stop() }

// BlueGreenOption configures a BlueGreen engine.
type BlueGreenOption func(*BlueGreen)

// WithBlueGreenClock injects a clock and a timer factory.
func WithBlueGreenClock(now func() time.Time, afterFn func(time.Duration, func()) timer) BlueGreenOption {
	return func(b *BlueGreen) {
		if now != nil {
			b.now = now
		}
		if afterFn != nil {
			b.afterFunc = afterFn
		}
	}
}

// NewBlueGreen constructs a BlueGreen engine in the Active phase. A
// nil emitter is replaced with a no-op.
func NewBlueGreen(id service.ServiceID, spec service.ServiceSpec, emitter EventEmitter, opts ...BlueGreenOption) *BlueGreen {
	if emitter == nil {
		emitter = noopEmitter{}
	}
	b := &BlueGreen{
		serviceID: id, emitter: emitter, now: time.Now,
		phase: "active", drain: service.DefaultBlueGreenDrain,
		afterFunc: func(d time.Duration, fn func()) timer { return realTimer{t: time.AfterFunc(d, fn)} },
	}
	for _, opt := range opts {
		opt(b)
	}
	b.lastChanged = b.now()
	b.active, b.drain = readBlueGreen(spec)
	b.weights = buildBlueGreenWeights(spec, b.active)
	return b
}

// Start validates lifecycle state. The engine has no progression
// goroutines so this is essentially a guard. Idempotent.
func (b *BlueGreen) Start(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("strategy: blue-green engine already stopped")
	}
	return nil
}

// Stop cancels the pending drain timer. Idempotent.
func (b *BlueGreen) Stop() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	if b.pendingDrain != nil {
		b.pendingDrain.Stop()
		b.pendingDrain = nil
	}
	return nil
}

// LiveWeights returns a fresh copy of the current weight map.
func (b *BlueGreen) LiveWeights() LiveWeights {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.weights.Clone()
}

// Update reacts to a spec edit. On active-backend change the engine
// flips: cancel any pending drain, move all new traffic, schedule
// the new drain timer, emit BlueGreenFlip.
func (b *BlueGreen) Update(spec service.ServiceSpec) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return fmt.Errorf("strategy: blue-green engine stopped")
	}
	newActive, newDrain := readBlueGreen(spec)
	if newActive == "" {
		b.mu.Unlock()
		return fmt.Errorf("strategy: blue-green spec missing active backend")
	}
	if newActive == b.active && b.pendingDrain == nil {
		b.weights, b.drain = buildBlueGreenWeights(spec, b.active), newDrain
		b.mu.Unlock()
		return nil
	}
	from := b.active
	if b.pendingDrain != nil {
		b.pendingDrain.Stop()
	}
	b.active, b.drain = newActive, newDrain
	b.weights = buildBlueGreenWeights(spec, newActive)
	b.phase, b.lastChanged = "draining", b.now()
	b.pendingFrom, b.pendingTo = from, newActive
	b.pendingDrain = b.afterFunc(b.drain, func() { b.onDrainFired(from, newActive) })
	id := b.serviceID
	b.mu.Unlock()
	b.emitter.EmitBlueGreenFlip(id, from, newActive)
	return nil
}

// State returns a diagnostic snapshot.
func (b *BlueGreen) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	detail := map[string]any{"active": b.active, "drain_ms": b.drain.Milliseconds()}
	if b.pendingDrain != nil {
		detail["draining_from"], detail["draining_to"] = b.pendingFrom, b.pendingTo
	}
	return State{
		Type:           string(service.StrategyTypeEnum.BlueGreen()),
		Phase:          b.phase,
		CurrentWeights: b.weights.Clone(),
		LastChangedAt:  b.lastChanged,
		Detail:         detail,
	}
}

// onDrainFired transitions back to Active phase unless a concurrent
// Update has already replaced this timer.
func (b *BlueGreen) onDrainFired(from, to string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.pendingFrom != from || b.pendingTo != to {
		return
	}
	b.pendingDrain, b.pendingFrom, b.pendingTo = nil, "", ""
	b.phase, b.lastChanged = "active", b.now()
}

// readBlueGreen returns (active, drain). Falls back to default drain.
func readBlueGreen(spec service.ServiceSpec) (string, time.Duration) {
	if spec.Strategy == nil || spec.Strategy.BlueGreen == nil {
		return "", service.DefaultBlueGreenDrain
	}
	drain := spec.Strategy.BlueGreen.Drain
	if drain <= 0 {
		drain = service.DefaultBlueGreenDrain
	}
	return spec.Strategy.BlueGreen.Active, drain
}

// buildBlueGreenWeights gives the active backend 100 and every other
// spec'd backend 0; SWRR sees a stable backend set across flips.
func buildBlueGreenWeights(spec service.ServiceSpec, active string) LiveWeights {
	out := make(LiveWeights, len(spec.Backends)+1)
	for _, b := range spec.Backends {
		out[b.Capsule] = 0
		if b.Capsule == active {
			out[b.Capsule] = blueGreenFullWeight
		}
	}
	if _, ok := out[active]; !ok && active != "" {
		out[active] = blueGreenFullWeight
	}
	return out
}
