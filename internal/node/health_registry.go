package node

import (
	"log"
	"math"
	"sync"
	"time"

	"github.com/tareksalem/falak/internal/node/protobuf/models"
)

// HealthStatus represents the health state of a peer
type HealthStatus string

const (
	healthy    HealthStatus = "healthy"
	suspect    HealthStatus = "suspect"
	failed     HealthStatus = "failed"
	rejoining  HealthStatus = "rejoining"
	quarantine HealthStatus = "quarantine"
)

var HealthStatusEnum = struct {
	Healthy    HealthStatus
	Suspect    HealthStatus
	Failed     HealthStatus
	Rejoining  HealthStatus
	Quarantine HealthStatus
}{
	Healthy:    healthy,
	Suspect:    suspect,
	Failed:     failed,
	Rejoining:  rejoining,
	Quarantine: quarantine,
}

// PeerHealthInfo contains health monitoring information for a peer
type PeerHealthInfo struct {
	NodeID            string
	Status            HealthStatus
	LastHeartbeat     time.Time
	LastHLC           uint64
	LastSeq           uint64
	Incarnation       uint64
	InterArrivalTimes []time.Duration // For phi accrual calculation
	Phi               float64         // Current suspicion score
	Strikes           int             // Flap counter
	Zone              string          // For quorum diversity
	ConnectedPeers    uint32
	Load              *models.Load
	mu                sync.RWMutex
}

// HealthRegistry tracks the health status of all known peers
type HealthRegistry struct {
	node      *Node
	peers     map[string]*PeerHealthInfo
	mu        sync.RWMutex
	
	// Configuration parameters
	phiSuspectThreshold  float64
	phiFailEdgeThreshold float64
	phiFailWanThreshold  float64
	heartbeatTimeout     time.Duration
	maxInterArrivalTimes int
}

// NewHealthRegistry creates a new health registry
func NewHealthRegistry(node *Node) *HealthRegistry {
	hr := &HealthRegistry{
		node:                 node,
		peers:                make(map[string]*PeerHealthInfo),
		phiSuspectThreshold:  5.0,
		phiFailEdgeThreshold: 9.0,
		phiFailWanThreshold:  12.0,
		heartbeatTimeout:     10 * time.Second,
		maxInterArrivalTimes: 100, // Keep last 100 inter-arrival times
	}
	
	// Initialize with phonebook peers
	hr.initializeExpectedPeers()
	
	return hr
}


// ProcessHeartbeat processes an incoming heartbeat and updates peer health
func (hr *HealthRegistry) ProcessHeartbeat(heartbeat *models.Heartbeat) {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	
	// Normalize the node ID to ensure consistent tracking
	nodeID := normalizePeerID(heartbeat.NodeId)
	now := time.Now()
	
	// Get or create peer health info
	peerInfo, exists := hr.peers[nodeID]
	if !exists {
		peerInfo = &PeerHealthInfo{
			NodeID:            nodeID,
			Status:            HealthStatusEnum.Healthy,
			InterArrivalTimes: make([]time.Duration, 0, hr.maxInterArrivalTimes),
			Zone:              "default", // TODO: Extract from heartbeat or phonebook
		}
		hr.peers[nodeID] = peerInfo
		log.Printf("📊 Added new peer to health registry: %s", nodeID)
	}
	
	peerInfo.mu.Lock()
	defer peerInfo.mu.Unlock()
	
	// Check for incarnation conflicts
	if heartbeat.Incarnation < peerInfo.Incarnation {
		log.Printf("⚠️ Received heartbeat with old incarnation from %s (%d < %d)", 
			nodeID, heartbeat.Incarnation, peerInfo.Incarnation)
		return // Ignore old incarnation
	}
	
	// If incarnation increased, this is a recovery
	if heartbeat.Incarnation > peerInfo.Incarnation {
		log.Printf("🔄 Peer %s recovered with new incarnation: %d -> %d", 
			nodeID, peerInfo.Incarnation, heartbeat.Incarnation)
		peerInfo.Status = HealthStatusEnum.Healthy
		peerInfo.Incarnation = heartbeat.Incarnation
		peerInfo.InterArrivalTimes = nil // Reset phi calculation
		peerInfo.Phi = 0
		peerInfo.Strikes = 0
	}
	
	// Calculate inter-arrival time
	if !peerInfo.LastHeartbeat.IsZero() {
		interArrival := now.Sub(peerInfo.LastHeartbeat)
		
		// Add to inter-arrival times (keep only recent ones)
		peerInfo.InterArrivalTimes = append(peerInfo.InterArrivalTimes, interArrival)
		if len(peerInfo.InterArrivalTimes) > hr.maxInterArrivalTimes {
			peerInfo.InterArrivalTimes = peerInfo.InterArrivalTimes[1:]
		}
		
		// Recalculate phi score
		peerInfo.Phi = hr.calculatePhi(peerInfo.InterArrivalTimes, interArrival)
	}
	
	// Update peer information
	peerInfo.LastHeartbeat = now
	peerInfo.LastHLC = heartbeat.Hlc
	peerInfo.LastSeq = heartbeat.Seq
	peerInfo.ConnectedPeers = heartbeat.PeersConnected
	peerInfo.Load = heartbeat.Load
	
	// Update status based on phi score
	hr.updatePeerStatus(peerInfo)
}

// calculatePhi calculates the phi accrual failure detector score
func (hr *HealthRegistry) calculatePhi(interArrivalTimes []time.Duration, currentInterval time.Duration) float64 {
	if len(interArrivalTimes) < 2 {
		return 0.0 // Not enough data
	}
	
	// Calculate mean and standard deviation of inter-arrival times
	var sum float64
	for _, interval := range interArrivalTimes {
		sum += float64(interval.Nanoseconds())
	}
	mean := sum / float64(len(interArrivalTimes))
	
	var variance float64
	for _, interval := range interArrivalTimes {
		diff := float64(interval.Nanoseconds()) - mean
		variance += diff * diff
	}
	variance /= float64(len(interArrivalTimes))
	stdDev := math.Sqrt(variance)
	
	if stdDev == 0 {
		return 0.0 // No variance
	}
	
	// Calculate phi using normal distribution approximation
	// phi = -log10(1 - CDF(currentInterval))
	currentNs := float64(currentInterval.Nanoseconds())
	z := (currentNs - mean) / stdDev
	
	// Approximate normal CDF using error function
	cdf := 0.5 * (1 + math.Erf(z/math.Sqrt(2)))
	
	// Avoid log(0) by setting minimum value
	if cdf >= 0.9999 {
		cdf = 0.9999
	}
	if cdf <= 0.0001 {
		cdf = 0.0001
	}
	
	phi := -math.Log10(1 - cdf)
	
	// Cap phi at reasonable maximum
	if phi > 50 {
		phi = 50
	}
	if phi < 0 {
		phi = 0
	}
	
	return phi
}

// updatePeerStatus updates peer status based on phi score
func (hr *HealthRegistry) updatePeerStatus(peerInfo *PeerHealthInfo) {
	oldStatus := peerInfo.Status
	
	switch peerInfo.Status {
	case HealthStatusEnum.Healthy:
		if peerInfo.Phi >= hr.phiSuspectThreshold {
			peerInfo.Status = HealthStatusEnum.Suspect
			log.Printf("⚠️ Peer %s marked as SUSPECT (phi: %.2f)", peerInfo.NodeID, peerInfo.Phi)
		}
		
	case HealthStatusEnum.Suspect:
		if peerInfo.Phi >= hr.phiFailEdgeThreshold {
			peerInfo.Status = HealthStatusEnum.Failed
			peerInfo.Strikes++
			log.Printf("❌ Peer %s marked as FAILED (phi: %.2f, strikes: %d)", 
				peerInfo.NodeID, peerInfo.Phi, peerInfo.Strikes)
		} else if peerInfo.Phi < hr.phiSuspectThreshold {
			peerInfo.Status = HealthStatusEnum.Healthy
			log.Printf("✅ Peer %s recovered to HEALTHY (phi: %.2f)", peerInfo.NodeID, peerInfo.Phi)
		}
		
	case HealthStatusEnum.Failed:
		// Failed peers can only recover with new incarnation (handled in ProcessHeartbeat)
		break
		
	case HealthStatusEnum.Rejoining:
		// Rejoining peers automatically become healthy after cooloff
		peerInfo.Status = HealthStatusEnum.Healthy
		log.Printf("✅ Peer %s rejoining completed", peerInfo.NodeID)
		
	case HealthStatusEnum.Quarantine:
		// TODO: Implement quarantine release logic
		break
	}
	
	// Check for flapping and quarantine
	if peerInfo.Strikes >= 3 && peerInfo.Status != HealthStatusEnum.Quarantine {
		peerInfo.Status = HealthStatusEnum.Quarantine
		log.Printf("🚫 Peer %s quarantined due to flapping (strikes: %d)", peerInfo.NodeID, peerInfo.Strikes)
	}
	
	// Log status changes
	if oldStatus != peerInfo.Status {
		log.Printf("🔄 Peer %s status: %s -> %s (phi: %.2f)", 
			peerInfo.NodeID, oldStatus, peerInfo.Status, peerInfo.Phi)
	}
}

// GetPeerHealth returns health information for a specific peer
func (hr *HealthRegistry) GetPeerHealth(nodeID string) (*PeerHealthInfo, bool) {
	hr.mu.RLock()
	defer hr.mu.RUnlock()
	
	// Normalize the node ID for consistent lookup
	normalizedID := normalizePeerID(nodeID)
	peerInfo, exists := hr.peers[normalizedID]
	if !exists {
		return nil, false
	}
	
	// Return a copy to avoid race conditions
	peerInfo.mu.RLock()
	defer peerInfo.mu.RUnlock()
	
	copy := &PeerHealthInfo{
		NodeID:         peerInfo.NodeID,
		Status:         peerInfo.Status,
		LastHeartbeat:  peerInfo.LastHeartbeat,
		LastHLC:        peerInfo.LastHLC,
		LastSeq:        peerInfo.LastSeq,
		Incarnation:    peerInfo.Incarnation,
		Phi:            peerInfo.Phi,
		Strikes:        peerInfo.Strikes,
		Zone:           peerInfo.Zone,
		ConnectedPeers: peerInfo.ConnectedPeers,
		Load:           peerInfo.Load,
	}
	
	return copy, true
}

// GetAllPeers returns health information for all peers
func (hr *HealthRegistry) GetAllPeers() map[string]*PeerHealthInfo {
	hr.mu.RLock()
	defer hr.mu.RUnlock()
	
	result := make(map[string]*PeerHealthInfo)
	for nodeID, peerInfo := range hr.peers {
		peerInfo.mu.RLock()
		result[nodeID] = &PeerHealthInfo{
			NodeID:         peerInfo.NodeID,
			Status:         peerInfo.Status,
			LastHeartbeat:  peerInfo.LastHeartbeat,
			LastHLC:        peerInfo.LastHLC,
			LastSeq:        peerInfo.LastSeq,
			Incarnation:    peerInfo.Incarnation,
			Phi:            peerInfo.Phi,
			Strikes:        peerInfo.Strikes,
			Zone:           peerInfo.Zone,
			ConnectedPeers: peerInfo.ConnectedPeers,
			Load:           peerInfo.Load,
		}
		peerInfo.mu.RUnlock()
	}
	
	return result
}

// GetHealthySuspectedPeers returns counts of healthy and suspected peers
func (hr *HealthRegistry) GetHealthySuspectedPeers() (healthy, suspected, failed int) {
	hr.mu.RLock()
	defer hr.mu.RUnlock()
	
	for _, peerInfo := range hr.peers {
		peerInfo.mu.RLock()
		switch peerInfo.Status {
		case HealthStatusEnum.Healthy:
			healthy++
		case HealthStatusEnum.Suspect:
			suspected++
		case HealthStatusEnum.Failed:
			failed++
		}
		peerInfo.mu.RUnlock()
	}
	
	return healthy, suspected, failed
}

// PeriodicHealthCheck performs periodic health monitoring tasks
func (hr *HealthRegistry) PeriodicHealthCheck() {
	hr.mu.RLock()
	defer hr.mu.RUnlock()
	
	now := time.Now()
	var suspectedPeers []string
	
	for nodeID, peerInfo := range hr.peers {
		peerInfo.mu.Lock()
		
		// Check for timeout
		if now.Sub(peerInfo.LastHeartbeat) > hr.heartbeatTimeout {
			if peerInfo.Status == HealthStatusEnum.Healthy {
				peerInfo.Status = HealthStatusEnum.Suspect
				peerInfo.Phi = hr.phiSuspectThreshold + 1 // Force suspect status
				log.Printf("⏰ Peer %s timed out, marked as SUSPECT", nodeID)
				suspectedPeers = append(suspectedPeers, nodeID)
				
				// Publish suspicion for timeout
				hr.publishSuspicionForTimeout(nodeID, peerInfo.Phi)
			} else if peerInfo.Status == HealthStatusEnum.Suspect {
				peerInfo.Status = HealthStatusEnum.Failed
				peerInfo.Strikes++
				log.Printf("⏰ Peer %s failed due to timeout", nodeID)
			}
		}
		
		peerInfo.mu.Unlock()
	}
	
	// Trigger SWIM probing for suspected peers
	hr.triggerSWIMProbing(suspectedPeers)
}

// triggerSWIMProbing triggers SWIM probing for suspected peers
func (hr *HealthRegistry) triggerSWIMProbing(suspectedPeers []string) {
	if hr.node.swimProber == nil || len(suspectedPeers) == 0 {
		return
	}
	
	// Probe suspected peers in background
	for _, nodeID := range suspectedPeers {
		go hr.node.swimProber.ProbeIfSuspected(nodeID)
	}
}

// publishSuspicionForTimeout publishes a suspicion when a peer times out
func (hr *HealthRegistry) publishSuspicionForTimeout(nodeID string, phi float64) {
	if hr.node.suspicionManager == nil {
		return
	}
	
	// Get cluster ID (use first cluster)
	clusterID := "default"
	if len(hr.node.clusters) > 0 {
		clusterID = hr.node.clusters[0]
	}
	
	// Publish suspicion
	if err := hr.node.suspicionManager.PublishSuspicion(nodeID, SuspicionReasonEnum.HeartbeatTimeout, phi, nil, clusterID); err != nil {
		log.Printf("❌ Failed to publish suspicion for timeout %s: %v", nodeID, err)
	}
}

// UpdatePeerPhi updates the phi score for a specific peer
func (hr *HealthRegistry) UpdatePeerPhi(nodeID string, newPhi float64) bool {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	
	// Normalize the node ID for consistent lookup
	normalizedID := normalizePeerID(nodeID)
	peerInfo, exists := hr.peers[normalizedID]
	if !exists {
		return false
	}
	
	peerInfo.mu.Lock()
	defer peerInfo.mu.Unlock()
	
	oldPhi := peerInfo.Phi
	peerInfo.Phi = newPhi
	
	// Update status based on new phi score
	hr.updatePeerStatus(peerInfo)
	
	log.Printf("📊 Updated phi score for %s: %.2f -> %.2f", nodeID, oldPhi, newPhi)
	return true
}

// ForceFailureStatus forces a peer to failed status (used for quorum decisions)
func (hr *HealthRegistry) ForceFailureStatus(nodeID string) bool {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	
	// Normalize the node ID for consistent lookup
	normalizedID := normalizePeerID(nodeID)
	peerInfo, exists := hr.peers[normalizedID]
	if !exists {
		return false
	}
	
	peerInfo.mu.Lock()
	defer peerInfo.mu.Unlock()
	
	oldStatus := peerInfo.Status
	peerInfo.Status = HealthStatusEnum.Failed
	peerInfo.Phi = hr.phiFailEdgeThreshold + 1 // Force high phi
	peerInfo.Strikes++
	
	log.Printf("💀 Forced peer %s to FAILED status (was: %s)", nodeID, oldStatus)
	return true
}

// RemovePeer removes a peer from health monitoring
func (hr *HealthRegistry) RemovePeer(nodeID string) {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	
	// Normalize the node ID for consistent removal
	normalizedID := normalizePeerID(nodeID)
	delete(hr.peers, normalizedID)
	log.Printf("📊 Removed peer from health registry: %s", normalizedID)
}

// initializeExpectedPeers initializes the health registry with peers from phonebook
func (hr *HealthRegistry) initializeExpectedPeers() {
	if hr.node.phonebook == nil {
		log.Printf("⚠️ Cannot initialize expected peers: phonebook not available")
		return
	}
	
	hr.mu.Lock()
	defer hr.mu.Unlock()
	
	phonebookPeers := hr.node.phonebook.GetPeers()
	initializedCount := 0
	
	for _, peer := range phonebookPeers {
		// Normalize peer ID for consistent tracking
		normalizedPeerID := normalizePeerID(peer.ID)
		normalizedNodeID := normalizePeerID(hr.node.id)
		
		// Skip ourselves
		if normalizedPeerID == normalizedNodeID {
			continue
		}
		
		// Only initialize if not already tracking
		if _, exists := hr.peers[normalizedPeerID]; !exists {
			peerInfo := &PeerHealthInfo{
				NodeID:            normalizedPeerID,
				Status:            HealthStatusEnum.Healthy, // Start optimistically 
				InterArrivalTimes: make([]time.Duration, 0, hr.maxInterArrivalTimes),
				Zone:              peer.DataCenter, // Use datacenter as zone
				LastHeartbeat:     time.Now(),      // Set to now to avoid immediate timeout
			}
			hr.peers[normalizedPeerID] = peerInfo
			initializedCount++
		}
	}
	
	if initializedCount > 0 {
		log.Printf("📊 Initialized health registry with %d expected peers from phonebook", initializedCount)
	}
}

// SyncWithPhonebook synchronizes the health registry with current phonebook state
func (hr *HealthRegistry) SyncWithPhonebook() {
	if hr.node.phonebook == nil {
		return
	}
	
	hr.mu.Lock()
	defer hr.mu.Unlock()
	
	phonebookPeers := hr.node.phonebook.GetPeers()
	phonebookMap := make(map[string]bool)
	
	// Add new peers from phonebook
	addedCount := 0
	for _, peer := range phonebookPeers {
		// Normalize peer ID for consistent tracking
		normalizedPeerID := normalizePeerID(peer.ID)
		normalizedNodeID := normalizePeerID(hr.node.id)
		
		// Skip ourselves
		if normalizedPeerID == normalizedNodeID {
			continue
		}
		
		phonebookMap[normalizedPeerID] = true
		
		// Add if not already tracking
		if _, exists := hr.peers[normalizedPeerID]; !exists {
			peerInfo := &PeerHealthInfo{
				NodeID:            normalizedPeerID,
				Status:            HealthStatusEnum.Healthy,
				InterArrivalTimes: make([]time.Duration, 0, hr.maxInterArrivalTimes),
				Zone:              peer.DataCenter,
				LastHeartbeat:     time.Now(), // Set to now to avoid immediate timeout
			}
			hr.peers[normalizedPeerID] = peerInfo
			addedCount++
		}
	}
	
	// Remove peers no longer in phonebook (optional - may want to keep for a while)
	removedCount := 0
	for nodeID := range hr.peers {
		if !phonebookMap[nodeID] {
			// Only remove if they've been failed for a while
			if peerInfo := hr.peers[nodeID]; peerInfo != nil {
				peerInfo.mu.RLock()
				shouldRemove := peerInfo.Status == HealthStatusEnum.Failed && 
					time.Since(peerInfo.LastHeartbeat) > 5*time.Minute
				peerInfo.mu.RUnlock()
				
				if shouldRemove {
					delete(hr.peers, nodeID)
					removedCount++
				}
			}
		}
	}
	
	if addedCount > 0 || removedCount > 0 {
		log.Printf("📊 Synced health registry with phonebook: +%d peers, -%d peers (total tracked: %d)", 
			addedCount, removedCount, len(hr.peers))
	}
}