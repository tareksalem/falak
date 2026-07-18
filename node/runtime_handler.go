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
	"github.com/tareksalem/falak/capsule/enums"
	"github.com/tareksalem/falak/capsule/scaling"
	"github.com/tareksalem/falak/network/dns"
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
//
// When injectDNS is true the adapter populates Spec.DNSFlags with the
// per-group Podman DNS flags (--dns=169.254.169.250, dns-search="",
// dns-option=ndots:0) for capsules that belong to a CapsuleGroup. The
// runtime handler then forwards them to the container runtime so the
// container's /etc/resolv.conf points at the link-local DNS responder.
// Standalone capsules keep Podman's default DNS behaviour regardless
// of this flag.
type runtimeCapsuleStoreAdapter struct {
	manager   *capsule.Manager
	sek       []byte // secrets encryption key; nil = no decryption
	injectDNS bool
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
		case enums.HealthCheckTypeEnum.HTTP():
			spec.HealthCheck.Type = falakrt.HealthCheckTypeEnum.HTTP()
		case enums.HealthCheckTypeEnum.TCP():
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
	case enums.NetworkModeEnum.Host():
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

	// Per-group DNS injection (Phase 11A.15): when the network manager
	// is wired up and the capsule belongs to a CapsuleGroup, point its
	// /etc/resolv.conf at the link-local DNS responder so capsule-name
	// resolution flows through the per-bridge listener. Standalone
	// capsules keep Podman's default DNS regardless of injectDNS.
	if a.injectDNS && c.Spec.GroupID != "" {
		spec.DNSFlags = dns.PodmanDNSFlags()
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

func (a *runtimeLifecycleAdapter) MarkFailed(capsuleID, reason string, category falakrt.FailureCategory) error {
	id := capsule.CapsuleID(capsuleID)
	if err := a.manager.ExecutionFailed(id); err != nil {
		a.logger.Debug("ExecutionFailed transition failed", zap.String("capsule_id", capsuleID), zap.Error(err))
	}
	a.eventBus.Publish(events.CapsuleExecutionFailed{
		BaseEvent: events.NewBaseEvent(),
		CapsuleID: capsuleID,
		Reason:    reason,
		Category:  translateFailureCategory(category),
	})
	return nil
}

// translateFailureCategory maps the runtime package's FailureCategory onto
// the node-internal events enum. The two enums are intentionally separate
// (the runtime module cannot import node-internal packages); this adapter is
// the single translation point. An unrecognized value defaults to Ambiguous,
// matching the "count it" safe default.
func translateFailureCategory(c falakrt.FailureCategory) events.FailureCategory {
	switch c {
	case falakrt.FailureCategoryEnum.NodeAttributable():
		return events.FailureCategoryEnum.NodeAttributable()
	case falakrt.FailureCategoryEnum.CapsuleGlobal():
		return events.FailureCategoryEnum.CapsuleGlobal()
	default:
		return events.FailureCategoryEnum.Ambiguous()
	}
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

// runtimeGroupViewAdapter satisfies runtime.GroupView by translating
// capsule.Manager group queries into the runtime package's narrow
// MemberInfo / SiblingState shape. Keeps the runtime package free of
// capsule module imports.
//
// The adapter is best-effort: when a member's parent group is not yet
// known to the local manager (gossip ordering), MemberInfo returns
// (zero, false) and the runtime handler treats the capsule as if it
// has no group dependencies. The orphan reaper in capsule_handler.go
// guards the case where the group never arrives.
type runtimeGroupViewAdapter struct {
	manager *capsule.Manager
}

// MemberInfo returns the dependency context for a capsule that is a
// group member. Returns (zero, false) when the capsule is standalone or
// when its parent group is not visible locally.
func (a *runtimeGroupViewAdapter) MemberInfo(capsuleID string) (falakrt.MemberInfo, bool) {
	if a == nil || a.manager == nil {
		return falakrt.MemberInfo{}, false
	}
	c := a.manager.Get(capsule.CapsuleID(capsuleID))
	if c == nil || c.Spec.GroupID == "" {
		return falakrt.MemberInfo{}, false
	}

	group := a.manager.Get(c.Spec.GroupID)
	if group == nil || group.Spec.Group == nil {
		return falakrt.MemberInfo{}, false
	}

	// Find this capsule's member-name and DependsOn list inside the
	// group spec. Members are matched by ID against the group's
	// MemberIDs slice (positional with Members).
	memberName := ""
	var dependsOn []string
	for i, mid := range group.Spec.Group.MemberIDs {
		if mid != c.ID {
			continue
		}
		if i < len(group.Spec.Group.Members) {
			memberName = group.Spec.Group.Members[i].Name
			dependsOn = append([]string(nil), group.Spec.Group.Members[i].DependsOn...)
		}
		break
	}

	// Build the siblings map: every member of this group keyed by name,
	// with the current Running status of the local capsule view.
	siblings := make(map[string]falakrt.SiblingState, len(group.Spec.Group.MemberIDs))
	for i, mid := range group.Spec.Group.MemberIDs {
		if i >= len(group.Spec.Group.Members) {
			break
		}
		name := group.Spec.Group.Members[i].Name
		sibling := a.manager.Get(mid)
		if sibling == nil {
			siblings[name] = falakrt.SiblingState{CapsuleID: mid.String()}
			continue
		}
		siblings[name] = falakrt.SiblingState{
			CapsuleID: mid.String(),
			Running:   sibling.Status == enums.CapsuleStatusEnum.Running(),
		}
	}

	return falakrt.MemberInfo{
		GroupID:   c.Spec.GroupID.String(),
		Name:      memberName,
		DependsOn: dependsOn,
		Siblings:  siblings,
	}, true
}

// CapsuleIDByMemberName resolves a sibling member's name to its capsule
// ID within the same group. Returns ("", false) if the group is not
// known locally or the name is not in the group.
func (a *runtimeGroupViewAdapter) CapsuleIDByMemberName(groupID, memberName string) (string, bool) {
	if a == nil || a.manager == nil {
		return "", false
	}
	group := a.manager.Get(capsule.CapsuleID(groupID))
	if group == nil || group.Spec.Group == nil {
		return "", false
	}
	for i, member := range group.Spec.Group.Members {
		if member.Name != memberName {
			continue
		}
		if i >= len(group.Spec.Group.MemberIDs) {
			return "", false
		}
		return group.Spec.Group.MemberIDs[i].String(), true
	}
	return "", false
}

// Colocation returns the colocation mode for the given group capsule.
// Returns ("", false) when the group is not visible locally or is not
// a Kind=Group capsule. The string is the underlying capsule package's
// canonical ColocationMode value; the runtime package compares it
// against its local constant runtime.ColocationSameNode.
func (a *runtimeGroupViewAdapter) Colocation(groupID string) (string, bool) {
	if a == nil || a.manager == nil {
		return "", false
	}
	group := a.manager.Get(capsule.CapsuleID(groupID))
	if group == nil || group.Spec.Group == nil {
		return "", false
	}
	return string(group.Spec.Group.Colocation), true
}

// runtimeGroupEventEmitter satisfies runtime.GroupEventEmitter by
// publishing MemberPlacementFailed and PullProgress events onto the
// node event bus. Keeps the runtime package free of node-internal
// imports.
//
// The emitter is a thin translation layer — every call publishes
// exactly one event. The election manager subscribes to PullProgress
// (to extend the reservation deadline) and the capsule handler
// subscribes to MemberPlacementFailed (to drive same-node rollback).
type runtimeGroupEventEmitter struct {
	bus         events.Bus
	nodeID      string
	clusterPath string
}

// EmitMemberPlacementFailed publishes events.MemberPlacementFailed on
// the node bus. capsuleID is the failing member; groupID is its
// parent. reason is a human-readable explanation surfaced in logs and
// observability dashboards.
func (e *runtimeGroupEventEmitter) EmitMemberPlacementFailed(groupID, capsuleID, reason string) {
	if e == nil || e.bus == nil {
		return
	}
	e.bus.Publish(events.MemberPlacementFailed{
		BaseEvent:   events.NewBaseEvent(),
		GroupID:     groupID,
		CapsuleID:   capsuleID,
		NodeID:      e.nodeID,
		ClusterPath: e.clusterPath,
		Reason:      reason,
	})
}

// EmitPullProgress publishes events.PullProgress on the node bus.
// groupID is empty for standalone capsules. BytesRemaining=-1 and
// BytesPerSecond=0 signal "alive, progress unknown" (the v1 mode);
// real progress values land in 10.16b.
func (e *runtimeGroupEventEmitter) EmitPullProgress(groupID, capsuleID string, bytesRemaining, bytesPerSecond int64) {
	if e == nil || e.bus == nil {
		return
	}
	e.bus.Publish(events.PullProgress{
		BaseEvent:      events.NewBaseEvent(),
		GroupID:        groupID,
		CapsuleID:      capsuleID,
		NodeID:         e.nodeID,
		ClusterPath:    e.clusterPath,
		BytesRemaining: bytesRemaining,
		BytesPerSecond: bytesPerSecond,
	})
}

// NewRuntimeGroupEventEmitter constructs an emitter suitable for
// wiring into runtime.NewHandler via runtime.WithGroupEventEmitter.
// nodeID is the local node ID; clusterPath identifies the cluster
// the event applies to (carried for cross-cluster observability
// even when a single node hosts multiple clusters).
func NewRuntimeGroupEventEmitter(bus events.Bus, nodeID, clusterPath string) *runtimeGroupEventEmitter {
	return &runtimeGroupEventEmitter{bus: bus, nodeID: nodeID, clusterPath: clusterPath}
}

// RuntimeBridge subscribes to ElectionWon events on the node event bus
// and dispatches them to the runtime.Handler. It also forwards
// CapsuleRunning events to the handler's group dependency coordinator
// so parked starts can release once their dependencies reach Running.
type RuntimeBridge struct {
	handler    *falakrt.Handler
	eventBus   events.Bus
	logger     *zap.Logger
	capsuleMgr *capsule.Manager // optional; required for group-claim FSM advancement

	// yieldStopGrace is the graceful-stop window used when an
	// ElectionYielded / GroupClaimYielded event stops the briefly-run
	// container(s) (O14c). Short by default — the container only ran for
	// the reconcile window and the real winner is already starting.
	yieldStopGrace time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// defaultYieldStopGrace is the RuntimeBridge default graceful-stop window
// for a yield-driven container stop (O14c). Matches the election
// manager's defaultYieldStopGrace so the two halves of the yield agree.
const defaultYieldStopGrace = 1 * time.Second

// RuntimeBridgeOption configures a RuntimeBridge.
type RuntimeBridgeOption func(*RuntimeBridge)

// WithRuntimeBridgeLogger sets the logger.
func WithRuntimeBridgeLogger(logger *zap.Logger) RuntimeBridgeOption {
	return func(b *RuntimeBridge) { b.logger = logger }
}

// WithRuntimeBridgeYieldStopGrace sets the graceful-stop window used when
// a post-hoc election yield stops the briefly-run container(s) (O14c).
// Non-positive values are ignored (default defaultYieldStopGrace).
func WithRuntimeBridgeYieldStopGrace(d time.Duration) RuntimeBridgeOption {
	return func(b *RuntimeBridge) {
		if d > 0 {
			b.yieldStopGrace = d
		}
	}
}

// WithRuntimeBridgeCapsuleManager wires the capsule manager used by
// the group-claim path to advance member capsules through the
// Announced → Assigned lifecycle states before StartGroup runs. The
// per-replica election path advances state through the election
// manager; the group-claim path bypasses it, so the bridge drives
// the transitions explicitly. Optional: without it, group members
// will fail MarkRunning because the runtime's lifecycle adapter
// cannot transition from Announced directly to Running.
func WithRuntimeBridgeCapsuleManager(m *capsule.Manager) RuntimeBridgeOption {
	return func(b *RuntimeBridge) { b.capsuleMgr = m }
}

// NewRuntimeBridge creates a bridge between the event bus and the
// runtime handler.
func NewRuntimeBridge(handler *falakrt.Handler, bus events.Bus, opts ...RuntimeBridgeOption) *RuntimeBridge {
	b := &RuntimeBridge{
		handler:        handler,
		eventBus:       bus,
		logger:         zap.NewNop(),
		yieldStopGrace: defaultYieldStopGrace,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Start begins listening for ElectionWon, CapsuleRunning, GroupClaimWon,
// ElectionYielded, and GroupClaimYielded events.
func (b *RuntimeBridge) Start(ctx context.Context) {
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.wg.Add(5)
	go b.electionLoop()
	go b.runningLoop()
	go b.groupLoop()
	go b.yieldLoop()
	go b.groupYieldLoop()
}

// Stop cancels the listeners and waits for clean exit.
func (b *RuntimeBridge) Stop() {
	b.cancel()
	b.wg.Wait()
}

// electionLoop dispatches ElectionWon events to the runtime handler.
func (b *RuntimeBridge) electionLoop() {
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

// runningLoop forwards CapsuleRunning events to the handler's group
// dependency coordinator so parked sibling starts can release. The
// coordinator filters non-group capsules internally; this loop fans
// every Running event to it without filtering on the bridge side.
func (b *RuntimeBridge) runningLoop() {
	defer b.wg.Done()
	ch := b.eventBus.Subscribe(events.TypeCapsuleRunning)
	for {
		select {
		case <-b.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			running, ok := ev.(events.CapsuleRunning)
			if !ok {
				continue
			}
			b.handler.OnDependencyRunning(running.CapsuleID)
		}
	}
}

// groupLoop translates GroupClaimWon events from the node bus into the
// runtime package's GroupClaimWon shape and dispatches them to
// Handler.StartGroup. The runtime package stays free of node-internal
// imports; the bridge owns the translation.
//
// Before dispatching StartGroup the bridge advances each member's
// lifecycle through Announced → Electing → Assigned so the runtime
// adapter's later MarkRunning(Executing → Running) transition does
// not reject from an invalid prior state. The per-replica election
// path does this through the election manager's WinElection call;
// the group-claim path bypasses per-replica elections, so the
// bridge drives the same transitions explicitly here. Failures are
// logged at debug level and ignored — they typically indicate the
// FSM is already past Assigned (e.g. when StartGroup is re-driven
// after a transient failure).
func (b *RuntimeBridge) groupLoop() {
	defer b.wg.Done()
	ch := b.eventBus.Subscribe(events.TypeGroupClaimWon)
	for {
		select {
		case <-b.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			won, ok := ev.(events.GroupClaimWon)
			if !ok {
				continue
			}
			b.logger.Info("runtime bridge: dispatching GroupClaimWon",
				zap.String("group_id", won.GroupID),
				zap.String("cluster", won.ClusterPath),
				zap.Int("members", len(won.MemberIDs)))

			b.advanceGroupMembersToAssigned(won)

			b.handler.StartGroup(falakrt.GroupClaimWon{
				GroupID:     won.GroupID,
				ClusterPath: won.ClusterPath,
				MemberIDs:   append([]string(nil), won.MemberIDs...),
				NodeID:      won.NodeID,
				Score:       won.Score,
			})
		}
	}
}

// yieldLoop dispatches ElectionYielded events (O14c) to the runtime
// handler's StopContainer. StopContainer plants the O2 self-removal
// ignore-set entry BEFORE Stop+Remove, so the yield-stop is suppressed at
// the container event consumer and does NOT self-trigger a re-election
// (HARD INVARIANT #1). The stop also covers the start-vs-stop race: if the
// container has not been created yet, StopContainer is a clean no-op that
// still leaves the ignore entry standing, so the async start (if it lands)
// is suppressed by the handler's onContainerRunning backstop.
//
// The binding re-point (UnassignReplica → AssignReplica(winner)) and the
// FSM mirror (SyncStatus Assigned) are driven by the CapsuleHandler's own
// ElectionYielded subscriber — HARD INVARIANT #2 lives on the capsule side
// where the binding state does.
func (b *RuntimeBridge) yieldLoop() {
	defer b.wg.Done()
	ch := b.eventBus.Subscribe(events.TypeElectionYielded)
	for {
		select {
		case <-b.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			yielded, ok := ev.(events.ElectionYielded)
			if !ok {
				continue
			}
			b.logger.Info("runtime bridge: stopping yielded container (O14c)",
				zap.String("capsule_id", yielded.CapsuleID),
				zap.String("replica_id", yielded.ReplicaID),
				zap.String("winner", yielded.WinnerNodeID))
			if err := b.handler.StopContainer(yielded.CapsuleID, yielded.ReplicaID, b.yieldStopGrace); err != nil {
				// Tolerate: the container may never have been created (start
				// lost the race) — the ignore entry is planted regardless,
				// which is the point. Debug, not Warn: this is the common,
				// benign case.
				b.logger.Debug("runtime bridge: yield stop returned error (often expected: container not yet started)",
					zap.String("capsule_id", yielded.CapsuleID),
					zap.String("replica_id", yielded.ReplicaID),
					zap.Error(err))
			}
		}
	}
}

// groupYieldLoop dispatches GroupClaimYielded events (O14c) by stopping
// every member the local node started, each through StopContainer (the O2
// ignore-set — HARD INVARIANT #1). Members of a same-node group are
// started with the fixed replica ID "0" (StartGroup's assignment), so the
// stop uses the same replica ID. The reservation was already re-pointed to
// the winner inside the election manager's reconcile; the FSM mirror +
// binding clears are driven by the CapsuleHandler's GroupClaimYielded
// subscriber. This loop is the container-teardown half only.
func (b *RuntimeBridge) groupYieldLoop() {
	defer b.wg.Done()
	ch := b.eventBus.Subscribe(events.TypeGroupClaimYielded)
	for {
		select {
		case <-b.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			yielded, ok := ev.(events.GroupClaimYielded)
			if !ok {
				continue
			}
			b.logger.Info("runtime bridge: stopping yielded group members (O14c)",
				zap.String("group_id", yielded.GroupID),
				zap.String("winner", yielded.WinnerNodeID),
				zap.Int("members", len(yielded.MemberIDs)))
			// Also cancel any parked starts for this group so a
			// dependency-released member does not start after we stop it.
			b.handler.CancelGroupStarts(yielded.GroupID)
			for _, mid := range yielded.MemberIDs {
				if err := b.handler.StopContainer(mid, "0", b.yieldStopGrace); err != nil {
					b.logger.Debug("runtime bridge: group yield member stop returned error (often expected: member not yet started)",
						zap.String("group_id", yielded.GroupID),
						zap.String("member", mid),
						zap.Error(err))
				}
			}
		}
	}
}

// advanceGroupMembersToAssigned walks each member of a same-node group
// through the Announced → Electing → Assigned lifecycle transitions
// using the capsule manager. The runtime's MarkRunning path expects
// the capsule to be in Executing state (which it can advance to from
// Assigned via StartExecution); without this advancement the runtime
// silently fails to publish CapsuleRunning and dependent siblings
// stay parked forever.
//
// Each transition is best-effort: SyncStatus rebuilds the FSM at the
// target state regardless of validation rules, so even capsules in
// unexpected states converge. Replica assignment is recorded so the
// per-replica node-failure detection path can still observe orphans
// if the runtime ever crashes mid-start.
func (b *RuntimeBridge) advanceGroupMembersToAssigned(won events.GroupClaimWon) {
	mgr := b.capsuleMgr
	if mgr == nil {
		return
	}
	for _, mid := range won.MemberIDs {
		id := capsule.CapsuleID(mid)
		if err := mgr.SyncStatus(id, enums.CapsuleStatusEnum.Assigned()); err != nil {
			b.logger.Debug("group bridge: sync member to Assigned failed",
				zap.String("capsule_id", mid), zap.Error(err))
		}
		if err := mgr.AssignReplica(id, capsule.ReplicaID("0"), won.NodeID); err != nil {
			b.logger.Debug("group bridge: AssignReplica failed",
				zap.String("capsule_id", mid),
				zap.String("node_id", won.NodeID),
				zap.Error(err))
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
