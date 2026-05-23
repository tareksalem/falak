package enums

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
func (capsuleStatusEnum) Announced() CapsuleStatus { return capsuleStatusAnnounced }
func (capsuleStatusEnum) Electing() CapsuleStatus  { return capsuleStatusElecting }
func (capsuleStatusEnum) Assigned() CapsuleStatus  { return capsuleStatusAssigned }
func (capsuleStatusEnum) Executing() CapsuleStatus { return capsuleStatusExecuting }
func (capsuleStatusEnum) Running() CapsuleStatus   { return capsuleStatusRunning }
func (capsuleStatusEnum) Stopping() CapsuleStatus  { return capsuleStatusStopping }
func (capsuleStatusEnum) Stopped() CapsuleStatus   { return capsuleStatusStopped }

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
