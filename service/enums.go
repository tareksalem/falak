package service

// Visibility scopes who may resolve and connect to a Service.
// `group` admits only callers in the same group as the backends;
// `cluster` admits any caller in the cluster; `external` is reserved
// (out of v1 scope) and rejected at validation.
type Visibility string

const (
	visibilityGroup    Visibility = "group"
	visibilityCluster  Visibility = "cluster"
	visibilityExternal Visibility = "external"
)

type visibilityEnum struct{}

// VisibilityEnum exposes typed Visibility values.
var VisibilityEnum visibilityEnum

// Group returns the group-scoped visibility.
func (visibilityEnum) Group() Visibility { return visibilityGroup }

// Cluster returns the cluster-scoped visibility (v1 default).
func (visibilityEnum) Cluster() Visibility { return visibilityCluster }

// External returns the reserved external-scoped visibility.
func (visibilityEnum) External() Visibility { return visibilityExternal }

// Valid reports whether v is any known Visibility value.
func (v Visibility) Valid() bool {
	switch v {
	case visibilityGroup, visibilityCluster, visibilityExternal:
		return true
	}
	return false
}

// IsAdmitted reports whether v is accepted by the v1 admission path.
func (v Visibility) IsAdmitted() bool {
	return v == visibilityGroup || v == visibilityCluster
}

// StrategyType selects the traffic-management strategy applied to a Service.
type StrategyType string

const (
	strategyTypeStatic    StrategyType = "static"
	strategyTypeBlueGreen StrategyType = "blue-green"
	strategyTypeCanary    StrategyType = "canary"
)

type strategyTypeEnum struct{}

// StrategyTypeEnum exposes typed StrategyType values.
var StrategyTypeEnum strategyTypeEnum

// Static returns the static-weighted strategy type.
func (strategyTypeEnum) Static() StrategyType { return strategyTypeStatic }

// BlueGreen returns the blue-green strategy type.
func (strategyTypeEnum) BlueGreen() StrategyType { return strategyTypeBlueGreen }

// Canary returns the canary strategy type.
func (strategyTypeEnum) Canary() StrategyType { return strategyTypeCanary }

// Valid reports whether s is a known StrategyType.
func (s StrategyType) Valid() bool {
	switch s {
	case strategyTypeStatic, strategyTypeBlueGreen, strategyTypeCanary:
		return true
	}
	return false
}

// Protocol is the transport protocol of a ServicePort. v1 admits TCP
// and UDP; HTTP is reserved for the L7 follow-up.
type Protocol string

const (
	protocolTCP  Protocol = "tcp"
	protocolUDP  Protocol = "udp"
	protocolHTTP Protocol = "http"
)

type protocolEnum struct{}

// ProtocolEnum exposes typed Protocol values.
var ProtocolEnum protocolEnum

// TCP returns the TCP transport protocol.
func (protocolEnum) TCP() Protocol { return protocolTCP }

// UDP returns the UDP transport protocol.
func (protocolEnum) UDP() Protocol { return protocolUDP }

// HTTP returns the reserved HTTP protocol.
func (protocolEnum) HTTP() Protocol { return protocolHTTP }

// Valid reports whether p is any known Protocol value.
func (p Protocol) Valid() bool {
	switch p {
	case protocolTCP, protocolUDP, protocolHTTP:
		return true
	}
	return false
}

// IsAdmitted reports whether p is accepted at admission in v1.
func (p Protocol) IsAdmitted() bool {
	return p == protocolTCP || p == protocolUDP
}

// BackendResolution describes the live resolution state of a backend.
type BackendResolution string

const (
	backendResolutionResolved                 BackendResolution = "resolved"
	backendResolutionUnresolved               BackendResolution = "unresolved"
	backendResolutionUnresolvedCapsuleDeleted BackendResolution = "unresolved_capsule_deleted"
	backendResolutionUnresolvedIdentityChange BackendResolution = "unresolved_identity_changed"
)

type backendResolutionEnum struct{}

// BackendResolutionEnum exposes typed BackendResolution values.
var BackendResolutionEnum backendResolutionEnum

// Resolved returns the resolved state.
func (backendResolutionEnum) Resolved() BackendResolution { return backendResolutionResolved }

// Unresolved returns the never-resolved state.
func (backendResolutionEnum) Unresolved() BackendResolution { return backendResolutionUnresolved }

// UnresolvedCapsuleDeleted returns the post-delete unresolved state.
func (backendResolutionEnum) UnresolvedCapsuleDeleted() BackendResolution {
	return backendResolutionUnresolvedCapsuleDeleted
}

// UnresolvedIdentityChanged returns the identity-changed unresolved state.
func (backendResolutionEnum) UnresolvedIdentityChanged() BackendResolution {
	return backendResolutionUnresolvedIdentityChange
}

// Valid reports whether r is a known BackendResolution value.
func (r BackendResolution) Valid() bool {
	switch r {
	case backendResolutionResolved, backendResolutionUnresolved,
		backendResolutionUnresolvedCapsuleDeleted, backendResolutionUnresolvedIdentityChange:
		return true
	}
	return false
}

// ServiceStatus is the lifecycle state of a Service.
type ServiceStatus string

const (
	serviceStatusCreated         ServiceStatus = "created"
	serviceStatusActive          ServiceStatus = "active"
	serviceStatusDraining        ServiceStatus = "draining"
	serviceStatusDeleted         ServiceStatus = "deleted"
	serviceStatusCanaryAborted   ServiceStatus = "canary_aborted"
	serviceStatusPlacementFailed ServiceStatus = "placement_failed"
)

type serviceStatusEnum struct{}

// ServiceStatusEnum exposes typed ServiceStatus values.
var ServiceStatusEnum serviceStatusEnum

// Created returns the freshly-created state.
func (serviceStatusEnum) Created() ServiceStatus { return serviceStatusCreated }

// Active returns the actively-routing state.
func (serviceStatusEnum) Active() ServiceStatus { return serviceStatusActive }

// Draining returns the graceful-shutdown state.
func (serviceStatusEnum) Draining() ServiceStatus { return serviceStatusDraining }

// Deleted returns the terminal removed state.
func (serviceStatusEnum) Deleted() ServiceStatus { return serviceStatusDeleted }

// CanaryAborted returns the canary-rollback terminal state.
func (serviceStatusEnum) CanaryAborted() ServiceStatus { return serviceStatusCanaryAborted }

// PlacementFailed returns the reserved strict-mode placement-failure state.
func (serviceStatusEnum) PlacementFailed() ServiceStatus { return serviceStatusPlacementFailed }

// Valid reports whether s is a known ServiceStatus value.
func (s ServiceStatus) Valid() bool {
	switch s {
	case serviceStatusCreated, serviceStatusActive, serviceStatusDraining,
		serviceStatusDeleted, serviceStatusCanaryAborted, serviceStatusPlacementFailed:
		return true
	}
	return false
}
