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
	TypeSyncCompleted = "sync.completed"
	TypeSyncFailed    = "sync.failed"
	TypeSyncRequested = "sync.requested"
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
	TypeCapsuleCreated   = "capsule.created"
	TypeCapsuleAnnounced = "capsule.announced"
	TypeCapsuleReceived  = "capsule.received"
	TypeCapsuleAssigned  = "capsule.assigned"
	TypeCapsuleRunning   = "capsule.running"
	TypeCapsuleStopping  = "capsule.stopping"
	TypeCapsuleStopped   = "capsule.stopped"
	TypeCapsuleWithdrawn = "capsule.withdrawn"
	TypeCapsuleFailed    = "capsule.failed"
	TypeCapsuleUpdated   = "capsule.updated"
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

// CapsuleExecutionFailed is emitted when a running container exits
// unexpectedly or fails health checks. The capsule handler subscribes
// to this to fire re-election.
type CapsuleExecutionFailed struct {
	BaseEvent
	CapsuleID string
	Reason    string
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
