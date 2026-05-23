// Package strategy implements the traffic-management strategies that
// translate a ServiceSpec into the live weight map consumed by the L4
// proxy backend selector. One Engine instance lives per Service.
//
// Engines are decoupled from the service.Manager: the Manager wires
// an Engine on Service create/update; the proxy reads LiveWeights()
// on every backend pick. Engines emit canary / blue-green progress
// events through the EventEmitter, which the Manager forwards onto
// the standard ManagerEvent bus.
package strategy

import (
	"context"
	"time"

	"github.com/tareksalem/falak/service"
)

// LiveWeights maps a backend's logical name (ServiceBackend.Capsule)
// to its current effective routing weight in the 0–10000 range.
type LiveWeights map[string]int32

// Clone returns a deep copy so callers can safely retain or mutate.
func (lw LiveWeights) Clone() LiveWeights {
	out := make(LiveWeights, len(lw))
	for k, v := range lw {
		out[k] = v
	}
	return out
}

// State is the diagnostic snapshot of an Engine. The strategy package
// does not mutate Service.Status itself — the Manager subscribes to
// engine events and applies the FSM transition.
type State struct {
	Type           string         // strategy variant ("static", "blue-green", "canary").
	Phase          string         // engine-specific phase label.
	CurrentWeights LiveWeights    // clone of the current weight map.
	LastChangedAt  time.Time      // wall-clock time of the most recent change.
	Detail         map[string]any // engine-specific structured diagnostics.
}

// Engine is the per-Service strategy driver. Implementations must be
// safe for concurrent use: the proxy reads LiveWeights from arbitrary
// goroutines while Start/Stop/Update/State run on control-plane paths.
type Engine interface {
	// LiveWeights returns a fresh copy of the current weight map.
	LiveWeights() LiveWeights
	// Start launches any background timers. Idempotent.
	Start(ctx context.Context) error
	// Stop cancels timers and waits for goroutine exit. Idempotent.
	Stop() error
	// Update applies a new ServiceSpec atomically.
	Update(spec service.ServiceSpec) error
	// State returns a diagnostic snapshot.
	State() State
}

// EventEmitter receives engine progress events. The Manager supplies
// a concrete implementation that translates each call into a
// service.ManagerEvent published on the service event bus.
type EventEmitter interface {
	// EmitCanaryStep fires after the canary engine bumps weight toward
	// the target backend. Weights is the post-step LiveWeights.
	EmitCanaryStep(serviceID service.ServiceID, target string, weights LiveWeights)
	// EmitCanaryAborted fires when an abort_on condition matches.
	EmitCanaryAborted(serviceID service.ServiceID, reason string)
	// EmitBlueGreenFlip fires when a flip starts (entering drain).
	EmitBlueGreenFlip(serviceID service.ServiceID, from, to string)
}

// MetricEvaluator resolves a single canary condition expression
// (success_criteria element or abort_on element) to a boolean. v1
// keeps the contract narrow; the capsule-metrics-backed
// implementation lands in 11B.21.
type MetricEvaluator interface {
	// Evaluate returns (matched, err). A nil error with matched=false
	// means the condition is well-formed but not currently true.
	Evaluate(condition string) (bool, error)
}

// noopEmitter satisfies EventEmitter when no emitter is wired.
type noopEmitter struct{}

func (noopEmitter) EmitCanaryStep(service.ServiceID, string, LiveWeights) {}
func (noopEmitter) EmitCanaryAborted(service.ServiceID, string)           {}
func (noopEmitter) EmitBlueGreenFlip(service.ServiceID, string, string)   {}
