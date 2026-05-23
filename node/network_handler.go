package node

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/network"
	"github.com/tareksalem/falak/network/endpoints"
	"github.com/tareksalem/falak/node/internal/events"
)

// Default network handler tunables. Subnet pool aligns with Podman's
// stock 10.88.0.0/16; dependency timeout caps the DependsOn wait;
// retry cap bounds Manager.Start retries before the subsystem is
// disabled for the node's lifetime.
const (
	DefaultBridgeSubnetPool       = "10.88.0.0/16"
	DefaultGroupDependencyTimeout = 5 * time.Minute
	DefaultNetworkRetryCap        = 3
)

// NetworkManagerLike is the narrow interface NetworkHandler depends on
// for its managed network.Manager.
type NetworkManagerLike interface {
	Start(ctx context.Context) error
	Stop() error
}

// NetworkBridgeProvider is the subset of network.Manager the service
// handler / proxy wiring depends on for bridge-level data: the live
// gateway list, source-IP-to-group resolution, the cluster identity
// (for the proxy's cross-cluster boundary check), the in-memory
// endpoint registry, and the listener seam for bridge add / remove
// callbacks. Real network.Manager satisfies it directly; tests wire a
// stub.
type NetworkBridgeProvider interface {
	BridgeGateways() []netip.Addr
	ResolveGroup(srcIP netip.Addr) (clusterPath, groupID string, ok bool)
	LocalCluster() string
	EndpointRegistry() *endpoints.Registry
	RegisterBridgeListener(l network.BridgeListener)
	UnregisterBridgeListener(l network.BridgeListener)
}

// NetworkBridgeProvider returns the bridge-level seam onto the wrapped
// network.Manager when the underlying factory built one that exposes
// the methods. Returns nil when the subsystem is disabled or the
// factory returned a stub that does not implement the interface
// (test-only fakes typically don't).
func (h *NetworkHandler) NetworkBridgeProvider() NetworkBridgeProvider {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mgr == nil {
		return nil
	}
	bp, ok := h.mgr.(NetworkBridgeProvider)
	if !ok {
		return nil
	}
	return bp
}

// NetworkManagerFactory builds a NetworkManagerLike for a given
// configuration snapshot. Nil factory disables the subsystem.
type NetworkManagerFactory func(cfg NetworkConfig, src network.EventSource, logger *zap.Logger) (NetworkManagerLike, error)

// NetworkConfig carries the per-node configuration the network handler
// needs to construct a network.Manager.
type NetworkConfig struct {
	Enabled           bool
	ClusterPath       string
	LocalNodeID       string
	LocalIP           string
	ClusterRootKey    []byte
	StatePath         string
	BridgeSubnetPool  string
	DependencyTimeout time.Duration
	RetryCap          int
}

// NetworkHandler wires the node event bus to the per-cluster
// network.Manager via networkEventAdapter (which satisfies
// network.EventSource by translating bus events into CapsuleEvents).
type NetworkHandler struct {
	cfg      NetworkConfig
	factory  NetworkManagerFactory
	eventBus events.Bus
	logger   *zap.Logger

	mu      sync.Mutex
	started bool
	stopped bool
	mgr     NetworkManagerLike
	adapter *networkEventAdapter
}

// NetworkHandlerOption configures a NetworkHandler.
type NetworkHandlerOption func(*NetworkHandler)

// WithNetworkEnabled toggles the entire subsystem. Default true.
func WithNetworkEnabled(enabled bool) NetworkHandlerOption {
	return func(h *NetworkHandler) { h.cfg.Enabled = enabled }
}

// WithNetworkClusterPath sets the cluster identity.
func WithNetworkClusterPath(p string) NetworkHandlerOption {
	return func(h *NetworkHandler) { h.cfg.ClusterPath = p }
}

// WithNetworkLocalNodeID sets the local libp2p node ID.
func WithNetworkLocalNodeID(id string) NetworkHandlerOption {
	return func(h *NetworkHandler) { h.cfg.LocalNodeID = id }
}

// WithNetworkLocalIP sets the underlay IP peers use to reach this node.
func WithNetworkLocalIP(ip string) NetworkHandlerOption {
	return func(h *NetworkHandler) { h.cfg.LocalIP = ip }
}

// WithNetworkClusterRootKey sets the HKDF-derived overlay key. Empty
// slices are ignored.
func WithNetworkClusterRootKey(k []byte) NetworkHandlerOption {
	return func(h *NetworkHandler) {
		if len(k) > 0 {
			h.cfg.ClusterRootKey = append(h.cfg.ClusterRootKey[:0:0], k...)
		}
	}
}

// WithNetworkStatePath sets the per-cluster on-disk state directory.
func WithNetworkStatePath(p string) NetworkHandlerOption {
	return func(h *NetworkHandler) { h.cfg.StatePath = p }
}

// WithNetworkBridgePool overrides DefaultBridgeSubnetPool.
func WithNetworkBridgePool(pool string) NetworkHandlerOption {
	return func(h *NetworkHandler) {
		if pool != "" {
			h.cfg.BridgeSubnetPool = pool
		}
	}
}

// WithNetworkDependencyTimeout overrides DefaultGroupDependencyTimeout.
func WithNetworkDependencyTimeout(d time.Duration) NetworkHandlerOption {
	return func(h *NetworkHandler) {
		if d > 0 {
			h.cfg.DependencyTimeout = d
		}
	}
}

// WithNetworkRetryCap overrides DefaultNetworkRetryCap.
func WithNetworkRetryCap(n int) NetworkHandlerOption {
	return func(h *NetworkHandler) {
		if n > 0 {
			h.cfg.RetryCap = n
		}
	}
}

// WithNetworkManagerFactory injects the factory the handler uses to
// construct the underlying network.Manager.
func WithNetworkManagerFactory(f NetworkManagerFactory) NetworkHandlerOption {
	return func(h *NetworkHandler) { h.factory = f }
}

// WithNetworkHandlerLogger sets the zap logger.
func WithNetworkHandlerLogger(l *zap.Logger) NetworkHandlerOption {
	return func(h *NetworkHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithNetworkHandlerEventBus binds the handler to the node event bus.
func WithNetworkHandlerEventBus(bus events.Bus) NetworkHandlerOption {
	return func(h *NetworkHandler) { h.eventBus = bus }
}

// NewNetworkHandler constructs a handler with sensible defaults.
func NewNetworkHandler(opts ...NetworkHandlerOption) *NetworkHandler {
	h := &NetworkHandler{
		cfg: NetworkConfig{
			Enabled:           true,
			BridgeSubnetPool:  DefaultBridgeSubnetPool,
			DependencyTimeout: DefaultGroupDependencyTimeout,
			RetryCap:          DefaultNetworkRetryCap,
		},
		logger: zap.NewNop(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Enabled reports whether the subsystem actually came up. Read after
// Start to distinguish "configured" from "running".
func (h *NetworkHandler) Enabled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.Enabled && h.mgr != nil
}

// Start brings the manager up and subscribes to the node bus. When
// disabled or the factory is unwired, Start is a structured-log no-op.
func (h *NetworkHandler) Start(ctx context.Context) error {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return errors.New("node: network handler already started")
	}
	if h.stopped {
		h.mu.Unlock()
		return errors.New("node: network handler already stopped")
	}
	h.started = true
	h.mu.Unlock()

	if !h.cfg.Enabled {
		h.logger.Info("network handler disabled by config",
			zap.String("cluster", h.cfg.ClusterPath))
		return nil
	}
	if h.factory == nil {
		h.logger.Warn("network handler enabled but no factory wired; subsystem skipped",
			zap.String("cluster", h.cfg.ClusterPath))
		return nil
	}
	if h.eventBus == nil {
		return errors.New("node: network handler requires WithNetworkHandlerEventBus")
	}
	if err := h.validateConfig(); err != nil {
		h.logger.Warn("network handler config invalid; subsystem skipped",
			zap.String("cluster", h.cfg.ClusterPath), zap.Error(err))
		return nil
	}

	adapter := newNetworkEventAdapter(ctx, h.eventBus, h.cfg.LocalNodeID, h.logger.Named("adapter"))
	mgr, err := h.factory(h.cfg, adapter, h.logger.Named("manager"))
	if err != nil {
		adapter.stop()
		return fmt.Errorf("network handler: build manager: %w", err)
	}
	if err := h.startManagerWithRetry(ctx, mgr); err != nil {
		adapter.stop()
		return fmt.Errorf("network handler: start manager: %w", err)
	}
	adapter.start()

	h.mu.Lock()
	h.mgr = mgr
	h.adapter = adapter
	h.mu.Unlock()

	h.logger.Info("network handler started",
		zap.String("cluster", h.cfg.ClusterPath),
		zap.String("node", h.cfg.LocalNodeID),
		zap.String("subnet_pool", h.cfg.BridgeSubnetPool))
	return nil
}

// startManagerWithRetry runs Manager.Start up to RetryCap times.
func (h *NetworkHandler) startManagerWithRetry(ctx context.Context, mgr NetworkManagerLike) error {
	var lastErr error
	for attempt := 1; attempt <= h.cfg.RetryCap; attempt++ {
		lastErr = mgr.Start(ctx)
		if lastErr == nil {
			return nil
		}
		h.logger.Warn("network manager start failed; retrying",
			zap.String("cluster", h.cfg.ClusterPath),
			zap.Int("attempt", attempt),
			zap.Int("retry_cap", h.cfg.RetryCap),
			zap.Error(lastErr))
	}
	h.logger.Error("network manager start exhausted retries; subsystem disabled for node lifetime",
		zap.String("cluster", h.cfg.ClusterPath),
		zap.Error(lastErr))
	return lastErr
}

// Stop drains the event adapter first (so no late callback fires
// against a torn-down manager) and then stops the manager. Idempotent.
func (h *NetworkHandler) Stop() error {
	h.mu.Lock()
	if !h.started || h.stopped {
		h.mu.Unlock()
		return nil
	}
	h.stopped = true
	mgr := h.mgr
	adapter := h.adapter
	h.mgr = nil
	h.adapter = nil
	h.mu.Unlock()

	if adapter != nil {
		adapter.stop()
	}
	if mgr != nil {
		if err := mgr.Stop(); err != nil {
			h.logger.Warn("network manager stop returned error",
				zap.String("cluster", h.cfg.ClusterPath), zap.Error(err))
			return err
		}
	}
	h.logger.Info("network handler stopped", zap.String("cluster", h.cfg.ClusterPath))
	return nil
}

// validateConfig enforces the invariants Start needs.
func (h *NetworkHandler) validateConfig() error {
	switch {
	case h.cfg.ClusterPath == "":
		return errors.New("cluster path required")
	case h.cfg.LocalNodeID == "":
		return errors.New("local node id required")
	case h.cfg.LocalIP == "":
		return errors.New("local ip required")
	case len(h.cfg.ClusterRootKey) == 0:
		return errors.New("cluster root key required")
	case h.cfg.StatePath == "":
		return errors.New("state path required")
	}
	return nil
}
