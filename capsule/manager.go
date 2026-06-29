package capsule

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	enums "github.com/tareksalem/falak/capsule/enums"
	"go.uber.org/zap"
)

// ErrNotFound is returned when a capsule lookup finds no match.
var ErrNotFound = errors.New("capsule not found")

// ErrCapsuleNameConflict is returned by Create when a capsule with the
// same name already exists in the same cluster. Falak enforces
// cluster-wide name uniqueness (locked decision #16 in
// service-networking.md): names must be unique within a cluster so
// Service backends, DNS lookups, and self-anti-affinity all agree on
// what "the capsule named X" means. Use errors.Is to detect it across
// wrapping layers — the API layer maps it to gRPC AlreadyExists.
var ErrCapsuleNameConflict = errors.New("capsule name already exists in cluster")

// ManagerEvent is emitted by the capsule manager on lifecycle actions.
//
// Meta carries optional contextual fields that some event types need but
// that don't fit on the Capsule snapshot itself. For example,
// EventCapsuleGroupReleased uses Meta["previous_group_id"] to expose the
// group ID a detached member used to belong to (the Capsule.Spec.GroupID
// has already been cleared at emit time). Meta is nil by default; only
// emitters that have side-band context populate it.
type ManagerEvent struct {
	Type      string
	CapsuleID CapsuleID
	Capsule   *Capsule
	Timestamp time.Time
	Meta      map[string]string
}

// Manager event types.
const (
	EventCapsuleCreated        = "capsule.created"
	EventCapsuleUpdated        = "capsule.updated"
	EventCapsuleDeleted        = "capsule.deleted"
	EventCapsuleReceived       = "capsule.received"
	EventCapsuleAnnounced      = "capsule.announced"
	EventCapsuleRunning        = "capsule.running"
	EventCapsuleStopping       = "capsule.stopping"
	EventCapsuleStopped        = "capsule.stopped"
	EventCapsuleStatusChanged  = "capsule.status_changed"
	EventCapsuleGroupReleased  = "capsule.group_released"
)

// MetaPreviousGroupID is the ManagerEvent.Meta key carrying the previous
// group ID for EventCapsuleGroupReleased. It is set on the side-band path
// because by the time the event fires the capsule's own Spec.GroupID has
// been cleared as part of the non-cascade group delete contract.
const MetaPreviousGroupID = "previous_group_id"

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
func (m *Manager) newLifecycle(id CapsuleID, initial enums.CapsuleStatus) *Lifecycle {
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
	// Hold the manager mutex across the store Update so the inner
	// UpdatedAt write does not race with concurrent Manager.Get
	// snapshot reads. The store has its own mutex; both together
	// serialise the field write against Get's read-side copy.
	updateErr := m.store.Update(c)
	m.mu.Unlock()

	if err := updateErr; err != nil {
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
func statusEventType(status enums.CapsuleStatus) string {
	switch status {
	case enums.CapsuleStatusEnum.Announced():
		return EventCapsuleAnnounced
	case enums.CapsuleStatusEnum.Running():
		return EventCapsuleRunning
	case enums.CapsuleStatusEnum.Stopping():
		return EventCapsuleStopping
	case enums.CapsuleStatusEnum.Stopped():
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

	// Group capsules must be admitted via CreateGroup (task 10.6) so the
	// member materialization, GroupID linking, and per-kind status
	// transitions are all handled in one place. Standalone Create rejects
	// group kind explicitly rather than silently mishandling it.
	if spec.Kind == CapsuleKindEnum.Group() {
		return nil, errors.New("group capsules must be created via CreateGroup (task 10.6)")
	}

	// Apply defaults
	DefaultSpec(&spec)

	// Validate
	if err := ValidateSpec(&spec); err != nil {
		return nil, fmt.Errorf("invalid capsule spec: %w", err)
	}

	// Enforce cluster-wide name uniqueness BEFORE allocating an ID or
	// touching the store. Without this guard, two capsules with the
	// same name + cluster can coexist; both end up reachable by the
	// election self-anti-affinity check (which matches by name, not
	// ID), causing the second create to silently deadlock at the
	// delay-strategy ineligibility branch.
	if existing := m.store.GetByNameInCluster(clusterID, spec.Name); existing != nil {
		return nil, fmt.Errorf("%w: %q in cluster %q (existing id=%s)",
			ErrCapsuleNameConflict, spec.Name, clusterID, existing.ID.String())
	}

	now := time.Now()
	c := &Capsule{
		ID:        NewCapsuleID(),
		ClusterID: clusterID,
		Spec:      spec,
		Status:    enums.CapsuleStatusEnum.Created(),
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
	m.lifecycles[c.ID] = m.newLifecycle(c.ID, enums.CapsuleStatusEnum.Created())
	m.lifecyclesMu.Unlock()

	m.logger.Info("capsule created",
		zap.String("id", c.ID.String()),
		zap.String("name", spec.Name),
		zap.String("cluster", clusterID),
		zap.String("orbit", spec.Orbit),
		zap.String("tier", string(spec.Tier)))

	m.metrics.IncCreated(clusterID)

	// Advance the lifecycle to Announced BEFORE emitting EventCapsuleCreated.
	// Subscribers of EventCapsuleCreated (the node-level CapsuleHandler)
	// synchronously announce the capsule onto the orbit AND publish initial
	// election requests. Both must observe the capsule already at status
	// Announced:
	//
	//   - The orbit announcement carries c.Status to peers; if it leaves at
	//     Created, every receiver installs its lifecycle at Created and the
	//     downstream StartElection (Announced → Electing) is rejected.
	//   - The originator's election handler invokes StartElection on the
	//     local lifecycle as soon as it dispatches the request; that
	//     transition is only valid from Announced.
	//
	// Running Announce first guarantees both invariants on every node.
	if err := m.Announce(c.ID); err != nil {
		m.logger.Warn("auto-announce after create failed",
			zap.String("id", c.ID.String()),
			zap.Error(err))
	}

	m.emit(EventCapsuleCreated, c)
	return c, nil
}

// Get retrieves a capsule by ID.
//
// The returned *Capsule is a snapshot: the struct itself is freshly
// allocated and the Replicas slice is copied. Callers may iterate and
// read fields without coordinating with the manager's mutex. Internal
// callers that need to MUTATE the live capsule (e.g. AssignReplica,
// SyncStatus) go through m.store.Get directly under m.mu, never through
// this method.
//
// The snapshot is taken under m.mu.RLock so concurrent writers
// (AssignReplica, SyncStatus, Update) that hold m.mu.Lock cannot
// interleave with the slice copy — closing a data race that would
// otherwise be observable when callers iterate c.Replicas while a
// replica is being assigned.
func (m *Manager) Get(id CapsuleID) *Capsule {
	c := m.store.Get(id)
	if c == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot := *c
	if len(c.Replicas) > 0 {
		snapshot.Replicas = make([]ReplicaState, len(c.Replicas))
		copy(snapshot.Replicas, c.Replicas)
	}
	return &snapshot
}

// GetByName retrieves a capsule by name.
func (m *Manager) GetByName(name string) *Capsule {
	return m.store.GetByName(name)
}

// ListLive returns all capsules with the LIVE pointer semantics: each
// returned *Capsule references the corresponding store entry directly.
//
// CAUTION: callers MUST NOT mutate the returned entries or read mutable
// fields (most importantly Replicas, which is rewritten by AssignReplica /
// SyncStatus) outside the manager mutex. The Spec is immutable post-create
// so reading c.Spec.* is always safe.
//
// In practice most callers want ListSnapshot, which copies the Replicas
// backing array under the manager's RLock and is safe for concurrent
// iteration. ListLive exists for hot paths (e.g. the per-node-failure scan
// in the capsule handler) that only consult immutable Spec fields.
func (m *Manager) ListLive() []*Capsule {
	return m.store.List()
}

// List is a deprecated alias for ListLive retained for one release so
// downstream call sites can migrate. New code MUST call ListLive or
// ListSnapshot explicitly so the live-pointer semantics are visible at
// the call site (audit minor #7).
//
// Deprecated: use ListLive when the caller only reads immutable Spec
// fields, or ListSnapshot when the caller iterates Replicas.
func (m *Manager) List() []*Capsule {
	return m.ListLive()
}

// ListSnapshot returns deep-copied capsule snapshots safe for concurrent
// readers. Each entry's Replicas slice is freshly allocated under
// m.mu.RLock so concurrent writers (AssignReplica, SyncStatus) that hold
// m.mu.Lock cannot interleave with the copy. Callers that iterate
// c.Replicas across all capsules (e.g. gravity self-anti-affinity lookups)
// MUST use this method instead of List() to stay race-free under -race.
func (m *Manager) ListSnapshot() []*Capsule {
	live := m.store.List()
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Capsule, 0, len(live))
	for _, c := range live {
		if c == nil {
			continue
		}
		snap := *c
		if len(c.Replicas) > 0 {
			snap.Replicas = make([]ReplicaState, len(c.Replicas))
			copy(snap.Replicas, c.Replicas)
		}
		out = append(out, &snap)
	}
	return out
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
//
// Delete is idempotent in the sense reapers depend on: when no capsule with
// the given id exists, ErrNotFound is returned (not a wrapped fmt error) so
// callers in cascade/teardown paths can match it via errors.Is and treat it
// as success.
//
// When the capsule's Kind is Group, Delete dispatches to deleteGroup which
// either cascades to every member (CascadeDelete=true) or detaches members
// from the group (CascadeDelete=false). Standalone capsules follow the
// existing path: store delete, lifecycle drop, EventCapsuleDeleted.
func (m *Manager) Delete(ctx context.Context, id CapsuleID) error {
	c := m.store.Get(id)
	if c == nil {
		return ErrNotFound
	}

	if c.Spec.Kind == CapsuleKindEnum.Group() {
		return m.deleteGroup(ctx, c)
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
			initial = enums.CapsuleStatusEnum.Announced()
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
//
// Fire enforces per-kind gating on triggers. The underlying FSM is uniform
// across kinds (so it stays small and easy to reason about), and this method
// is the seam that rejects triggers that are valid for one kind but not the
// other:
//
//   - Group capsules reject all election/execution/scaling triggers.
//     Group lifecycle is admission-driven, not election-driven.
//   - Non-group capsules reject TriggerMembersAdmitted. That trigger is
//     only meaningful for groups.
func (m *Manager) Fire(id CapsuleID, trigger string) error {
	lc := m.getLifecycle(id)
	if lc == nil {
		return fmt.Errorf("capsule %s not found", id)
	}
	c := m.store.Get(id)
	if c == nil {
		return fmt.Errorf("capsule %s not found", id)
	}
	if c.Spec.Kind == CapsuleKindEnum.Group() {
		switch trigger {
		case TriggerElectionStarted, TriggerElectionWon, TriggerElectionTimeout,
			TriggerExecutionStart, TriggerContainerReady, TriggerNodeFailed,
			TriggerScaleUpNeeded, TriggerScaleDownNeeded:
			return fmt.Errorf("trigger %q not valid for group capsules", trigger)
		}
	} else {
		if trigger == TriggerMembersAdmitted {
			return fmt.Errorf("trigger %q only valid for group capsules", trigger)
		}
	}
	if err := lc.Fire(trigger); err != nil {
		return fmt.Errorf("capsule %s: %w", id, err)
	}
	return nil
}

// Status returns the current lifecycle state for a capsule.
func (m *Manager) Status(id CapsuleID) (enums.CapsuleStatus, error) {
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

// MarkNodeFailed transitions a running capsule back to Announced after the
// node hosting it lost the container (crash/removal), so a fresh election can
// re-place it. Fires TriggerNodeFailed (Running → Announced). Returns the Fire
// error if the capsule is not in a state that permits it; callers in the crash
// path treat that as non-fatal (the capsule may already have been downgraded
// by a sibling replica's crash event).
func (m *Manager) MarkNodeFailed(id CapsuleID) error {
	return m.Fire(id, TriggerNodeFailed)
}

// MarkGroupRunning transitions a group capsule from Announced to Running by
// firing TriggerMembersAdmitted. This is the group-only analogue of
// MarkRunning — group lifecycle is admission-driven, not election-driven, so
// a group goes Created → Announced → Running once every member has been
// materialized and persisted. Calling this on a non-group capsule returns an
// error from Fire.
func (m *Manager) MarkGroupRunning(id CapsuleID) error {
	return m.Fire(id, TriggerMembersAdmitted)
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
			c.Replicas[i].Status = enums.CapsuleStatusEnum.Assigned()
			c.Replicas[i].StartedAt = time.Now()
			found = true
			break
		}
	}
	if !found {
		c.Replicas = append(c.Replicas, ReplicaState{
			ReplicaID: replicaID,
			NodeID:    nodeID,
			Status:    enums.CapsuleStatusEnum.Assigned(),
			StartedAt: time.Now(),
		})
	}
	c.UpdatedAt = time.Now()
	// Hold the manager mutex across the store Update so the store's
	// internal write of capsule.UpdatedAt (line 270 of store.go) does
	// not race with concurrent readers calling Manager.Get (which
	// snapshot-copies *c under m.mu.RLock). The store also serialises
	// the write on its own mutex; both locks together close the prior
	// race surfaced once same-node group placement was wired.
	err := m.store.Update(c)
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to persist replica assignment: %w", err)
	}

	m.logger.Info("capsule replica assigned",
		zap.String("id", id.String()),
		zap.String("replica_id", string(replicaID)),
		zap.String("node_id", nodeID))
	return nil
}

// UnassignReplica clears the node binding for a replica whose container is
// known to be gone (crash or out-of-band removal). It sets the replica's
// NodeID back to empty and its Status back to Announced — the capsule-level
// "looking for a home" state that AssignReplica overwrites when it claims the
// slot. Clearing the binding (rather than deleting the slot) keeps the
// ReplicaID stable across the subsequent re-election and re-assign.
//
// Clearing matters for correctness: NodesRunningCapsule
// (node/election_handler.go) skips replicas with an empty NodeID, so once the
// binding is cleared the originating node is no longer counted by the
// self-anti-affinity rule in gravity.IsEligible and becomes eligible to
// re-place the replica it just lost. Without this, a single-node cluster
// dead-locks on re-election ("no claim heard").
//
// UnassignReplica is idempotent: it returns nil with no side effects if the
// capsule is unknown, the replica is unknown, or the replica's NodeID is
// already empty.
func (m *Manager) UnassignReplica(id CapsuleID, replicaID ReplicaID) error {
	c := m.store.Get(id)
	if c == nil {
		// Unknown capsule — nothing to clear.
		return nil
	}

	m.mu.Lock()
	idx := -1
	for i, r := range c.Replicas {
		if r.ReplicaID == replicaID {
			idx = i
			break
		}
	}
	if idx == -1 || c.Replicas[idx].NodeID == "" {
		// Unknown replica or already unbound — idempotent no-op.
		m.mu.Unlock()
		return nil
	}

	c.Replicas[idx].NodeID = ""
	c.Replicas[idx].Status = enums.CapsuleStatusEnum.Announced()
	c.UpdatedAt = time.Now()
	// Hold the manager mutex across the store Update so the store's internal
	// write of capsule.UpdatedAt does not race with concurrent readers
	// calling Manager.Get (which snapshot-copies *c under m.mu.RLock). This
	// mirrors AssignReplica's exact locking discipline; both locks together
	// close the same race the AssignReplica comment describes.
	err := m.store.Update(c)
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to persist replica unassignment: %w", err)
	}

	m.logger.Info("capsule replica unassigned",
		zap.String("id", id.String()),
		zap.String("replica_id", string(replicaID)))
	return nil
}

// SyncStatus updates a capsule's status to mirror remote state received from
// the mesh. Unlike Fire, this does NOT validate the transition — it is used
// only when mirroring state from another node's PubSub announcement.
// The local lifecycle machine is rebuilt at the new state so future local
// transitions proceed correctly from there.
func (m *Manager) SyncStatus(id CapsuleID, status enums.CapsuleStatus) error {
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
	// Hold the manager mutex across the store Update so the store's
	// internal UpdatedAt write does not race with concurrent
	// Manager.Get snapshot reads.
	err := m.store.Update(c)
	m.mu.Unlock()
	if err != nil {
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

// emitWithMeta emits a manager event with side-band metadata. Used by
// flows that need to expose context that isn't on the capsule snapshot
// itself — e.g. the previous group ID for EventCapsuleGroupReleased.
func (m *Manager) emitWithMeta(eventType string, c *Capsule, meta map[string]string) {
	m.handlerMu.RLock()
	h := m.handler
	m.handlerMu.RUnlock()
	h(ManagerEvent{
		Type:      eventType,
		CapsuleID: c.ID,
		Capsule:   c,
		Timestamp: time.Now(),
		Meta:      meta,
	})
}
