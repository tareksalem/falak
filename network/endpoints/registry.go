package endpoints

import (
	"context"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
)

// SwimStateAlive is the wire value receivers treat as "route to me".
// Records in any other state are kept in LookupAll but excluded from
// Lookup.
const SwimStateAlive = "alive"

// DefaultSweepInterval is how often the registry scans for expired
// entries. 5 seconds is small enough to evict gone peers promptly and
// large enough that the sweep cost is invisible at expected fleet
// sizes.
const DefaultSweepInterval = 5 * time.Second

// Endpoint is the registry-side view of an EndpointRecord. The
// Registry stores Endpoints (decoupled from the wire proto) so
// downstream consumers (DNS, L4 proxy) can depend on a stable shape.
type Endpoint struct {
	// ClusterPath identifies the cluster the publisher belongs to.
	ClusterPath string
	// GroupID is the capsule group's stable identifier.
	GroupID string
	// CapsuleName is the human-friendly DNS-visible name.
	CapsuleName string
	// ReplicaID uniquely names one running instance.
	ReplicaID string
	// NodeID is the publisher node; lets the proxy route via overlay.
	NodeID string
	// BridgeIP is the IP exposed on the per-group bridge on NodeID.
	BridgeIP string
	// NamedPorts are the declared (name -> port + protocol) bindings.
	NamedPorts []NamedPort
	// SwimState is the publisher's current SWIM view of the replica.
	SwimState string
	// EmittedAt is the time the publisher last refreshed this record.
	EmittedAt time.Time
	// TTL is the publisher-advertised lifetime hint.
	TTL time.Duration
}

// NamedPort is the registry view of a single declared port.
type NamedPort struct {
	Name          string
	ContainerPort uint32
	Protocol      string
}

// EndpointAddress is the (BridgeIP, port) pair returned by
// LookupNamedPort. The proxy uses it directly to dial a backend.
type EndpointAddress struct {
	BridgeIP string
	Port     uint32
	NodeID   string
	Protocol string
}

// Registry holds the local mirror of EndpointRecords learnt over the
// per-group gossip topics. Insert / Withdraw mutate; Lookup variants
// read; a background sweeper evicts entries past their TTL window.
type Registry struct {
	logger        *zap.Logger
	sweepInterval time.Duration
	now           func() time.Time

	mu      sync.RWMutex
	records map[string]Endpoint // keyed by recordKey()

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

// RegistryOption configures a Registry.
type RegistryOption func(*Registry)

// WithRegistryLogger sets the zap logger.
func WithRegistryLogger(l *zap.Logger) RegistryOption {
	return func(r *Registry) { r.logger = l }
}

// WithSweepInterval overrides DefaultSweepInterval. Useful in tests
// to drive the sweeper to a fast cadence.
func WithSweepInterval(d time.Duration) RegistryOption {
	return func(r *Registry) { r.sweepInterval = d }
}

// WithNowFunc injects a clock for deterministic tests.
func WithNowFunc(fn func() time.Time) RegistryOption {
	return func(r *Registry) { r.now = fn }
}

// NewRegistry constructs a Registry and starts its TTL sweeper.
// Stop the registry via Stop to terminate the sweeper goroutine.
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{
		logger:        zap.NewNop(),
		sweepInterval: DefaultSweepInterval,
		now:           time.Now,
		records:       make(map[string]Endpoint),
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.sweepInterval <= 0 {
		r.sweepInterval = DefaultSweepInterval
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.wg.Add(1)
	go r.sweepLoop()
	return r
}

// Insert upserts a record into the registry. EmittedAt is taken from
// the proto; if missing, the registry's clock is used so the entry
// participates in the sweep correctly.
func (r *Registry) Insert(rec *endpointpb.EndpointRecord) {
	if rec == nil {
		return
	}
	ep := Endpoint{
		ClusterPath: rec.ClusterPath,
		GroupID:     rec.GroupId,
		CapsuleName: rec.CapsuleName,
		ReplicaID:   rec.ReplicaId,
		NodeID:      rec.NodeId,
		BridgeIP:    rec.BridgeIp,
		SwimState:   rec.SwimState,
		TTL:         time.Duration(rec.TtlSeconds) * time.Second,
	}
	if rec.EmittedAt != nil {
		ep.EmittedAt = rec.EmittedAt.AsTime()
	} else {
		ep.EmittedAt = r.now()
	}
	if ep.TTL <= 0 {
		ep.TTL = DefaultPublisherTTL
	}
	for _, np := range rec.NamedPorts {
		ep.NamedPorts = append(ep.NamedPorts, NamedPort{
			Name:          np.Name,
			ContainerPort: np.ContainerPort,
			Protocol:      np.Protocol,
		})
	}

	key := recordKey(ep.ClusterPath, ep.GroupID, ep.CapsuleName, ep.ReplicaID)
	r.mu.Lock()
	r.records[key] = ep
	r.mu.Unlock()
}

// Withdraw removes the record for (cluster, group, capsule, replica).
// Idempotent: removing an unknown key is a no-op.
func (r *Registry) Withdraw(w *endpointpb.EndpointWithdrawal) {
	if w == nil {
		return
	}
	key := recordKey(w.ClusterPath, w.GroupId, w.CapsuleName, w.ReplicaId)
	r.mu.Lock()
	delete(r.records, key)
	r.mu.Unlock()
}

// WithdrawKey removes by explicit tuple. Used by callers that don't
// have an EndpointWithdrawal proto handy.
func (r *Registry) WithdrawKey(clusterPath, groupID, capsuleName, replicaID string) {
	key := recordKey(clusterPath, groupID, capsuleName, replicaID)
	r.mu.Lock()
	delete(r.records, key)
	r.mu.Unlock()
}

// Lookup returns all alive endpoints for (cluster, group, capsule)
// sorted by EmittedAt descending (freshest first). Suspect and dead
// records are filtered out — callers wanting the unfiltered view use
// LookupAll.
func (r *Registry) Lookup(clusterPath, groupID, capsuleName string) []Endpoint {
	return r.lookupFiltered(clusterPath, groupID, capsuleName, true)
}

// LookupAll returns every endpoint regardless of SwimState. Used by
// diagnostics and reconciliation paths.
func (r *Registry) LookupAll(clusterPath, groupID, capsuleName string) []Endpoint {
	return r.lookupFiltered(clusterPath, groupID, capsuleName, false)
}

func (r *Registry) lookupFiltered(clusterPath, groupID, capsuleName string, aliveOnly bool) []Endpoint {
	r.mu.RLock()
	out := make([]Endpoint, 0)
	for _, ep := range r.records {
		if ep.ClusterPath != clusterPath || ep.GroupID != groupID || ep.CapsuleName != capsuleName {
			continue
		}
		if aliveOnly && ep.SwimState != SwimStateAlive {
			continue
		}
		out = append(out, ep)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].EmittedAt.After(out[j].EmittedAt)
	})
	return out
}

// LookupNamedPort returns (BridgeIP, port) pairs for every alive
// replica that exposes a port named portName. Used by the L4 proxy to
// dial backends by name (e.g. "http") rather than by numeric port.
func (r *Registry) LookupNamedPort(clusterPath, groupID, capsuleName, portName string) []EndpointAddress {
	eps := r.Lookup(clusterPath, groupID, capsuleName)
	out := make([]EndpointAddress, 0, len(eps))
	for _, ep := range eps {
		for _, np := range ep.NamedPorts {
			if np.Name == portName {
				out = append(out, EndpointAddress{
					BridgeIP: ep.BridgeIP,
					Port:     np.ContainerPort,
					NodeID:   ep.NodeID,
					Protocol: np.Protocol,
				})
			}
		}
	}
	return out
}

// Snapshot returns a copy of every endpoint regardless of state. Used
// by integration tests and the diagnostics gRPC.
func (r *Registry) Snapshot() []Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Endpoint, 0, len(r.records))
	for _, ep := range r.records {
		out = append(out, ep)
	}
	return out
}

// Len returns the current entry count.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.records)
}

// Stop terminates the sweeper goroutine. Stop is idempotent.
func (r *Registry) Stop() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.cancel()
	r.mu.Unlock()
	r.wg.Wait()
}

// sweepLoop evicts entries whose EmittedAt is older than 2 × TTL. The
// 2× factor lets one missed refresh slide; two misses in a row mean
// the peer is genuinely gone.
func (r *Registry) sweepLoop() {
	defer r.wg.Done()
	t := time.NewTicker(r.sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			r.sweep()
		}
	}
}

func (r *Registry) sweep() {
	now := r.now()
	var evicted []string
	r.mu.Lock()
	for key, ep := range r.records {
		ttl := ep.TTL
		if ttl <= 0 {
			ttl = DefaultPublisherTTL
		}
		if now.Sub(ep.EmittedAt) > 2*ttl {
			delete(r.records, key)
			evicted = append(evicted, key)
		}
	}
	r.mu.Unlock()
	if len(evicted) > 0 {
		r.logger.Debug("endpoint registry swept",
			zap.Int("count", len(evicted)))
	}
}
