package election

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/election/gravity"
	electionpb "github.com/tareksalem/falak/election/proto/electionpb"
)

// CapsuleStore is the minimal capsule lookup the election manager needs.
// Implemented by capsule.Manager via a thin adapter on the node side.
type CapsuleStore interface {
	Get(id capsule.CapsuleID) *capsule.Capsule
}

// LifecycleController exposes the lifecycle transitions the manager
// invokes when an election starts, completes, or fails.
type LifecycleController interface {
	StartElection(id capsule.CapsuleID) error
	WinElection(id capsule.CapsuleID) error
	ElectionTimeout(id capsule.CapsuleID) error
}

// EventSink receives lifecycle events emitted by the manager. The node
// bridges these onto the node event bus so the rest of the system
// (runtime, observability) can react.
type EventSink interface {
	EmitWon(req Request, winnerNodeID string, score float64)
	EmitLost(req Request, winnerNodeID string)
	EmitFailed(req Request, reason string)
}

// noopSink is the default sink — silently drops events.
type noopSink struct{}

func (noopSink) EmitWon(Request, string, float64) {}
func (noopSink) EmitLost(Request, string)         {}
func (noopSink) EmitFailed(Request, string)       {}

// StrategyRegistry maps cluster paths to the strategy they use.
//
// Every cluster uses the default strategy unless a per-cluster override
// has been installed via Set. The registry is consulted on every
// election request to find the right strategy.
type StrategyRegistry struct {
	mu         sync.RWMutex
	defaultS   Strategy
	perCluster map[string]Strategy
}

// NewStrategyRegistry creates a registry with the given default. The
// default is required: every election needs a strategy to dispatch to.
func NewStrategyRegistry(defaultStrategy Strategy) *StrategyRegistry {
	return &StrategyRegistry{
		defaultS:   defaultStrategy,
		perCluster: make(map[string]Strategy),
	}
}

// Set installs a cluster-specific strategy override.
func (r *StrategyRegistry) Set(clusterPath string, s Strategy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.perCluster[clusterPath] = s
}

// Get returns the strategy for a cluster, falling back to the default.
func (r *StrategyRegistry) Get(clusterPath string) Strategy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.perCluster[clusterPath]; ok {
		return s
	}
	return r.defaultS
}

// inflightKey identifies a single in-flight election so the manager can
// dedupe parallel requests for the same (capsule, replica).
type inflightKey struct {
	CapsuleID capsule.CapsuleID
	ReplicaID string
}

// inflightEntry tracks the state of one in-flight election.
type inflightEntry struct {
	cancel context.CancelFunc
}

// Manager is the central election orchestrator. It owns the cluster
// election topics, dispatches incoming requests to strategies, runs the
// distributed protocol (publish + tiebreak + timeout), and translates
// outcomes into capsule lifecycle transitions and event sink notifications.
//
// The manager is per-node. It joins one ClusterTopic per cluster the
// node has joined and cleans them up on Stop.
type Manager struct {
	mu        sync.Mutex
	registry  *StrategyRegistry
	store     CapsuleStore
	lifecycle LifecycleController
	sink      EventSink
	logger    *zap.Logger
	nodeID    string

	calculator     *gravity.Calculator
	provider       gravity.StateProvider
	perClusterCalc map[string]*gravity.Calculator

	// pubsub primitives
	ps       *pubsub.PubSub
	signer   Signer
	verifier Verifier
	topics   map[string]*ClusterTopic // clusterPath -> topic

	// timing
	publishTimeout         time.Duration
	electionTimeout        time.Duration
	tiebreakWindow         time.Duration
	perClusterTimeout      map[string]time.Duration

	inflight map[inflightKey]*inflightEntry

	// localClaims records which (cluster, capsule_name) pairs the local
	// node has already published a claim for. Multi-replica elections
	// fire in parallel; without a local guard, the best-fit node would
	// publish a claim for every replica simultaneously. After the first
	// successful publish this map marks the capsule as claimed so
	// subsequent rounds on the same node short-circuit to
	// waitForRemoteVerdict instead of publishing a duplicate claim.
	localClaims   map[string]bool
	localClaimsMu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithLogger sets the logger.
func WithLogger(logger *zap.Logger) ManagerOption {
	return func(m *Manager) { m.logger = logger }
}

// WithCapsuleStore wires the capsule lookup used to fetch capsule
// details when dispatching an election.
func WithCapsuleStore(s CapsuleStore) ManagerOption {
	return func(m *Manager) { m.store = s }
}

// WithLifecycleController wires the lifecycle hooks called on election
// start / win / failure.
func WithLifecycleController(lc LifecycleController) ManagerOption {
	return func(m *Manager) { m.lifecycle = lc }
}

// WithEventSink wires the sink for outcome events.
func WithEventSink(s EventSink) ManagerOption {
	return func(m *Manager) { m.sink = s }
}

// WithNodeID sets the local node ID.
func WithNodeID(id string) ManagerOption {
	return func(m *Manager) { m.nodeID = id }
}

// WithCalculator wires the gravity calculator used to score nodes.
func WithCalculator(c *gravity.Calculator) ManagerOption {
	return func(m *Manager) { m.calculator = c }
}

// WithStateProvider wires the gravity state provider used to fetch
// node snapshots.
func WithStateProvider(p gravity.StateProvider) ManagerOption {
	return func(m *Manager) { m.provider = p }
}

// WithPubSub wires the libp2p pubsub instance for opening election topics.
func WithPubSub(ps *pubsub.PubSub) ManagerOption {
	return func(m *Manager) { m.ps = ps }
}

// WithSigner wires the message signer.
func WithSigner(s Signer) ManagerOption {
	return func(m *Manager) { m.signer = s }
}

// WithVerifier wires the message verifier.
func WithVerifier(v Verifier) ManagerOption {
	return func(m *Manager) { m.verifier = v }
}

// WithPublishTimeout sets the upper bound on a single Publish call.
func WithPublishTimeout(d time.Duration) ManagerOption {
	return func(m *Manager) { m.publishTimeout = d }
}

// WithElectionTimeout sets how long the manager waits for a winner
// before declaring an election failed. Configurable per-cluster in the
// future; for v1 it's process-wide.
func WithElectionTimeout(d time.Duration) ManagerOption {
	return func(m *Manager) { m.electionTimeout = d }
}

// WithTiebreakWindow sets how long the local winner waits after
// publishing its claim to see if a better remote claim arrives.
func WithTiebreakWindow(d time.Duration) ManagerOption {
	return func(m *Manager) { m.tiebreakWindow = d }
}

// NewManager constructs a Manager with the given default strategy and
// options.
func NewManager(defaultStrategy Strategy, opts ...ManagerOption) *Manager {
	m := &Manager{
		registry:          NewStrategyRegistry(defaultStrategy),
		logger:            zap.NewNop(),
		sink:              noopSink{},
		topics:            make(map[string]*ClusterTopic),
		inflight:          make(map[inflightKey]*inflightEntry),
		localClaims:       make(map[string]bool),
		perClusterCalc:    make(map[string]*gravity.Calculator),
		perClusterTimeout: make(map[string]time.Duration),
		publishTimeout:    3 * time.Second,
		electionTimeout:   10 * time.Second,
		tiebreakWindow:    300 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// SetClusterCalculator installs a per-cluster gravity calculator. Used
// by the node-side config wiring to apply cluster-specific weight
// overrides from CUE configuration. Cluster-specific calculators fall
// back to the default when not set.
func (m *Manager) SetClusterCalculator(clusterPath string, calc *gravity.Calculator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if calc == nil {
		delete(m.perClusterCalc, clusterPath)
		m.logger.Debug("cluster calculator override removed",
			zap.String("cluster", clusterPath))
		return
	}
	m.perClusterCalc[clusterPath] = calc
	m.logger.Info("cluster calculator override installed",
		zap.String("cluster", clusterPath))
}

// SetClusterTimeout installs a per-cluster election timeout override.
// Used by CUE config wiring. Clusters without an override use the
// manager-wide default.
func (m *Manager) SetClusterTimeout(clusterPath string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d <= 0 {
		delete(m.perClusterTimeout, clusterPath)
		m.logger.Debug("cluster timeout override removed",
			zap.String("cluster", clusterPath))
		return
	}
	m.perClusterTimeout[clusterPath] = d
	m.logger.Info("cluster timeout override installed",
		zap.String("cluster", clusterPath),
		zap.Duration("timeout", d))
}

// calculatorFor returns the calculator to use for an election in the
// given cluster: the per-cluster override if set, otherwise the default.
func (m *Manager) calculatorFor(clusterPath string) *gravity.Calculator {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.perClusterCalc[clusterPath]; ok {
		return c
	}
	return m.calculator
}

// timeoutFor returns the election timeout to use for a given cluster.
func (m *Manager) timeoutFor(clusterPath string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.perClusterTimeout[clusterPath]; ok {
		return d
	}
	return m.electionTimeout
}

// Registry exposes the strategy registry so cluster joins can install
// per-cluster overrides.
func (m *Manager) Registry() *StrategyRegistry { return m.registry }

// Start prepares the manager for handling requests. Must be called
// before HandleRequest.
func (m *Manager) Start(parent context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx != nil {
		return
	}
	m.ctx, m.cancel = context.WithCancel(parent)
	m.logger.Info("election manager started",
		zap.String("default_strategy", m.registry.defaultS.Name()),
		zap.Duration("election_timeout", m.electionTimeout),
		zap.Duration("tiebreak_window", m.tiebreakWindow))
}

// JoinCluster opens the election topic for a cluster. Called by the
// node bridge after the cluster has been joined and authenticated.
func (m *Manager) JoinCluster(clusterPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil {
		return errors.New("election manager: not started")
	}
	if _, ok := m.topics[clusterPath]; ok {
		m.logger.Debug("election cluster already joined", zap.String("cluster", clusterPath))
		return nil
	}
	if m.ps == nil {
		return errors.New("election manager: pubsub not configured")
	}
	t, err := NewClusterTopic(m.ctx, clusterPath, m.nodeID, m.ps, m.signer, m.verifier, m.logger.Named("topic"))
	if err != nil {
		m.logger.Error("election manager failed to join cluster",
			zap.String("cluster", clusterPath),
			zap.Error(err))
		return fmt.Errorf("election manager: join cluster %s: %w", clusterPath, err)
	}
	m.topics[clusterPath] = t
	strategyName := m.registry.Get(clusterPath).Name()
	m.logger.Info("election cluster joined",
		zap.String("cluster", clusterPath),
		zap.String("strategy", strategyName),
		zap.Duration("timeout", m.timeoutForLocked(clusterPath)))
	return nil
}

// timeoutForLocked returns the election timeout to use for a given
// cluster. Caller must hold m.mu.
func (m *Manager) timeoutForLocked(clusterPath string) time.Duration {
	if d, ok := m.perClusterTimeout[clusterPath]; ok {
		return d
	}
	return m.electionTimeout
}

// LeaveCluster closes the election topic for a cluster. Idempotent.
func (m *Manager) LeaveCluster(clusterPath string) {
	m.mu.Lock()
	t, ok := m.topics[clusterPath]
	if ok {
		delete(m.topics, clusterPath)
	}
	m.mu.Unlock()
	if t != nil {
		t.Stop()
		m.logger.Info("election cluster left", zap.String("cluster", clusterPath))
	}
}

// Stop cancels every in-flight election, waits for them to exit, and
// closes every cluster topic. Idempotent.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	for _, e := range m.inflight {
		e.cancel()
	}
	m.inflight = make(map[inflightKey]*inflightEntry)
	topics := make([]*ClusterTopic, 0, len(m.topics))
	for _, t := range m.topics {
		topics = append(topics, t)
	}
	m.topics = make(map[string]*ClusterTopic)
	m.mu.Unlock()

	m.wg.Wait()
	for _, t := range topics {
		t.Stop()
	}
	m.logger.Info("election manager stopped")
}

// HandleRequest dispatches an election request. Multiple requests for
// the same (capsule, replica) collapse onto one round.
func (m *Manager) HandleRequest(req Request) error {
	if m.ctx == nil {
		return errors.New("election manager: not started")
	}
	if m.store == nil || m.lifecycle == nil || m.calculator == nil || m.provider == nil {
		return errors.New("election manager: missing required dependency (store, lifecycle, calculator, or provider)")
	}
	c := m.store.Get(req.CapsuleID)
	if c == nil {
		return fmt.Errorf("election manager: capsule %s not found", req.CapsuleID)
	}

	key := inflightKey{CapsuleID: req.CapsuleID, ReplicaID: req.ReplicaID}
	m.mu.Lock()
	if _, exists := m.inflight[key]; exists {
		m.mu.Unlock()
		m.logger.Debug("election already in flight, deduping",
			zap.String("capsule_id", string(req.CapsuleID)),
			zap.String("replica_id", req.ReplicaID))
		return nil
	}
	topic, ok := m.topics[req.ClusterPath]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("election manager: cluster %s not joined", req.ClusterPath)
	}
	strategy := m.registry.Get(req.ClusterPath)
	if strategy == nil {
		m.mu.Unlock()
		return errors.New("election manager: no strategy configured")
	}

	roundCtx, roundCancel := context.WithCancel(m.ctx)
	m.inflight[key] = &inflightEntry{cancel: roundCancel}
	m.mu.Unlock()

	if err := m.lifecycle.StartElection(req.CapsuleID); err != nil {
		m.logger.Debug("StartElection rejected (already past Announced?)",
			zap.String("capsule_id", string(req.CapsuleID)),
			zap.Error(err))
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer roundCancel()
		defer m.removeInflight(key)
		m.runElection(roundCtx, req, c, strategy, topic)
	}()
	return nil
}

// runElection drives one election round end-to-end:
//
//  1. Subscribe to topic messages for this (capsule, replica).
//  2. Ask the strategy for a Decision.
//  3. If ineligible, wait for a remote claim or timeout.
//  4. If eligible, wait until PublishAt (cancelled by a winning remote
//     claim if one arrives first), then publish our claim.
//  5. After publishing, wait the tiebreak window for any better claim.
//  6. Report the final outcome.
func (m *Manager) runElection(ctx context.Context, req Request, c *capsule.Capsule, strategy Strategy, topic *ClusterTopic) {
	m.logger.Info("election round started",
		zap.String("capsule_id", string(req.CapsuleID)),
		zap.String("replica_id", req.ReplicaID),
		zap.String("cluster", req.ClusterPath),
		zap.String("strategy", strategy.Name()),
		zap.String("reason", string(req.Reason)))

	claimsCh, failuresCh, cancelListen := topic.Listen(string(req.CapsuleID), req.ReplicaID, 16)
	defer cancelListen()

	calc := m.calculatorFor(req.ClusterPath)
	decision := strategy.Decide(ctx, req, c, calc, m.provider)

	m.logger.Debug("strategy decided",
		zap.String("capsule_id", string(req.CapsuleID)),
		zap.String("replica_id", req.ReplicaID),
		zap.String("strategy", strategy.Name()),
		zap.Bool("eligible", decision.Eligible),
		zap.Float64("score", decision.Score),
		zap.Time("publish_at", decision.PublishAt),
		zap.String("reason", decision.Reason))

	deadline := time.Now().Add(m.timeoutFor(req.ClusterPath))

	if !decision.Eligible {
		m.waitForRemoteVerdict(ctx, req, deadline, claimsCh, failuresCh)
		return
	}

	// Self-anti-affinity short-circuit for multi-replica capsules. If
	// this node has already published a claim for another replica of
	// the same capsule in this manager's lifetime, give up and wait
	// for the remote verdict. This guard is what prevents the
	// best-fit node from winning every replica of a multi-replica
	// capsule in a parallel fire.
	if m.hasLocalClaim(req.CapsuleID) {
		m.logger.Debug("local node already claimed this capsule; stepping aside",
			zap.String("capsule_id", string(req.CapsuleID)),
			zap.String("replica_id", req.ReplicaID))
		m.waitForRemoteVerdict(ctx, req, deadline, claimsCh, failuresCh)
		return
	}

	// Ours is eligible — wait until PublishAt, but if a better remote
	// claim arrives first we step aside.
	wait := time.Until(decision.PublishAt)
	if wait > 0 {
		select {
		case <-ctx.Done():
			m.report(req, OutcomeEnum.Failed(), "", 0, "context cancelled")
			return
		case f := <-failuresCh:
			m.report(req, OutcomeEnum.Failed(), "", 0, "remote failure: "+f.Reason)
			return
		case rival := <-claimsCh:
			if isBetter(rival, decision, m.nodeID) {
				m.report(req, OutcomeEnum.Lost(), rival.NodeId, rival.GravityScore, "")
				return
			}
			// We still believe we win — fall through and publish anyway.
		case <-time.After(wait):
		case <-time.After(time.Until(deadline)):
			m.report(req, OutcomeEnum.Failed(), "", 0, "election timeout")
			return
		}
	}

	// Reserve the local claim slot before publishing. If another
	// goroutine beat us to it, fall back to waitForRemoteVerdict —
	// that round will see our own claim and resolve as OutcomeLost.
	if !m.tryClaimCapsule(req.CapsuleID) {
		m.logger.Debug("local claim slot taken while waiting; stepping aside",
			zap.String("capsule_id", string(req.CapsuleID)),
			zap.String("replica_id", req.ReplicaID))
		m.waitForRemoteVerdict(ctx, req, deadline, claimsCh, failuresCh)
		return
	}

	publishedAt := time.Now()
	claim := &electionpb.Claim{
		CapsuleId:       string(req.CapsuleID),
		ReplicaId:       req.ReplicaID,
		ClusterPath:     req.ClusterPath,
		NodeId:          m.nodeID,
		GravityScore:    decision.Score,
		TimestampMicros: publishedAt.UnixMicro(),
	}
	pubCtx, pubCancel := context.WithTimeout(ctx, m.publishTimeout)
	if err := topic.PublishClaim(pubCtx, claim); err != nil {
		pubCancel()
		// Release the slot so the next scheduled round for this
		// capsule on this node can retry.
		m.releaseCapsuleClaim(req.CapsuleID)
		m.logger.Warn("election claim publish failed",
			zap.String("capsule_id", string(req.CapsuleID)),
			zap.String("replica_id", req.ReplicaID),
			zap.String("cluster", req.ClusterPath),
			zap.Error(err))
		m.report(req, OutcomeEnum.Failed(), "", 0, "publish failed: "+err.Error())
		return
	}
	pubCancel()
	m.logger.Debug("election claim published",
		zap.String("capsule_id", string(req.CapsuleID)),
		zap.String("replica_id", req.ReplicaID),
		zap.String("cluster", req.ClusterPath),
		zap.Float64("score", decision.Score),
		zap.Time("published_at", publishedAt))

	// Tiebreak window: wait briefly for a competing claim. Anything
	// arriving in this window that beats us via the deterministic rule
	// makes us a loser instead.
	tiebreakDeadline := publishedAt.Add(m.tiebreakWindow)
	for {
		remaining := time.Until(tiebreakDeadline)
		if remaining <= 0 {
			m.report(req, OutcomeEnum.Won(), m.nodeID, decision.Score, "")
			return
		}
		select {
		case <-ctx.Done():
			m.report(req, OutcomeEnum.Failed(), "", 0, "context cancelled")
			return
		case rival := <-claimsCh:
			if isBetter(rival, decision, m.nodeID) {
				// We published but lost to a better rival. Release
				// the local claim slot so a later round (if any) can
				// retry on this node.
				m.releaseCapsuleClaim(req.CapsuleID)
				m.report(req, OutcomeEnum.Lost(), rival.NodeId, rival.GravityScore, "")
				return
			}
			// Worse rival; ignore and keep waiting.
		case <-time.After(remaining):
			m.report(req, OutcomeEnum.Won(), m.nodeID, decision.Score, "")
			return
		}
	}
}

// waitForRemoteVerdict is the path taken by ineligible nodes (and nodes
// that lost the strategy decision but still want to mirror the cluster
// outcome). It waits for a Claim or ElectionFailed message to arrive
// before the election timeout expires.
func (m *Manager) waitForRemoteVerdict(
	ctx context.Context,
	req Request,
	deadline time.Time,
	claims <-chan *electionpb.Claim,
	failures <-chan *electionpb.ElectionFailed,
) {
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			m.report(req, OutcomeEnum.Failed(), "", 0, "election timeout (no claim heard)")
			return
		}
		select {
		case <-ctx.Done():
			m.report(req, OutcomeEnum.Failed(), "", 0, "context cancelled")
			return
		case claim, ok := <-claims:
			if !ok {
				m.report(req, OutcomeEnum.Failed(), "", 0, "claim channel closed")
				return
			}
			m.report(req, OutcomeEnum.Lost(), claim.NodeId, claim.GravityScore, "")
			return
		case f, ok := <-failures:
			if !ok {
				continue
			}
			m.report(req, OutcomeEnum.Failed(), "", 0, "remote failure: "+f.Reason)
			return
		case <-time.After(remaining):
			m.report(req, OutcomeEnum.Failed(), "", 0, "election timeout (no claim heard)")
			return
		}
	}
}

// isBetter returns true when rival's claim should beat the local
// decision under the deterministic tiebreak: higher score wins, then
// earlier timestamp, then lexicographically smaller node ID.
func isBetter(rival *electionpb.Claim, ours Decision, ourNodeID string) bool {
	if rival.GravityScore != ours.Score {
		return rival.GravityScore > ours.Score
	}
	rivalTime := rival.TimestampMicros
	ourTime := ours.PublishAt.UnixMicro()
	if rivalTime != ourTime {
		return rivalTime < ourTime
	}
	return rival.NodeId < ourNodeID
}

// report finalizes an election with a verdict, calling the lifecycle
// transition and emitting the matching event.
//
// For OutcomeEnum.Failed() the manager also gossips an ElectionFailed message
// on the cluster election topic so every other node observes the same
// verdict. This is how distributed failure consensus is maintained:
// the first node to decide its round has failed publishes the failure,
// and every peer short-circuits its own in-flight election on receipt.
func (m *Manager) report(req Request, outcome Outcome, winner string, score float64, reason string) {
	switch outcome {
	case OutcomeEnum.Won():
		if err := m.lifecycle.WinElection(req.CapsuleID); err != nil {
			m.logger.Warn("WinElection failed",
				zap.String("capsule_id", string(req.CapsuleID)),
				zap.Error(err))
		}
		m.sink.EmitWon(req, winner, score)
		m.logger.Info("election won",
			zap.String("capsule_id", string(req.CapsuleID)),
			zap.String("replica_id", req.ReplicaID),
			zap.String("winner", winner),
			zap.Float64("score", score))

	case OutcomeEnum.Lost():
		m.sink.EmitLost(req, winner)
		m.logger.Info("election lost",
			zap.String("capsule_id", string(req.CapsuleID)),
			zap.String("replica_id", req.ReplicaID),
			zap.String("winner", winner))

	case OutcomeEnum.Failed():
		// Release the local claim slot so a retry (scale-up,
		// re-election after a transient failure) can participate on
		// this node again. Without this, a single publish error or
		// timeout permanently blocks the node from ever claiming
		// this capsule for the lifetime of the Manager.
		m.releaseCapsuleClaim(req.CapsuleID)

		if err := m.lifecycle.ElectionTimeout(req.CapsuleID); err != nil {
			m.logger.Debug("ElectionTimeout transition rejected",
				zap.String("capsule_id", string(req.CapsuleID)),
				zap.Error(err))
		}
		m.sink.EmitFailed(req, reason)
		m.publishFailureToCluster(req, reason)
		m.logger.Warn("election failed",
			zap.String("capsule_id", string(req.CapsuleID)),
			zap.String("replica_id", req.ReplicaID),
			zap.String("reason", reason))
	}
}

// publishFailureToCluster gossips an ElectionFailed message on the
// cluster's election topic so peer nodes learn about the failure and
// short-circuit their own in-flight election for this (capsule, replica).
//
// Failures are fire-and-forget — if the publish itself errors (topic
// closed, publish timeout), the log entry is all we produce. Peers that
// do not receive the gossip will eventually time out their own rounds
// and publish their own failure, so consensus still converges without
// retries in the caller.
func (m *Manager) publishFailureToCluster(req Request, reason string) {
	m.mu.Lock()
	topic, ok := m.topics[req.ClusterPath]
	m.mu.Unlock()
	if !ok {
		m.logger.Debug("no cluster topic to publish failure",
			zap.String("cluster", req.ClusterPath),
			zap.String("capsule_id", string(req.CapsuleID)))
		return
	}

	msg := &electionpb.ElectionFailed{
		CapsuleId:       string(req.CapsuleID),
		ReplicaId:       req.ReplicaID,
		ClusterPath:     req.ClusterPath,
		SenderId:        m.nodeID,
		Reason:          reason,
		TimestampMicros: time.Now().UnixMicro(),
	}

	pubCtx, cancel := context.WithTimeout(context.Background(), m.publishTimeout)
	defer cancel()
	if err := topic.PublishFailure(pubCtx, msg); err != nil {
		m.logger.Warn("failed to publish election failure",
			zap.String("capsule_id", string(req.CapsuleID)),
			zap.String("replica_id", req.ReplicaID),
			zap.Error(err))
	}
}

// removeInflight clears an in-flight entry once the election round
// finishes.
func (m *Manager) removeInflight(key inflightKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inflight, key)
}

// hasLocalClaim reports whether the local node has already published a
// claim for any replica of the given capsule since the manager started.
// It is used to short-circuit parallel multi-replica elections so a
// single node cannot win every slot.
func (m *Manager) hasLocalClaim(id capsule.CapsuleID) bool {
	m.localClaimsMu.Lock()
	defer m.localClaimsMu.Unlock()
	return m.localClaims[string(id)]
}

// tryClaimCapsule atomically sets the local claim flag for a capsule
// and returns true if this caller was the one who set it. Subsequent
// callers for the same capsule receive false and must fall through to
// waitForRemoteVerdict.
func (m *Manager) tryClaimCapsule(id capsule.CapsuleID) bool {
	m.localClaimsMu.Lock()
	defer m.localClaimsMu.Unlock()
	if m.localClaims[string(id)] {
		return false
	}
	m.localClaims[string(id)] = true
	return true
}

// releaseCapsuleClaim clears the local claim flag for a capsule so a
// later round can retry. Called when a publish fails or the node ends
// up losing the round after publishing (rival with better score).
func (m *Manager) releaseCapsuleClaim(id capsule.CapsuleID) {
	m.localClaimsMu.Lock()
	defer m.localClaimsMu.Unlock()
	delete(m.localClaims, string(id))
}

// ForgetCapsule removes every piece of Manager-local state associated
// with a capsule. Called by the node's CapsuleHandler when a capsule
// is deleted or withdrawn so the localClaims map does not grow without
// bound over long uptimes.
//
// It is safe to call for capsules the Manager never saw — unknown
// entries are silently ignored.
func (m *Manager) ForgetCapsule(id capsule.CapsuleID) {
	m.releaseCapsuleClaim(id)
}
