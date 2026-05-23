package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ElectionWon is the event the runtime handler listens for. It mirrors
// the node event bus's ElectionWon shape but is defined here so the
// runtime package does not import the node-internal events package.
// The node wiring layer maps between the two.
type ElectionWon struct {
	CapsuleID   string
	ReplicaID   string
	ClusterPath string
	Score       float64
	NodeID      string
}

// CapsuleSpec carries the capsule configuration the handler needs to
// decide how to start the container. The node wiring layer populates
// it from the full capsule.CapsuleSpec.
type CapsuleSpec struct {
	Name         string
	Image        string
	ImageDigest  string
	Env          map[string]string
	NetworkMode  NetworkMode
	Ports        []PortMapping
	Resources    ResourceLimits
	Command       []string
	LogRetention  LogRetention
	SnapshotTTL   time.Duration
	MaxSnapshots  int
	HealthCheck   *HealthCheck
	FailurePolicy struct {
		RestartLimit    int
		MaxNodeAttempts int
		GracefulTimeout time.Duration
	}
	RegistryUsername string // decrypted by the adapter
	RegistryPassword string // decrypted by the adapter

	// DNSFlags are opaque Podman create flags (e.g. "--dns=...",
	// "--dns-search=", "--dns-option=ndots:0") that the node-side
	// adapter populates for capsules belonging to a CapsuleGroup. The
	// runtime layer passes them through via WithDNSFlags so the
	// container's /etc/resolv.conf points at the per-group DNS
	// responder. Empty for standalone capsules: those keep Podman's
	// default networking and DNS.
	DNSFlags []string
}

// CapsuleStore is the narrow interface the handler uses to look up
// capsule details. Satisfied by capsule.Manager behind an adapter in
// the node wiring layer.
type CapsuleStore interface {
	GetSpec(capsuleID string) (*CapsuleSpec, error)
}

// SnapshotStore is the narrow interface for checking and recording
// local snapshots. Satisfied by snapshot.Store behind an adapter.
type SnapshotStore interface {
	HasLocal(capsuleID, tag string) bool
	SnapshotPath(capsuleID, tag string) string
	RecordSnapshot(capsuleID, tag, checksum, path string, size int64, ttl time.Duration) error
	MarkInUse(capsuleID, tag string, inUse bool) error
	TouchAccess(capsuleID, tag string) error
}

// SnapshotPuller pulls a snapshot from a remote peer. Satisfied by the
// snapshot.Discovery + snapshot.PullSnapshot wiring.
type SnapshotPuller interface {
	// Pull fetches a snapshot from the mesh and returns the local path.
	// Returns ("", nil) if no peer has the snapshot.
	Pull(ctx context.Context, capsuleID, tag string) (path string, err error)
}

// LifecycleNotifier is the callback interface the handler uses to
// update capsule lifecycle state and broadcast to the mesh.
type LifecycleNotifier interface {
	MarkRunning(capsuleID string) error
	MarkFailed(capsuleID, reason string) error
	MarkStopped(capsuleID string) error
}

// GroupEventEmitter is the narrow interface the runtime handler uses
// to publish group-aware events onto the node event bus without
// importing the node-internal events package. Satisfied by an adapter
// in the node wiring layer.
//
// MemberPlacementFailed is emitted INSTEAD of the per-replica
// MarkFailed path when a member of a same-node group fails its initial
// start sequence. The node-side capsule handler drives same-node
// rollback (stop siblings, release reservation, request re-election
// with ExcludeNodes) from this event.
//
// PullProgress is emitted as a heartbeat during image pulls. The
// election manager treats each heartbeat as "alive, extend the
// capacity reservation deadline".
type GroupEventEmitter interface {
	// EmitMemberPlacementFailed signals that a same-node group member
	// could not be started locally. The runtime handler computes
	// groupID via GroupView; capsuleID is the failing member; reason
	// carries a human-readable explanation.
	EmitMemberPlacementFailed(groupID, capsuleID, reason string)

	// EmitPullProgress is a periodic heartbeat fired while an image
	// pull is in flight. bytesRemaining and bytesPerSecond are
	// best-effort progress estimates; pass -1 / 0 to signal "alive,
	// progress unknown". groupID is empty for standalone capsules.
	EmitPullProgress(groupID, capsuleID string, bytesRemaining, bytesPerSecond int64)
}

// containerID builds a deterministic container name from capsule +
// replica IDs.
func containerID(capsuleID, replicaID string) string {
	return fmt.Sprintf("falak-%s-%s", capsuleID, replicaID)
}

// computeFileChecksum returns the SHA-256 hex digest and byte size of
// a file. Returns empty string and 0 on any error (non-fatal).
func computeFileChecksum(path string) (string, int64) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0
	}
	return hex.EncodeToString(h.Sum(nil)), n
}

// SnapshotBroadcaster is the narrow interface used to announce snapshot
// availability to the mesh after a cold-start capture. Satisfied by
// snapshot.Discovery.
type SnapshotBroadcaster interface {
	BroadcastAvailable(capsuleID, tag, checksum string, size int64) error
}

// StatsRegistry is the narrow interface for registering per-capsule
// metrics providers. The runtime handler feeds container stats into
// this registry so scaling rules can evaluate live data. Satisfied by
// scaling.MetricsRegistry behind an adapter.
type StatsRegistry interface {
	RegisterStats(capsuleID string, getCPU, getMemory func() float64)
	UnregisterStats(capsuleID string)
}

// Handler bridges election outcomes to the container runtime. It
// subscribes to ElectionWon events, decides whether to restore from
// snapshot or cold-start, manages the container lifecycle, and reports
// outcomes back to the capsule lifecycle.
type Handler struct {
	runtime           Runtime
	capsuleStore      CapsuleStore
	snapshotStore     SnapshotStore
	snapshotPuller    SnapshotPuller
	snapshotBC        SnapshotBroadcaster
	lifecycle         LifecycleNotifier
	statsRegistry     StatsRegistry
	groupView         GroupView
	groupEmitter      GroupEventEmitter
	depTimeout        time.Duration
	pullHeartbeatTick time.Duration
	logger            *zap.Logger

	mu      sync.Mutex
	running map[string]context.CancelFunc // containerID -> cancel for stats/watcher goroutine

	// groupDispatched tracks capsule IDs that were dispatched as part
	// of a StartGroup call. Their start failures route to
	// MemberPlacementFailed rollback (driven by the node-side capsule
	// handler) rather than the per-replica MarkFailed path. Cleared
	// when the capsule reaches Running so a future crash-and-retry
	// goes through the normal per-replica flow.
	groupDispatched map[string]struct{}

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// group is the parked-start coordinator. nil when the handler runs
	// without a GroupView (e.g. tests with no group module).
	group *groupCoord
}

// HandlerOption configures a Handler.
type HandlerOption func(*Handler)

// WithHandlerLogger sets the logger.
func WithHandlerLogger(logger *zap.Logger) HandlerOption {
	return func(h *Handler) { h.logger = logger }
}

// WithCapsuleStore wires the capsule spec lookup.
func WithCapsuleStore(s CapsuleStore) HandlerOption {
	return func(h *Handler) { h.capsuleStore = s }
}

// WithSnapshotStore wires the local snapshot store.
func WithSnapshotStore(s SnapshotStore) HandlerOption {
	return func(h *Handler) { h.snapshotStore = s }
}

// WithSnapshotPuller wires the remote snapshot fetcher.
func WithSnapshotPuller(p SnapshotPuller) HandlerOption {
	return func(h *Handler) { h.snapshotPuller = p }
}

// WithLifecycleNotifier wires the capsule lifecycle callback.
func WithLifecycleNotifier(n LifecycleNotifier) HandlerOption {
	return func(h *Handler) { h.lifecycle = n }
}

// WithSnapshotBroadcaster wires the gossip broadcaster for snapshot
// availability. Called after a cold-start capture to announce the
// snapshot to the mesh.
func WithSnapshotBroadcaster(bc SnapshotBroadcaster) HandlerOption {
	return func(h *Handler) { h.snapshotBC = bc }
}

// WithStatsRegistry wires the per-capsule metrics registry so
// container stats feed into scaling rule evaluation.
func WithStatsRegistry(sr StatsRegistry) HandlerOption {
	return func(h *Handler) { h.statsRegistry = sr }
}

// WithGroupView wires the group dependency lookup. When set, the
// handler parks ElectionWon events for capsules whose DependsOn list
// includes siblings not yet Running. Without it, every capsule starts
// immediately on ElectionWon (the Phase-9 behaviour).
func WithGroupView(v GroupView) HandlerOption {
	return func(h *Handler) { h.groupView = v }
}

// WithGroupEventEmitter wires the emitter the handler uses to publish
// group-aware events (MemberPlacementFailed for rollback,
// PullProgress for reservation-deadline extension). When unset, the
// runtime handler falls back to the per-replica MarkFailed path for
// every start failure regardless of group membership; suitable for
// tests that exercise the standalone-capsule code paths.
func WithGroupEventEmitter(e GroupEventEmitter) HandlerOption {
	return func(h *Handler) { h.groupEmitter = e }
}

// WithPullHeartbeatInterval overrides the interval between PullProgress
// heartbeats emitted during image pulls. Non-positive values are
// ignored. Default is 10 seconds; very small values are used by tests
// to assert that the heartbeat actually fires without waiting for the
// production interval.
func WithPullHeartbeatInterval(d time.Duration) HandlerOption {
	return func(h *Handler) {
		if d > 0 {
			h.pullHeartbeatTick = d
		}
	}
}

// WithDependencyTimeout overrides the default group-dependency wait
// (5 minutes). When the timeout fires before all DependsOn members
// reach Running, the parked start is dropped and MarkFailed is called
// so the lifecycle re-elects on a different node. Non-positive values
// are ignored (default kept).
func WithDependencyTimeout(d time.Duration) HandlerOption {
	return func(h *Handler) {
		if d > 0 {
			h.depTimeout = d
		}
	}
}

// defaultPullHeartbeatInterval bounds how long the handler waits
// between PullProgress heartbeats during a long-running image pull.
// 10s matches the election manager's typical reservation-watchdog
// granularity so a missed heartbeat for a single interval cannot by
// itself trip a reservation timeout.
const defaultPullHeartbeatInterval = 10 * time.Second

// NewHandler constructs a runtime handler.
func NewHandler(rt Runtime, opts ...HandlerOption) *Handler {
	h := &Handler{
		runtime:           rt,
		logger:            zap.NewNop(),
		running:           make(map[string]context.CancelFunc),
		groupDispatched:   make(map[string]struct{}),
		pullHeartbeatTick: defaultPullHeartbeatInterval,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// markGroupDispatched marks every member capsule listed in ids as
// "dispatched via StartGroup" so that a start failure for that
// capsule routes to MemberPlacementFailed rather than the per-replica
// MarkFailed path. The groupID argument is logged for observability;
// the flag itself is keyed per-capsule (a member belongs to exactly
// one group).
func (h *Handler) markGroupDispatched(groupID string, ids []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		h.groupDispatched[id] = struct{}{}
	}
}

// isGroupDispatched reports whether the given capsule was last
// dispatched through StartGroup (i.e. its start is part of a same-node
// group placement). The flag is consulted by reportStartFailure to
// decide between the MemberPlacementFailed rollback path and the
// per-replica MarkFailed path.
func (h *Handler) isGroupDispatched(capsuleID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.groupDispatched[capsuleID]
	return ok
}

// clearGroupDispatched removes the StartGroup flag for a capsule.
// Called when the capsule reaches Running (a successful start ends
// the group-placement window) AND when the rollback path clears the
// flag explicitly so a stale flag does not survive into a future
// election round.
func (h *Handler) clearGroupDispatched(capsuleID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.groupDispatched, capsuleID)
}

// Start begins the handler's background goroutines.
func (h *Handler) Start(ctx context.Context) {
	h.ctx, h.cancel = context.WithCancel(ctx)

	// Initialize the group coordinator now that we have the long-lived
	// context. The coordinator's per-park goroutines derive from h.ctx
	// so Stop cancels them cleanly.
	if h.groupView != nil {
		h.group = newGroupCoord(
			h.groupView,
			h.depTimeout,
			h.logger.Named("group"),
			h.startContainerNow,
			func(capsuleID, reason string) {
				if h.lifecycle != nil {
					if err := h.lifecycle.MarkFailed(capsuleID, reason); err != nil {
						h.logger.Warn("group dep timeout: MarkFailed failed",
							zap.String("capsule_id", capsuleID),
							zap.Error(err))
					}
				}
			},
			&h.wg,
		)
	}
}

// Stop cancels all container watchers and waits for clean exit.
func (h *Handler) Stop() {
	h.cancel()
	if h.group != nil {
		h.group.stop()
	}
	h.mu.Lock()
	for id, cancel := range h.running {
		cancel()
		delete(h.running, id)
	}
	h.mu.Unlock()
	h.wg.Wait()
}

// HandleElectionWon is called when the local node wins an election for
// a capsule replica. When the capsule is a group member with unmet
// dependencies, the start is parked until every DependsOn entry reaches
// Running. Otherwise (standalone capsule, or all deps already Running)
// the container starts immediately.
//
// The parked-start path enforces first-boot-only dependency semantics:
// once a capsule has been released past first boot, future re-elections
// skip dep gating entirely. This prevents cascade-restart storms when
// a long-running dependency crashes and re-elects elsewhere.
func (h *Handler) HandleElectionWon(event ElectionWon) {
	if h.group != nil {
		if waiting := h.group.shouldPark(event); len(waiting) > 0 {
			h.group.park(h.ctx, event, waiting)
			return
		}
	}
	h.startContainerNow(event)
}

// OnDependencyRunning is the hook the node-side bridge calls when a
// capsule reaches Running on the local view. Idempotent for unrelated
// capsules. When the running capsule is a group member, parked siblings
// waiting on it have their waiting set updated and may release.
func (h *Handler) OnDependencyRunning(capsuleID string) {
	if h.group != nil {
		h.group.onDependencyRunning(capsuleID)
	}
}

// startContainerNow launches the per-replica goroutine that drives the
// container start. Used both by HandleElectionWon for capsules with no
// dependency wait and by the group coordinator when a parked start is
// released.
func (h *Handler) startContainerNow(event ElectionWon) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.startContainer(event)
	}()
}

// startContainer is the main runtime flow. It runs in a goroutine per
// replica.
func (h *Handler) startContainer(event ElectionWon) {
	capsuleID := event.CapsuleID
	replicaID := event.ReplicaID
	cID := containerID(capsuleID, replicaID)

	spec, err := h.capsuleStore.GetSpec(capsuleID)
	if err != nil {
		h.logger.Error("runtime: capsule spec lookup failed",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
		h.reportStartFailure(capsuleID, "spec lookup failed: "+err.Error())
		return
	}

	tag := spec.ImageDigest
	if tag == "" {
		tag = spec.Image
	}

	ctx := h.ctx

	// Decision: local snapshot → restore. Pull from peer → restore.
	// No snapshot → cold start + capture.
	if h.snapshotStore != nil && h.snapshotStore.HasLocal(capsuleID, tag) {
		h.restoreFromSnapshot(ctx, cID, capsuleID, tag, spec)
		return
	}

	if h.snapshotPuller != nil {
		path, pullErr := h.snapshotPuller.Pull(ctx, capsuleID, tag)
		if pullErr == nil && path != "" {
			h.restoreFromSnapshot(ctx, cID, capsuleID, tag, spec)
			return
		}
		if pullErr != nil {
			h.logger.Debug("runtime: snapshot pull failed, falling back to cold start",
				zap.String("capsule_id", capsuleID),
				zap.Error(pullErr))
		}
	}

	h.coldStart(ctx, cID, capsuleID, tag, spec)
}

// restoreFromSnapshot restores a container from a local snapshot.
func (h *Handler) restoreFromSnapshot(ctx context.Context, cID, capsuleID, tag string, spec *CapsuleSpec) {
	snapPath := h.snapshotStore.SnapshotPath(capsuleID, tag)

	h.logger.Info("runtime: restoring from snapshot",
		zap.String("container_id", cID),
		zap.String("capsule_id", capsuleID),
		zap.String("snapshot_path", snapPath))

	if err := h.runtime.Restore(ctx, cID, snapPath,
		WithRestoreNetworkMode(spec.NetworkMode),
		WithRestorePortMappings(spec.Ports...),
		WithRestoreEnv(spec.Env),
	); err != nil {
		h.logger.Error("runtime: restore failed, falling back to cold start",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
		h.coldStart(ctx, cID, capsuleID, tag, spec)
		return
	}

	h.snapshotStore.MarkInUse(capsuleID, tag, true)
	h.snapshotStore.TouchAccess(capsuleID, tag)
	h.onContainerRunning(cID, capsuleID, spec)
}

// coldStart pulls the image, creates and starts the container from
// scratch, then captures a snapshot for future fast restarts.
func (h *Handler) coldStart(ctx context.Context, cID, capsuleID, tag string, spec *CapsuleSpec) {
	h.logger.Info("runtime: cold starting",
		zap.String("container_id", cID),
		zap.String("capsule_id", capsuleID),
		zap.String("image", spec.Image))

	// Pull image (with registry credentials if provided).
	var pullOpts []PullOption
	if spec.RegistryUsername != "" {
		pullOpts = append(pullOpts, WithRegistryAuth(spec.RegistryUsername, spec.RegistryPassword))
	}
	if err := h.pullWithHeartbeat(ctx, capsuleID, spec.Image, pullOpts...); err != nil {
		h.logger.Error("runtime: image pull failed",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
		h.reportStartFailure(capsuleID, "image pull failed: "+err.Error())
		return
	}

	// Create container.
	createOpts := []CreateOption{
		WithNetworkMode(spec.NetworkMode),
		WithPortMappings(spec.Ports...),
		WithResourceLimits(spec.Resources),
		WithEnv(spec.Env),
		WithLogRetention(spec.LogRetention),
	}
	if len(spec.Command) > 0 {
		createOpts = append(createOpts, WithCommand(spec.Command...))
	}
	if len(spec.DNSFlags) > 0 {
		createOpts = append(createOpts, WithDNSFlags(spec.DNSFlags))
	}

	if err := h.runtime.Create(ctx, cID, spec.Image, createOpts...); err != nil {
		h.logger.Error("runtime: create failed",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
		h.reportStartFailure(capsuleID, "create failed: "+err.Error())
		return
	}

	// Start container.
	if err := h.runtime.Start(ctx, cID); err != nil {
		h.logger.Error("runtime: start failed",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
		h.runtime.Remove(ctx, cID)
		h.reportStartFailure(capsuleID, "start failed: "+err.Error())
		return
	}

	h.onContainerRunning(cID, capsuleID, spec)

	// Capture snapshot in the background for future fast restarts.
	if h.snapshotStore != nil {
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			h.captureSnapshot(capsuleID, cID, tag, spec)
		}()
	}
}

// captureSnapshot checkpoints the running container, records the
// snapshot, and broadcasts availability. The container is restarted
// after checkpoint (CRIU stops it during capture).
func (h *Handler) captureSnapshot(capsuleID, cID, tag string, spec *CapsuleSpec) {
	ctx := h.ctx

	snapPath := h.snapshotStore.SnapshotPath(capsuleID, tag)

	h.logger.Info("runtime: capturing snapshot",
		zap.String("capsule_id", capsuleID),
		zap.String("container_id", cID),
		zap.String("path", snapPath))

	if err := h.runtime.Checkpoint(ctx, cID, snapPath); err != nil {
		h.logger.Warn("runtime: snapshot capture failed (non-fatal, container continues)",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
		return
	}

	ttl := spec.SnapshotTTL
	if ttl == 0 {
		ttl = 72 * time.Hour
	}

	// Compute SHA-256 and size of the snapshot so the record and
	// broadcast carry meaningful integrity data.
	checksum, snapSize := computeFileChecksum(snapPath)

	if err := h.snapshotStore.RecordSnapshot(capsuleID, tag, checksum, snapPath, snapSize, ttl); err != nil {
		h.logger.Warn("runtime: record snapshot failed",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
	}

	// Restart the container after checkpoint (CRIU stops it). If the
	// restart fails, the container is dead — cancel the watcher and
	// report failure so the election system can re-place it.
	if err := h.runtime.Start(ctx, cID); err != nil {
		h.logger.Error("runtime: restart after checkpoint failed",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))

		h.mu.Lock()
		if cancel, ok := h.running[cID]; ok {
			cancel()
			delete(h.running, cID)
		}
		h.mu.Unlock()
		h.lifecycle.MarkFailed(capsuleID, "restart after checkpoint failed: "+err.Error())
		return
	}

	// Announce to the mesh that this node now has the snapshot.
	if h.snapshotBC != nil {
		if err := h.snapshotBC.BroadcastAvailable(capsuleID, tag, checksum, snapSize); err != nil {
			h.logger.Warn("runtime: snapshot broadcast failed",
				zap.String("capsule_id", capsuleID),
				zap.Error(err))
		}
	}

	h.logger.Info("runtime: snapshot captured",
		zap.String("capsule_id", capsuleID),
		zap.String("tag", tag))
}

// onContainerRunning is called after a container is successfully started
// (either via restore or cold start). It marks the capsule as running,
// starts a health checker if configured, and starts a background watcher
// for crashes.
func (h *Handler) onContainerRunning(cID, capsuleID string, spec *CapsuleSpec) {
	h.logger.Info("runtime: container running",
		zap.String("container_id", cID),
		zap.String("capsule_id", capsuleID))

	// Clear the StartGroup dispatch flag now that the member has
	// reached Running — a future crash + per-replica re-election is
	// no longer part of the original group placement window.
	h.clearGroupDispatched(capsuleID)

	if err := h.lifecycle.MarkRunning(capsuleID); err != nil {
		h.logger.Warn("runtime: MarkRunning failed",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
	}

	// Start a background goroutine to watch for container exit. If a
	// watcher already exists for this container ID (e.g. due to event
	// bus redelivery), cancel the old one first to prevent orphaned
	// goroutines.
	watchCtx, watchCancel := context.WithCancel(h.ctx)
	h.mu.Lock()
	if oldCancel, exists := h.running[cID]; exists {
		h.logger.Warn("runtime: replacing existing watcher for container",
			zap.String("container_id", cID))
		oldCancel()
	}
	h.running[cID] = watchCancel
	h.mu.Unlock()

	// Start health checker if the capsule spec defines one.
	if spec != nil && spec.HealthCheck != nil {
		// Determine the container's address based on network mode.
		// Host mode: probe on localhost. Bridge mode: query the
		// container's assigned IP from the runtime.
		containerAddr := "127.0.0.1"
		if spec.NetworkMode == NetworkModeEnum.Bridge() {
			info, err := h.runtime.Inspect(h.ctx, cID)
			if err == nil && info.IP != "" {
				containerAddr = info.IP
			}
		}

		hc := NewHealthChecker(*spec.HealthCheck,
			WithHealthCheckerLogger(h.logger.Named("healthcheck")))
		hc.Start(watchCtx, containerAddr, func() {
			h.logger.Warn("runtime: health check failed, marking container unhealthy",
				zap.String("container_id", cID),
				zap.String("capsule_id", capsuleID),
				zap.String("address", containerAddr))
			h.lifecycle.MarkFailed(capsuleID, "health check exhausted retries")
			watchCancel()
		})
	}

	// Start stats collection if a registry is wired. Reads from the
	// runtime Stats channel and updates the registry so scaling rules
	// can evaluate live CPU/memory data.
	if h.statsRegistry != nil {
		var lastCPU, lastMem float64
		var statsMu sync.Mutex

		h.statsRegistry.RegisterStats(capsuleID,
			func() float64 { statsMu.Lock(); defer statsMu.Unlock(); return lastCPU },
			func() float64 { statsMu.Lock(); defer statsMu.Unlock(); return lastMem },
		)

		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			defer h.statsRegistry.UnregisterStats(capsuleID)
			ch, err := h.runtime.Stats(watchCtx, cID)
			if err != nil {
				h.logger.Debug("runtime: stats stream failed",
					zap.String("container_id", cID), zap.Error(err))
				return
			}
			for s := range ch {
				statsMu.Lock()
				lastCPU = s.CPUPercent
				lastMem = float64(s.MemoryMB)
				statsMu.Unlock()
			}
		}()
	}

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		restartLimit := 0
		if spec != nil {
			restartLimit = spec.FailurePolicy.RestartLimit
		}
		h.watchContainer(watchCtx, cID, capsuleID, restartLimit)
	}()
}

// watchContainer polls the container status and detects crashes. When
// the container exits unexpectedly, it attempts local restarts up to
// restartLimit times before reporting failure (which triggers
// re-election to another node).
func (h *Handler) watchContainer(ctx context.Context, cID, capsuleID string, restartLimit int) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	localRestarts := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := h.runtime.Inspect(ctx, cID)
			if err != nil {
				continue // transient inspect error, retry
			}
			if info.Status != ContainerStatusEnum.Failed() &&
				info.Status != ContainerStatusEnum.Stopped() {
				continue // still running
			}

			h.logger.Warn("runtime: container exited",
				zap.String("container_id", cID),
				zap.String("capsule_id", capsuleID),
				zap.String("status", string(info.Status)),
				zap.Int("exit_code", info.ExitCode),
				zap.Int("local_restarts", localRestarts),
				zap.Int("restart_limit", restartLimit))

			// Attempt local restart if under the limit.
			if localRestarts < restartLimit {
				localRestarts++
				h.logger.Info("runtime: attempting local restart",
					zap.String("container_id", cID),
					zap.String("capsule_id", capsuleID),
					zap.Int("attempt", localRestarts))

				if err := h.runtime.Start(ctx, cID); err != nil {
					h.logger.Warn("runtime: local restart failed",
						zap.String("container_id", cID),
						zap.Error(err))
					// Fall through to MarkFailed below.
				} else {
					continue // restart succeeded, keep watching
				}
			}

			// Local restarts exhausted (or restart failed) — report
			// failure so the capsule handler fires a re-election.
			h.mu.Lock()
			delete(h.running, cID)
			h.mu.Unlock()

			h.lifecycle.MarkFailed(capsuleID, fmt.Sprintf(
				"container exited with code %d after %d local restarts",
				info.ExitCode, localRestarts))
			return
		}
	}
}

// RollingUpdate performs a zero-downtime update for a capsule. It starts
// a new container with the updated spec, waits for it to become healthy,
// then stops the old container. If the new container fails to start or
// pass health checks, the old container remains running and the update
// is aborted.
//
// oldReplicaID is the replica currently running. newSpec is the updated
// capsule spec (new image, env, etc.). The new container gets the same
// replica ID — the old container is renamed internally during the swap.
func (h *Handler) RollingUpdate(capsuleID, replicaID string, newSpec *CapsuleSpec) error {
	oldCID := containerID(capsuleID, replicaID)
	newCID := containerID(capsuleID, replicaID) + "-new"
	ctx := h.ctx

	h.logger.Info("runtime: rolling update starting",
		zap.String("capsule_id", capsuleID),
		zap.String("replica_id", replicaID),
		zap.String("new_image", newSpec.Image))

	// Pull new image.
	if err := h.runtime.Pull(ctx, newSpec.Image); err != nil {
		return fmt.Errorf("rolling update: pull failed: %w", err)
	}

	// Create and start the new container.
	createOpts := []CreateOption{
		WithNetworkMode(newSpec.NetworkMode),
		WithResourceLimits(newSpec.Resources),
		WithEnv(newSpec.Env),
		WithLogRetention(newSpec.LogRetention),
	}
	if len(newSpec.Command) > 0 {
		createOpts = append(createOpts, WithCommand(newSpec.Command...))
	}
	if len(newSpec.DNSFlags) > 0 {
		createOpts = append(createOpts, WithDNSFlags(newSpec.DNSFlags))
	}

	if err := h.runtime.Create(ctx, newCID, newSpec.Image, createOpts...); err != nil {
		return fmt.Errorf("rolling update: create failed: %w", err)
	}

	if err := h.runtime.Start(ctx, newCID); err != nil {
		h.runtime.Remove(ctx, newCID)
		return fmt.Errorf("rolling update: start failed: %w", err)
	}

	// Wait for the new container to become healthy before stopping the
	// old one. If a health check is configured, run it and wait. If
	// no health check, fall back to a short stabilization wait + inspect.
	if newSpec.HealthCheck != nil {
		containerAddr := "127.0.0.1"
		if newSpec.NetworkMode == NetworkModeEnum.Bridge() {
			if info, err := h.runtime.Inspect(ctx, newCID); err == nil && info.IP != "" {
				containerAddr = info.IP
			}
		}

		healthy := make(chan struct{}, 1)
		hcCtx, hcCancel := context.WithTimeout(ctx, 30*time.Second)
		defer hcCancel()

		hc := NewHealthChecker(*newSpec.HealthCheck,
			WithHealthCheckerLogger(h.logger.Named("healthcheck.rolling")))
		hc.Start(hcCtx, containerAddr, func() {
			// Unhealthy — signal failure.
			hcCancel()
		})

		// Wait for either initial_delay + first successful probe, or timeout.
		probeWait := newSpec.HealthCheck.InitialDelay + newSpec.HealthCheck.Interval*time.Duration(newSpec.HealthCheck.Retries+1)
		if probeWait > 30*time.Second {
			probeWait = 30 * time.Second
		}

		select {
		case <-time.After(probeWait):
			// Check if container is still running after the probe window.
			info, err := h.runtime.Inspect(ctx, newCID)
			if err != nil || info.Status != ContainerStatusEnum.Running() {
				hc.Stop()
				// Use a fresh context for cleanup — the original ctx may
				// already be cancelled (e.g. handler shutting down).
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
				h.runtime.Stop(cleanupCtx, newCID)
				h.runtime.Remove(cleanupCtx, newCID)
				cleanupCancel()
				return fmt.Errorf("rolling update: new container not healthy after probe window, aborting")
			}
			healthy <- struct{}{}
		case <-hcCtx.Done():
			hc.Stop()
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
			h.runtime.Stop(cleanupCtx, newCID)
			h.runtime.Remove(cleanupCtx, newCID)
			cleanupCancel()
			return fmt.Errorf("rolling update: health check failed or timed out, aborting")
		}
		hc.Stop()
		<-healthy
	} else {
		// No health check — wait briefly and verify running.
		time.Sleep(2 * time.Second)
		info, err := h.runtime.Inspect(ctx, newCID)
		if err != nil || info.Status != ContainerStatusEnum.Running() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
			h.runtime.Stop(cleanupCtx, newCID)
			h.runtime.Remove(cleanupCtx, newCID)
			cleanupCancel()
			return fmt.Errorf("rolling update: new container not healthy, aborting")
		}
	}

	// Stop the old container.
	h.mu.Lock()
	if cancel, ok := h.running[oldCID]; ok {
		cancel()
		delete(h.running, oldCID)
	}
	h.mu.Unlock()

	h.runtime.Stop(ctx, oldCID, WithGracePeriod(10*time.Second))
	h.runtime.Remove(ctx, oldCID)

	// Track the new container under the original ID for future watches.
	h.onContainerRunning(newCID, capsuleID, newSpec)

	h.logger.Info("runtime: rolling update complete",
		zap.String("capsule_id", capsuleID),
		zap.String("replica_id", replicaID))

	return nil
}

// StreamingPuller is the capability interface a Runtime may optionally
// implement to expose byte-level pull progress. The handler uses a type
// assertion at pull time: runtimes that implement StreamingPuller (the
// Podman REST runtime is the only production implementation today) feed
// real (current, total) progress through the heartbeat; runtimes that do
// not (the mock runtime, future containerd backend) keep the
// progress-unknown fallback.
type StreamingPuller interface {
	// PullStreaming pulls image and invokes onProgress on every progress
	// line emitted by the daemon. onProgress must not block — the handler
	// rate-limits emission separately so the callback can fire on every
	// daemon tick without flooding the event bus.
	PullStreaming(ctx context.Context, image string, onProgress func(current, total int64), opts ...PullOption) error
}

// pullProgressMinEmitInterval is the minimum wall-clock gap between two
// EmitPullProgress calls driven by the StreamingPuller callback. Without
// this rate-limit a fast registry stream could fan out hundreds of
// PullProgress events per second; the election manager only needs one
// per second to extend the reservation deadline cleanly.
const pullProgressMinEmitInterval = 1 * time.Second

// pullWithHeartbeat wraps runtime.Pull so that long-running pulls emit
// PullProgress heartbeats every h.pullHeartbeatTick on the configured
// GroupEventEmitter. The election manager treats each heartbeat as
// "alive, extend the capacity reservation deadline" so genuinely slow
// pulls do not falsely trip the reservation watchdog.
//
// When the underlying runtime implements StreamingPuller the heartbeat
// carries real (BytesRemaining, BytesPerSecond) derived from the
// daemon's progress feed; emission is rate-limited to one per second so
// the event bus is not flooded. Runtimes that do not implement
// StreamingPuller (the mock runtime, custom backends) fall back to the
// periodic "-1, 0" heartbeat — the election manager only needs liveness
// in that case.
//
// A heartbeat is emitted immediately at start and on completion in
// addition to the periodic ticks; this is what gives the election
// manager an extension even for pulls that complete inside one tick.
func (h *Handler) pullWithHeartbeat(ctx context.Context, capsuleID, image string, opts ...PullOption) error {
	emit := h.groupEmitter
	groupID := ""
	if emit != nil && h.groupView != nil {
		if info, ok := h.groupView.MemberInfo(capsuleID); ok {
			groupID = info.GroupID
		}
	}

	// Initial heartbeat. The emitter handles its own goroutine
	// scheduling so this call is non-blocking from the runtime's
	// perspective even if the event bus has a slow subscriber.
	if emit != nil {
		emit.EmitPullProgress(groupID, capsuleID, -1, 0)
	}

	tickCtx, tickCancel := context.WithCancel(ctx)
	defer tickCancel()
	done := make(chan struct{})

	// Periodic-heartbeat fallback. Always running so liveness is
	// reported even when the runtime supports streaming but the
	// daemon's progress feed stalls between two emit ticks.
	if emit != nil && h.pullHeartbeatTick > 0 {
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			ticker := time.NewTicker(h.pullHeartbeatTick)
			defer ticker.Stop()
			for {
				select {
				case <-tickCtx.Done():
					return
				case <-done:
					return
				case <-ticker.C:
					emit.EmitPullProgress(groupID, capsuleID, -1, 0)
				}
			}
		}()
	}

	var err error
	if streamer, ok := h.runtime.(StreamingPuller); ok && emit != nil {
		var (
			progressMu  sync.Mutex
			lastCurrent int64
			lastEmitTS  time.Time
		)
		onProgress := func(current, total int64) {
			if current <= 0 || total <= 0 {
				return // liveness-only line; the periodic ticker covers it
			}
			progressMu.Lock()
			now := time.Now()
			if !lastEmitTS.IsZero() && now.Sub(lastEmitTS) < pullProgressMinEmitInterval {
				progressMu.Unlock()
				return
			}
			deltaBytes := current - lastCurrent
			deltaSecs := now.Sub(lastEmitTS).Seconds()
			lastCurrent = current
			lastEmitTS = now
			progressMu.Unlock()

			bytesRemaining := total - current
			if bytesRemaining < 0 {
				bytesRemaining = 0
			}
			var bps int64
			if deltaSecs > 0 && deltaBytes > 0 {
				bps = int64(float64(deltaBytes) / deltaSecs)
			}
			emit.EmitPullProgress(groupID, capsuleID, bytesRemaining, bps)
		}
		err = streamer.PullStreaming(ctx, image, onProgress, opts...)
	} else {
		err = h.runtime.Pull(ctx, image, opts...)
	}
	close(done)

	// Completion heartbeat: signals the reservation watchdog one last
	// time so the deadline reflects the actual pull duration.
	if emit != nil {
		emit.EmitPullProgress(groupID, capsuleID, 0, 0)
	}

	return err
}

// reportStartFailure routes a startup failure either to the
// MemberPlacementFailed group-rollback path (when the capsule was
// dispatched via StartGroup AND belongs to a same-node group AND a
// GroupEventEmitter is wired) or to the per-replica
// LifecycleNotifier.MarkFailed path (otherwise).
//
// The dispatch flag is the key distinguisher: a per-replica election
// path also routes start failures here, but those failures must not
// trigger group rollback — they should follow the per-replica
// re-election flow. Same-node rollback is reserved for failures of
// containers that were started as part of an atomic group placement.
func (h *Handler) reportStartFailure(capsuleID, reason string) {
	if h.groupEmitter != nil && h.groupView != nil && h.isGroupDispatched(capsuleID) {
		info, ok := h.groupView.MemberInfo(capsuleID)
		if ok && info.GroupID != "" {
			mode, modeOK := h.groupView.Colocation(info.GroupID)
			if modeOK && mode == ColocationSameNode {
				h.logger.Warn("runtime: same-node group member start failed; emitting MemberPlacementFailed",
					zap.String("group_id", info.GroupID),
					zap.String("capsule_id", capsuleID),
					zap.String("reason", reason))
				// Clear the dispatch flag — rollback owns the next
				// step; a stale flag would re-route a follow-up
				// per-replica failure through the rollback path.
				h.clearGroupDispatched(capsuleID)
				h.groupEmitter.EmitMemberPlacementFailed(info.GroupID, capsuleID, reason)
				return
			}
		}
	}
	if h.lifecycle != nil {
		if err := h.lifecycle.MarkFailed(capsuleID, reason); err != nil {
			h.logger.Warn("runtime: MarkFailed failed",
				zap.String("capsule_id", capsuleID),
				zap.Error(err))
		}
	}
}

// CancelGroupStarts purges any in-flight parked starts whose capsule
// belongs to the given group ID AND clears the StartGroup dispatch
// flag for every member of the group. Used by the node-side
// MemberPlacementFailed rollback path so siblings parked on
// dependency waits do NOT race a fresh GroupClaimWon by suddenly
// firing their startContainer paths after the rollback has already
// stopped them.
//
// Idempotent and safe to call when no parked entries match (returns
// silently). Returns the number of parked entries cancelled — useful
// for observability and test assertions.
func (h *Handler) CancelGroupStarts(groupID string) int {
	if h == nil {
		return 0
	}

	// Clear every dispatched member for this group regardless of
	// whether a parked entry was cancelled — a member already running
	// startContainer (e.g. mid-Pull) would otherwise route a
	// subsequent failure back through MemberPlacementFailed.
	if h.groupView != nil {
		h.mu.Lock()
		for capsuleID := range h.groupDispatched {
			info, ok := h.groupView.MemberInfo(capsuleID)
			if !ok || info.GroupID != groupID {
				continue
			}
			delete(h.groupDispatched, capsuleID)
		}
		h.mu.Unlock()
	}

	if h.group == nil {
		return 0
	}
	return h.group.cancelGroup(groupID)
}

// StopContainer gracefully stops a running container (user-initiated).
func (h *Handler) StopContainer(capsuleID, replicaID string, gracePeriod time.Duration) error {
	cID := containerID(capsuleID, replicaID)

	// Cancel the watcher.
	h.mu.Lock()
	if cancel, ok := h.running[cID]; ok {
		cancel()
		delete(h.running, cID)
	}
	h.mu.Unlock()

	if err := h.runtime.Stop(h.ctx, cID, WithGracePeriod(gracePeriod)); err != nil {
		return fmt.Errorf("runtime: stop %s: %w", cID, err)
	}
	if err := h.runtime.Remove(h.ctx, cID); err != nil {
		h.logger.Warn("runtime: remove after stop failed",
			zap.String("container_id", cID),
			zap.Error(err))
	}

	h.lifecycle.MarkStopped(capsuleID)
	return nil
}
