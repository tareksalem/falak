package strategy

import (
	"context"
	"sync"
	"time"

	"github.com/tareksalem/falak/service"
)

// Static is the trivial Engine: it surfaces the backend weights from
// the ServiceSpec as live weights, with hot-reload on Update.
//
// Resolution filtering is the Manager's responsibility — it zeroes
// the weight of any backend that has transitioned to
// UnresolvedIdentityChanged or is otherwise
// inadmissible before handing the spec to Update. The engine
// therefore implements a single rule: keep backends with strictly
// positive weight.
type Static struct {
	serviceID service.ServiceID
	now       func() time.Time
	mu        sync.RWMutex
	weights   LiveWeights
	lastSet   time.Time
}

// StaticOption configures a Static engine.
type StaticOption func(*Static)

// WithStaticClock injects a clock for deterministic tests.
func WithStaticClock(now func() time.Time) StaticOption {
	return func(s *Static) {
		if now != nil {
			s.now = now
		}
	}
}

// NewStatic constructs a Static engine pre-populated from spec.
func NewStatic(id service.ServiceID, spec service.ServiceSpec, opts ...StaticOption) *Static {
	s := &Static{serviceID: id, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	s.weights = buildStaticWeights(spec)
	s.lastSet = s.now()
	return s
}

// LiveWeights returns a fresh copy of the current weight map.
func (s *Static) LiveWeights() LiveWeights {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.weights.Clone()
}

// Start is a no-op; the static engine runs no background timers.
func (s *Static) Start(_ context.Context) error { return nil }

// Stop is a no-op.
func (s *Static) Stop() error { return nil }

// Update rebuilds the weight map from the supplied spec. Backends
// with non-positive weight (the convention for unresolved or
// excluded backends) are dropped.
func (s *Static) Update(spec service.ServiceSpec) error {
	next := buildStaticWeights(spec)
	s.mu.Lock()
	s.weights = next
	s.lastSet = s.now()
	s.mu.Unlock()
	return nil
}

// State returns a diagnostic snapshot.
func (s *Static) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return State{
		Type:           string(service.StrategyTypeEnum.Static()),
		Phase:          "active",
		CurrentWeights: s.weights.Clone(),
		LastChangedAt:  s.lastSet,
		Detail:         map[string]any{"backend_count": len(s.weights)},
	}
}

// buildStaticWeights derives the live weight map from a spec.
// Backends with non-positive weight are omitted.
func buildStaticWeights(spec service.ServiceSpec) LiveWeights {
	out := make(LiveWeights, len(spec.Backends))
	for _, b := range spec.Backends {
		if b.Weight > 0 {
			out[b.Capsule] = b.Weight
		}
	}
	return out
}
