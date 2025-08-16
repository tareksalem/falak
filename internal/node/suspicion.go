package node

import (
	"crypto/sha256"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/tareksalem/falak/internal/node/protobuf/models"
)

// SuspicionReason represents why a node is suspected
type SuspicionReason string

const (
	reasonHeartbeatTimeout SuspicionReason = "heartbeat_timeout"
	reasonDirectPingFail   SuspicionReason = "direct_ping_fail"
	reasonIndirectProbeFail SuspicionReason = "indirect_probe_fail"
	reasonPhiThreshold     SuspicionReason = "phi_threshold"
)

var SuspicionReasonEnum = struct {
	HeartbeatTimeout  SuspicionReason
	DirectPingFail    SuspicionReason
	IndirectProbeFail SuspicionReason
	PhiThreshold      SuspicionReason
}{
	HeartbeatTimeout:  reasonHeartbeatTimeout,
	DirectPingFail:    reasonDirectPingFail,
	IndirectProbeFail: reasonIndirectProbeFail,
	PhiThreshold:      reasonPhiThreshold,
}

// SuspicionManager handles suspicion and refutation processing
type SuspicionManager struct {
	node *Node
	
	// Active suspicions by node ID
	activeSuspicions map[string]*ActiveSuspicion
	mu               sync.RWMutex
	
	// Configuration
	suspicionTimeout  time.Duration
	refutationWindow  time.Duration
	maxSuspicions     int
}

// ActiveSuspicion tracks an ongoing suspicion against a peer
type ActiveSuspicion struct {
	TargetNodeID    string
	AccuserNodeID   string
	Reason          SuspicionReason
	Phi             float64
	Witnesses       []*models.Witness
	StartTime       time.Time
	LastUpdate      time.Time
	RefutationCount int
	ClusterID       string
	mu              sync.RWMutex
}

// NewSuspicionManager creates a new suspicion manager
func NewSuspicionManager(node *Node) *SuspicionManager {
	return &SuspicionManager{
		node:              node,
		activeSuspicions:  make(map[string]*ActiveSuspicion),
		suspicionTimeout:  30 * time.Second,
		refutationWindow:  10 * time.Second,
		maxSuspicions:     100,
	}
}


// PublishSuspicion publishes a suspicion message for a failed peer
func (sm *SuspicionManager) PublishSuspicion(targetNodeID string, reason SuspicionReason, phi float64, witnesses []*models.Witness, clusterID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	
	// Normalize target node ID for consistent tracking
	normalizedTargetID := normalizePeerID(targetNodeID)
	
	// Check if we already have an active suspicion for this peer
	if existing, exists := sm.activeSuspicions[normalizedTargetID]; exists {
		existing.mu.Lock()
		existing.LastUpdate = time.Now()
		existing.Phi = phi
		if len(witnesses) > 0 {
			existing.Witnesses = witnesses
		}
		existing.mu.Unlock()
		log.Printf("📢 Updated existing suspicion for %s (phi: %.2f)", targetNodeID, phi)
		return nil
	}
	
	// Create new suspicion
	suspicion := &ActiveSuspicion{
		TargetNodeID:  normalizedTargetID,
		AccuserNodeID: sm.node.id,
		Reason:        reason,
		Phi:           phi,
		Witnesses:     witnesses,
		StartTime:     time.Now(),
		LastUpdate:    time.Now(),
		ClusterID:     clusterID,
	}
	
	// Add to active suspicions
	sm.activeSuspicions[normalizedTargetID] = suspicion
	
	// Create suspicion message
	suspicionMsg := &models.Suspicion{
		ClusterId:         clusterID,
		TargetNodeId:      normalizedTargetID,
		AccuserNodeId:     sm.node.id,
		AccuserIncarnation: sm.getCurrentIncarnation(),
		Hlc:               sm.getHLC(),
		Phi:               float32(phi),
		Witnesses:         witnesses,
		Signature:         []byte{}, // Will be set by signing
	}
	
	// Sign the suspicion message
	suspicionMsg.Signature = sm.signSuspicionMessage(suspicionMsg)
	
	// Serialize and publish
	data, err := proto.Marshal(suspicionMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal suspicion message: %w", err)
	}
	
	// Publish to suspicion topic
	suspicionTopic := fmt.Sprintf("falak/%s/suspicion", clusterID)
	if err := sm.node.PublishToTopic(suspicionTopic, data); err != nil {
		return fmt.Errorf("failed to publish suspicion: %w", err)
	}
	
	log.Printf("📢 Published suspicion for %s (reason: %s, phi: %.2f, witnesses: %d)", 
		normalizedTargetID, reason, phi, len(witnesses))
	
	// Trigger quorum evaluation
	if sm.node.quorumManager != nil {
		if err := sm.node.quorumManager.ProcessSuspicionForQuorum(suspicionMsg); err != nil {
			log.Printf("❌ Failed to process suspicion for quorum: %v", err)
		}
	}
	
	// Start timer for suspicion timeout
	go sm.handleSuspicionTimeout(normalizedTargetID)
	
	return nil
}

// ProcessSuspicionMessage processes an incoming suspicion message
func (sm *SuspicionManager) ProcessSuspicionMessage(data []byte) error {
	var suspicion models.Suspicion
	if err := proto.Unmarshal(data, &suspicion); err != nil {
		return fmt.Errorf("failed to unmarshal suspicion message: %w", err)
	}
	
	// Verify signature
	if !sm.verifySuspicionSignature(&suspicion) {
		log.Printf("❌ Invalid suspicion signature from %s", suspicion.AccuserNodeId)
		return fmt.Errorf("invalid suspicion signature")
	}
	
	// Normalize IDs for consistent comparison
	normalizedTargetID := normalizePeerID(suspicion.TargetNodeId)
	normalizedAccuserID := normalizePeerID(suspicion.AccuserNodeId)
	normalizedNodeID := normalizePeerID(sm.node.id)
	
	log.Printf("📢 Received suspicion: %s accused %s (phi: %.2f)", 
		normalizedAccuserID, normalizedTargetID, suspicion.Phi)
	
	// If this suspicion is about us, we should refute it
	if normalizedTargetID == normalizedNodeID {
		log.Printf("🛡️ Suspicion is about us, publishing refutation")
		return sm.PublishRefutation(normalizedAccuserID, suspicion.ClusterId)
	}
	
	// If we have health information about the target, check if we agree
	if sm.node.healthRegistry != nil {
		if peerHealth, exists := sm.node.healthRegistry.GetPeerHealth(normalizedTargetID); exists {
			if peerHealth.Status == HealthStatusEnum.Healthy && peerHealth.Phi < 3.0 {
				// We believe the peer is healthy, send refutation
				log.Printf("🛡️ We believe %s is healthy, publishing refutation", normalizedTargetID)
				return sm.PublishRefutation(normalizedAccuserID, suspicion.ClusterId)
			}
		}
	}
	
	// Update our health registry with the suspicion
	sm.updateHealthFromSuspicion(&suspicion)
	
	// Trigger quorum evaluation for incoming suspicions
	if sm.node.quorumManager != nil {
		if err := sm.node.quorumManager.ProcessSuspicionForQuorum(&suspicion); err != nil {
			log.Printf("❌ Failed to process incoming suspicion for quorum: %v", err)
		}
	}
	
	return nil
}

// PublishRefutation publishes a refutation message
func (sm *SuspicionManager) PublishRefutation(accuserNodeID, clusterID string) error {
	// Get recent proof of communication (e.g., recent heartbeat ack)
	recentAck := sm.getRecentCommunicationProof()
	
	refutation := &models.Refutation{
		ClusterId:         clusterID,
		TargetNodeId:      sm.node.id,
		RefuterNodeId:     sm.node.id,
		TargetIncarnation: sm.getCurrentIncarnation(),
		Hlc:               sm.getHLC(),
		RecentAck:         recentAck,
		Signature:         []byte{}, // Will be set by signing
	}
	
	// Sign the refutation message
	refutation.Signature = sm.signRefutationMessage(refutation)
	
	// Serialize and publish
	data, err := proto.Marshal(refutation)
	if err != nil {
		return fmt.Errorf("failed to marshal refutation message: %w", err)
	}
	
	// Publish to refutation topic
	refutationTopic := fmt.Sprintf("falak/%s/refutation", clusterID)
	if err := sm.node.PublishToTopic(refutationTopic, data); err != nil {
		return fmt.Errorf("failed to publish refutation: %w", err)
	}
	
	log.Printf("🛡️ Published refutation against accusation from %s", accuserNodeID)
	return nil
}

// ProcessRefutationMessage processes an incoming refutation message
func (sm *SuspicionManager) ProcessRefutationMessage(data []byte) error {
	var refutation models.Refutation
	if err := proto.Unmarshal(data, &refutation); err != nil {
		return fmt.Errorf("failed to unmarshal refutation message: %w", err)
	}
	
	// Verify signature
	if !sm.verifyRefutationSignature(&refutation) {
		log.Printf("❌ Invalid refutation signature from %s", refutation.RefuterNodeId)
		return fmt.Errorf("invalid refutation signature")
	}
	
	log.Printf("🛡️ Received refutation: %s refutes suspicion of %s", 
		refutation.RefuterNodeId, refutation.TargetNodeId)
	
	// Check if we have an active suspicion for this target
	sm.mu.Lock()
	defer sm.mu.Unlock()
	
	if suspicion, exists := sm.activeSuspicions[refutation.TargetNodeId]; exists {
		suspicion.mu.Lock()
		suspicion.RefutationCount++
		suspicion.mu.Unlock()
		
		log.Printf("🛡️ Refutation count for %s: %d", refutation.TargetNodeId, suspicion.RefutationCount)
		
		// If we get enough refutations, cancel the suspicion
		if suspicion.RefutationCount >= 2 {
			delete(sm.activeSuspicions, refutation.TargetNodeId)
			log.Printf("✅ Suspicion cancelled for %s due to refutations", refutation.TargetNodeId)
			
			// Update health registry to healthy if we have the peer
			if sm.node.healthRegistry != nil {
				if _, exists := sm.node.healthRegistry.GetPeerHealth(refutation.TargetNodeId); exists {
					// Reset phi score and mark as healthy
					sm.node.healthRegistry.ProcessHeartbeat(&models.Heartbeat{
						NodeId:      refutation.TargetNodeId,
						Incarnation: refutation.TargetIncarnation,
						Hlc:         refutation.Hlc,
					})
				}
			}
		}
	}
	
	return nil
}

// handleSuspicionTimeout handles timeouts for active suspicions
func (sm *SuspicionManager) handleSuspicionTimeout(targetNodeID string) {
	timer := time.NewTimer(sm.suspicionTimeout)
	defer timer.Stop()
	
	select {
	case <-timer.C:
		sm.mu.Lock()
		suspicion, exists := sm.activeSuspicions[targetNodeID]
		if !exists {
			sm.mu.Unlock()
			return
		}
		
		// Check if suspicion is still active and not refuted
		suspicion.mu.RLock()
		refutationCount := suspicion.RefutationCount
		clusterID := suspicion.ClusterID
		suspicion.mu.RUnlock()
		
		if refutationCount < 2 {
			// Suspicion timeout reached, escalate to failure
			delete(sm.activeSuspicions, targetNodeID)
			sm.mu.Unlock()
			
			log.Printf("⏰ Suspicion timeout for %s, escalating to failure", targetNodeID)
			
			// Update health registry to mark as failed
			if sm.node.healthRegistry != nil {
				sm.escalateToFailure(targetNodeID, clusterID)
			}
		} else {
			sm.mu.Unlock()
		}
		
	case <-sm.node.ctx.Done():
		return
	}
}

// escalateToFailure escalates a suspicion to failure status
func (sm *SuspicionManager) escalateToFailure(targetNodeID, clusterID string) {
	log.Printf("❌ Escalating %s to FAILED status", targetNodeID)
	
	// Force failure status in health registry
	if sm.node.healthRegistry != nil {
		if sm.node.healthRegistry.ForceFailureStatus(targetNodeID) {
			log.Printf("💀 Successfully forced %s to FAILED status", targetNodeID)
		}
	}
	
	// Note: Final failure announcements are handled by QuorumManager
	log.Printf("📢 Suspicion escalated to failure, awaiting quorum consensus for %s", targetNodeID)
}

// updateHealthFromSuspicion updates health registry based on received suspicion
func (sm *SuspicionManager) updateHealthFromSuspicion(suspicion *models.Suspicion) {
	if sm.node.healthRegistry == nil {
		return
	}
	
	// Normalize target ID for consistent health tracking
	normalizedTargetID := normalizePeerID(suspicion.TargetNodeId)
	
	// If we have health info for the suspected peer, increase phi score
	if peerHealth, exists := sm.node.healthRegistry.GetPeerHealth(normalizedTargetID); exists {
		log.Printf("📊 Updating health for %s based on suspicion (current phi: %.2f, accused phi: %.2f)", 
			normalizedTargetID, peerHealth.Phi, suspicion.Phi)
		
		// If the accuser's phi score is higher than ours, adopt it
		if float64(suspicion.Phi) > peerHealth.Phi {
			// Update phi score in health registry
			if sm.node.healthRegistry.UpdatePeerPhi(normalizedTargetID, float64(suspicion.Phi)) {
				log.Printf("📊 Updated phi score for %s to %.2f based on suspicion", normalizedTargetID, suspicion.Phi)
			}
		}
	}
}

// Utility methods

func (sm *SuspicionManager) getCurrentIncarnation() uint64 {
	if sm.node.heartbeatManager != nil {
		return sm.node.heartbeatManager.GetCurrentIncarnation()
	}
	return 1
}

func (sm *SuspicionManager) getHLC() uint64 {
	if sm.node.heartbeatManager != nil {
		return sm.node.heartbeatManager.GetCurrentHLC()
	}
	return uint64(time.Now().UnixNano() / 1000000)
}

func (sm *SuspicionManager) getRecentCommunicationProof() []byte {
	// TODO: Implement recent communication proof (e.g., hash of recent heartbeat)
	// For now, return a simple timestamp proof
	timestamp := time.Now().Unix()
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", sm.node.id, timestamp)))
	return hash[:]
}

func (sm *SuspicionManager) signSuspicionMessage(suspicion *models.Suspicion) []byte {
	// TODO: Use real libp2p signing
	data := fmt.Sprintf("%s:%s:%s:%d:%.2f", 
		suspicion.ClusterId, suspicion.TargetNodeId, suspicion.AccuserNodeId, 
		suspicion.Hlc, suspicion.Phi)
	hash := sha256.Sum256([]byte(data))
	return hash[:]
}

func (sm *SuspicionManager) signRefutationMessage(refutation *models.Refutation) []byte {
	// TODO: Use real libp2p signing
	data := fmt.Sprintf("%s:%s:%s:%d", 
		refutation.ClusterId, refutation.TargetNodeId, refutation.RefuterNodeId, refutation.Hlc)
	hash := sha256.Sum256([]byte(data))
	return hash[:]
}

func (sm *SuspicionManager) verifySuspicionSignature(suspicion *models.Suspicion) bool {
	// TODO: Implement real signature verification
	return len(suspicion.Signature) == 32
}

func (sm *SuspicionManager) verifyRefutationSignature(refutation *models.Refutation) bool {
	// TODO: Implement real signature verification
	return len(refutation.Signature) == 32
}

// GetActiveSuspicions returns a copy of all active suspicions
func (sm *SuspicionManager) GetActiveSuspicions() map[string]*ActiveSuspicion {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	
	result := make(map[string]*ActiveSuspicion)
	for nodeID, suspicion := range sm.activeSuspicions {
		suspicion.mu.RLock()
		result[nodeID] = &ActiveSuspicion{
			TargetNodeID:    suspicion.TargetNodeID,
			AccuserNodeID:   suspicion.AccuserNodeID,
			Reason:          suspicion.Reason,
			Phi:             suspicion.Phi,
			Witnesses:       suspicion.Witnesses,
			StartTime:       suspicion.StartTime,
			LastUpdate:      suspicion.LastUpdate,
			RefutationCount: suspicion.RefutationCount,
			ClusterID:       suspicion.ClusterID,
		}
		suspicion.mu.RUnlock()
	}
	
	return result
}

// CancelSuspicion cancels an active suspicion for a recovered node
func (sm *SuspicionManager) CancelSuspicion(nodeID string, reason string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	
	if _, exists := sm.activeSuspicions[nodeID]; exists {
		delete(sm.activeSuspicions, nodeID)
		log.Printf("✅ Cancelled suspicion for %s (reason: %s)", nodeID, reason)
		return true
	}
	
	return false
}

// CleanupExpiredSuspicions removes old suspicions that have expired
func (sm *SuspicionManager) CleanupExpiredSuspicions() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	
	now := time.Now()
	for nodeID, suspicion := range sm.activeSuspicions {
		suspicion.mu.RLock()
		age := now.Sub(suspicion.StartTime)
		suspicion.mu.RUnlock()
		
		if age > sm.suspicionTimeout*2 {
			delete(sm.activeSuspicions, nodeID)
			log.Printf("🧹 Cleaned up expired suspicion for %s", nodeID)
		}
	}
}