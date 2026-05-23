// Package capsule provides the core capsule entity, types, and lifecycle management
// for Falak's decentralized container execution platform.
package capsule

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	enums "github.com/tareksalem/falak/capsule/enums"
)

// CapsuleID uniquely identifies a capsule in the mesh.
type CapsuleID string

// NewCapsuleID generates a new random CapsuleID.
func NewCapsuleID() CapsuleID {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("failed to generate capsule ID: %v", err))
	}
	return CapsuleID(hex.EncodeToString(b))
}

// String returns the string representation of the CapsuleID.
func (id CapsuleID) String() string {
	return string(id)
}

// --- Core Types ---

// Labels is a key-value map used on all entities (nodes, clusters, datacenters, capsules).
type Labels map[string]string

// ResourceRequirements defines the resource constraints for a capsule.
// Reservation fields (CPUCores, MemoryMB, DiskMB) are the guaranteed
// minimum used by gravity for scoring. Max fields are hard ceilings —
// container is throttled (CPU) or OOM-killed (memory) if exceeded.
// When a Max field is zero, it defaults to the reservation (no burst).
type ResourceRequirements struct {
	CPUCores    int32
	CPUCoresMax int32 // hard CPU limit; 0 = same as CPUCores
	MemoryMB    int64
	MemoryMBMax int64 // hard memory limit; 0 = same as MemoryMB
	DiskMB      int64
}

// EffectiveCPUMax returns the hard CPU limit, defaulting to the
// reservation when no explicit max is set.
func (r ResourceRequirements) EffectiveCPUMax() int32 {
	if r.CPUCoresMax > 0 {
		return r.CPUCoresMax
	}
	return r.CPUCores
}

// EffectiveMemoryMax returns the hard memory limit, defaulting to the
// reservation when no explicit max is set.
func (r ResourceRequirements) EffectiveMemoryMax() int64 {
	if r.MemoryMBMax > 0 {
		return r.MemoryMBMax
	}
	return r.MemoryMB
}

// ReplicaConfig defines how many replicas to run.
type ReplicaConfig struct {
	Min   int32
	Max   int32
	Exact int32 // 0 means use Min/Max autoscaling
}

// ScalingRule defines a named group of conditions that trigger a scaling action.
type ScalingRule struct {
	Name       string
	Trigger    enums.TriggerMode
	Conditions []string
	Action     enums.ScalingAction
	Cooldown   time.Duration
}

// PlacementRule defines where a capsule should be deployed.
type PlacementRule struct {
	Name     string
	Type     enums.PlacementType
	Mode     enums.PlacementMode // only for Type=capsule
	Names    []string            // match by entity name(s)
	Labels   Labels              // key-value labels; "same" keyword for capsule comparison
	Required bool                // default true; false = soft/preferred
}

// --- Network config ---

// PortMapping maps a container port to a host port.
type PortMapping struct {
	Name          string // human-readable label (e.g. "http", "grpc")
	ContainerPort uint16
	HostPort      uint16 // 0 = auto-assign
	Protocol      string // "tcp" (default) or "udp"
}

// NetworkConfig defines the capsule's network isolation and port exposure.
type NetworkConfig struct {
	Mode  enums.NetworkMode // "bridge" (default) or "host"
	Ports []PortMapping     // port mappings (bridge mode only)
}

// --- Health check config ---

// HealthCheck defines how the runtime probes container liveness.
type HealthCheck struct {
	Type         enums.HealthCheckType
	Path         string // HTTP only (e.g. "/health")
	Port         uint16
	Interval     time.Duration // time between probes (default 10s)
	Timeout      time.Duration // max wait per probe (default 3s)
	Retries      int           // consecutive failures before unhealthy (default 3)
	InitialDelay time.Duration // grace period before first probe (default 5s)
}

// --- Registry credentials ---

// RegistryAuth holds encrypted credentials for pulling from a private
// registry. Values are encrypted with the cluster's SEK before being
// gossiped in the capsule spec.
type RegistryAuth struct {
	URL               string // e.g. "ghcr.io"
	UsernameEncrypted []byte // encrypted with SEK
	PasswordEncrypted []byte // encrypted with SEK
}

// --- Failure policy ---

// FailurePolicy controls how the runtime handles container crashes and
// health check failures.
type FailurePolicy struct {
	RestartLimit    int           // max local restarts before re-election (default 3)
	MaxNodeAttempts int           // max different nodes to try (default 3)
	GracefulTimeout time.Duration // SIGTERM → SIGKILL grace period (default 10s)
}

// --- Log retention ---

// LogRetention configures how container stdout/stderr logs are stored.
type LogRetention struct {
	MaxFileSizeMB int // max size of a single log file (default 10)
	MaxFiles      int // max rotated files kept (default 10)
}

// --- Snapshot config ---

// SnapshotConfig configures per-capsule snapshot behavior.
type SnapshotConfig struct {
	MaxPerCapsule int           // max snapshot tags kept (default 3)
	TTL           time.Duration // snapshot expiry (default 72h)
}

// --- Runtime config ---

// RuntimeConfig defines how the capsule container should run.
type RuntimeConfig struct {
	Env            map[string]string // plain environment variables
	Network        NetworkConfig
	HealthCheck    *HealthCheck // nil = no health check
	FailurePolicy  FailurePolicy
	LogRetention   LogRetention
	StatsInterval  time.Duration // per-capsule stats interval (default 5s)
	Registry       *RegistryAuth // nil = public image
	SnapshotConfig SnapshotConfig
}

// MomentumConfig provides advanced momentum tuning.
type MomentumConfig struct {
	Base           int32
	BoostOnTraffic bool
	ReduceOnIdle   bool
	IdleTimeout    time.Duration
}

// MomentumState tracks the current momentum of a capsule.
type MomentumState struct {
	Current      int32
	Base         int32
	LastAdjusted time.Time
}

// ReplicaID uniquely identifies a replica of a capsule.
type ReplicaID string

// ReplicaState tracks the state of a single running replica.
type ReplicaState struct {
	ReplicaID ReplicaID
	NodeID    string
	Status    enums.CapsuleStatus
	StartedAt time.Time
}

// CapsuleSpec is the user-provided specification for a capsule.
type CapsuleSpec struct {
	// Identity
	Name        string
	Image       string
	ImageAlias  string // original tag the user provided (e.g. "myapp:latest")
	ImageDigest string // resolved digest at creation time (e.g. "sha256:abc...")
	Orbit       string
	Tier        enums.Tier
	Labels      Labels

	// Resources
	Resources      ResourceRequirements
	Replicas       ReplicaConfig
	ScalingRules   []ScalingRule
	PlacementRules []PlacementRule

	// Runtime
	Runtime        RuntimeConfig
	MomentumConfig MomentumConfig

	// Command overrides the container entrypoint. Empty = use image default.
	Command []string

	// Group-related fields (Phase 10).

	// Kind discriminates standalone capsules from group-kind capsules.
	// Default is Capsule (applied by DefaultSpec).
	Kind CapsuleKind

	// GroupID is set on member capsules to the ID of their parent group.
	// Empty on standalone capsules and on group capsules themselves.
	GroupID CapsuleID

	// GroupMember is true on member capsules and false otherwise.
	GroupMember bool

	// Group is the group sub-spec, populated only when Kind == Group.
	Group *GroupSpec
}

// Capsule is the full capsule entity as it exists in the mesh.
type Capsule struct {
	ID        CapsuleID
	ClusterID string
	Spec      CapsuleSpec
	Status    enums.CapsuleStatus
	Replicas  []ReplicaState
	Momentum  MomentumState
	Version   string
	CreatedAt time.Time
	UpdatedAt time.Time
}
