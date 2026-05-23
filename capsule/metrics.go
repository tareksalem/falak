package capsule

import (
	"sync/atomic"

	enums "github.com/tareksalem/falak/capsule/enums"
)

// Metrics defines observability hooks for the capsule subsystem. Implementations
// can plug these into Prometheus, OpenTelemetry, StatsD, or any other backend.
//
// All methods must be safe for concurrent use. Implementations should be fast
// and non-blocking — these are called from hot paths like announcement receive.
//
// Use NoopMetrics (the default) if observability is not needed; it has zero
// allocation and zero cost.
type Metrics interface {
	// IncCreated increments the counter of locally-created capsules.
	IncCreated(clusterID string)

	// IncReceived increments the counter of capsules received from the mesh.
	IncReceived(clusterID string)

	// IncAnnounced increments the counter of announce publishes.
	IncAnnounced(clusterID, orbit string)

	// IncWithdrawn increments the counter of withdrawal publishes.
	IncWithdrawn(clusterID, reason string)

	// IncStatusTransition records a lifecycle transition (from → to).
	IncStatusTransition(from, to enums.CapsuleStatus, trigger string)

	// IncScalingTriggered records a scaling rule firing.
	IncScalingTriggered(action enums.ScalingAction, rule string)

	// ObserveMomentum records the current momentum value for a capsule.
	ObserveMomentum(capsuleID CapsuleID, value int32)
}

// NoopMetrics is the default Metrics implementation; all calls are no-ops.
type NoopMetrics struct{}

func (NoopMetrics) IncCreated(string)                                                    {}
func (NoopMetrics) IncReceived(string)                                                   {}
func (NoopMetrics) IncAnnounced(string, string)                                          {}
func (NoopMetrics) IncWithdrawn(string, string)                                          {}
func (NoopMetrics) IncStatusTransition(enums.CapsuleStatus, enums.CapsuleStatus, string) {}
func (NoopMetrics) IncScalingTriggered(enums.ScalingAction, string)                      {}
func (NoopMetrics) ObserveMomentum(CapsuleID, int32)                                     {}

// CountingMetrics is a simple in-memory Metrics implementation useful for
// tests and debug UIs. It tracks totals using atomic counters, with no tags.
type CountingMetrics struct {
	Created       atomic.Int64
	Received      atomic.Int64
	Announced     atomic.Int64
	Withdrawn     atomic.Int64
	Transitions   atomic.Int64
	ScalingEvents atomic.Int64
}

// NewCountingMetrics returns a new CountingMetrics.
func NewCountingMetrics() *CountingMetrics { return &CountingMetrics{} }

func (c *CountingMetrics) IncCreated(string)           { c.Created.Add(1) }
func (c *CountingMetrics) IncReceived(string)          { c.Received.Add(1) }
func (c *CountingMetrics) IncAnnounced(string, string) { c.Announced.Add(1) }
func (c *CountingMetrics) IncWithdrawn(string, string) { c.Withdrawn.Add(1) }
func (c *CountingMetrics) IncStatusTransition(enums.CapsuleStatus, enums.CapsuleStatus, string) {
	c.Transitions.Add(1)
}
func (c *CountingMetrics) IncScalingTriggered(enums.ScalingAction, string) { c.ScalingEvents.Add(1) }
func (c *CountingMetrics) ObserveMomentum(CapsuleID, int32)                {}
