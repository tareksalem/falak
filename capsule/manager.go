package capsule

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ErrNotFound is returned when a capsule lookup finds no match.
var ErrNotFound = errors.New("capsule not found")

// ManagerEvent is emitted by the capsule manager on lifecycle actions.
type ManagerEvent struct {
	Type      string
	CapsuleID CapsuleID
	Capsule   *Capsule
	Timestamp time.Time
}

// Manager event types.
const (
	EventCapsuleCreated       = "capsule.created"
	EventCapsuleUpdated       = "capsule.updated"
	EventCapsuleDeleted       = "capsule.deleted"
	EventCapsuleReceived      = "capsule.received"
	EventCapsuleAnnounced     = "capsule.announced"
	EventCapsuleRunning       = "capsule.running"
	EventCapsuleStopping      = "capsule.stopping"
	EventCapsuleStopped       = "capsule.stopped"
	EventCapsuleStatusChanged = "capsule.status_changed"
)

// EventHandler is called when a manager event occurs.
type EventHandler func(event ManagerEvent)

// Manager handles capsule lifecycle: create, get, list, delete, and mesh coordination.
//
// Capsules belong to a cluster (identified by clusterID, e.g. "region/dc/cluster").
// A single Manager can hold capsules from multiple clusters — the cluster is
// supplied at Create time, not at construction time.
type Manager struct {
	mu        sync.RWMutex
	store     *Store
	handlerMu sync.RWMutex
	handler   EventHandler
	logger    *zap.Logger
	metrics   Metrics

	// lifecycles holds one state machine per capsule. The machine validates
	// every transition and drives status updates — no code should set
	// capsule.Status directly.
	lifecyclesMu sync.RWMutex
	lifecycles   map[CapsuleID]*Lifecycle
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithManagerLogger sets the logger.
func WithManagerLogger(logger *zap.Logger) ManagerOption {
	return func(m *Manager) {
		m.logger = logger
	}
}

// WithManagerEventHandler sets the event handler.
func WithManagerEventHandler(h EventHandler) ManagerOption {
	return func(m *Manager) {
		m.handler = h
	}
}

// WithManagerStore sets the capsule store.
func WithManagerStore(s *Store) ManagerOption {
	return func(m *Manager) {
		m.store = s
	}
}

// WithManagerMetrics sets the metrics sink for observability. Defaults to NoopMetrics.
func WithManagerMetrics(m Metrics) ManagerOption {
	return func(mgr *Manager) {
		mgr.metrics = m
	}
}

// NewManager creates a new capsule manager.
func NewManager(opts ...ManagerOption) *Manager {
	mgr := &Manager{
		store:      NewStore(),
		handler:    func(ManagerEvent) {},
		logger:     zap.NewNop(),
		metrics:    NoopMetrics{},
		lifecycles: make(map[CapsuleID]*Lifecycle),
	}
	for _, opt := range opts {
		opt(mgr)
	}

	// Restore lifecycles for any capsules already in the store (e.g. after
	// reloading from SQLite on restart). The machine starts at the capsule's
	// persisted status.
	for _, c := range mgr.store.List() {
		mgr.lifecycles[c.ID] = mgr.newLifecycle(c.ID, c.Status)
	}

	return mgr
}

// newLifecycle constructs a Lifecycle wired to emit via the manager's event bus.
func (m *Manager) newLifecycle(id CapsuleID, initial CapsuleStatus) *Lifecycle {
	return NewLifecycle(id,
		WithLifecycleLogger(m.logger.Named("lifecycle")),
		WithLifecycleInitialState(initial),
		WithLifecycleHandler(func(event LifecycleEvent) {
			m.onLifecycleTransition(event)
		}),
	)
}

// onLifecycleTransition reacts to state machine transitions: it persists the
// new status and emits a ManagerEvent so external subscribers stay in sync.
func (m *Manager) onLifecycleTransition(event LifecycleEvent) {
	c := m.store.Get(event.CapsuleID)
	if c == nil {
		return
	}

	m.mu.Lock()
	c.Status = event.To
	c.UpdatedAt = time.Now()
	m.mu.Unlock()

	if err := m.store.Update(c); err != nil {
		m.logger.Error("failed to persist lifecycle transition",
			zap.String("capsule_id", string(event.CapsuleID)),
			zap.String("cluster", c.ClusterID),
			zap.String("from", string(event.From)),
			zap.String("to", string(event.To)),
			zap.String("trigger", event.Trigger),
			zap.Error(err))
		return
	}

	m.logger.Debug("capsule lifecycle transitioned",
		zap.String("capsule_id", string(event.CapsuleID)),
		zap.String("cluster", c.ClusterID),
		zap.String("from", string(event.From)),
		zap.String("to", string(event.To)),
		zap.String("trigger", event.Trigger))

	m.metrics.IncStatusTransition(event.From, event.To, event.Trigger)
	m.emit(statusEventType(event.To), c)
}

// statusEventType maps a CapsuleStatus to the corresponding ManagerEvent type.
func statusEventType(status CapsuleStatus) string {
	switch status {
	case CapsuleStatusEnum.Announced():
		return EventCapsuleAnnounced
	case CapsuleStatusEnum.Running():
		return EventCapsuleRunning
	case CapsuleStatusEnum.Stopping():
		return EventCapsuleStopping
	case CapsuleStatusEnum.Stopped():
		return EventCapsuleStopped
	default:
		return EventCapsuleStatusChanged
	}
}

// Create validates a spec, creates a capsule for the given cluster, stores it,
// and emits CapsuleCreated. The clusterID identifies which cluster this capsule
// belongs to (e.g. "us-east/dc1/prod"); a single Manager can hold capsules from
// many clusters.
func (m *Manager) Create(_ context.Context, clusterID string, spec CapsuleSpec) (*Capsule, error) {
	if clusterID == "" {
		return nil, fmt.Errorf("clusterID is required")
	}

	// Apply defaults
	DefaultSpec(&spec)

	// Validate
	if err := ValidateSpec(&spec); err != nil {
		return nil, fmt.Errorf("invalid capsule spec: %w", err)
	}

	now := time.Now()
	c := &Capsule{
		ID:        NewCapsuleID(),
		ClusterID: clusterID,
		Spec:      spec,
		Status:    CapsuleStatusEnum.Created(),
		Replicas:  nil,
		Momentum: MomentumState{
			Current:      spec.MomentumConfig.Base,
			Base:         spec.MomentumConfig.Base,
			LastAdjusted: now,
		},
		Version:   "1",
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := m.store.Create(c); err != nil {
		return nil, fmt.Errorf("failed to store capsule: %w", err)
	}

	// Install a fresh lifecycle starting at Created.
	m.lifecyclesMu.Lock()
	m.lifecycles[c.ID] = m.newLifecycle(c.ID, CapsuleStatusEnum.Created())
	m.lifecyclesMu.Unlock()

	m.logger.Info("capsule created",
		zap.String("id", c.ID.String()),
		zap.String("name", spec.Name),
		zap.String("cluster", clusterID),
		zap.String("orbit", spec.Orbit),
		zap.String("tier", string(spec.Tier)))

	m.metrics.IncCreated(clusterID)
	m.emit(EventCapsuleCreated, c)

	// Immediately advance the lifecycle to Announced. The originator
	// always announces the capsule on creation; remote nodes that
	// receive the capsule via gossip start their lifecycle at the
	// already-Announced state mirrored from the originator. This makes
	// the subsequent StartElection transition (Announced → Electing)
	// always valid on every node.
	if err := m.Announce(c.ID); err != nil {
		m.logger.Warn("auto-announce after create failed",
			zap.String("id", c.ID.String()),
			zap.Error(err))
	}
	return c, nil
}

// Get retrieves a capsule by ID.
func (m *Manager) Get(id CapsuleID) *Capsule {
	return m.store.Get(id)
}

// GetByName retrieves a capsule by name.
func (m *Manager) GetByName(name string) *Capsule {
	return m.store.GetByName(name)
}

// List returns all capsules.
func (m *Manager) List() []*Capsule {
	return m.store.List()
}

// ListByOrbit returns capsules in a specific orbit.
func (m *Manager) ListByOrbit(orbit string) []*Capsule {
	return m.store.ListByOrbit(orbit)
}

// ListByLabels returns capsules matching the given labels.
func (m *Manager) ListByLabels(labels Labels) []*Capsule {
	return m.store.ListByLabels(labels)
}

// Update modifies a capsule spec and re-announces it.
// The capsule's clusterID is preserved.
func (m *Manager) Update(_ context.Context, id CapsuleID, spec CapsuleSpec) (*Capsule, error) {
	DefaultSpec(&spec)

	if err := ValidateSpec(&spec); err != nil {
		return nil, fmt.Errorf("invalid capsule spec: %w", err)
	}

	c := m.store.Get(id)
	if c == nil {
		return nil, fmt.Errorf("capsule %s not found", id)
	}

	m.mu.Lock()
	c.Spec = spec
	c.UpdatedAt = time.Now()
	m.mu.Unlock()

	if err := m.store.Update(c); err != nil {
		return nil, fmt.Errorf("failed to update capsule: %w", err)
	}

	m.logger.Info("capsule updated",
		zap.String("id", c.ID.String()),
		zap.String("name", spec.Name),
		zap.String("cluster", c.ClusterID))

	m.emit(EventCapsuleUpdated, c)
	return c, nil
}

// Delete removes a capsule and emits CapsuleDeleted.
func (m *Manager) Delete(_ context.Context, id CapsuleID) error {
	c := m.store.Get(id)
	if c == nil {
		return fmt.Errorf("capsule %s not found", id)
	}

	if err := m.store.Delete(id); err != nil {
		return fmt.Errorf("failed to delete capsule: %w", err)
	}

	// Drop the lifecycle machine for this capsule.
	m.lifecyclesMu.Lock()
	delete(m.lifecycles, id)
	m.lifecyclesMu.Unlock()

	m.logger.Info("capsule deleted",
		zap.String("id", c.ID.String()),
		zap.String("name", c.Spec.Name),
		zap.String("cluster", c.ClusterID))

	m.emit(EventCapsuleDeleted, c)
	return nil
}

// Receive stores a capsule received from the mesh (via PubSub).
// This is called when another node announces a capsule.
// A lifecycle machine is installed for new capsules, starting at the
// received status so subsequent transitions can proceed validly.
func (m *Manager) Receive(c *Capsule) error {
	existing := m.store.Get(c.ID)
	if existing != nil {
		if err := m.store.Update(c); err != nil {
			return fmt.Errorf("failed to update received capsule: %w", err)
		}
		m.logger.Debug("capsule updated from mesh",
			zap.String("id", c.ID.String()),
			zap.String("name", c.Spec.Name),
			zap.String("cluster", c.ClusterID))
	} else {
		if err := m.store.Create(c); err != nil {
			return fmt.Errorf("failed to store received capsule: %w", err)
		}
		// Install a fresh lifecycle at the received capsule's current status.
		initial := c.Status
		if !initial.Valid() {
			initial = CapsuleStatusEnum.Announced()
		}
		m.lifecyclesMu.Lock()
		m.lifecycles[c.ID] = m.newLifecycle(c.ID, initial)
		m.lifecyclesMu.Unlock()

		m.logger.Info("capsule received from mesh",
			zap.String("id", c.ID.String()),
			zap.String("name", c.Spec.Name),
			zap.String("cluster", c.ClusterID),
			zap.String("orbit", c.Spec.Orbit))
	}

	m.metrics.IncReceived(c.ClusterID)
	m.emit(EventCapsuleReceived, c)
	return nil
}

// Fire drives a lifecycle transition for a capsule. This is the only way
// to change a capsule's status — the state machine validates the transition
// and rejects illegal moves. On success the store is updated and a
// ManagerEvent is emitted corresponding to the new status.
func (m *Manager) Fire(id CapsuleID, trigger string) error {
	lc := m.getLifecycle(id)
	if lc == nil {
		return fmt.Errorf("capsule %s not found", id)
	}
	if err := lc.Fire(trigger); err != nil {
		return fmt.Errorf("capsule %s: %w", id, err)
	}
	return nil
}

// Status returns the current lifecycle state for a capsule.
func (m *Manager) Status(id CapsuleID) (CapsuleStatus, error) {
	lc := m.getLifecycle(id)
	if lc == nil {
		return "", fmt.Errorf("capsule %s not found", id)
	}
	return lc.State(), nil
}

// Announce transitions Created → Announced (or Stopped → Announced).
func (m *Manager) Announce(id CapsuleID) error {
	return m.Fire(id, TriggerAnnounce)
}

// StartElection transitions Announced → Electing.
func (m *Manager) StartElection(id CapsuleID) error {
	return m.Fire(id, TriggerElectionStarted)
}

// WinElection transitions Electing → Assigned.
func (m *Manager) WinElection(id CapsuleID) error {
	return m.Fire(id, TriggerElectionWon)
}

// ElectionTimeout transitions Electing → Announced for a retry.
func (m *Manager) ElectionTimeout(id CapsuleID) error {
	return m.Fire(id, TriggerElectionTimeout)
}

// StartExecution transitions Assigned → Executing.
func (m *Manager) StartExecution(id CapsuleID) error {
	return m.Fire(id, TriggerExecutionStart)
}

// MarkRunning transitions Executing → Running after the container is ready.
func (m *Manager) MarkRunning(id CapsuleID) error {
	return m.Fire(id, TriggerContainerReady)
}

// ExecutionFailed transitions Assigned/Executing → Announced for re-election.
func (m *Manager) ExecutionFailed(id CapsuleID) error {
	return m.Fire(id, TriggerExecutionFailed)
}

// StopCapsule transitions Running → Stopping.
func (m *Manager) StopCapsule(id CapsuleID) error {
	return m.Fire(id, TriggerStopRequested)
}

// MarkStopped transitions Stopping → Stopped after the container exits.
func (m *Manager) MarkStopped(id CapsuleID) error {
	return m.Fire(id, TriggerContainerStopped)
}

// NodeFailed transitions Running → Announced for fast re-election on the mesh.
func (m *Manager) NodeFailed(id CapsuleID) error {
	return m.Fire(id, TriggerNodeFailed)
}

// ScaleUp transitions Running → Announced to trigger a new election for an additional replica.
func (m *Manager) ScaleUp(id CapsuleID) error {
	return m.Fire(id, TriggerScaleUpNeeded)
}

// ScaleDown transitions Running → Stopping to remove a replica.
func (m *Manager) ScaleDown(id CapsuleID) error {
	return m.Fire(id, TriggerScaleDownNeeded)
}

// getLifecycle returns the lifecycle machine for a capsule, or nil if absent.
func (m *Manager) getLifecycle(id CapsuleID) *Lifecycle {
	m.lifecyclesMu.RLock()
	defer m.lifecyclesMu.RUnlock()
	return m.lifecycles[id]
}

// AssignReplica records that a capsule replica is now hosted on a node.
// Called by the election manager when the local node wins an election,
// and by the capsule receive path when a status update from the mesh
// reveals a peer took over.
//
// AssignReplica is idempotent: calling it twice with the same
// (replicaID, nodeID) updates the existing slot instead of appending.
// If the replica already exists with a different node ID, the entry
// is overwritten — the latest writer wins.
func (m *Manager) AssignReplica(id CapsuleID, replicaID ReplicaID, nodeID string) error {
	c := m.store.Get(id)
	if c == nil {
		return fmt.Errorf("capsule %s not found", id)
	}

	m.mu.Lock()
	found := false
	for i, r := range c.Replicas {
		if r.ReplicaID == replicaID {
			c.Replicas[i].NodeID = nodeID
			c.Replicas[i].Status = CapsuleStatusEnum.Assigned()
			c.Replicas[i].StartedAt = time.Now()
			found = true
			break
		}
	}
	if !found {
		c.Replicas = append(c.Replicas, ReplicaState{
			ReplicaID: replicaID,
			NodeID:    nodeID,
			Status:    CapsuleStatusEnum.Assigned(),
			StartedAt: time.Now(),
		})
	}
	c.UpdatedAt = time.Now()
	m.mu.Unlock()

	if err := m.store.Update(c); err != nil {
		return fmt.Errorf("failed to persist replica assignment: %w", err)
	}

	m.logger.Info("capsule replica assigned",
		zap.String("id", id.String()),
		zap.String("replica_id", string(replicaID)),
		zap.String("node_id", nodeID))
	return nil
}

// SyncStatus updates a capsule's status to mirror remote state received from
// the mesh. Unlike Fire, this does NOT validate the transition — it is used
// only when mirroring state from another node's PubSub announcement.
// The local lifecycle machine is rebuilt at the new state so future local
// transitions proceed correctly from there.
func (m *Manager) SyncStatus(id CapsuleID, status CapsuleStatus) error {
	if !status.Valid() {
		return fmt.Errorf("invalid status %q", status)
	}

	c := m.store.Get(id)
	if c == nil {
		return fmt.Errorf("capsule %s not found", id)
	}

	m.mu.Lock()
	previous := c.Status
	c.Status = status
	c.UpdatedAt = time.Now()
	m.mu.Unlock()

	if err := m.store.Update(c); err != nil {
		return fmt.Errorf("failed to persist synced status: %w", err)
	}

	// Rebuild the lifecycle machine at the new state so local transitions
	// continue validly from here.
	m.lifecyclesMu.Lock()
	m.lifecycles[id] = m.newLifecycle(id, status)
	m.lifecyclesMu.Unlock()

	m.logger.Debug("capsule status synced from mesh",
		zap.String("id", id.String()),
		zap.String("from", string(previous)),
		zap.String("to", string(status)))

	m.emit(statusEventType(status), c)
	return nil
}

// Count returns the number of capsules.
func (m *Manager) Count() int {
	return m.store.Count()
}

// SetEventHandler updates the event handler after construction.
// Safe to call concurrently with event emission.
func (m *Manager) SetEventHandler(h EventHandler) {
	m.handlerMu.Lock()
	defer m.handlerMu.Unlock()
	if h == nil {
		m.handler = func(ManagerEvent) {}
		return
	}
	m.handler = h
}

func (m *Manager) emit(eventType string, c *Capsule) {
	m.handlerMu.RLock()
	h := m.handler
	m.handlerMu.RUnlock()
	h(ManagerEvent{
		Type:      eventType,
		CapsuleID: c.ID,
		Capsule:   c,
		Timestamp: time.Now(),
	})
}
