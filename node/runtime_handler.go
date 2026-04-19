package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/scaling"
	"github.com/tareksalem/falak/node/internal/events"
	falakrt "github.com/tareksalem/falak/runtime"
	"github.com/tareksalem/falak/shared/secrets"
	"github.com/tareksalem/falak/snapshot"
)

// runtimeCapsuleStoreAdapter satisfies runtime.CapsuleStore by mapping
// capsule.Manager lookups to the runtime.CapsuleSpec shape the handler
// expects. Keeps the runtime package free of capsule module imports.
// If sek is non-nil, encrypted registry credentials in the capsule
// spec are decrypted before being passed to the runtime handler.
type runtimeCapsuleStoreAdapter struct {
	manager *capsule.Manager
	sek     []byte // secrets encryption key; nil = no decryption
}

func (a *runtimeCapsuleStoreAdapter) GetSpec(capsuleID string) (*falakrt.CapsuleSpec, error) {
	c := a.manager.Get(capsule.CapsuleID(capsuleID))
	if c == nil {
		return nil, capsule.ErrNotFound
	}
	s := c.Spec
	spec := &falakrt.CapsuleSpec{
		Name:        s.Name,
		Image:       s.Image,
		ImageDigest: s.ImageDigest,
		Env:         s.Runtime.Env,
		Command:     s.Command,
		Resources: falakrt.ResourceLimits{
			CPUCores:    float64(s.Resources.CPUCores),
			CPUCoresMax: float64(s.Resources.EffectiveCPUMax()),
			MemoryMB:    s.Resources.MemoryMB,
			MemoryMBMax: s.Resources.EffectiveMemoryMax(),
		},
		LogRetention: falakrt.LogRetention{
			MaxFileSizeMB: s.Runtime.LogRetention.MaxFileSizeMB,
			MaxFiles:      s.Runtime.LogRetention.MaxFiles,
		},
		SnapshotTTL:  s.Runtime.SnapshotConfig.TTL,
		MaxSnapshots: s.Runtime.SnapshotConfig.MaxPerCapsule,
	}

	// Map health check.
	if s.Runtime.HealthCheck != nil {
		hc := s.Runtime.HealthCheck
		spec.HealthCheck = &falakrt.HealthCheck{
			Port:         hc.Port,
			Path:         hc.Path,
			Interval:     hc.Interval,
			Timeout:      hc.Timeout,
			Retries:      hc.Retries,
			InitialDelay: hc.InitialDelay,
		}
		switch hc.Type {
		case capsule.HealthCheckTypeEnum.HTTP():
			spec.HealthCheck.Type = falakrt.HealthCheckTypeEnum.HTTP()
		case capsule.HealthCheckTypeEnum.TCP():
			spec.HealthCheck.Type = falakrt.HealthCheckTypeEnum.TCP()
		}
	}

	// Map failure policy.
	spec.FailurePolicy.RestartLimit = s.Runtime.FailurePolicy.RestartLimit
	spec.FailurePolicy.MaxNodeAttempts = s.Runtime.FailurePolicy.MaxNodeAttempts
	spec.FailurePolicy.GracefulTimeout = s.Runtime.FailurePolicy.GracefulTimeout

	// Decrypt registry credentials if present and SEK is available.
	if s.Runtime.Registry != nil && a.sek != nil {
		if len(s.Runtime.Registry.UsernameEncrypted) > 0 {
			if u, err := secrets.DecryptString(a.sek, s.Runtime.Registry.UsernameEncrypted); err == nil {
				spec.RegistryUsername = u
			}
		}
		if len(s.Runtime.Registry.PasswordEncrypted) > 0 {
			if p, err := secrets.DecryptString(a.sek, s.Runtime.Registry.PasswordEncrypted); err == nil {
				spec.RegistryPassword = p
			}
		}
	}

	// Map network mode.
	switch s.Runtime.Network.Mode {
	case capsule.NetworkModeEnum.Host():
		spec.NetworkMode = falakrt.NetworkModeEnum.Host()
	default:
		spec.NetworkMode = falakrt.NetworkModeEnum.Bridge()
	}

	// Map port mappings.
	for _, p := range s.Runtime.Network.Ports {
		spec.Ports = append(spec.Ports, falakrt.PortMapping{
			Name:          p.Name,
			ContainerPort: p.ContainerPort,
			HostPort:      p.HostPort,
			Protocol:      p.Protocol,
		})
	}

	return spec, nil
}

// runtimeLifecycleAdapter satisfies runtime.LifecycleNotifier by
// publishing lifecycle events on the node event bus and calling the
// capsule manager's lifecycle transitions.
type runtimeLifecycleAdapter struct {
	manager  *capsule.Manager
	eventBus events.Bus
	logger   *zap.Logger
}

func (a *runtimeLifecycleAdapter) MarkRunning(capsuleID string) error {
	id := capsule.CapsuleID(capsuleID)
	if err := a.manager.StartExecution(id); err != nil {
		a.logger.Debug("StartExecution failed", zap.String("capsule_id", capsuleID), zap.Error(err))
	}
	if err := a.manager.MarkRunning(id); err != nil {
		a.logger.Debug("MarkRunning failed", zap.String("capsule_id", capsuleID), zap.Error(err))
		return err
	}
	a.eventBus.Publish(events.CapsuleRunning{
		BaseEvent: events.NewBaseEvent(),
		CapsuleID: capsuleID,
	})
	return nil
}

func (a *runtimeLifecycleAdapter) MarkFailed(capsuleID, reason string) error {
	id := capsule.CapsuleID(capsuleID)
	if err := a.manager.ExecutionFailed(id); err != nil {
		a.logger.Debug("ExecutionFailed transition failed", zap.String("capsule_id", capsuleID), zap.Error(err))
	}
	a.eventBus.Publish(events.CapsuleExecutionFailed{
		BaseEvent: events.NewBaseEvent(),
		CapsuleID: capsuleID,
		Reason:    reason,
	})
	return nil
}

func (a *runtimeLifecycleAdapter) MarkStopped(capsuleID string) error {
	id := capsule.CapsuleID(capsuleID)
	if err := a.manager.StopCapsule(id); err != nil {
		a.logger.Debug("StopCapsule failed", zap.String("capsule_id", capsuleID), zap.Error(err))
	}
	if err := a.manager.MarkStopped(id); err != nil {
		a.logger.Debug("MarkStopped failed", zap.String("capsule_id", capsuleID), zap.Error(err))
		return err
	}
	return nil
}

// RuntimeBridge subscribes to ElectionWon events on the node event bus
// and dispatches them to the runtime.Handler. It is the glue between
// the election subsystem and the container runtime.
type RuntimeBridge struct {
	handler  *falakrt.Handler
	eventBus events.Bus
	logger   *zap.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// RuntimeBridgeOption configures a RuntimeBridge.
type RuntimeBridgeOption func(*RuntimeBridge)

// WithRuntimeBridgeLogger sets the logger.
func WithRuntimeBridgeLogger(logger *zap.Logger) RuntimeBridgeOption {
	return func(b *RuntimeBridge) { b.logger = logger }
}

// NewRuntimeBridge creates a bridge between the event bus and the
// runtime handler.
func NewRuntimeBridge(handler *falakrt.Handler, bus events.Bus, opts ...RuntimeBridgeOption) *RuntimeBridge {
	b := &RuntimeBridge{
		handler:  handler,
		eventBus: bus,
		logger:   zap.NewNop(),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Start begins listening for ElectionWon events.
func (b *RuntimeBridge) Start(ctx context.Context) {
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.wg.Add(1)
	go b.loop()
}

// Stop cancels the listener and waits for clean exit.
func (b *RuntimeBridge) Stop() {
	b.cancel()
	b.wg.Wait()
}

func (b *RuntimeBridge) loop() {
	defer b.wg.Done()
	ch := b.eventBus.Subscribe(events.TypeElectionWon)
	for {
		select {
		case <-b.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			won, ok := ev.(events.ElectionWon)
			if !ok {
				continue
			}
			b.logger.Info("runtime bridge: dispatching ElectionWon",
				zap.String("capsule_id", won.CapsuleID),
				zap.String("replica_id", won.ReplicaID))

			b.handler.HandleElectionWon(falakrt.ElectionWon{
				CapsuleID:   won.CapsuleID,
				ReplicaID:   won.ReplicaID,
				ClusterPath: won.ClusterPath,
				Score:       won.Score,
				NodeID:      won.NodeID,
			})
		}
	}
}

// --- Stats adapter -------------------------------------------------------

// runtimeStatsRegistryAdapter satisfies runtime.StatsRegistry by
// bridging to scaling.MetricsRegistry. Each capsule gets a
// statsMetricsProvider that returns live CPU/memory values fed by the
// runtime handler's stats goroutine.
type runtimeStatsRegistryAdapter struct {
	registry scaling.MetricsRegistry
}

func (a *runtimeStatsRegistryAdapter) RegisterStats(capsuleID string, getCPU, getMemory func() float64) {
	a.registry.Register(capsule.CapsuleID(capsuleID), &statsMetricsProvider{
		getCPU:    getCPU,
		getMemory: getMemory,
	})
}

func (a *runtimeStatsRegistryAdapter) UnregisterStats(capsuleID string) {
	a.registry.Unregister(capsule.CapsuleID(capsuleID))
}

// statsMetricsProvider implements scaling.MetricsProvider by reading
// live values from closures updated by the stats goroutine.
type statsMetricsProvider struct {
	getCPU    func() float64
	getMemory func() float64
}

func (p *statsMetricsProvider) GetMetric(name string) (float64, bool) {
	switch name {
	case "cpu":
		return p.getCPU(), true
	case "memory":
		return p.getMemory(), true
	default:
		return 0, false
	}
}

// --- Snapshot adapters ---------------------------------------------------

// runtimeSnapshotStoreAdapter satisfies runtime.SnapshotStore by
// delegating to the snapshot.Store. Keeps the runtime package free of
// snapshot module imports.
type runtimeSnapshotStoreAdapter struct {
	store *snapshot.Store
}

func (a *runtimeSnapshotStoreAdapter) HasLocal(capsuleID, tag string) bool {
	rec, err := a.store.Get(capsuleID, tag)
	return err == nil && rec != nil
}

func (a *runtimeSnapshotStoreAdapter) SnapshotPath(capsuleID, tag string) string {
	return a.store.SnapshotPath(capsuleID, tag)
}

func (a *runtimeSnapshotStoreAdapter) RecordSnapshot(capsuleID, tag, checksum, path string, size int64, ttl time.Duration) error {
	return a.store.Put(snapshot.Record{
		CapsuleID:    capsuleID,
		Tag:          tag,
		Checksum:     checksum,
		Path:         path,
		Size:         size,
		TTL:          ttl,
		CreatedAt:    time.Now(),
		LastAccessed: time.Now(),
	})
}

func (a *runtimeSnapshotStoreAdapter) MarkInUse(capsuleID, tag string, inUse bool) error {
	return a.store.SetInUse(capsuleID, tag, inUse)
}

func (a *runtimeSnapshotStoreAdapter) TouchAccess(capsuleID, tag string) error {
	return a.store.TouchAccess(capsuleID, tag)
}

// runtimeSnapshotPullerAdapter satisfies runtime.SnapshotPuller by
// using snapshot.Discovery to find holders and snapshot.PullSnapshot
// to transfer the data.
type runtimeSnapshotPullerAdapter struct {
	discovery *snapshot.Discovery
	store     *snapshot.Store
	host      host.Host
	logger    *zap.Logger
}

func (a *runtimeSnapshotPullerAdapter) Pull(ctx context.Context, capsuleID, tag string) (string, error) {
	// First check gossip index for known holders.
	holders := a.discovery.FindHolders(capsuleID, tag)
	if len(holders) > 0 {
		for _, holderID := range holders {
			peerID, err := peer.Decode(holderID)
			if err != nil {
				continue
			}
			rec, err := snapshot.PullSnapshot(ctx, a.host, peerID, a.store, capsuleID, tag, a.logger)
			if err == nil {
				return rec.Path, nil
			}
			a.logger.Debug("snapshot pull from known holder failed",
				zap.String("peer", holderID), zap.Error(err))
		}
	}

	// Fallback: query all peers for first responder.
	peerID, err := a.discovery.QueryFirstResponder(ctx, capsuleID, tag)
	if err != nil {
		return "", fmt.Errorf("snapshot pull: no peer has %s/%s: %w", capsuleID, tag, err)
	}

	rec, err := snapshot.PullSnapshot(ctx, a.host, peerID, a.store, capsuleID, tag, a.logger)
	if err != nil {
		return "", fmt.Errorf("snapshot pull from %s: %w", peerID, err)
	}
	return rec.Path, nil
}
