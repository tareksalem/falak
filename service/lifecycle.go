package service

import (
	"context"
	"fmt"
	"sync"

	"github.com/qmuntal/stateless"
	"go.uber.org/zap"
)

// Lifecycle triggers for a Service. The FSM accepts only these names;
// callers must never set Service.Status directly.
const (
	TriggerActivate       = "activate"        // Created → Active
	TriggerStartDraining  = "start_draining"  // Active → Draining
	TriggerFinishDraining = "finish_draining" // Draining → Deleted
	TriggerAbortCanary    = "abort_canary"    // Active → CanaryAborted
	TriggerResume         = "resume"          // CanaryAborted → Active
)

// LifecycleEvent is emitted on every state transition. The FSM is the
// single source of truth for what state a Service is in.
type LifecycleEvent struct {
	ServiceID ServiceID
	From      ServiceStatus
	To        ServiceStatus
	Trigger   string
}

// LifecycleHandler is invoked for every transition by the FSM.
type LifecycleHandler func(event LifecycleEvent)

// Lifecycle manages the state machine for a single Service. Thread-safe.
type Lifecycle struct {
	mu          sync.RWMutex
	serviceID   ServiceID
	machine     *stateless.StateMachine
	handler     LifecycleHandler
	logger      *zap.Logger
	lastTrigger string
	prevState   ServiceStatus
}

// LifecycleOption configures a Lifecycle.
type LifecycleOption func(*Lifecycle)

// WithLifecycleLogger sets the zap logger used for transition logs.
func WithLifecycleLogger(logger *zap.Logger) LifecycleOption {
	return func(l *Lifecycle) {
		if logger != nil {
			l.logger = logger
		}
	}
}

// WithLifecycleHandler installs a handler invoked on every transition.
func WithLifecycleHandler(h LifecycleHandler) LifecycleOption {
	return func(l *Lifecycle) {
		if h != nil {
			l.handler = h
		}
	}
}

// WithLifecycleInitialState overrides the starting state (default:
// Created). Used when restoring a Service from persistent storage.
func WithLifecycleInitialState(state ServiceStatus) LifecycleOption {
	return func(l *Lifecycle) { l.prevState = state }
}

// NewLifecycle constructs a Lifecycle for serviceID with the supplied options.
func NewLifecycle(serviceID ServiceID, opts ...LifecycleOption) *Lifecycle {
	l := &Lifecycle{
		serviceID: serviceID,
		handler:   func(LifecycleEvent) {},
		logger:    zap.NewNop(),
	}
	for _, opt := range opts {
		opt(l)
	}
	initial := ServiceStatusEnum.Created()
	if l.prevState != "" && l.prevState.Valid() {
		initial = l.prevState
	}
	l.machine = l.buildMachine(initial)
	return l
}

// State returns the current lifecycle state.
func (l *Lifecycle) State() ServiceStatus {
	l.mu.RLock()
	defer l.mu.RUnlock()
	state, _ := l.machine.State(context.Background())
	if cs, ok := state.(ServiceStatus); ok {
		return cs
	}
	return ""
}

// Fire applies trigger to the FSM. Returns an error if trigger is not
// permitted from the current state.
func (l *Lifecycle) Fire(trigger string) error {
	l.mu.Lock()
	current, _ := l.machine.State(context.Background())
	if cs, ok := current.(ServiceStatus); ok {
		l.prevState = cs
	}
	l.lastTrigger = trigger
	l.mu.Unlock()
	if err := l.machine.Fire(trigger); err != nil {
		return fmt.Errorf("service lifecycle transition failed: trigger=%s, err=%w", trigger, err)
	}
	return nil
}

// CanFire reports whether trigger is permitted from the current state.
func (l *Lifecycle) CanFire(trigger string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	can, _ := l.machine.CanFire(trigger)
	return can
}

func (l *Lifecycle) buildMachine(initial ServiceStatus) *stateless.StateMachine {
	created := ServiceStatusEnum.Created()
	active := ServiceStatusEnum.Active()
	draining := ServiceStatusEnum.Draining()
	deleted := ServiceStatusEnum.Deleted()
	canaryAborted := ServiceStatusEnum.CanaryAborted()
	placementFailed := ServiceStatusEnum.PlacementFailed()

	sm := stateless.NewStateMachine(initial)
	sm.Configure(created).Permit(TriggerActivate, active).OnEntry(l.onEntry(created))
	sm.Configure(active).
		Permit(TriggerStartDraining, draining).
		Permit(TriggerAbortCanary, canaryAborted).
		OnEntry(l.onEntry(active))
	sm.Configure(draining).Permit(TriggerFinishDraining, deleted).OnEntry(l.onEntry(draining))
	sm.Configure(deleted).OnEntry(l.onEntry(deleted)) // terminal
	sm.Configure(canaryAborted).Permit(TriggerResume, active).OnEntry(l.onEntry(canaryAborted))
	// PlacementFailed is reserved-but-not-entered in v1; declared for
	// forward-compat with WithLifecycleInitialState(PlacementFailed).
	sm.Configure(placementFailed).OnEntry(l.onEntry(placementFailed))
	return sm
}

func (l *Lifecycle) onEntry(state ServiceStatus) stateless.ActionFunc {
	return func(_ context.Context, _ ...any) error {
		l.mu.RLock()
		from := l.prevState
		trigger := l.lastTrigger
		l.mu.RUnlock()

		l.logger.Debug("service state transition",
			zap.String("service", string(l.serviceID)),
			zap.String("from", string(from)),
			zap.String("to", string(state)),
			zap.String("trigger", trigger))

		l.handler(LifecycleEvent{
			ServiceID: l.serviceID,
			From:      from,
			To:        state,
			Trigger:   trigger,
		})
		return nil
	}
}
