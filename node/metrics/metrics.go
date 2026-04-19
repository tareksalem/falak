// Package metrics samples local host resource utilization, persists a
// rolling window for the local node, gossips updates to peers via the
// existing health PubSub topic, and exposes a StateProvider adapter for
// the election gravity calculator.
//
// The package owns three concerns:
//
//  1. Sampling. A periodic loop reads CPU, memory, disk, load average, and
//     network counters via gopsutil and emits a Snapshot.
//  2. Storage. A SQLite store keeps the last N local snapshots plus a
//     single latest snapshot per peer node.
//  3. Gossip. Each fresh local sample is signed and published as a
//     ResourceUpdate message on the cluster's health topic; subscribers
//     update their peer_latest rows on receive.
//
// metrics is a node-level concern: it observes the host, not capsules.
// Per-container metrics belong to the runtime module and are scoped
// independently.
package metrics

import "time"

// Snapshot is one observation of the local host's resource utilization.
// All fields are filled by a single pass through the collector; partial
// snapshots are never produced (collection failures return an error
// instead of a half-filled value).
//
// Memory and disk are reported in megabytes to keep arithmetic in int64
// without overflow on petabyte-class hosts. Network throughput is in
// bytes per second computed across the sampling interval.
type Snapshot struct {
	// NodeID is the libp2p peer ID of the node that produced the snapshot.
	// Set by the collector at sample time and preserved through gossip.
	NodeID string

	// CapturedAt is when the snapshot was taken. Used to detect stale
	// data and to compute network throughput between two consecutive
	// samples.
	CapturedAt time.Time

	// CPU describes processor capacity and current load.
	CPU CPUStats

	// Memory describes RAM capacity and pressure.
	Memory MemoryStats

	// Disk describes the primary storage capacity. The collector
	// aggregates all writable mounts into a single primary value;
	// per-mount details are not gossiped in v1.
	Disk DiskStats

	// Load describes the kernel's load averages over 1, 5, and 15 minutes.
	// On non-Linux platforms these may be zero — gopsutil reports the
	// equivalent metric where available and 0 elsewhere.
	Load LoadStats

	// Network describes aggregate throughput across all interfaces in
	// bytes per second since the previous sample.
	Network NetworkStats
}

// CPUStats describes CPU capacity and current utilization.
type CPUStats struct {
	// Cores is the number of logical CPU cores on the host.
	Cores int32

	// UsedPercent is the aggregate utilization across all cores in [0, 100].
	UsedPercent float64
}

// MemoryStats describes RAM capacity and pressure in megabytes.
type MemoryStats struct {
	TotalMB      int64
	AvailableMB  int64
	UsedMB       int64
	UsedPercent  float64
}

// DiskStats describes a single aggregated disk volume in megabytes.
// In v1 the collector reports the root filesystem (or its OS equivalent)
// as the primary disk; multi-volume reporting is deferred until the
// runtime module needs it for snapshot placement.
type DiskStats struct {
	TotalMB      int64
	FreeMB       int64
	UsedMB       int64
	UsedPercent  float64
}

// LoadStats describes the kernel load averages.
type LoadStats struct {
	One     float64
	Five    float64
	Fifteen float64
}

// NetworkStats describes aggregated network throughput in bytes per
// second across all non-loopback interfaces. Computed by the collector
// as the delta between two consecutive samples divided by the elapsed
// duration.
type NetworkStats struct {
	BytesSentPerSec int64
	BytesRecvPerSec int64
}
