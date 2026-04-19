// Package capsule provides the core capsule entity, types, and lifecycle management
// for Falak's decentralized container execution platform.
package capsule

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
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

// --- CapsuleStatus Enum ---

// CapsuleStatus represents the lifecycle state of a capsule.
type CapsuleStatus string

const (
	capsuleStatusCreated   CapsuleStatus = "created"
	capsuleStatusAnnounced CapsuleStatus = "announced"
	capsuleStatusElecting  CapsuleStatus = "electing"
	capsuleStatusAssigned  CapsuleStatus = "assigned"
	capsuleStatusExecuting CapsuleStatus = "executing"
	capsuleStatusRunning   CapsuleStatus = "running"
	capsuleStatusStopping  CapsuleStatus = "stopping"
	capsuleStatusStopped   CapsuleStatus = "stopped"
)

type capsuleStatusEnum struct{}

// CapsuleStatusEnum provides access to CapsuleStatus values.
var CapsuleStatusEnum capsuleStatusEnum

func (capsuleStatusEnum) Created() CapsuleStatus   { return capsuleStatusCreated }
func (capsuleStatusEnum) Announced() CapsuleStatus  { return capsuleStatusAnnounced }
func (capsuleStatusEnum) Electing() CapsuleStatus   { return capsuleStatusElecting }
func (capsuleStatusEnum) Assigned() CapsuleStatus   { return capsuleStatusAssigned }
func (capsuleStatusEnum) Executing() CapsuleStatus  { return capsuleStatusExecuting }
func (capsuleStatusEnum) Running() CapsuleStatus    { return capsuleStatusRunning }
func (capsuleStatusEnum) Stopping() CapsuleStatus   { return capsuleStatusStopping }
func (capsuleStatusEnum) Stopped() CapsuleStatus    { return capsuleStatusStopped }

// Valid returns true if the status is a known CapsuleStatus value.
func (s CapsuleStatus) Valid() bool {
	switch s {
	case capsuleStatusCreated, capsuleStatusAnnounced, capsuleStatusElecting,
		capsuleStatusAssigned, capsuleStatusExecuting, capsuleStatusRunning,
		capsuleStatusStopping, capsuleStatusStopped:
		return true
	}
	return false
}

// --- Tier Enum ---

// Tier represents the priority tier of a capsule.
type Tier string

const (
	tierCritical   Tier = "critical"
	tierStandard   Tier = "standard"
	tierBackground Tier = "background"
)

type tierEnum struct{}

// TierEnum provides access to Tier values.
var TierEnum tierEnum

func (tierEnum) Critical() Tier   { return tierCritical }
func (tierEnum) Standard() Tier   { return tierStandard }
func (tierEnum) Background() Tier { return tierBackground }

// BaseMomentum returns the default momentum value for the tier.
func (t Tier) BaseMomentum() int32 {
	switch t {
	case tierCritical:
		return 90
	case tierStandard:
		return 50
	case tierBackground:
		return 20
	default:
		return 50
	}
}

// Valid returns true if the tier is a known Tier value.
func (t Tier) Valid() bool {
	switch t {
	case tierCritical, tierStandard, tierBackground:
		return true
	}
	return false
}

// --- ScalingAction Enum ---

// ScalingAction represents what a scaling rule triggers.
type ScalingAction string

const (
	scalingActionScaleUp     ScalingAction = "scaleUp"
	scalingActionScaleDown   ScalingAction = "scaleDown"
	scalingActionScaleToZero ScalingAction = "scaleToZero"
)

type scalingActionEnum struct{}

// ScalingActionEnum provides access to ScalingAction values.
var ScalingActionEnum scalingActionEnum

func (scalingActionEnum) ScaleUp() ScalingAction     { return scalingActionScaleUp }
func (scalingActionEnum) ScaleDown() ScalingAction   { return scalingActionScaleDown }
func (scalingActionEnum) ScaleToZero() ScalingAction { return scalingActionScaleToZero }

// Valid returns true if the action is a known ScalingAction value.
func (a ScalingAction) Valid() bool {
	switch a {
	case scalingActionScaleUp, scalingActionScaleDown, scalingActionScaleToZero:
		return true
	}
	return false
}

// --- TriggerMode Enum ---

// TriggerMode determines how scaling conditions are evaluated.
type TriggerMode string

const (
	triggerModeAny TriggerMode = "any"
	triggerModeAll TriggerMode = "all"
)

type triggerModeEnum struct{}

// TriggerModeEnum provides access to TriggerMode values.
var TriggerModeEnum triggerModeEnum

func (triggerModeEnum) Any() TriggerMode { return triggerModeAny }
func (triggerModeEnum) All() TriggerMode { return triggerModeAll }

// Valid returns true if the mode is a known TriggerMode value.
func (m TriggerMode) Valid() bool {
	switch m {
	case triggerModeAny, triggerModeAll:
		return true
	}
	return false
}

// --- PlacementType Enum ---

// PlacementType identifies the entity type in a placement rule.
type PlacementType string

const (
	placementTypeNode       PlacementType = "node"
	placementTypeCluster    PlacementType = "cluster"
	placementTypeDatacenter PlacementType = "datacenter"
	placementTypeCapsule    PlacementType = "capsule"
)

type placementTypeEnum struct{}

// PlacementTypeEnum provides access to PlacementType values.
var PlacementTypeEnum placementTypeEnum

func (placementTypeEnum) Node() PlacementType       { return placementTypeNode }
func (placementTypeEnum) Cluster() PlacementType    { return placementTypeCluster }
func (placementTypeEnum) Datacenter() PlacementType { return placementTypeDatacenter }
func (placementTypeEnum) Capsule() PlacementType    { return placementTypeCapsule }

// Valid returns true if the type is a known PlacementType value.
func (t PlacementType) Valid() bool {
	switch t {
	case placementTypeNode, placementTypeCluster, placementTypeDatacenter, placementTypeCapsule:
		return true
	}
	return false
}

// --- PlacementMode Enum ---

// PlacementMode determines affinity direction for capsule placement rules.
type PlacementMode string

const (
	placementModeNear PlacementMode = "near"
	placementModeAway PlacementMode = "away"
)

type placementModeEnum struct{}

// PlacementModeEnum provides access to PlacementMode values.
var PlacementModeEnum placementModeEnum

func (placementModeEnum) Near() PlacementMode { return placementModeNear }
func (placementModeEnum) Away() PlacementMode { return placementModeAway }

// Valid returns true if the mode is a known PlacementMode value.
func (m PlacementMode) Valid() bool {
	switch m {
	case placementModeNear, placementModeAway:
		return true
	}
	return false
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
	Trigger    TriggerMode
	Conditions []string
	Action     ScalingAction
	Cooldown   time.Duration
}

// PlacementRule defines where a capsule should be deployed.
type PlacementRule struct {
	Name     string
	Type     PlacementType
	Mode     PlacementMode // only for Type=capsule
	Names    []string      // match by entity name(s)
	Labels   Labels        // key-value labels; "same" keyword for capsule comparison
	Required bool          // default true; false = soft/preferred
}

// --- Network config ---

// NetworkMode selects the container's network isolation model.
type NetworkMode string

const (
	networkModeBridge NetworkMode = "bridge"
	networkModeHost   NetworkMode = "host"
)

type networkModeEnum struct{}

// NetworkModeEnum provides access to NetworkMode values.
var NetworkModeEnum networkModeEnum

func (networkModeEnum) Bridge() NetworkMode { return networkModeBridge }
func (networkModeEnum) Host() NetworkMode   { return networkModeHost }

// PortMapping maps a container port to a host port.
type PortMapping struct {
	Name          string // human-readable label (e.g. "http", "grpc")
	ContainerPort uint16
	HostPort      uint16 // 0 = auto-assign
	Protocol      string // "tcp" (default) or "udp"
}

// NetworkConfig defines the capsule's network isolation and port exposure.
type NetworkConfig struct {
	Mode  NetworkMode   // "bridge" (default) or "host"
	Ports []PortMapping // port mappings (bridge mode only)
}

// --- Health check config ---

// HealthCheckType selects the probe mechanism.
type HealthCheckType string

const (
	healthCheckHTTP HealthCheckType = "http"
	healthCheckTCP  HealthCheckType = "tcp"
)

type healthCheckTypeEnum struct{}

// HealthCheckTypeEnum provides access to HealthCheckType values.
var HealthCheckTypeEnum healthCheckTypeEnum

func (healthCheckTypeEnum) HTTP() HealthCheckType { return healthCheckHTTP }
func (healthCheckTypeEnum) TCP() HealthCheckType  { return healthCheckTCP }

// HealthCheck defines how the runtime probes container liveness.
type HealthCheck struct {
	Type         HealthCheckType
	Path         string        // HTTP only (e.g. "/health")
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
	URL              string // e.g. "ghcr.io"
	UsernameEncrypted []byte // encrypted with SEK
	PasswordEncrypted []byte // encrypted with SEK
}

// --- Failure policy ---

// FailurePolicy controls how the runtime handles container crashes and
// health check failures.
type FailurePolicy struct {
	RestartLimit     int // max local restarts before re-election (default 3)
	MaxNodeAttempts  int // max different nodes to try (default 3)
	GracefulTimeout  time.Duration // SIGTERM → SIGKILL grace period (default 10s)
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
	Env             map[string]string // plain environment variables
	Network         NetworkConfig
	HealthCheck     *HealthCheck      // nil = no health check
	FailurePolicy   FailurePolicy
	LogRetention    LogRetention
	StatsInterval   time.Duration     // per-capsule stats interval (default 5s)
	Registry        *RegistryAuth     // nil = public image
	SnapshotConfig  SnapshotConfig
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
	Status    CapsuleStatus
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
	Tier        Tier
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
}

// Capsule is the full capsule entity as it exists in the mesh.
type Capsule struct {
	ID        CapsuleID
	ClusterID string
	Spec      CapsuleSpec
	Status    CapsuleStatus
	Replicas  []ReplicaState
	Momentum  MomentumState
	Version   string
	CreatedAt time.Time
	UpdatedAt time.Time
}
