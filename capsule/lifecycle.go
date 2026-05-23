package capsule

import (
	"context"
	"fmt"
	"sync"

	"github.com/qmuntal/stateless"
	"github.com/tareksalem/falak/capsule/enums"
	"go.uber.org/zap"
)

// Transition triggers for the capsule lifecycle state machine.
// These drive all legal state changes. Calling Fire with an invalid trigger
// for the current state returns an error — never set capsule.Status directly.
const (
	TriggerAnnounce         = "announce"
	TriggerElectionStarted  = "election_started"
	TriggerElectionWon      = "election_won"
	TriggerElectionTimeout  = "election_timeout"
	TriggerExecutionStart   = "execution_started"
	TriggerExecutionFailed  = "execution_failed"
	TriggerContainerReady   = "container_ready"
	TriggerStopRequested    = "stop_requested"
	TriggerContainerStopped = "container_stopped"
	TriggerNodeFailed       = "node_failed"
	TriggerScaleUpNeeded    = "scale_up_needed"
	TriggerScaleDownNeeded  = "scale_down_needed"

	// TriggerMembersAdmitted advances a group-kind capsule from Announced to
	// Running once every member has been admitted (materialized + persisted)
	// by the originator. The FSM accepts this trigger from any kind; the
	// Manager is responsible for kind-gating so non-group capsules cannot
	// fire it. See Manager.Fire and Manager.MarkGroupRunning.
	TriggerMembersAdmitted = "members_admitted"
)

// LifecycleEvent is emitted on state transitions.
type LifecycleEvent struct {
	CapsuleID CapsuleID
	From      enums.CapsuleStatus
	To        enums.CapsuleStatus
	Trigger   string
}

// LifecycleHandler is called on state transitions.
type LifecycleHandler func(event LifecycleEvent)

// Lifecycle manages the state machine for a single capsule's lifecycle.
// All state transitions must go through Fire() — direct status assignment is
// not supported. The machine is thread-safe and captures the previous state
// and trigger name on every transition for observability.
type Lifecycle struct {
	mu          sync.RWMutex
	capsuleID   CapsuleID
	machine     *stateless.StateMachine
	handler     LifecycleHandler
	logger      *zap.Logger
	lastTrigger string
	prevState   enums.CapsuleStatus
}

// LifecycleOption configures a Lifecycle.
type LifecycleOption func(*Lifecycle)

// WithLifecycleLogger sets the logger.
func WithLifecycleLogger(logger *zap.Logger) LifecycleOption {
	return func(l *Lifecycle) {
		l.logger = logger
	}
}

// WithLifecycleHandler sets the transition event handler.
func WithLifecycleHandler(h LifecycleHandler) LifecycleOption {
	return func(l *Lifecycle) {
		l.handler = h
	}
}

// WithLifecycleInitialState overrides the starting state (default: Created).
// Used when restoring a capsule from persistent storage.
func WithLifecycleInitialState(state enums.CapsuleStatus) LifecycleOption {
	return func(l *Lifecycle) {
		l.prevState = state
	}
}

// NewLifecycle creates a new lifecycle state machine for a capsule.
func NewLifecycle(capsuleID CapsuleID, opts ...LifecycleOption) *Lifecycle {
	l := &Lifecycle{
		capsuleID: capsuleID,
		handler:   func(LifecycleEvent) {},
		logger:    zap.NewNop(),
	}
	for _, opt := range opts {
		opt(l)
	}

	initial := enums.CapsuleStatusEnum.Created()
	if l.prevState != "" && l.prevState.Valid() {
		initial = l.prevState
	}
	l.machine = l.buildMachine(initial)
	return l
}

// State returns the current lifecycle state.
func (l *Lifecycle) State() enums.CapsuleStatus {
	l.mu.RLock()
	defer l.mu.RUnlock()
	state, _ := l.machine.State(context.Background())
	if cs, ok := state.(enums.CapsuleStatus); ok {
		return cs
	}
	return ""
}

// Fire triggers a state transition. Returns an error if the trigger is not
// permitted in the current state.
func (l *Lifecycle) Fire(trigger string) error {
	l.mu.Lock()
	// Capture state before the transition so onEntry can report From.
	current, _ := l.machine.State(context.Background())
	if cs, ok := current.(enums.CapsuleStatus); ok {
		l.prevState = cs
	}
	l.lastTrigger = trigger
	l.mu.Unlock()

	if err := l.machine.Fire(trigger); err != nil {
		return fmt.Errorf("lifecycle transition failed: trigger=%s, err=%w", trigger, err)
	}
	return nil
}

// CanFire checks if a trigger can be fired in the current state.
func (l *Lifecycle) CanFire(trigger string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	can, _ := l.machine.CanFire(trigger)
	return can
}

func (l *Lifecycle) buildMachine(initial enums.CapsuleStatus) *stateless.StateMachine {
	created := enums.CapsuleStatusEnum.Created()
	announced := enums.CapsuleStatusEnum.Announced()
	electing := enums.CapsuleStatusEnum.Electing()
	assigned := enums.CapsuleStatusEnum.Assigned()
	executing := enums.CapsuleStatusEnum.Executing()
	running := enums.CapsuleStatusEnum.Running()
	stopping := enums.CapsuleStatusEnum.Stopping()
	stopped := enums.CapsuleStatusEnum.Stopped()

	sm := stateless.NewStateMachine(initial)

	// Created → Announced
	sm.Configure(created).
		Permit(TriggerAnnounce, announced).
		OnEntry(l.onEntry(created))

	// Announced → Electing (capsules) or Running (groups, on members admitted).
	// The FSM permits both transitions from Announced; Manager.Fire enforces
	// per-kind gating so groups never fire election triggers and capsules
	// never fire MembersAdmitted.
	sm.Configure(announced).
		Permit(TriggerElectionStarted, electing).
		Permit(TriggerMembersAdmitted, running).
		OnEntry(l.onEntry(announced))

	// Electing → Assigned or back to Announced
	sm.Configure(electing).
		Permit(TriggerElectionWon, assigned).
		Permit(TriggerElectionTimeout, announced).
		OnEntry(l.onEntry(electing))

	// Assigned → Executing or back to Announced
	sm.Configure(assigned).
		Permit(TriggerExecutionStart, executing).
		Permit(TriggerExecutionFailed, announced).
		OnEntry(l.onEntry(assigned))

	// Executing → Running or back to Announced
	sm.Configure(executing).
		Permit(TriggerContainerReady, running).
		Permit(TriggerExecutionFailed, announced).
		OnEntry(l.onEntry(executing))

	// Running → Stopping or back to Announced (re-election or scale up)
	sm.Configure(running).
		Permit(TriggerStopRequested, stopping).
		Permit(TriggerNodeFailed, announced).
		Permit(TriggerScaleUpNeeded, announced).
		Permit(TriggerScaleDownNeeded, stopping).
		OnEntry(l.onEntry(running))

	// Stopping → Stopped
	sm.Configure(stopping).
		Permit(TriggerContainerStopped, stopped).
		OnEntry(l.onEntry(stopping))

	// Stopped → Announced (restart)
	sm.Configure(stopped).
		Permit(TriggerAnnounce, announced).
		OnEntry(l.onEntry(stopped))

	return sm
}

func (l *Lifecycle) onEntry(state enums.CapsuleStatus) stateless.ActionFunc {
	return func(_ context.Context, _ ...any) error {
		l.mu.RLock()
		from := l.prevState
		trigger := l.lastTrigger
		l.mu.RUnlock()

		l.logger.Debug("capsule state transition",
			zap.String("capsule_id", string(l.capsuleID)),
			zap.String("from", string(from)),
			zap.String("to", string(state)),
			zap.String("trigger", trigger))

		l.handler(LifecycleEvent{
			CapsuleID: l.capsuleID,
			From:      from,
			To:        state,
			Trigger:   trigger,
		})

		return nil
	}
}
