// Package phonebook provides peer storage and discovery.
package phonebook

import (
	"time"
)

// NodeStatus represents the health status of a node.
type NodeStatus string

const (
	nodeStatusActive      NodeStatus = "active"
	nodeStatusSuspected   NodeStatus = "suspected"
	nodeStatusQuarantined NodeStatus = "quarantined"
	nodeStatusFailed      NodeStatus = "failed"
	// nodeStatusDeparted marks a peer that left gracefully (via Drain).
	// We keep the row — public key, cert, addresses — so when the peer
	// restarts with the same identity its signed pubsub messages still
	// verify and the cluster picks it up transparently. SWIM does not
	// probe Departed peers; the next successful probe (or the next
	// libp2p reconnect) flips them back to Active.
	nodeStatusDeparted NodeStatus = "departed"
	// nodeStatusPendingAuth marks a freshly-added member that has been
	// announced (Step 2 NewMemberAnnounced) but whose auth handshake or
	// libp2p mesh is still settling. SWIM skips these peers so we don't
	// burn score on a peer that the ping protocol can't reach yet
	// (Bug #13). The monitor auto-promotes the entry to Active after the
	// configurable PendingAuthGrace window, OR sooner when the
	// authenticator finishes its handshake.
	nodeStatusPendingAuth NodeStatus = "pending_auth"
)

type nodeStatusEnum struct{}

// NodeStatusEnum provides access to NodeStatus values.
var NodeStatusEnum nodeStatusEnum

func (nodeStatusEnum) Active() NodeStatus       { return nodeStatusActive }
func (nodeStatusEnum) Suspected() NodeStatus    { return nodeStatusSuspected }
func (nodeStatusEnum) Quarantined() NodeStatus  { return nodeStatusQuarantined }
func (nodeStatusEnum) Failed() NodeStatus       { return nodeStatusFailed }
func (nodeStatusEnum) Departed() NodeStatus     { return nodeStatusDeparted }
func (nodeStatusEnum) PendingAuth() NodeStatus  { return nodeStatusPendingAuth }

// Entry represents a known peer in a specific cluster.
type Entry struct {
	// Composite key
	NodeID      string `json:"node_id"`
	ClusterPath string `json:"cluster_path"`

	// Friendly operator-assigned name (e.g. "node1"). Travels in
	// JoinRequest.Capabilities.Metadata[node_name]. May be empty for
	// nodes that never published a name (older agents).
	Name string `json:"name,omitempty"`

	// Identity
	PublicKey []byte `json:"public_key"`

	// Connection info
	Addresses []string `json:"addresses"`

	// Location
	Region     string `json:"region"`
	Datacenter string `json:"datacenter"`

	// Capabilities (cached)
	Capabilities *Capabilities `json:"capabilities,omitempty"`

	// Timestamps
	FirstSeen     time.Time `json:"first_seen"`
	LastSeen      time.Time `json:"last_seen"`
	LastConnected time.Time `json:"last_connected"`
	UpdatedAt     time.Time `json:"updated_at"`

	// Reliability tracking (connection)
	ConnectionAttempts int     `json:"connection_attempts"`
	ConnectionSuccess  int     `json:"connection_success"`
	SuccessRate        float64 `json:"success_rate"`
	ConsecutiveFails   int     `json:"consecutive_fails"`

	// Health tracking (SWIM)
	ReliabilityScore float64    `json:"reliability_score"`
	LastProbeTime    time.Time  `json:"last_probe_time"`
	LastProbeSuccess bool       `json:"last_probe_success"`
	Status           NodeStatus `json:"status"`

	// Connection status
	IsConnected bool `json:"is_connected"`
}

// Capabilities describes a node's resources and features.
type Capabilities struct {
	CPUCores   int32             `json:"cpu_cores"`
	MemoryMB   int64             `json:"memory_mb"`
	DiskGB     int64             `json:"disk_gb"`
	Datacenter string            `json:"datacenter"`
	Tags       []string          `json:"tags"`
	Metadata   map[string]string `json:"metadata"`
}

// IPhonebook defines the interface for peer storage.
type IPhonebook interface {
	// Lookup
	Get(nodeID string, clusterPath string) (*Entry, error)
	Exists(nodeID string, clusterPath string) (bool, error)
	GetByCluster(clusterPath string) ([]*Entry, error)
	GetByNode(nodeID string) ([]*Entry, error)
	GetBestPeers(clusterPath string, limit int) ([]*Entry, error)

	// Mutations
	Add(entry *Entry) error
	Update(entry *Entry) error
	Remove(nodeID string, clusterPath string) error
	RemoveAllForNode(nodeID string) error

	// Connection tracking
	RecordConnectionAttempt(nodeID string, clusterPath string, success bool) error
	RecordDisconnect(nodeID string, clusterPath string) error

	// Health tracking (SWIM)
	SetReliabilityScore(nodeID string, clusterPath string, score float64) error
	SetStatus(nodeID string, clusterPath string, status NodeStatus) error
	GetByStatus(clusterPath string, status NodeStatus) ([]*Entry, error)
	RecordProbe(nodeID string, clusterPath string, success bool) error

	// Maintenance
	Prune(maxAge time.Duration, maxConsecutiveFails int) (int, error)
	Count() (int, error)
	CountByCluster(clusterPath string) (int, error)

	// Lifecycle
	Close() error
}
