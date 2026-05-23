package strategy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tareksalem/falak/service"
)

const (
	canaryFullWeight  = int32(100)
	canaryMinInterval = time.Millisecond
	defaultAbortFloor = 10 * time.Second
)

// canaryMode is selected implicitly from the spec fields.
type canaryMode int

const (
	canaryModeManual canaryMode = iota
	canaryModeAuto
	canaryModeGated
)

func (m canaryMode) String() string {
	switch m {
	case canaryModeManual:
		return "manual"
	case canaryModeAuto:
		return "auto"
	case canaryModeGated:
		return "gated"
	default:
		return "unknown"
	}
}

// Canary implements the three-mode canary strategy with a
// continuously-evaluated abort guard. Weights move from `from` to
// `target` in `step`-sized increments.
//
// Mode (Decision #18): interval == 0 → manual (Advance only);
// interval > 0, no criteria → auto; interval > 0, criteria set → gated.
//
// Abort (Decision #19): any abort_on match → full revert + emit
// CanaryAborted. The Manager owns the FSM transition to the
// CanaryAborted Service status.
type Canary struct {
	serviceID service.ServiceID
	emitter   EventEmitter
	eval      MetricEvaluator
	now       func() time.Time
	newTicker func(d time.Duration) ticker

	mu          sync.Mutex
	mode        canaryMode
	from        string
	target      string
	step        int32
	interval    time.Duration
	criteria    []string
	abortOn     []string
	weights     LiveWeights
	startW      LiveWeights
	phase       string
	lastChanged time.Time
	aborted     bool
	completed   bool

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
}

// ticker abstracts time.Ticker for deterministic tests.
type ticker interface {
	C() <-chan time.Time
	Stop()
}

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

// CanaryOption configures a Canary engine.
type CanaryOption func(*Canary)

// WithCanaryClock injects a clock and ticker factory for tests.
func WithCanaryClock(now func() time.Time, newTicker func(time.Duration) ticker) CanaryOption {
	return func(c *Canary) {
		if now != nil {
			c.now = now
		}
		if newTicker != nil {
			c.newTicker = newTicker
		}
	}
}

// NewCanary constructs a Canary engine. Nil emitter / eval are
// replaced with no-ops for unit tests; in production both are
// supplied by the Manager.
func NewCanary(id service.ServiceID, spec service.ServiceSpec, emitter EventEmitter, eval MetricEvaluator, opts ...CanaryOption) *Canary {
	if emitter == nil {
		emitter = noopEmitter{}
	}
	if eval == nil {
		eval = staticEval(false)
	}
	c := &Canary{
		serviceID: id, emitter: emitter, eval: eval,
		now:       time.Now,
		newTicker: func(d time.Duration) ticker { return realTicker{t: time.NewTicker(d)} },
		phase:     "pending",
	}
	for _, opt := range opts {
		opt(c)
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.applySpec(spec, true)
	return c
}

// Start launches progression and abort-watch goroutines if applicable.
func (c *Canary) Start(_ context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("strategy: canary engine already stopped")
	}
	if c.phase == "running" || c.aborted || c.completed {
		c.mu.Unlock()
		return nil
	}
	c.phase = "running"
	c.lastChanged = c.now()
	mode := c.mode
	interval := c.interval
	cadence := c.abortCadenceLocked()
	hasAbort := len(c.abortOn) > 0
	c.mu.Unlock()

	if mode == canaryModeAuto || mode == canaryModeGated {
		c.wg.Add(1)
		go c.runProgression(interval)
	}
	if hasAbort {
		c.wg.Add(1)
		go c.runAbortWatch(cadence)
	}
	return nil
}

// Stop cancels timers and waits for goroutine exit.
func (c *Canary) Stop() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.cancel()
	c.mu.Unlock()
	c.wg.Wait()
	return nil
}

// LiveWeights returns a fresh copy of the current weight map.
func (c *Canary) LiveWeights() LiveWeights {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.weights.Clone()
}

// Update re-applies a spec. If `target` or `from` is now flagged
// UnresolvedIdentityChanged (weight ≤ 0 in spec) the engine
// auto-aborts.
func (c *Canary) Update(spec service.ServiceSpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("strategy: canary engine stopped")
	}
	if c.aborted || c.completed {
		return nil
	}
	if reason := identityChangeReason(spec, c.from, c.target); reason != "" {
		c.abortLocked(reason)
		return nil
	}
	c.applySpec(spec, false)
	return nil
}

// Advance forces a single step. Idempotent at the target.
func (c *Canary) Advance() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.aborted || c.completed {
		return
	}
	c.stepLocked()
}

// State returns a diagnostic snapshot.
func (c *Canary) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return State{
		Type:           string(service.StrategyTypeEnum.Canary()),
		Phase:          c.phase,
		CurrentWeights: c.weights.Clone(),
		LastChangedAt:  c.lastChanged,
		Detail: map[string]any{
			"mode": c.mode.String(), "from": c.from,
			"target": c.target, "step_pct": c.step,
		},
	}
}

// applySpec captures spec fields. When initial, also seeds weights.
func (c *Canary) applySpec(spec service.ServiceSpec, initial bool) {
	if spec.Strategy == nil || spec.Strategy.Canary == nil {
		return
	}
	can := spec.Strategy.Canary
	c.from = can.From
	c.target = can.Target
	c.step = can.Step
	if c.step <= 0 {
		c.step = 10
	}
	c.interval = can.Interval
	c.criteria = append(c.criteria[:0], can.SuccessCriteria...)
	c.abortOn = append(c.abortOn[:0], can.AbortOn...)
	c.mode = pickCanaryMode(c.interval, c.criteria)
	if initial {
		c.weights = LiveWeights{c.from: canaryFullWeight, c.target: 0}
		c.startW = c.weights.Clone()
		c.lastChanged = c.now()
	}
}

// stepLocked advances one progression tick. Caller holds c.mu. The
// emitter is invoked outside the lock to avoid re-entrancy hazards.
func (c *Canary) stepLocked() {
	if c.completed || c.aborted {
		return
	}
	current := c.weights[c.target]
	if current >= canaryFullWeight {
		c.completed = true
		c.phase = "completed"
		c.lastChanged = c.now()
		return
	}
	next := current + c.step
	if next > canaryFullWeight {
		next = canaryFullWeight
	}
	c.weights[c.target] = next
	c.weights[c.from] = canaryFullWeight - next
	c.lastChanged = c.now()
	if next >= canaryFullWeight {
		c.completed = true
		c.phase = "completed"
	} else {
		c.phase = "advancing"
	}
	weights := c.weights.Clone()
	target := c.target
	id := c.serviceID
	c.mu.Unlock()
	c.emitter.EmitCanaryStep(id, target, weights)
	c.mu.Lock()
}

// abortLocked performs full revert + emit. Caller holds c.mu.
func (c *Canary) abortLocked(reason string) {
	c.aborted = true
	c.phase = "aborted"
	c.weights = c.startW.Clone()
	c.lastChanged = c.now()
	id := c.serviceID
	c.mu.Unlock()
	c.emitter.EmitCanaryAborted(id, reason)
	c.mu.Lock()
}

// abortCadenceLocked returns the cadence at which abort_on is
// evaluated: interval/3 floored at defaultAbortFloor when interval
// is long enough, else interval/3 (or a tiny minimum when manual).
func (c *Canary) abortCadenceLocked() time.Duration {
	if c.interval <= 0 {
		return defaultAbortFloor
	}
	cad := c.interval / 3
	if cad < canaryMinInterval {
		cad = canaryMinInterval
	}
	if c.interval >= defaultAbortFloor && cad < defaultAbortFloor {
		return defaultAbortFloor
	}
	return cad
}

// runProgression is the auto/gated tick loop.
func (c *Canary) runProgression(interval time.Duration) {
	defer c.wg.Done()
	if interval <= 0 {
		return
	}
	tk := c.newTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-tk.C():
			if c.tickProgress() {
				return
			}
		}
	}
}

// tickProgress runs one progression tick; returns true when the loop
// should exit (completed / aborted / closed).
func (c *Canary) tickProgress() bool {
	c.mu.Lock()
	if c.closed || c.aborted || c.completed {
		c.mu.Unlock()
		return true
	}
	mode := c.mode
	criteria := append([]string(nil), c.criteria...)
	c.mu.Unlock()

	if mode == canaryModeGated {
		ok, err := allTrue(c.eval, criteria)
		if err != nil || !ok {
			c.mu.Lock()
			if !c.closed && !c.aborted && !c.completed {
				c.phase = "frozen"
				c.lastChanged = c.now()
			}
			c.mu.Unlock()
			return false
		}
	}
	c.mu.Lock()
	if !c.closed && !c.aborted && !c.completed {
		c.stepLocked()
	}
	done := c.completed || c.aborted || c.closed
	c.mu.Unlock()
	return done
}

// runAbortWatch continuously evaluates abort_on.
func (c *Canary) runAbortWatch(cadence time.Duration) {
	defer c.wg.Done()
	if cadence <= 0 {
		return
	}
	tk := c.newTicker(cadence)
	defer tk.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-tk.C():
			if c.tickAbort() {
				return
			}
		}
	}
}

// tickAbort runs one abort-watch tick; returns true when the loop
// should exit.
func (c *Canary) tickAbort() bool {
	c.mu.Lock()
	if c.closed || c.aborted || c.completed {
		c.mu.Unlock()
		return true
	}
	conds := append([]string(nil), c.abortOn...)
	c.mu.Unlock()
	reason := firstTrue(c.eval, conds)
	if reason == "" {
		return false
	}
	c.mu.Lock()
	if !c.closed && !c.aborted && !c.completed {
		c.abortLocked(reason)
	}
	done := c.aborted
	c.mu.Unlock()
	return done
}

// pickCanaryMode applies Decision #18.
func pickCanaryMode(interval time.Duration, criteria []string) canaryMode {
	switch {
	case interval <= 0:
		return canaryModeManual
	case len(criteria) > 0:
		return canaryModeGated
	default:
		return canaryModeAuto
	}
}

// identityChangeReason returns a non-empty reason if `from` or
// `target` is flagged via a non-positive weight in the spec.
func identityChangeReason(spec service.ServiceSpec, from, target string) string {
	for _, b := range spec.Backends {
		if (b.Capsule == from || b.Capsule == target) && b.Weight <= 0 {
			return fmt.Sprintf("backend %q identity-changed or unresolved", b.Capsule)
		}
	}
	return ""
}

// allTrue returns true iff every condition evaluates true with no error.
func allTrue(eval MetricEvaluator, conds []string) (bool, error) {
	for _, c := range conds {
		ok, err := eval.Evaluate(c)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// firstTrue returns the first condition that evaluates true, or "".
func firstTrue(eval MetricEvaluator, conds []string) string {
	for _, c := range conds {
		ok, err := eval.Evaluate(c)
		if err == nil && ok {
			return c
		}
	}
	return ""
}

// staticEval is a constant MetricEvaluator used when none is wired.
type staticEval bool

func (s staticEval) Evaluate(string) (bool, error) { return bool(s), nil }
