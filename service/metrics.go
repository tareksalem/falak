package service

// MetricsSink receives counters from the manager. The current sink is a
// stub so callers can wire a real exporter later without breaking the API.
type MetricsSink interface {
	IncCreated(clusterID string)
	IncUpdated(clusterID string)
	IncDeleted(clusterID string)
	IncReceived(clusterID string)
	IncBackendResolved(clusterID string)
	IncBackendUnresolved(clusterID, reason string)
}

// NoopMetrics is the default no-op MetricsSink.
type NoopMetrics struct{}

// IncCreated satisfies MetricsSink.
func (NoopMetrics) IncCreated(string) {}

// IncUpdated satisfies MetricsSink.
func (NoopMetrics) IncUpdated(string) {}

// IncDeleted satisfies MetricsSink.
func (NoopMetrics) IncDeleted(string) {}

// IncReceived satisfies MetricsSink.
func (NoopMetrics) IncReceived(string) {}

// IncBackendResolved satisfies MetricsSink.
func (NoopMetrics) IncBackendResolved(string) {}

// IncBackendUnresolved satisfies MetricsSink.
func (NoopMetrics) IncBackendUnresolved(string, string) {}
