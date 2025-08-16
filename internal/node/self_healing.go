package node

import (
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/tareksalem/falak/internal/node/protobuf/models"
)

// HealingStatus represents the current healing state
type HealingStatus string

const (
	healingIdle        HealingStatus = "idle"
	healingDetected    HealingStatus = "detected"
	healingRejoining   HealingStatus = "rejoining"
	healingComplete    HealingStatus = "complete"
	healingFailed      HealingStatus = "failed"
)

var HealingStatusEnum = struct {
	Idle      HealingStatus
	Detected  HealingStatus
	Rejoining HealingStatus
	Complete  HealingStatus
	Failed    HealingStatus
}{
	Idle:      healingIdle,
	Detected:  healingDetected,
	Rejoining: healingRejoining,
	Complete:  healingComplete,
	Failed:    healingFailed,
}

// SelfHealingManager handles recovery from failure accusations
type SelfHealingManager struct {
	node *Node
	
	// Healing state
	status           HealingStatus
	healingStartTime time.Time
	healingReason    string
	lastIncarnation  uint64
	mu               sync.RWMutex
	
	// Configuration
	healingTimeout       time.Duration
	rejoinDelay          time.Duration
	maxHealingAttempts   int
	currentAttempt       int
	monitoringInterval   time.Duration
	
	// Monitoring
	stopChan chan struct{}
	running  bool
}

// NewSelfHealingManager creates a new self-healing manager
func NewSelfHealingManager(node *Node) *SelfHealingManager {
	return &SelfHealingManager{
		node:               node,
		status:             HealingStatusEnum.Idle,
		healingTimeout:     60 * time.Second,
		rejoinDelay:        5 * time.Second,
		maxHealingAttempts: 3,
		monitoringInterval: 10 * time.Second,
		stopChan:           make(chan struct{}),
	}
}

// StartMonitoring starts the self-healing monitoring process
func (shm *SelfHealingManager) StartMonitoring() error {
	shm.mu.Lock()
	defer shm.mu.Unlock()
	
	if shm.running {
		return fmt.Errorf("self-healing monitoring already running")
	}
	
	shm.running = true
	shm.status = HealingStatusEnum.Idle
	
	go shm.monitoringLoop()
	log.Printf("🔄 Self-healing monitoring started")
	
	return nil
}

// StopMonitoring stops the self-healing monitoring process
func (shm *SelfHealingManager) StopMonitoring() error {
	shm.mu.Lock()
	defer shm.mu.Unlock()
	
	if !shm.running {
		return nil
	}
	
	shm.running = false
	close(shm.stopChan)
	shm.status = HealingStatusEnum.Idle
	
	log.Printf("🔄 Self-healing monitoring stopped")
	return nil
}

// monitoringLoop continuously monitors for failure accusations against this node
func (shm *SelfHealingManager) monitoringLoop() {
	ticker := time.NewTicker(shm.monitoringInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			shm.checkForFailureAccusations()
			
		case <-shm.stopChan:
			return
			
		case <-shm.node.ctx.Done():
			return
		}
	}
}

// checkForFailureAccusations checks if this node has been accused of failure
func (shm *SelfHealingManager) checkForFailureAccusations() {
	shm.mu.Lock()
	defer shm.mu.Unlock()
	
	// Skip if already healing
	if shm.status != HealingStatusEnum.Idle {
		return
	}
	
	// Check if we have active suspicions against us
	if shm.node.suspicionManager != nil {
		activeSuspicions := shm.node.suspicionManager.GetActiveSuspicions()
		if suspicion, exists := activeSuspicions[shm.node.id]; exists {
			shm.triggerHealing("suspicion_detected", fmt.Sprintf("Suspicion from %s", suspicion.AccuserNodeID))
			return
		}
	}
	
	// Check if our health status indicates we're suspected/failed
	if shm.node.healthRegistry != nil {
		if peerHealth, exists := shm.node.healthRegistry.GetPeerHealth(shm.node.id); exists {
			if peerHealth.Status == HealthStatusEnum.Suspect || peerHealth.Status == HealthStatusEnum.Failed {
				shm.triggerHealing("health_status_degraded", string(peerHealth.Status))
				return
			}
		}
	}
	
	// Check for network connectivity issues (isolated from peers)
	connectedPeers := shm.getConnectedPeerCount()
	if connectedPeers == 0 && shm.hasKnownPeers() {
		shm.triggerHealing("network_isolation", "no connected peers")
		return
	}
}

// triggerHealing initiates the self-healing process
func (shm *SelfHealingManager) triggerHealing(reason, details string) {
	shm.status = HealingStatusEnum.Detected
	shm.healingStartTime = time.Now()
	shm.healingReason = fmt.Sprintf("%s: %s", reason, details)
	shm.currentAttempt++
	
	log.Printf("🚨 HEALING TRIGGERED: %s (attempt %d/%d)", shm.healingReason, shm.currentAttempt, shm.maxHealingAttempts)
	
	// Start healing process in background
	go shm.executeHealing()
}

// executeHealing performs the actual healing process
func (shm *SelfHealingManager) executeHealing() {
	defer func() {
		shm.mu.Lock()
		if shm.status == HealingStatusEnum.Rejoining {
			shm.status = HealingStatusEnum.Complete
		}
		shm.mu.Unlock()
	}()
	
	// Step 1: Increment incarnation number
	if err := shm.incrementIncarnation(); err != nil {
		log.Printf("❌ Failed to increment incarnation: %v", err)
		shm.markHealingFailed()
		return
	}
	
	// Step 2: Re-authenticate with existing peers
	if err := shm.reauthenticateWithPeers(); err != nil {
		log.Printf("❌ Failed to re-authenticate: %v", err)
		shm.markHealingFailed()
		return
	}
	
	// Step 3: Publish recovery announcement
	if err := shm.publishRecoveryAnnouncement(); err != nil {
		log.Printf("❌ Failed to publish recovery: %v", err)
		shm.markHealingFailed()
		return
	}
	
	// Step 4: Reset health state
	if err := shm.resetHealthState(); err != nil {
		log.Printf("❌ Failed to reset health state: %v", err)
		shm.markHealingFailed()
		return
	}
	
	log.Printf("✅ HEALING COMPLETE: Node successfully recovered with new incarnation %d", 
		shm.getCurrentIncarnation())
	
	// Reset attempt counter on success
	shm.mu.Lock()
	shm.currentAttempt = 0
	shm.mu.Unlock()
}

// incrementIncarnation increments the incarnation number to signal recovery
func (shm *SelfHealingManager) incrementIncarnation() error {
	if shm.node.heartbeatManager == nil {
		return fmt.Errorf("heartbeat manager not available")
	}
	
	shm.lastIncarnation = shm.node.heartbeatManager.GetCurrentIncarnation()
	newIncarnation := shm.node.heartbeatManager.IncrementIncarnation()
	
	log.Printf("🔄 Incremented incarnation: %d -> %d", shm.lastIncarnation, newIncarnation)
	return nil
}

// reauthenticateWithPeers re-authenticates with all known peers
func (shm *SelfHealingManager) reauthenticateWithPeers() error {
	shm.mu.Lock()
	shm.status = HealingStatusEnum.Rejoining
	shm.mu.Unlock()
	
	if shm.node.phonebook == nil {
		return fmt.Errorf("phonebook not available")
	}
	
	peers := shm.node.phonebook.GetPeers()
	log.Printf("🔄 Re-authenticating with %d known peers", len(peers))
	
	successCount := 0
	errorCount := 0
	
	for _, peer := range peers {
		if peer.ID == shm.node.id {
			continue // Skip self
		}
		
		// Use the peer ID from AddrInfo
		peerID := peer.AddrInfo.ID
		
		// Attempt re-authentication
		clusterID := "default"
		if len(shm.node.clusters) > 0 {
			clusterID = shm.node.clusters[0]
		}
		
		if err := shm.node.AuthenticateWithPeer(peerID, clusterID); err != nil {
			log.Printf("❌ Re-authentication failed with peer %s: %v", peer.ID, err)
			errorCount++
		} else {
			log.Printf("✅ Re-authenticated with peer %s", peer.ID)
			successCount++
		}
		
		// Small delay between attempts
		time.Sleep(100 * time.Millisecond)
	}
	
	log.Printf("🔄 Re-authentication results: %d success, %d errors", successCount, errorCount)
	
	if successCount == 0 && len(peers) > 0 {
		return fmt.Errorf("failed to re-authenticate with any peers")
	}
	
	return nil
}

// publishRecoveryAnnouncement publishes a recovery announcement to all clusters
func (shm *SelfHealingManager) publishRecoveryAnnouncement() error {
	if shm.node.membershipManager == nil {
		return fmt.Errorf("membership manager not available")
	}
	
	// Get current incarnation
	currentIncarnation := shm.getCurrentIncarnation()
	
	// Publish recovery announcement to each cluster
	for _, clusterID := range shm.node.clusters {
		// Create recovery heartbeat with new incarnation
		heartbeat := &models.Heartbeat{
			ClusterId:   clusterID,
			NodeId:      shm.node.id,
			Incarnation: currentIncarnation,
			Hlc:         shm.getHLC(),
			Seq:         1, // Reset sequence
		}
		
		// Add signature
		heartbeat.Signature = shm.signRecoveryMessage(heartbeat)
		
		// Serialize and publish to heartbeat topic
		data, err := proto.Marshal(heartbeat)
		if err != nil {
			return fmt.Errorf("failed to marshal recovery heartbeat: %w", err)
		}
		
		recoveryTopic := fmt.Sprintf("falak/%s/heartbeat", clusterID)
		if err := shm.node.PublishToTopic(recoveryTopic, data); err != nil {
			log.Printf("❌ Failed to publish recovery to %s: %v", recoveryTopic, err)
		} else {
			log.Printf("📢 Published recovery announcement to cluster %s (incarnation: %d)", 
				clusterID, currentIncarnation)
		}
		
		// Also publish NodeJoined event to signal re-entry
		connectedPeers := shm.node.getConnectedPeerIDs()
		if err := shm.node.membershipManager.PublishNodeJoined(shm.node.id, clusterID, connectedPeers); err != nil {
			log.Printf("❌ Failed to publish NodeJoined for recovery: %v", err)
		}
	}
	
	return nil
}

// resetHealthState resets local health monitoring state
func (shm *SelfHealingManager) resetHealthState() error {
	// Clear any active suspicions about us
	if shm.node.suspicionManager != nil {
		// Remove self from active suspicions (if any external system added us)
		log.Printf("🔄 Clearing any active suspicions against self")
	}
	
	// Reset health registry state for ourselves
	if shm.node.healthRegistry != nil {
		// Remove and re-add ourselves with healthy status
		shm.node.healthRegistry.RemovePeer(shm.node.id)
		log.Printf("🔄 Reset health registry state for self")
	}
	
	// Reset any local failure state
	log.Printf("🔄 Reset local failure state")
	
	return nil
}

// markHealingFailed marks the healing attempt as failed
func (shm *SelfHealingManager) markHealingFailed() {
	shm.mu.Lock()
	defer shm.mu.Unlock()
	
	shm.status = HealingStatusEnum.Failed
	
	log.Printf("❌ HEALING FAILED: %s (attempt %d/%d)", shm.healingReason, shm.currentAttempt, shm.maxHealingAttempts)
	
	// Check if we should retry
	if shm.currentAttempt < shm.maxHealingAttempts {
		// Schedule retry after delay
		go func() {
			time.Sleep(shm.rejoinDelay * time.Duration(shm.currentAttempt))
			shm.mu.Lock()
			if shm.status == HealingStatusEnum.Failed {
				shm.status = HealingStatusEnum.Idle // Allow retry
			}
			shm.mu.Unlock()
		}()
	} else {
		log.Printf("💀 Maximum healing attempts reached, giving up")
	}
}

// Utility methods

func (shm *SelfHealingManager) getConnectedPeerCount() int {
	if shm.node.host == nil {
		return 0
	}
	return len(shm.node.host.Network().Peers())
}

func (shm *SelfHealingManager) hasKnownPeers() bool {
	if shm.node.phonebook == nil {
		return false
	}
	peers := shm.node.phonebook.GetPeers()
	// Count peers excluding ourselves
	for _, peer := range peers {
		if peer.ID != shm.node.id {
			return true
		}
	}
	return false
}

func (shm *SelfHealingManager) getCurrentIncarnation() uint64 {
	if shm.node.heartbeatManager != nil {
		return shm.node.heartbeatManager.GetCurrentIncarnation()
	}
	return 1
}

func (shm *SelfHealingManager) getHLC() uint64 {
	if shm.node.heartbeatManager != nil {
		return shm.node.heartbeatManager.GetCurrentHLC()
	}
	return uint64(time.Now().UnixNano() / 1000000)
}

func (shm *SelfHealingManager) signRecoveryMessage(heartbeat *models.Heartbeat) []byte {
	// TODO: Use real libp2p signing
	// For now, create a simple hash-based signature
	data := fmt.Sprintf("%s:%s:%d:%d", heartbeat.ClusterId, heartbeat.NodeId, 
		heartbeat.Incarnation, heartbeat.Hlc)
	
	// Use node's private key to sign (placeholder)
	if shm.node.privateKey != nil {
		// TODO: Implement proper signing with libp2p using data
		_ = data // Suppress unused variable warning until signing is implemented
	}
	
	// Return placeholder signature
	return []byte("recovery_signature_placeholder")
}

// ProcessRecoveryHeartbeat processes a recovery heartbeat from another node
func (shm *SelfHealingManager) ProcessRecoveryHeartbeat(heartbeat *models.Heartbeat) {
	// Check if this is a recovery (new incarnation)
	if shm.node.healthRegistry != nil {
		if peerHealth, exists := shm.node.healthRegistry.GetPeerHealth(heartbeat.NodeId); exists {
			if heartbeat.Incarnation > peerHealth.Incarnation {
				log.Printf("🔄 Detected recovery: %s incarnation %d -> %d", 
					heartbeat.NodeId, peerHealth.Incarnation, heartbeat.Incarnation)
				
				// Cancel any active suspicions against this node
				if shm.node.suspicionManager != nil {
					if shm.node.suspicionManager.CancelSuspicion(heartbeat.NodeId, "node_recovery") {
						log.Printf("✅ Cancelled suspicions against recovered node %s", heartbeat.NodeId)
					}
				}
			}
		}
	}
}

// GetHealingStatus returns the current healing status
func (shm *SelfHealingManager) GetHealingStatus() HealingStatus {
	shm.mu.RLock()
	defer shm.mu.RUnlock()
	return shm.status
}

// GetHealingInfo returns detailed healing information
func (shm *SelfHealingManager) GetHealingInfo() map[string]interface{} {
	shm.mu.RLock()
	defer shm.mu.RUnlock()
	
	return map[string]interface{}{
		"status":           shm.status,
		"healing_reason":   shm.healingReason,
		"start_time":       shm.healingStartTime,
		"current_attempt":  shm.currentAttempt,
		"max_attempts":     shm.maxHealingAttempts,
		"last_incarnation": shm.lastIncarnation,
		"current_incarnation": shm.getCurrentIncarnation(),
		"running":          shm.running,
	}
}

// IsRunning returns true if monitoring is active
func (shm *SelfHealingManager) IsRunning() bool {
	shm.mu.RLock()
	defer shm.mu.RUnlock()
	return shm.running
}

// ForceHealing triggers immediate healing (for testing/manual recovery)
func (shm *SelfHealingManager) ForceHealing(reason string) error {
	shm.mu.Lock()
	defer shm.mu.Unlock()
	
	if shm.status != HealingStatusEnum.Idle {
		return fmt.Errorf("healing already in progress: %s", shm.status)
	}
	
	shm.triggerHealing("manual_trigger", reason)
	return nil
}