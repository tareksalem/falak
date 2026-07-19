// Package service provides the core Service entity, enums, types, and
// lifecycle management for Falak's service-mesh layer.
//
// A Service is a logical name decoupled from any specific capsule. It
// declares exposed ports, a visibility scope, a list of backends — each
// pointing to a capsule by name with a weight — plus an optional
// deployment strategy (static / blue-green / canary). Deleting a
// Service stops routing only; capsules themselves are never touched.
//
// Backends bind by capsule name and capture the resolved capsule ID at
// first resolve; an identity change then requires an explicit rebind.
// Backend resolution is lenient: an unresolved backend is admitted and
// routed to once its capsule appears. Default per-Service timeouts are
// idle 5min, connect 5s.
//
// Enums live alongside in enums.go; spec validation in spec.go; store
// in store.go; lifecycle FSM in lifecycle.go.
package service

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// ServiceID uniquely identifies a Service. A delete + recreate of the
// same logical name produces a new ID.
type ServiceID string

// NewServiceID generates a fresh random ServiceID (16 bytes hex).
func NewServiceID() ServiceID {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand only fails on catastrophic OS failure — panic is
		// the only safe response since ID uniqueness is an invariant.
		panic(fmt.Sprintf("failed to generate service ID: %v", err))
	}
	return ServiceID(hex.EncodeToString(b))
}

// String returns the string form of the ServiceID.
func (id ServiceID) String() string { return string(id) }

// Default timeouts and weights.
const (
	DefaultIdleTimeout    = 5 * time.Minute
	DefaultConnectTimeout = 5 * time.Second
	DefaultBlueGreenDrain = 30 * time.Second
	DefaultBackendWeight  = int32(100)
)

// ServicePort declares one ingress port the Service exposes.
type ServicePort struct {
	Name     string   // DNS-friendly handle for the port (e.g. "http").
	Port     uint16   // TCP/UDP port number callers connect to.
	Protocol Protocol // transport selection; defaults to TCP via DefaultSpec.
}

// ServiceBackend references a capsule by name with a weight and an
// optional port-name remap. CapturedCapsuleID is empty until the
// manager resolves the backend for the first time.
type ServiceBackend struct {
	Capsule           string            // bare capsule name written by the operator.
	CapturedCapsuleID string            // resolved capsule ID, manager-owned post-resolve.
	PortMap           map[string]string // service-port → capsule named-port; empty = identity map.
	Weight            int32             // SWRR weight; 0 excludes the backend.
}

// CanaryStrategy parameterises an in-progress canary rollout. Mode is
// implicit: presence/absence of Interval and SuccessCriteria selects
// auto / gated / manual at the strategy layer.
type CanaryStrategy struct {
	Target          string        // backend traffic moves toward.
	From            string        // backend traffic moves away from.
	Step            int32         // percentage moved per progression tick.
	Interval        time.Duration // wait between progression ticks.
	SuccessCriteria []string      // gated-mode metric expressions.
	AbortOn         []string      // abort-trigger expressions.
}

// BlueGreenStrategy parameterises a blue-green flip.
type BlueGreenStrategy struct {
	Active string        // backend currently receiving traffic.
	Drain  time.Duration // post-flip grace window for in-flight connections.
}

// Strategy is the top-level traffic-management strategy on a Service.
type Strategy struct {
	Type      StrategyType
	Canary    *CanaryStrategy    // populated when Type == Canary.
	BlueGreen *BlueGreenStrategy // populated when Type == BlueGreen.
}

// ServiceTimeouts holds per-Service connection timeouts.
type ServiceTimeouts struct {
	Idle    time.Duration // close after this period of inactivity.
	Connect time.Duration // bound proxy→backend dial time.
}

// ServiceSpec is the operator-authored Service configuration.
type ServiceSpec struct {
	Name       string
	Visibility Visibility
	Group      string // owning group for Visibility == Group.
	Ports      []ServicePort
	Backends   []ServiceBackend
	Strategy   *Strategy
	Timeouts   ServiceTimeouts
}

// BackendState mirrors the live resolution state of a single backend.
// Stored adjacent to the Service so it survives store reload.
type BackendState struct {
	Name              string
	Resolution        BackendResolution
	CapturedCapsuleID string
	LastResolvedAt    time.Time
	// PriorWeight retains the operator-supplied spec weight at the
	// moment the resolution turned inadmissible (e.g.
	// UnresolvedIdentityChanged forces the live spec weight to zero
	// so canary auto-aborts pick it up). Rebind reads it back to
	// restore the original weight on a successful re-resolve.
	// Zero means "no prior weight recorded".
	PriorWeight int32
}

// Service is the in-memory representation of a stored Service.
type Service struct {
	ID            ServiceID
	ClusterID     string
	Spec          ServiceSpec
	Status        ServiceStatus
	Version       string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	BackendStates []BackendState
}
