package node

import (
	"context"
	"sync"

	"github.com/libp2p/go-libp2p/core/crypto"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
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
func (l *electionCapsuleLookup) NodesRunningCapsule(clusterPath, capsuleName string) []string {
	if l == nil || l.manager == nil {
		return nil
	}
	var out []string
	for _, c := range l.manager.List() {
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
	mgr      *election.Manager
	bus      events.Bus
	logger   *zap.Logger

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

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.loop()
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
