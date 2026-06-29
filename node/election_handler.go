package node

import (
	"context"
	"sync"

	"github.com/libp2p/go-libp2p/core/crypto"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/enums"
	"github.com/tareksalem/falak/election"
	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
)

// electionLifecycleAdapter bridges election.LifecycleController to the
// capsule.Manager's lifecycle methods. The adapter exists to keep the
// election package free of any direct dependency on capsule.Manager (it
// only knows about the narrow LifecycleController interface).
type electionLifecycleAdapter struct {
	manager *capsule.Manager
}

func (a *electionLifecycleAdapter) StartElection(id capsule.CapsuleID) error {
	return a.manager.StartElection(id)
}

func (a *electionLifecycleAdapter) WinElection(id capsule.CapsuleID) error {
	return a.manager.WinElection(id)
}

// WinElectionWithBinding drives the Win FSM transition (Electing →
// Assigned) and then records the replica→node binding durably via
// AssignReplica, returning the first error. The election manager calls
// this on the Won path BEFORE releasing its in-flight local claim slot so
// the binding is visible to gravity.NodesRunningCapsule the instant the
// slot frees — preserving multi-replica self-anti-affinity through durable
// state instead of a retained slot.
//
// AssignReplica is idempotent, so the redundant mirror performed by the
// handleElectionWon subscriber on the winning node is harmless.
func (a *electionLifecycleAdapter) WinElectionWithBinding(id capsule.CapsuleID, replicaID capsule.ReplicaID, nodeID string) error {
	if err := a.manager.WinElection(id); err != nil {
		return err
	}
	return a.manager.AssignReplica(id, replicaID, nodeID)
}

func (a *electionLifecycleAdapter) ElectionTimeout(id capsule.CapsuleID) error {
	return a.manager.ElectionTimeout(id)
}

// electionStoreAdapter implements election.CapsuleStore by delegating
// to capsule.Manager. Same rationale as the lifecycle adapter — keep
// election decoupled from the full capsule.Manager surface.
type electionStoreAdapter struct {
	manager *capsule.Manager
}

func (a *electionStoreAdapter) Get(id capsule.CapsuleID) *capsule.Capsule {
	return a.manager.Get(id)
}

// electionCapsuleLookup implements gravity.CapsuleTargetLookup by
// iterating the local capsule.Manager for capsules with the given name.
// A node ID appears in the result whenever the local manager believes
// the named capsule has a replica assigned to it.
//
// "Local" here means "this node's view of the cluster" — every node
// tracks all capsules (via orbit gossip) and all replica assignments
// (via election outcome bookkeeping). That makes the lookup sufficient
// for the self-anti-affinity check because the local node's own
// assignments are always observable without cross-node queries.
type electionCapsuleLookup struct {
	manager     *capsule.Manager
	localNodeID string
}

// NodesRunningCapsule returns the node IDs currently hosting any
// replica of the named capsule, scoped to the given cluster.
//
// ListSnapshot (not List) is used so the Replicas slice we iterate is a
// freshly-allocated copy. The live entry's slice header is being mutated
// under the manager mutex by AssignReplica; reading the same backing
// array here without coordination races under -race
// (TestCapsuleGroup_SameNode_RecoverOnNodeJoined, surfaced in 11A.15
// closeout, fixed under 11A.18 audit).
func (l *electionCapsuleLookup) NodesRunningCapsule(clusterPath, capsuleName string) []string {
	if l == nil || l.manager == nil {
		return nil
	}
	var out []string
	for _, c := range l.manager.ListSnapshot() {
		if c.ClusterID != clusterPath || c.Spec.Name != capsuleName {
			continue
		}
		for _, r := range c.Replicas {
			if r.NodeID != "" {
				out = append(out, r.NodeID)
			}
		}
	}
	return out
}

// electionEventSink implements election.EventSink by publishing the
// outcome events onto the node's internal event bus. The runtime module
// (and any future observers) subscribes to these events to know when a
// capsule is ready to start, when it lost its slot, or when its election
// failed entirely.
type electionEventSink struct {
	bus    events.Bus
	nodeID string
	logger *zap.Logger
}

func (s *electionEventSink) EmitWon(req election.Request, winnerNodeID string, score float64) {
	s.bus.Publish(events.ElectionWon{
		BaseEvent:   events.NewBaseEvent(),
		CapsuleID:   string(req.CapsuleID),
		ReplicaID:   req.ReplicaID,
		ClusterPath: req.ClusterPath,
		Score:       score,
		NodeID:      winnerNodeID,
	})
}

func (s *electionEventSink) EmitLost(req election.Request, winnerNodeID string) {
	s.bus.Publish(events.ElectionLost{
		BaseEvent:    events.NewBaseEvent(),
		CapsuleID:    string(req.CapsuleID),
		ReplicaID:    req.ReplicaID,
		ClusterPath:  req.ClusterPath,
		WinnerNodeID: winnerNodeID,
	})
}

func (s *electionEventSink) EmitFailed(req election.Request, reason string) {
	s.bus.Publish(events.ElectionFailed{
		BaseEvent:   events.NewBaseEvent(),
		CapsuleID:   string(req.CapsuleID),
		ReplicaID:   req.ReplicaID,
		ClusterPath: req.ClusterPath,
		Reason:      reason,
	})
}

// groupClaimSinkAdapter implements election.GroupClaimSink by publishing
// the outcome events onto the node's internal event bus. The runtime
// handler subscribes to events.GroupClaimWon to start the group's
// members in topological order on the winning node; observability
// subscribers consume Lost/Failed.
type groupClaimSinkAdapter struct {
	bus    events.Bus
	logger *zap.Logger
}

// EmitGroupWon publishes a GroupClaimWon event on the node bus.
func (s *groupClaimSinkAdapter) EmitGroupWon(req election.GroupClaimRequest, nodeID string, score float64) {
	memberIDs := make([]string, 0, len(req.MemberIDs))
	for _, m := range req.MemberIDs {
		memberIDs = append(memberIDs, string(m))
	}
	s.bus.Publish(events.GroupClaimWon{
		BaseEvent:   events.NewBaseEvent(),
		GroupID:     string(req.GroupID),
		ClusterPath: req.ClusterPath,
		MemberIDs:   memberIDs,
		NodeID:      nodeID,
		Score:       score,
	})
}

// EmitGroupLost publishes a GroupClaimLost event on the node bus.
func (s *groupClaimSinkAdapter) EmitGroupLost(req election.GroupClaimRequest, nodeID string) {
	s.bus.Publish(events.GroupClaimLost{
		BaseEvent:    events.NewBaseEvent(),
		GroupID:      string(req.GroupID),
		ClusterPath:  req.ClusterPath,
		WinnerNodeID: nodeID,
	})
}

// EmitGroupFailed publishes a GroupClaimFailed event on the node bus.
func (s *groupClaimSinkAdapter) EmitGroupFailed(req election.GroupClaimRequest, reason string) {
	s.bus.Publish(events.GroupClaimFailed{
		BaseEvent:   events.NewBaseEvent(),
		GroupID:     string(req.GroupID),
		ClusterPath: req.ClusterPath,
		Reason:      reason,
	})
}

// NewGroupClaimSinkAdapter constructs the GroupClaimSink for wiring to
// election.Manager. Exported so the node's main wiring can install it
// via election.WithGroupClaimSink.
func NewGroupClaimSinkAdapter(bus events.Bus, logger *zap.Logger) election.GroupClaimSink {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &groupClaimSinkAdapter{bus: bus, logger: logger}
}

// electionSigner implements election.Signer using the local node's
// libp2p private key. Identical wire-level scheme to the orbit signer
// (length-prefixed canonical bytes) — only the message format differs.
type electionSigner struct {
	key crypto.PrivKey
}

func (s *electionSigner) Sign(content []byte) ([]byte, error) {
	return s.key.Sign(content)
}

// electionVerifier implements election.Verifier by looking up sender
// public keys in the phonebook. Returns false on any failure (unknown
// sender, unmarshalable key, signature mismatch).
type electionVerifier struct {
	pb phonebook.IPhonebook
}

func (v *electionVerifier) Verify(senderID string, content, signature []byte) bool {
	if len(signature) == 0 {
		return false
	}
	// The verifier does not know which cluster a message belongs to from
	// the canonical content alone, so it iterates phonebook lookups
	// across known clusters until one succeeds. In practice this is a
	// single iteration: most nodes are in one cluster, and the entry
	// for a sender will be present in the cluster that received the
	// message. The phonebook's GetByNode helper returns every entry for
	// a given node ID across all clusters.
	entries, err := v.pb.GetByNode(senderID)
	if err != nil || len(entries) == 0 {
		return false
	}
	for _, entry := range entries {
		pubKey, err := crypto.UnmarshalPublicKey(entry.PublicKey)
		if err != nil {
			continue
		}
		ok, err := pubKey.Verify(content, signature)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// ElectionHandler wires the election.Manager into the node's event bus,
// subscribing to events.ElectionRequested and dispatching each event
// onto the manager. It is a thin adapter — all real work happens in the
// election package itself.
//
// The handler also owns the goroutine lifecycle for its event subscription,
// stopping cleanly when the node shuts down.
type ElectionHandler struct {
	mgr     *election.Manager
	bus     events.Bus
	logger  *zap.Logger
	capsMgr *capsule.Manager // optional; used to clear group reservations

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ElectionHandlerOption configures an ElectionHandler.
type ElectionHandlerOption func(*ElectionHandler)

// WithElectionHandlerLogger sets the logger.
func WithElectionHandlerLogger(logger *zap.Logger) ElectionHandlerOption {
	return func(h *ElectionHandler) { h.logger = logger }
}

// WithElectionHandlerCapsuleManager wires the capsule manager used to
// resolve group membership when CapsuleRunning events fire. The
// handler uses it to detect when every member of a same-node group has
// reached Running so the group reservation can be cleared before the
// image-pull deadline elapses. When unset, the handler simply skips
// the reservation-clear path; group reservations then rely on the
// reservation watchdog's failure-emission semantics.
func WithElectionHandlerCapsuleManager(m *capsule.Manager) ElectionHandlerOption {
	return func(h *ElectionHandler) { h.capsMgr = m }
}

// NewElectionHandler constructs a handler bound to the given manager
// and event bus. Both are required.
func NewElectionHandler(mgr *election.Manager, bus events.Bus, opts ...ElectionHandlerOption) *ElectionHandler {
	h := &ElectionHandler{
		mgr:    mgr,
		bus:    bus,
		logger: zap.NewNop(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Start subscribes to election events and begins dispatching. Idempotent.
func (h *ElectionHandler) Start(parent context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		return
	}
	h.ctx, h.cancel = context.WithCancel(parent)

	h.wg.Add(4)
	go func() {
		defer h.wg.Done()
		h.loop()
	}()
	go func() {
		defer h.wg.Done()
		h.groupLoop()
	}()
	go func() {
		defer h.wg.Done()
		h.runningLoop()
	}()
	go func() {
		defer h.wg.Done()
		h.pullProgressLoop()
	}()
	h.logger.Info("election handler started")
}

// Stop cancels the dispatch goroutine and waits for it to exit. Idempotent.
func (h *ElectionHandler) Stop() {
	h.mu.Lock()
	if h.cancel == nil {
		h.mu.Unlock()
		return
	}
	h.cancel()
	h.cancel = nil
	h.mu.Unlock()

	h.wg.Wait()
	h.logger.Info("election handler stopped")
}

// loop is the dispatch goroutine. It subscribes to ElectionRequested
// events on the bus and forwards each one to the manager. The loop
// terminates when the context is cancelled.
func (h *ElectionHandler) loop() {
	ch := h.bus.Subscribe(events.TypeElectionRequested)
	for {
		select {
		case <-h.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			req, ok := ev.(events.ElectionRequested)
			if !ok {
				continue
			}
			h.dispatch(req)
		}
	}
}

// dispatch translates an ElectionRequested event into an election.Request
// and forwards it to the manager. Errors are logged at warn level — the
// election won't run, but the dispatch loop continues.
func (h *ElectionHandler) dispatch(ev events.ElectionRequested) {
	req := election.Request{
		CapsuleID:      capsule.CapsuleID(ev.CapsuleID),
		ReplicaID:      ev.ReplicaID,
		ClusterPath:    ev.ClusterPath,
		Reason:         election.Reason(ev.Reason),
		PreviousNodeID: ev.PreviousNodeID,
		Priority:       ev.Priority,
		CreatedAt:      ev.OccurredAt,
	}
	if err := h.mgr.HandleRequest(req); err != nil {
		h.logger.Warn("election dispatch failed",
			zap.String("capsule_id", ev.CapsuleID),
			zap.String("replica_id", ev.ReplicaID),
			zap.Error(err))
	}
}

// groupLoop subscribes to GroupClaimRequested and GroupReelectionRequested
// events on the bus and translates each one into a GroupClaimRequest for
// the election manager. The loop terminates when the context is cancelled.
func (h *ElectionHandler) groupLoop() {
	claimCh := h.bus.Subscribe(events.TypeGroupClaimRequested)
	reelectCh := h.bus.Subscribe(events.TypeGroupReelectionRequested)
	for {
		select {
		case <-h.ctx.Done():
			return
		case ev, ok := <-claimCh:
			if !ok {
				return
			}
			req, ok := ev.(events.GroupClaimRequested)
			if !ok {
				continue
			}
			h.dispatchGroup(req)
		case ev, ok := <-reelectCh:
			if !ok {
				return
			}
			rev, ok := ev.(events.GroupReelectionRequested)
			if !ok {
				continue
			}
			h.dispatchGroupReelection(rev)
		}
	}
}

// dispatchGroup translates a GroupClaimRequested event into an
// election.GroupClaimRequest and forwards it to the manager.
func (h *ElectionHandler) dispatchGroup(ev events.GroupClaimRequested) {
	memberIDs := make([]capsule.CapsuleID, 0, len(ev.MemberIDs))
	for _, m := range ev.MemberIDs {
		memberIDs = append(memberIDs, capsule.CapsuleID(m))
	}
	req := election.GroupClaimRequest{
		GroupID:      capsule.CapsuleID(ev.GroupID),
		MemberIDs:    memberIDs,
		ClusterPath:  ev.ClusterPath,
		Reason:       election.Reason(ev.Reason),
		ExcludeNodes: append([]string(nil), ev.ExcludeNodes...),
		Priority:     ev.Priority,
		CreatedAt:    ev.OccurredAt,
	}
	if err := h.mgr.HandleGroupClaimRequest(req); err != nil {
		h.logger.Warn("group claim dispatch failed",
			zap.String("group_id", ev.GroupID),
			zap.String("cluster", ev.ClusterPath),
			zap.Error(err))
	}
}

// dispatchGroupReelection translates a GroupReelectionRequested event into
// a GroupClaimRequest. The exclude list is the union of FailedNodeID
// (when non-empty: the holder that just died or the rollback winner
// that just failed) and ExcludeNodes (cumulative history of prior
// failed placement rounds populated by the 10.17 NodeJoined recovery
// path). Behaves identically to dispatchGroup otherwise.
func (h *ElectionHandler) dispatchGroupReelection(ev events.GroupReelectionRequested) {
	memberIDs := make([]capsule.CapsuleID, 0, len(ev.MemberIDs))
	for _, m := range ev.MemberIDs {
		memberIDs = append(memberIDs, capsule.CapsuleID(m))
	}
	exclude := make([]string, 0, len(ev.ExcludeNodes)+1)
	seen := make(map[string]struct{}, len(ev.ExcludeNodes)+1)
	if ev.FailedNodeID != "" {
		exclude = append(exclude, ev.FailedNodeID)
		seen[ev.FailedNodeID] = struct{}{}
	}
	for _, id := range ev.ExcludeNodes {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		exclude = append(exclude, id)
	}
	req := election.GroupClaimRequest{
		GroupID:      capsule.CapsuleID(ev.GroupID),
		MemberIDs:    memberIDs,
		ClusterPath:  ev.ClusterPath,
		Reason:       election.ReasonEnum.NodeFailure(),
		ExcludeNodes: exclude,
		CreatedAt:    ev.OccurredAt,
	}
	if err := h.mgr.HandleGroupClaimRequest(req); err != nil {
		h.logger.Warn("group reelection dispatch failed",
			zap.String("group_id", ev.GroupID),
			zap.String("cluster", ev.ClusterPath),
			zap.String("failed_node", ev.FailedNodeID),
			zap.Error(err))
	}
}

// runningLoop subscribes to events.TypeCapsuleRunning and, when the
// running capsule is a group member whose siblings are all Running on
// the local view, clears the group's capacity reservation on the
// election manager. The clear is idempotent so duplicate Running
// events (one per member) collapse onto a single Clear call.
//
// Without this hook, the reservation watchdog would fire a spurious
// GroupClaimFailed verdict when its image-pull-derived deadline
// expired even though every member had already started.
func (h *ElectionHandler) runningLoop() {
	if h.capsMgr == nil {
		// Without a capsule manager we cannot resolve group membership
		// or sibling state. Drain the subscription anyway so the bus
		// does not accumulate undelivered events on this slot.
		ch := h.bus.Subscribe(events.TypeCapsuleRunning)
		for {
			select {
			case <-h.ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}
			}
		}
	}
	ch := h.bus.Subscribe(events.TypeCapsuleRunning)
	for {
		select {
		case <-h.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			running, ok := ev.(events.CapsuleRunning)
			if !ok {
				continue
			}
			h.onCapsuleRunning(running.CapsuleID)
		}
	}
}

// pullProgressLoop subscribes to events.TypePullProgress and forwards
// each heartbeat to election.Manager.OnPullProgress. The election
// manager keys reservations by group ID and extends the deadline on
// every heartbeat; calls for capsules with no active reservation are
// no-ops on the manager side.
//
// The loop is intentionally a thin pass-through — all rate-limiting
// and bookkeeping live inside the manager. This is also why
// OnPullProgress must be cheap and idempotent: a runtime emitting a
// heartbeat every 10s for a 3-member group sees 3x the call rate
// before any one member finishes its pull.
func (h *ElectionHandler) pullProgressLoop() {
	ch := h.bus.Subscribe(events.TypePullProgress)
	for {
		select {
		case <-h.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			progress, ok := ev.(events.PullProgress)
			if !ok {
				continue
			}
			if progress.GroupID == "" {
				// Standalone capsule pulls do not carry a reservation;
				// nothing for the election manager to extend.
				continue
			}
			h.mgr.OnPullProgress(capsule.CapsuleID(progress.GroupID), progress.CapsuleID)
		}
	}
}

// onCapsuleRunning checks whether the capsule belongs to a same-node
// group and, if every member of that group has reached Running on the
// local view, clears the group's capacity reservation. Standalone
// capsules and incomplete groups are silently skipped.
func (h *ElectionHandler) onCapsuleRunning(capsuleID string) {
	if h.capsMgr == nil {
		return
	}
	c := h.capsMgr.Get(capsule.CapsuleID(capsuleID))
	if c == nil || c.Spec.GroupID == "" {
		return
	}
	groupID := c.Spec.GroupID
	group, members := h.capsMgr.GetGroup(groupID)
	if group == nil || group.Spec.Group == nil {
		return
	}
	if group.Spec.Group.Colocation != capsule.ColocationModeEnum.SameNode() {
		// Only same-node groups carry a capacity reservation.
		return
	}
	if !h.mgr.HasReservation(groupID) {
		// No reservation to clear (already cleared, or this node never
		// won the group election).
		return
	}
	for _, m := range members {
		// Re-fetch via Get so reading m.Status does not race with
		// concurrent lifecycle transitions writing the same field on
		// the underlying capsule pointer.
		snap := h.capsMgr.Get(m.ID)
		if snap == nil || snap.Status != enums.CapsuleStatusEnum.Running() {
			return
		}
	}
	h.mgr.ClearGroupReservation(groupID)
	h.logger.Info("group reservation cleared after all members running",
		zap.String("group_id", groupID.String()),
		zap.Int("members", len(members)))
}
