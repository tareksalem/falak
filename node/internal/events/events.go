// Package events provides internal event-driven communication between node components.
//
// Events are categorized into two types:
//   - Internal events: Consumed by node components for core functionality
//   - Observable events: Published for monitoring, metrics, and external integrations
//
// Observable events (may not have internal subscribers):
//   - PeerAuthenticated: Signals successful cluster authentication
//   - AuthenticationFailed: Signals authentication failures for monitoring
//   - SyncCompleted: Signals successful sync completion for metrics
//   - SyncFailed: Signals sync failures for monitoring
package events

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// Event is the base interface for all internal events.
type Event interface {
	EventType() string
	Timestamp() time.Time
}

// BaseEvent provides common event fields.
type BaseEvent struct {
	OccurredAt time.Time
}

func (e BaseEvent) Timestamp() time.Time {
	return e.OccurredAt
}

// NewBaseEvent creates a new BaseEvent with current timestamp.
func NewBaseEvent() BaseEvent {
	return BaseEvent{OccurredAt: time.Now()}
}

// --- Authentication Events ---

const (
	TypePeerAuthenticated    = "auth.peer_authenticated"
	TypeNewMemberAnnounced   = "auth.new_member_announced"
	TypeNewMemberReceived    = "auth.new_member_received"
	TypeAuthenticationFailed = "auth.authentication_failed"
	TypeSessionStale         = "auth.session_stale"
	TypeReauthWithPeer       = "auth.reauth_with_peer"
	TypeMemberAdmitted       = "auth.member_admitted"
)

// PeerAuthenticated is emitted when we successfully authenticate to a cluster.
// This is an observable event for monitoring and external integrations.
type PeerAuthenticated struct {
	BaseEvent
	ClusterPath   string
	VoucherNodeID string
}

func (e PeerAuthenticated) EventType() string { return TypePeerAuthenticated }

// NewMemberAnnounced is emitted when we broadcast a new member to the cluster.
type NewMemberAnnounced struct {
	BaseEvent
	NodeID       string
	ClusterPath  string
	Addresses    []string
	PublicKey    []byte
	Capabilities *Capabilities
}

func (e NewMemberAnnounced) EventType() string { return TypeNewMemberAnnounced }

// NewMemberReceived is emitted when we receive a new member announcement from PubSub.
type NewMemberReceived struct {
	BaseEvent
	NodeID        string
	ClusterPath   string
	Addresses     []string
	PublicKey     []byte
	Capabilities  *Capabilities
	VoucherNodeID string
	JoinedAt      *timestamppb.Timestamp
}

func (e NewMemberReceived) EventType() string { return TypeNewMemberReceived }

// AuthenticationFailed is emitted when authentication fails.
// This is an observable event for monitoring and alerting.
type AuthenticationFailed struct {
	BaseEvent
	ClusterPath string
	TargetPeer  string
	Reason      string
}

func (e AuthenticationFailed) EventType() string { return TypeAuthenticationFailed }

// SessionStale is emitted when a session becomes stale and needs re-authentication.
type SessionStale struct {
	BaseEvent
	ClusterPath string
}

func (e SessionStale) EventType() string { return TypeSessionStale }

// ReauthWithPeerRequested is emitted by the reconnector after it has
// re-dialed a specific known-but-disconnected peer and re-established a
// libp2p connection. A re-dialed connection is NOT cluster membership —
// the session must be re-authenticated. The ReauthSubscriber consumes
// this and drives the existing Authenticate flow pinned to PeerID (with
// a phonebook fallback if that specific peer refuses). Keeping the
// re-auth logic in the auth module preserves the module boundary: the
// reconnector only dials and emits; auth owns the handshake.
type ReauthWithPeerRequested struct {
	BaseEvent
	ClusterPath string
	PeerID      string
}

func (e ReauthWithPeerRequested) EventType() string { return TypeReauthWithPeer }

// MemberAdmitted is emitted by the voucher immediately after it has fully
// authenticated a new cluster member (AuthComplete sent, joiner promoted to
// Active). It is the event-driven seam that triggers Layer 1 of the join
// convergence design (O13): the syncer subscribes and actively pushes the
// new member to every existing Active peer so nobody has to wait for the
// best-effort Step-2 PubSub broadcast or the periodic anti-entropy tail.
//
// This is an internal event (the syncer reacts to it). Auth never calls the
// syncer directly — it publishes MemberAdmitted and the syncer owns the push.
//
// NewMember carries the full member info the voucher already holds from the
// JoinRequest, so the syncer can build the push payload without a phonebook
// re-read (which would race the very Add the joiner triggered).
type MemberAdmitted struct {
	BaseEvent
	ClusterPath string
	NewMember   MemberInfo
}

// EventType returns the event type identifier for MemberAdmitted.
func (e MemberAdmitted) EventType() string { return TypeMemberAdmitted }

// --- Cluster Lifecycle Events ---

const (
	TypeClusterJoinRequested   = "cluster.join_requested"
	TypeClusterJoined          = "cluster.joined"
	TypeClusterJoinFailed      = "cluster.join_failed"
	TypeClusterMembersReceived = "cluster.members_received"
)

// ClusterJoinRequested is emitted when a node wants to join a cluster.
type ClusterJoinRequested struct {
	BaseEvent
	ClusterPath    string
	PSK            []byte
	BootstrapPeers []string
}

func (e ClusterJoinRequested) EventType() string { return TypeClusterJoinRequested }

// ClusterJoined is emitted when a node successfully joins a cluster.
type ClusterJoined struct {
	BaseEvent
	ClusterPath   string
	VoucherNodeID string // The peer that vouched for us (empty if first node)
}

func (e ClusterJoined) EventType() string { return TypeClusterJoined }

// ClusterJoinFailed is emitted when joining a cluster fails.
type ClusterJoinFailed struct {
	BaseEvent
	ClusterPath string
	Reason      string
}

func (e ClusterJoinFailed) EventType() string { return TypeClusterJoinFailed }

// ClusterMembersReceived is emitted when we receive the cluster member list after authentication.
type ClusterMembersReceived struct {
	BaseEvent
	ClusterPath string
	Members     []MemberInfo
}

func (e ClusterMembersReceived) EventType() string { return TypeClusterMembersReceived }

// MemberInfo contains information about a cluster member.
type MemberInfo struct {
	NodeID       string
	Addresses    []string
	PublicKey    []byte
	Capabilities *Capabilities
	JoinedAt     *timestamppb.Timestamp
}

// Capabilities describes a node's resources.
type Capabilities struct {
	CPUCores   int32
	MemoryMB   int64
	DiskGB     int64
	Datacenter string
	Tags       []string
	Metadata   map[string]string
}

// --- Sync Events ---

const (
	TypeSyncCompleted        = "sync.completed"
	TypeSyncFailed           = "sync.failed"
	TypeSyncRequested        = "sync.requested"
	TypeMemberPushDelivered  = "sync.member_push_delivered"
	TypeMemberPushFanoutDone = "sync.member_push_fanout_done"
)

// SyncCompleted is emitted after successful member list synchronization.
// This is an observable event for metrics and monitoring.
type SyncCompleted struct {
	BaseEvent
	ClusterPath string
	MemberCount int
	NewMembers  int
	SyncedFrom  string // Peer ID we synced from
}

func (e SyncCompleted) EventType() string { return TypeSyncCompleted }

// SyncFailed is emitted when synchronization fails.
// This is an observable event for monitoring and alerting.
type SyncFailed struct {
	BaseEvent
	ClusterPath string
	Peer        string
	Reason      string
}

func (e SyncFailed) EventType() string { return TypeSyncFailed }

// SyncRequested is emitted when a sync is needed.
type SyncRequested struct {
	BaseEvent
	ClusterPath   string
	Reason        string // "post_auth", "periodic", "unknown_sender", etc.
	PreferredPeer string // Optional: sync from this peer first if available
}

func (e SyncRequested) EventType() string { return TypeSyncRequested }

// MemberPushDelivered is emitted by the Layer-1 voucher fan-out after a
// SyncPush carrying a newly-admitted member has been successfully delivered
// to one existing peer (the receiver ACKed with no error). It is an
// observable event: tests block on it to assert deterministic convergence
// (no sleeps), and operators can use it to trace fan-out.
type MemberPushDelivered struct {
	BaseEvent
	ClusterPath  string
	NewMemberID  string // the member being propagated
	TargetPeer   string // the existing peer we pushed to
}

// EventType returns the event type identifier for MemberPushDelivered.
func (e MemberPushDelivered) EventType() string { return TypeMemberPushDelivered }

// MemberPushFanoutDone is emitted once the Layer-1 fan-out for a single
// MemberAdmitted has finished attempting every Active target. Delivered is
// the count of successful pushes; Targets is the total attempted. Tests use
// this to block until the whole fan-out round completes regardless of how
// many targets there were.
type MemberPushFanoutDone struct {
	BaseEvent
	ClusterPath string
	NewMemberID string
	Targets     int
	Delivered   int
}

// EventType returns the event type identifier for MemberPushFanoutDone.
func (e MemberPushFanoutDone) EventType() string { return TypeMemberPushFanoutDone }

// --- Health Events ---

const (
	TypeNodeSuspected   = "health.node_suspected"
	TypeNodeQuarantined = "health.node_quarantined"
	TypeNodeFailed      = "health.node_failed"
	TypeNodeRecovered   = "health.node_recovered"
	TypeNodeProbeResult = "health.probe_result"
	TypeNodeDeparting   = "health.node_departing"
)

// NodeDeparting is emitted when a node begins a graceful drain. Peers
// should treat this like a preemptive NodeFailed — update the phonebook,
// re-elect orphaned replicas immediately. The node will shut down
// shortly after publishing this event.
type NodeDeparting struct {
	BaseEvent
	NodeID string
}

func (e NodeDeparting) EventType() string { return TypeNodeDeparting }

// NodeSuspected is emitted when a node's reliability score crosses the suspected threshold.
type NodeSuspected struct {
	BaseEvent
	NodeID      string
	ClusterPath string
	Score       float64
}

func (e NodeSuspected) EventType() string { return TypeNodeSuspected }

// NodeQuarantined is emitted when a node's reliability score crosses the quarantine threshold.
// The node is isolated and will be removed if it doesn't recover within the quarantine timeout.
type NodeQuarantined struct {
	BaseEvent
	NodeID      string
	ClusterPath string
	Score       float64
}

func (e NodeQuarantined) EventType() string { return TypeNodeQuarantined }

// NodeFailed is emitted when a quarantined node fails to recover within the timeout.
// The node is removed from the phonebook. Capsule re-election should be triggered.
type NodeFailed struct {
	BaseEvent
	NodeID      string
	ClusterPath string
}

func (e NodeFailed) EventType() string { return TypeNodeFailed }

// NodeRecovered is emitted when a previously suspected or quarantined node responds to a probe.
// Its score is reset to 0 and status returns to active.
type NodeRecovered struct {
	BaseEvent
	NodeID      string
	ClusterPath string
	PreviousStatus string // "suspected" or "quarantined"
}

func (e NodeRecovered) EventType() string { return TypeNodeRecovered }

// NodeProbeResult is emitted after every probe attempt (success or failure).
// This is an observable event for metrics and debugging.
type NodeProbeResult struct {
	BaseEvent
	NodeID      string
	ClusterPath string
	Success     bool
	ProbeType   string  // "direct", "indirect"
	Score       float64 // Score after this probe
}

func (e NodeProbeResult) EventType() string { return TypeNodeProbeResult }

// --- Capsule Events ---

const (
	TypeCapsuleCreated        = "capsule.created"
	TypeCapsuleAnnounced      = "capsule.announced"
	TypeCapsuleReceived       = "capsule.received"
	TypeCapsuleAssigned       = "capsule.assigned"
	TypeCapsuleRunning        = "capsule.running"
	TypeCapsuleStopping       = "capsule.stopping"
	TypeCapsuleStopped        = "capsule.stopped"
	TypeCapsuleWithdrawn      = "capsule.withdrawn"
	TypeCapsuleFailed         = "capsule.failed"
	TypeCapsuleUpdated        = "capsule.updated"
	TypeCapsuleGroupReleased  = "capsule.group_released"
)

// CapsuleCreated is emitted when a capsule is created locally.
type CapsuleCreated struct {
	BaseEvent
	CapsuleID   string
	CapsuleName string
	Orbit       string
	ClusterPath string
}

func (e CapsuleCreated) EventType() string { return TypeCapsuleCreated }

// CapsuleReceived is emitted when a capsule announcement is received from the mesh.
type CapsuleReceived struct {
	BaseEvent
	CapsuleID      string
	CapsuleName    string
	Orbit          string
	ClusterPath    string
	AnnouncingNode string
}

func (e CapsuleReceived) EventType() string { return TypeCapsuleReceived }

// CapsuleAnnounced is emitted when a capsule is published to its orbit topic.
type CapsuleAnnounced struct {
	BaseEvent
	CapsuleID   string
	CapsuleName string
	Orbit       string
	ClusterPath string
}

func (e CapsuleAnnounced) EventType() string { return TypeCapsuleAnnounced }

// CapsuleUpdated is emitted when a capsule spec is updated.
type CapsuleUpdated struct {
	BaseEvent
	CapsuleID   string
	CapsuleName string
	ClusterPath string
}

func (e CapsuleUpdated) EventType() string { return TypeCapsuleUpdated }

// CapsuleGroupReleased is emitted when a member capsule has been detached
// from its group as part of a non-cascade group delete. The member keeps
// running as a standalone capsule; its GroupID has been cleared and the
// capsule has been re-announced on its orbit. PreviousGroupID carries the
// group the capsule used to belong to so downstream subscribers
// (Phase 11A bridge teardown, observability, audit) can correlate the
// release with the now-deleted group without re-querying state.
type CapsuleGroupReleased struct {
	BaseEvent
	CapsuleID       string
	CapsuleName     string
	Orbit           string
	ClusterPath     string
	PreviousGroupID string
}

// EventType returns the event type identifier for CapsuleGroupReleased.
func (e CapsuleGroupReleased) EventType() string { return TypeCapsuleGroupReleased }

// CapsuleWithdrawn is emitted when a capsule is removed from the mesh.
type CapsuleWithdrawn struct {
	BaseEvent
	CapsuleID   string
	ClusterPath string
	Reason      string
}

func (e CapsuleWithdrawn) EventType() string { return TypeCapsuleWithdrawn }

// CapsuleRunning is emitted when a container is successfully started
// (either via cold start or snapshot restore) and confirmed running.
type CapsuleRunning struct {
	BaseEvent
	CapsuleID string
}

func (e CapsuleRunning) EventType() string { return TypeCapsuleRunning }

// FailureCategory classifies why a capsule failed, set at the failure site
// in the runtime and carried on CapsuleExecutionFailed. The execution
// reliability tracker uses it to decide whether a failure reflects on the
// node (count it) or on the capsule itself (ignore it). This mirrors the
// runtime package's FailureCategory across the module boundary (the runtime
// module cannot import node-internal packages).
//
// Enum pattern: private consts + private carrier struct + public var accessor.
type FailureCategory string

const (
	failureNodeAttributable FailureCategory = "node_attributable"
	failureCapsuleGlobal    FailureCategory = "capsule_global"
	failureAmbiguous        FailureCategory = "ambiguous"
)

type failureCategoryEnum struct{}

// FailureCategoryEnum is the public accessor for FailureCategory values.
var FailureCategoryEnum failureCategoryEnum

// NodeAttributable returns the node-at-fault category.
func (failureCategoryEnum) NodeAttributable() FailureCategory { return failureNodeAttributable }

// CapsuleGlobal returns the capsule-at-fault category (excluded from a
// node's execution reliability).
func (failureCategoryEnum) CapsuleGlobal() FailureCategory { return failureCapsuleGlobal }

// Ambiguous returns the unattributable category.
func (failureCategoryEnum) Ambiguous() FailureCategory { return failureAmbiguous }

// CapsuleExecutionFailed is emitted when a running container exits
// unexpectedly or fails health checks. The capsule handler subscribes
// to this to fire re-election.
type CapsuleExecutionFailed struct {
	BaseEvent
	CapsuleID string
	Reason    string

	// Category is the typed blame attribution set at the failure site.
	// Empty (the zero value) is treated as Ambiguous by consumers — i.e.
	// counted as a node failure — which is the safe default.
	Category FailureCategory
}

func (e CapsuleExecutionFailed) EventType() string { return TypeCapsuleFailed }

// --- Scaling Events ---

const (
	TypeScaleUpNeeded   = "scaling.up_needed"
	TypeScaleDownNeeded = "scaling.down_needed"
	TypeScaleToZero     = "scaling.to_zero"
)

// ScaleUpNeeded is emitted when a capsule needs more replicas.
type ScaleUpNeeded struct {
	BaseEvent
	CapsuleID   string
	RuleName    string
	ClusterPath string
}

func (e ScaleUpNeeded) EventType() string { return TypeScaleUpNeeded }

// ScaleDownNeeded is emitted when a capsule has too many replicas.
type ScaleDownNeeded struct {
	BaseEvent
	CapsuleID   string
	RuleName    string
	ClusterPath string
}

func (e ScaleDownNeeded) EventType() string { return TypeScaleDownNeeded }

// ScaleToZero is emitted when a capsule should be scaled to zero.
type ScaleToZero struct {
	BaseEvent
	CapsuleID   string
	RuleName    string
	ClusterPath string
}

func (e ScaleToZero) EventType() string { return TypeScaleToZero }

// --- Momentum Events ---

const (
	TypeMomentumBoosted = "momentum.boosted"
	TypeMomentumReduced = "momentum.reduced"
)

// MomentumBoosted is emitted when a capsule's momentum increases.
type MomentumBoosted struct {
	BaseEvent
	CapsuleID string
	OldValue  int32
	NewValue  int32
	Reason    string
}

func (e MomentumBoosted) EventType() string { return TypeMomentumBoosted }

// MomentumReduced is emitted when a capsule's momentum decreases.
type MomentumReduced struct {
	BaseEvent
	CapsuleID string
	OldValue  int32
	NewValue  int32
	Reason    string
}

func (e MomentumReduced) EventType() string { return TypeMomentumReduced }

// --- Election Events ---

const (
	TypeElectionRequested = "election.requested"
	TypeElectionWon       = "election.won"
	TypeElectionLost      = "election.lost"
	TypeElectionFailed    = "election.failed"
	TypeElectionYielded   = "election.yielded"

	// TypeGroupClaimYielded is the group twin of TypeElectionYielded: a
	// node that briefly won a same-node group election but observed a
	// strictly-better rival during the post-hoc reconcile window steps
	// down, stopping every member it started.
	TypeGroupClaimYielded = "election.group_claim_yielded"

	// Group-mode election events (Phase 10.14). same-node CapsuleGroups
	// elect atomically: one claim covers every member's combined
	// resources + placement constraints. These events fire instead of
	// the per-replica election triggers above when a group has
	// Colocation == SameNode.
	TypeGroupClaimRequested      = "election.group_claim_requested"
	TypeGroupClaimWon            = "election.group_claim_won"
	TypeGroupClaimLost           = "election.group_claim_lost"
	TypeGroupClaimFailed         = "election.group_claim_failed"
	TypeGroupReelectionRequested = "election.group_reelection_requested"

	// Phase 10.15 / 10.16 events — surfaced by runtime + capsule
	// handler to coordinate rollback and image-pull-aware reservation
	// deadlines.
	TypeMemberPlacementFailed = "election.member_placement_failed"
	TypePullProgress          = "runtime.pull_progress"
)

// ElectionReason explains why an election was requested. It is used by the
// election manager for logging, prioritization, and metrics — never for
// algorithmic decisions.
type ElectionReason string

// Private backing constants for the ElectionReason enum. External callers
// access them through the ElectionReasonEnum accessor.
const (
	electionReasonInitial     ElectionReason = "initial"
	electionReasonScaleUp     ElectionReason = "scale_up"
	electionReasonNodeFailure ElectionReason = "node_failure"
	electionReasonManual      ElectionReason = "manual"
	electionReasonRebalance   ElectionReason = "rebalance"
)

// electionReasonEnum is the unexported carrier struct used to expose the
// valid ElectionReason values as methods on a single package-level
// accessor.
type electionReasonEnum struct{}

// ElectionReasonEnum is the public accessor for ElectionReason values.
// Use events.ElectionReasonEnum.Initial() instead of a bare constant.
var ElectionReasonEnum electionReasonEnum

// Initial returns the "initial" reason — a brand new capsule needs its
// first replicas placed.
func (electionReasonEnum) Initial() ElectionReason { return electionReasonInitial }

// ScaleUp returns the "scale_up" reason — an existing capsule needs an
// additional replica because its scaling rules triggered.
func (electionReasonEnum) ScaleUp() ElectionReason { return electionReasonScaleUp }

// NodeFailure returns the "node_failure" reason — a previously-running
// replica was orphaned by a node failure and needs to be re-placed.
func (electionReasonEnum) NodeFailure() ElectionReason { return electionReasonNodeFailure }

// Manual returns the "manual" reason — an operator explicitly requested
// re-election via the API.
func (electionReasonEnum) Manual() ElectionReason { return electionReasonManual }

// Rebalance returns the "rebalance" reason — a periodic optimization
// pass triggered re-election to improve placement.
func (electionReasonEnum) Rebalance() ElectionReason { return electionReasonRebalance }

// ElectionRequested is published by a decider (CapsuleHandler, ScalingMonitor,
// API, …) when an election needs to be run for a specific capsule replica.
//
// Every node fires this event locally when it observes the underlying
// trigger (capsule create, scale-up, node failure, capsule received from
// the mesh). Each node runs its own strategy, scores only itself, and
// publishes a Claim. The Manager dedupes by (CapsuleID, ReplicaID).
type ElectionRequested struct {
	BaseEvent
	CapsuleID      string
	ReplicaID      string
	Reason         ElectionReason
	ClusterPath    string
	PreviousNodeID string // populated for ElectionReasonEnum.NodeFailure()
	Priority       int    // higher = more urgent; failures fire as high priority
}

func (e ElectionRequested) EventType() string { return TypeElectionRequested }

// ElectionWon is emitted on the local event bus when this node has been
// selected to run a capsule replica. The runtime module subscribes to this
// to start the container.
type ElectionWon struct {
	BaseEvent
	CapsuleID   string
	ReplicaID   string
	ClusterPath string
	Score       float64 // gravity score that won the election (0–100)
	NodeID      string  // local node ID, for symmetry with the other variants
}

func (e ElectionWon) EventType() string { return TypeElectionWon }

// ElectionLost is emitted on the local event bus when another node won
// the election. This node mirrors the resulting state but takes no action.
type ElectionLost struct {
	BaseEvent
	CapsuleID    string
	ReplicaID    string
	ClusterPath  string
	WinnerNodeID string
}

func (e ElectionLost) EventType() string { return TypeElectionLost }

// ElectionFailed is emitted on the local event bus when no node could be
// elected (no eligible candidates, timeout, all eligible nodes refused).
// The capsule is then marked as failed in the manager and the failure
// propagates through the mesh as part of normal capsule status gossip.
type ElectionFailed struct {
	BaseEvent
	CapsuleID   string
	ReplicaID   string
	ClusterPath string
	Reason      string // human-readable failure reason
}

func (e ElectionFailed) EventType() string { return TypeElectionFailed }

// ElectionYielded is emitted (O14c) when this node briefly reported Won
// for a replica but, during the bounded post-hoc reconcile window,
// observed a strictly-better rival claim and stepped down to avoid
// duplicate execution. It is the "un-win" signal.
//
// Two subscribers react, in this order:
//   - RuntimeBridge routes it to runtime.Handler.StopContainer, which
//     plants the O2 self-removal ignore-set entry BEFORE Stop+Remove so
//     the yield-stop does NOT self-trigger a re-election (HARD INVARIANT #1).
//   - CapsuleHandler re-points the replica binding to the real winner
//     (UnassignReplica then AssignReplica(winner)) and mirrors the winner
//     as remote via SyncStatus(Assigned), identical to handleElectionLost
//     (HARD INVARIANT #2). B becomes a clean loser that briefly ran.
//
// Yielding is one-shot and terminal: a yielded round never re-enters
// reconcile and never re-elects off the yield.
type ElectionYielded struct {
	BaseEvent
	CapsuleID    string
	ReplicaID    string
	ClusterPath  string
	WinnerNodeID string
}

// EventType returns the event type identifier for ElectionYielded.
func (e ElectionYielded) EventType() string { return TypeElectionYielded }

// GroupClaimYielded is the group twin of ElectionYielded (O14c). When a
// node that briefly won a same-node group election observes a
// strictly-better rival during the reconcile window, it steps down and
// stops every member it started. The election Manager re-points the
// group's capacity reservation to the winner inside its reconcile loop
// (a single recordReservation call, atomic watchdog re-arm); this event
// drives ONLY the node-side container stops + FSM mirror. It does NOT
// trigger a re-election and does NOT consume a placement-retry slot — the
// winner is already known (WinnerNodeID).
type GroupClaimYielded struct {
	BaseEvent
	GroupID      string
	ClusterPath  string
	MemberIDs    []string // topological order, as started
	WinnerNodeID string
}

// EventType returns the event type identifier for GroupClaimYielded.
func (e GroupClaimYielded) EventType() string { return TypeGroupClaimYielded }

// GroupClaimRequested is fired locally on every node when a same-node
// CapsuleGroup needs an atomic placement. Unlike per-replica
// ElectionRequested, the claim covers every member's combined
// resources + AND of all placement rules. The winning node alone
// starts the members in topological order (see runtime.StartGroup).
type GroupClaimRequested struct {
	BaseEvent
	GroupID     string
	ClusterPath string
	// MemberIDs lists the member capsule IDs in topological order.
	// The runtime handler walks this list when starting the group on
	// the winning node; the election manager uses it to compute the
	// combined-fit score against the local node.
	MemberIDs []string
	// Reason explains why the claim was requested (initial placement,
	// re-election after a node failure, retry after rollback).
	Reason ElectionReason
	// ExcludeNodes lists nodes the previous round already failed on.
	// Used after rollback to avoid re-electing on the same node.
	ExcludeNodes []string
	Priority     int
}

func (e GroupClaimRequested) EventType() string { return TypeGroupClaimRequested }

// GroupClaimWon is emitted when the local node has been selected to host
// every member of a same-node CapsuleGroup. The runtime handler's
// StartGroup is the primary subscriber.
type GroupClaimWon struct {
	BaseEvent
	GroupID     string
	ClusterPath string
	MemberIDs   []string // topological order
	NodeID      string   // local node ID
	Score       float64
}

func (e GroupClaimWon) EventType() string { return TypeGroupClaimWon }

// GroupClaimLost is emitted when another node won the group election.
type GroupClaimLost struct {
	BaseEvent
	GroupID      string
	ClusterPath  string
	WinnerNodeID string
}

func (e GroupClaimLost) EventType() string { return TypeGroupClaimLost }

// GroupClaimFailed is emitted when no node could be elected for the
// group (no node fits all members, timeout, all eligible nodes refused).
// Triggers the placement_failed state on the group capsule; recovery
// fires automatically when cluster membership grows (NodeJoined).
type GroupClaimFailed struct {
	BaseEvent
	GroupID     string
	ClusterPath string
	Reason      string
}

func (e GroupClaimFailed) EventType() string { return TypeGroupClaimFailed }

// GroupReelectionRequested fires when the node currently hosting a
// same-node group dies, when a placement rollback round picks a new
// node, or when the cluster grows and a previously placement-failed
// group becomes a candidate for recovery (10.17). The capsule handler
// emits it; the election manager treats it like a fresh
// GroupClaimRequested with the failed node + ExcludeNodes added to the
// election manager's exclude list.
//
// FailedNodeID is the node we are recovering FROM on this specific
// emission (the holder that just died, or the rollback winner that
// just failed placement). It is empty on a 10.17 NodeJoined-driven
// recovery, where no single node is the cause — the recovery is
// triggered by cluster growth.
//
// ExcludeNodes is the cumulative set of nodes from prior failed
// placement rounds for this group. The election manager merges
// FailedNodeID + ExcludeNodes before computing the eligible set.
type GroupReelectionRequested struct {
	BaseEvent
	GroupID      string
	ClusterPath  string
	MemberIDs    []string
	FailedNodeID string
	ExcludeNodes []string
}

func (e GroupReelectionRequested) EventType() string { return TypeGroupReelectionRequested }

// MemberPlacementFailed is emitted by the runtime when a member of a
// same-node group fails to start beyond its restart_limit (image pull
// error, container start failure, healthcheck never passes). The
// group's manager subscribes to this to drive rollback: stop siblings,
// release capacity reservation, re-elect on a different node.
type MemberPlacementFailed struct {
	BaseEvent
	GroupID     string
	CapsuleID   string
	NodeID      string
	ClusterPath string
	Reason      string
}

func (e MemberPlacementFailed) EventType() string { return TypeMemberPlacementFailed }

// PullProgress is emitted periodically by the runtime image-pull layer
// while a pull is making progress. The election manager uses these
// heartbeats to extend the capacity-reservation deadline so slow image
// pulls do not falsely time out and trigger rollback.
type PullProgress struct {
	BaseEvent
	GroupID         string
	CapsuleID       string
	NodeID          string
	ClusterPath     string
	BytesRemaining  int64
	BytesPerSecond  int64
}

func (e PullProgress) EventType() string { return TypePullProgress }
