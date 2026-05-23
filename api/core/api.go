package core

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"go.uber.org/zap"
)

// NodeFacade is the narrow interface Core uses to access the Node's
// subsystems. The real Node satisfies it; tests can use a stub.
// Each method group corresponds to a subsystem the API exposes.
type NodeFacade interface {
	// Identity
	NodeID() string
	NodeName() string
	ListenAddrs() []string

	// Capsule operations
	CapsuleCreate(ctx context.Context, cluster string, req CreateCapsuleRequest) (*CapsuleResource, error)
	CapsuleGet(ctx context.Context, id string) (*CapsuleResource, error)
	CapsuleList(ctx context.Context, req ListCapsulesRequest) (*ListCapsulesResponse, error)
	CapsuleDelete(ctx context.Context, id string) error
	CapsuleUpdate(ctx context.Context, req UpdateCapsuleRequest) (*CapsuleResource, error)

	// Cluster operations
	ClusterJoin(ctx context.Context, req JoinClusterRequest) error
	ClusterLeave(ctx context.Context, path string) error
	ClusterList(ctx context.Context) (*ListClustersResponse, error)

	// Node operations
	NodeList(ctx context.Context, req ListNodesRequest) (*ListNodesResponse, error)
	NodeGet(ctx context.Context, cluster, nodeID string) (*NodeResource, error)

	// Watch
	WatchEvents(ctx context.Context, resourceType string) (<-chan WatchEvent, error)

	// Health
	IsReady() bool
}

// Core is the central API entry point. All business logic routes through
// here. Transport layers (gRPC, HTTP, CLI) call Core methods exclusively.
type Core struct {
	node      NodeFacade
	services  ServiceFacade
	logger    *zap.Logger
	startedAt time.Time

	mu sync.RWMutex
}

// Option configures a Core.
type Option func(*Core)

// WithLogger sets the logger.
func WithLogger(logger *zap.Logger) Option {
	return func(c *Core) { c.logger = logger }
}

// WithServices wires the ServiceFacade implementation. Optional — when
// unset, every Service-* method returns ErrUnavailable so the transport
// layer can surface "feature disabled on this node" cleanly.
func WithServices(s ServiceFacade) Option {
	return func(c *Core) { c.services = s }
}

// Services returns the wired ServiceFacade, or nil when none was set.
func (c *Core) Services() ServiceFacade {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.services
}

// New creates a Core API instance backed by the given node.
func New(node NodeFacade, opts ...Option) *Core {
	c := &Core{
		node:      node,
		logger:    zap.NewNop(),
		startedAt: time.Now(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// --- Health endpoints ----------------------------------------------------

// Healthz returns nil if the API server process is alive. Always succeeds.
func (c *Core) Healthz() error {
	return nil
}

// Readyz returns nil if the node is fully initialized and ready to serve.
func (c *Core) Readyz() error {
	if c.node == nil {
		return fmt.Errorf("%w: node not initialized", ErrUnavailable)
	}
	if !c.node.IsReady() {
		return fmt.Errorf("%w: node not ready", ErrUnavailable)
	}
	return nil
}

// --- System endpoints ----------------------------------------------------

// GetInfo returns system information about this node.
func (c *Core) GetInfo() SystemInfo {
	return SystemInfo{
		NodeID:    c.node.NodeID(),
		NodeName:  c.node.NodeName(),
		Version:   "0.1.0-alpha",
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		Uptime:    time.Since(c.startedAt),
	}
}
