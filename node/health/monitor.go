package health

import (
	"context"
	"math/rand"
	"sort"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/multiformats/go-multiaddr"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
	"github.com/tareksalem/falak/node/proto/healthpb"
)

// Default timing configuration. All overridable via functional options.
const (
	DefaultProtocolPeriod          = 2 * time.Second
	DefaultQuarantineCheckInterval = 5 * time.Second
	DefaultQuarantineProbeInterval = 10 * time.Second
	DefaultMaxResponders           = 2
	// DefaultPendingAuthGrace is how long a freshly-announced peer
	// stays in PendingAuth before SWIM auto-promotes it to Active.
	// 10s comfortably covers the typical 5-15s gossipsub mesh-formation
	// window without leaving a real failure undetected for long
	// (Bug #13).
	DefaultPendingAuthGrace = 10 * time.Second
)

// SessionRefresher is called when a successful probe occurs to refresh
// the auth session and prevent unnecessary re-authentication.
type SessionRefresher func(clusterPath string)

// Monitor implements the modified SWIM protocol for failure detection.
// Each protocol period, it picks one random active peer and pings it.
// On failure, it broadcasts a PingRequest for indirect probing by top-N responders.
// It also periodically publishes PingRequests for quarantined peers.
type Monitor struct {
	host        host.Host
	ps          *pubsub.PubSub
	phonebook   phonebook.IPhonebook
	privateKey  crypto.PrivKey
	eventBus    events.Bus
	logger      *zap.Logger
	clusterPath string

	// Sub-components
	pingHandler *PingHandler // Can be set externally via WithPingHandler or created in Start
	healthPS    *HealthPubSub
	scores      *ScoreTracker

	// Configurable timing
	protocolPeriod          time.Duration
	pingTimeout             time.Duration
	quarantineCheckInterval time.Duration
	quarantineProbeInterval time.Duration
	maxResponders           int
	pendingAuthGrace        time.Duration

	// Configurable score parameters
	scoreConfig ScoreConfig

	// Configurable PubSub parameters
	pubsubConfig HealthPubSubConfig

	// Optional callback to refresh auth session on successful probes
	sessionRefresher SessionRefresher

	// Lifecycle
	parentCtx context.Context
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// Option configures a Monitor.
type Option func(*Monitor)

// WithMonitorContext sets the parent context.
func WithMonitorContext(ctx context.Context) Option {
	return func(m *Monitor) { m.parentCtx = ctx }
}

// WithMonitorHost sets the libp2p host.
func WithMonitorHost(h host.Host) Option {
	return func(m *Monitor) { m.host = h }
}

// WithMonitorPubSub sets the PubSub instance.
func WithMonitorPubSub(ps *pubsub.PubSub) Option {
	return func(m *Monitor) { m.ps = ps }
}

// WithMonitorPhonebook sets the phonebook.
func WithMonitorPhonebook(pb phonebook.IPhonebook) Option {
	return func(m *Monitor) { m.phonebook = pb }
}

// WithMonitorPrivateKey sets the private key for signing.
func WithMonitorPrivateKey(key crypto.PrivKey) Option {
	return func(m *Monitor) { m.privateKey = key }
}

// WithMonitorEventBus sets the event bus.
func WithMonitorEventBus(bus events.Bus) Option {
	return func(m *Monitor) { m.eventBus = bus }
}

// WithMonitorLogger sets the logger.
func WithMonitorLogger(logger *zap.Logger) Option {
	return func(m *Monitor) { m.logger = logger }
}

// WithMonitorClusterPath sets the cluster path.
func WithMonitorClusterPath(clusterPath string) Option {
	return func(m *Monitor) { m.clusterPath = clusterPath }
}

// WithProtocolPeriod sets how often each node probes a random peer.
func WithProtocolPeriod(d time.Duration) Option {
	return func(m *Monitor) { m.protocolPeriod = d }
}

// WithPingTimeout sets the max wait time for a ping/ack exchange.
func WithPingTimeout(d time.Duration) Option {
	return func(m *Monitor) { m.pingTimeout = d }
}

// WithQuarantineCheckInterval sets how often to check quarantined nodes for timeout.
func WithQuarantineCheckInterval(d time.Duration) Option {
	return func(m *Monitor) { m.quarantineCheckInterval = d }
}

// WithQuarantineProbeInterval sets how often to publish PingRequests for quarantined peers.
func WithQuarantineProbeInterval(d time.Duration) Option {
	return func(m *Monitor) { m.quarantineProbeInterval = d }
}

// WithMaxResponders sets how many longest-lived nodes self-select as responders.
func WithMaxResponders(n int) Option {
	return func(m *Monitor) { m.maxResponders = n }
}

// WithPendingAuthGrace sets the duration a freshly-announced peer stays in
// the PendingAuth status before SWIM auto-promotes it to Active. Probes
// against PendingAuth peers are skipped during the grace window so the
// gossipsub mesh has time to form (Bug #13). Zero disables the skip.
func WithPendingAuthGrace(d time.Duration) Option {
	return func(m *Monitor) { m.pendingAuthGrace = d }
}

// WithScoreIncrement sets the score added per failed probe.
func WithScoreIncrement(v float64) Option {
	return func(m *Monitor) { m.scoreConfig.ScoreIncrement = v }
}

// WithSuspectedThreshold sets the score at which a node enters suspected state.
func WithSuspectedThreshold(v float64) Option {
	return func(m *Monitor) { m.scoreConfig.SuspectedThreshold = v }
}

// WithQuarantineThreshold sets the score at which a node is quarantined.
func WithQuarantineThreshold(v float64) Option {
	return func(m *Monitor) { m.scoreConfig.QuarantineThreshold = v }
}

// WithQuarantineTimeout sets how long a node stays quarantined before being marked failed.
func WithQuarantineTimeout(d time.Duration) Option {
	return func(m *Monitor) { m.scoreConfig.QuarantineTimeout = d }
}

// WithMaxMessageAge sets the maximum age for health PubSub messages (replay protection).
func WithMaxMessageAge(d time.Duration) Option {
	return func(m *Monitor) { m.pubsubConfig.MaxMessageAge = d }
}

// WithPublishMaxRetries sets the max retries for health message publishing.
func WithPublishMaxRetries(n int) Option {
	return func(m *Monitor) { m.pubsubConfig.PublishMaxRetries = n }
}

// WithPublishRetryDelay sets the delay between publish retries.
func WithPublishRetryDelay(d time.Duration) Option {
	return func(m *Monitor) { m.pubsubConfig.PublishRetryDelay = d }
}

// WithScaleBands sets custom cluster-size scaling bands for thresholds.
func WithScaleBands(bands []ClusterSizeScaleBand) Option {
	return func(m *Monitor) { m.scoreConfig.ScaleBands = bands }
}

// WithSessionRefresher sets a callback to refresh the auth session on successful probes.
func WithSessionRefresher(fn SessionRefresher) Option {
	return func(m *Monitor) { m.sessionRefresher = fn }
}

// WithPingHandler sets an externally created ping handler.
// When set, the monitor uses this handler instead of creating its own.
// The caller is responsible for registering/unregistering it on the host.
func WithPingHandler(ph *PingHandler) Option {
	return func(m *Monitor) { m.pingHandler = ph }
}

// NewMonitor creates a new health monitor with the given options.
func NewMonitor(opts ...Option) *Monitor {
	m := &Monitor{
		logger:                  zap.NewNop(),
		protocolPeriod:          DefaultProtocolPeriod,
		pingTimeout:             DefaultPingTimeout,
		quarantineCheckInterval: DefaultQuarantineCheckInterval,
		quarantineProbeInterval: DefaultQuarantineProbeInterval,
		maxResponders:           DefaultMaxResponders,
		pendingAuthGrace:        DefaultPendingAuthGrace,
		scoreConfig:             DefaultScoreConfig(),
		pubsubConfig:            DefaultHealthPubSubConfig(),
	}

	for _, opt := range opts {
		opt(m)
	}

	if m.parentCtx != nil {
		m.ctx, m.cancel = context.WithCancel(m.parentCtx)
	} else {
		m.ctx, m.cancel = context.WithCancel(context.Background())
	}

	return m
}

// Start initializes sub-components and starts the SWIM protocol loops.
func (m *Monitor) Start() error {
	if m.host == nil {
		return errRequired("host")
	}
	if m.ps == nil {
		return errRequired("pubsub")
	}
	if m.phonebook == nil {
		return errRequired("phonebook")
	}
	if m.privateKey == nil {
		return errRequired("privateKey")
	}
	if m.eventBus == nil {
		return errRequired("eventBus")
	}
	if m.clusterPath == "" {
		return errRequired("clusterPath")
	}

	// Create ping handler if not provided externally
	if m.pingHandler == nil {
		m.pingHandler = NewPingHandler(m.host, m.logger.Named("ping"), m.pingTimeout)
		m.pingHandler.Register()
	}

	// Create score tracker
	m.scores = NewScoreTracker(m.clusterPath, m.phonebook, m.eventBus, m.logger.Named("score"), m.scoreConfig)

	// Create and subscribe to health PubSub
	m.healthPS = NewHealthPubSub(
		m.host, m.ps, m.phonebook, m.privateKey,
		m.logger.Named("pubsub"), m.clusterPath, m.pubsubConfig,
	)
	m.healthPS.SetHandlers(m.onScoreUpdate, m.onPingRequest, m.onThresholdCrossed)

	if err := m.healthPS.Subscribe(m.ctx); err != nil {
		return err
	}

	// Start SWIM protocol loop
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.protocolLoop()
	}()

	// Start quarantine check loop
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.quarantineCheckLoop()
	}()

	// Start quarantine probe loop (publishes PingRequests for quarantined peers)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.quarantineProbeLoop()
	}()

	// Subscribe to local NodeDeparting events and broadcast them to
	// the health pubsub so peers can evict us instantly instead of
	// waiting for SWIM detection (~16-30s).
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.departureBroadcastLoop()
	}()

	// Subscribe to local NodeProbeResult events and persist them on the
	// phonebook entry so operators can inspect "last probe time" + "last
	// probe success" via `falak node health`. Without this the per-probe
	// telemetry is published into the void — see BUGS.md #22.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.probeResultPersistLoop()
	}()

	m.logger.Info("health monitor started",
		zap.String("cluster", m.clusterPath),
		zap.Duration("period", m.protocolPeriod),
		zap.Duration("pingTimeout", m.pingTimeout))

	return nil
}

// Stop stops the health monitor and cleans up.
// Note: if a PingHandler was provided externally via WithPingHandler,
// it is NOT unregistered here — the caller manages its lifecycle.
func (m *Monitor) Stop() {
	m.cancel()
	m.wg.Wait()

	if m.healthPS != nil {
		m.healthPS.Close()
	}

	m.logger.Debug("health monitor stopped", zap.String("cluster", m.clusterPath))
}

// --- SWIM Protocol Loop ---

// protocolLoop runs the SWIM protocol: each period, pick a random active peer and ping it.
func (m *Monitor) protocolLoop() {
	ticker := time.NewTicker(m.protocolPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.runProtocolPeriod()
		}
	}
}

// runProtocolPeriod picks one random active peer and probes it.
func (m *Monitor) runProtocolPeriod() {
	target := m.selectRandomActivePeer()
	if target == nil {
		return // No peers to probe
	}

	targetID, err := peer.Decode(target.NodeID)
	if err != nil {
		m.logger.Debug("invalid peer ID in phonebook", zap.String("nodeId", target.NodeID))
		return
	}

	// Ensure libp2p peerstore has the target's addresses so we can dial
	m.addPeerAddresses(targetID, target.Addresses)

	success, err := m.pingHandler.PingWithError(m.ctx, targetID)

	if success {
		// Per-probe Debug log so operators can answer "is SWIM actually
		// probing this peer?" without inferring from the event bus
		// (Bug #21). Score 0 is the steady state — only deviations are
		// interesting in aggregate, but a per-probe trace at Debug is
		// cheap and uniquely diagnostic.
		m.logger.Debug("probe ok",
			zap.String("target", target.NodeID),
			zap.String("cluster", m.clusterPath),
			zap.Float64("score", target.ReliabilityScore))
		m.onProbeSuccess(target.NodeID)
		return
	}

	// Ghost-entry shortcut: libp2p reports "dial to self" when the
	// target peer's multiaddrs collide with our own listen addresses.
	// That can only happen when a phonebook entry survived a prior
	// node identity at the same host:port. Treat it as eviction, not
	// a probe failure — otherwise SWIM would walk the ghost through
	// suspect→quarantine→fail and skew cluster-size thresholds the
	// whole time.
	// Mirror probe-ok with a probe-fail Debug so the trace shows both
	// sides of "is SWIM working here?" before the score machinery kicks
	// in (which only logs once a threshold is crossed).
	m.logger.Debug("probe failed",
		zap.String("target", target.NodeID),
		zap.String("cluster", m.clusterPath),
		zap.Float64("score", target.ReliabilityScore),
		zap.Error(err))

	if IsDialToSelfError(err) {
		m.logger.Info("evicting phonebook ghost (multiaddrs collide with local host)",
			zap.String("nodeId", target.NodeID),
			zap.String("cluster", m.clusterPath))
		if rmErr := m.phonebook.Remove(target.NodeID, m.clusterPath); rmErr != nil {
			m.logger.Warn("failed to evict phonebook ghost",
				zap.String("nodeId", target.NodeID),
				zap.Error(rmErr))
		}
		return
	}

	m.onDirectPingFail(target.NodeID)
}

// selectRandomProbePeer picks a random peer to probe from the phonebook.
// Includes active and suspected peers — suspected peers need continued probing
// to either recover or reach quarantine. Excludes quarantined (handled by
// quarantine probe loop) and failed peers.
func (m *Monitor) selectRandomActivePeer() *phonebook.Entry {
	peers, err := m.phonebook.GetByCluster(m.clusterPath)
	if err != nil || len(peers) == 0 {
		return nil
	}

	selfID := m.host.ID().String()
	var candidates []*phonebook.Entry
	for _, p := range peers {
		if p.NodeID == selfID {
			continue
		}
		if p.Status == phonebook.NodeStatusEnum.Quarantined() ||
			p.Status == phonebook.NodeStatusEnum.Failed() ||
			p.Status == phonebook.NodeStatusEnum.Departed() {
			// Departed peers said goodbye voluntarily — don't pester
			// them with probes. The libp2p Notifiee reactivates them
			// on reconnect.
			continue
		}
		if p.Status == phonebook.NodeStatusEnum.PendingAuth() {
			// Skip probes while the auth handshake / gossipsub mesh is
			// still settling (Bug #13). Auto-promote to Active once the
			// grace window elapses so a stuck PendingAuth marker doesn't
			// leave a peer permanently un-probed.
			if m.pendingAuthGrace > 0 && time.Since(p.UpdatedAt) < m.pendingAuthGrace {
				continue
			}
			if err := m.phonebook.SetStatus(p.NodeID, m.clusterPath, phonebook.NodeStatusEnum.Active()); err != nil {
				m.logger.Debug("failed to auto-promote pending-auth peer",
					zap.String("peer", p.NodeID),
					zap.Error(err))
				continue
			}
			m.logger.Debug("pending-auth grace elapsed, promoting to active",
				zap.String("peer", p.NodeID),
				zap.String("cluster", m.clusterPath),
				zap.Duration("waited", time.Since(p.UpdatedAt)))
			p.Status = phonebook.NodeStatusEnum.Active()
		}
		candidates = append(candidates, p)
	}

	if len(candidates) == 0 {
		return nil
	}

	return candidates[rand.Intn(len(candidates))]
}

// onProbeSuccess handles a successful direct ping.
func (m *Monitor) onProbeSuccess(nodeID string) {
	m.scores.RecordSuccess(nodeID, "direct")

	// Refresh auth session to prevent unnecessary re-auth
	if m.sessionRefresher != nil {
		m.sessionRefresher(m.clusterPath)
	}

	// Publish score update (alive=true resets score for all listeners)
	m.healthPS.PublishScoreUpdate(m.ctx, &healthpb.ScoreUpdate{
		TargetNodeId: nodeID,
		Score:        0,
		Alive:        true,
		UpdatedBy:    m.host.ID().String(),
		Timestamp:    timestamppb.Now(),
		Reason:       "direct_ping_success",
	})
}

// onDirectPingFail handles a failed direct ping. Increments score, publishes
// ScoreUpdate + PingRequest for indirect probing by top-N responders.
func (m *Monitor) onDirectPingFail(nodeID string) {
	newScore := m.scores.RecordFailure(nodeID, "direct_ping_fail")

	// Publish score update
	m.healthPS.PublishScoreUpdate(m.ctx, &healthpb.ScoreUpdate{
		TargetNodeId: nodeID,
		Score:        newScore,
		Alive:        false,
		UpdatedBy:    m.host.ID().String(),
		Timestamp:    timestamppb.Now(),
		Reason:       "direct_ping_fail",
	})

	// Publish ping request for indirect probing
	m.healthPS.PublishPingRequest(m.ctx, &healthpb.PingRequest{
		TargetNodeId: nodeID,
		RequesterId:  m.host.ID().String(),
		RequestId:    uuid.New().String(),
		Timestamp:    timestamppb.Now(),
	})

	// Check if quarantine was triggered (the score tracker already updated status)
	entry, err := m.phonebook.Get(nodeID, m.clusterPath)
	if err == nil && entry != nil && entry.Status == phonebook.NodeStatusEnum.Quarantined() {
		m.healthPS.PublishThresholdCrossed(m.ctx, &healthpb.ThresholdCrossed{
			TargetNodeId: nodeID,
			Score:        newScore,
			ReportedBy:   m.host.ID().String(),
			Timestamp:    timestamppb.Now(),
		})
	}
}

// --- PubSub Message Handlers ---

// ReasonNodeDeparting is the ScoreUpdate.Reason marker that signals an
// intentional graceful shutdown rather than a probe failure. Peers
// receiving this should evict the sender immediately instead of running
// the SWIM score machinery.
const ReasonNodeDeparting = "node_departing"

// onScoreUpdate handles ScoreUpdate from PubSub (SET semantics, alive always wins).
func (m *Monitor) onScoreUpdate(update *healthpb.ScoreUpdate) {
	// Graceful-shutdown shortcut: when a peer broadcasts its own
	// departure, mark the entry as Departed instead of removing it
	// (Bug #24). Keeping the row preserves the public key + cert so
	// that when the peer restarts with the same identity its signed
	// pubsub messages still verify and sync requests are accepted.
	// SWIM does not probe Departed peers; the next successful probe
	// (or sync handshake) reactivates the entry.
	if update.Reason == ReasonNodeDeparting && !update.Alive {
		m.logger.Info("peer announced graceful departure",
			zap.String("peer", update.TargetNodeId),
			zap.String("cluster", m.clusterPath))
		if err := m.phonebook.SetStatus(update.TargetNodeId, m.clusterPath, phonebook.NodeStatusEnum.Departed()); err != nil {
			m.logger.Warn("failed to mark departing peer in phonebook",
				zap.String("peer", update.TargetNodeId),
				zap.Error(err))
		}
		return
	}

	m.scores.SetScore(update.TargetNodeId, update.Score, update.Alive, update.Reason)

	// If alive, refresh our session too (cluster is active)
	if update.Alive && m.sessionRefresher != nil {
		m.sessionRefresher(m.clusterPath)
	}
}

// probeResultPersistLoop subscribes to NodeProbeResult events and
// records each one on the phonebook (LastProbeTime, LastProbeSuccess).
// Closes Bug #22 — the event was previously published with no consumers
// outside tests. Powers `falak node health`'s live SWIM display.
func (m *Monitor) probeResultPersistLoop() {
	ch := m.eventBus.Subscribe(events.TypeNodeProbeResult)
	for {
		select {
		case <-m.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			result, ok := ev.(events.NodeProbeResult)
			if !ok {
				continue
			}
			// Only persist results for our own cluster (the monitor
			// runs per-cluster so the event could in principle come
			// from a sibling monitor).
			if result.ClusterPath != m.clusterPath {
				continue
			}
			if err := m.phonebook.RecordProbe(result.NodeID, m.clusterPath, result.Success); err != nil {
				m.logger.Debug("failed to record probe result on phonebook",
					zap.String("peer", result.NodeID),
					zap.Bool("success", result.Success),
					zap.Error(err))
			}
		}
	}
}

// departureBroadcastLoop subscribes to local NodeDeparting events and
// publishes a corresponding health pubsub message so peers learn of the
// graceful shutdown in ~1 gossip round instead of via SWIM probe misses.
func (m *Monitor) departureBroadcastLoop() {
	ch := m.eventBus.Subscribe(events.TypeNodeDeparting)
	for {
		select {
		case <-m.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			dep, ok := ev.(events.NodeDeparting)
			if !ok {
				continue
			}
			update := &healthpb.ScoreUpdate{
				TargetNodeId: dep.NodeID,
				Score:        0,
				Alive:        false,
				UpdatedBy:    m.host.ID().String(),
				Timestamp:    timestamppb.Now(),
				Reason:       ReasonNodeDeparting,
			}
			if err := m.healthPS.PublishScoreUpdate(m.ctx, update); err != nil {
				m.logger.Warn("failed to broadcast departure",
					zap.String("cluster", m.clusterPath),
					zap.Error(err))
				continue
			}
			m.logger.Info("broadcast graceful departure",
				zap.String("cluster", m.clusterPath))
		}
	}
}

// onPingRequest handles a PingRequest — self-select if we're a top-N responder,
// then probe the target and publish the result.
func (m *Monitor) onPingRequest(req *healthpb.PingRequest) {
	// Don't respond to our own requests
	if req.RequesterId == m.host.ID().String() {
		return
	}

	targetID, err := peer.Decode(req.TargetNodeId)
	if err != nil {
		return
	}

	// Am I one of the top-N longest-lived nodes? (responder self-selection)
	if !m.isTopResponder(targetID) {
		return
	}

	// Ensure libp2p peerstore has the target's addresses
	entry, err := m.phonebook.Get(req.TargetNodeId, m.clusterPath)
	if err == nil && entry != nil {
		m.addPeerAddresses(targetID, entry.Addresses)
	}

	// Increment score before probing (as per plan)
	m.scores.RecordFailure(req.TargetNodeId, "indirect_pre_probe")

	// Ping the target
	success := m.pingHandler.Ping(m.ctx, targetID)

	if success {
		m.scores.RecordSuccess(req.TargetNodeId, "indirect_ping_success")

		m.healthPS.PublishScoreUpdate(m.ctx, &healthpb.ScoreUpdate{
			TargetNodeId: req.TargetNodeId,
			Score:        0,
			Alive:        true,
			UpdatedBy:    m.host.ID().String(),
			Timestamp:    timestamppb.Now(),
			Reason:       "indirect_ping_success",
		})
	} else {
		newScore := m.scores.GetScore(req.TargetNodeId)

		m.healthPS.PublishScoreUpdate(m.ctx, &healthpb.ScoreUpdate{
			TargetNodeId: req.TargetNodeId,
			Score:        newScore,
			Alive:        false,
			UpdatedBy:    m.host.ID().String(),
			Timestamp:    timestamppb.Now(),
			Reason:       "indirect_ping_fail",
		})

		// Check threshold (score tracker already handles status update,
		// but we publish ThresholdCrossed for cluster-wide final confirmation)
		entry, _ := m.phonebook.Get(req.TargetNodeId, m.clusterPath)
		if entry != nil && entry.Status == phonebook.NodeStatusEnum.Quarantined() {
			m.healthPS.PublishThresholdCrossed(m.ctx, &healthpb.ThresholdCrossed{
				TargetNodeId: req.TargetNodeId,
				Score:        newScore,
				ReportedBy:   m.host.ID().String(),
				Timestamp:    timestamppb.Now(),
			})
		}
	}
}

// onThresholdCrossed handles ThresholdCrossed — all nodes do a final confirmation ping.
func (m *Monitor) onThresholdCrossed(tc *healthpb.ThresholdCrossed) {
	targetID, err := peer.Decode(tc.TargetNodeId)
	if err != nil {
		return
	}

	// Final confirmation ping
	success := m.pingHandler.Ping(m.ctx, targetID)

	if success {
		// Target is alive — reset score
		m.scores.RecordSuccess(tc.TargetNodeId, "final_confirmation_success")

		m.healthPS.PublishScoreUpdate(m.ctx, &healthpb.ScoreUpdate{
			TargetNodeId: tc.TargetNodeId,
			Score:        0,
			Alive:        true,
			UpdatedBy:    m.host.ID().String(),
			Timestamp:    timestamppb.Now(),
			Reason:       "final_confirmation_success",
		})
	}
	// If fail, quarantine was already set by the score tracker
}

// isTopResponder checks if this node is one of the top-N longest-lived active nodes,
// excluding the target being probed.
func (m *Monitor) isTopResponder(excludeTarget peer.ID) bool {
	peers, err := m.phonebook.GetByCluster(m.clusterPath)
	if err != nil {
		return false
	}

	// Sort by FirstSeen (longest-lived first)
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].FirstSeen.Before(peers[j].FirstSeen)
	})

	selfID := m.host.ID().String()
	position := 0

	for _, p := range peers {
		// Skip target, self (we count self separately), and inactive nodes
		peerID, _ := peer.Decode(p.NodeID)
		if peerID == excludeTarget {
			continue
		}
		if p.Status != phonebook.NodeStatusEnum.Active() {
			continue
		}

		if p.NodeID == selfID {
			return position < m.maxResponders
		}

		position++
		if position >= m.maxResponders {
			return false
		}
	}
	return false
}

// --- Quarantine Management ---

// quarantineCheckLoop periodically checks quarantined nodes for timeout.
func (m *Monitor) quarantineCheckLoop() {
	ticker := time.NewTicker(m.quarantineCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			failed := m.scores.CheckQuarantineTimeouts()
			for _, nodeID := range failed {
				// Remove failed nodes from phonebook
				if err := m.phonebook.Remove(nodeID, m.clusterPath); err != nil {
					m.logger.Error("failed to remove failed node",
						zap.String("nodeId", nodeID),
						zap.Error(err))
				} else {
					m.logger.Info("removed failed node from phonebook",
						zap.String("nodeId", nodeID),
						zap.String("cluster", m.clusterPath))
				}
			}
		}
	}
}

// quarantineProbeLoop periodically publishes PingRequests for quarantined peers
// so the top-N responders can probe them. This ensures quarantined peers can
// either recover (score reset) or be detected as still unreachable.
func (m *Monitor) quarantineProbeLoop() {
	ticker := time.NewTicker(m.quarantineProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.probeQuarantinedPeers()
		}
	}
}

// probeQuarantinedPeers publishes PingRequests for all quarantined peers.
func (m *Monitor) probeQuarantinedPeers() {
	quarantined, err := m.phonebook.GetByStatus(m.clusterPath, phonebook.NodeStatusEnum.Quarantined())
	if err != nil || len(quarantined) == 0 {
		return
	}

	for _, entry := range quarantined {
		m.healthPS.PublishPingRequest(m.ctx, &healthpb.PingRequest{
			TargetNodeId: entry.NodeID,
			RequesterId:  m.host.ID().String(),
			RequestId:    uuid.New().String(),
			Timestamp:    timestamppb.Now(),
		})
	}
}

// addPeerAddresses adds a peer's addresses to the libp2p peerstore so we can dial them.
func (m *Monitor) addPeerAddresses(peerID peer.ID, addresses []string) {
	addrs := make([]multiaddr.Multiaddr, 0, len(addresses))
	for _, addrStr := range addresses {
		addr, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			continue
		}
		addrs = append(addrs, addr)
	}
	if len(addrs) > 0 {
		m.host.Peerstore().AddAddrs(peerID, addrs, peerstore.TempAddrTTL)
	}
}

// errRequired returns a formatted error for missing required dependencies.
func errRequired(name string) error {
	return &requiredError{name: name}
}

type requiredError struct {
	name string
}

func (e *requiredError) Error() string {
	return e.name + " is required"
}
