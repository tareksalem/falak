// Package gravity computes per-(capsule, node) suitability scores used to
// pick a winner during election.
//
// Gravity is the only place where capsule placement rules and node state
// meet. It is intentionally pure: every input is explicit, every factor is
// a small testable function, and the result is a deterministic Score on a
// 0–100 percentage scale. Concrete election strategies (delay-based,
// deterministic, …) build on top of these scores without owning any of
// the scoring logic themselves.
package gravity

import (
	"github.com/tareksalem/falak/capsule"
)

// NodeStatus mirrors the lifecycle health states tracked by the phonebook.
// Defined here to avoid pulling node/phonebook into the gravity package
// (the gravity package must stay free of node-internal dependencies so it
// can be tested in isolation).
type NodeStatus string

// Private constants for the NodeStatus enum. Callers access them through
// the NodeStatusEnum accessor below so that the set of valid values is
// controlled at the package boundary.
const (
	nodeStatusActive      NodeStatus = "active"
	nodeStatusSuspected   NodeStatus = "suspected"
	nodeStatusQuarantined NodeStatus = "quarantined"
	nodeStatusFailed      NodeStatus = "failed"
)

// nodeStatusEnum is the unexported carrier type used to expose the valid
// NodeStatus values as methods on a single package-level accessor. This is
// the project-wide enum pattern: private constants + private struct +
// public var accessor.
type nodeStatusEnum struct{}

// NodeStatusEnum is the public accessor for NodeStatus values. Use
// gravity.NodeStatusEnum.Active() instead of a bare constant.
var NodeStatusEnum nodeStatusEnum

// Active returns the "active" node status — the only state considered
// eligible for new work.
func (nodeStatusEnum) Active() NodeStatus { return nodeStatusActive }

// Suspected returns the "suspected" node status — SWIM has flagged the
// node as possibly unreachable but has not yet confirmed failure.
func (nodeStatusEnum) Suspected() NodeStatus { return nodeStatusSuspected }

// Quarantined returns the "quarantined" node status — the node is
// excluded from new work due to repeated failures or operator action.
func (nodeStatusEnum) Quarantined() NodeStatus { return nodeStatusQuarantined }

// Failed returns the "failed" node status — SWIM has confirmed the node
// is unreachable and its replicas should be redistributed.
func (nodeStatusEnum) Failed() NodeStatus { return nodeStatusFailed }

// IsHealthy returns true if the node is in a state acceptable for new work.
// Only Active nodes are considered healthy; suspected, quarantined, and
// failed nodes are excluded from elections.
func (s NodeStatus) IsHealthy() bool {
	return s == nodeStatusActive
}

// Resources holds the runtime-observable resource state of a node.
//
// Total fields represent the node's hardware capacity (advertised at boot
// and stable for the node's lifetime). Free fields represent currently
// available capacity, updated as capsules start and stop. Free is always
// less than or equal to Total.
//
// Memory and disk are reported in megabytes to keep arithmetic in int64
// without overflow on petabyte-class hosts.
type Resources struct {
	CPUCoresTotal int32
	CPUCoresFree  int32

	MemoryMBTotal int64
	MemoryMBFree  int64

	DiskMBTotal int64
	DiskMBFree  int64
}

// CPUUtilization returns the fraction of CPU currently in use, in [0, 1].
// Returns 0 when the node has not yet reported its capacity (CPUCoresTotal == 0).
func (r Resources) CPUUtilization() float64 {
	if r.CPUCoresTotal <= 0 {
		return 0
	}
	used := r.CPUCoresTotal - r.CPUCoresFree
	if used < 0 {
		used = 0
	}
	return float64(used) / float64(r.CPUCoresTotal)
}

// MemoryUtilization returns the fraction of memory currently in use, in [0, 1].
func (r Resources) MemoryUtilization() float64 {
	if r.MemoryMBTotal <= 0 {
		return 0
	}
	used := r.MemoryMBTotal - r.MemoryMBFree
	if used < 0 {
		used = 0
	}
	return float64(used) / float64(r.MemoryMBTotal)
}

// DiskUtilization returns the fraction of disk currently in use, in [0, 1].
func (r Resources) DiskUtilization() float64 {
	if r.DiskMBTotal <= 0 {
		return 0
	}
	used := r.DiskMBTotal - r.DiskMBFree
	if used < 0 {
		used = 0
	}
	return float64(used) / float64(r.DiskMBTotal)
}

// NodeState is the snapshot of a node used by gravity to score it against
// a capsule. Every field comes from a stable source: phonebook (identity,
// labels, status, reliability), local metrics (resources), or the capsule
// store (running capsule count).
//
// NodeState is immutable: callers construct one per election round and
// discard it. The StateProvider is responsible for assembling the snapshot
// from authoritative sources every time it is queried.
type NodeState struct {
	// NodeID is the libp2p peer ID of the node.
	NodeID string

	// ClusterPath is the cluster the node belongs to.
	ClusterPath string

	// Datacenter is the geographic / failure-domain identifier (used by
	// diversity scoring). May be empty for single-DC clusters.
	Datacenter string

	// Region is the broader geographic region (used by latency-aware factors).
	Region string

	// Labels are the node's key-value metadata, used by hardware/soft
	// placement matching.
	Labels capsule.Labels

	// Resources is the live resource state. Updated by the runtime as
	// capsules start and stop.
	Resources Resources

	// Status is the SWIM-derived health state for the node.
	Status NodeStatus

	// ReliabilityScore is the historical success rate from the phonebook,
	// in [0, 1]. Newly-joined nodes default to 1.0 (no history yet).
	ReliabilityScore float64

	// ExecutionReliability is the node's historical ability to actually
	// start and run capsules, in [0, 1], sourced from the node-local
	// execution-reliability tracker. Distinct from ReliabilityScore
	// (connection health). Nodes with no execution history default to the
	// optimistic prior (0.8) supplied by the metrics provider.
	ExecutionReliability float64

	// RunningCapsuleCount is the number of capsules currently running on
	// this node. Used by the load penalty factor.
	RunningCapsuleCount int
}

// StateProvider supplies the local node's state snapshot to the gravity
// calculator.
//
// Implementations must be safe for concurrent use. The provider is the
// integration boundary between the gravity package (pure scoring) and the
// rest of Falak (phonebook, capsule store, local metrics).
//
// Every node scores only itself during an election, so the provider has
// a single entry point: LocalNode. Cross-node scoring would rely on
// stale gossiped state and is intentionally not supported.
type StateProvider interface {
	// LocalNode returns the snapshot for the node currently running this
	// process, scoped to the given cluster.
	LocalNode(clusterPath string) (NodeState, error)
}
