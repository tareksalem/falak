package metrics

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/node/phonebook"
)

// Manager wires together the collector, store, publisher, and subscriber
// into a single per-node component. It is the only type external code
// should construct or interact with — collector/store/publisher/subscriber
// are exported solely for unit testing.
//
// Lifecycle: NewManager → Start → JoinCluster (per cluster) → Stop.
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	store    *Store
	collector *Collector
	publisher *Publisher
	subscriber *Subscriber
	logger   *zap.Logger
	nodeID   string

	clusters map[string]struct{} // joined clusters
	ctx      context.Context
	cancel   context.CancelFunc
	started  bool
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithLogger sets the logger.
func WithLogger(logger *zap.Logger) ManagerOption {
	return func(m *Manager) {
		m.logger = logger
	}
}

// NewManager constructs a Manager. The data directory must already exist;
// the manager opens (or creates) `<dataDir>/metrics.db` inside it. Returns
// an error when the database cannot be opened.
//
// Required parameters:
//   - cfg: validated Config (use NewConfig)
//   - dataDir: where metrics.db lives
//   - nodeID: this node's libp2p peer ID as a string
//   - privateKey: this node's libp2p private key (for signing gossip)
//   - ps: the libp2p PubSub instance shared with the rest of the node
//   - pb: the phonebook for verifying incoming peer signatures
func NewManager(
	cfg Config,
	dataDir string,
	nodeID string,
	privateKey crypto.PrivKey,
	ps *pubsub.PubSub,
	pb phonebook.IPhonebook,
	opts ...ManagerOption,
) (*Manager, error) {
	if cfg.Interval == 0 {
		return nil, errors.New("metrics manager: config has zero interval")
	}
	if nodeID == "" {
		return nil, errors.New("metrics manager: nodeID is required")
	}

	m := &Manager{
		cfg:      cfg,
		nodeID:   nodeID,
		logger:   zap.NewNop(),
		clusters: make(map[string]struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}

	store, err := OpenStore(filepath.Join(dataDir, "metrics.db"),
		WithStoreLogger(m.logger.Named("store")),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics manager: open store: %w", err)
	}
	m.store = store

	m.collector = NewCollector(nodeID, cfg.Interval,
		WithCollectorLogger(m.logger.Named("collector")),
		WithCollectorOnSnapshot(m.handleLocalSnapshot),
	)
	m.publisher = NewPublisher(ps, nodeID, privateKey,
		WithPublisherLogger(m.logger.Named("publisher")),
	)
	m.subscriber = NewSubscriber(m.publisher, pb, nodeID, m.handlePeerSnapshot,
		WithSubscriberLogger(m.logger.Named("subscriber")),
	)

	return m, nil
}

// Start launches the collector loop. Idempotent: a second Start without
// an intervening Stop is a no-op. When Config.Enabled is false the
// manager still constructs cleanly but Start does not run the collector,
// allowing operators to disable metrics on dev nodes without restructuring
// their config.
func (m *Manager) Start(parent context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return
	}
	m.ctx, m.cancel = context.WithCancel(parent)
	m.started = true

	if !m.cfg.Enabled {
		m.logger.Info("metrics subsystem disabled by config; skipping collector start")
		return
	}

	m.collector.Start(m.ctx)
	m.logger.Info("metrics manager started",
		zap.String("node_id", m.nodeID),
		zap.Duration("interval", m.cfg.Interval))
}

// Stop halts every component the manager owns and closes the database.
// Safe to call multiple times.
func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return
	}
	m.started = false

	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	clusters := make([]string, 0, len(m.clusters))
	for path := range m.clusters {
		clusters = append(clusters, path)
	}
	m.clusters = make(map[string]struct{})
	m.mu.Unlock()

	m.collector.Stop()
	for _, path := range clusters {
		m.publisher.LeaveCluster(path)
	}
	m.subscriber.Stop()

	if err := m.store.Close(); err != nil {
		m.logger.Warn("metrics store close failed", zap.Error(err))
	}
	m.logger.Info("metrics manager stopped")
}

// JoinCluster registers a cluster with both the publisher and subscriber.
// Called from the node's cluster-join hook so that metrics flow as soon
// as authentication completes.
//
// Returns an error when the publisher or subscriber cannot join the
// cluster's health topic.
func (m *Manager) JoinCluster(clusterPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		return errors.New("metrics manager: not started")
	}
	if !m.cfg.Enabled {
		return nil
	}
	if _, ok := m.clusters[clusterPath]; ok {
		return nil
	}

	if err := m.publisher.JoinCluster(clusterPath); err != nil {
		return fmt.Errorf("publisher join: %w", err)
	}
	if err := m.subscriber.JoinCluster(m.ctx, clusterPath); err != nil {
		// Roll back the publisher join so the cluster ends up cleanly absent.
		m.publisher.LeaveCluster(clusterPath)
		return fmt.Errorf("subscriber join: %w", err)
	}
	m.clusters[clusterPath] = struct{}{}
	m.logger.Info("metrics joined cluster", zap.String("cluster", clusterPath))
	return nil
}

// LeaveCluster removes a cluster from both the publisher and subscriber.
// Idempotent.
func (m *Manager) LeaveCluster(clusterPath string) {
	m.mu.Lock()
	delete(m.clusters, clusterPath)
	m.mu.Unlock()

	m.publisher.LeaveCluster(clusterPath)
	m.subscriber.LeaveCluster(clusterPath)
}

// LocalLatest returns the most recent local snapshot, or zero when none
// has been collected yet.
func (m *Manager) LocalLatest() (Snapshot, error) {
	return m.store.LocalLatest()
}

// LocalHistory returns the rolling window of local snapshots, oldest first.
func (m *Manager) LocalHistory() ([]Snapshot, error) {
	return m.store.LocalHistory()
}

// PeerLatest returns the most recent snapshot received for a peer node,
// or zero when no row exists.
func (m *Manager) PeerLatest(nodeID string) (Snapshot, error) {
	return m.store.PeerLatest(nodeID)
}

// AllPeers returns the latest snapshot for every known peer.
func (m *Manager) AllPeers() ([]Snapshot, error) {
	return m.store.AllPeers()
}

// Config returns the active configuration. Useful for observability and
// for components that need to know the sampling cadence.
func (m *Manager) Config() Config {
	return m.cfg
}

// handleLocalSnapshot is the collector callback. It writes the snapshot
// to the rolling window and publishes it to every joined cluster. Errors
// are logged but never propagated — the next sample will retry.
func (m *Manager) handleLocalSnapshot(snap Snapshot) {
	if err := m.store.InsertLocal(snap); err != nil {
		m.logger.Warn("metrics insert local failed", zap.Error(err))
	}

	m.mu.Lock()
	clusters := make([]string, 0, len(m.clusters))
	for path := range m.clusters {
		clusters = append(clusters, path)
	}
	m.mu.Unlock()

	if len(clusters) == 0 {
		return
	}
	pubCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, path := range clusters {
		if err := m.publisher.Publish(pubCtx, path, snap); err != nil {
			m.logger.Debug("metrics publish failed",
				zap.String("cluster", path),
				zap.Error(err))
		}
	}
}

// handlePeerSnapshot is the subscriber callback. It writes the received
// peer snapshot to peer_latest, replacing any prior value.
func (m *Manager) handlePeerSnapshot(snap Snapshot) {
	if err := m.store.UpsertPeer(snap); err != nil {
		m.logger.Debug("metrics peer upsert failed",
			zap.String("peer", snap.NodeID),
			zap.Error(err))
	}
}
