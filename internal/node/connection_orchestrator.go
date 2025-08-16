package node

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// ConnectionOrchestrator manages parallel connection attempts across data centers
type ConnectionOrchestrator struct {
	connManager      *ConnectionManager
	resultChan       chan DCConnectionResult
	timeoutDuration  time.Duration
	minSuccessfulDCs int
	maxWaitTime      time.Duration
	mu               sync.RWMutex
	orchestrationID  string
}

// OrchestrationConfig configures the orchestration behavior
type OrchestrationConfig struct {
	MinSuccessfulDCs   int           `json:"min_successful_dcs"`
	MaxWaitTime        time.Duration `json:"max_wait_time"`
	PreferredDCs       []string      `json:"preferred_dcs,omitempty"`
	ParallelismLevel   int           `json:"parallelism_level"`
	EarlySuccessWindow time.Duration `json:"early_success_window"`
	FailFastThreshold  float64       `json:"fail_fast_threshold"`
}

// DefaultOrchestrationConfig returns sensible defaults
func DefaultOrchestrationConfig() *OrchestrationConfig {
	return &OrchestrationConfig{
		MinSuccessfulDCs:   1,
		MaxWaitTime:        30 * time.Second,
		ParallelismLevel:   3, // Maximum concurrent DC connections
		EarlySuccessWindow: 5 * time.Second,
		FailFastThreshold:  0.8, // Fail fast if 80% of DCs fail quickly
	}
}

// NewConnectionOrchestrator creates a new connection orchestrator
func NewConnectionOrchestrator(cm *ConnectionManager, config *OrchestrationConfig) *ConnectionOrchestrator {
	if config == nil {
		config = DefaultOrchestrationConfig()
	}

	return &ConnectionOrchestrator{
		connManager:      cm,
		resultChan:       make(chan DCConnectionResult, 10),
		timeoutDuration:  config.MaxWaitTime,
		minSuccessfulDCs: config.MinSuccessfulDCs,
		maxWaitTime:      config.MaxWaitTime,
		orchestrationID:  fmt.Sprintf("orch-%d", time.Now().UnixNano()),
	}
}

// AttemptParallelConnections orchestrates parallel connections to multiple data centers
func (co *ConnectionOrchestrator) AttemptParallelConnections() error {
	log.Printf("[%s] Starting parallel connection orchestration", co.orchestrationID)

	// Get available data centers
	peersByDC := co.connManager.groupPeersByDataCenter()
	if len(peersByDC) == 0 {
		return fmt.Errorf("no data centers available for connection")
	}

	dcList := make([]string, 0, len(peersByDC))
	for dcID := range peersByDC {
		dcList = append(dcList, dcID)
	}

	// Prioritize data centers
	prioritizedDCs := co.prioritizeDataCenters(dcList, peersByDC)

	log.Printf("[%s] Attempting connections to %d data centers: %v",
		co.orchestrationID, len(prioritizedDCs), prioritizedDCs)

	// Start orchestration
	ctx, cancel := context.WithTimeout(context.Background(), co.maxWaitTime)
	defer cancel()

	return co.executeParallelConnections(ctx, prioritizedDCs, peersByDC)
}

// prioritizeDataCenters sorts data centers by connection priority
func (co *ConnectionOrchestrator) prioritizeDataCenters(dcList []string, peersByDC map[string][]Peer) []string {
	type dcScore struct {
		dcID  string
		score float64
	}

	var scores []dcScore

	for _, dcID := range dcList {
		peers := peersByDC[dcID]
		score := co.calculateDataCenterScore(dcID, peers)
		scores = append(scores, dcScore{dcID: dcID, score: score})
	}

	// Sort by score (highest first)
	sort.Slice(scores, func(i, j int) bool {
		return scores[i].score > scores[j].score
	})

	result := make([]string, len(scores))
	for i, s := range scores {
		result[i] = s.dcID
	}

	return result
}

// calculateDataCenterScore calculates a priority score for a data center
func (co *ConnectionOrchestrator) calculateDataCenterScore(dcID string, peers []Peer) float64 {
	score := 0.0

	// Base score: number of available peers
	score += float64(len(peers)) * 10.0

	// Bonus for seed peers
	seedPeers := 0
	for _, peer := range peers {
		if peer.Status == "seed" {
			seedPeers++
		}
		// Bonus for higher trust scores
		score += peer.TrustScore * 5.0
	}
	score += float64(seedPeers) * 20.0

	// Check historical success rate
	if stats := co.connManager.GetConnectionStats(); stats != nil {
		if dcStats, exists := stats[dcID]; exists {
			// Boost score for historically successful DCs
			if dcStats.SuccessCount > 0 {
				successRate := float64(dcStats.SuccessCount) /
					float64(dcStats.SuccessCount+dcStats.FailureCount)
				score += successRate * 30.0
			}

			// Penalty for currently degraded DCs
			if dcStats.HealthStatus == "degraded" {
				score *= 0.7
			} else if dcStats.HealthStatus == "unreachable" {
				score *= 0.3
			}
		}
	}

	// Local DC preference (if we can determine local DC)
	if co.isLocalDataCenter(dcID) {
		score += 50.0
	}

	return score
}

// isLocalDataCenter determines if a DC is the local one (simple heuristic)
func (co *ConnectionOrchestrator) isLocalDataCenter(dcID string) bool {
	// This is a placeholder - in reality, this would check node configuration
	// or use some other mechanism to determine locality
	return dcID == "local" || dcID == "default"
}

// executeParallelConnections runs the actual parallel connection process
func (co *ConnectionOrchestrator) executeParallelConnections(
	ctx context.Context,
	prioritizedDCs []string,
	peersByDC map[string][]Peer,
) error {

	totalDCs := len(prioritizedDCs)
	if totalDCs == 0 {
		return fmt.Errorf("no data centers to connect to")
	}

	// Create result channels
	resultChan := make(chan DCConnectionResult, totalDCs)

	// Launch connection attempts
	activeConnections := 0
	dcIndex := 0
	maxParallel := 3 // Configurable parallelism level

	var wg sync.WaitGroup

	// Start initial batch of connections
	for activeConnections < maxParallel && dcIndex < totalDCs {
		dcID := prioritizedDCs[dcIndex]
		peers := peersByDC[dcID]

		wg.Add(1)
		go func(dc string, dcPeers []Peer) {
			defer wg.Done()
			co.connectToDataCenterWithResult(dc, dcPeers, resultChan)
		}(dcID, peers)

		activeConnections++
		dcIndex++
	}

	// Collect results and potentially start more connections
	var results []DCConnectionResult
	successfulConnections := 0
	completedConnections := 0
	earlySuccessDeadline := time.Now().Add(5 * time.Second)

	for completedConnections < totalDCs {
		select {
		case result := <-resultChan:
			results = append(results, result)
			completedConnections++

			if result.Success {
				successfulConnections++
				log.Printf("[%s] ✓ DC %s connected successfully (%d peers, %v latency)",
					co.orchestrationID, result.DataCenter, result.PeersConnected, result.Latency)
			} else {
				log.Printf("[%s] ✗ DC %s connection failed: %v",
					co.orchestrationID, result.DataCenter, result.Error)
			}

			// Check if we can satisfy requirements early
			if successfulConnections >= co.minSuccessfulDCs {
				remainingTime := time.Until(earlySuccessDeadline)
				if remainingTime <= 0 || co.shouldStopEarly(results, totalDCs) {
					log.Printf("[%s] Early success: %d/%d DCs connected, stopping",
						co.orchestrationID, successfulConnections, co.minSuccessfulDCs)
					break
				}
			}

			// Start next connection if available
			if dcIndex < totalDCs {
				dcID := prioritizedDCs[dcIndex]
				peers := peersByDC[dcID]

				wg.Add(1)
				go func(dc string, dcPeers []Peer) {
					defer wg.Done()
					co.connectToDataCenterWithResult(dc, dcPeers, resultChan)
				}(dcID, peers)

				dcIndex++
			}

		case <-ctx.Done():
			log.Printf("[%s] Connection orchestration timed out", co.orchestrationID)
			return fmt.Errorf("connection orchestration timed out")
		}
	}

	// Wait for any remaining goroutines
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Drain any remaining results
	for result := range resultChan {
		results = append(results, result)
		if result.Success {
			successfulConnections++
		}
	}

	return co.evaluateConnectionResults(results, successfulConnections)
}

// connectToDataCenterWithResult wraps the connection manager call
func (co *ConnectionOrchestrator) connectToDataCenterWithResult(
	dcID string,
	peers []Peer,
	resultChan chan<- DCConnectionResult,
) {
	// Use the connection manager to connect
	co.connManager.connectToDataCenter(dcID, peers, resultChan)
}

// shouldStopEarly determines if we should stop connecting early
func (co *ConnectionOrchestrator) shouldStopEarly(results []DCConnectionResult, totalDCs int) bool {
	if len(results) < 3 { // Need some data points
		return false
	}

	// Check if we have enough successful connections with good latency
	fastSuccesses := 0
	for _, result := range results {
		if result.Success && result.Latency < 2*time.Second {
			fastSuccesses++
		}
	}

	// Stop early if we have multiple fast successes
	return fastSuccesses >= 2
}

// evaluateConnectionResults analyzes the final connection results
func (co *ConnectionOrchestrator) evaluateConnectionResults(
	results []DCConnectionResult,
	successfulConnections int,
) error {

	if successfulConnections < co.minSuccessfulDCs {
		// Analyze failure patterns
		failureAnalysis := co.analyzeFailures(results)
		return fmt.Errorf(
			"insufficient successful connections: got %d, need %d. Analysis: %s",
			successfulConnections, co.minSuccessfulDCs, failureAnalysis,
		)
	}

	// Log success summary
	totalLatency := time.Duration(0)
	totalPeers := 0

	for _, result := range results {
		if result.Success {
			totalLatency += result.Latency
			totalPeers += result.PeersConnected
		}
	}

	avgLatency := totalLatency / time.Duration(successfulConnections)

	log.Printf("[%s] ✓ Connection orchestration successful: %d DCs, %d peers, avg latency %v",
		co.orchestrationID, successfulConnections, totalPeers, avgLatency)

	return nil
}

// analyzeFailures provides insights into connection failures
func (co *ConnectionOrchestrator) analyzeFailures(results []DCConnectionResult) string {
	if len(results) == 0 {
		return "no connection attempts made"
	}

	failures := 0
	timeouts := 0
	networkErrors := 0
	authErrors := 0

	for _, result := range results {
		if !result.Success && result.Error != nil {
			failures++
			errStr := result.Error.Error()

			if result.Latency > 10*time.Second {
				timeouts++
			} else if containsAny(errStr, []string{"network", "connection refused", "timeout"}) {
				networkErrors++
			} else if containsAny(errStr, []string{"auth", "certificate", "handshake"}) {
				authErrors++
			}
		}
	}

	if failures == 0 {
		return "all attempted connections succeeded"
	}

	analysis := fmt.Sprintf("%d failures", failures)
	if timeouts > 0 {
		analysis += fmt.Sprintf(", %d timeouts", timeouts)
	}
	if networkErrors > 0 {
		analysis += fmt.Sprintf(", %d network errors", networkErrors)
	}
	if authErrors > 0 {
		analysis += fmt.Sprintf(", %d auth errors", authErrors)
	}

	return analysis
}

// containsAny checks if a string contains any of the given substrings
func containsAny(s string, substrings []string) bool {
	for _, substr := range substrings {
		if len(s) >= len(substr) {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
		}
	}
	return false
}

// WaitForMinimumConnections waits until minimum connections are established
func (co *ConnectionOrchestrator) WaitForMinimumConnections() error {
	timeout := time.After(co.timeoutDuration)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for minimum connections")
		case <-ticker.C:
			healthyDCs := co.connManager.GetHealthyDataCenters()
			if len(healthyDCs) >= co.minSuccessfulDCs {
				log.Printf("[%s] Minimum connections achieved: %d healthy DCs",
					co.orchestrationID, len(healthyDCs))
				return nil
			}
		}
	}
}

// GetOrchestrationStatus returns the current status of orchestration
func (co *ConnectionOrchestrator) GetOrchestrationStatus() OrchestrationStatus {
	co.mu.RLock()
	defer co.mu.RUnlock()

	healthyDCs := co.connManager.GetHealthyDataCenters()
	stats := co.connManager.GetConnectionStats()

	return OrchestrationStatus{
		OrchestrationID:    co.orchestrationID,
		HealthyDataCenters: len(healthyDCs),
		MinRequiredDCs:     co.minSuccessfulDCs,
		DataCenterStats:    stats,
		LastUpdate:         time.Now(),
	}
}

// OrchestrationStatus provides status information about the orchestration
type OrchestrationStatus struct {
	OrchestrationID    string                     `json:"orchestration_id"`
	HealthyDataCenters int                        `json:"healthy_datacenters"`
	MinRequiredDCs     int                        `json:"min_required_dcs"`
	DataCenterStats    map[string]ConnectionStats `json:"datacenter_stats"`
	LastUpdate         time.Time                  `json:"last_update"`
}

// IsHealthy returns true if the orchestration meets minimum requirements
func (os OrchestrationStatus) IsHealthy() bool {
	return os.HealthyDataCenters >= os.MinRequiredDCs
}

// GetSummary returns a human-readable summary
func (os OrchestrationStatus) GetSummary() string {
	status := "unhealthy"
	if os.IsHealthy() {
		status = "healthy"
	}

	return fmt.Sprintf("Orchestration %s: %s (%d/%d DCs)",
		os.OrchestrationID, status, os.HealthyDataCenters, os.MinRequiredDCs)
}
