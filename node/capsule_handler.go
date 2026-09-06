package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/enums"
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
//
// ReservationNodeID returns the node ID currently holding the group
// reservation for the given group capsule (empty when no
// reservation is active). The capsule handler uses it to detect
// orphaned same-node groups in onNodeFailed: same-node group
// placement records a single reservation per group keyed on the
// winning node, not per-replica assignments on member capsules,
// so the per-replica orphan walk cannot find a match.
//
// CancelGroupInFlight cancels any in-flight group election round for
// the given group so a follow-up GroupReelectionRequested fired by
// the node-failure path is not deduped by a still-running original
// round.
type electionForgetter interface {
	ForgetCapsule(id capsule.CapsuleID)
	ReservationNodeID(id capsule.CapsuleID) string
	CancelGroupInFlight(id capsule.CapsuleID)

	// ClearGroupReservation removes the capacity reservation for a
	// same-node group so a follow-up re-election (driven by either a
	// node-failure path or a member-placement-failed rollback) can
	// record a fresh reservation without colliding with the prior
	// one. Idempotent for groups with no active reservation.
	ClearGroupReservation(id capsule.CapsuleID)
}

// defaultOrphanGrace is the default delay between observing an orphan
// member capsule (one whose GroupID points at an unknown group) and
// deleting it. Configurable via WithCapsuleHandlerOrphanGrace; the
// grace window covers transient gossip ordering where a member
// announcement arrives before its parent group.
const defaultOrphanGrace = 30 * time.Second

// defaultPlacementRetryCap bounds how many same-node group rollback
// rounds the handler will drive for a single group before giving up
// and emitting a terminal GroupClaimFailed event. Each round picks a
// different node (the failing node lands in ExcludeNodes), so the cap
// also bounds how many distinct nodes the group will be attempted on.
const defaultPlacementRetryCap = 3

// defaultDeleteStopGrace is the graceful-stop window handed to the
// runtime when tearing down a deleted capsule's local container. A
// user-initiated delete is an orderly teardown, so the container is
// given a chance to exit cleanly before being force-removed.
// Configurable via WithCapsuleHandlerDeleteStopGrace.
const defaultDeleteStopGrace = 10 * time.Second

// placementFailedEntry holds the per-group bookkeeping the capsule
// handler retains while it accumulates rollback rounds and after it
// gives up on a same-node group placement. The 10.17 recovery path
// (NodeJoined) reads these to retry placement when cluster capacity
// grows, and the GroupClaimWon path (10.14) reads them to drop the
// entry once placement converges.
//
// clusterPath, lastReason, and excludeNodes are required to rebuild a
// GroupReelectionRequested event identical in shape to the one that
// last fired (modulo the empty FailedNodeID — the recovery is not
// triggered by a specific node-failure event). failedAt is retained
// only for diagnostic logging.
//
// capReached gates the recovery path: only entries whose retry cap
// has been reached are eligible for NodeJoined-driven recovery. The
// entry exists ahead of cap exhaustion so excludeNodes accumulates
// across every rollback round in this group's history, not just the
// final round.
type placementFailedEntry struct {
	clusterPath  string
	lastReason   string
	excludeNodes []string
	failedAt     time.Time
	capReached   bool
}

// runtimeGroupRollback is the narrow interface CapsuleHandler uses
// during same-node group rollback. Satisfied by *runtime.Handler;
// kept narrow so capsule_handler tests do not need a full
// runtime.Handler to drive the MemberPlacementFailed path.
//
// CancelGroupStarts purges any in-flight parked starts whose capsule
// belongs to the given group.
//
// StopContainer gracefully stops a running container for a sibling
// member so the rollback path can remove the partial-state survivor
// from the failing winner's runtime before the re-election lands.
type runtimeGroupRollback interface {
	CancelGroupStarts(groupID string) int
	StopContainer(capsuleID, replicaID string, gracePeriod time.Duration) error
}

// CapsuleHandler bridges the capsule module with the node's event bus and PubSub.
// It owns the capsule.Manager, orbit.Manager (per cluster), and scaling.Monitor,
// and coordinates capsule announcements, reception, and scaling decisions.
type CapsuleHandler struct {
	manager         *capsule.Manager
	mu              sync.RWMutex
	orbits          map[string]*orbit.Manager      // clusterPath -> orbit manager
	announcers      map[string]*orbit.Announcer    // clusterPath -> announcer
	dedups          map[string]*orbit.Deduplicator // clusterPath -> dedup
	scalingMonitor  *scaling.Monitor
	metricsRegistry scaling.MetricsRegistry
	eventBus        events.Bus
	logger          *zap.Logger
	nodeID          string
	ps              *pubsub.PubSub
	privateKey      crypto.PrivKey
	phonebook       phonebook.IPhonebook
	electionForget  electionForgetter
	runtimeRollback runtimeGroupRollback
	// serviceHandler receives OnCapsuleReceived / OnCapsuleDeleted
	// fan-out so service backend identity binding fires in lock-step
	// with capsule arrival / withdrawal. Optional: when unset (tests,
	// service-disabled deployments) the fan-out is skipped silently.
	serviceHandler *ServiceHandler

	// placementRetries counts how many MemberPlacementFailed rollback
	// rounds the handler has driven for each same-node group. Bounded
	// by placementRetryCap; on reaching the cap the handler emits a
	// terminal GroupClaimFailed and stops re-electing. The map is
	// keyed by group capsule ID and is protected by mu.
	placementRetries map[capsule.CapsuleID]int
	placementRetryCap int

	// placementFailedGroups records same-node groups that exhausted
	// their retry cap. Entries are added when the cap fires (the
	// retry counter is reset alongside so the next attempt starts
	// clean), consulted on NodeJoined to drive auto-recovery, and
	// cleared when a GroupClaimWon arrives for the group. The map is
	// keyed by group capsule ID and is protected by mu.
	placementFailedGroups map[capsule.CapsuleID]placementFailedEntry

	// orphanGrace bounds how long the handler waits for a missing parent
	// group to arrive before reaping a received member capsule whose
	// GroupID points nowhere. Default defaultOrphanGrace.
	orphanGrace time.Duration

	// deleteStopGrace is the graceful-stop window handed to the runtime
	// when stopping a deleted capsule's local container. Default
	// defaultDeleteStopGrace.
	deleteStopGrace time.Duration

	// pendingReaps holds the per-member cancel functions for in-flight
	// orphan reap timers. Indexed by member capsule ID. Protected by mu.
	// A timer is added when an orphan is observed and removed either by
	// cancellation (parent group arrived in time) or by the reap firing.
	pendingReaps map[capsule.CapsuleID]context.CancelFunc

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

// WithCapsuleHandlerRuntimeRollback wires the hook used to cancel
// in-flight parked starts in the runtime handler during a same-node
// group rollback. Satisfied by *runtime.Handler. Without it the
// handler still drives the rollback (sibling stop, reservation
// release, re-election) but leaves parked starts in place to age out
// via their own dependency timeout — safe but slower.
func WithCapsuleHandlerRuntimeRollback(r runtimeGroupRollback) CapsuleHandlerOption {
	return func(h *CapsuleHandler) { h.runtimeRollback = r }
}

// SetRuntimeRollback installs or replaces the runtime rollback hook
// after construction. Used by the node wiring where the runtime
// handler is built after the CapsuleHandler.
func (h *CapsuleHandler) SetRuntimeRollback(r runtimeGroupRollback) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runtimeRollback = r
}

// WithCapsuleHandlerServiceHandler wires the ServiceHandler that
// receives capsule-receipt / -deletion fan-out for service-mesh
// identity binding. Optional; when unset the capsule handler skips
// the fan-out and only publishes the standard CapsuleReceived /
// CapsuleWithdrawn events on the node bus.
func WithCapsuleHandlerServiceHandler(s *ServiceHandler) CapsuleHandlerOption {
	return func(h *CapsuleHandler) { h.serviceHandler = s }
}

// SetServiceHandler installs or replaces the ServiceHandler fan-out
// hook after construction. The node wiring uses this because the
// ServiceHandler is built after the CapsuleHandler (it depends on
// the capsule manager for backend lookup).
func (h *CapsuleHandler) SetServiceHandler(s *ServiceHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.serviceHandler = s
}

// WithCapsuleHandlerPlacementRetryCap overrides the maximum number of
// MemberPlacementFailed rollback rounds the handler will drive for a
// single same-node group before giving up and emitting a terminal
// GroupClaimFailed. Non-positive values are ignored; default is
// defaultPlacementRetryCap.
func WithCapsuleHandlerPlacementRetryCap(n int) CapsuleHandlerOption {
	return func(h *CapsuleHandler) {
		if n > 0 {
			h.placementRetryCap = n
		}
	}
}

// WithCapsuleHandlerOrphanGrace sets the grace period the orphan reaper
// waits between observing a member capsule whose GroupID points at an
// unknown group and deleting that member. The window absorbs transient
// gossip ordering — a member announcement that arrives slightly ahead
// of its parent group is NOT a permanent orphan. Defaults to 30s when
// the option is omitted; non-positive values are ignored and the
// default is kept.
func WithCapsuleHandlerOrphanGrace(d time.Duration) CapsuleHandlerOption {
	return func(h *CapsuleHandler) {
		if d > 0 {
			h.orphanGrace = d
		}
	}
}

// WithCapsuleHandlerDeleteStopGrace sets the graceful-stop window the
// handler hands to the runtime when stopping a deleted capsule's local
// container. Defaults to defaultDeleteStopGrace when the option is
// omitted; non-positive values are ignored and the default is kept.
func WithCapsuleHandlerDeleteStopGrace(d time.Duration) CapsuleHandlerOption {
	return func(h *CapsuleHandler) {
		if d > 0 {
			h.deleteStopGrace = d
		}
	}
}

// NewCapsuleHandler creates a new capsule handler with an owned capsule manager.
// The manager is created internally and its event handler is wired to the handler's
// onManagerEvent method, which bridges capsule events to the node event bus AND
// triggers orbit announcements on CapsuleCreated.
func NewCapsuleHandler(manager *capsule.Manager, opts ...CapsuleHandlerOption) *CapsuleHandler {
	h := &CapsuleHandler{
		manager:               manager,
		orbits:                make(map[string]*orbit.Manager),
		announcers:            make(map[string]*orbit.Announcer),
		dedups:                make(map[string]*orbit.Deduplicator),
		pendingReaps:          make(map[capsule.CapsuleID]context.CancelFunc),
		placementRetries:      make(map[capsule.CapsuleID]int),
		placementRetryCap:     defaultPlacementRetryCap,
		placementFailedGroups: make(map[capsule.CapsuleID]placementFailedEntry),
		orphanGrace:           defaultOrphanGrace,
		deleteStopGrace:       defaultDeleteStopGrace,
		logger:                zap.NewNop(),
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

// HasPlacementFailedEntry reports whether the handler currently tracks
// the given group as placement-failed pending NodeJoined recovery.
// Returns true only for entries whose retry cap has been reached;
// in-progress rollback bookkeeping is invisible to this accessor.
// Exposed for integration tests asserting the 10.17 recovery contract.
func (h *CapsuleHandler) HasPlacementFailedEntry(id capsule.CapsuleID) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	entry, ok := h.placementFailedGroups[id]
	return ok && entry.capReached
}

// Start begins listening for node events and starts the scaling monitor.
// It derives a long-lived handler context from the passed ctx; orbit message
// loops, scaling monitor, and the dedup cleanup loop all run on that context.
func (h *CapsuleHandler) Start(ctx context.Context) {
	h.ctx, h.cancel = context.WithCancel(ctx)

	h.wg.Add(14)
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
	go func() {
		defer h.wg.Done()
		h.handleMemberRunning(h.ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.handleOrphanReaper(h.ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.handleMemberPlacementFailed(h.ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.handleNodeJoined(h.ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.handleGroupClaimWon(h.ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.handleElectionYielded(h.ctx)
	}()
	go func() {
		defer h.wg.Done()
		h.handleGroupClaimYielded(h.ctx)
	}()

	h.scalingMonitor.Start(h.ctx)

	h.logger.Info("capsule handler started")
}

// Stop halts the handler and its components. Cancels the long-lived context
// so all goroutines exit, waits for them to finish, then leaves all orbit
// subscriptions.
//
// Stop also cancels every pending orphan-reap timer so no Delete fires
// after shutdown — without this, a reap scheduled moments before Stop
// would run on a manager whose store may have already been torn down.
func (h *CapsuleHandler) Stop() {
	if h.cancel != nil {
		h.cancel()
	}
	h.scalingMonitor.Stop()

	// Cancel pending orphan reaps before waiting on goroutines so any
	// reaper waiting on a timer wakes up immediately and exits.
	h.mu.Lock()
	for id, cancel := range h.pendingReaps {
		cancel()
		delete(h.pendingReaps, id)
	}
	h.mu.Unlock()

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

	// Auto-join the cluster-wide capsule control plane. Every node in the
	// cluster subscribes so ALL capsule announcements (standalone and
	// group) reach the entire mesh — this is what lets every candidate
	// node run the gravity election instead of only the originator.
	// Joined synchronously while holding h.mu to keep startup deterministic.
	if err := orbitMgr.Join(h.ctx, orbit.CapsuleControlOrbit); err != nil {
		h.logger.Warn("failed to auto-join capsule control plane",
			zap.String("cluster", clusterPath),
			zap.String("orbit", orbit.CapsuleControlOrbit),
			zap.Error(err))
	}

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
			if err := h.manager.SyncStatus(id, enums.CapsuleStatusEnum.Assigned()); err != nil {
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
			if err := h.manager.SyncStatus(id, enums.CapsuleStatusEnum.Announced()); err != nil {
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
			h.onContainerCrash(failed)
		}
	}
}

// onContainerCrash is the per-event body of handleContainerCrash, split out
// so tests can drive it without spinning up the subscriber goroutine. For
// each replica of the crashed capsule that was bound to this node it clears
// the stale binding (UnassignReplica) and then requests a re-election.
//
// Order is load-bearing: UnassignReplica is synchronous and persists the
// cleared binding before requestElection (which only publishes an event)
// returns. The originating node's self-anti-affinity in gravity.IsEligible
// counts replicas via NodesRunningCapsule, which skips empty NodeIDs — so the
// binding MUST be clear before the election round evaluates eligibility, or
// the node that just lost the container is wrongly excluded from reclaiming
// it (single-node clusters dead-lock entirely).
func (h *CapsuleHandler) onContainerCrash(failed events.CapsuleExecutionFailed) {
	id := capsule.CapsuleID(failed.CapsuleID)
	c := h.manager.Get(id)
	if c == nil {
		return
	}

	h.logger.Warn("container crash detected, requesting re-election",
		zap.String("capsule_id", failed.CapsuleID),
		zap.String("reason", failed.Reason))

	// Downgrade the capsule-level FSM Running → Announced ONCE (the FSM is
	// per-capsule, not per-replica) so the re-election round's StartElection /
	// WinElection / MarkRunning transitions are valid. Non-fatal: a non-running
	// state or a sibling replica's crash event may have already downgraded it.
	if err := h.manager.MarkNodeFailed(id); err != nil {
		h.logger.Debug("FSM downgrade on crash rejected (already downgraded?)",
			zap.String("capsule_id", failed.CapsuleID), zap.Error(err))
	}

	// c.Replicas is a snapshot copy from Manager.Get, so clearing the live
	// binding via UnassignReplica mid-iteration does not mutate the slice we
	// are ranging over. Fire a re-election for each replica that was running
	// on this node. The local node may or may not win again — gravity
	// decides.
	for _, replica := range c.Replicas {
		if replica.NodeID != h.nodeID {
			continue
		}

		// Clear the stale binding BEFORE requesting the election so the
		// lost replica stops counting toward self-anti-affinity and this
		// node becomes eligible to re-place it.
		if err := h.manager.UnassignReplica(id, replica.ReplicaID); err != nil {
			h.logger.Warn("failed to clear crashed replica binding before re-election",
				zap.String("capsule_id", failed.CapsuleID),
				zap.String("replica", string(replica.ReplicaID)),
				zap.Error(err))
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

// handleMemberRunning subscribes to CapsuleRunning events and, when the
// running capsule is a group member, checks whether every other member
// of that group has also reached Running. If yes, it fires
// MarkGroupRunning on the manager so the group's lifecycle advances
// from Announced to Running.
//
// The subscriber observes BOTH local container starts (via the
// runtime adapter's direct publish) and remote running transitions
// (via the manager-event bridge in onManagerEvent). Each Running event
// for a member re-evaluates the parent group; MarkGroupRunning is
// idempotent at the FSM level — a group already past Running rejects
// the trigger with an error which is logged at Debug and ignored.
func (h *CapsuleHandler) handleMemberRunning(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeCapsuleRunning)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			running, ok := event.(events.CapsuleRunning)
			if !ok {
				continue
			}
			h.onMemberRunning(capsule.CapsuleID(running.CapsuleID))
		}
	}
}

// onMemberRunning is the per-event body of handleMemberRunning, split out
// so tests can drive it without spinning up the subscriber goroutine.
func (h *CapsuleHandler) onMemberRunning(memberID capsule.CapsuleID) {
	member := h.manager.Get(memberID)
	if member == nil {
		// The member may have been deleted between the event firing
		// and this lookup. Nothing to coordinate.
		return
	}
	if member.Spec.GroupID == "" {
		// Standalone capsule — no group coordination needed.
		return
	}

	groupID := member.Spec.GroupID
	group := h.manager.Get(groupID)
	if group == nil {
		// Group capsule not yet observed locally. Could be a transient
		// gossip ordering issue; the orphan reaper handles the case
		// where it stays missing.
		h.logger.Debug("member running but parent group not yet known",
			zap.String("cluster", member.ClusterID),
			zap.String("group", groupID.String()),
			zap.String("member", memberID.String()))
		return
	}

	_, members := h.manager.GetGroup(groupID)
	if len(members) == 0 {
		// GetGroup returned no members — possible if the group lookup
		// raced with a deletion or the store is in an unexpected state.
		return
	}

	allRunning := true
	for _, m := range members {
		// Re-fetch via Get to take a snapshot under the manager mutex
		// so reading c.Status does not race with concurrent lifecycle
		// transitions writing to the same capsule pointer.
		snap := h.manager.Get(m.ID)
		if snap == nil || snap.Status != enums.CapsuleStatusEnum.Running() {
			allRunning = false
			break
		}
	}
	if !allRunning {
		h.logger.Debug("group not yet fully running",
			zap.String("cluster", group.ClusterID),
			zap.String("group", groupID.String()),
			zap.Int("members", len(members)))
		return
	}

	if err := h.manager.MarkGroupRunning(groupID); err != nil {
		// MarkGroupRunning is idempotent at the call site: groups
		// already past Announced (e.g. Running, Stopping, Stopped)
		// reject the trigger from the FSM. Treat as success.
		h.logger.Debug("MarkGroupRunning rejected (likely already past)",
			zap.String("cluster", group.ClusterID),
			zap.String("group", groupID.String()),
			zap.Error(err))
		return
	}

	h.logger.Info("group capsule running",
		zap.String("cluster", group.ClusterID),
		zap.String("group", groupID.String()),
		zap.String("name", group.Spec.Name),
		zap.Int("members", len(members)))
}

// handleOrphanReaper subscribes to CapsuleReceived events and schedules
// deferred deletion for members whose parent group is not (yet) known
// to the local manager. The grace window absorbs transient gossip
// ordering where a member announcement lands ahead of the group.
//
// If during the grace window the matching group capsule is received,
// the corresponding reap is canceled — see onGroupReceivedCancelReaps.
// On Stop() every pending reap is cancelled so no Delete fires post-shutdown.
func (h *CapsuleHandler) handleOrphanReaper(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeCapsuleReceived)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			received, ok := event.(events.CapsuleReceived)
			if !ok {
				continue
			}
			h.onCapsuleReceivedForReaper(capsule.CapsuleID(received.CapsuleID))
		}
	}
}

// onCapsuleReceivedForReaper drives the orphan reaper's per-event logic.
// Two responsibilities:
//   - If the received capsule is a group, cancel any pending reap for
//     members of that group whose orphan window had been opened earlier.
//   - If the received capsule is a member whose parent group is unknown,
//     schedule a deferred reap on a new goroutine guarded by h.ctx and
//     a per-member cancel context.
func (h *CapsuleHandler) onCapsuleReceivedForReaper(id capsule.CapsuleID) {
	c := h.manager.Get(id)
	if c == nil {
		return
	}

	// A group capsule arrival cancels any orphan reaps for its members.
	if c.Spec.Kind == capsule.CapsuleKindEnum.Group() {
		h.cancelReapsForGroup(c.ID)
		return
	}

	// Standalone capsules are not orphans.
	if c.Spec.GroupID == "" {
		return
	}

	// Member capsule: does the parent group exist locally?
	if g := h.manager.Get(c.Spec.GroupID); g != nil {
		return
	}

	h.scheduleOrphanReap(c.ID, c.Spec.GroupID, c.ClusterID)
}

// cancelReapsForGroup cancels every pending orphan reap whose member
// capsule belongs to groupID. The cancel propagates through the per-reap
// context so the deferred-delete goroutine exits without firing Delete.
func (h *CapsuleHandler) cancelReapsForGroup(groupID capsule.CapsuleID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for memberID, cancel := range h.pendingReaps {
		member := h.manager.Get(memberID)
		if member == nil {
			continue
		}
		if member.Spec.GroupID == groupID {
			cancel()
			delete(h.pendingReaps, memberID)
			h.logger.Debug("orphan reap cancelled: parent group arrived",
				zap.String("group", groupID.String()),
				zap.String("member", memberID.String()))
		}
	}
}

// scheduleOrphanReap arms a single-shot timer that deletes the orphan
// member after the configured grace period. Re-entry is idempotent: if
// a reap is already pending for memberID, a new one is NOT scheduled.
//
// The reap goroutine respects two cancellation sources: the per-member
// cancel context (used to abort when the parent group arrives or on
// handler Stop), and h.ctx (the long-lived handler context).
func (h *CapsuleHandler) scheduleOrphanReap(memberID, groupID capsule.CapsuleID, clusterPath string) {
	h.mu.Lock()
	if _, exists := h.pendingReaps[memberID]; exists {
		h.mu.Unlock()
		return
	}
	reapCtx, cancel := context.WithCancel(h.ctx)
	h.pendingReaps[memberID] = cancel
	grace := h.orphanGrace
	h.mu.Unlock()

	h.logger.Info("orphan member capsule scheduled for reap",
		zap.String("cluster", clusterPath),
		zap.String("group", groupID.String()),
		zap.String("member", memberID.String()),
		zap.Duration("grace", grace))

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		timer := time.NewTimer(grace)
		defer timer.Stop()

		select {
		case <-reapCtx.Done():
			// Cancelled — parent group arrived OR handler stopping.
			h.mu.Lock()
			delete(h.pendingReaps, memberID)
			h.mu.Unlock()
			return
		case <-timer.C:
			h.executeOrphanReap(memberID, groupID, clusterPath)
		}
	}()
}

// executeOrphanReap deletes the member capsule whose orphan grace
// expired. ErrNotFound is treated as success — another node may have
// already withdrawn it. The pending-reap entry is cleared before exit.
func (h *CapsuleHandler) executeOrphanReap(memberID, groupID capsule.CapsuleID, clusterPath string) {
	h.mu.Lock()
	delete(h.pendingReaps, memberID)
	h.mu.Unlock()

	// Re-check the parent group: a concurrent arrival between the
	// timer firing and this lookup means the orphan is no longer one.
	if h.manager.Get(groupID) != nil {
		h.logger.Debug("orphan reap aborted: parent group present at fire time",
			zap.String("cluster", clusterPath),
			zap.String("group", groupID.String()),
			zap.String("member", memberID.String()))
		return
	}

	// Use the handler context so the delete inherits cancellation on
	// Stop. A short timeout bounds the call independently of the
	// handler's overall lifetime.
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()

	if err := h.manager.Delete(ctx, memberID); err != nil {
		if errors.Is(err, capsule.ErrNotFound) {
			// Idempotent: another node already withdrew it.
			h.logger.Debug("orphan reap: member already gone",
				zap.String("cluster", clusterPath),
				zap.String("group", groupID.String()),
				zap.String("member", memberID.String()))
			return
		}
		h.logger.Warn("orphan reap delete failed",
			zap.String("cluster", clusterPath),
			zap.String("group", groupID.String()),
			zap.String("member", memberID.String()),
			zap.Error(err))
		return
	}

	h.logger.Info("orphan member capsule reaped",
		zap.String("cluster", clusterPath),
		zap.String("group", groupID.String()),
		zap.String("member", memberID.String()))
}

// handleMemberPlacementFailed subscribes to MemberPlacementFailed events
// and drives the same-node group rollback path: stop every sibling,
// cancel any in-flight parked starts in the runtime, release the
// election capacity reservation, then publish a
// GroupReelectionRequested with the failing node added to
// ExcludeNodes. After a configurable cap on retry rounds the handler
// emits GroupClaimFailed and stops re-electing — the group remains
// Announced until operator intervention.
//
// Same-orbit groups are never reached by this path: the runtime only
// emits MemberPlacementFailed when GroupView.Colocation reports
// same-node.
func (h *CapsuleHandler) handleMemberPlacementFailed(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeMemberPlacementFailed)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			failed, ok := event.(events.MemberPlacementFailed)
			if !ok {
				continue
			}
			h.onMemberPlacementFailed(failed)
		}
	}
}

// onMemberPlacementFailed is the per-event body of
// handleMemberPlacementFailed. Split out so tests can drive it without
// the subscriber goroutine.
func (h *CapsuleHandler) onMemberPlacementFailed(failed events.MemberPlacementFailed) {
	groupID := capsule.CapsuleID(failed.GroupID)
	if groupID == "" {
		h.logger.Debug("member placement failed event missing group_id; ignoring",
			zap.String("capsule_id", failed.CapsuleID),
			zap.String("reason", failed.Reason))
		return
	}

	group, siblings := h.manager.GetGroup(groupID)
	if group == nil || group.Spec.Group == nil {
		h.logger.Warn("member placement failed: parent group not known locally",
			zap.String("capsule_id", failed.CapsuleID),
			zap.String("group_id", failed.GroupID),
			zap.String("reason", failed.Reason))
		return
	}

	if group.Spec.Group.Colocation != capsule.ColocationModeEnum.SameNode() {
		// Same-orbit groups should never reach this path; the runtime
		// emits MemberPlacementFailed only when Colocation reports
		// same-node. Log and drop defensively.
		h.logger.Debug("member placement failed for non-same-node group; ignoring",
			zap.String("group_id", failed.GroupID),
			zap.String("capsule_id", failed.CapsuleID),
			zap.String("colocation", string(group.Spec.Group.Colocation)))
		return
	}

	// Bump the retry counter under mu and decide whether to give up.
	// Snapshot the prior exclude list — entries are upserted on every
	// round (not just cap exhaustion), so the cumulative set across
	// all rollback rounds is always available.
	clusterPath := failed.ClusterPath
	if clusterPath == "" {
		clusterPath = group.ClusterID
	}
	h.mu.Lock()
	attempt := h.placementRetries[groupID] + 1
	h.placementRetries[groupID] = attempt
	retryCap := h.placementRetryCap
	rollback := h.runtimeRollback
	forget := h.electionForget
	prior := h.placementFailedGroups[groupID]
	cumulativeExclude := mergeExcludeNodes(prior.excludeNodes, failed.NodeID)
	// Upsert the entry every round so the history is preserved even
	// before cap exhaustion. capReached stays false until the cap
	// fires; only then does NodeJoined recovery consider the entry.
	h.placementFailedGroups[groupID] = placementFailedEntry{
		clusterPath:  clusterPath,
		lastReason:   failed.Reason,
		excludeNodes: cumulativeExclude,
		failedAt:     time.Now(),
		capReached:   prior.capReached,
	}
	h.mu.Unlock()

	if retryCap <= 0 {
		retryCap = defaultPlacementRetryCap
	}

	h.logger.Warn("same-node group member placement failed, rolling back",
		zap.String("group_id", failed.GroupID),
		zap.String("capsule_id", failed.CapsuleID),
		zap.String("failed_node", failed.NodeID),
		zap.String("reason", failed.Reason),
		zap.Int("attempt", attempt),
		zap.Int("cap", retryCap))

	// Cancel any in-flight parked starts for this group's members so a
	// dependency-released sibling does not race the rollback's stop
	// calls by suddenly starting after we just stopped it.
	if rollback != nil {
		_ = rollback.CancelGroupStarts(failed.GroupID)
	}

	// Walk siblings and roll each one back to a pre-placement state:
	// stop the runtime container (through the ignore-set), downgrade the
	// FSM, and clear every replica binding (O5b). The winning node's
	// actual container must be stopped, not just its FSM transitioned,
	// otherwise it races the new winner's StartGroup. Factored into
	// rollbackGroupContainers so the O14c group-yield subscriber reuses
	// the identical mechanism (stop+downgrade+unbind) without the
	// retry-cap / re-election policy that follows here.
	h.rollbackGroupContainers(failed.GroupID, siblings, rollback)

	// Release the capacity reservation for this group so a re-election
	// can proceed without the watchdog firing a spurious failure.
	if forget != nil {
		forget.CancelGroupInFlight(groupID)
		forget.ClearGroupReservation(groupID)
	}

	// At the cap: stop driving rollbacks AND mark the group's tracked
	// entry as capReached so the NodeJoined recovery path (10.17) can
	// retry when cluster capacity grows. Reset the retry counter so
	// the recovery-driven retry starts a fresh round budget. Surface
	// the terminal failure via GroupClaimFailed for operator
	// visibility; the group remains Announced.
	if attempt > retryCap {
		h.mu.Lock()
		entry := h.placementFailedGroups[groupID]
		entry.capReached = true
		h.placementFailedGroups[groupID] = entry
		// Reset the retry counter so the recovery path's first retry
		// is not pre-loaded with prior attempts.
		delete(h.placementRetries, groupID)
		h.mu.Unlock()

		h.logger.Error("same-node group placement retry cap reached; emitting terminal GroupClaimFailed",
			zap.String("group_id", failed.GroupID),
			zap.String("cluster", clusterPath),
			zap.Int("attempt", attempt),
			zap.Int("cap", retryCap),
			zap.Int("exclude_nodes", len(cumulativeExclude)))
		h.eventBus.Publish(events.GroupClaimFailed{
			BaseEvent:   events.NewBaseEvent(),
			GroupID:     failed.GroupID,
			ClusterPath: clusterPath,
			Reason:      fmt.Sprintf("placement retries exhausted after %d attempts: %s", attempt, failed.Reason),
		})
		return
	}

	memberIDs := make([]string, 0, len(group.Spec.Group.MemberIDs))
	for _, id := range group.Spec.Group.MemberIDs {
		memberIDs = append(memberIDs, id.String())
	}

	h.eventBus.Publish(events.GroupReelectionRequested{
		BaseEvent:    events.NewBaseEvent(),
		GroupID:      failed.GroupID,
		ClusterPath:  clusterPath,
		MemberIDs:    memberIDs,
		FailedNodeID: failed.NodeID,
		ExcludeNodes: cumulativeExclude,
	})

	h.logger.Info("same-node group re-election requested after placement rollback",
		zap.String("group_id", failed.GroupID),
		zap.String("cluster", clusterPath),
		zap.String("failed_node", failed.NodeID),
		zap.Int("members", len(memberIDs)),
		zap.Int("attempt", attempt),
		zap.Int("exclude_nodes", len(cumulativeExclude)))
}

// mergeExcludeNodes returns the union of prior and the optional new
// node ID. The result has no duplicates and preserves the input order:
// prior entries first (in their original order), the new ID appended
// only if non-empty and not already present.
func mergeExcludeNodes(prior []string, newNode string) []string {
	out := make([]string, 0, len(prior)+1)
	seen := make(map[string]struct{}, len(prior)+1)
	for _, id := range prior {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if newNode != "" {
		if _, dup := seen[newNode]; !dup {
			out = append(out, newNode)
		}
	}
	return out
}

// handleNodeJoined subscribes to NewMemberReceived events on the bus
// (the node-level signal that a peer just joined a cluster) and drives
// the 10.17 recovery path: every same-node group recorded in
// placementFailedGroups by the placement-failed cap (10.16) gets a
// fresh GroupReelectionRequested with the cumulative excludeNodes set
// so the historically-failed nodes are skipped while the just-joined
// peer becomes a candidate.
//
// Entries are NOT removed here. A successful placement triggers
// handleGroupClaimWon which clears the entry; a subsequent placement
// failure on the new candidate flows through onMemberPlacementFailed
// again, which adds the new failing node to the entry's excludeNodes
// on the next cap exhaustion.
func (h *CapsuleHandler) handleNodeJoined(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeNewMemberReceived)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			joined, ok := event.(events.NewMemberReceived)
			if !ok {
				continue
			}
			h.onNodeJoined(joined)
		}
	}
}

// onNodeJoined is the per-event body of handleNodeJoined. Split out so
// tests can drive it without spinning up the subscriber goroutine.
func (h *CapsuleHandler) onNodeJoined(joined events.NewMemberReceived) {
	// Snapshot the pending-recovery set under mu so the actual emission
	// happens without holding the handler lock (publish path may take
	// other locks). Only entries flagged capReached are eligible —
	// pre-cap rollback bookkeeping is in-flight and the existing
	// per-MPF rollback path handles it.
	h.mu.Lock()
	if len(h.placementFailedGroups) == 0 {
		h.mu.Unlock()
		return
	}
	pending := make(map[capsule.CapsuleID]placementFailedEntry, len(h.placementFailedGroups))
	for id, entry := range h.placementFailedGroups {
		if !entry.capReached {
			continue
		}
		pending[id] = entry
	}
	h.mu.Unlock()
	if len(pending) == 0 {
		return
	}

	for groupID, entry := range pending {
		group, members := h.manager.GetGroup(groupID)
		if group == nil || group.Spec.Group == nil {
			// Group was deleted locally between the cap firing and the
			// recovery tick. Drop the stale entry so the map stays bounded.
			h.mu.Lock()
			delete(h.placementFailedGroups, groupID)
			h.mu.Unlock()
			h.logger.Debug("placement-failed entry dropped: group no longer present",
				zap.String("group_id", groupID.String()),
				zap.String("cluster", entry.clusterPath),
				zap.String("joined_node", joined.NodeID))
			continue
		}

		// Refresh MemberIDs from the current group state — members may
		// have been added or removed after the cap fired.
		var memberIDs []string
		if len(members) > 0 {
			memberIDs = make([]string, 0, len(members))
			for _, m := range members {
				memberIDs = append(memberIDs, m.ID.String())
			}
		} else {
			memberIDs = make([]string, 0, len(group.Spec.Group.MemberIDs))
			for _, id := range group.Spec.Group.MemberIDs {
				memberIDs = append(memberIDs, id.String())
			}
		}

		h.eventBus.Publish(events.GroupReelectionRequested{
			BaseEvent:    events.NewBaseEvent(),
			GroupID:      groupID.String(),
			ClusterPath:  entry.clusterPath,
			MemberIDs:    memberIDs,
			FailedNodeID: "", // recovery, not a node-failure-driven re-election
			ExcludeNodes: append([]string(nil), entry.excludeNodes...),
		})

		h.logger.Info("same-node group placement recovery requested after node joined",
			zap.String("group_id", groupID.String()),
			zap.String("cluster", entry.clusterPath),
			zap.String("joined_node", joined.NodeID),
			zap.Int("members", len(memberIDs)),
			zap.Int("exclude_nodes", len(entry.excludeNodes)),
			zap.String("last_reason", entry.lastReason))
	}
}

// handleGroupClaimWon subscribes to GroupClaimWon events and clears
// any matching placementFailedGroups entry. A successful win means a
// pending recovery converged (or the very first placement succeeded
// before the cap was ever hit) and the group no longer needs to be
// watched. Idempotent for groups never tracked.
func (h *CapsuleHandler) handleGroupClaimWon(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeGroupClaimWon)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			won, ok := event.(events.GroupClaimWon)
			if !ok {
				continue
			}
			groupID := capsule.CapsuleID(won.GroupID)
			h.mu.Lock()
			_, tracked := h.placementFailedGroups[groupID]
			if tracked {
				delete(h.placementFailedGroups, groupID)
			}
			h.mu.Unlock()
			if tracked {
				h.logger.Info("placement-failed entry cleared after group claim won",
					zap.String("group_id", won.GroupID),
					zap.String("cluster", won.ClusterPath),
					zap.String("winner", won.NodeID))
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
// dispatching the appropriate re-election trigger for each. As the
// originator of the failure detection, this node is responsible for
// triggering the re-election; remote nodes do not duplicate the request.
//
// Same-node group members take a dedicated branch: instead of firing
// one per-replica ElectionRequested per orphaned member (which would
// fragment the group across nodes), the handler emits a single
// GroupReelectionRequested for the parent group with the failed node
// added to ExcludeNodes. Siblings already in a non-Running state are
// rolled back to Announced so the election manager can rebuild the
// group's lifecycle at a valid pre-election point; siblings that had
// reached Running on the local view are stopped before the
// re-election to avoid partial-state placement on the new winner.
//
// Standalone capsules and same-orbit group members follow the
// pre-existing per-replica re-election path.
func (h *CapsuleHandler) onNodeFailed(_ context.Context, failed events.NodeFailed) {
	// Track groups for which we've already emitted a single
	// GroupReelectionRequested so each parent group fires the request
	// at most once per node-failure event, even when multiple members
	// of the same group had replicas on the failed node.
	handledGroups := make(map[capsule.CapsuleID]struct{})

	// ListLive: this scan only reads immutable Spec fields and looks
	// up the per-capsule snapshot via Get inside the inner loop, so
	// the cheaper ListLive (no Replicas copy) is the right primitive.
	capsules := h.manager.ListLive()

	// First pass: same-node group capsules whose capacity reservation
	// is held by the failed node. The per-replica orphan walk below
	// cannot detect these because the group-claim protocol records a
	// single reservation per group keyed on the winning node, not
	// per-replica assignments on individual member capsules. Losing
	// nodes (and pre-runtime-start states on the winner) never see
	// AssignReplica fire on member capsules during group placement.
	h.mu.RLock()
	forget := h.electionForget
	h.mu.RUnlock()
	if forget != nil {
		for _, c := range capsules {
			if c.Spec.Kind != capsule.CapsuleKindEnum.Group() {
				continue
			}
			if c.Spec.Group == nil {
				continue
			}
			if c.Spec.Group.Colocation != capsule.ColocationModeEnum.SameNode() {
				continue
			}
			if forget.ReservationNodeID(c.ID) != failed.NodeID {
				continue
			}
			h.emitGroupReelectionForGroup(c, failed, handledGroups)
		}
	}

	for _, listed := range capsules {
		// Re-fetch via Get to take a snapshot (Manager.Get copies under
		// the manager mutex) so reading c.Replicas does not race with
		// concurrent AssignReplica writes from the group-claim bridge.
		c := h.manager.Get(listed.ID)
		if c == nil {
			continue
		}
		orphanReplica, isOrphan := findReplicaOnNode(c, failed.NodeID)
		if !isOrphan {
			continue
		}

		// Same-node group member: route through the group-re-election
		// path so the FSM and reservation bookkeeping stay coherent.
		if c.Spec.GroupMember && c.Spec.GroupID != "" {
			if h.maybeEmitGroupReelection(c, failed, handledGroups) {
				continue
			}
		}

		h.logger.Warn("capsule replica orphaned due to node failure",
			zap.String("capsule_id", c.ID.String()),
			zap.String("name", c.Spec.Name),
			zap.String("cluster", c.ClusterID),
			zap.String("failed_node", failed.NodeID),
			zap.String("replica_id", string(orphanReplica.ReplicaID)))

		h.requestElection(events.ElectionRequested{
			BaseEvent:      events.NewBaseEvent(),
			CapsuleID:      c.ID.String(),
			ReplicaID:      string(orphanReplica.ReplicaID),
			Reason:         events.ElectionReasonEnum.NodeFailure(),
			ClusterPath:    failed.ClusterPath,
			PreviousNodeID: failed.NodeID,
			Priority:       priorityNodeFailure,
		})
	}
}

// emitGroupReelectionForGroup is the reservation-driven variant of
// maybeEmitGroupReelection: same control flow but the caller is a
// group capsule itself rather than one of its members. Used by
// onNodeFailed's first-pass scan over groups whose reservation is
// held by the failed node.
//
// Idempotent: groups already handled via the per-member path skip
// re-emission. Resets siblings out of post-election states and stops
// any that the local view believes are Running, then publishes a
// single GroupReelectionRequested for the group.
func (h *CapsuleHandler) emitGroupReelectionForGroup(
	group *capsule.Capsule,
	failed events.NodeFailed,
	handledGroups map[capsule.CapsuleID]struct{},
) {
	if _, done := handledGroups[group.ID]; done {
		return
	}
	if group.Spec.Group == nil {
		return
	}

	_, siblings := h.manager.GetGroup(group.ID)
	for _, sib := range siblings {
		// Re-fetch via Get to take a snapshot — sib.Status races with
		// concurrent lifecycle transitions writing the same field.
		snap := h.manager.Get(sib.ID)
		if snap == nil {
			continue
		}
		switch snap.Status {
		case enums.CapsuleStatusEnum.Created(),
			enums.CapsuleStatusEnum.Assigned(),
			enums.CapsuleStatusEnum.Executing(),
			enums.CapsuleStatusEnum.Electing():
			if err := h.manager.SyncStatus(sib.ID, enums.CapsuleStatusEnum.Announced()); err != nil {
				h.logger.Debug("group reelection: sibling resync to Announced failed",
					zap.String("group", group.ID.String()),
					zap.String("sibling", sib.ID.String()),
					zap.String("status", string(snap.Status)),
					zap.Error(err))
			}
		case enums.CapsuleStatusEnum.Running():
			if err := h.manager.StopCapsule(sib.ID); err != nil {
				h.logger.Debug("group reelection: sibling stop failed",
					zap.String("group", group.ID.String()),
					zap.String("sibling", sib.ID.String()),
					zap.Error(err))
			}
		}

		// O5b: clear every sibling replica binding BEFORE the
		// GroupReelectionRequested publish. Here the binding points at the
		// DEAD node, so the surviving node's own self-exclusion is not the
		// immediate symptom — but a stale dead-node member binding otherwise
		// leaves NodesRunningCapsule reporting the dead node as a member
		// holder, skewing anti-affinity/gravity for the OTHER members'
		// placement. Clearing is correct binding hygiene, consistent with O3
		// and with onMemberPlacementFailed. Idempotent; log at Debug and
		// continue on error.
		for _, r := range snap.Replicas {
			if err := h.manager.UnassignReplica(sib.ID, r.ReplicaID); err != nil {
				h.logger.Debug("group reelection: sibling replica unbind failed",
					zap.String("group", group.ID.String()),
					zap.String("sibling", sib.ID.String()),
					zap.String("replica", string(r.ReplicaID)),
					zap.Error(err))
			}
		}
	}

	memberIDs := make([]string, 0, len(group.Spec.Group.MemberIDs))
	for _, id := range group.Spec.Group.MemberIDs {
		memberIDs = append(memberIDs, id.String())
	}

	// Cancel any in-flight original group election so the new
	// reelection event is not deduped, THEN clear the stale capacity
	// reservation before re-firing. Ordering is load-bearing and mirrors
	// the member-crash path (onMemberPlacementFailed): cancel-round
	// (releases the group-claim slot) → clear-reservation → refire. The
	// reservation holder is the failed node (this variant is only reached
	// after onNodeFailed's ReservationNodeID == failed.NodeID filter), so
	// the reservation is definitively stale; leaving it would over-commit
	// the surviving node's headroom and reproduce the O5 reservation
	// deadlock the re-election is trying to escape. Both calls are
	// idempotent no-ops when nothing is in flight / reserved.
	h.mu.RLock()
	forget := h.electionForget
	h.mu.RUnlock()
	if forget != nil {
		forget.CancelGroupInFlight(group.ID)
		forget.ClearGroupReservation(group.ID)
	}

	h.eventBus.Publish(events.GroupReelectionRequested{
		BaseEvent:    events.NewBaseEvent(),
		GroupID:      group.ID.String(),
		ClusterPath:  failed.ClusterPath,
		MemberIDs:    memberIDs,
		FailedNodeID: failed.NodeID,
	})

	handledGroups[group.ID] = struct{}{}

	h.logger.Warn("same-node group orphaned (reservation holder failed), re-election requested",
		zap.String("group", group.ID.String()),
		zap.String("cluster", failed.ClusterPath),
		zap.String("failed_node", failed.NodeID),
		zap.Int("members", len(memberIDs)))
}

// findReplicaOnNode returns the first replica of the capsule whose
// NodeID matches the failed node ID. The bool is false when no replica
// of the capsule was hosted on the failed node.
func findReplicaOnNode(c *capsule.Capsule, nodeID string) (capsule.ReplicaState, bool) {
	for _, r := range c.Replicas {
		if r.NodeID == nodeID {
			return r, true
		}
	}
	return capsule.ReplicaState{}, false
}

// maybeEmitGroupReelection handles the same-node group branch of
// onNodeFailed. It returns true when the member belongs to a
// SameNode-colocation group and the handler emitted (or already
// emitted) a GroupReelectionRequested for that group's parent.
//
// Behaviour:
//   - Looks up the parent group locally; if the group is unknown or
//     uses a different colocation mode, returns false so the caller
//     falls back to the per-replica re-election path.
//   - Resyncs every sibling currently in {Created, Assigned,
//     Executing} back to Announced so the lifecycle can run a fresh
//     election from a valid pre-election state.
//   - Stops every sibling that the local view believes is Running so
//     the new winner does not see stale survivors.
//   - Publishes exactly one GroupReelectionRequested for the parent
//     group, with the failed node in ExcludeNodes. The handledGroups
//     set ensures we never double-fire when several members of the
//     same group share the failed node.
func (h *CapsuleHandler) maybeEmitGroupReelection(
	member *capsule.Capsule,
	failed events.NodeFailed,
	handledGroups map[capsule.CapsuleID]struct{},
) bool {
	groupID := member.Spec.GroupID
	if _, done := handledGroups[groupID]; done {
		// Another member of the same group already drove the
		// reelection path on this node-failure tick. Mark the orphan
		// as handled so the caller skips the per-replica fallback.
		return true
	}

	group, siblings := h.manager.GetGroup(groupID)
	if group == nil || group.Spec.Group == nil {
		return false
	}
	if group.Spec.Group.Colocation != capsule.ColocationModeEnum.SameNode() {
		return false
	}

	// Walk siblings and reset / stop them so the new election winner
	// starts the group from a clean state.
	for _, sib := range siblings {
		// Snapshot via Get so reading sib.Status does not race with
		// concurrent lifecycle transitions on the same capsule pointer.
		snap := h.manager.Get(sib.ID)
		if snap == nil {
			continue
		}
		switch snap.Status {
		case enums.CapsuleStatusEnum.Created(),
			enums.CapsuleStatusEnum.Assigned(),
			enums.CapsuleStatusEnum.Executing(),
			enums.CapsuleStatusEnum.Electing():
			if err := h.manager.SyncStatus(sib.ID, enums.CapsuleStatusEnum.Announced()); err != nil {
				h.logger.Debug("group reelection: sibling resync to Announced failed",
					zap.String("group", groupID.String()),
					zap.String("sibling", sib.ID.String()),
					zap.String("status", string(snap.Status)),
					zap.Error(err))
			}
		case enums.CapsuleStatusEnum.Running():
			// Survivor saw this sibling running locally — stop it
			// before the new winner re-places the group. Best effort:
			// on a remote-only view the FSM rejects with an error,
			// which we log at Debug and ignore.
			if err := h.manager.StopCapsule(sib.ID); err != nil {
				h.logger.Debug("group reelection: sibling stop failed",
					zap.String("group", groupID.String()),
					zap.String("sibling", sib.ID.String()),
					zap.Error(err))
			}
		}

		// O5b: clear every sibling replica binding BEFORE the
		// GroupReelectionRequested publish, so a node-failure-driven group
		// re-election evaluates member eligibility against cleared bindings
		// (the failed-node holder no longer skews NodesRunningCapsule). Same
		// hygiene as emitGroupReelectionForGroup and onMemberPlacementFailed;
		// mirrors O3. Idempotent; log at Debug and continue on error.
		for _, r := range snap.Replicas {
			if err := h.manager.UnassignReplica(sib.ID, r.ReplicaID); err != nil {
				h.logger.Debug("group reelection: sibling replica unbind failed",
					zap.String("group", groupID.String()),
					zap.String("sibling", sib.ID.String()),
					zap.String("replica", string(r.ReplicaID)),
					zap.Error(err))
			}
		}
	}

	memberIDs := make([]string, 0, len(group.Spec.Group.MemberIDs))
	for _, id := range group.Spec.Group.MemberIDs {
		memberIDs = append(memberIDs, id.String())
	}

	// Cancel any in-flight group round so the new GroupReelectionRequested
	// is not deduped by an old round still walking the wait timer, THEN
	// clear the stale capacity reservation before re-firing. Same
	// ordering as the member-crash path (onMemberPlacementFailed):
	// cancel-round (releases the group-claim slot) → clear-reservation →
	// refire. On the node-failure path the reservation is held by the
	// failed node (or already absent); ClearGroupReservation is
	// idempotent, so clearing it here removes a guaranteed-stale
	// reservation that would otherwise over-commit the surviving node and
	// reproduce the O5 reservation deadlock.
	h.mu.RLock()
	forget := h.electionForget
	h.mu.RUnlock()
	if forget != nil {
		forget.CancelGroupInFlight(groupID)
		forget.ClearGroupReservation(groupID)
	}

	h.eventBus.Publish(events.GroupReelectionRequested{
		BaseEvent:    events.NewBaseEvent(),
		GroupID:      groupID.String(),
		ClusterPath:  failed.ClusterPath,
		MemberIDs:    memberIDs,
		FailedNodeID: failed.NodeID,
	})

	handledGroups[groupID] = struct{}{}

	h.logger.Warn("same-node group orphaned, re-election requested",
		zap.String("group", groupID.String()),
		zap.String("cluster", failed.ClusterPath),
		zap.String("failed_node", failed.NodeID),
		zap.Int("members", len(memberIDs)))
	return true
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

// maybeRequestGroupClaim publishes a GroupClaimRequested event when the
// announced capsule is a group with SameNode colocation. The election
// manager subscribes to the event and runs the per-cluster group-claim
// protocol; the winning node alone starts every member in topological
// order. Groups with SameOrbit colocation skip this path: their members
// are placed independently by the per-replica election triggered on
// each member's own announcement.
//
// The event is published on every node that observes the group
// announcement (originator + every receiver). Each node runs the
// election locally; the manager's group-claim dedup ensures a single
// round per (group_id) on each node.
func (h *CapsuleHandler) maybeRequestGroupClaim(c *capsule.Capsule) {
	if c.Spec.Kind != capsule.CapsuleKindEnum.Group() {
		return
	}
	if c.Spec.Group == nil {
		return
	}
	if c.Spec.Group.Colocation != capsule.ColocationModeEnum.SameNode() {
		return
	}
	memberIDs := make([]string, 0, len(c.Spec.Group.MemberIDs))
	for _, id := range c.Spec.Group.MemberIDs {
		memberIDs = append(memberIDs, string(id))
	}
	if len(memberIDs) == 0 {
		h.logger.Debug("same-node group has no members; skipping group claim",
			zap.String("cluster", c.ClusterID),
			zap.String("group", c.ID.String()))
		return
	}
	h.eventBus.Publish(events.GroupClaimRequested{
		BaseEvent:   events.NewBaseEvent(),
		GroupID:     c.ID.String(),
		ClusterPath: c.ClusterID,
		MemberIDs:   memberIDs,
		Reason:      events.ElectionReasonEnum.Initial(),
		Priority:    priorityInitial,
	})
	h.logger.Info("same-node group claim requested",
		zap.String("cluster", c.ClusterID),
		zap.String("group", c.ID.String()),
		zap.Int("members", len(memberIDs)))
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
		// Fan out locally-created capsules to the service handler so
		// previously-unresolved Service backends bind to the fresh
		// capsule ID. Mirrors the EventCapsuleReceived branch for the
		// originator-side.
		h.mu.RLock()
		svcHCreate := h.serviceHandler
		h.mu.RUnlock()
		if svcHCreate != nil {
			svcHCreate.OnCapsuleReceived(event.Capsule.Spec.Name, event.CapsuleID.String())
		}
		// Group capsules defer announcement until EventCapsuleAnnounced
		// so the wire message includes the populated MemberIDs slice
		// (Manager.CreateGroup populates IDs AFTER all members are
		// created, then fires Announce). They also do not run elections
		// — the FSM rejects election triggers on Kind=Group.
		if event.Capsule.Spec.Kind == capsule.CapsuleKindEnum.Group() {
			break
		}
		// Standalone capsule path: announce to the orbit so other nodes
		// discover it, and request initial elections.
		h.announceCapsule(event.Capsule)
		// As the originator, request an initial election for every replica.
		// Other nodes that receive this capsule via gossip do NOT duplicate
		// this request (originator-only trigger).
		h.requestInitialElections(event.Capsule)

	case capsule.EventCapsuleAnnounced:
		// Group capsules are announced here (the originator's Announce
		// lifecycle transition fires after MemberIDs is populated). Bridge
		// the manager event onto the node bus and publish the wire-level
		// announcement carrying the full group spec.
		//
		// Standalone capsules already announced under EventCapsuleCreated;
		// they reach this case too when their own Announce trigger fires,
		// but a second announcement is a no-op (the orbit announcer dedups
		// by (capsule_id, status) within the dedup window).
		if event.Capsule.Spec.Kind == capsule.CapsuleKindEnum.Group() {
			h.announceCapsule(event.Capsule)
			// Same-node groups need an atomic placement: every node
			// runs the group-claim protocol locally on receipt of the
			// group announcement, and the winner alone starts every
			// member. SameOrbit groups skip this — their members are
			// placed independently by the per-replica election fire
			// path already triggered on each member capsule.
			h.maybeRequestGroupClaim(event.Capsule)
		}

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
		// Fan out to the service handler so backend identity binding
		// resolves against the freshly-received capsule. Read under
		// the handler mutex to stay race-detector-clean: the wiring
		// installs serviceHandler post-construction.
		h.mu.RLock()
		svcH := h.serviceHandler
		h.mu.RUnlock()
		if svcH != nil {
			svcH.OnCapsuleReceived(event.Capsule.Spec.Name, event.CapsuleID.String())
		}
		// Register received capsule with scaling monitor if it has rules.
		h.registerScaling(event.Capsule)
		// Each node also runs its own local strategy when it observes a
		// fresh capsule, contributing its score to the distributed election.
		// This is local-only — no extra wire traffic — but ensures the
		// election is decentralized rather than driven by the originator alone.
		//
		// Group-kind capsules with SameNode colocation take the group-claim
		// path instead of per-replica elections; SameOrbit groups still
		// fall through to per-replica elections for each member.
		if event.Capsule.Spec.Kind == capsule.CapsuleKindEnum.Group() {
			h.maybeRequestGroupClaim(event.Capsule)
		} else {
			h.requestInitialElections(event.Capsule)
		}

	case capsule.EventCapsuleDeleted:
		// Stop+remove any container this node hosts for the deleted
		// capsule BEFORE the rest of the teardown. This is the fix for
		// O8: without it the Podman container is orphaned (still
		// running, untracked) after the capsule metadata is gone. The
		// same EventCapsuleDeleted path fires on the originating node
		// (CLI delete) AND on every peer that processes the orbit
		// withdrawal (onOrbitMessage -> manager.Delete), so wiring the
		// stop here covers both the local-delete and withdrawal-received
		// paths — each node tears down only the replicas it hosts.
		h.stopLocalReplicas(event.Capsule)

		h.eventBus.Publish(events.CapsuleWithdrawn{
			BaseEvent:   events.NewBaseEvent(),
			CapsuleID:   event.CapsuleID.String(),
			ClusterPath: event.Capsule.ClusterID,
			Reason:      "deleted",
		})
		// Fan out to the service handler so dependent Services mark
		// their backends unresolved on capsule disappearance.
		h.mu.RLock()
		svcHDel := h.serviceHandler
		h.mu.RUnlock()
		if svcHDel != nil {
			svcHDel.OnCapsuleDeleted(event.Capsule.Spec.Name)
		}
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

	case capsule.EventCapsuleRunning:
		// Bridge the capsule manager's running event onto the node bus.
		// The runtime adapter already publishes events.CapsuleRunning
		// for local container starts, but the manager's transition also
		// fires when a remote peer reports Running via mesh status sync.
		// Bridging here ensures handleMemberRunning observes BOTH paths
		// (local + remote) so group-running coordination converges
		// regardless of which node hosts each member.
		h.eventBus.Publish(events.CapsuleRunning{
			BaseEvent: events.NewBaseEvent(),
			CapsuleID: event.CapsuleID.String(),
		})

	case capsule.EventCapsuleGroupReleased:
		// A member was detached from its group as part of a non-cascade
		// group delete. Bridge to the node bus so downstream subscribers
		// (Phase 11A bridge teardown, observability) can react. The
		// EventCapsuleUpdated branch above already handled re-announce
		// on its orbit.
		previous := ""
		if event.Meta != nil {
			previous = event.Meta[capsule.MetaPreviousGroupID]
		}
		h.eventBus.Publish(events.CapsuleGroupReleased{
			BaseEvent:       events.NewBaseEvent(),
			CapsuleID:       event.CapsuleID.String(),
			CapsuleName:     event.Capsule.Spec.Name,
			Orbit:           event.Capsule.Spec.Orbit,
			ClusterPath:     event.Capsule.ClusterID,
			PreviousGroupID: previous,
		})
	}
}

// stopLocalReplicas stops and removes the runtime container for every
// replica of c that this node hosts (replica.NodeID == h.nodeID). It is
// the O8 fix: capsule deletion must tear the local container down, not
// just drop metadata and withdraw the orbit announcement.
//
// The stop is routed through runtimeGroupRollback.StopContainer — the
// SAME intentional-teardown method the group-rollback path uses. That
// method adds the container to the runtime handler's self-removal ignore
// set BEFORE issuing Stop/Remove, so the resulting Podman died/remove
// events are suppressed and this orderly teardown does NOT self-trigger
// the O2 crash/re-election path (CapsuleExecutionFailed).
//
// Idempotent: a replica whose container is already gone (deleted on
// another node, or never started locally) makes StopContainer return an
// error, which is logged at Warn and tolerated — deletion always
// completes even when the stop fails.
func (h *CapsuleHandler) stopLocalReplicas(c *capsule.Capsule) {
	if c == nil {
		return
	}

	h.mu.RLock()
	rollback := h.runtimeRollback
	grace := h.deleteStopGrace
	h.mu.RUnlock()

	// Without a runtime rollback hook (tests, runtime-disabled
	// deployments) there is no local container to stop.
	if rollback == nil {
		return
	}
	if grace <= 0 {
		grace = defaultDeleteStopGrace
	}

	for _, replica := range c.Replicas {
		if replica.NodeID != h.nodeID {
			continue
		}
		if err := rollback.StopContainer(c.ID.String(), string(replica.ReplicaID), grace); err != nil {
			// Tolerate: the container may already be gone (idempotent
			// teardown). Deletion must complete regardless.
			h.logger.Warn("failed to stop local container on capsule delete (often expected: container already gone)",
				zap.String("capsule", c.ID.String()),
				zap.String("replica", string(replica.ReplicaID)),
				zap.String("node", h.nodeID),
				zap.Error(err))
			continue
		}
		h.logger.Info("stopped local container for deleted capsule",
			zap.String("capsule", c.ID.String()),
			zap.String("replica", string(replica.ReplicaID)),
			zap.String("node", h.nodeID))
	}
}

// announceCapsule publishes a capsule announcement to its orbit. The
// originating node MUST be subscribed to that orbit's gossipsub topic
// before publishing — otherwise the underlying topic.Publish rejects
// with "not joined". Auto-join the orbit here on first announce so
// operators don't have to pre-declare an `orbits:` list in the daemon
// config just to create a capsule.
func (h *CapsuleHandler) announceCapsule(c *capsule.Capsule) {
	// Serialize from a race-safe snapshot, not the live event pointer. The
	// manager emits events carrying the LIVE *Capsule; the announce path
	// walks c.Replicas via replicaStatesToProto, which would race a
	// concurrent AssignReplica / SyncStatus that mutates that same slice
	// under the manager mutex. Manager.Get deep-copies Replicas under
	// m.mu.RLock — the sanctioned accessor for exactly this read. If the
	// capsule was deleted between emit and here, fall back to the live
	// pointer (the delete-announce/withdrawal path needs to fire even though
	// the row is gone; its Replicas are no longer being mutated).
	if snap := h.manager.Get(c.ID); snap != nil {
		c = snap
	}

	h.mu.RLock()
	ann, ok := h.announcers[c.ClusterID]
	orbitMgr := h.orbits[c.ClusterID]
	h.mu.RUnlock()
	if !ok {
		h.logger.Debug("no announcer for cluster, skipping announcement",
			zap.String("cluster", c.ClusterID),
			zap.String("capsule_id", c.ID.String()))
		return
	}

	// The control plane is auto-joined in SetupCluster; guard defensively
	// in case a capsule is announced before setup completed (e.g. store
	// replay on restart).
	if orbitMgr != nil && !orbitMgr.IsJoined(orbit.CapsuleControlOrbit) {
		if err := orbitMgr.Join(h.ctx, orbit.CapsuleControlOrbit); err != nil {
			h.logger.Error("failed to join capsule control plane before announce",
				zap.String("cluster", c.ClusterID),
				zap.String("capsule_id", c.ID.String()),
				zap.Error(err))
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Announce on the cluster-wide control plane so every node observes the
	// capsule and runs the gravity election. Spec.Orbit travels in the
	// payload as an affinity hint.
	if err := ann.AnnounceOn(ctx, c, orbit.CapsuleControlOrbit); err != nil {
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

// withdrawCapsule publishes a withdrawal to the orbit. Mirrors
// announceCapsule's auto-join behaviour: a capsule loaded from the
// store on daemon restart has no orbit subscription yet, so a delete
// in that state would fail to publish the withdrawal and leave zombie
// entries on peers. Auto-join the orbit before publishing so the
// withdrawal always lands.
func (h *CapsuleHandler) withdrawCapsule(c *capsule.Capsule, reason string) {
	h.mu.RLock()
	ann, ok := h.announcers[c.ClusterID]
	orbitMgr := h.orbits[c.ClusterID]
	h.mu.RUnlock()
	if !ok {
		return
	}

	// The control plane is auto-joined in SetupCluster; guard defensively
	// for the store-replay-on-restart path.
	if orbitMgr != nil && !orbitMgr.IsJoined(orbit.CapsuleControlOrbit) {
		if err := orbitMgr.Join(h.ctx, orbit.CapsuleControlOrbit); err != nil {
			h.logger.Error("failed to join capsule control plane before withdraw",
				zap.String("cluster", c.ClusterID),
				zap.String("capsule_id", c.ID.String()),
				zap.Error(err))
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := ann.Withdraw(ctx, orbit.CapsuleControlOrbit, c.ID, reason); err != nil {
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
		if err := h.manager.SyncStatus(id, enums.CapsuleStatus(update.Status)); err != nil {
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
	case enums.ScalingActionEnum.ScaleUp():
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
	case enums.ScalingActionEnum.ScaleDown():
		h.eventBus.Publish(events.ScaleDownNeeded{
			BaseEvent:   base,
			CapsuleID:   string(event.CapsuleID),
			RuleName:    event.RuleName,
			ClusterPath: clusterPath,
		})
	case enums.ScalingActionEnum.ScaleToZero():
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
		Status:    enums.CapsuleStatus(pb.Status),
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
			Status:    enums.CapsuleStatus(r.Status),
			IP:        r.Ip,
			Ports:     portBindingsFromProto(r.Ports),
		}
		if r.StartedAt != nil {
			rs.StartedAt = r.StartedAt.AsTime()
		}
		c.Replicas = append(c.Replicas, rs)
	}

	return c
}

// portBindingsFromProto reconstructs resolved per-replica host-port bindings
// from the gossip wire so a peer's `capsule get` shows the actual host ports a
// remote replica published.
func portBindingsFromProto(in []*capsulePb.PortBinding) []capsule.PortBinding {
	if len(in) == 0 {
		return nil
	}
	out := make([]capsule.PortBinding, 0, len(in))
	for _, b := range in {
		if b == nil {
			continue
		}
		out = append(out, capsule.PortBinding{
			Name:          b.Name,
			ContainerPort: uint16(b.ContainerPort),
			HostPort:      uint16(b.HostPort),
		})
	}
	return out
}

func protoToSpec(pb *capsulePb.CapsuleSpec) capsule.CapsuleSpec {
	spec := capsule.CapsuleSpec{
		Name:        pb.Name,
		Image:       pb.Image,
		ImageDigest: pb.ImageDigest,
		Orbit:       pb.Orbit,
		Tier:        enums.Tier(pb.Tier),
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
		spec.Runtime.StatsInterval = time.Duration(pb.Runtime.StatsIntervalSeconds) * time.Second
		if n := pb.Runtime.Network; n != nil {
			spec.Runtime.Network.Mode = enums.NetworkMode(n.Mode)
			for _, p := range n.Ports {
				spec.Runtime.Network.Ports = append(spec.Runtime.Network.Ports, capsule.PortMapping{
					Name:          p.Name,
					ContainerPort: uint16(p.ContainerPort),
					HostPort:      uint16(p.HostPort),
					Protocol:      p.Protocol,
				})
			}
		}
		if hc := pb.Runtime.HealthCheck; hc != nil {
			spec.Runtime.HealthCheck = &capsule.HealthCheck{
				Type:         enums.HealthCheckType(hc.Type),
				Path:         hc.Path,
				Port:         uint16(hc.Port),
				Interval:     time.Duration(hc.IntervalSeconds) * time.Second,
				Timeout:      time.Duration(hc.TimeoutSeconds) * time.Second,
				Retries:      int(hc.Retries),
				InitialDelay: time.Duration(hc.InitialDelaySeconds) * time.Second,
			}
		}
		if fp := pb.Runtime.FailurePolicy; fp != nil {
			spec.Runtime.FailurePolicy = capsule.FailurePolicy{
				RestartLimit:    int(fp.RestartLimit),
				MaxNodeAttempts: int(fp.MaxNodeAttempts),
				GracefulTimeout: time.Duration(fp.GracefulTimeoutSeconds) * time.Second,
			}
		}
		if lr := pb.Runtime.LogRetention; lr != nil {
			spec.Runtime.LogRetention = capsule.LogRetention{
				MaxFileSizeMB: int(lr.MaxFileSizeMb),
				MaxFiles:      int(lr.MaxFiles),
			}
		}
		if sn := pb.Runtime.Snapshot; sn != nil {
			spec.Runtime.SnapshotConfig = capsule.SnapshotConfig{
				MaxPerCapsule: int(sn.MaxPerCapsule),
				TTL:           time.Duration(sn.TtlSeconds) * time.Second,
			}
		}
		if rg := pb.Runtime.Registry; rg != nil {
			spec.Runtime.Registry = &capsule.RegistryAuth{
				URL:               rg.Url,
				UsernameEncrypted: rg.UsernameEncrypted,
				PasswordEncrypted: rg.PasswordEncrypted,
			}
		}
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
			Trigger:    enums.TriggerMode(rule.Trigger),
			Conditions: rule.Conditions,
			Action:     enums.ScalingAction(rule.Action),
			Cooldown:   time.Duration(rule.CooldownSeconds) * time.Second,
		})
	}

	for _, rule := range pb.PlacementRules {
		spec.PlacementRules = append(spec.PlacementRules, capsule.PlacementRule{
			Name:     rule.Name,
			Type:     enums.PlacementType(rule.Type),
			Mode:     enums.PlacementMode(rule.Mode),
			Names:    rule.TargetNames,
			Labels:   rule.Labels,
			Required: rule.Required,
		})
	}

	// Group fields (Phase 10). Unmapped wire values fall back to
	// Capsule kind / empty GroupSpec so receivers stay backward-
	// compatible with pre-Phase-10 announcements.
	spec.Kind = capsuleKindFromProto(pb.Kind)
	if pb.GroupId != "" {
		spec.GroupID = capsule.CapsuleID(pb.GroupId)
	}
	spec.GroupMember = pb.GroupMember
	if pb.Group != nil {
		gs := groupSpecFromProto(pb.Group)
		spec.Group = &gs
	}

	return spec
}

// capsuleKindFromProto maps a proto enum back to the Go CapsuleKind.
// UNSPECIFIED maps to Capsule for backward compatibility.
func capsuleKindFromProto(k capsulePb.CapsuleKind) capsule.CapsuleKind {
	switch k {
	case capsulePb.CapsuleKind_CAPSULE_KIND_GROUP:
		return capsule.CapsuleKindEnum.Group()
	default:
		return capsule.CapsuleKindEnum.Capsule()
	}
}

// colocationFromProto maps a proto enum back to the Go ColocationMode.
// UNSPECIFIED maps to SameOrbit (the documented default).
func colocationFromProto(m capsulePb.ColocationMode) capsule.ColocationMode {
	switch m {
	case capsulePb.ColocationMode_COLOCATION_MODE_SAME_NODE:
		return capsule.ColocationModeEnum.SameNode()
	default:
		return capsule.ColocationModeEnum.SameOrbit()
	}
}

// groupSpecFromProto reconstructs a Go GroupSpec from its proto shape.
// Members are converted via protoToSpec; the dependency map is merged
// onto each MemberSpec by name.
func groupSpecFromProto(pb *capsulePb.GroupSpec) capsule.GroupSpec {
	if pb == nil {
		return capsule.GroupSpec{}
	}
	g := capsule.GroupSpec{
		Colocation:    colocationFromProto(pb.Colocation),
		CascadeDelete: pb.CascadeDelete,
	}
	for _, mid := range pb.MemberIds {
		g.MemberIDs = append(g.MemberIDs, capsule.CapsuleID(mid))
	}
	for _, m := range pb.Members {
		ms := capsule.MemberSpec{
			Name: m.Name,
		}
		if m.Spec != nil {
			ms.Spec = protoToSpec(m.Spec)
		}
		if deps, ok := pb.Deps[m.Name]; ok && deps != nil {
			ms.DependsOn = append([]string(nil), deps.DependsOn...)
		}
		g.Members = append(g.Members, ms)
	}
	return g
}
