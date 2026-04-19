package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/orbit"
	capsulePb "github.com/tareksalem/falak/capsule/proto/capsulepb"
	"github.com/tareksalem/falak/capsule/scaling"
	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
)

// Priority levels for election requests, applied to events.ElectionRequested.
// Higher values are processed first by the election manager.
const (
	priorityInitial     = 50  // first-time deploy
	priorityScaleUp     = 60  // scale-up driven by user-defined rules
	priorityNodeFailure = 100 // recovery from a failed node — most urgent
)

// electionForgetter is the narrow hook CapsuleHandler uses to tell the
// election subsystem that a capsule is gone so the Manager's local
// state (claim guard, in-flight rounds) can be cleaned up. It is
// satisfied by *election.Manager.
type electionForgetter interface {
	ForgetCapsule(id capsule.CapsuleID)
}

// CapsuleHandler bridges the capsule module with the node's event bus and PubSub.
// It owns the capsule.Manager, orbit.Manager (per cluster), and scaling.Monitor,
// and coordinates capsule announcements, reception, and scaling decisions.
type CapsuleHandler struct {
	manager          *capsule.Manager
	mu               sync.RWMutex
	orbits           map[string]*orbit.Manager      // clusterPath -> orbit manager
	announcers       map[string]*orbit.Announcer    // clusterPath -> announcer
	dedups           map[string]*orbit.Deduplicator // clusterPath -> dedup
	scalingMonitor   *scaling.Monitor
	metricsRegistry  scaling.MetricsRegistry
	eventBus         events.Bus
	logger           *zap.Logger
	nodeID           string
	ps               *pubsub.PubSub
	privateKey       crypto.PrivKey
	phonebook        phonebook.IPhonebook
	electionForget   electionForgetter

	// ctx is the long-lived handler context (set in Start, canceled in Stop).
	// It governs orbit message loops, scaling monitor, and dedup cleanup loop.
	ctx    context.Context
	cancel context.CancelFunc

	// wg tracks the handler's own goroutines so Stop can wait for clean exit.
	wg sync.WaitGroup
}

// CapsuleHandlerOption configures a CapsuleHandler.
type CapsuleHandlerOption func(*CapsuleHandler)

// WithCapsuleHandlerLogger sets the logger.
func WithCapsuleHandlerLogger(logger *zap.Logger) CapsuleHandlerOption {
	return func(h *CapsuleHandler) {
		h.logger = logger
	}
}

// WithCapsuleHandlerEventBus sets the event bus.
func WithCapsuleHandlerEventBus(bus events.Bus) CapsuleHandlerOption {
	return func(h *CapsuleHandler) {
		h.eventBus = bus
	}
}

// WithCapsuleHandlerNodeID sets the node ID.
func WithCapsuleHandlerNodeID(id string) CapsuleHandlerOption {
	return func(h *CapsuleHandler) {
		h.nodeID = id
	}
}

// WithCapsuleHandlerPubSub sets the libp2p PubSub instance.
func WithCapsuleHandlerPubSub(ps *pubsub.PubSub) CapsuleHandlerOption {
	return func(h *CapsuleHandler) {
		h.ps = ps
	}
}

// WithCapsuleHandlerPrivateKey sets the local node's private key for signing.
func WithCapsuleHandlerPrivateKey(key crypto.PrivKey) CapsuleHandlerOption {
	return func(h *CapsuleHandler) {
		h.privateKey = key
	}
}

// WithCapsuleHandlerPhonebook sets the phonebook for signature verification.
func WithCapsuleHandlerPhonebook(pb phonebook.IPhonebook) CapsuleHandlerOption {
	return func(h *CapsuleHandler) {
		h.phonebook = pb
	}
}

// WithCapsuleHandlerMetricsRegistry sets the metrics registry used by the
// scaling monitor. The runtime module populates this registry as it starts
// containers and drains it as they stop. If not set, a default empty registry
// is used (scaling rules will evaluate but find no metrics).
func WithCapsuleHandlerMetricsRegistry(r scaling.MetricsRegistry) CapsuleHandlerOption {
	return func(h *CapsuleHandler) {
		h.metricsRegistry = r
	}
}

// WithCapsuleHandlerElectionForgetter wires an optional hook that the
// handler calls whenever a capsule is deleted or withdrawn. The election
// Manager uses this to free per-capsule state (claim guard, in-flight
// rounds). If unset the delete path does nothing election-side, which is
// fine for tests and ad-hoc callers.
func WithCapsuleHandlerElectionForgetter(f electionForgetter) CapsuleHandlerOption {
	return func(h *CapsuleHandler) {
		h.electionForget = f
	}
}

// SetElectionForgetter installs or replaces the election forget hook
// after construction. Used by the node wiring where the election
// Manager is built after the CapsuleHandler but needs a back-reference
// for cleanup on capsule deletion.
func (h *CapsuleHandler) SetElectionForgetter(f electionForgetter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.electionForget = f
}

// NewCapsuleHandler creates a new capsule handler with an owned capsule manager.
// The manager is created internally and its event handler is wired to the handler's
// onManagerEvent method, which bridges capsule events to the node event bus AND
// triggers orbit announcements on CapsuleCreated.
func NewCapsuleHandler(manager *capsule.Manager, opts ...CapsuleHandlerOption) *CapsuleHandler {
	h := &CapsuleHandler{
		manager:    manager,
		orbits:     make(map[string]*orbit.Manager),
		announcers: make(map[string]*orbit.Announcer),
		dedups:     make(map[string]*orbit.Deduplicator),
		logger:     zap.NewNop(),
	}
	for _, opt := range opts {
		opt(h)
	}

	// If no manager was passed, create one with the event handler wired.
	if h.manager == nil {
		h.manager = capsule.NewManager(
			capsule.WithManagerLogger(h.logger),
			capsule.WithManagerEventHandler(h.onManagerEvent),
		)
	} else {
		// Wire the event handler onto the existing manager.
		h.manager.SetEventHandler(h.onManagerEvent)
	}

	// Default to an in-memory registry if none was provided.
	if h.metricsRegistry == nil {
		h.metricsRegistry = scaling.NewInMemoryRegistry()
	}

	// Create scaling monitor wired to the metrics registry and event bridge.
	h.scalingMonitor = scaling.NewMonitor(
		scaling.WithMonitorLogger(h.logger.Named("scaling")),
		scaling.WithMonitorHandler(h.onScaleEvent),
		scaling.WithMonitorRegistry(h.metricsRegistry),
	)

	return h
}

// MetricsRegistry returns the registry used by the scaling monitor.
// The runtime module should register a MetricsProvider here for each capsule
// it starts, and Unregister when execution stops.
func (h *CapsuleHandler) MetricsRegistry() scaling.MetricsRegistry {
	return h.metricsRegistry
}

// Start begins listening for node events and starts the scaling monitor.
// It derives a long-lived handler context from the passed ctx; orbit message
// loops, scaling monitor, and the dedup cleanup loop all run on that context.
func (h *CapsuleHandler) Start(ctx context.Context) {
	h.ctx, h.cancel = context.WithCancel(ctx)

	h.wg.Add(7)
	go func() {
		defer h.wg.Done()
		h.handleClusterJoined(h.ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.handleNodeFailed(h.ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.dedupCleanupLoop(h.ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.handleElectionWon(h.ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.handleElectionLost(h.ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.handleElectionFailed(h.ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.handleContainerCrash(h.ctx)
	}()

	h.scalingMonitor.Start(h.ctx)

	h.logger.Info("capsule handler started")
}

// Stop halts the handler and its components. Cancels the long-lived context
// so all goroutines exit, waits for them to finish, then leaves all orbit
// subscriptions.
func (h *CapsuleHandler) Stop() {
	if h.cancel != nil {
		h.cancel()
	}
	h.scalingMonitor.Stop()

	// Wait for handler-owned goroutines to exit after context cancel.
	h.wg.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, om := range h.orbits {
		om.LeaveAll()
	}
	h.logger.Info("capsule handler stopped")
}

// Manager returns the underlying capsule manager.
func (h *CapsuleHandler) Manager() *capsule.Manager {
	return h.manager
}

// SetupCluster initializes orbit management for a cluster.
// Creates an orbit.Manager wired to this handler's message handler,
// along with an announcer and deduplicator.
func (h *CapsuleHandler) SetupCluster(_ context.Context, clusterPath string) *orbit.Manager {
	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.orbits[clusterPath]; ok {
		return existing
	}

	dedup := orbit.NewDeduplicator(
		orbit.WithDeduplicatorLogger(h.logger.Named("dedup")),
	)
	h.dedups[clusterPath] = dedup

	// Use the handler's long-lived context so orbit message loops survive
	// across test helper calls and only stop when the handler stops.
	orbitMgr := orbit.NewManager(
		orbit.WithContext(h.ctx),
		orbit.WithPubSub(h.ps),
		orbit.WithClusterPath(clusterPath),
		orbit.WithNodeID(h.nodeID),
		orbit.WithLogger(h.logger.Named("orbit")),
		orbit.WithHandler(func(orbitName string, data []byte, senderID string) {
			h.onOrbitMessage(h.ctx, clusterPath, orbitName, data, senderID)
		}),
	)
	h.orbits[clusterPath] = orbitMgr

	annOpts := []orbit.AnnouncerOption{
		orbit.WithAnnouncerLogger(h.logger.Named("announcer")),
		orbit.WithAnnouncerNodeID(h.nodeID),
		orbit.WithAnnouncerDedup(dedup),
	}
	if h.privateKey != nil {
		annOpts = append(annOpts, orbit.WithAnnouncerSigner(newCapsuleSigner(h.privateKey)))
	}
	if h.phonebook != nil {
		annOpts = append(annOpts, orbit.WithAnnouncerVerifier(newCapsuleVerifier(h.phonebook, clusterPath)))
	}
	announcer := orbit.NewAnnouncer(orbitMgr, annOpts...)
	h.announcers[clusterPath] = announcer

	h.logger.Info("cluster capsule orbit setup",
		zap.String("cluster", clusterPath))

	return orbitMgr
}

// LeaveCluster tears down all orbit, announcer, and dedup state for a
// cluster. It leaves every orbit the handler had joined for the cluster
// (closing per-orbit topics and message-loop goroutines) and removes the
// per-cluster entries from the handler's maps. Idempotent.
func (h *CapsuleHandler) LeaveCluster(clusterPath string) {
	h.mu.Lock()
	orbitMgr := h.orbits[clusterPath]
	delete(h.orbits, clusterPath)
	delete(h.announcers, clusterPath)
	delete(h.dedups, clusterPath)
	h.mu.Unlock()

	if orbitMgr != nil {
		orbitMgr.LeaveAll()
		h.logger.Info("capsule handler left cluster",
			zap.String("cluster", clusterPath))
	}
}

// JoinOrbit subscribes the node to a specific orbit in the cluster.
func (h *CapsuleHandler) JoinOrbit(ctx context.Context, clusterPath, orbitName string) error {
	h.mu.RLock()
	orbitMgr, ok := h.orbits[clusterPath]
	h.mu.RUnlock()
	if !ok {
		orbitMgr = h.SetupCluster(ctx, clusterPath)
	}
	return orbitMgr.Join(ctx, orbitName)
}

// handleClusterJoined subscribes to ClusterJoined events.
func (h *CapsuleHandler) handleClusterJoined(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeClusterJoined)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			joined, ok := event.(events.ClusterJoined)
			if !ok {
				continue
			}
			h.SetupCluster(ctx, joined.ClusterPath)
			h.logger.Info("cluster joined, capsule handler ready",
				zap.String("cluster", joined.ClusterPath))
		}
	}
}

// handleNodeFailed subscribes to NodeFailed events and triggers re-election.
func (h *CapsuleHandler) handleNodeFailed(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeNodeFailed)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			failed, ok := event.(events.NodeFailed)
			if !ok {
				continue
			}
			h.onNodeFailed(ctx, failed)
		}
	}
}

// handleElectionWon subscribes to local ElectionWon events and records
// the replica assignment on the capsule store. Without this the capsule
// manager's Replicas slice would stay empty even on the winning node,
// breaking the node-failure detection loop that iterates replicas.
//
// The lifecycle state has already been advanced to Assigned by the
// election manager via WinElection — this subscription only fills in
// the Replicas metadata so downstream code (runtime, re-election, node
// failure recovery) can see WHICH node is assigned to WHICH replica.
func (h *CapsuleHandler) handleElectionWon(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeElectionWon)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			won, ok := event.(events.ElectionWon)
			if !ok {
				continue
			}
			id := capsule.CapsuleID(won.CapsuleID)
			replica := capsule.ReplicaID(won.ReplicaID)
			if err := h.manager.AssignReplica(id, replica, won.NodeID); err != nil {
				h.logger.Warn("assign replica after election won failed",
					zap.String("capsule_id", won.CapsuleID),
					zap.String("replica_id", won.ReplicaID),
					zap.String("node_id", won.NodeID),
					zap.Error(err))
			}
		}
	}
}

// handleElectionLost subscribes to ElectionLost events so losing nodes
// can mirror the winner's Assigned state locally AND record the replica
// assignment in their capsule store.
//
// Without the SyncStatus call, losers would stay in the Electing state
// forever because the election lifecycle only advances on the winning
// node via StartElection/WinElection.
//
// Without the AssignReplica call, losers would have no record of which
// node took the replica — breaking node-failure detection (which
// iterates c.Replicas to find orphans when a peer dies).
func (h *CapsuleHandler) handleElectionLost(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeElectionLost)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			lost, ok := event.(events.ElectionLost)
			if !ok {
				continue
			}
			id := capsule.CapsuleID(lost.CapsuleID)
			if err := h.manager.SyncStatus(id, capsule.CapsuleStatusEnum.Assigned()); err != nil {
				h.logger.Debug("sync status after election lost failed",
					zap.String("capsule_id", lost.CapsuleID),
					zap.String("winner", lost.WinnerNodeID),
					zap.Error(err))
			}
			// Record the remote winner's replica assignment so this
			// node can detect future orphaning if the winner fails.
			if lost.WinnerNodeID != "" {
				if err := h.manager.AssignReplica(id, capsule.ReplicaID(lost.ReplicaID), lost.WinnerNodeID); err != nil {
					h.logger.Debug("assign replica after election lost failed",
						zap.String("capsule_id", lost.CapsuleID),
						zap.String("replica_id", lost.ReplicaID),
						zap.String("winner", lost.WinnerNodeID),
						zap.Error(err))
				}
			}
		}
	}
}

// handleElectionFailed subscribes to ElectionFailed events and keeps the
// local lifecycle in sync. The election manager has already called
// ElectionTimeout on the winning-round lifecycle, but nodes that were
// mere observers (ineligible, lost earlier, or never entered Electing)
// also need a uniform "failed" view so `falak capsule status` reports
// the same thing on every node.
//
// Because the capsule lifecycle does not have an explicit "failed" state
// in v1 we downgrade it to Announced — a "fresh start" that a future
// re-election or manual intervention can act on. A dedicated Failed
// state is tracked in task backlog.
func (h *CapsuleHandler) handleElectionFailed(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeElectionFailed)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			failed, ok := event.(events.ElectionFailed)
			if !ok {
				continue
			}
			id := capsule.CapsuleID(failed.CapsuleID)
			// Sync back to Announced so the capsule is re-eligible for
			// a future election round (manual reschedule, failure
			// recovery, etc.). The explicit Failed status is future work.
			if err := h.manager.SyncStatus(id, capsule.CapsuleStatusEnum.Announced()); err != nil {
				h.logger.Debug("sync status after election failed",
					zap.String("capsule_id", failed.CapsuleID),
					zap.Error(err))
			}
			h.logger.Warn("capsule election failed",
				zap.String("capsule_id", failed.CapsuleID),
				zap.String("replica_id", failed.ReplicaID),
				zap.String("reason", failed.Reason))
		}
	}
}

// handleContainerCrash subscribes to CapsuleExecutionFailed events
// (emitted by the runtime handler when a container exits unexpectedly)
// and fires a re-election for the crashed capsule's replicas on the
// local node. This is distinct from onNodeFailed (which handles SWIM
// detected failures of remote nodes) — this handles local container
// crashes that don't involve a node failure.
func (h *CapsuleHandler) handleContainerCrash(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeCapsuleFailed)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			failed, ok := event.(events.CapsuleExecutionFailed)
			if !ok {
				continue
			}
			id := capsule.CapsuleID(failed.CapsuleID)
			c := h.manager.Get(id)
			if c == nil {
				continue
			}

			h.logger.Warn("container crash detected, requesting re-election",
				zap.String("capsule_id", failed.CapsuleID),
				zap.String("reason", failed.Reason))

			// Fire a re-election for each replica that was running on
			// this node. The local node may or may not win again —
			// gravity decides.
			for _, replica := range c.Replicas {
				if replica.NodeID != h.nodeID {
					continue
				}
				h.requestElection(events.ElectionRequested{
					BaseEvent:   events.NewBaseEvent(),
					CapsuleID:   failed.CapsuleID,
					ReplicaID:   string(replica.ReplicaID),
					Reason:      events.ElectionReasonEnum.NodeFailure(),
					ClusterPath: c.ClusterID,
					Priority:    priorityNodeFailure,
				})
			}
		}
	}
}

// dedupCleanupLoop periodically cleans expired entries from all deduplicators.
func (h *CapsuleHandler) dedupCleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.mu.RLock()
			dedups := make([]*orbit.Deduplicator, 0, len(h.dedups))
			for _, d := range h.dedups {
				dedups = append(dedups, d)
			}
			h.mu.RUnlock()
			for _, d := range dedups {
				d.Cleanup()
			}
		}
	}
}

// onNodeFailed handles a node failure by finding orphaned capsules and
// firing one ElectionRequested for each orphaned replica. As the originator
// of the failure detection, this node is responsible for triggering the
// re-election; remote nodes do not duplicate the request.
func (h *CapsuleHandler) onNodeFailed(_ context.Context, failed events.NodeFailed) {
	capsules := h.manager.List()
	for _, c := range capsules {
		for _, replica := range c.Replicas {
			if replica.NodeID != failed.NodeID {
				continue
			}
			h.logger.Warn("capsule replica orphaned due to node failure",
				zap.String("capsule_id", c.ID.String()),
				zap.String("name", c.Spec.Name),
				zap.String("cluster", c.ClusterID),
				zap.String("failed_node", failed.NodeID),
				zap.String("replica_id", string(replica.ReplicaID)))

			h.requestElection(events.ElectionRequested{
				BaseEvent:      events.NewBaseEvent(),
				CapsuleID:      c.ID.String(),
				ReplicaID:      string(replica.ReplicaID),
				Reason:         events.ElectionReasonEnum.NodeFailure(),
				ClusterPath:    failed.ClusterPath,
				PreviousNodeID: failed.NodeID,
				Priority:       priorityNodeFailure,
			})
		}
	}
}

// requestElection publishes an ElectionRequested event onto the local node
// event bus. The election manager subscribes to this event and dedupes by
// (capsule_id, replica_id), so multiple decisions to request an election
// for the same replica collapse into a single round.
//
// requestElection is the single chokepoint for "fire an election" — every
// originator (capsule create, node failure detection, scale-up trigger,
// future API endpoints) goes through it.
func (h *CapsuleHandler) requestElection(ev events.ElectionRequested) {
	h.eventBus.Publish(ev)
	h.logger.Info("election requested",
		zap.String("capsule_id", ev.CapsuleID),
		zap.String("replica_id", ev.ReplicaID),
		zap.String("cluster", ev.ClusterPath),
		zap.String("reason", string(ev.Reason)))
}

// requestInitialElections fires one ElectionRequested per declared replica
// slot when a capsule is first created. The number of requests equals the
// effective minimum replica count (Exact when set, otherwise Min, with at
// least one).
//
// All requests are fired in parallel. Every node runs its strategy
// locally and scores only itself; the implicit self-anti-affinity rule
// in gravity.IsEligible prevents a single node from winning more than
// one replica of the same capsule. The manager dedupes by
// (CapsuleID, ReplicaID) so repeat events collapse to a single round.
func (h *CapsuleHandler) requestInitialElections(c *capsule.Capsule) {
	count := initialReplicaCount(c.Spec.Replicas)
	if count <= 0 {
		return
	}
	for i := int32(0); i < count; i++ {
		h.requestElection(events.ElectionRequested{
			BaseEvent:   events.NewBaseEvent(),
			CapsuleID:   c.ID.String(),
			ReplicaID:   replicaSlotID(i),
			Reason:      events.ElectionReasonEnum.Initial(),
			ClusterPath: c.ClusterID,
			Priority:    priorityInitial,
		})
	}
}

// initialReplicaCount returns the number of replicas to launch for a new
// capsule. Exact takes precedence over Min/Max; if neither is set, the
// default of 1 replica is used.
func initialReplicaCount(r capsule.ReplicaConfig) int32 {
	if r.Exact > 0 {
		return r.Exact
	}
	if r.Min > 0 {
		return r.Min
	}
	return 1
}

// replicaSlotID formats a replica slot index as a stable string identifier.
// Election requests use these as the ReplicaID; the runtime uses the same
// string when reporting which slot a container is filling.
func replicaSlotID(i int32) string {
	return fmt.Sprintf("%d", i)
}

// nextReplicaSlot computes the next replica slot identifier for a
// scale-up election. The slot index is one past the highest currently
// used slot, ensuring stable IDs even after individual replicas have
// been removed.
//
// Anti-affinity to nodes already running a replica is now enforced by
// gravity.IsEligible (each node checks itself), so this helper no
// longer returns an exclude list.
func nextReplicaSlot(c *capsule.Capsule) string {
	highest := -1
	for _, r := range c.Replicas {
		if idx := parseReplicaSlot(string(r.ReplicaID)); idx > highest {
			highest = idx
		}
	}
	return replicaSlotID(int32(highest + 1))
}

// parseReplicaSlot parses a replica slot identifier back into an integer
// index. Unparseable IDs return -1 so they don't affect the highest-slot
// calculation in nextReplicaSlot.
func parseReplicaSlot(id string) int {
	var n int
	if _, err := fmt.Sscanf(id, "%d", &n); err != nil {
		return -1
	}
	return n
}

// onManagerEvent bridges capsule.Manager events to the node event bus
// AND triggers orbit announcements for locally-created capsules.
func (h *CapsuleHandler) onManagerEvent(event capsule.ManagerEvent) {
	switch event.Type {
	case capsule.EventCapsuleCreated:
		h.eventBus.Publish(events.CapsuleCreated{
			BaseEvent:   events.NewBaseEvent(),
			CapsuleID:   event.CapsuleID.String(),
			CapsuleName: event.Capsule.Spec.Name,
			Orbit:       event.Capsule.Spec.Orbit,
			ClusterPath: event.Capsule.ClusterID,
		})
		// Announce to the orbit so other nodes discover this capsule.
		h.announceCapsule(event.Capsule)
		// As the originator, request an initial election for every replica.
		// Other nodes that receive this capsule via gossip do NOT duplicate
		// this request (originator-only trigger).
		h.requestInitialElections(event.Capsule)

	case capsule.EventCapsuleUpdated:
		h.eventBus.Publish(events.CapsuleUpdated{
			BaseEvent:   events.NewBaseEvent(),
			CapsuleID:   event.CapsuleID.String(),
			CapsuleName: event.Capsule.Spec.Name,
			ClusterPath: event.Capsule.ClusterID,
		})
		// Re-announce updated capsule.
		h.announceCapsule(event.Capsule)

	case capsule.EventCapsuleReceived:
		h.eventBus.Publish(events.CapsuleReceived{
			BaseEvent:   events.NewBaseEvent(),
			CapsuleID:   event.CapsuleID.String(),
			CapsuleName: event.Capsule.Spec.Name,
			Orbit:       event.Capsule.Spec.Orbit,
			ClusterPath: event.Capsule.ClusterID,
		})
		// Register received capsule with scaling monitor if it has rules.
		h.registerScaling(event.Capsule)
		// Each node also runs its own local strategy when it observes a
		// fresh capsule, contributing its score to the distributed election.
		// This is local-only — no extra wire traffic — but ensures the
		// election is decentralized rather than driven by the originator alone.
		h.requestInitialElections(event.Capsule)

	case capsule.EventCapsuleDeleted:
		h.eventBus.Publish(events.CapsuleWithdrawn{
			BaseEvent:   events.NewBaseEvent(),
			CapsuleID:   event.CapsuleID.String(),
			ClusterPath: event.Capsule.ClusterID,
			Reason:      "deleted",
		})
		// Withdraw from orbit so other nodes remove it.
		h.withdrawCapsule(event.Capsule, "deleted")
		// Unregister from scaling monitor.
		h.scalingMonitor.Unregister(event.CapsuleID)
		// Drop per-capsule state held by the election manager so the
		// local claim map does not grow without bound. The forgetter
		// is installed after CapsuleHandler construction, so read it
		// under the handler mutex to stay race-detector-clean.
		h.mu.RLock()
		forget := h.electionForget
		h.mu.RUnlock()
		if forget != nil {
			forget.ForgetCapsule(event.CapsuleID)
		}
	}
}

// announceCapsule publishes a capsule announcement to its orbit.
func (h *CapsuleHandler) announceCapsule(c *capsule.Capsule) {
	h.mu.RLock()
	ann, ok := h.announcers[c.ClusterID]
	h.mu.RUnlock()
	if !ok {
		h.logger.Debug("no announcer for cluster, skipping announcement",
			zap.String("cluster", c.ClusterID),
			zap.String("capsule_id", c.ID.String()))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := ann.Announce(ctx, c); err != nil {
		h.logger.Error("failed to announce capsule",
			zap.String("capsule_id", c.ID.String()),
			zap.Error(err))
		return
	}

	h.eventBus.Publish(events.CapsuleAnnounced{
		BaseEvent:   events.NewBaseEvent(),
		CapsuleID:   c.ID.String(),
		CapsuleName: c.Spec.Name,
		Orbit:       c.Spec.Orbit,
		ClusterPath: c.ClusterID,
	})
}

// withdrawCapsule publishes a withdrawal to the orbit.
func (h *CapsuleHandler) withdrawCapsule(c *capsule.Capsule, reason string) {
	h.mu.RLock()
	ann, ok := h.announcers[c.ClusterID]
	h.mu.RUnlock()
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := ann.Withdraw(ctx, c.Spec.Orbit, c.ID, reason); err != nil {
		h.logger.Error("failed to withdraw capsule",
			zap.String("capsule_id", c.ID.String()),
			zap.Error(err))
	}
}

// registerScaling adds a capsule to the scaling monitor if it has scaling rules.
// The MetricsProvider is resolved on each evaluation tick via the MetricsRegistry,
// so this method does not need a provider at registration time. The runtime
// module is responsible for populating the registry when it starts the capsule.
func (h *CapsuleHandler) registerScaling(c *capsule.Capsule) {
	if len(c.Spec.ScalingRules) == 0 {
		return
	}

	rules, err := scaling.FromSpecList(c.Spec.ScalingRules)
	if err != nil {
		h.logger.Error("failed to parse scaling rules",
			zap.String("capsule_id", c.ID.String()),
			zap.Error(err))
		return
	}

	h.scalingMonitor.Register(&scaling.CapsuleMetrics{
		CapsuleID: c.ID,
		Rules:     rules,
	})
}

// onOrbitMessage handles incoming orbit PubSub messages.
func (h *CapsuleHandler) onOrbitMessage(ctx context.Context, clusterPath, orbitName string, data []byte, senderID string) {
	h.mu.RLock()
	ann, ok := h.announcers[clusterPath]
	h.mu.RUnlock()
	if !ok {
		return
	}

	msgType, payload, err := ann.HandleMessage(data)
	if err != nil {
		h.logger.Debug("orbit message rejected",
			zap.String("orbit", orbitName),
			zap.String("sender", senderID),
			zap.Error(err))
		return
	}

	switch msgType {
	case "announcement":
		announcement, ok := payload.(*capsulePb.CapsuleAnnouncement)
		if !ok {
			return
		}
		c := protoToCapsule(announcement.Capsule)
		if c == nil {
			return
		}
		if err := h.manager.Receive(c); err != nil {
			h.logger.Error("failed to receive capsule",
				zap.Error(err))
		}

	case "status":
		update, ok := payload.(*capsulePb.CapsuleStatusUpdate)
		if !ok {
			return
		}
		id := capsule.CapsuleID(update.CapsuleId)
		if err := h.manager.SyncStatus(id, capsule.CapsuleStatus(update.Status)); err != nil {
			h.logger.Debug("failed to sync capsule status from mesh",
				zap.String("capsule_id", update.CapsuleId),
				zap.Error(err))
		}

	case "withdrawal":
		withdrawal, ok := payload.(*capsulePb.CapsuleWithdrawal)
		if !ok {
			return
		}
		id := capsule.CapsuleID(withdrawal.CapsuleId)
		if err := h.manager.Delete(ctx, id); err != nil {
			h.logger.Debug("failed to delete withdrawn capsule",
				zap.String("capsule_id", withdrawal.CapsuleId),
				zap.Error(err))
		}
	}
}

// onScaleEvent bridges scaling events to the node event bus and, for
// scale-up actions, fires the originator-side ElectionRequested so a new
// replica is placed somewhere in the mesh.
func (h *CapsuleHandler) onScaleEvent(event scaling.ScaleEvent) {
	base := events.NewBaseEvent()

	// Look up the cluster path from the capsule
	c := h.manager.Get(event.CapsuleID)
	clusterPath := ""
	if c != nil {
		clusterPath = c.ClusterID
	}

	switch event.Action {
	case capsule.ScalingActionEnum.ScaleUp():
		h.eventBus.Publish(events.ScaleUpNeeded{
			BaseEvent:   base,
			CapsuleID:   string(event.CapsuleID),
			RuleName:    event.RuleName,
			ClusterPath: clusterPath,
		})
		// Fire an election for the new replica. The replica ID is the
		// next available slot index — i.e. one past the highest already
		// running. Anti-affinity to nodes already running a replica is
		// handled by gravity.IsEligible (self-check on each node).
		if c != nil {
			nextSlot := nextReplicaSlot(c)
			h.requestElection(events.ElectionRequested{
				BaseEvent:   events.NewBaseEvent(),
				CapsuleID:   string(event.CapsuleID),
				ReplicaID:   nextSlot,
				Reason:      events.ElectionReasonEnum.ScaleUp(),
				ClusterPath: clusterPath,
				Priority:    priorityScaleUp,
			})
		}
	case capsule.ScalingActionEnum.ScaleDown():
		h.eventBus.Publish(events.ScaleDownNeeded{
			BaseEvent:   base,
			CapsuleID:   string(event.CapsuleID),
			RuleName:    event.RuleName,
			ClusterPath: clusterPath,
		})
	case capsule.ScalingActionEnum.ScaleToZero():
		h.eventBus.Publish(events.ScaleToZero{
			BaseEvent:   base,
			CapsuleID:   string(event.CapsuleID),
			RuleName:    event.RuleName,
			ClusterPath: clusterPath,
		})
	}
}

// --- Proto to domain conversion ---

func protoToCapsule(pb *capsulePb.Capsule) *capsule.Capsule {
	if pb == nil {
		return nil
	}

	c := &capsule.Capsule{
		ID:        capsule.CapsuleID(pb.Id),
		ClusterID: pb.ClusterId,
		Status:    capsule.CapsuleStatus(pb.Status),
		Version:   pb.Version,
	}

	if pb.CreatedAt != nil {
		c.CreatedAt = pb.CreatedAt.AsTime()
	}
	if pb.UpdatedAt != nil {
		c.UpdatedAt = pb.UpdatedAt.AsTime()
	}
	if pb.Momentum != nil {
		c.Momentum = capsule.MomentumState{
			Current: pb.Momentum.Current,
			Base:    pb.Momentum.Base,
		}
		if pb.Momentum.LastAdjusted != nil {
			c.Momentum.LastAdjusted = pb.Momentum.LastAdjusted.AsTime()
		}
	}

	if pb.Spec != nil {
		c.Spec = protoToSpec(pb.Spec)
	}

	for _, r := range pb.Replicas {
		rs := capsule.ReplicaState{
			ReplicaID: capsule.ReplicaID(r.ReplicaId),
			NodeID:    r.NodeId,
			Status:    capsule.CapsuleStatus(r.Status),
		}
		if r.StartedAt != nil {
			rs.StartedAt = r.StartedAt.AsTime()
		}
		c.Replicas = append(c.Replicas, rs)
	}

	return c
}

func protoToSpec(pb *capsulePb.CapsuleSpec) capsule.CapsuleSpec {
	spec := capsule.CapsuleSpec{
		Name:        pb.Name,
		Image:       pb.Image,
		ImageDigest: pb.ImageDigest,
		Orbit:       pb.Orbit,
		Tier:        capsule.Tier(pb.Tier),
		Labels:      pb.Labels,
	}

	if pb.Resources != nil {
		spec.Resources = capsule.ResourceRequirements{
			CPUCores: pb.Resources.CpuCores,
			MemoryMB: pb.Resources.MemoryMb,
			DiskMB:   pb.Resources.DiskMb,
		}
	}

	if pb.Replicas != nil {
		spec.Replicas = capsule.ReplicaConfig{
			Min:   pb.Replicas.Min,
			Max:   pb.Replicas.Max,
			Exact: pb.Replicas.Exact,
		}
	}

	if pb.Runtime != nil {
		spec.Runtime.Env = pb.Runtime.Env
	}

	if pb.MomentumConfig != nil {
		spec.MomentumConfig = capsule.MomentumConfig{
			Base:           pb.MomentumConfig.Base,
			BoostOnTraffic: pb.MomentumConfig.BoostOnTraffic,
			ReduceOnIdle:   pb.MomentumConfig.ReduceOnIdle,
			IdleTimeout:    time.Duration(pb.MomentumConfig.IdleTimeoutSeconds) * time.Second,
		}
	}

	for _, rule := range pb.ScalingRules {
		spec.ScalingRules = append(spec.ScalingRules, capsule.ScalingRule{
			Name:       rule.Name,
			Trigger:    capsule.TriggerMode(rule.Trigger),
			Conditions: rule.Conditions,
			Action:     capsule.ScalingAction(rule.Action),
			Cooldown:   time.Duration(rule.CooldownSeconds) * time.Second,
		})
	}

	for _, rule := range pb.PlacementRules {
		spec.PlacementRules = append(spec.PlacementRules, capsule.PlacementRule{
			Name:     rule.Name,
			Type:     capsule.PlacementType(rule.Type),
			Mode:     capsule.PlacementMode(rule.Mode),
			Names:    rule.TargetNames,
			Labels:   rule.Labels,
			Required: rule.Required,
		})
	}

	return spec
}
