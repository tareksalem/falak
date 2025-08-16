package node

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// ConnectionOptimizer handles connection priority calculations and peer ranking
type ConnectionOptimizer struct {
	node              *Node
	localDataCenter   string
	latencyMap        map[string]time.Duration
	preferenceWeights map[string]float64
	mu                sync.RWMutex
	config            *OptimizationConfig
}

// OptimizationConfig configures the connection optimization behavior
type OptimizationConfig struct {
	LocalDCBonus        float64 `json:"local_dc_bonus"`
	SameDCBonus         float64 `json:"same_dc_bonus"`
	CrossRegionPenalty  float64 `json:"cross_region_penalty"`
	SeedPeerBonus       float64 `json:"seed_peer_bonus"`
	TrustScoreWeight    float64 `json:"trust_score_weight"`
	LatencyWeight       float64 `json:"latency_weight"`
	RecentnessWeight    float64 `json:"recentness_weight"`
	MaxLatencyThreshold time.Duration `json:"max_latency_threshold"`
	PreferIPv4          bool          `json:"prefer_ipv4"`
	AvoidPrivateIPs     bool          `json:"avoid_private_ips"`
}

// DefaultOptimizationConfig returns sensible optimization defaults
func DefaultOptimizationConfig() *OptimizationConfig {
	return &OptimizationConfig{
		LocalDCBonus:        100.0,
		SameDCBonus:         50.0,
		CrossRegionPenalty:  -20.0,
		SeedPeerBonus:       25.0,
		TrustScoreWeight:    30.0,
		LatencyWeight:       40.0,
		RecentnessWeight:    15.0,
		MaxLatencyThreshold: 5 * time.Second,
		PreferIPv4:          true,
		AvoidPrivateIPs:     false,
	}
}

// NewConnectionOptimizer creates a new connection optimizer
func NewConnectionOptimizer(node *Node, localDC string, config *OptimizationConfig) *ConnectionOptimizer {
	if config == nil {
		config = DefaultOptimizationConfig()
	}

	return &ConnectionOptimizer{
		node:              node,
		localDataCenter:   localDC,
		latencyMap:        make(map[string]time.Duration),
		preferenceWeights: make(map[string]float64),
		config:            config,
	}
}

// RankPeersByPriority sorts peers by connection priority (highest first)
func (co *ConnectionOptimizer) RankPeersByPriority(peers []Peer) []Peer {
	if len(peers) <= 1 {
		return peers
	}

	// Create peer priority entries
	type peerPriority struct {
		peer     Peer
		priority float64
		reasons  []string
	}

	var priorities []peerPriority

	for _, peer := range peers {
		priority, reasons := co.CalculateConnectionPriority(peer)
		priorities = append(priorities, peerPriority{
			peer:     peer,
			priority: priority,
			reasons:  reasons,
		})
	}

	// Sort by priority (highest first)
	sort.Slice(priorities, func(i, j int) bool {
		return priorities[i].priority > priorities[j].priority
	})

	// Extract sorted peers
	result := make([]Peer, len(priorities))
	for i, pp := range priorities {
		result[i] = pp.peer
		
		// Log top priority reasons for debugging
		if i < 5 { // Log top 5
			log.Printf("Peer %s priority %.1f: %v", 
				pp.peer.ID[:12], pp.priority, pp.reasons)
		}
	}

	return result
}

// CalculateConnectionPriority calculates a priority score for a peer
func (co *ConnectionOptimizer) CalculateConnectionPriority(peer Peer) (float64, []string) {
	score := 0.0
	var reasons []string

	// 1. Data Center Proximity Scoring
	dcScore, dcReasons := co.calculateDataCenterScore(peer)
	score += dcScore
	reasons = append(reasons, dcReasons...)

	// 2. Trust Score
	trustScore := peer.TrustScore * co.config.TrustScoreWeight
	score += trustScore
	if trustScore > 10 {
		reasons = append(reasons, fmt.Sprintf("trust:%.1f", trustScore))
	}

	// 3. Peer Type Bonuses
	typeScore, typeReasons := co.calculateTypeScore(peer)
	score += typeScore
	reasons = append(reasons, typeReasons...)

	// 4. Latency Scoring
	latencyScore, latencyReasons := co.calculateLatencyScore(peer)
	score += latencyScore
	reasons = append(reasons, latencyReasons...)

	// 5. Recentness Scoring (when was peer last seen)
	recentnessScore, recentnessReasons := co.calculateRecentnessScore(peer)
	score += recentnessScore
	reasons = append(reasons, recentnessReasons...)

	// 6. Address Quality Scoring
	addrScore, addrReasons := co.calculateAddressScore(peer)
	score += addrScore
	reasons = append(reasons, addrReasons...)

	// 7. Load Balancing Scoring
	loadScore, loadReasons := co.calculateLoadBalancingScore(peer)
	score += loadScore
	reasons = append(reasons, loadReasons...)

	return score, reasons
}

// calculateDataCenterScore scores based on data center proximity
func (co *ConnectionOptimizer) calculateDataCenterScore(peer Peer) (float64, []string) {
	score := 0.0
	var reasons []string

	if peer.DataCenter == "" {
		return 0.0, []string{"dc:unknown"}
	}

	// Local data center gets highest bonus
	if peer.DataCenter == co.localDataCenter {
		score += co.config.LocalDCBonus
		reasons = append(reasons, "dc:local")
		return score, reasons
	}

	// Same region gets moderate bonus
	if co.isSameRegion(peer.DataCenter, co.localDataCenter) {
		score += co.config.SameDCBonus * 0.7
		reasons = append(reasons, "dc:same-region")
	} else {
		// Cross-region penalty
		score += co.config.CrossRegionPenalty
		reasons = append(reasons, "dc:cross-region")
	}

	return score, reasons
}

// calculateTypeScore scores based on peer type and role
func (co *ConnectionOptimizer) calculateTypeScore(peer Peer) (float64, []string) {
	score := 0.0
	var reasons []string

	// Seed peers get bonus
	if peer.Status == "seed" {
		score += co.config.SeedPeerBonus
		reasons = append(reasons, "type:seed")
	}

	// Bootstrap peers get extra bonus
	if role, exists := peer.Tags["role"]; exists && role == "bootstrap" {
		score += co.config.SeedPeerBonus * 1.5
		reasons = append(reasons, "type:bootstrap")
	}

	// High priority seed peers
	if priority, exists := peer.Tags["seed_priority"]; exists {
		if p, ok := priority.(int); ok && p >= 90 {
			score += 15.0
			reasons = append(reasons, "type:high-priority")
		}
	}

	return score, reasons
}

// calculateLatencyScore scores based on historical latency
func (co *ConnectionOptimizer) calculateLatencyScore(peer Peer) (float64, []string) {
	co.mu.RLock()
	latency, hasLatency := co.latencyMap[peer.ID]
	co.mu.RUnlock()

	if !hasLatency {
		return 0.0, []string{"latency:unknown"}
	}

	// Convert latency to score (lower latency = higher score)
	if latency > co.config.MaxLatencyThreshold {
		return -30.0, []string{"latency:high"}
	}

	// Score inversely proportional to latency
	latencyMS := float64(latency.Milliseconds())
	if latencyMS <= 50 {
		return co.config.LatencyWeight, []string{"latency:excellent"}
	} else if latencyMS <= 200 {
		return co.config.LatencyWeight * 0.8, []string{"latency:good"}
	} else if latencyMS <= 500 {
		return co.config.LatencyWeight * 0.5, []string{"latency:fair"}
	} else {
		return co.config.LatencyWeight * 0.2, []string{"latency:poor"}
	}
}

// calculateRecentnessScore scores based on how recently the peer was seen
func (co *ConnectionOptimizer) calculateRecentnessScore(peer Peer) (float64, []string) {
	if peer.LastSeen.IsZero() {
		return 0.0, []string{"seen:never"}
	}

	timeSince := time.Since(peer.LastSeen)
	
	if timeSince < 5*time.Minute {
		return co.config.RecentnessWeight, []string{"seen:recent"}
	} else if timeSince < 1*time.Hour {
		return co.config.RecentnessWeight * 0.8, []string{"seen:hourly"}
	} else if timeSince < 24*time.Hour {
		return co.config.RecentnessWeight * 0.5, []string{"seen:daily"}
	} else if timeSince < 7*24*time.Hour {
		return co.config.RecentnessWeight * 0.2, []string{"seen:weekly"}
	} else {
		return -5.0, []string{"seen:stale"}
	}
}

// calculateAddressScore scores based on address quality
func (co *ConnectionOptimizer) calculateAddressScore(peer Peer) (float64, []string) {
	score := 0.0
	var reasons []string

	if len(peer.AddrInfo.Addrs) == 0 {
		return -50.0, []string{"addr:none"}
	}

	hasPublicIPv4 := false
	hasPrivateIP := false
	hasIPv6 := false

	for _, addr := range peer.AddrInfo.Addrs {
		addrStr := addr.String()
		
		// Check for IPv4/IPv6
		if isIPv4Address(addrStr) {
			if isPrivateIP(addrStr) {
				hasPrivateIP = true
			} else {
				hasPublicIPv4 = true
			}
		} else if isIPv6Address(addrStr) {
			hasIPv6 = true
		}
	}

	// Score based on address types
	if hasPublicIPv4 {
		score += 20.0
		reasons = append(reasons, "addr:public-ipv4")
	}
	
	if hasPrivateIP && !co.config.AvoidPrivateIPs {
		score += 10.0
		reasons = append(reasons, "addr:private")
	} else if hasPrivateIP && co.config.AvoidPrivateIPs {
		score -= 10.0
		reasons = append(reasons, "addr:private-avoided")
	}

	if hasIPv6 && !co.config.PreferIPv4 {
		score += 5.0
		reasons = append(reasons, "addr:ipv6")
	}

	return score, reasons
}

// calculateLoadBalancingScore promotes diversity in connections
func (co *ConnectionOptimizer) calculateLoadBalancingScore(peer Peer) (float64, []string) {
	// Check how many connections we already have to this DC
	dcConnections := co.getDataCenterConnectionCount(peer.DataCenter)
	
	if dcConnections == 0 {
		return 15.0, []string{"balance:new-dc"}
	} else if dcConnections < 3 {
		return 5.0, []string{"balance:balanced"}
	} else {
		return -10.0, []string{"balance:saturated"}
	}
}

// UpdateLatencyMetrics updates the latency information for a peer
func (co *ConnectionOptimizer) UpdateLatencyMetrics(peerID string, latency time.Duration) {
	co.mu.Lock()
	defer co.mu.Unlock()
	
	// Store raw latency
	co.latencyMap[peerID] = latency
	
	// Update weighted moving average
	if existing, exists := co.preferenceWeights[peerID]; exists {
		// Exponential moving average with alpha = 0.3
		alpha := 0.3
		newWeight := alpha*calculateLatencyWeight(latency) + (1-alpha)*existing
		co.preferenceWeights[peerID] = newWeight
	} else {
		co.preferenceWeights[peerID] = calculateLatencyWeight(latency)
	}
}

// calculateLatencyWeight converts latency to a weight score
func calculateLatencyWeight(latency time.Duration) float64 {
	// Inverse relationship: lower latency = higher weight
	ms := float64(latency.Milliseconds())
	if ms <= 0 {
		return 100.0
	}
	return math.Max(0, 100.0-ms/10.0)
}

// GetOptimizedPeerList returns a list of peers optimized for connection
func (co *ConnectionOptimizer) GetOptimizedPeerList(maxPeers int) []Peer {
	allPeers := co.node.phonebook.GetPeers()
	if len(allPeers) == 0 {
		return nil
	}

	// Rank all peers
	rankedPeers := co.RankPeersByPriority(allPeers)
	
	// Return top N peers
	if maxPeers > 0 && len(rankedPeers) > maxPeers {
		return rankedPeers[:maxPeers]
	}
	
	return rankedPeers
}

// GetOptimizedPeersByDataCenter returns optimized peers grouped by data center
func (co *ConnectionOptimizer) GetOptimizedPeersByDataCenter(maxPerDC int) map[string][]Peer {
	result := make(map[string][]Peer)
	
	// Get peers by data center
	datacenters := co.node.phonebook.GetDataCenters()
	
	for _, dc := range datacenters {
		dcPeers := co.node.phonebook.GetPeersByDataCenter(dc)
		if len(dcPeers) == 0 {
			continue
		}
		
		// Rank peers within this data center
		rankedPeers := co.RankPeersByPriority(dcPeers)
		
		// Limit per DC
		if maxPerDC > 0 && len(rankedPeers) > maxPerDC {
			rankedPeers = rankedPeers[:maxPerDC]
		}
		
		result[dc] = rankedPeers
	}
	
	return result
}

// Helper functions

// isSameRegion checks if two data centers are in the same region
func (co *ConnectionOptimizer) isSameRegion(dc1, dc2 string) bool {
	// Simple heuristic: check if they have common prefix
	if len(dc1) < 3 || len(dc2) < 3 {
		return false
	}
	
	// Extract region (e.g., "us-east" from "us-east-1")
	region1 := extractRegion(dc1)
	region2 := extractRegion(dc2)
	
	return region1 == region2
}

// extractRegion extracts region from data center name
func extractRegion(dc string) string {
	// Simple implementation: take first two parts of dash-separated name
	parts := strings.Split(dc, "-")
	if len(parts) >= 2 {
		return parts[0] + "-" + parts[1]
	}
	return dc
}

// getDataCenterConnectionCount counts existing connections to a data center
func (co *ConnectionOptimizer) getDataCenterConnectionCount(dc string) int {
	// This would integrate with the connection manager to get actual connection counts
	// For now, return 0 as placeholder
	return 0
}

// isIPv4Address checks if an address string contains IPv4
func isIPv4Address(addr string) bool {
	return strings.Contains(addr, "/ip4/")
}

// isIPv6Address checks if an address string contains IPv6
func isIPv6Address(addr string) bool {
	return strings.Contains(addr, "/ip6/")
}

// isPrivateIP checks if an address is a private IP
func isPrivateIP(addr string) bool {
	// Simple check for common private IP ranges in multiaddr format
	return strings.Contains(addr, "/ip4/10.") ||
		   strings.Contains(addr, "/ip4/192.168.") ||
		   strings.Contains(addr, "/ip4/172.16.") ||
		   strings.Contains(addr, "/ip4/172.17.") ||
		   strings.Contains(addr, "/ip4/172.18.") ||
		   strings.Contains(addr, "/ip4/172.19.") ||
		   strings.Contains(addr, "/ip4/172.2") ||
		   strings.Contains(addr, "/ip4/172.30.") ||
		   strings.Contains(addr, "/ip4/172.31.")
}

// GetOptimizationStats returns statistics about the optimization process
func (co *ConnectionOptimizer) GetOptimizationStats() OptimizationStats {
	co.mu.RLock()
	defer co.mu.RUnlock()
	
	stats := OptimizationStats{
		LocalDataCenter:    co.localDataCenter,
		TrackedPeers:       len(co.latencyMap),
		AverageLatency:     co.calculateAverageLatency(),
		OptimizationConfig: *co.config,
		LastUpdate:         time.Now(),
	}
	
	return stats
}

// OptimizationStats provides statistics about connection optimization
type OptimizationStats struct {
	LocalDataCenter    string                `json:"local_data_center"`
	TrackedPeers       int                   `json:"tracked_peers"`
	AverageLatency     time.Duration         `json:"average_latency"`
	OptimizationConfig OptimizationConfig    `json:"optimization_config"`
	LastUpdate         time.Time             `json:"last_update"`
}

// calculateAverageLatency calculates the average latency across all tracked peers
func (co *ConnectionOptimizer) calculateAverageLatency() time.Duration {
	if len(co.latencyMap) == 0 {
		return 0
	}
	
	total := time.Duration(0)
	for _, latency := range co.latencyMap {
		total += latency
	}
	
	return total / time.Duration(len(co.latencyMap))
}