// This file implements the replica selector with outlier detection
// for the L4 proxy (plan 11B.12). Once SWRR picks a backend (logical
// capsule name), the Selector picks one of that backend's replicas
// from the endpoint registry, filtered by outlier-detection state.
//
// Outlier detection differentiates two failure types (decision #30):
//
//   1. Dial failures (backend dial never produced a connection) —
//      ambiguous between replica death and underlay blip. Mitigation:
//      down-weight (halve selection probability) for the eject window.
//      After three consecutive halvings the replica is ejected — a
//      single flap does not remove it from rotation.
//
//   2. Forward failures (RST / abort / read-after-write) — unambiguous
//      replica failure. Mitigation: immediate eject for the current
//      eject duration; the next ejection doubles the window up to the
//      configured maximum.
//
// State is per-replica (capsule_name + replica_id) and lives entirely
// in this package; the registry sees only success / failure events.
// Ejected replicas are filtered at Pick time; the eject window is
// checked against an injectable clock (WithNowFunc) for deterministic
// tests.
package proxy

import (
	"errors"
	"math/rand"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/network/endpoints"
)

// Defaults align with the locked design (decision #30).
const (
	// DefaultDialFailureThreshold is the consecutive-halving count
	// after which a dial-failing replica is ejected.
	DefaultDialFailureThreshold = 3
	// DefaultEjectInitial is the first eject duration.
	DefaultEjectInitial = 30 * time.Second
	// DefaultEjectMax caps the backoff growth.
	DefaultEjectMax = 5 * time.Minute
	// DefaultBackoffFactor doubles the eject window on each ejection.
	DefaultBackoffFactor = 2
	// DefaultDialWindow is the rolling window inside which dial
	// failures count toward the consecutive-halving threshold. A
	// success or expiry resets the counter.
	DefaultDialWindow = 30 * time.Second
)

// ErrNoHealthyReplica is returned by Pick when no replica passes both
// the alive-only and outlier-eject filters.
var ErrNoHealthyReplica = errors.New("proxy: no healthy replica")

// replicaKey identifies a replica in the outlier state map.
type replicaKey struct {
	CapsuleName string
	ReplicaID   string
}

// replicaState is the per-replica outlier-detection state. dialFailures
// counts consecutive halvings (not raw failures); each one halves the
// selection probability via downWeight.
type replicaState struct {
	mu                   sync.Mutex
	dialFailures         int32
	lastDialFailureAt    time.Time
	downWeight           float64 // 1.0 = full, 0.5 = halved once, 0.125 = halved 3x.
	ejectedUntil         time.Time
	currentEjectDuration time.Duration
}

// Selector chooses a replica for a (capsule, group, cluster) tuple,
// driven by an endpoints.Registry and the outlier state above.
type Selector struct {
	logger               *zap.Logger
	registry             *endpoints.Registry
	rng                  *rand.Rand
	now                  func() time.Time
	dialFailureThreshold int32
	ejectInitial         time.Duration
	ejectMax             time.Duration
	backoffFactor        int
	dialWindow           time.Duration

	mu    sync.Mutex
	state map[replicaKey]*replicaState
}

// SelectorOption configures a Selector.
type SelectorOption func(*Selector)

// WithRegistry wires the endpoints registry the selector reads from.
// Required — Pick returns ErrNoHealthyReplica if unset.
func WithRegistry(r *endpoints.Registry) SelectorOption {
	return func(s *Selector) { s.registry = r }
}

// WithSelectorLogger sets the zap logger. Defaults to NewNop.
func WithSelectorLogger(l *zap.Logger) SelectorOption {
	return func(s *Selector) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithDialFailureThreshold overrides DefaultDialFailureThreshold —
// the number of consecutive halvings before a replica is ejected.
func WithDialFailureThreshold(n int32) SelectorOption {
	return func(s *Selector) {
		if n > 0 {
			s.dialFailureThreshold = n
		}
	}
}

// WithEjectInitial overrides DefaultEjectInitial.
func WithEjectInitial(d time.Duration) SelectorOption {
	return func(s *Selector) {
		if d > 0 {
			s.ejectInitial = d
		}
	}
}

// WithEjectMax overrides DefaultEjectMax.
func WithEjectMax(d time.Duration) SelectorOption {
	return func(s *Selector) {
		if d > 0 {
			s.ejectMax = d
		}
	}
}

// WithBackoffFactor overrides DefaultBackoffFactor.
func WithBackoffFactor(f int) SelectorOption {
	return func(s *Selector) {
		if f >= 1 {
			s.backoffFactor = f
		}
	}
}

// WithSelectorRand injects the rng used for random replica selection.
func WithSelectorRand(r *rand.Rand) SelectorOption {
	return func(s *Selector) {
		if r != nil {
			s.rng = r
		}
	}
}

// WithSelectorNowFunc injects a clock for deterministic tests.
func WithSelectorNowFunc(fn func() time.Time) SelectorOption {
	return func(s *Selector) {
		if fn != nil {
			s.now = fn
		}
	}
}

// WithDialWindow overrides DefaultDialWindow.
func WithDialWindow(d time.Duration) SelectorOption {
	return func(s *Selector) {
		if d > 0 {
			s.dialWindow = d
		}
	}
}

// NewSelector constructs a Selector.
func NewSelector(opts ...SelectorOption) *Selector {
	s := &Selector{
		logger:               zap.NewNop(),
		now:                  time.Now,
		rng:                  rand.New(rand.NewSource(1)),
		dialFailureThreshold: DefaultDialFailureThreshold,
		ejectInitial:         DefaultEjectInitial,
		ejectMax:             DefaultEjectMax,
		backoffFactor:        DefaultBackoffFactor,
		dialWindow:           DefaultDialWindow,
		state:                make(map[replicaKey]*replicaState),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Pick returns one alive, non-ejected replica chosen at random with
// down-weight bias. Returns ErrNoHealthyReplica when no candidate
// remains.
func (s *Selector) Pick(clusterPath, groupID, capsuleName string) (endpoints.Endpoint, error) {
	if s.registry == nil {
		return endpoints.Endpoint{}, ErrNoHealthyReplica
	}
	eps := s.registry.Lookup(clusterPath, groupID, capsuleName)
	if len(eps) == 0 {
		return endpoints.Endpoint{}, ErrNoHealthyReplica
	}
	now := s.now()

	// First pass: filter out ejected replicas and collect per-replica
	// down-weights for the weighted-random pick.
	type cand struct {
		ep     endpoints.Endpoint
		weight float64
	}
	var (
		cands []cand
		total float64
	)
	for _, ep := range eps {
		st := s.getState(capsuleName, ep.ReplicaID)
		st.mu.Lock()
		if !st.ejectedUntil.IsZero() && now.Before(st.ejectedUntil) {
			st.mu.Unlock()
			continue
		}
		w := st.downWeight
		if w <= 0 {
			w = 1.0
		}
		st.mu.Unlock()
		cands = append(cands, cand{ep: ep, weight: w})
		total += w
	}
	if len(cands) == 0 || total <= 0 {
		return endpoints.Endpoint{}, ErrNoHealthyReplica
	}

	// Weighted-random pick over cand weights.
	s.mu.Lock()
	r := s.rng.Float64() * total
	s.mu.Unlock()
	for _, c := range cands {
		r -= c.weight
		if r <= 0 {
			return c.ep, nil
		}
	}
	return cands[len(cands)-1].ep, nil
}

// RecordDialFailure registers a dial failure against (capsuleName,
// replicaID). Each call halves the replica's selection weight; after
// dialFailureThreshold consecutive halvings the replica is ejected
// for the current eject duration. The dial-failure counter resets if
// the configured dialWindow elapses with no further failures.
func (s *Selector) RecordDialFailure(capsuleName, replicaID string) {
	st := s.getState(capsuleName, replicaID)
	now := s.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	// If the last failure is older than dialWindow, treat this as a
	// fresh streak.
	if !st.lastDialFailureAt.IsZero() && now.Sub(st.lastDialFailureAt) > s.dialWindow {
		st.dialFailures = 0
		st.downWeight = 1.0
	}
	st.lastDialFailureAt = now
	st.dialFailures++
	if st.downWeight <= 0 {
		st.downWeight = 1.0
	}
	st.downWeight /= 2
	if st.dialFailures >= s.dialFailureThreshold {
		s.ejectLocked(st, now)
	}
	s.logger.Debug("proxy selector recorded dial failure",
		zap.String("capsule", capsuleName),
		zap.String("replica", replicaID),
		zap.Int32("count", st.dialFailures),
		zap.Float64("down_weight", st.downWeight))
}

// RecordForwardFailure registers a mid-stream forward failure and
// immediately ejects the replica for the current eject duration.
// The next ejection doubles the duration, capped at ejectMax.
func (s *Selector) RecordForwardFailure(capsuleName, replicaID string) {
	st := s.getState(capsuleName, replicaID)
	now := s.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	s.ejectLocked(st, now)
	s.logger.Warn("proxy selector ejected replica on forward failure",
		zap.String("capsule", capsuleName),
		zap.String("replica", replicaID),
		zap.Duration("duration_ms", st.currentEjectDuration))
}

// RecordSuccess clears the dial-failure streak and restores the
// replica's selection weight. The eject duration is preserved across
// success — a replica that ejected once is still penalised on its
// next ejection. Calling this during an active eject window is a
// no-op (the replica is still ejected); the counter is only cleared
// once the window has expired.
func (s *Selector) RecordSuccess(capsuleName, replicaID string) {
	st := s.getState(capsuleName, replicaID)
	now := s.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.ejectedUntil.IsZero() && now.Before(st.ejectedUntil) {
		// Still within eject window — do nothing.
		return
	}
	st.dialFailures = 0
	st.downWeight = 1.0
	st.lastDialFailureAt = time.Time{}
}

// EjectedUntil reports the replica's current eject expiry. Used by
// stats / tests; returns the zero time if the replica is not ejected.
func (s *Selector) EjectedUntil(capsuleName, replicaID string) time.Time {
	st := s.getState(capsuleName, replicaID)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.ejectedUntil
}

// getState returns the replicaState, creating it if absent. The map
// lock is short — every Pick / Record* call goes through here.
func (s *Selector) getState(capsuleName, replicaID string) *replicaState {
	key := replicaKey{CapsuleName: capsuleName, ReplicaID: replicaID}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.state[key]
	if !ok {
		st = &replicaState{downWeight: 1.0}
		s.state[key] = st
	}
	return st
}

// ejectLocked is called with st.mu held. It applies the current
// eject duration and doubles the next one. The counters are reset
// inside the window — the replica is ineligible for selection until
// the window expires.
func (s *Selector) ejectLocked(st *replicaState, now time.Time) {
	if st.currentEjectDuration <= 0 {
		st.currentEjectDuration = s.ejectInitial
	}
	st.ejectedUntil = now.Add(st.currentEjectDuration)
	// Pre-arm the next ejection at backoffFactor× the current one.
	next := st.currentEjectDuration * time.Duration(s.backoffFactor)
	if next > s.ejectMax {
		next = s.ejectMax
	}
	st.currentEjectDuration = next
	// Counter & weight collapse — the replica is out of rotation.
	st.dialFailures = 0
	st.downWeight = 0
}
