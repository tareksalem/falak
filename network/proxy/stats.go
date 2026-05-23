// This file implements the per-Service stats counter set for the L4
// proxy (plan 11B.15) and its lifecycle registry (decision #35).
//
// All hot-path counters are atomic.Int64 so the TCP / UDP forwarders
// can update them without acquiring a mutex per packet. The only
// mutex-protected field is picks_per_backend which the SWRR layer
// updates once per pick (low-rate, cardinality-bounded by backend
// count). Snapshot copies values cheaply for API exposure.
//
// Per decision #28 the proxy emits NO per-connection logs at any
// level — connection refusals, NXDOMAINs, eject events all go through
// network/internal/samplelog (one event/sec/class/service) and bump
// counters here. The accompanying log-sampler is constructed in the
// proxy struct (tcp.go) so it can be shared across the two forwarders.
//
// Lifecycle (decision #35): a Service delete must evict the Stats
// entry from the Registry so memory is bounded across create+delete
// cycles. The Registry exposes Delete for that path.
package proxy

import (
	"sync"
	"sync/atomic"
)

// Stats holds the per-Service counter set. Atomic counters are updated
// from the TCP and UDP forwarders without locking; PicksPerBackend is
// updated less frequently and uses an internal mutex.
type Stats struct {
	// ConnectionsOpened increments on every accepted connection /
	// new UDP flow.
	ConnectionsOpened atomic.Int64
	// ConnectionsActive tracks live forwarders. Incremented on accept,
	// decremented on close.
	ConnectionsActive atomic.Int64
	// ConnectionsClosed increments after a forwarder finishes (success
	// or failure).
	ConnectionsClosed atomic.Int64
	// BytesSent counts bytes proxied from the client to the backend.
	BytesSent atomic.Int64
	// BytesReceived counts bytes proxied from the backend to the
	// client.
	BytesReceived atomic.Int64
	// ConnectErrors counts dial / setup failures (no replica reached).
	ConnectErrors atomic.Int64
	// ForwardErrors counts mid-stream forwarding failures.
	ForwardErrors atomic.Int64
	// ForwardErrorsNoReplica counts the subset of ForwardErrors that
	// were caused by ErrNoHealthyReplica.
	ForwardErrorsNoReplica atomic.Int64
	// OutlierEjections counts replica ejections (dial + forward
	// combined).
	OutlierEjections atomic.Int64

	pmu             sync.Mutex
	picksPerBackend map[string]int64
}

// newStats constructs a zero-valued Stats with the picks map ready.
func newStats() *Stats {
	return &Stats{picksPerBackend: make(map[string]int64)}
}

// AddPick increments the pick count for backend. Cheap mutex on a
// short critical section — pick rate is dominated by connection rate,
// not by per-packet throughput.
func (s *Stats) AddPick(backend string) {
	s.pmu.Lock()
	s.picksPerBackend[backend]++
	s.pmu.Unlock()
}

// StatsSnapshot is an immutable copy of a Stats at one moment, safe
// to ship over the API.
type StatsSnapshot struct {
	ConnectionsOpened      int64
	ConnectionsActive      int64
	ConnectionsClosed      int64
	BytesSent              int64
	BytesReceived          int64
	ConnectErrors          int64
	ForwardErrors          int64
	ForwardErrorsNoReplica int64
	OutlierEjections       int64
	PicksPerBackend        map[string]int64
}

// Snapshot returns a deep copy of the Stats counter set.
func (s *Stats) Snapshot() StatsSnapshot {
	snap := StatsSnapshot{
		ConnectionsOpened:      s.ConnectionsOpened.Load(),
		ConnectionsActive:      s.ConnectionsActive.Load(),
		ConnectionsClosed:      s.ConnectionsClosed.Load(),
		BytesSent:              s.BytesSent.Load(),
		BytesReceived:          s.BytesReceived.Load(),
		ConnectErrors:          s.ConnectErrors.Load(),
		ForwardErrors:          s.ForwardErrors.Load(),
		ForwardErrorsNoReplica: s.ForwardErrorsNoReplica.Load(),
		OutlierEjections:       s.OutlierEjections.Load(),
		PicksPerBackend:        make(map[string]int64),
	}
	s.pmu.Lock()
	for k, v := range s.picksPerBackend {
		snap.PicksPerBackend[k] = v
	}
	s.pmu.Unlock()
	return snap
}

// Reset zeroes every counter. Used by tests; the production lifecycle
// is the StatsRegistry, which evicts entries on Service delete rather
// than clearing them in-place.
func (s *Stats) Reset() {
	s.ConnectionsOpened.Store(0)
	s.ConnectionsActive.Store(0)
	s.ConnectionsClosed.Store(0)
	s.BytesSent.Store(0)
	s.BytesReceived.Store(0)
	s.ConnectErrors.Store(0)
	s.ForwardErrors.Store(0)
	s.ForwardErrorsNoReplica.Store(0)
	s.OutlierEjections.Store(0)
	s.pmu.Lock()
	s.picksPerBackend = make(map[string]int64)
	s.pmu.Unlock()
}

// StatsRegistry holds one Stats per Service ID and evicts entries on
// Service delete (decision #35). Safe for concurrent use.
type StatsRegistry struct {
	mu      sync.RWMutex
	entries map[string]*Stats
}

// NewStatsRegistry constructs an empty StatsRegistry.
func NewStatsRegistry() *StatsRegistry {
	return &StatsRegistry{entries: make(map[string]*Stats)}
}

// GetOrCreate returns the Stats for serviceID, allocating one on first
// access. Called from the data path on every connection.
func (r *StatsRegistry) GetOrCreate(serviceID string) *Stats {
	r.mu.RLock()
	st, ok := r.entries[serviceID]
	r.mu.RUnlock()
	if ok {
		return st
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.entries[serviceID]; ok {
		return st
	}
	st = newStats()
	r.entries[serviceID] = st
	return st
}

// Get returns the Stats for serviceID, or nil if not present.
func (r *StatsRegistry) Get(serviceID string) *Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[serviceID]
}

// Snapshot returns a snapshot of the Stats for serviceID, or nil if
// not present. The snapshot is safe to retain.
func (r *StatsRegistry) Snapshot(serviceID string) *StatsSnapshot {
	r.mu.RLock()
	st, ok := r.entries[serviceID]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	snap := st.Snapshot()
	return &snap
}

// Delete evicts the Stats entry for serviceID. Called on
// EventServiceDeleted to keep memory bounded across create+delete
// churn.
func (r *StatsRegistry) Delete(serviceID string) {
	r.mu.Lock()
	delete(r.entries, serviceID)
	r.mu.Unlock()
}

// Len returns the number of tracked services. Used by tests.
func (r *StatsRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// Reset zeroes the Stats for serviceID. Used by tests.
func (r *StatsRegistry) Reset(serviceID string) {
	r.mu.RLock()
	st, ok := r.entries[serviceID]
	r.mu.RUnlock()
	if ok {
		st.Reset()
	}
}
