package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
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
	Name          string
	Image         string
	ImageDigest   string
	Env           map[string]string
	NetworkMode   NetworkMode
	Ports         []PortMapping
	Resources     ResourceLimits
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
//
// MarkFailed carries a typed FailureCategory determined at the failure
// site so downstream consumers (notably the node-local execution
// reliability tracker) do not have to re-derive blame from reason strings.
type LifecycleNotifier interface {
	MarkRunning(capsuleID string) error
	MarkFailed(capsuleID, reason string, category FailureCategory) error
	MarkStopped(capsuleID string) error
}

// ReplicaNetworkRecorder is an OPTIONAL capability a LifecycleNotifier may
// also implement to persist a replica's resolved container IP and host-port
// bindings, discovered by the handler's post-start readback, BEFORE the
// capsule is announced Running. The handler probes for it via a type
// assertion (like StreamingPuller); a notifier that does not implement it
// simply skips the record step — the container still runs and is
// mesh-reachable, only the per-replica observability is unavailable.
type ReplicaNetworkRecorder interface {
	// RecordReplicaNetwork stores the resolved IP and host-port bindings for
	// the given replica. Called once per successful start, before MarkRunning.
	RecordReplicaNetwork(capsuleID, replicaID, ip string, ports []PortBinding) error
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

// SnapshotReplicator is the narrow interface used to trigger holder-driven
// replication of a freshly-captured snapshot to K standby peers (O11 HA
// fast-restart). Replicate MUST be non-blocking: it hands the work to a
// bounded background worker pool so the capture path and container start
// are never gated on replication. Satisfied by snapshot.Replicator behind
// a node-side adapter.
type SnapshotReplicator interface {
	Replicate(capsuleID, tag, checksum string, size int64)
}

// StatsRegistry is the narrow interface for registering per-capsule
// metrics providers. The runtime handler feeds container stats into
// this registry so scaling rules can evaluate live data. Satisfied by
// scaling.MetricsRegistry behind an adapter.
type StatsRegistry interface {
	RegisterStats(capsuleID string, getCPU, getMemory func() float64)
	UnregisterStats(capsuleID string)
}

// runningContainer holds the per-container bookkeeping the centralized
// event consumer and reconcile sweep need to drive crash recovery without
// a per-container polling goroutine. cancel tears down the container's
// health-check and stats goroutines; capsuleID and restartLimit feed the
// recovery decision; localRestarts counts restarts already attempted so
// the limit survives across separate Died events.
type runningContainer struct {
	cancel        context.CancelFunc
	capsuleID     string
	restartLimit  int
	localRestarts int
	inspectErrors int // consecutive transient Inspect failures (reconcile)
}

// Handler bridges election outcomes to the container runtime. It
// subscribes to ElectionWon events, decides whether to restore from
// snapshot or cold-start, manages the container lifecycle, and reports
// outcomes back to the capsule lifecycle.
type Handler struct {
	runtime            Runtime
	capsuleStore       CapsuleStore
	snapshotStore      SnapshotStore
	snapshotPuller     SnapshotPuller
	snapshotBC         SnapshotBroadcaster
	snapshotReplicator SnapshotReplicator // guarded by mu (settable post-Start)
	lifecycle          LifecycleNotifier
	statsRegistry      StatsRegistry
	groupView          GroupView
	groupEmitter       GroupEventEmitter
	depTimeout         time.Duration
	pullHeartbeatTick  time.Duration
	logger             *zap.Logger

	// Event/reconcile tuning (functional options, no magic numbers).
	reconcileInterval     time.Duration // periodic reconcile sweep cadence
	maxInspectErrors      int           // consecutive transient Inspect errors before escalation
	eventReconnectBackoff time.Duration // base backoff between event-stream reconnect attempts

	// Port/IP readback tuning (O15). After a container starts, the handler
	// polls Inspect until the container IP is present and every published
	// spec port is bound to a non-empty host port, up to a bounded budget.
	// Both configurable; no magic numbers.
	portReadbackInterval time.Duration // gap between readback Inspect polls
	portReadbackBudget   time.Duration // total time budget for the readback

	// readbackTicker is an injectable ticker factory for deterministic
	// readback tests. nil in production → time.NewTicker. Tests supply a
	// manually-pulsed channel so the poll advances without real sleeps.
	readbackTicker func(d time.Duration) (<-chan time.Time, func())

	mu      sync.Mutex
	running map[string]*runningContainer // containerID -> live container bookkeeping

	// ignored maps an owned containerID to the deadline until which
	// backend events for it are suppressed. Populated before any
	// handler-initiated Stop/Remove so Falak's own teardowns do not
	// self-trigger re-election. Swept by the reconcile loop.
	ignored map[string]time.Time

	// groupDispatched tracks capsule IDs that were dispatched as part
	// of a StartGroup call. Their start failures route to
	// MemberPlacementFailed rollback (driven by the node-side capsule
	// handler) rather than the per-replica MarkFailed path. Cleared
	// when the capsule reaches Running so a future crash-and-retry
	// goes through the normal per-replica flow.
	groupDispatched map[string]struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

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

// WithSnapshotReplicator wires the holder-driven replicator invoked after
// a successful capture to push K standby copies to peers (O11). Optional:
// without it, capture still broadcasts availability but keeps a single
// copy (the pre-O11 behaviour).
func WithSnapshotReplicator(r SnapshotReplicator) HandlerOption {
	return func(h *Handler) { h.snapshotReplicator = r }
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

// Event/reconcile defaults. The reconcile interval is deliberately long
// (30s) because the event stream carries the fast path; reconcile is the
// correctness backstop for events dropped on a socket restart.
// maxInspectErrors bounds how many consecutive transient Inspect failures
// are tolerated before the container is declared unreachable.
// eventReconnectBackoff is the base (jittered) delay between event-stream
// reconnect attempts.
const (
	defaultReconcileInterval     = 30 * time.Second
	defaultMaxInspectErrors      = 5
	defaultEventReconnectBackoff = 1 * time.Second
)

// Port/IP readback defaults. A cold-started container's auto host port and a
// restored container's re-published host port populate in Inspect a short
// moment after start (the host-port DNAT lands after the IP does), so the
// handler polls briefly. 20 × 100ms = ~2s covers the observed lag with margin
// while bounding the wait so a genuinely-unbound fixed port still escalates
// promptly. Both are overridable via functional options.
const (
	defaultPortReadbackInterval = 100 * time.Millisecond
	defaultPortReadbackBudget   = 2 * time.Second
)

// ignoreTTL bounds how long an entry in the self-removal ignore set
// survives. The backend emits died+remove for an intentional teardown
// within milliseconds, so a few seconds is ample; the cap prevents the
// set from pinning a stale entry if the expected event never arrives.
const ignoreTTL = 30 * time.Second

// WithReconcileInterval overrides the periodic reconcile-sweep cadence.
// The sweep is the correctness backstop behind the event stream. Default
// 30s. Non-positive values are ignored (default kept).
func WithReconcileInterval(d time.Duration) HandlerOption {
	return func(h *Handler) {
		if d > 0 {
			h.reconcileInterval = d
		}
	}
}

// WithMaxInspectErrors sets the number of consecutive transient Inspect
// errors tolerated during reconcile before a container is escalated as
// "runtime unreachable" (MarkFailed → re-election). Default 5.
// Non-positive values are ignored (default kept).
func WithMaxInspectErrors(n int) HandlerOption {
	return func(h *Handler) {
		if n > 0 {
			h.maxInspectErrors = n
		}
	}
}

// WithEventReconnectBackoff sets the base delay between event-stream
// reconnect attempts. The actual wait is jittered around this value.
// Default 1s. Non-positive values are ignored (default kept).
func WithEventReconnectBackoff(d time.Duration) HandlerOption {
	return func(h *Handler) {
		if d > 0 {
			h.eventReconnectBackoff = d
		}
	}
}

// WithPortReadbackInterval overrides the gap between post-start readback
// Inspect polls (default 100ms). Non-positive values are ignored.
func WithPortReadbackInterval(d time.Duration) HandlerOption {
	return func(h *Handler) {
		if d > 0 {
			h.portReadbackInterval = d
		}
	}
}

// WithPortReadbackBudget overrides the total time budget for the post-start
// port/IP readback (default 2s). Once the budget is exhausted, a still-unbound
// FIXED spec port fails the start (→ re-election) while a still-unbound AUTO
// port logs a warning and the container continues. Non-positive values are
// ignored.
func WithPortReadbackBudget(d time.Duration) HandlerOption {
	return func(h *Handler) {
		if d > 0 {
			h.portReadbackBudget = d
		}
	}
}

// WithPortReadbackTicker injects the ticker factory the port/IP readback uses
// between Inspect polls. Production leaves it unset (a real time.Ticker);
// deterministic tests supply a factory returning a channel they control (or a
// closed channel to advance the poll without real sleeps). A nil factory is
// ignored. Mirrors the test-seam clock injection used elsewhere in the
// codebase (e.g. the reconnector's WithReconnectClock).
func WithPortReadbackTicker(fn func(d time.Duration) (<-chan time.Time, func())) HandlerOption {
	return func(h *Handler) {
		if fn != nil {
			h.readbackTicker = fn
		}
	}
}

// NewHandler constructs a runtime handler.
func NewHandler(rt Runtime, opts ...HandlerOption) *Handler {
	h := &Handler{
		runtime:               rt,
		logger:                zap.NewNop(),
		running:               make(map[string]*runningContainer),
		ignored:               make(map[string]time.Time),
		groupDispatched:       make(map[string]struct{}),
		pullHeartbeatTick:     defaultPullHeartbeatInterval,
		reconcileInterval:     defaultReconcileInterval,
		maxInspectErrors:      defaultMaxInspectErrors,
		eventReconnectBackoff: defaultEventReconnectBackoff,
		portReadbackInterval:  defaultPortReadbackInterval,
		portReadbackBudget:    defaultPortReadbackBudget,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// SetSnapshotReplicator wires (or replaces) the holder-driven replicator
// after construction. The snapshot mesh (Discovery + Replicator) is
// created per-cluster on join, which happens AFTER the handler is built at
// node start, so the node wiring injects the replicator here. Guarded by
// the handler mutex because captureSnapshot reads it from a background
// goroutine.
func (h *Handler) SetSnapshotReplicator(r SnapshotReplicator) {
	h.mu.Lock()
	h.snapshotReplicator = r
	h.mu.Unlock()
}

// snapshotReplicatorRef returns the currently-wired replicator under the
// handler mutex.
func (h *Handler) snapshotReplicatorRef() SnapshotReplicator {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshotReplicator
}

// snapshotBroadcaster returns the currently-wired broadcaster under the
// handler mutex. Guarded because SetSnapshotMesh may install it after the
// handler has started its background capture goroutines.
func (h *Handler) snapshotBroadcaster() SnapshotBroadcaster {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshotBC
}

// snapshotPullerRef returns the currently-wired puller under the handler
// mutex (settable post-start via SetSnapshotMesh).
func (h *Handler) snapshotPullerRef() SnapshotPuller {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshotPuller
}

// SetSnapshotMesh installs the full snapshot mesh (availability
// broadcaster, remote puller, holder-driven replicator) after
// construction. The mesh's Discovery + Replicator are created per-cluster
// on join, which is AFTER the handler is built at node start, so the node
// wiring injects them here. Any argument may be nil to leave that role
// unwired. Guarded because the capture/start paths read these fields from
// background goroutines.
func (h *Handler) SetSnapshotMesh(bc SnapshotBroadcaster, puller SnapshotPuller, rep SnapshotReplicator) {
	h.mu.Lock()
	h.snapshotBC = bc
	h.snapshotPuller = puller
	h.snapshotReplicator = rep
	h.mu.Unlock()
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
					// A dependency that never reached Running could be the
					// node's fault or the dependency's — ambiguous, so count it.
					if err := h.lifecycle.MarkFailed(capsuleID, reason, FailureCategoryEnum.Ambiguous()); err != nil {
						h.logger.Warn("group dep timeout: MarkFailed failed",
							zap.String("capsule_id", capsuleID),
							zap.Error(err))
					}
				}
			},
			&h.wg,
		)
	}

	// Long-lived event consumer: the primary, low-latency crash/removal
	// signal. Reconnects with jittered backoff when the stream drops.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.consumeEvents(h.ctx)
	}()

	// Periodic reconcile sweep: the correctness backstop for events
	// dropped during a backend socket restart.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.reconcileLoop(h.ctx)
	}()
}

// Stop cancels all container watchers and waits for clean exit.
func (h *Handler) Stop() {
	h.cancel()
	if h.group != nil {
		h.group.stop()
	}
	h.mu.Lock()
	for id, rc := range h.running {
		rc.cancel()
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

	// O14c start-vs-stop race guard (fast path). ElectionWon → start is
	// async; a post-hoc election yield's StopContainer may plant an
	// ignore-set entry for this container BEFORE this start goroutine
	// runs. Honour it here as a clean no-op so the yield's teardown wins
	// and no duplicate container is created. The onContainerRunning
	// backstop below covers the narrower interleaving where the ignore
	// entry is planted AFTER this check but BEFORE the container is
	// registered.
	if h.isIgnored(cID) {
		h.logger.Debug("runtime: start suppressed by ignore-set (yield/stop landed first)",
			zap.String("container_id", cID),
			zap.String("capsule_id", capsuleID))
		return
	}

	spec, err := h.capsuleStore.GetSpec(capsuleID)
	if err != nil {
		h.logger.Error("runtime: capsule spec lookup failed",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
		// Spec lookup hits the local store/runtime — a node condition.
		h.reportStartFailure(capsuleID, "spec lookup failed: "+err.Error(), FailureCategoryEnum.NodeAttributable())
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
		h.restoreFromSnapshot(ctx, cID, capsuleID, replicaID, tag, spec)
		return
	}

	if puller := h.snapshotPullerRef(); puller != nil {
		path, pullErr := puller.Pull(ctx, capsuleID, tag)
		if pullErr == nil && path != "" {
			h.restoreFromSnapshot(ctx, cID, capsuleID, replicaID, tag, spec)
			return
		}
		if pullErr != nil {
			h.logger.Debug("runtime: snapshot pull failed, falling back to cold start",
				zap.String("capsule_id", capsuleID),
				zap.Error(pullErr))
		}
	}

	h.coldStart(ctx, cID, capsuleID, replicaID, tag, spec)
}

// restoreFromSnapshot restores a container from a local snapshot.
func (h *Handler) restoreFromSnapshot(ctx context.Context, cID, capsuleID, replicaID, tag string, spec *CapsuleSpec) {
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
		h.coldStart(ctx, cID, capsuleID, replicaID, tag, spec)
		return
	}

	h.snapshotStore.MarkInUse(capsuleID, tag, true)
	h.snapshotStore.TouchAccess(capsuleID, tag)
	h.onContainerRunning(cID, capsuleID, replicaID, spec)
}

// coldStart pulls the image, creates and starts the container from
// scratch, then captures a snapshot for future fast restarts.
func (h *Handler) coldStart(ctx context.Context, cID, capsuleID, replicaID, tag string, spec *CapsuleSpec) {
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
		// Sub-classify the pull error at the site: disk-full/permission is
		// the node's fault, manifest-not-found/auth is the capsule's fault,
		// network/registry-down is ambiguous.
		h.reportStartFailure(capsuleID, "image pull failed: "+err.Error(), classifyPullError(err))
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
		// Create failures (runc/cgroup/namespace) are node conditions.
		h.reportStartFailure(capsuleID, "create failed: "+err.Error(), FailureCategoryEnum.NodeAttributable())
		return
	}

	// Start container.
	if err := h.runtime.Start(ctx, cID); err != nil {
		h.logger.Error("runtime: start failed",
			zap.String("capsule_id", capsuleID),
			zap.Error(err))
		h.runtime.Remove(ctx, cID)
		// Start failures are node conditions (the image and spec were valid
		// enough to create; the node could not run the container).
		h.reportStartFailure(capsuleID, "start failed: "+err.Error(), FailureCategoryEnum.NodeAttributable())
		return
	}

	h.onContainerRunning(cID, capsuleID, replicaID, spec)

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

	// CRIU stops the container during checkpoint and we restart it right
	// after. Suppress its died event for that window so the intentional
	// checkpoint-stop does not self-trigger crash recovery. Ownership is
	// retained throughout (suppressEvents does not drop h.running).
	h.suppressEvents(cID)
	defer h.unsuppressEvents(cID)

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
		if rc, ok := h.running[cID]; ok {
			rc.cancel()
			delete(h.running, cID)
		}
		h.mu.Unlock()
		// Checkpoint/restore is a node/runtime capability — node-attributable.
		h.lifecycle.MarkFailed(capsuleID, "restart after checkpoint failed: "+err.Error(), FailureCategoryEnum.NodeAttributable())
		return
	}

	// Announce to the mesh that this node now has the snapshot.
	if bc := h.snapshotBroadcaster(); bc != nil {
		if err := bc.BroadcastAvailable(capsuleID, tag, checksum, snapSize); err != nil {
			h.logger.Warn("runtime: snapshot broadcast failed",
				zap.String("capsule_id", capsuleID),
				zap.Error(err))
		}
	}

	// Replicate to K standby peers for HA fast-restart (O11). The call is
	// non-blocking — it enqueues onto a bounded background worker pool — so
	// the capture path and container are never gated on replication. Done
	// AFTER BroadcastAvailable so the replicator's holder count reflects
	// any copies already known to the index.
	if rep := h.snapshotReplicatorRef(); rep != nil {
		rep.Replicate(capsuleID, tag, checksum, snapSize)
	}

	h.logger.Info("runtime: snapshot captured",
		zap.String("capsule_id", capsuleID),
		zap.String("tag", tag))
}

// onContainerRunning is called after a container is successfully started
// (either via restore or cold start). It marks the capsule as running,
// starts a health checker if configured, and starts a background watcher
// for crashes.
func (h *Handler) onContainerRunning(cID, capsuleID, replicaID string, spec *CapsuleSpec) {
	// O14c start-vs-stop race backstop. A post-hoc election yield's
	// StopContainer may have planted an ignore-set entry for this
	// container AFTER startContainer's top-of-function guard passed but
	// while the create/start was in flight. If the entry is live now, the
	// yield's teardown must win: do NOT MarkRunning, do NOT register a
	// watcher (which would emit CapsuleRunning and resurrect the duplicate
	// we are trying to kill), and tear the just-created container back
	// down. The ignore entry is left standing (it ages out via
	// sweepIgnored) so the died/removed events from this teardown stay
	// suppressed at handleContainerEvent — HARD INVARIANT #1.
	//
	// The check is done under h.mu, on the raw map (not isIgnored, which
	// re-locks h.mu) so it is atomic with the register/decline decision:
	// no TOCTOU window between "not ignored" and "installed running
	// entry". onContainerRunning is only ever reached on the cold-start /
	// restore paths for the container's OWN cID (RollingUpdate registers a
	// different -new cID; captureSnapshot restarts via runtime.Start
	// directly, never through here), so a live same-cID ignore entry here
	// can only be a stop-intent.
	h.mu.Lock()
	if deadline, ok := h.ignored[cID]; ok && time.Now().Before(deadline) {
		h.mu.Unlock()
		h.logger.Info("runtime: honouring concurrent stop; tearing down just-started container",
			zap.String("container_id", cID),
			zap.String("capsule_id", capsuleID))
		// Stop+Remove OUTSIDE the lock — backend calls must never hold
		// h.mu. Best-effort: errors are logged, not surfaced, because the
		// container may already be gone (the yield's own Stop raced ahead).
		if err := h.runtime.Stop(h.ctx, cID); err != nil {
			h.logger.Debug("runtime: stop of yielded container failed (often expected: not yet fully started)",
				zap.String("container_id", cID), zap.Error(err))
		}
		if err := h.runtime.Remove(h.ctx, cID); err != nil {
			h.logger.Debug("runtime: remove of yielded container failed (often expected: already gone)",
				zap.String("container_id", cID), zap.Error(err))
		}
		return
	}
	h.mu.Unlock()

	// --- Port/IP readback (O15) ---------------------------------------
	// Resolve the container's IP and published host ports before announcing
	// Running so the replica state (and `capsule get`) reflect the ACTUAL
	// bindings. An auto host port is a runtime allocation absent from the
	// spec; a snapshot-restored replica re-publishes a fresh one. The
	// host-port DNAT lands a moment after the container IP, so the poll keys
	// on HostPort specifically. Skipped when the capsule publishes no ports
	// or runs on the host network (no per-container host-port NAT).
	var resolvedIP string
	var readbackDone bool
	if published := publishedSpecPorts(spec); len(published) > 0 {
		ip, bindings, fixedUnbound, autoUnbound := h.awaitReplicaNetwork(h.ctx, cID, published)
		resolvedIP = ip
		readbackDone = true
		if fixedUnbound {
			h.logger.Error("runtime: fixed host port never bound within readback budget; failing start",
				zap.String("container_id", cID),
				zap.String("capsule_id", capsuleID),
				zap.String("replica_id", replicaID),
				zap.Duration("budget", h.portReadbackBudget))
			// The container is not registered in h.running yet, so its
			// died/removed events are dropped as "not owned". Tear it down so
			// it does not linger holding a partial allocation, then fail the
			// start → re-election (covers a cross-node fixed-port collision).
			if err := h.runtime.Stop(h.ctx, cID); err != nil {
				h.logger.Debug("runtime: stop of port-unbound container failed",
					zap.String("container_id", cID), zap.Error(err))
			}
			if err := h.runtime.Remove(h.ctx, cID); err != nil {
				h.logger.Debug("runtime: remove of port-unbound container failed",
					zap.String("container_id", cID), zap.Error(err))
			}
			h.reportStartFailure(capsuleID,
				"fixed host port not bound within readback budget",
				FailureCategoryEnum.NodeAttributable())
			return
		}
		if autoUnbound {
			// An auto port is observability only — the mesh dials the bridge
			// IP + container port and never uses the host port. Record what we
			// have (0 for the unbound auto port) and keep the working
			// container; do NOT kill it.
			h.logger.Warn("runtime: auto host port(s) unresolved within readback budget; recording 0 and continuing",
				zap.String("container_id", cID),
				zap.String("capsule_id", capsuleID),
				zap.String("replica_id", replicaID),
				zap.Duration("budget", h.portReadbackBudget))
		}
		// Record resolved network on the replica state BEFORE MarkRunning so
		// the bindings are in place by the time the status update gossips.
		h.recordReplicaNetwork(capsuleID, replicaID, ip, bindings)
	}

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

	// Record the container as owned. Crash/removal detection is handled
	// centrally by the event consumer and reconcile sweep (keyed off the
	// h.running entry below); this context only scopes the per-container
	// health-check and stats goroutines. If an entry already exists for
	// this container ID (e.g. event-bus redelivery), cancel the old
	// goroutines first to prevent orphans. A re-registration clears any
	// stale ignore-set entry so a fresh container is watched again.
	//
	// The ignore-set is re-checked inside this same critical section
	// because a stop-intent could have been planted between the decline
	// check above releasing h.mu and this re-acquire; if so, decline here
	// too rather than register a doomed watcher.
	restartLimit := 0
	if spec != nil {
		restartLimit = spec.FailurePolicy.RestartLimit
	}
	watchCtx, watchCancel := context.WithCancel(h.ctx)
	h.mu.Lock()
	if deadline, ok := h.ignored[cID]; ok && time.Now().Before(deadline) {
		h.mu.Unlock()
		watchCancel()
		h.logger.Info("runtime: honouring concurrent stop after MarkRunning; tearing down container",
			zap.String("container_id", cID),
			zap.String("capsule_id", capsuleID))
		if err := h.runtime.Stop(h.ctx, cID); err != nil {
			h.logger.Debug("runtime: stop of yielded container failed",
				zap.String("container_id", cID), zap.Error(err))
		}
		if err := h.runtime.Remove(h.ctx, cID); err != nil {
			h.logger.Debug("runtime: remove of yielded container failed",
				zap.String("container_id", cID), zap.Error(err))
		}
		return
	}
	if old, exists := h.running[cID]; exists {
		h.logger.Warn("runtime: replacing existing watcher for container",
			zap.String("container_id", cID))
		old.cancel()
	}
	h.running[cID] = &runningContainer{
		cancel:       watchCancel,
		capsuleID:    capsuleID,
		restartLimit: restartLimit,
	}
	delete(h.ignored, cID)
	h.mu.Unlock()

	// Start health checker if the capsule spec defines one.
	if spec != nil && spec.HealthCheck != nil {
		// Determine the container's address based on network mode.
		// Host mode: probe on localhost. Bridge mode: use the IP resolved by
		// the port readback above when it ran (avoids a second Inspect);
		// otherwise (no published ports) do a single Inspect for the IP.
		containerAddr := "127.0.0.1"
		if spec.NetworkMode == NetworkModeEnum.Bridge() {
			if readbackDone && resolvedIP != "" {
				containerAddr = resolvedIP
			} else if !readbackDone {
				info, err := h.runtime.Inspect(h.ctx, cID)
				if err == nil && info.IP != "" {
					containerAddr = info.IP
				}
			}
		}

		hc := NewHealthChecker(*spec.HealthCheck,
			WithHealthCheckerLogger(h.logger.Named("healthcheck")))
		hc.Start(watchCtx, containerAddr, func() {
			h.logger.Warn("runtime: health check failed, marking container unhealthy",
				zap.String("container_id", cID),
				zap.String("capsule_id", capsuleID),
				zap.String("address", containerAddr))
			// Health-check exhaustion could be an app fault or a node
			// networking fault — ambiguous; count it (decay forgives).
			h.lifecycle.MarkFailed(capsuleID, "health check exhausted retries", FailureCategoryEnum.Ambiguous())
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
}

// --- Port/IP readback (O15) ----------------------------------------------

// publishedSpecPorts returns the spec ports that require a post-start
// host-port readback: the capsule must run in bridge mode (host network has no
// per-container host-port NAT) and declare at least one port. Returns nil
// otherwise so the caller skips the readback entirely.
func publishedSpecPorts(spec *CapsuleSpec) []PortMapping {
	if spec == nil || len(spec.Ports) == 0 {
		return nil
	}
	if spec.NetworkMode == NetworkModeEnum.Host() {
		return nil
	}
	return spec.Ports
}

// awaitReplicaNetwork polls Inspect after a container starts until its IP is
// present AND every published spec port is bound to a non-empty host port, or
// the readback budget is exhausted. It returns the last-observed IP, the
// resolved per-port bindings (HostPort 0 for any still-unbound port), and two
// classification flags: fixedUnbound is true when a user-specified fixed spec
// port (HostPort>0) never bound; autoUnbound when only auto ports (HostPort==0)
// remain unbound. Keying on HostPort specifically matters because the IP
// populates in Inspect before the host-port DNAT does. The ticker is
// injectable for deterministic tests.
func (h *Handler) awaitReplicaNetwork(ctx context.Context, cID string, published []PortMapping) (ip string, bindings []PortBinding, fixedUnbound, autoUnbound bool) {
	interval := h.portReadbackInterval
	if interval <= 0 {
		interval = defaultPortReadbackInterval
	}
	budget := h.portReadbackBudget
	if budget <= 0 {
		budget = defaultPortReadbackBudget
	}
	attempts := int(budget / interval)
	if attempts < 1 {
		attempts = 1
	}

	var tickCh <-chan time.Time
	var stop func()
	if h.readbackTicker != nil {
		tickCh, stop = h.readbackTicker(interval)
	} else {
		tk := time.NewTicker(interval)
		tickCh, stop = tk.C, tk.Stop
	}
	defer stop()

	var lastIP string
	lastBindings := resolveBindings(published, nil)
	for attempt := 1; ; attempt++ {
		info, err := h.runtime.Inspect(ctx, cID)
		if err != nil {
			h.logger.Debug("runtime: readback inspect failed (will retry)",
				zap.String("container_id", cID),
				zap.Int("attempt", attempt),
				zap.Error(err))
		} else {
			lastIP = info.IP
			lastBindings = resolveBindings(published, info.Ports)
			if info.IP != "" && allBound(lastBindings) {
				return info.IP, lastBindings, false, false
			}
		}
		if attempt >= attempts {
			break
		}
		select {
		case <-ctx.Done():
			// Handler shutting down — return best-effort without escalating.
			return lastIP, lastBindings, false, false
		case <-tickCh:
		}
	}

	for i, p := range published {
		if lastBindings[i].HostPort != 0 {
			continue
		}
		if p.HostPort > 0 {
			fixedUnbound = true
		} else {
			autoUnbound = true
		}
	}
	return lastIP, lastBindings, fixedUnbound, autoUnbound
}

// resolveBindings maps each published spec port to a resolved PortBinding,
// filling HostPort from the matching entry in the container's actual port list
// (0 when not yet bound). Order follows published so per-port index alignment
// is stable across polls.
func resolveBindings(published, actual []PortMapping) []PortBinding {
	out := make([]PortBinding, len(published))
	for i, p := range published {
		out[i] = PortBinding{
			Name:          p.Name,
			ContainerPort: p.ContainerPort,
			HostPort:      matchHostPort(p, actual),
		}
	}
	return out
}

// matchHostPort finds the resolved host port for a published spec port within
// the container's actual port list, matching on container port and protocol
// (an empty protocol is treated as "tcp", the runtime default). Returns 0 when
// no matching entry carries a non-zero host port.
func matchHostPort(p PortMapping, actual []PortMapping) uint16 {
	want := p.Protocol
	if want == "" {
		want = "tcp"
	}
	for _, a := range actual {
		if a.ContainerPort != p.ContainerPort {
			continue
		}
		got := a.Protocol
		if got == "" {
			got = "tcp"
		}
		if got != want {
			continue
		}
		if a.HostPort != 0 {
			return a.HostPort
		}
	}
	return 0
}

// allBound reports whether every binding carries a non-zero host port.
func allBound(bindings []PortBinding) bool {
	for _, b := range bindings {
		if b.HostPort == 0 {
			return false
		}
	}
	return true
}

// recordReplicaNetwork pushes the resolved IP + host-port bindings onto the
// replica state via the lifecycle notifier's optional ReplicaNetworkRecorder
// capability. A notifier that does not implement it (or a nil notifier) is a
// silent no-op — the container runs regardless; only per-replica observability
// is skipped.
func (h *Handler) recordReplicaNetwork(capsuleID, replicaID, ip string, ports []PortBinding) {
	if h.lifecycle == nil {
		return
	}
	rec, ok := h.lifecycle.(ReplicaNetworkRecorder)
	if !ok || rec == nil {
		return
	}
	if err := rec.RecordReplicaNetwork(capsuleID, replicaID, ip, ports); err != nil {
		h.logger.Warn("runtime: record replica network failed",
			zap.String("capsule_id", capsuleID),
			zap.String("replica_id", replicaID),
			zap.Error(err))
	}
}

// --- Event-driven crash/removal detection --------------------------------

// ignoreContainer marks a container's backend events as suppressed and
// removes it from the owned set, in that order, BEFORE any
// handler-initiated Stop/Remove. The event consumer checks the ignore set
// so Falak's own teardowns (RollingUpdate swap, StopContainer) never
// self-trigger a re-election. The entry is swept after ignoreTTL.
func (h *Handler) ignoreContainer(cID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ignored[cID] = time.Now().Add(ignoreTTL)
	if rc, ok := h.running[cID]; ok {
		rc.cancel()
		delete(h.running, cID)
	}
}

// suppressEvents marks a container's backend events as ignored WITHOUT
// dropping ownership. Used during snapshot checkpoint, where CRIU stops
// the container (emitting a died event) but the handler immediately
// restarts it and must keep watching it afterwards. Paired with
// unsuppressEvents.
func (h *Handler) suppressEvents(cID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ignored[cID] = time.Now().Add(ignoreTTL)
}

// unsuppressEvents clears an events-suppression entry set by
// suppressEvents once the intentional stop/restart window has closed.
func (h *Handler) unsuppressEvents(cID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.ignored, cID)
}

// isIgnored reports whether the container is currently in the ignore set
// (its deadline has not yet passed).
func (h *Handler) isIgnored(cID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	deadline, ok := h.ignored[cID]
	if !ok {
		return false
	}
	if time.Now().After(deadline) {
		delete(h.ignored, cID)
		return false
	}
	return true
}

// sweepIgnored drops expired entries from the ignore set so it cannot grow
// unbounded when an expected died/remove event never arrives.
func (h *Handler) sweepIgnored() {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for cID, deadline := range h.ignored {
		if now.After(deadline) {
			delete(h.ignored, cID)
		}
	}
}

// consumeEvents is the long-lived event consumer. It subscribes to the
// backend event stream and reacts to crash/removal events for owned,
// non-ignored containers. When the stream drops (channel close) it
// reconciles every owned container — catching transitions missed during
// the gap — then reconnects with jittered backoff.
func (h *Handler) consumeEvents(ctx context.Context) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}

		ch, err := h.runtime.Events(ctx)
		if err != nil {
			attempt++
			h.logger.Warn("runtime: event stream subscribe failed; will retry",
				zap.Int("attempt", attempt),
				zap.Error(err))
			if !h.backoffSleep(ctx, attempt) {
				return
			}
			continue
		}
		attempt = 0

		// Drain the stream until it closes or ctx is cancelled.
		for evt := range ch {
			h.handleContainerEvent(ctx, evt)
		}

		if ctx.Err() != nil {
			return
		}

		// Stream dropped (likely backend socket restart). Reconcile all
		// owned containers to recover any transition missed during the
		// gap, then reconnect with backoff.
		h.reconcileAll(ctx)
		attempt++
		h.logger.Warn("runtime: event stream closed; reconnecting",
			zap.Int("attempt", attempt))
		if !h.backoffSleep(ctx, attempt) {
			return
		}
	}
}

// backoffSleep waits a jittered backoff before the next reconnect attempt.
// Returns false if ctx is cancelled during the wait. The jitter is full
// jitter over [0, base) added to the base so concurrent handlers do not
// reconnect in lockstep against a recovering socket.
func (h *Handler) backoffSleep(ctx context.Context, attempt int) bool {
	base := h.eventReconnectBackoff
	if base <= 0 {
		base = defaultEventReconnectBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(base)))
	wait := base + jitter
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// handleContainerEvent applies one backend event. It acts only on events
// for currently-owned, non-ignored containers; everything else (routine
// actions, foreign containers, Falak's own teardowns) is dropped at Debug.
func (h *Handler) handleContainerEvent(ctx context.Context, evt ContainerEvent) {
	cID := evt.ContainerID
	if cID == "" {
		return
	}

	h.mu.Lock()
	rc, owned := h.running[cID]
	_, ignored := h.ignored[cID]
	h.mu.Unlock()

	if !owned || ignored {
		h.logger.Debug("runtime: dropping container event",
			zap.String("container_id", cID),
			zap.String("action", string(evt.Action)),
			zap.Bool("owned", owned),
			zap.Bool("ignored", ignored))
		return
	}

	switch evt.Action {
	case ContainerEventActionEnum.Died():
		h.handleCrash(ctx, cID, rc.capsuleID, evt.ExitCode)
	case ContainerEventActionEnum.Removed():
		h.handleRemoved(cID, rc.capsuleID)
	default:
		h.logger.Debug("runtime: routine container event",
			zap.String("container_id", cID),
			zap.String("action", string(evt.Action)))
	}
}

// handleRemoved handles a terminal removal: the container is gone, so it
// cannot be restarted locally. Clears ownership and reports failure so the
// capsule handler fires a re-election.
func (h *Handler) handleRemoved(cID, capsuleID string) {
	h.mu.Lock()
	rc, ok := h.running[cID]
	if ok {
		rc.cancel()
		delete(h.running, cID)
	}
	h.mu.Unlock()
	if !ok {
		return // already handled
	}

	h.logger.Warn("runtime: container removed; re-electing",
		zap.String("container_id", cID),
		zap.String("capsule_id", capsuleID))

	if h.lifecycle != nil {
		if err := h.lifecycle.MarkFailed(capsuleID, "container removed", FailureCategoryEnum.Ambiguous()); err != nil {
			h.logger.Warn("runtime: MarkFailed failed",
				zap.String("capsule_id", capsuleID),
				zap.Error(err))
		}
	}
}

// handleCrash handles a container exit. It attempts a local restart up to
// the container's restart limit; once the limit is exhausted (or a restart
// fails) it clears ownership and reports failure so the capsule handler
// fires a re-election. Restart counts persist across separate Died events
// via the runningContainer entry. The call is idempotent: a duplicate Died
// for an already-cleared container is a no-op.
func (h *Handler) handleCrash(ctx context.Context, cID, capsuleID string, exitCode int) {
	h.mu.Lock()
	rc, ok := h.running[cID]
	if !ok {
		h.mu.Unlock()
		return
	}
	restartLimit := rc.restartLimit
	localRestarts := rc.localRestarts
	h.mu.Unlock()

	h.logger.Warn("runtime: container exited",
		zap.String("container_id", cID),
		zap.String("capsule_id", capsuleID),
		zap.Int("exit_code", exitCode),
		zap.Int("local_restarts", localRestarts),
		zap.Int("restart_limit", restartLimit))

	if localRestarts < restartLimit {
		h.logger.Info("runtime: attempting local restart",
			zap.String("container_id", cID),
			zap.String("capsule_id", capsuleID),
			zap.Int("attempt", localRestarts+1))

		if err := h.runtime.Start(ctx, cID); err != nil {
			h.logger.Warn("runtime: local restart failed",
				zap.String("container_id", cID),
				zap.Error(err))
			// Fall through to MarkFailed below.
		} else {
			h.mu.Lock()
			if cur, still := h.running[cID]; still {
				cur.localRestarts++
			}
			h.mu.Unlock()
			return // restart succeeded; the next exit will re-evaluate
		}
	}

	h.mu.Lock()
	if cur, still := h.running[cID]; still {
		cur.cancel()
		delete(h.running, cID)
	}
	h.mu.Unlock()

	if h.lifecycle != nil {
		// A container that exits non-zero after exhausting local restarts is
		// most often an application fault that would recur on any node, but it
		// can also be a node condition (OOM kill). Treat as ambiguous so it is
		// counted yet forgiven by decay rather than excluded outright.
		if err := h.lifecycle.MarkFailed(capsuleID, fmt.Sprintf(
			"container exited with code %d after %d local restarts",
			exitCode, localRestarts), FailureCategoryEnum.Ambiguous()); err != nil {
			h.logger.Warn("runtime: MarkFailed failed",
				zap.String("capsule_id", capsuleID),
				zap.Error(err))
		}
	}
}

// reconcileLoop runs the periodic reconcile sweep — the correctness
// backstop behind the best-effort event stream.
func (h *Handler) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(h.reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.sweepIgnored()
			h.reconcileAll(ctx)
		}
	}
}

// reconcileAll inspects every owned container and drives recovery for any
// that the event stream may have missed:
//   - not-found (ErrContainerNotFound) → terminal removal → MarkFailed.
//   - Stopped/Failed status            → crash path (restart up to limit).
//   - other (transient) Inspect error  → per-container consecutive-error
//     counter; escalate via MarkFailed at maxInspectErrors, reset on success.
func (h *Handler) reconcileAll(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	// Snapshot the owned set so we do not hold the lock across Inspect.
	h.mu.Lock()
	ids := make([]string, 0, len(h.running))
	caps := make(map[string]string, len(h.running))
	for cID, rc := range h.running {
		ids = append(ids, cID)
		caps[cID] = rc.capsuleID
	}
	h.mu.Unlock()

	for _, cID := range ids {
		if ctx.Err() != nil {
			return
		}
		if h.isIgnored(cID) {
			continue
		}
		capsuleID := caps[cID]

		info, err := h.runtime.Inspect(ctx, cID)
		if err != nil {
			if errors.Is(err, ErrContainerNotFound) {
				h.logger.Warn("runtime: reconcile found container removed; re-electing",
					zap.String("container_id", cID),
					zap.String("capsule_id", capsuleID))
				h.resetInspectErrors(cID)
				h.handleRemoved(cID, capsuleID)
				continue
			}
			h.escalateInspectError(cID, capsuleID, err)
			continue
		}
		h.resetInspectErrors(cID)

		if info.Status == ContainerStatusEnum.Failed() ||
			info.Status == ContainerStatusEnum.Stopped() {
			h.logger.Warn("runtime: reconcile found exited container",
				zap.String("container_id", cID),
				zap.String("capsule_id", capsuleID),
				zap.String("status", string(info.Status)),
				zap.Int("exit_code", info.ExitCode))
			h.handleCrash(ctx, cID, capsuleID, info.ExitCode)
		}
	}
}

// escalateInspectError increments the per-container consecutive-error
// counter and, once it reaches maxInspectErrors, declares the container's
// runtime unreachable: clears ownership and MarkFailed → re-election.
// A single blip that is followed by a successful Inspect never escalates
// because resetInspectErrors clears the counter on success.
func (h *Handler) escalateInspectError(cID, capsuleID string, cause error) {
	h.mu.Lock()
	rc, ok := h.running[cID]
	if !ok {
		h.mu.Unlock()
		return
	}
	rc.inspectErrors++
	count := rc.inspectErrors
	limit := h.maxInspectErrors
	escalate := count >= limit
	if escalate {
		rc.cancel()
		delete(h.running, cID)
	}
	h.mu.Unlock()

	if !escalate {
		h.logger.Debug("runtime: transient inspect error",
			zap.String("container_id", cID),
			zap.Int("count", count),
			zap.Int("max", limit),
			zap.Error(cause))
		return
	}

	h.logger.Error("runtime: container runtime unreachable; re-electing",
		zap.String("container_id", cID),
		zap.String("capsule_id", capsuleID),
		zap.Int("count", count),
		zap.Error(cause))

	if h.lifecycle != nil {
		// The local container runtime being unreachable is squarely the
		// node's fault — node-attributable.
		if err := h.lifecycle.MarkFailed(capsuleID, "runtime unreachable", FailureCategoryEnum.NodeAttributable()); err != nil {
			h.logger.Warn("runtime: MarkFailed failed",
				zap.String("capsule_id", capsuleID),
				zap.Error(err))
		}
	}
}

// resetInspectErrors clears the consecutive-error counter for a container
// after a successful Inspect.
func (h *Handler) resetInspectErrors(cID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rc, ok := h.running[cID]; ok {
		rc.inspectErrors = 0
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

	// Stop the old container. Add it to the ignore set and drop it from
	// the owned set BEFORE the Stop/Remove so the resulting died/remove
	// events are suppressed and do not self-trigger a re-election.
	h.ignoreContainer(oldCID)

	h.runtime.Stop(ctx, oldCID, WithGracePeriod(10*time.Second))
	h.runtime.Remove(ctx, oldCID)

	// Track the new container under the original ID for future watches.
	h.onContainerRunning(newCID, capsuleID, replicaID, newSpec)

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
func (h *Handler) reportStartFailure(capsuleID, reason string, category FailureCategory) {
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
		if err := h.lifecycle.MarkFailed(capsuleID, reason, category); err != nil {
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

	// Add to the ignore set and drop from the owned set BEFORE the
	// Stop/Remove so the resulting died/remove events are suppressed and
	// this user-initiated teardown does not self-trigger a re-election.
	h.ignoreContainer(cID)

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
