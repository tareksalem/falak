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
	runtime        Runtime
	capsuleStore   CapsuleStore
	snapshotStore  SnapshotStore
	snapshotPuller SnapshotPuller
	snapshotBC     SnapshotBroadcaster
	lifecycle      LifecycleNotifier
	statsRegistry  StatsRegistry
	logger         *zap.Logger

	mu       sync.Mutex
	running  map[string]context.CancelFunc // containerID -> cancel for stats/watcher goroutine
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
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

// NewHandler constructs a runtime handler.
func NewHandler(rt Runtime, opts ...HandlerOption) *Handler {
	h := &Handler{
		runtime: rt,
		logger:  zap.NewNop(),
		running: make(map[string]context.CancelFunc),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Start begins the handler's background goroutines.
func (h *Handler) Start(ctx context.Context) {
	h.ctx, h.cancel = context.WithCancel(ctx)
}

// Stop cancels all container watchers and waits for clean exit.
func (h *Handler) Stop() {
	h.cancel()
	h.mu.Lock()
	for id, cancel := range h.running {
		cancel()
		delete(h.running, id)
	}
	h.mu.Unlock()
	h.wg.Wait()
}

// HandleElectionWon is called when the local node wins an election for
// a capsule replica. It decides the start mode (restore vs cold start),
// creates and starts the container, captures a snapshot if needed, and
// reports the outcome.
func (h *Handler) HandleElectionWon(event ElectionWon) {
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
		h.lifecycle.MarkFailed(capsuleID, "spec lookup failed: "+err.Error())
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
	if err := h.runtime.Pull(ctx, spec.Image, pullOpts...); err != nil {
		h.logger.Error("runtime: image pull failed",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
		h.lifecycle.MarkFailed(capsuleID, "image pull failed: "+err.Error())
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

	if err := h.runtime.Create(ctx, cID, spec.Image, createOpts...); err != nil {
		h.logger.Error("runtime: create failed",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
		h.lifecycle.MarkFailed(capsuleID, "create failed: "+err.Error())
		return
	}

	// Start container.
	if err := h.runtime.Start(ctx, cID); err != nil {
		h.logger.Error("runtime: start failed",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
		h.runtime.Remove(ctx, cID)
		h.lifecycle.MarkFailed(capsuleID, "start failed: "+err.Error())
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
