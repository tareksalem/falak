// Package node provides the core node implementation for Falak.
package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	gosync "sync"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/election"
	"github.com/tareksalem/falak/network/proxy"
	falakrt "github.com/tareksalem/falak/runtime"
	"github.com/tareksalem/falak/shared"
	"github.com/tareksalem/falak/shared/secrets"
	"github.com/tareksalem/falak/snapshot"
	"github.com/tareksalem/falak/election/delay"
	"github.com/tareksalem/falak/election/gravity"
	"github.com/tareksalem/falak/node/auth"
	"github.com/tareksalem/falak/node/auth/certs"
	"github.com/tareksalem/falak/node/health"
	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/metrics"
	"github.com/tareksalem/falak/node/phonebook"
	nodesync "github.com/tareksalem/falak/node/sync"
)

// NodeState represents the lifecycle state of a node.
type NodeState string

// Private backing constants for the NodeState enum. External callers must
// go through NodeStateEnum instead of bare constants.
const (
	nodeStateInitializing NodeState = "initializing"
	nodeStateStarting     NodeState = "starting"
	nodeStateRunning      NodeState = "running"
	nodeStateDraining     NodeState = "draining"
	nodeStateShuttingDown NodeState = "shutting_down"
	nodeStateStopped      NodeState = "stopped"
)

// nodeStateEnum is the unexported carrier struct used to expose the valid
// NodeState values as methods on a single package-level accessor.
type nodeStateEnum struct{}

// NodeStateEnum is the public accessor for NodeState values.
// Use node.NodeStateEnum.Running() instead of bare constants.
var NodeStateEnum nodeStateEnum

// Initializing returns the "initializing" state — Start has begun but the
// host/pubsub/phonebook stack has not yet been wired.
func (nodeStateEnum) Initializing() NodeState { return nodeStateInitializing }

// Starting returns the "starting" state — reserved for future use between
// Initializing and Running.
func (nodeStateEnum) Starting() NodeState { return nodeStateStarting }

// Running returns the "running" state — the node is fully operational and
// accepting cluster joins.
func (nodeStateEnum) Running() NodeState { return nodeStateRunning }

// Draining returns the "draining" state — the node is preparing to exit,
// rejecting new work so in-flight work can wind down.
func (nodeStateEnum) Draining() NodeState { return nodeStateDraining }

// ShuttingDown returns the "shutting_down" state — Stop has been called and
// components are being torn down in reverse-start order.
func (nodeStateEnum) ShuttingDown() NodeState { return nodeStateShuttingDown }

// Stopped returns the "stopped" state — the node is inert and can be
// restarted via Start.
func (nodeStateEnum) Stopped() NodeState { return nodeStateStopped }

// Default timeouts and settings
const (
	DefaultStartupTimeout    = 30 * time.Second
	DefaultConnectionTimeout = 10 * time.Second
	DefaultShutdownTimeout   = 10 * time.Second
	DefaultListenAddr        = "/ip4/0.0.0.0/tcp/0"
)

// Node represents a Falak node that can join clusters and participate
// in the distributed network.
type Node struct {
	mu    gosync.RWMutex
	state NodeState

	// Configuration (set via options)
	name       string
	region     string
	datacenter string

	// Network configuration
	listenAddrs []string
	privateKey  crypto.PrivKey

	// Storage configuration
	dataDir string

	// Timeouts
	startupTimeout    time.Duration
	connectionTimeout time.Duration
	shutdownTimeout   time.Duration

	// Identity (set during Start)
	id peer.ID

	// Networking (set during Start)
	host   host.Host
	pubsub *pubsub.PubSub

	// Core components (set during Start)
	eventBus            events.Bus
	phonebook           phonebook.IPhonebook
	phonebookSubscriber *phonebook.Subscriber
	authenticator       *auth.Authenticator
	syncer              *nodesync.Syncer
	reauthSubscriber    *auth.ReauthSubscriber

	// Health: ping handler (registered at Start, shared across clusters)
	pingHandler *health.PingHandler
	// Health monitors per cluster (started on ClusterJoined)
	healthMonitors map[string]*health.Monitor

	// Capsule management (created at Start, sets up clusters on ClusterJoined)
	capsuleHandler *CapsuleHandler

	// Node-level resource metrics (created at Start, joins each cluster
	// as the node joins it). Powers the election gravity calculator.
	metricsManager *metrics.Manager

	// Election manager (created at Start, joins each cluster's election
	// topic as the node joins them). Decides which node runs each capsule
	// replica via the configured strategy.
	electionManager *election.Manager
	electionHandler *ElectionHandler

	// Runtime handler (created at Start). Subscribes to ElectionWon
	// events, starts/stops containers, manages snapshots.
	runtimeHandler   *falakrt.Handler
	containerRuntime falakrt.Runtime // injected via WithRuntime option

	// Network handler (created at Start when network.enabled=true and
	// a NetworkManagerFactory is wired). Bridges node event bus to the
	// per-cluster network.Manager. Tests omit the factory and the
	// handler short-circuits to a no-op.
	networkHandler        *NetworkHandler
	networkConfig         NetworkConfig
	networkManagerFactory NetworkManagerFactory
	networkConfigured     bool

	// Service handler (created at Start when service.enabled=true).
	// Owns the service.Manager, gossip publisher/subscriber, and the
	// proxy manager. Default disabled — opt in via WithServiceConfig.
	serviceHandler   *ServiceHandler
	serviceConfig    ServiceConfig
	serviceConfigured bool

	// Snapshot store + discovery (created alongside runtime handler).
	snapshotStore     *snapshot.Store
	snapshotDiscovery *snapshot.Discovery

	// Secrets encryption key (derived from PSK on cluster join).
	// Used to decrypt registry credentials at container start.
	secretsKey []byte

	// Cluster membership tracking
	joinedClusters map[string]time.Time // clusterPath -> joinedAt

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc

	// Logging
	logger *zap.Logger

	// Testing flags
	rejectAllAuth bool

	// placementRetryCap, when set positive, overrides the capsule
	// handler's default same-node group placement retry cap. Used by
	// integration tests that need to drive cap exhaustion with a
	// small node count.
	placementRetryCap int
}

// ClusterConfig holds configuration for joining a cluster.
type ClusterConfig struct {
	Path           string                  // region/datacenter/cluster
	PSK            []byte                  // Pre-shared key for authentication
	BootstrapPeers []string                // Multiaddrs of bootstrap peers
	Certificates   *certs.ClusterCertConfig // nil = auto mode, set = external CA mode

	// Orbits is the list of orbits to subscribe to in this cluster.
	// Empty means no orbit subscriptions — capsule events will not be received.
	Orbits []string

	// Capsules are declarative capsule specs to create in this cluster after
	// successful join. Typically loaded from the CUE config file.
	Capsules []capsule.CapsuleSpec

	// Election is the per-cluster election configuration. Nil means use
	// the default strategy (delay), default timeout, and default weights.
	Election *ClusterElectionConfig
}

// ClusterElectionConfig applies per-cluster tuning to the election
// subsystem. Mirrors config.ElectionConfig but uses native types
// (time.Duration, gravity.Weights) for direct consumption by the node.
type ClusterElectionConfig struct {
	// Algorithm selects the election strategy. Only "delay" is supported;
	// any other value falls back to the registry default.
	Algorithm string

	// Timeout is the maximum duration for a single election round.
	// Zero means use the manager-wide default (10s).
	Timeout time.Duration

	// Weights overrides gravity factor weights for this cluster.
	// Nil means use the defaults.
	Weights *gravity.Weights
}

// Option configures a Node.
type Option func(*Node)

// WithName sets the node name.
func WithName(name string) Option {
	return func(n *Node) {
		n.name = name
	}
}

// WithRegion sets the node's region.
func WithRegion(region string) Option {
	return func(n *Node) {
		n.region = region
	}
}

// WithDatacenter sets the node's datacenter.
func WithDatacenter(datacenter string) Option {
	return func(n *Node) {
		n.datacenter = datacenter
	}
}

// WithRuntime sets the container runtime backend used by the runtime
// handler to start/stop/checkpoint containers. If not set, the node
// operates without a runtime (election still works, containers are not
// actually started — useful for testing the orchestration layer).
func WithRuntime(rt falakrt.Runtime) Option {
	return func(n *Node) {
		n.containerRuntime = rt
	}
}

// WithListenAddrs sets the network listen addresses.
func WithListenAddrs(addrs ...string) Option {
	return func(n *Node) {
		n.listenAddrs = addrs
	}
}

// WithPrivateKey sets the node's private key for identity.
// If not set, a new Ed25519 key pair will be generated.
func WithPrivateKey(key crypto.PrivKey) Option {
	return func(n *Node) {
		n.privateKey = key
	}
}

// WithDataDir sets the data directory for persistent storage.
func WithDataDir(dir string) Option {
	return func(n *Node) {
		n.dataDir = dir
	}
}

// WithStartupTimeout sets the startup timeout.
func WithStartupTimeout(d time.Duration) Option {
	return func(n *Node) {
		n.startupTimeout = d
	}
}

// WithConnectionTimeout sets the connection timeout for peer connections.
func WithConnectionTimeout(d time.Duration) Option {
	return func(n *Node) {
		n.connectionTimeout = d
	}
}

// WithShutdownTimeout sets the shutdown timeout.
func WithShutdownTimeout(d time.Duration) Option {
	return func(n *Node) {
		n.shutdownTimeout = d
	}
}

// WithLogger sets the logger for the node.
func WithLogger(logger *zap.Logger) Option {
	return func(n *Node) {
		n.logger = logger
	}
}

// WithPlacementRetryCap overrides the same-node group placement
// retry cap on the embedded CapsuleHandler. Non-positive values are
// ignored. Exposed for integration tests that need to drive cap
// exhaustion deterministically with a small node count.
func WithPlacementRetryCap(n int) Option {
	return func(node *Node) {
		if n > 0 {
			node.placementRetryCap = n
		}
	}
}

// WithNetworkConfig wires the per-cluster network subsystem
// configuration onto this node. The configuration is consumed when the
// network handler is built in Start; calling this multiple times
// replaces the prior value. When omitted the network subsystem stays
// disabled regardless of WithNetworkManagerFactory.
func WithNetworkConfig(cfg NetworkConfig) Option {
	return func(n *Node) {
		n.networkConfig = cfg
		n.networkConfigured = true
	}
}

// WithNodeNetworkManagerFactory injects the factory the network handler
// uses to construct the underlying network.Manager. Production wires a
// builder that pulls bridge / overlay / DNS / endpoint dependencies
// together; tests pass a stub that returns a fake manager. Omitting it
// leaves the network subsystem disabled even when WithNetworkConfig
// requests it.
func WithNodeNetworkManagerFactory(f NetworkManagerFactory) Option {
	return func(n *Node) { n.networkManagerFactory = f }
}

// WithServiceConfig enables the Service (traffic-management) subsystem
// on this node with the given configuration. Omit to leave the
// subsystem off — capsules still run, but no Service control plane
// comes up. The handler is constructed in Start after the network
// handler so the per-group bridge gateway map is available for the
// proxy manager to consult.
func WithServiceConfig(cfg ServiceConfig) Option {
	return func(n *Node) {
		n.serviceConfig = cfg
		n.serviceConfigured = true
	}
}

// WithRejectAllAuth makes the node reject every incoming auth announcement.
// The flag exists solely to exercise the PKI-auth rejection path in tests —
// a node configured this way cannot onboard peers.
//
// TEST/DEBUG ONLY — never enable this in production. Leaving it on in a
// live cluster is equivalent to a self-imposed denial-of-service against
// cluster growth: no new member can complete the broadcast confirmation.
func WithRejectAllAuth(reject bool) Option {
	return func(n *Node) {
		n.rejectAllAuth = reject
	}
}

// New creates a new Node with the given options.
func New(opts ...Option) *Node {
	ctx, cancel := context.WithCancel(context.Background())

	n := &Node{
		state:             nodeStateStopped,
		listenAddrs:       []string{DefaultListenAddr},
		startupTimeout:    DefaultStartupTimeout,
		connectionTimeout: DefaultConnectionTimeout,
		shutdownTimeout:   DefaultShutdownTimeout,
		healthMonitors:    make(map[string]*health.Monitor),
		joinedClusters:    make(map[string]time.Time),
		ctx:               ctx,
		cancel:            cancel,
		logger:            zap.NewNop(),
	}

	for _, opt := range opts {
		opt(n)
	}

	// Apply named logger
	n.logger = n.logger.Named("node")

	return n
}

// ID returns the node's peer ID.
func (n *Node) ID() peer.ID {
	return n.id
}

// Name returns the node's name.
func (n *Node) Name() string {
	return n.name
}

// DataDir returns the node's data directory.
func (n *Node) DataDir() string {
	if n.dataDir != "" {
		return n.dataDir
	}
	return n.defaultDataDir()
}

// State returns the current lifecycle state.
func (n *Node) State() NodeState {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.state
}

func (n *Node) setState(state NodeState) {
	n.mu.Lock()
	n.state = state
	n.mu.Unlock()
	n.logger.Debug("state changed", zap.String("state", string(state)))
}

// Start initializes and starts all node components.
func (n *Node) Start() error {
	n.mu.Lock()
	if n.state != nodeStateStopped {
		n.mu.Unlock()
		return fmt.Errorf("node is not stopped (state: %s)", n.state)
	}
	n.state = nodeStateInitializing
	n.mu.Unlock()

	n.logger.Info("starting node", zap.String("name", n.name))

	// 1. Generate or use provided private key
	if err := n.initializeIdentity(); err != nil {
		n.setState(nodeStateStopped)
		return fmt.Errorf("failed to initialize identity: %w", err)
	}

	// 2. Create libp2p host
	if err := n.initializeHost(); err != nil {
		n.setState(nodeStateStopped)
		return fmt.Errorf("failed to initialize host: %w", err)
	}

	// 3. Create PubSub
	if err := n.initializePubSub(); err != nil {
		n.cleanup()
		return fmt.Errorf("failed to initialize pubsub: %w", err)
	}

	// 4. Create EventBus
	n.eventBus = events.NewBus(
		events.WithContext(n.ctx),
		events.WithLogger(n.logger.Named("eventbus")),
	)

	// 5. Create Phonebook
	if err := n.initializePhonebook(); err != nil {
		n.cleanup()
		return fmt.Errorf("failed to initialize phonebook: %w", err)
	}

	// 6. Create and start PhonebookSubscriber
	n.phonebookSubscriber = phonebook.NewSubscriber(
		phonebook.WithContext(n.ctx),
		phonebook.WithPhonebook(n.phonebook),
		phonebook.WithEventBus(n.eventBus),
		phonebook.WithSubscriberLogger(n.logger.Named("phonebook-subscriber")),
	)
	if err := n.phonebookSubscriber.Start(); err != nil {
		n.cleanup()
		return fmt.Errorf("failed to start phonebook subscriber: %w", err)
	}

	// 7. Create and start Authenticator
	n.authenticator = auth.New(
		auth.WithContext(n.ctx),
		auth.WithHost(n.host),
		auth.WithPubSub(n.pubsub),
		auth.WithEventBus(n.eventBus),
		auth.WithPhonebook(n.phonebook),
		auth.WithPrivateKey(n.privateKey),
		auth.WithDataDir(n.DataDir()),
		auth.WithRejectAllAuth(n.rejectAllAuth),
		auth.WithLogger(n.logger.Named("auth")),
	)
	if err := n.authenticator.Start(); err != nil {
		n.cleanup()
		return fmt.Errorf("failed to start authenticator: %w", err)
	}
	n.authenticator.RegisterProtocol()

	// 8. Create and start Syncer (with revocation sync adapter)
	n.syncer = nodesync.New(
		nodesync.WithContext(n.ctx),
		nodesync.WithHost(n.host),
		nodesync.WithPhonebook(n.phonebook),
		nodesync.WithEventBus(n.eventBus),
		nodesync.WithLogger(n.logger.Named("sync")),
		nodesync.WithRevocationSource(newRevocationAdapter(n.authenticator)),
	)
	if err := n.syncer.Start(); err != nil {
		n.cleanup()
		return fmt.Errorf("failed to start syncer: %w", err)
	}

	// 9. Register health ping handler (must be ready before joining clusters)
	n.pingHandler = health.NewPingHandler(n.host, n.logger.Named("health.ping"), health.DefaultPingTimeout)
	n.pingHandler.Register()

	// 10. Create and start ReauthSubscriber
	n.reauthSubscriber = auth.NewReauthSubscriber(
		auth.WithReauthContext(n.ctx),
		auth.WithReauthAuthenticator(n.authenticator),
		auth.WithReauthPhonebook(n.phonebook),
		auth.WithReauthEventBus(n.eventBus),
		auth.WithReauthLogger(n.logger.Named("reauth")),
	)
	if err := n.reauthSubscriber.Start(); err != nil {
		n.cleanup()
		return fmt.Errorf("failed to start reauth subscriber: %w", err)
	}

	// 11. Create and start metrics manager — must come before the capsule
	// handler so the gravity StateProvider has somewhere to read from.
	if err := n.initializeMetricsManager(); err != nil {
		n.cleanup()
		return fmt.Errorf("failed to start metrics manager: %w", err)
	}

	// 12. Create and start CapsuleHandler
	if err := n.initializeCapsuleHandler(); err != nil {
		n.cleanup()
		return fmt.Errorf("failed to start capsule handler: %w", err)
	}

	// 12a. Create and start the per-cluster NetworkHandler. The handler
	// bridges the node event bus to the network.Manager (bridge,
	// overlay, DNS, endpoint gossip). When the factory is unwired the
	// handler logs a warning and skips, leaving the runtime fully
	// functional with Podman's default networking. Must come before
	// the runtime handler so per-group bridges exist when containers
	// start.
	n.initializeNetworkHandler()

	// 12b. Create and start the Service handler. Sits on top of the
	// network handler so the proxy manager can see the bridge gateway
	// map. Disabled by default — opt in via WithServiceConfig.
	n.initializeServiceHandler()

	// 13. Create and start the election manager. Depends on metrics
	// (state provider), capsules (lifecycle + store), pubsub, and
	// the phonebook (for verifying election claim signatures).
	if err := n.initializeElectionManager(); err != nil {
		n.cleanup()
		return fmt.Errorf("failed to start election manager: %w", err)
	}

	// 14. Create snapshot store + discovery. Depends on the data dir
	// and pubsub being available.
	if n.containerRuntime != nil {
		if err := n.initializeSnapshotStore(); err != nil {
			n.logger.Warn("snapshot store init failed (snapshots disabled)", zap.Error(err))
		}
	}

	// 15. Create and start the runtime handler. Depends on election
	// (subscribes to ElectionWon), capsules (lookup specs), snapshot
	// store, and an injected container Runtime backend. If no runtime
	// was injected (testing), the handler is skipped.
	if n.containerRuntime != nil {
		n.initializeRuntimeHandler()
	}

	n.setState(nodeStateRunning)
	n.logger.Info("node started",
		zap.String("id", n.id.String()),
		zap.Strings("addrs", n.ListenAddrs()))

	return nil
}

// Drain gracefully prepares the node for shutdown. It:
//  1. Sets the node state to Draining so the election manager rejects
//     new election requests on this node.
//  2. Broadcasts a departure event so peers update their phonebooks
//     and immediately re-elect orphaned replicas (without waiting for
//     the SWIM failure timeout).
//  3. Calls Stop to tear down all components.
//
// Drain is the recommended way to shut down a node in production.
// Stop without Drain still works but relies on SWIM to detect the
// departure, which takes seconds.
//
// TODO(audit-session14): Drain is currently a stub — NodeDeparting is
// published on the internal event bus only, there is no PubSub bridge,
// no subscriber checks IsDraining, and containers are not explicitly
// stopped before Stop(). The 500ms sleep below is a placeholder waiting
// for a propagation mechanism that does not yet exist. See the
// "Audit follow-ups (Session 14)" section of PROGRESS.md for the full
// fix plan. When that work lands, replace the magic sleep with a
// bounded wait on peer acknowledgement or a WithDrainPropagationDelay
// configurable option.
func (n *Node) Drain() error {
	n.mu.Lock()
	if n.state != nodeStateRunning {
		n.mu.Unlock()
		return fmt.Errorf("cannot drain node in state %s", n.state)
	}
	n.state = nodeStateDraining
	n.mu.Unlock()

	n.logger.Info("draining node — rejecting new elections and notifying peers")

	// Publish a departure event so peers can re-elect immediately.
	// NOTE: today this only reaches local subscribers; see the Drain
	// docstring for the wider issue.
	n.eventBus.Publish(events.NodeDeparting{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      n.id.String(),
	})

	// Placeholder: once NodeDeparting is bridged to PubSub, replace
	// this with a bounded wait on peer acknowledgement.
	time.Sleep(500 * time.Millisecond)

	return n.Stop()
}

// IsDraining returns true if the node is in the draining state.
func (n *Node) IsDraining() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.state == nodeStateDraining
}

// Stop gracefully shuts down the node.
func (n *Node) Stop() error {
	n.mu.Lock()
	if n.state == nodeStateStopped || n.state == nodeStateShuttingDown {
		n.mu.Unlock()
		return nil
	}
	n.state = nodeStateShuttingDown
	n.mu.Unlock()

	n.logger.Info("stopping node")

	// Cancel context to signal all components
	n.cancel()

	// Stop components in reverse order
	n.cleanup()

	n.setState(nodeStateStopped)
	n.logger.Info("node stopped")

	return nil
}

// cleanup stops all components in reverse order.
func (n *Node) cleanup() {
	// Stop runtime handler first — it owns running containers.
	if n.runtimeHandler != nil {
		n.runtimeHandler.Stop()
	}

	// Stop election handler + manager — they sit on top of capsules,
	// metrics, and pubsub, so we tear them down before their dependencies.
	if n.electionHandler != nil {
		n.electionHandler.Stop()
	}
	if n.electionManager != nil {
		n.electionManager.Stop()
	}

	// Stop service handler BEFORE the network handler: the proxy
	// listeners owned by the service subsystem bind to bridge gateway
	// IPs that the network handler will tear down.
	if n.serviceHandler != nil {
		if err := n.serviceHandler.Stop(); err != nil {
			n.logger.Warn("service handler stop returned error", zap.Error(err))
		}
	}

	// Stop network handler BEFORE the capsule handler: bridge teardown
	// callbacks need the event bus alive so withdraw / leave events
	// propagate through the manager's subscriptions one last time.
	if n.networkHandler != nil {
		if err := n.networkHandler.Stop(); err != nil {
			n.logger.Warn("network handler stop returned error", zap.Error(err))
		}
	}

	// Stop capsule handler (it depends on pubsub, eventbus, phonebook)
	if n.capsuleHandler != nil {
		n.capsuleHandler.Stop()
	}

	// Stop metrics manager (collector loop, publisher topics, subscriber goroutines).
	if n.metricsManager != nil {
		n.metricsManager.Stop()
	}

	// Stop health monitors for all clusters
	n.mu.RLock()
	for _, monitor := range n.healthMonitors {
		monitor.Stop()
	}
	n.mu.RUnlock()

	// Unregister ping handler
	if n.pingHandler != nil {
		n.pingHandler.Unregister()
	}

	if n.reauthSubscriber != nil {
		n.reauthSubscriber.Stop()
	}
	if n.syncer != nil {
		n.syncer.Stop()
	}
	if n.authenticator != nil {
		n.authenticator.Stop()
	}
	if n.phonebookSubscriber != nil {
		n.phonebookSubscriber.Stop()
	}
	if n.snapshotStore != nil {
		n.snapshotStore.Close()
	}
	if n.phonebook != nil {
		n.phonebook.Close()
	}
	if n.eventBus != nil {
		n.eventBus.Close()
	}
	if n.host != nil {
		n.host.Close()
	}
}

// Join joins a cluster using the provided configuration.
// This method publishes a ClusterJoinRequested event and waits for
// either ClusterJoined or ClusterJoinFailed response events.
func (n *Node) Join(ctx context.Context, cfg ClusterConfig) error {
	if n.State() != nodeStateRunning {
		return fmt.Errorf("node is not running")
	}

	n.logger.Info("joining cluster",
		zap.String("cluster", cfg.Path),
		zap.Int("bootstrapPeers", len(cfg.BootstrapPeers)))

	// Register per-cluster cert config with authenticator if provided
	if cfg.Certificates != nil {
		n.authenticator.RegisterCertConfig(cfg.Path, cfg.Certificates)
	}

	// Subscribe to response events
	joinedCh := n.eventBus.Subscribe(events.TypeClusterJoined)
	failedCh := n.eventBus.Subscribe(events.TypeClusterJoinFailed)
	defer n.eventBus.Unsubscribe(events.TypeClusterJoined, joinedCh)
	defer n.eventBus.Unsubscribe(events.TypeClusterJoinFailed, failedCh)

	// Publish join request - Authenticator will handle the flow
	n.eventBus.Publish(events.ClusterJoinRequested{
		BaseEvent:      events.NewBaseEvent(),
		ClusterPath:    cfg.Path,
		PSK:            cfg.PSK,
		BootstrapPeers: cfg.BootstrapPeers,
	})

	// Wait for result
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case event := <-joinedCh:
			joined, ok := event.(events.ClusterJoined)
			if !ok || joined.ClusterPath != cfg.Path {
				continue // Not our cluster, keep waiting
			}

			// Track membership
			n.mu.Lock()
			n.joinedClusters[cfg.Path] = time.Now()
			n.mu.Unlock()

			// Start health monitor for this cluster
			n.startHealthMonitor(cfg.Path)

			// Join the metrics gossip for this cluster so peers see our
			// resource updates and we receive theirs.
			if n.metricsManager != nil {
				if err := n.metricsManager.JoinCluster(cfg.Path); err != nil {
					n.logger.Warn("metrics manager failed to join cluster",
						zap.String("cluster", cfg.Path),
						zap.Error(err))
				}
			}

			// Open the election PubSub topic for this cluster so the
			// node both publishes its own claims and observes peers'.
			// Apply any per-cluster strategy/timeout/weights overrides
			// before joining so the first election uses the right config.
			if n.electionManager != nil {
				n.applyClusterElectionConfig(cfg)
				if err := n.electionManager.JoinCluster(cfg.Path); err != nil {
					n.logger.Warn("election manager failed to join cluster",
						zap.String("cluster", cfg.Path),
						zap.Error(err))
				}
			}

			// Derive the secrets encryption key from the PSK. Same on
			// every node in the cluster — used to decrypt registry
			// credentials at container start.
			if len(cfg.PSK) > 0 && n.secretsKey == nil {
				if sek, err := secrets.DeriveKey(cfg.PSK, cfg.Path); err == nil {
					n.secretsKey = sek
				} else {
					n.logger.Warn("failed to derive secrets key",
						zap.String("cluster", cfg.Path), zap.Error(err))
				}
			}

			// Subscribe to configured orbits.
			if err := n.joinConfiguredOrbits(ctx, cfg); err != nil {
				n.logger.Warn("failed to join some orbits",
					zap.String("cluster", cfg.Path),
					zap.Error(err))
			}

			// Create declared capsules.
			if err := n.createConfiguredCapsules(ctx, cfg); err != nil {
				n.logger.Warn("failed to create some configured capsules",
					zap.String("cluster", cfg.Path),
					zap.Error(err))
			}

			n.logger.Info("joined cluster", zap.String("cluster", cfg.Path))
			return nil

		case event := <-failedCh:
			failed, ok := event.(events.ClusterJoinFailed)
			if !ok || failed.ClusterPath != cfg.Path {
				continue // Not our cluster, keep waiting
			}

			return fmt.Errorf("failed to join cluster: %s", failed.Reason)
		}
	}
}

// applyClusterElectionConfig applies per-cluster election overrides to
// the election manager before the cluster's pubsub topic is opened.
//
//   - Algorithm — installs a strategy override in the registry.
//   - Timeout — sets a per-cluster election timeout.
//   - Weights — builds a dedicated gravity.Calculator for the cluster
//     using the supplied overrides, merged onto the defaults.
//
// It is a no-op when the cluster config has no Election block.
func (n *Node) applyClusterElectionConfig(cfg ClusterConfig) {
	if cfg.Election == nil {
		return
	}

	// Strategy selection. Only the delay strategy is supported today.
	switch cfg.Election.Algorithm {
	case "", "delay":
		// Default — no override needed.
	default:
		n.logger.Warn("unknown election algorithm, falling back to default",
			zap.String("cluster", cfg.Path),
			zap.String("algorithm", cfg.Election.Algorithm))
	}

	// Timeout override.
	if cfg.Election.Timeout > 0 {
		n.electionManager.SetClusterTimeout(cfg.Path, cfg.Election.Timeout)
		n.logger.Info("cluster election timeout set",
			zap.String("cluster", cfg.Path),
			zap.Duration("timeout", cfg.Election.Timeout))
	}

	// Weight overrides — build a dedicated Calculator.
	if cfg.Election.Weights != nil {
		weights := gravity.DefaultWeights().Merge(*cfg.Election.Weights)
		// Reuse the same lookup that the default calculator uses.
		calc := gravity.NewCalculator(gravity.WithWeights(weights))
		n.electionManager.SetClusterCalculator(cfg.Path, calc)
		n.logger.Info("cluster election weights overridden",
			zap.String("cluster", cfg.Path))
	}
}

// joinConfiguredOrbits subscribes the capsule handler to every orbit named in
// the cluster config. It is safe to pass an empty list (no-op).
func (n *Node) joinConfiguredOrbits(ctx context.Context, cfg ClusterConfig) error {
	if n.capsuleHandler == nil || len(cfg.Orbits) == 0 {
		return nil
	}

	var firstErr error
	for _, orbitName := range cfg.Orbits {
		if err := n.capsuleHandler.JoinOrbit(ctx, cfg.Path, orbitName); err != nil {
			n.logger.Error("failed to join orbit",
				zap.String("cluster", cfg.Path),
				zap.String("orbit", orbitName),
				zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n.logger.Info("subscribed to orbit",
			zap.String("cluster", cfg.Path),
			zap.String("orbit", orbitName))
	}
	return firstErr
}

// createConfiguredCapsules creates every declared capsule spec on this node
// after the cluster has been successfully joined. Each spec is validated by
// the capsule.Manager before being announced to the mesh.
func (n *Node) createConfiguredCapsules(ctx context.Context, cfg ClusterConfig) error {
	if n.capsuleHandler == nil || len(cfg.Capsules) == 0 {
		return nil
	}

	var firstErr error
	for _, spec := range cfg.Capsules {
		_, err := n.capsuleHandler.Manager().Create(ctx, cfg.Path, spec)
		if err != nil {
			n.logger.Error("failed to create configured capsule",
				zap.String("cluster", cfg.Path),
				zap.String("name", spec.Name),
				zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n.logger.Info("created configured capsule",
			zap.String("cluster", cfg.Path),
			zap.String("name", spec.Name),
			zap.String("orbit", spec.Orbit))
	}
	return firstErr
}

// Leave tears down every per-cluster resource this node wired up during
// Join: health monitor, election topic, metrics gossip, capsule orbits,
// authentication state (session, keys, provider, revocations), and the
// cluster membership bookkeeping.
//
// Teardown order mirrors the reverse of Join's build order so upstream
// consumers (runtime, election) stop referencing cluster state before
// their dependencies are torn down.
func (n *Node) Leave(clusterPath string) error {
	n.mu.Lock()
	if _, ok := n.joinedClusters[clusterPath]; !ok {
		n.mu.Unlock()
		return fmt.Errorf("not a member of cluster %s", clusterPath)
	}
	delete(n.joinedClusters, clusterPath)

	monitor := n.healthMonitors[clusterPath]
	delete(n.healthMonitors, clusterPath)
	n.mu.Unlock()

	// Election manager first — it sits on top of capsules + metrics.
	if n.electionManager != nil {
		n.electionManager.LeaveCluster(clusterPath)
	}

	// Capsule handler: leave all orbits for this cluster and drop
	// per-cluster announcer/dedup state.
	if n.capsuleHandler != nil {
		n.capsuleHandler.LeaveCluster(clusterPath)
	}

	// Metrics manager — publisher + subscriber for this cluster.
	if n.metricsManager != nil {
		n.metricsManager.LeaveCluster(clusterPath)
	}

	// Health monitor (already detached from the map above; stop outside
	// the node lock to avoid holding it during potentially blocking work).
	if monitor != nil {
		monitor.Stop()
	}

	// Authenticator: close topic and drop session, keys, provider,
	// revocation list, cert config for this cluster.
	if n.authenticator != nil {
		n.authenticator.LeaveCluster(clusterPath)
	}

	n.logger.Info("left cluster", zap.String("cluster", clusterPath))
	return nil
}

// startHealthMonitor creates and starts a health monitor for a cluster.
func (n *Node) startHealthMonitor(clusterPath string) {
	monitor := health.NewMonitor(
		health.WithMonitorContext(n.ctx),
		health.WithMonitorHost(n.host),
		health.WithMonitorPubSub(n.pubsub),
		health.WithMonitorPhonebook(n.phonebook),
		health.WithMonitorPrivateKey(n.privateKey),
		health.WithMonitorEventBus(n.eventBus),
		health.WithMonitorLogger(n.logger.Named("health")),
		health.WithMonitorClusterPath(clusterPath),
		health.WithPingHandler(n.pingHandler),
		health.WithSessionRefresher(func(cp string) {
			n.authenticator.UpdateSessionActivity(cp)
		}),
	)

	if err := monitor.Start(); err != nil {
		n.logger.Error("failed to start health monitor",
			zap.String("cluster", clusterPath),
			zap.Error(err))
		return
	}

	n.mu.Lock()
	n.healthMonitors[clusterPath] = monitor
	n.mu.Unlock()
}

// ListenAddrs returns the node's listen addresses as strings.
func (n *Node) ListenAddrs() []string {
	if n.host == nil {
		return nil
	}
	addrs := n.host.Addrs()
	result := make([]string, len(addrs))
	for i, addr := range addrs {
		result[i] = addr.String()
	}
	return result
}

// Addrs returns the full multiaddrs (including peer ID) for this node.
func (n *Node) Addrs() []string {
	if n.host == nil {
		return nil
	}
	addrs := n.host.Addrs()
	result := make([]string, len(addrs))
	for i, addr := range addrs {
		result[i] = fmt.Sprintf("%s/p2p/%s", addr.String(), n.id.String())
	}
	return result
}

// Phonebook returns the node's phonebook for querying peer state.
func (n *Node) Phonebook() phonebook.IPhonebook {
	return n.phonebook
}

// PubSub returns the libp2p GossipSub instance.
func (n *Node) PubSub() *pubsub.PubSub {
	return n.pubsub
}

// EventBus returns the node's internal event bus.
func (n *Node) EventBus() events.Bus {
	return n.eventBus
}

// PrivateKey returns the node's private key (for signing).
func (n *Node) PrivateKey() crypto.PrivKey {
	return n.privateKey
}

// Capsules returns the node's capsule manager for capsule CRUD operations.
// This is the primary entry point for the gRPC API and falak CLI.
func (n *Node) Capsules() *capsule.Manager {
	if n.capsuleHandler == nil {
		return nil
	}
	return n.capsuleHandler.Manager()
}

// CapsuleHandler returns the node's capsule handler for advanced operations
// like joining specific orbits. Most code should use Capsules() instead.
func (n *Node) CapsuleHandler() *CapsuleHandler {
	return n.capsuleHandler
}

// Metrics returns the node's resource metrics manager. Returns nil before
// Start has completed. Used by the election state provider and by
// observability commands.
func (n *Node) Metrics() *metrics.Manager {
	return n.metricsManager
}

// Election returns the node's election manager. Returns nil before
// Start has completed. Used by tests and by future API/observability
// surfaces. Most code should rely on the lifecycle events emitted on
// the node event bus instead of touching the manager directly.
func (n *Node) Election() *election.Manager {
	return n.electionManager
}

// JoinedClusters returns the list of clusters this node has joined.
func (n *Node) JoinedClusters() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	clusters := make([]string, 0, len(n.joinedClusters))
	for path := range n.joinedClusters {
		clusters = append(clusters, path)
	}
	return clusters
}

// initializeIdentity sets up the node's cryptographic identity. Precedence:
//
//  1. Key passed via WithPrivateKey — used as-is (tests, explicit injection).
//  2. Otherwise loaded from <dataDir>/node.key, generated and persisted
//     on first run with 0600 permissions.
//
// Deriving the identity from the node name is deliberately NOT supported:
// that would let anyone who knows the name impersonate the node.
func (n *Node) initializeIdentity() error {
	if n.privateKey == nil {
		dataDir := n.DataDir()
		if err := os.MkdirAll(dataDir, 0700); err != nil {
			return fmt.Errorf("failed to create data directory: %w", err)
		}

		keyPath := filepath.Join(dataDir, shared.DefaultKeyFileName)
		priv, generated, err := shared.LoadOrGenerateKey(keyPath)
		if err != nil {
			return fmt.Errorf("failed to load or generate node key: %w", err)
		}
		if generated {
			n.logger.Info("generated new node identity key",
				zap.String("path", keyPath))
		}
		n.privateKey = priv
	}

	id, err := peer.IDFromPrivateKey(n.privateKey)
	if err != nil {
		return fmt.Errorf("failed to get peer ID: %w", err)
	}
	n.id = id

	n.logger.Debug("identity initialized", zap.String("id", id.String()))
	return nil
}

// initializeHost creates the libp2p host.
func (n *Node) initializeHost() error {
	listenAddrs := make([]multiaddr.Multiaddr, 0, len(n.listenAddrs))
	for _, addrStr := range n.listenAddrs {
		addr, err := multiaddr.NewMultiaddr(addrStr)
		if err != nil {
			return fmt.Errorf("invalid listen address %s: %w", addrStr, err)
		}
		listenAddrs = append(listenAddrs, addr)
	}

	h, err := libp2p.New(
		libp2p.Identity(n.privateKey),
		libp2p.ListenAddrs(listenAddrs...),
	)
	if err != nil {
		return fmt.Errorf("failed to create host: %w", err)
	}

	n.host = h
	n.logger.Debug("host initialized",
		zap.String("id", h.ID().String()),
		zap.Int("addrs", len(h.Addrs())))

	return nil
}

// initializePubSub creates the GossipSub instance.
func (n *Node) initializePubSub() error {
	ps, err := pubsub.NewGossipSub(n.ctx, n.host)
	if err != nil {
		return fmt.Errorf("failed to create gossipsub: %w", err)
	}
	n.pubsub = ps
	n.logger.Debug("pubsub initialized")
	return nil
}

// initializePhonebook creates the phonebook storage.
// initializeMetricsManager creates the node-level resource metrics manager.
// The manager owns its own SQLite store at <dataDir>/metrics.db, samples
// host metrics on a configurable interval, and gossips them to peers via
// the existing health pubsub topic.
//
// The metrics manager must be started before the capsule handler because
// the election gravity calculator depends on the metrics StateProvider for
// resource scoring.
func (n *Node) initializeMetricsManager() error {
	dataDir := n.dataDir
	if dataDir == "" {
		dataDir = n.defaultDataDir()
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	cfg, err := metrics.NewConfig(metrics.DefaultConfig())
	if err != nil {
		return fmt.Errorf("failed to load metrics config: %w", err)
	}

	mgr, err := metrics.NewManager(
		cfg,
		dataDir,
		n.id.String(),
		n.privateKey,
		n.pubsub,
		n.phonebook,
		metrics.WithLogger(n.logger.Named("metrics")),
	)
	if err != nil {
		return fmt.Errorf("failed to construct metrics manager: %w", err)
	}

	n.metricsManager = mgr
	n.metricsManager.Start(n.ctx)
	n.logger.Debug("metrics manager initialized",
		zap.Duration("interval", cfg.Interval),
		zap.Bool("enabled", cfg.Enabled))
	return nil
}

// initializeElectionManager constructs the election subsystem and the
// thin handler that pumps events.ElectionRequested onto it.
//
// The election manager owns:
//
//   - A gravity calculator (default weights — per-cluster overrides will
//     come from CUE config in a future iteration).
//   - A StateProvider sourced from the metrics manager + phonebook,
//     so gravity can score the local node against the capsule spec.
//   - The delay-based strategy registered as the single built-in. Every
//     node scores only itself and publishes a Claim; the manager's
//     tiebreak protocol resolves collisions.
//   - The capsule.Manager wrapped behind narrow store/lifecycle adapters
//     so the election package never imports the full Manager surface.
//   - A signer/verifier pair built around the local libp2p key and the
//     phonebook for cross-node claim verification.
func (n *Node) initializeElectionManager() error {
	if n.capsuleHandler == nil || n.metricsManager == nil {
		return fmt.Errorf("election: capsule handler and metrics manager must be initialized first")
	}

	capsMgr := n.capsuleHandler.Manager()
	store := &electionStoreAdapter{manager: capsMgr}
	lifecycle := &electionLifecycleAdapter{manager: capsMgr}
	sink := &electionEventSink{
		bus:    n.eventBus,
		nodeID: n.id.String(),
		logger: n.logger.Named("election.sink"),
	}
	signer := &electionSigner{key: n.privateKey}
	verifier := &electionVerifier{pb: n.phonebook}

	provider := metrics.NewProvider(n.metricsManager, n.phonebook, n.id.String())

	// Wire the capsule store as the gravity target lookup so
	// self-anti-affinity (a node never runs two replicas of the same
	// capsule) works in production, and so capsule-typed placement
	// rules can be evaluated against live state.
	lookup := &electionCapsuleLookup{manager: capsMgr, localNodeID: n.id.String()}
	calc := gravity.NewCalculator(gravity.WithCapsuleTargetLookup(lookup))

	// Every cluster uses the delay strategy: each node scores only itself
	// and publishes a Claim. There is no per-cluster strategy override;
	// the strategy is the election protocol.
	delayStrat := delay.New(delay.WithLogger(n.logger.Named("election.delay")))

	groupSink := NewGroupClaimSinkAdapter(n.eventBus, n.logger.Named("election.group.sink"))

	mgr := election.NewManager(delayStrat,
		election.WithLogger(n.logger.Named("election")),
		election.WithCapsuleStore(store),
		election.WithLifecycleController(lifecycle),
		election.WithEventSink(sink),
		election.WithGroupClaimSink(groupSink),
		election.WithNodeID(n.id.String()),
		election.WithCalculator(calc),
		election.WithStateProvider(provider),
		election.WithPubSub(n.pubsub),
		election.WithSigner(signer),
		election.WithVerifier(verifier),
	)

	mgr.Start(n.ctx)

	n.electionManager = mgr
	n.electionHandler = NewElectionHandler(mgr, n.eventBus,
		WithElectionHandlerLogger(n.logger.Named("election.handler")),
		WithElectionHandlerCapsuleManager(capsMgr),
	)
	n.electionHandler.Start(n.ctx)

	// The capsule handler was constructed before the election manager
	// existed; wire the forget hook now so capsule deletions clean up
	// per-capsule election state.
	n.capsuleHandler.SetElectionForgetter(mgr)

	n.logger.Debug("election manager initialized",
		zap.String("default_strategy", delayStrat.Name()))
	return nil
}

// initializeSnapshotStore creates the snapshot SQLite store and the
// transfer server. Discovery is created per-cluster on join (because
// it needs a cluster-scoped PubSub topic). The store and transfer
// server are node-wide and created once at startup.
func (n *Node) initializeSnapshotStore() error {
	dataDir := n.dataDir
	if dataDir == "" {
		dataDir = n.defaultDataDir()
	}

	snapDir := filepath.Join(dataDir, "snapshots")
	if err := os.MkdirAll(snapDir, 0700); err != nil {
		return fmt.Errorf("snapshot: mkdir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "snapshots.db")
	store, err := snapshot.New(dbPath, snapDir)
	if err != nil {
		return fmt.Errorf("snapshot: open store: %w", err)
	}
	n.snapshotStore = store

	// Transfer server: handles incoming snapshot pull requests.
	snapshot.NewTransferServer(n.host, store,
		snapshot.WithTransferServerLogger(n.logger.Named("snapshot.transfer")))

	n.logger.Debug("snapshot store initialized",
		zap.String("dir", snapDir),
		zap.String("db", dbPath))
	return nil
}

// initializeRuntimeHandler creates the runtime.Handler that bridges
// ElectionWon events to actual container lifecycle operations. It
// uses the injected containerRuntime (set via WithRuntime) and
// connects it to the capsule store, lifecycle adapter, and event bus.
// A RuntimeBridge goroutine subscribes to ElectionWon events and
// dispatches them to the handler.
func (n *Node) initializeRuntimeHandler() {
	capsMgr := n.capsuleHandler.Manager()

	// injectDNS turns on per-group DNS flag injection only when the
	// network handler actually came up. Standalone nodes (no factory
	// wired, network.enabled=false, or build failed) skip the injection
	// so containers keep Podman's default networking — otherwise they
	// would point at a DNS listener that does not exist.
	injectDNS := n.networkHandler != nil && n.networkHandler.Enabled()

	capsuleStore := &runtimeCapsuleStoreAdapter{
		manager:   capsMgr,
		sek:       n.secretsKey,
		injectDNS: injectDNS,
	}
	lifecycle := &runtimeLifecycleAdapter{
		manager:  capsMgr,
		eventBus: n.eventBus,
		logger:   n.logger.Named("runtime.lifecycle"),
	}

	handlerOpts := []falakrt.HandlerOption{
		falakrt.WithHandlerLogger(n.logger.Named("runtime")),
		falakrt.WithCapsuleStore(capsuleStore),
		falakrt.WithLifecycleNotifier(lifecycle),
		falakrt.WithGroupView(&runtimeGroupViewAdapter{manager: capsMgr}),
		falakrt.WithGroupEventEmitter(NewRuntimeGroupEventEmitter(n.eventBus, n.id.String(), "")),
	}

	// Wire snapshot adapters if we have a snapshot store. The store is
	// created alongside the runtime handler, using a SQLite DB in the
	// node's data dir.
	if n.snapshotStore != nil {
		handlerOpts = append(handlerOpts,
			falakrt.WithSnapshotStore(&runtimeSnapshotStoreAdapter{store: n.snapshotStore}),
		)
		if n.snapshotDiscovery != nil {
			handlerOpts = append(handlerOpts,
				falakrt.WithSnapshotPuller(&runtimeSnapshotPullerAdapter{
					discovery: n.snapshotDiscovery,
					store:     n.snapshotStore,
					host:      n.host,
					logger:    n.logger.Named("runtime.snapshot"),
				}),
				falakrt.WithSnapshotBroadcaster(n.snapshotDiscovery),
			)
		}
	}

	// Wire stats into the scaling registry so scaling rules see live
	// container CPU/memory data.
	if n.capsuleHandler != nil {
		handlerOpts = append(handlerOpts,
			falakrt.WithStatsRegistry(&runtimeStatsRegistryAdapter{
				registry: n.capsuleHandler.MetricsRegistry(),
			}),
		)
	}

	h := falakrt.NewHandler(n.containerRuntime, handlerOpts...)
	h.Start(n.ctx)
	n.runtimeHandler = h

	// Allow the capsule handler to cancel parked-starts in the runtime
	// during same-node group rollback. Wired post-construction because
	// the runtime handler is built after the capsule handler.
	if n.capsuleHandler != nil {
		n.capsuleHandler.SetRuntimeRollback(h)
	}

	// Bridge ElectionWon events from the event bus to the runtime handler.
	bridge := NewRuntimeBridge(h, n.eventBus,
		WithRuntimeBridgeLogger(n.logger.Named("runtime.bridge")),
		WithRuntimeBridgeCapsuleManager(capsMgr),
	)
	bridge.Start(n.ctx)

	n.logger.Debug("runtime handler initialized")
}

// initializeNetworkHandler wires the per-cluster network subsystem
// (bridge, overlay, DNS, endpoint gossip) onto the running node. The
// handler is best-effort: when no factory is wired OR Enabled is false
// the call is a structured-log no-op, leaving the rest of the node
// running normally. The factory is invoked once per Node lifetime and
// the resulting manager is torn down in cleanup before the capsule
// handler so bridge teardowns observe live subscriptions.
func (n *Node) initializeNetworkHandler() {
	cfg := n.networkConfig
	if !n.networkConfigured {
		cfg = NetworkConfig{Enabled: false}
	}
	if cfg.LocalNodeID == "" {
		cfg.LocalNodeID = n.id.String()
	}
	if cfg.BridgeSubnetPool == "" {
		cfg.BridgeSubnetPool = DefaultBridgeSubnetPool
	}
	if cfg.DependencyTimeout <= 0 {
		cfg.DependencyTimeout = DefaultGroupDependencyTimeout
	}
	if cfg.RetryCap <= 0 {
		cfg.RetryCap = DefaultNetworkRetryCap
	}

	h := NewNetworkHandler(
		WithNetworkEnabled(cfg.Enabled),
		WithNetworkClusterPath(cfg.ClusterPath),
		WithNetworkLocalNodeID(cfg.LocalNodeID),
		WithNetworkLocalIP(cfg.LocalIP),
		WithNetworkClusterRootKey(cfg.ClusterRootKey),
		WithNetworkStatePath(cfg.StatePath),
		WithNetworkBridgePool(cfg.BridgeSubnetPool),
		WithNetworkDependencyTimeout(cfg.DependencyTimeout),
		WithNetworkRetryCap(cfg.RetryCap),
		WithNetworkHandlerEventBus(n.eventBus),
		WithNetworkHandlerLogger(n.logger.Named("network")),
		WithNetworkManagerFactory(n.networkManagerFactory),
	)
	if err := h.Start(n.ctx); err != nil {
		n.logger.Warn("network handler start failed; node continues without network subsystem",
			zap.String("cluster", cfg.ClusterPath), zap.Error(err))
		return
	}
	n.networkHandler = h
	n.logger.Debug("network handler initialized",
		zap.String("cluster", cfg.ClusterPath),
		zap.Bool("enabled", h.Enabled()))
}

// initializeServiceHandler wires the Service (traffic-management)
// subsystem onto the running node.
//
// Wiring order (matches Stop unwinding):
//
//  1. Construct the ServiceHandler with the capsule manager (lookup
//     adapter for backend resolution).
//  2. Build the proxy.ProxyManager when the network handler has
//     surfaced a bridge provider — selector/resolver/source-resolver
//     are derived from it; the handler itself becomes the proxy's
//     StrategyGetter.
//  3. Register the proxy as a bridge listener so per-group bridge
//     add / remove events drive listener bind / unbind.
//  4. Hand the proxy + the (still-nil-for-now) publisher/subscriber to
//     the handler; gossip wiring is completed per-cluster in
//     joinServiceCluster after the node has joined a cluster.
//
// Default disabled: when the operator did not call WithServiceConfig
// the handler is built with Enabled false and Start short-circuits to
// a log line. Failures at any step downgrade the subsystem rather
// than blocking node startup.
func (n *Node) initializeServiceHandler() {
	cfg := n.serviceConfig
	if !n.serviceConfigured {
		cfg = ServiceConfig{Enabled: false}
	}

	if n.capsuleHandler == nil {
		n.logger.Debug("service handler skipped: capsule handler not initialised")
		return
	}

	opts := []ServiceHandlerOption{
		WithServiceHandlerEnabled(cfg.Enabled),
		WithServiceHandlerLogger(n.logger.Named("service")),
		WithServiceHandlerCapsuleManager(n.capsuleHandler.Manager()),
	}

	h := NewServiceHandler(opts...)
	if cfg.Enabled {
		if proxyMgr := n.buildServiceProxy(h); proxyMgr != nil {
			// Install via option-pattern so the handler keeps its
			// existing single-source-of-truth for state.
			WithServiceHandlerProxyManager(proxyMgr)(h)
		}
	}
	if err := h.Start(n.ctx); err != nil {
		n.logger.Warn("service handler start failed; node continues without service subsystem",
			zap.Error(err))
		return
	}
	n.serviceHandler = h
	// Cross-wire the capsule handler so capsule receipts / deletes fan
	// out to identity binding without going through the node bus
	// (which would be observable but adds a hop).
	if n.capsuleHandler != nil {
		n.capsuleHandler.SetServiceHandler(h)
	}
	n.logger.Debug("service handler initialized", zap.Bool("enabled", cfg.Enabled))
}

// buildServiceProxy assembles the proxy.ProxyManager for the service
// handler when the network handler has surfaced a bridge provider.
// Returns nil when the dependencies are not in place — the handler
// still owns its strategy registry and event flow; only the routing
// layer is degraded.
func (n *Node) buildServiceProxy(h *ServiceHandler) *proxy.ProxyManager {
	if n.networkHandler == nil {
		n.logger.Debug("service proxy skipped: network handler not wired")
		return nil
	}
	bp := n.networkHandler.NetworkBridgeProvider()
	if bp == nil {
		n.logger.Debug("service proxy skipped: network manager exposes no bridge provider")
		return nil
	}
	registry := bp.EndpointRegistry()
	if registry == nil {
		n.logger.Debug("service proxy skipped: network manager has no endpoint registry")
		return nil
	}
	stats := proxy.NewStatsRegistry()
	selectorFor := func(_ string) *proxy.Selector {
		return proxy.NewSelector(proxy.WithRegistry(registry))
	}
	resolverFor := func(serviceID string) proxy.ServiceResolver {
		return &serviceResolverAdapter{handler: h, registry: registry, serviceID: serviceID}
	}
	pm := proxy.NewProxyManager(
		proxy.WithManagerLogger(n.logger.Named("service.proxy")),
		proxy.WithManagerStats(stats),
		proxy.WithManagerSelectorBuilder(selectorFor),
		proxy.WithManagerResolverBuilder(resolverFor),
		proxy.WithManagerRegistry(registry),
		proxy.WithManagerStrategies(h),
		proxy.WithManagerSourceResolver(bridgeSourceResolver{bp: bp}),
		proxy.WithManagerBridgeGateways(bp.BridgeGateways),
	)
	bp.RegisterBridgeListener(&proxyBridgeListener{proxy: pm, logger: n.logger.Named("service.proxy.bridge")})
	return pm
}

// ServiceHandler returns the wired ServiceHandler. May be nil when
// disabled or when initialization failed.
func (n *Node) ServiceHandler() *ServiceHandler {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.serviceHandler
}

// initializeCapsuleHandler creates the capsule manager (with persistent SQLite store)
// and the CapsuleHandler that wires it to the node event bus, PubSub, signing,
// and orbit management.
func (n *Node) initializeCapsuleHandler() error {
	dataDir := n.dataDir
	if dataDir == "" {
		dataDir = n.defaultDataDir()
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	dbPath := filepath.Join(dataDir, "capsules.db")
	store, err := capsule.OpenStore(dbPath, capsule.WithStoreLogger(n.logger.Named("capsule.store")))
	if err != nil {
		return fmt.Errorf("failed to open capsule store: %w", err)
	}

	manager := capsule.NewManager(
		capsule.WithManagerStore(store),
		capsule.WithManagerLogger(n.logger.Named("capsule.manager")),
	)

	handlerOpts := []CapsuleHandlerOption{
		WithCapsuleHandlerLogger(n.logger.Named("capsule.handler")),
		WithCapsuleHandlerEventBus(n.eventBus),
		WithCapsuleHandlerNodeID(n.id.String()),
		WithCapsuleHandlerPubSub(n.pubsub),
		WithCapsuleHandlerPrivateKey(n.privateKey),
		WithCapsuleHandlerPhonebook(n.phonebook),
	}
	if n.placementRetryCap > 0 {
		handlerOpts = append(handlerOpts, WithCapsuleHandlerPlacementRetryCap(n.placementRetryCap))
	}
	n.capsuleHandler = NewCapsuleHandler(manager, handlerOpts...)
	n.capsuleHandler.Start(n.ctx)

	n.logger.Debug("capsule handler initialized", zap.String("store", dbPath))
	return nil
}

func (n *Node) initializePhonebook() error {
	dataDir := n.dataDir
	if dataDir == "" {
		dataDir = n.defaultDataDir()
	}

	// Ensure data directory exists
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	dbPath := filepath.Join(dataDir, "phonebook.db")

	pb, err := phonebook.Open(dbPath)
	if err != nil {
		return fmt.Errorf("failed to create phonebook: %w", err)
	}
	n.phonebook = pb
	n.logger.Debug("phonebook initialized", zap.String("path", dbPath))
	return nil
}

// defaultDataDir returns the default data directory based on OS and node name.
func (n *Node) defaultDataDir() string {
	var baseDir string

	switch runtime.GOOS {
	case "darwin":
		// macOS: ~/Library/Application Support/falak/<node-name>
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		baseDir = filepath.Join(home, "Library", "Application Support", "falak")
	case "windows":
		// Windows: %APPDATA%\falak\<node-name>
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = "."
		}
		baseDir = filepath.Join(appData, "falak")
	default:
		// Linux and others: ~/.local/share/falak/<node-name>
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		baseDir = filepath.Join(home, ".local", "share", "falak")
	}

	// Include node name in path to support multiple nodes on same machine
	nodeName := n.name
	if nodeName == "" {
		nodeName = "default"
	}

	return filepath.Join(baseDir, nodeName)
}

