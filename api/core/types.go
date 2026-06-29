// Package core provides the single source of truth for the Falak API.
// All business logic lives here as typed Go methods. Transport layers
// (gRPC, HTTP, CLI) are thin adapters that delegate to Core methods.
package core

import (
	"time"
)

// ObjectMeta carries the common metadata fields every API resource has.
// Modeled after Kubernetes ObjectMeta but stripped to what Falak needs.
type ObjectMeta struct {
	Name      string
	ID        string
	Cluster   string
	Labels    map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Pagination holds cursor-based pagination fields for List requests.
type Pagination struct {
	PageSize      int32
	NextPageToken string
}

// PagedResult wraps a list response with pagination metadata.
type PagedResult struct {
	NextPageToken string
	TotalCount    int32
}

// WatchEventType categorizes a watch event.
type WatchEventType string

const (
	watchEventAdded    WatchEventType = "ADDED"
	watchEventModified WatchEventType = "MODIFIED"
	watchEventDeleted  WatchEventType = "DELETED"
	watchEventError    WatchEventType = "ERROR"
)

type watchEventTypeEnum struct{}

// WatchEventTypeEnum provides access to WatchEventType values.
var WatchEventTypeEnum watchEventTypeEnum

func (watchEventTypeEnum) Added() WatchEventType    { return watchEventAdded }
func (watchEventTypeEnum) Modified() WatchEventType  { return watchEventModified }
func (watchEventTypeEnum) Deleted() WatchEventType   { return watchEventDeleted }
func (watchEventTypeEnum) Error() WatchEventType     { return watchEventError }

// WatchEvent is a single event in a Watch stream.
type WatchEvent struct {
	Type      WatchEventType
	Resource  interface{}
	Timestamp time.Time
}

// --- Capsule request/response types --------------------------------------

// CreateCapsuleRequest is the input for Core.CreateCapsule.
type CreateCapsuleRequest struct {
	Cluster string
	Name    string
	Image   string
	Orbit   string
	Tier    string
	Labels  map[string]string

	// Resources
	CPUCores    int32
	CPUCoresMax int32
	MemoryMB    int64
	MemoryMBMax int64
	DiskMB      int64

	// Replicas
	ReplicasMin   int32
	ReplicasMax   int32
	ReplicasExact int32

	// Runtime
	Env          map[string]string
	Command      []string
	NetworkMode  string
	ImageDigest  string

	// Registry (plain text — Core encrypts before storing)
	RegistryURL      string
	RegistryUsername string
	RegistryPassword string

	// Network ports — container/host mappings sourced from CUE
	// runtime.network.ports. host=0 lets the runtime pick a free port.
	Ports []PortMapping
}

// PortMapping describes a single container-to-host port binding. Mirrors
// capsule.PortMapping at the API layer so CreateCapsule can carry the
// runtime.network.ports section of a CUE file end-to-end.
type PortMapping struct {
	Name      string
	Container int32
	Host      int32
	Protocol  string
}

// GetCapsuleRequest is the input for Core.GetCapsule.
type GetCapsuleRequest struct {
	ID      string
	Cluster string
}

// ListCapsulesRequest is the input for Core.ListCapsules.
type ListCapsulesRequest struct {
	Cluster    string
	Labels     map[string]string
	Orbit      string
	Pagination Pagination
}

// ListCapsulesResponse is the output of Core.ListCapsules.
type ListCapsulesResponse struct {
	Capsules []CapsuleResource
	Paging   PagedResult
}

// DeleteCapsuleRequest is the input for Core.DeleteCapsule.
type DeleteCapsuleRequest struct {
	ID      string
	Cluster string
}

// UpdateCapsuleRequest is the input for Core.UpdateCapsule.
type UpdateCapsuleRequest struct {
	ID      string
	Cluster string
	Image   string
	Env     map[string]string
	Command []string
}

// CapsuleResource is the API representation of a capsule (meta + spec + status).
type CapsuleResource struct {
	Meta   ObjectMeta
	Spec   CapsuleSpecView
	Status CapsuleStatusView
}

// CapsuleSpecView is the read-only view of a capsule spec in API responses.
type CapsuleSpecView struct {
	Image       string
	ImageDigest string
	Orbit       string
	Tier        string
	CPUCores    int32
	MemoryMB    int64
	DiskMB      int64
	ReplicasMin int32
	ReplicasMax int32
	Env         map[string]string
	Command     []string
	NetworkMode string
	Ports       []PortMapping
}

// CapsuleStatusView is the read-only view of capsule status in API responses.
type CapsuleStatusView struct {
	Status   string
	Replicas []ReplicaView
}

// ReplicaView is the read-only view of a single replica.
type ReplicaView struct {
	ReplicaID string
	NodeID    string
	Status    string
	StartedAt time.Time
}

// --- Cluster request/response types --------------------------------------

// JoinClusterRequest is the input for Core.JoinCluster.
type JoinClusterRequest struct {
	Path           string
	PSK            string
	BootstrapPeers []string
}

// ListClustersResponse is the output of Core.ListClusters.
type ListClustersResponse struct {
	Clusters []ClusterResource
}

// ClusterResource is the API representation of a joined cluster.
type ClusterResource struct {
	Path     string
	JoinedAt time.Time
	Members  int
}

// --- Node request/response types -----------------------------------------

// ListNodesRequest is the input for Core.ListNodes.
type ListNodesRequest struct {
	Cluster    string
	Pagination Pagination
}

// ListNodesResponse is the output of Core.ListNodes.
type ListNodesResponse struct {
	Nodes  []NodeResource
	Paging PagedResult
}

// NodeResource is the API representation of a node.
type NodeResource struct {
	Meta   ObjectMeta
	Status NodeStatusView
}

// NodeStatusView is the read-only view of node status.
type NodeStatusView struct {
	Status     string
	Addresses  []string
	CPUCores   int32
	MemoryMB   int64
	Datacenter string
	Region     string

	// SWIM-derived live health fields. Populated from the phonebook
	// entry's LastProbeTime/LastProbeSuccess/ReliabilityScore columns
	// (which the health monitor's probeResultPersistLoop updates on
	// every probe). Powers `falak node health`.
	LastProbeTime    time.Time
	LastProbeSuccess bool
	ReliabilityScore float64
}

// --- System types --------------------------------------------------------

// SystemInfo is the output of Core.GetInfo.
type SystemInfo struct {
	NodeID     string
	NodeName   string
	Version    string
	GoVersion  string
	Platform   string
	Uptime     time.Duration
	Clusters   []string
	APIAddress string
}
