// Package health provides SWIM-based failure detection for cluster nodes.
//
// score.go manages per-node reliability scores. Scores start at 0 (healthy)
// and increment on failed probes. When a probe succeeds, the score resets to 0.
// Threshold crossings trigger status transitions in the phonebook.
package health

import (
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
)

// Default score configuration values. All are overridable via functional options.
// These are base values — effective values are scaled by cluster size.
const (
	DefaultScoreIncrement      = 0.5
	DefaultSuspectedThreshold  = 4.0  // Base: ~8 failed probes to enter suspected
	DefaultQuarantineThreshold = 10.0 // Base: ~20 failed probes to quarantine
	DefaultQuarantineTimeout   = 30 * time.Second // Base: time in quarantine before marked failed
)

// ClusterSizeScaleBand defines how thresholds scale at a given cluster size range.
type ClusterSizeScaleBand struct {
	MinNodes           int     // Minimum cluster size for this band (inclusive)
	ThresholdScale     float64 // Multiplier for score thresholds (lower = faster detection)
	TimeoutScale       float64 // Multiplier for quarantine timeout
}

// DefaultScaleBands defines the default cluster-size-aware scaling.
// Smaller clusters detect failures faster because there are fewer observers.
var DefaultScaleBands = []ClusterSizeScaleBand{
	{MinNodes: 0, ThresholdScale: 0.3, TimeoutScale: 0.33},  // 1-2 nodes
	{MinNodes: 3, ThresholdScale: 0.5, TimeoutScale: 0.5},   // 3-5 nodes
	{MinNodes: 6, ThresholdScale: 0.7, TimeoutScale: 0.67},  // 6-10 nodes
	{MinNodes: 11, ThresholdScale: 1.0, TimeoutScale: 1.0},  // 11+ nodes
}

// ScoreConfig holds all configurable score parameters.
type ScoreConfig struct {
	// ScoreIncrement is added to a node's score on each failed probe.
	ScoreIncrement float64

	// SuspectedThreshold is the base score at which a node enters the suspected state.
	// Effective value is scaled by cluster size.
	SuspectedThreshold float64

	// QuarantineThreshold is the base score at which a node is quarantined.
	// Effective value is scaled by cluster size.
	QuarantineThreshold float64

	// QuarantineTimeout is the base time a node stays quarantined before being marked failed.
	// Effective value is scaled by cluster size.
	QuarantineTimeout time.Duration

	// ScaleBands defines how thresholds scale with cluster size.
	// If nil, DefaultScaleBands is used.
	ScaleBands []ClusterSizeScaleBand
}

// DefaultScoreConfig returns the default score configuration.
func DefaultScoreConfig() ScoreConfig {
	return ScoreConfig{
		ScoreIncrement:      DefaultScoreIncrement,
		SuspectedThreshold:  DefaultSuspectedThreshold,
		QuarantineThreshold: DefaultQuarantineThreshold,
		QuarantineTimeout:   DefaultQuarantineTimeout,
		ScaleBands:          DefaultScaleBands,
	}
}

// effectiveThresholds computes the actual thresholds based on current cluster size.
type effectiveThresholds struct {
	SuspectedThreshold  float64
	QuarantineThreshold float64
	QuarantineTimeout   time.Duration
}

// computeEffective returns thresholds scaled by cluster size.
func (cfg *ScoreConfig) computeEffective(clusterSize int) effectiveThresholds {
	bands := cfg.ScaleBands
	if len(bands) == 0 {
		bands = DefaultScaleBands
	}

	// Find the matching band (last band where MinNodes <= clusterSize)
	var band ClusterSizeScaleBand
	for _, b := range bands {
		if clusterSize >= b.MinNodes {
			band = b
		}
	}

	return effectiveThresholds{
		SuspectedThreshold:  cfg.SuspectedThreshold * band.ThresholdScale,
		QuarantineThreshold: cfg.QuarantineThreshold * band.ThresholdScale,
		QuarantineTimeout:   time.Duration(float64(cfg.QuarantineTimeout) * band.TimeoutScale),
	}
}

// ScoreTracker manages reliability scores for nodes in a cluster.
// Thresholds are dynamically scaled based on current cluster size — smaller
// clusters detect failures faster. All nodes compute the same effective
// thresholds because they share the same phonebook data via sync.
type ScoreTracker struct {
	mu          sync.RWMutex
	phonebook   phonebook.IPhonebook
	eventBus    events.Bus
	logger      *zap.Logger
	config      ScoreConfig
	clusterPath string

	// Track when nodes entered quarantine for timeout detection
	quarantineTimers map[string]time.Time // nodeID → quarantine start time
}

// NewScoreTracker creates a score tracker for a cluster.
func NewScoreTracker(
	clusterPath string,
	pb phonebook.IPhonebook,
	eventBus events.Bus,
	logger *zap.Logger,
	config ScoreConfig,
) *ScoreTracker {
	return &ScoreTracker{
		phonebook:        pb,
		eventBus:         eventBus,
		logger:           logger,
		config:           config,
		clusterPath:      clusterPath,
		quarantineTimers: make(map[string]time.Time),
	}
}

// clusterSize returns the current number of nodes in the cluster.
func (st *ScoreTracker) clusterSize() int {
	entries, err := st.phonebook.GetByCluster(st.clusterPath)
	if err != nil {
		return 1
	}
	return len(entries)
}

// effective returns the current effective thresholds based on cluster size.
func (st *ScoreTracker) effective() effectiveThresholds {
	return st.config.computeEffective(st.clusterSize())
}

// EffectiveThresholds returns the current effective thresholds (for logging/debugging).
func (st *ScoreTracker) EffectiveThresholds() (suspectedThreshold, quarantineThreshold float64, quarantineTimeout time.Duration) {
	e := st.effective()
	return e.SuspectedThreshold, e.QuarantineThreshold, e.QuarantineTimeout
}

// RecordFailure increments a node's reliability score after a failed probe.
// Returns the new score. Emits status transition events if thresholds are crossed.
func (st *ScoreTracker) RecordFailure(nodeID string, reason string) float64 {
	st.mu.Lock()
	defer st.mu.Unlock()

	entry, err := st.phonebook.Get(nodeID, st.clusterPath)
	if err != nil || entry == nil {
		st.logger.Debug("cannot record failure for unknown node",
			zap.String("nodeId", nodeID))
		return 0
	}

	oldScore := entry.ReliabilityScore
	oldStatus := entry.Status
	newScore := oldScore + st.config.ScoreIncrement

	// Update phonebook
	if err := st.phonebook.SetReliabilityScore(nodeID, st.clusterPath, newScore); err != nil {
		st.logger.Error("failed to update reliability score",
			zap.String("nodeId", nodeID),
			zap.Error(err))
		return oldScore
	}

	// Check threshold crossings and update status
	st.checkThresholds(nodeID, oldScore, newScore, oldStatus, reason)

	return newScore
}

// RecordSuccess resets a node's reliability score to 0 after a successful probe.
// Emits a NodeRecovered event if the node was previously suspected or quarantined.
func (st *ScoreTracker) RecordSuccess(nodeID string, reason string) {
	st.mu.Lock()
	defer st.mu.Unlock()

	entry, err := st.phonebook.Get(nodeID, st.clusterPath)
	if err != nil || entry == nil {
		return
	}

	oldStatus := entry.Status

	// Reset score to 0
	if err := st.phonebook.SetReliabilityScore(nodeID, st.clusterPath, 0); err != nil {
		st.logger.Error("failed to reset reliability score",
			zap.String("nodeId", nodeID),
			zap.Error(err))
		return
	}

	// Set status to active
	if err := st.phonebook.SetStatus(nodeID, st.clusterPath, phonebook.NodeStatusEnum.Active()); err != nil {
		st.logger.Error("failed to set node status to active",
			zap.String("nodeId", nodeID),
			zap.Error(err))
		return
	}

	// Remove quarantine timer if present
	delete(st.quarantineTimers, nodeID)

	// Emit recovery event if was degraded — includes Departed so that
	// a peer that gracefully left and then came back (e.g. operator
	// restarted the daemon) gets reactivated on the first successful
	// probe instead of staying invisible.
	if oldStatus == phonebook.NodeStatusEnum.Suspected() ||
		oldStatus == phonebook.NodeStatusEnum.Quarantined() ||
		oldStatus == phonebook.NodeStatusEnum.Departed() {
		st.logger.Info("node recovered",
			zap.String("nodeId", nodeID),
			zap.String("cluster", st.clusterPath),
			zap.String("previousStatus", string(oldStatus)))

		st.eventBus.Publish(events.NodeRecovered{
			BaseEvent:      events.NewBaseEvent(),
			NodeID:         nodeID,
			ClusterPath:    st.clusterPath,
			PreviousStatus: string(oldStatus),
		})
	}

	// Emit probe result
	st.eventBus.Publish(events.NodeProbeResult{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      nodeID,
		ClusterPath: st.clusterPath,
		Success:     true,
		ProbeType:   reason,
		Score:       0,
	})
}

// SetScore sets a node's score directly (used when receiving ScoreUpdate from PubSub).
// Uses SET semantics, not ADD. Checks effective thresholds (cluster-size-scaled) after setting.
func (st *ScoreTracker) SetScore(nodeID string, score float64, alive bool, reason string) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if alive {
		// Alive always wins — reset to 0
		oldEntry, _ := st.phonebook.Get(nodeID, st.clusterPath)
		st.phonebook.SetReliabilityScore(nodeID, st.clusterPath, 0)
		st.phonebook.SetStatus(nodeID, st.clusterPath, phonebook.NodeStatusEnum.Active())
		delete(st.quarantineTimers, nodeID)

		// Emit recovery event if was degraded
		if oldEntry != nil && (oldEntry.Status == phonebook.NodeStatusEnum.Suspected() || oldEntry.Status == phonebook.NodeStatusEnum.Quarantined()) {
			st.eventBus.Publish(events.NodeRecovered{
				BaseEvent:      events.NewBaseEvent(),
				NodeID:         nodeID,
				ClusterPath:    st.clusterPath,
				PreviousStatus: string(oldEntry.Status),
			})
		}
		return
	}

	entry, err := st.phonebook.Get(nodeID, st.clusterPath)
	if err != nil || entry == nil {
		return
	}

	oldScore := entry.ReliabilityScore
	oldStatus := entry.Status

	st.phonebook.SetReliabilityScore(nodeID, st.clusterPath, score)
	st.checkThresholds(nodeID, oldScore, score, oldStatus, reason)
}

// GetScore returns the current reliability score for a node.
func (st *ScoreTracker) GetScore(nodeID string) float64 {
	entry, err := st.phonebook.Get(nodeID, st.clusterPath)
	if err != nil || entry == nil {
		return 0
	}
	return entry.ReliabilityScore
}

// CheckQuarantineTimeouts checks all quarantined nodes and marks them as failed
// if they've been quarantined longer than the effective QuarantineTimeout
// (scaled by current cluster size).
// Returns the list of node IDs that were marked as failed.
func (st *ScoreTracker) CheckQuarantineTimeouts() []string {
	st.mu.Lock()
	defer st.mu.Unlock()

	eff := st.config.computeEffective(st.clusterSize())
	var failed []string
	now := time.Now()

	for nodeID, quarantinedAt := range st.quarantineTimers {
		if now.Sub(quarantinedAt) <= eff.QuarantineTimeout {
			continue
		}

		st.logger.Info("quarantine timeout exceeded, marking as failed",
			zap.String("nodeId", nodeID),
			zap.String("cluster", st.clusterPath),
			zap.Duration("quarantineDuration", now.Sub(quarantinedAt)))

		if err := st.phonebook.SetStatus(nodeID, st.clusterPath, phonebook.NodeStatusEnum.Failed()); err != nil {
			st.logger.Error("failed to mark node as failed",
				zap.String("nodeId", nodeID),
				zap.Error(err))
			continue
		}

		delete(st.quarantineTimers, nodeID)
		failed = append(failed, nodeID)

		st.eventBus.Publish(events.NodeFailed{
			BaseEvent:   events.NewBaseEvent(),
			NodeID:      nodeID,
			ClusterPath: st.clusterPath,
		})
	}

	return failed
}

// checkThresholds checks if a score change crossed any effective thresholds
// (scaled by cluster size) and updates status. Caller must hold st.mu.
func (st *ScoreTracker) checkThresholds(nodeID string, oldScore, newScore float64, oldStatus phonebook.NodeStatus, reason string) {
	eff := st.config.computeEffective(st.clusterSize())

	// Quarantine threshold crossing
	if oldScore < eff.QuarantineThreshold && newScore >= eff.QuarantineThreshold {
		st.phonebook.SetStatus(nodeID, st.clusterPath, phonebook.NodeStatusEnum.Quarantined())
		st.quarantineTimers[nodeID] = time.Now()

		st.logger.Warn("node quarantined",
			zap.String("nodeId", nodeID),
			zap.String("cluster", st.clusterPath),
			zap.Float64("score", newScore),
			zap.Float64("effectiveThreshold", eff.QuarantineThreshold),
			zap.Int("clusterSize", st.clusterSize()),
			zap.String("reason", reason))

		st.eventBus.Publish(events.NodeQuarantined{
			BaseEvent:   events.NewBaseEvent(),
			NodeID:      nodeID,
			ClusterPath: st.clusterPath,
			Score:       newScore,
		})
		return
	}

	// Suspected threshold crossing
	if oldScore < eff.SuspectedThreshold && newScore >= eff.SuspectedThreshold {
		st.phonebook.SetStatus(nodeID, st.clusterPath, phonebook.NodeStatusEnum.Suspected())

		st.logger.Info("node suspected",
			zap.String("nodeId", nodeID),
			zap.String("cluster", st.clusterPath),
			zap.Float64("score", newScore),
			zap.Float64("effectiveThreshold", eff.SuspectedThreshold),
			zap.Int("clusterSize", st.clusterSize()),
			zap.String("reason", reason))

		st.eventBus.Publish(events.NodeSuspected{
			BaseEvent:   events.NewBaseEvent(),
			NodeID:      nodeID,
			ClusterPath: st.clusterPath,
			Score:       newScore,
		})
	}

	// Emit probe result
	st.eventBus.Publish(events.NodeProbeResult{
		BaseEvent:   events.NewBaseEvent(),
		NodeID:      nodeID,
		ClusterPath: st.clusterPath,
		Success:     false,
		ProbeType:   reason,
		Score:       newScore,
	})
}
