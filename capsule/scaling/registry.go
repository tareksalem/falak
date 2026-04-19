package scaling

import (
	"sync"

	"github.com/tareksalem/falak/capsule"
)

// MetricsRegistry is a per-capsule lookup for MetricsProvider instances.
//
// The runtime module owns the actual measurement of container metrics
// (CPU, memory, RPS, etc). When a runtime starts executing a capsule, it
// constructs a MetricsProvider for that capsule and registers it here.
// When it stops execution, it unregisters.
//
// The scaling.Monitor reads from the registry on each evaluation tick,
// so providers can be swapped in/out as capsules are assigned to runtimes
// without restarting the monitor.
//
// Implementations must be safe for concurrent use.
type MetricsRegistry interface {
	// Register installs a provider for the given capsule. Any previously
	// registered provider for this capsule is replaced.
	Register(id capsule.CapsuleID, provider MetricsProvider)

	// Unregister removes the provider for the given capsule. A no-op if
	// no provider was registered.
	Unregister(id capsule.CapsuleID)

	// Get returns the provider for a capsule, or nil if none is registered.
	// Callers must not cache the returned provider — always call Get again
	// on the next evaluation so runtime swaps are observed.
	Get(id capsule.CapsuleID) MetricsProvider
}

// InMemoryRegistry is a thread-safe in-memory implementation of MetricsRegistry.
// It is the default for single-node deployments; production clusters may
// provide alternate implementations (e.g. remote scraping).
type InMemoryRegistry struct {
	mu        sync.RWMutex
	providers map[capsule.CapsuleID]MetricsProvider
}

// NewInMemoryRegistry creates an empty in-memory registry.
func NewInMemoryRegistry() *InMemoryRegistry {
	return &InMemoryRegistry{
		providers: make(map[capsule.CapsuleID]MetricsProvider),
	}
}

// Register adds or replaces the provider for a capsule.
func (r *InMemoryRegistry) Register(id capsule.CapsuleID, provider MetricsProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[id] = provider
}

// Unregister removes the provider for a capsule.
func (r *InMemoryRegistry) Unregister(id capsule.CapsuleID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.providers, id)
}

// Get returns the provider for a capsule, or nil if not registered.
func (r *InMemoryRegistry) Get(id capsule.CapsuleID) MetricsProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[id]
}

// Count returns the number of registered providers (for observability).
func (r *InMemoryRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.providers)
}
