package metrics

import (
	"fmt"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/election/gravity"
	"github.com/tareksalem/falak/node/phonebook"
)

// CapsuleCountProvider supplies the number of capsules currently running
// on a given node, scoped by cluster path. The metrics package does not
// own this information; the node-side wiring fulfills the interface by
// asking the capsule manager.
//
// The interface is intentionally narrow: a single method, no caching, no
// labels. The provider call happens once per gravity calculation so the
// implementation should be O(small) — typically an in-memory map lookup.
type CapsuleCountProvider interface {
	// CountForNode returns how many capsule replicas the named node is
	// currently hosting in the given cluster. Returns 0 (not an error)
	// when the node is unknown — gravity will then treat it as empty.
	CountForNode(clusterPath, nodeID string) int
}

// noCapsuleCounts is the zero CapsuleCountProvider used when the manager
// is constructed without a node-side bridge. It always returns 0 so
// gravity calculation still works for tests and bootstrap scenarios.
type noCapsuleCounts struct{}

func (noCapsuleCounts) CountForNode(string, string) int { return 0 }

// ExecutionReliabilityReader supplies the node-global execution reliability
// score in [0, 1]. The Provider only READS it — the execution-reliability
// tracker owns the state and its mutation (it subscribes to the event bus).
// Kept as a narrow interface so the metrics package stays free of the
// reliability package and there is no import cycle.
type ExecutionReliabilityReader interface {
	// Current returns the live execution-reliability score in [0, 1].
	Current() float64
}

// Provider adapts the metrics Manager and the phonebook to the
// gravity.StateProvider interface used by the election package. It is
// the seam where election (which knows nothing about phonebook or
// metrics tables) meets the rest of the node.
//
// Provider is read-only and safe for concurrent use. It performs no
// caching of its own — every query reads from the underlying store and
// phonebook so callers always see the freshest state.
type Provider struct {
	manager       *Manager
	phonebook     phonebook.IPhonebook
	localNodeID   string
	capsuleCounts CapsuleCountProvider

	// neutralReliability is the optimistic prior applied to a node that has
	// no connection history (ConnectionAttempts == 0). A node that has never
	// been probed has NO DATA, not "0% reliable"; treating it as 0 would let
	// the reliability factor zero out the whole gravity score for a freshly
	// joined node. Optimistic-until-proven (default 1.0) matches SWIM/Consul/
	// Cassandra semantics and is what gravity's factorReliability already
	// documents. The prior is applied ONLY in this read path — the stored
	// phonebook SuccessRate stays honest because it feeds health/eviction/sync.
	neutralReliability float64

	// execReliability supplies the node's execution-reliability score. When
	// nil (tests, bootstrap before the tracker is wired) buildState falls
	// back to neutralExecutionReliability so the gravity factor never zeroes
	// out a node that simply has no tracker attached yet.
	execReliability ExecutionReliabilityReader

	// neutralExecutionReliability is the score used when execReliability is
	// nil. It matches the tracker's zero-history prior (0.8) so behaviour is
	// continuous whether or not a tracker is wired.
	neutralExecutionReliability float64
}

// ProviderOption configures a Provider.
type ProviderOption func(*Provider)

// WithCapsuleCounts wires in a counter for running capsules per node.
// Without this, the running-capsule load penalty is always zero — fine
// for tests, but production should always set this.
func WithCapsuleCounts(c CapsuleCountProvider) ProviderOption {
	return func(p *Provider) {
		p.capsuleCounts = c
	}
}

// WithNeutralReliability overrides the optimistic reliability prior applied
// to nodes with no connection history (ConnectionAttempts == 0). The default
// is 1.0 (fully optimistic, matching gravity.factorReliability's documented
// "defaulting to 1.0 for newly-joined nodes"). Lower it to make the scheduler
// more cautious about unproven nodes. The value only affects the gravity-state
// read path; it never mutates the stored phonebook SuccessRate.
func WithNeutralReliability(v float64) ProviderOption {
	return func(p *Provider) {
		p.neutralReliability = v
	}
}

// WithExecutionReliability wires the node-global execution-reliability reader
// (the tracker) into the provider's read path. Without it, buildState uses the
// optimistic neutral default (0.8).
func WithExecutionReliability(r ExecutionReliabilityReader) ProviderOption {
	return func(p *Provider) {
		p.execReliability = r
	}
}

// NewProvider constructs a Provider for the given metrics manager and
// phonebook. The localNodeID is the libp2p peer ID of the node hosting
// this provider — used by LocalNode to differentiate "me" from peers.
func NewProvider(
	mgr *Manager,
	pb phonebook.IPhonebook,
	localNodeID string,
	opts ...ProviderOption,
) *Provider {
	p := &Provider{
		manager:                     mgr,
		phonebook:                   pb,
		localNodeID:                 localNodeID,
		capsuleCounts:               noCapsuleCounts{},
		neutralReliability:          1.0,
		neutralExecutionReliability: 0.8,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// LocalNode satisfies gravity.StateProvider. Combines:
//   - phonebook entry for the local node (labels, status, datacenter)
//   - latest local snapshot from metrics.Manager (resources)
//   - capsule count from CapsuleCountProvider (load penalty)
//
// Returns an error when the phonebook does not have an entry for the
// local node — that should never happen in practice because every node
// inserts itself on cluster join, but we surface it instead of silently
// returning a zero value.
func (p *Provider) LocalNode(clusterPath string) (gravity.NodeState, error) {
	entry, err := p.phonebook.Get(p.localNodeID, clusterPath)
	if err != nil || entry == nil {
		return gravity.NodeState{}, fmt.Errorf("metrics provider: local node %s not in phonebook for cluster %s",
			p.localNodeID, clusterPath)
	}

	snap, err := p.manager.LocalLatest()
	if err != nil {
		return gravity.NodeState{}, fmt.Errorf("metrics provider: local snapshot: %w", err)
	}

	state := p.buildState(p.localNodeID, clusterPath, entry, snap)
	state.RunningCapsuleCount = p.capsuleCounts.CountForNode(clusterPath, p.localNodeID)
	return state, nil
}

// buildState assembles a gravity.NodeState from a phonebook entry and a
// metrics snapshot. Fields that exist only in one source are taken from
// there; fields that exist in both prefer the metrics snapshot because
// it is fresher.
//
// When snap is the zero Snapshot (no metrics yet for this node) the
// resource fields fall back to the phonebook capabilities (total CPU /
// memory advertised at cluster join). The "free" fields then default
// to the totals — the gravity calculator treats this as "node is empty",
// which is conservative for newly-joined nodes that haven't reported yet.
//
// Reliability prior: a node with zero connection attempts has NO DATA, not
// "0% reliable". For such a node we substitute the optimistic neutral prior
// (default 1.0) instead of the stored SuccessRate (which is 0 by default and
// would otherwise zero out gravity's reliability factor for a freshly-joined
// node). Once the node has any connection history (ConnectionAttempts > 0) we
// use the honest measured SuccessRate. The stored phonebook value is NEVER
// mutated here — it feeds health/eviction/sync and must stay truthful.
//
// Execution reliability is read from the wired tracker (if any) or the
// optimistic neutral default (0.8). buildState only reads; it never mutates
// tracker state.
func (p *Provider) buildState(
	nodeID string,
	clusterPath string,
	entry *phonebook.Entry,
	snap Snapshot,
) gravity.NodeState {
	reliability := entry.SuccessRate
	if entry.ConnectionAttempts == 0 {
		reliability = p.neutralReliability
	}

	execReliability := p.neutralExecutionReliability
	if p.execReliability != nil {
		execReliability = p.execReliability.Current()
	}

	state := gravity.NodeState{
		NodeID:               nodeID,
		ClusterPath:          clusterPath,
		Datacenter:           entry.Datacenter,
		Region:               entry.Region,
		Labels:               labelsFromEntry(entry),
		Status:               translateStatus(entry.Status),
		ReliabilityScore:     reliability,
		ExecutionReliability: execReliability,
	}

	// If we have a fresh snapshot, use it for resource fields.
	if !snap.CapturedAt.IsZero() {
		state.Resources = gravity.Resources{
			CPUCoresTotal: snap.CPU.Cores,
			CPUCoresFree:  computeFreeCPU(snap.CPU.Cores, snap.CPU.UsedPercent),
			MemoryMBTotal: snap.Memory.TotalMB,
			MemoryMBFree:  snap.Memory.AvailableMB,
			DiskMBTotal:   snap.Disk.TotalMB,
			DiskMBFree:    snap.Disk.FreeMB,
		}
		return state
	}

	// Fall back to phonebook capabilities for nodes without fresh metrics.
	if caps := entry.Capabilities; caps != nil {
		state.Resources = gravity.Resources{
			CPUCoresTotal: caps.CPUCores,
			CPUCoresFree:  caps.CPUCores, // assume empty until we hear otherwise
			MemoryMBTotal: caps.MemoryMB,
			MemoryMBFree:  caps.MemoryMB,
			DiskMBTotal:   caps.DiskGB * 1024, // capabilities track disk in GB
			DiskMBFree:    caps.DiskGB * 1024,
		}
	}
	return state
}

// computeFreeCPU translates "X cores at Y% usage" into "Z free cores"
// for the gravity headroom calculation. The result is rounded down so
// the calculator never overestimates available capacity.
func computeFreeCPU(cores int32, usedPercent float64) int32 {
	if cores <= 0 {
		return 0
	}
	if usedPercent <= 0 {
		return cores
	}
	if usedPercent >= 100 {
		return 0
	}
	free := float64(cores) * (1 - usedPercent/100)
	return int32(free)
}

// labelsFromEntry extracts the labels from a phonebook entry's
// capabilities. The entry's Capabilities.Metadata is the source-of-truth
// for node labels (set during the auth handshake when the node joins).
func labelsFromEntry(entry *phonebook.Entry) capsule.Labels {
	if entry.Capabilities == nil || entry.Capabilities.Metadata == nil {
		return capsule.Labels{}
	}
	out := make(capsule.Labels, len(entry.Capabilities.Metadata))
	for k, v := range entry.Capabilities.Metadata {
		out[k] = v
	}
	return out
}

// translateStatus converts a phonebook NodeStatus to the gravity
// NodeStatus. The two enums use slightly different string values so a
// straight cast is unsafe.
func translateStatus(s phonebook.NodeStatus) gravity.NodeStatus {
	switch s {
	case phonebook.NodeStatusEnum.Active():
		return gravity.NodeStatusEnum.Active()
	case phonebook.NodeStatusEnum.Suspected():
		return gravity.NodeStatusEnum.Suspected()
	case phonebook.NodeStatusEnum.Quarantined():
		return gravity.NodeStatusEnum.Quarantined()
	case phonebook.NodeStatusEnum.Failed():
		return gravity.NodeStatusEnum.Failed()
	}
	return gravity.NodeStatusEnum.Active()
}
