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

// QuorumDecision represents the result of a quorum evaluation
type QuorumDecision string

const (
	quorumPending    QuorumDecision = "pending"
	quorumReached    QuorumDecision = "reached"
	quorumRejected   QuorumDecision = "rejected"
	quorumTimeout    QuorumDecision = "timeout"
)

var QuorumDecisionEnum = struct {
	Pending  QuorumDecision
	Reached  QuorumDecision
	Rejected QuorumDecision
	Timeout  QuorumDecision
}{
	Pending:  quorumPending,
	Reached:  quorumReached,
	Rejected: quorumRejected,
	Timeout:  quorumTimeout,
}

// QuorumManager handles distributed consensus for failure detection
type QuorumManager struct {
	node *Node
	
	// Active quorum evaluations by target node ID
	activeQuorums map[string]*QuorumEvaluation
	mu            sync.RWMutex
	
	// Configuration
	minAccusers         int           // Minimum number of accusers needed
	minZones           int           // Minimum number of different zones required
	quorumTimeout      time.Duration // How long to wait for consensus
	maxActiveQuorums   int           // Maximum concurrent evaluations
	trustThreshold     float64       // Minimum trust score to count votes
}

// QuorumEvaluation tracks an ongoing consensus evaluation
type QuorumEvaluation struct {
	TargetNodeID    string
	ClusterID       string
	StartTime       time.Time
	Accusations     map[string]*AccusationVote // accuser node ID -> vote
	ZoneCount       map[string]int             // zone -> accusation count
	Decision        QuorumDecision
	QuorumReached   bool
	mu              sync.RWMutex
}

// AccusationVote represents a single node's vote for failure
type AccusationVote struct {
	AccuserNodeID   string
	AccuserZone     string
	Phi             float64
	Witnesses       []*models.Witness
	TrustScore      float64
	Timestamp       time.Time
	Signature       []byte
}

// NewQuorumManager creates a new quorum manager
func NewQuorumManager(node *Node) *QuorumManager {
	return &QuorumManager{
		node:             node,
		activeQuorums:    make(map[string]*QuorumEvaluation),
		minAccusers:      3,  // Need at least 3 nodes to agree
		minZones:         2,  // Need accusers from at least 2 zones
		quorumTimeout:    15 * time.Second,
		maxActiveQuorums: 50,
		trustThreshold:   0.5, // Only count votes from trusted nodes
	}
}

// ProcessSuspicionForQuorum processes a suspicion message for quorum evaluation
func (qm *QuorumManager) ProcessSuspicionForQuorum(suspicion *models.Suspicion) error {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	
	targetNodeID := suspicion.TargetNodeId
	
	// Get or create quorum evaluation
	evaluation, exists := qm.activeQuorums[targetNodeID]
	if !exists {
		// Check if we have room for new evaluations
		if len(qm.activeQuorums) >= qm.maxActiveQuorums {
			log.Printf("⚖️ Max active quorums reached, ignoring suspicion for %s", targetNodeID)
			return nil
		}
		
		evaluation = &QuorumEvaluation{
			TargetNodeID: targetNodeID,
			ClusterID:    suspicion.ClusterId,
			StartTime:    time.Now(),
			Accusations:  make(map[string]*AccusationVote),
			ZoneCount:    make(map[string]int),
			Decision:     QuorumDecisionEnum.Pending,
		}
		qm.activeQuorums[targetNodeID] = evaluation
		
		// Start timeout timer
		go qm.handleQuorumTimeout(targetNodeID)
		
		log.Printf("⚖️ Started quorum evaluation for %s", targetNodeID)
	}
	
	// Add accusation vote
	return qm.addAccusationVote(evaluation, suspicion)
}

// addAccusationVote adds a suspicion as an accusation vote
func (qm *QuorumManager) addAccusationVote(evaluation *QuorumEvaluation, suspicion *models.Suspicion) error {
	evaluation.mu.Lock()
	defer evaluation.mu.Unlock()
	
	accuserNodeID := suspicion.AccuserNodeId
	
	// Check if already voted
	if _, exists := evaluation.Accusations[accuserNodeID]; exists {
		log.Printf("⚖️ Node %s already voted in quorum for %s", accuserNodeID, evaluation.TargetNodeID)
		return nil
	}
	
	// Get accuser's zone and trust score
	accuserZone := qm.getNodeZone(accuserNodeID)
	trustScore := qm.getNodeTrustScore(accuserNodeID)
	
	// Check trust threshold
	if trustScore < qm.trustThreshold {
		log.Printf("⚖️ Ignoring vote from untrusted node %s (trust: %.2f)", accuserNodeID, trustScore)
		return nil
	}
	
	// Create accusation vote
	vote := &AccusationVote{
		AccuserNodeID: accuserNodeID,
		AccuserZone:   accuserZone,
		Phi:           float64(suspicion.Phi),
		Witnesses:     suspicion.Witnesses,
		TrustScore:    trustScore,
		Timestamp:     time.Now(),
		Signature:     suspicion.Signature,
	}
	
	// Add vote
	evaluation.Accusations[accuserNodeID] = vote
	evaluation.ZoneCount[accuserZone]++
	
	log.Printf("⚖️ Added accusation vote: %s (zone: %s, phi: %.2f, trust: %.2f)", 
		accuserNodeID, accuserZone, vote.Phi, trustScore)
	
	// Check if quorum is reached
	return qm.evaluateQuorum(evaluation)
}

// evaluateQuorum checks if consensus has been reached
func (qm *QuorumManager) evaluateQuorum(evaluation *QuorumEvaluation) error {
	// Count valid accusations
	validAccusers := len(evaluation.Accusations)
	validZones := len(evaluation.ZoneCount)
	
	log.Printf("⚖️ Quorum status for %s: %d accusers from %d zones (need %d accusers from %d zones)", 
		evaluation.TargetNodeID, validAccusers, validZones, qm.minAccusers, qm.minZones)
	
	// Check if quorum requirements are met
	if validAccusers >= qm.minAccusers && validZones >= qm.minZones {
		// Quorum reached!
		evaluation.Decision = QuorumDecisionEnum.Reached
		evaluation.QuorumReached = true
		
		log.Printf("✅ Quorum REACHED for %s (%d accusers from %d zones)", 
			evaluation.TargetNodeID, validAccusers, validZones)
		
		// Publish quorum verdict
		return qm.publishQuorumVerdict(evaluation)
	}
	
	return nil
}

// publishQuorumVerdict publishes a quorum verdict message
func (qm *QuorumManager) publishQuorumVerdict(evaluation *QuorumEvaluation) error {
	// Collect accuser information
	var accuserIDs []string
	var accuserZones []string
	var accuserSigs [][]byte
	
	for _, vote := range evaluation.Accusations {
		accuserIDs = append(accuserIDs, vote.AccuserNodeID)
		accuserZones = append(accuserZones, vote.AccuserZone)
		accuserSigs = append(accuserSigs, vote.Signature)
	}
	
	// Create quorum verdict
	verdict := &models.QuorumVerdict{
		ClusterId:         evaluation.ClusterID,
		TargetNodeId:      evaluation.TargetNodeID,
		Verdict:           "failed",
		Hlc:               qm.getHLC(),
		TargetIncarnation: qm.getTargetIncarnation(evaluation.TargetNodeID),
		AccuserIds:        accuserIDs,
		AccuserZones:      accuserZones,
		AccuserSigs:       accuserSigs,
		QuorumAggSig:      qm.generateAggregateSignature(evaluation),
	}
	
	// Serialize and publish
	data, err := proto.Marshal(verdict)
	if err != nil {
		return fmt.Errorf("failed to marshal quorum verdict: %w", err)
	}
	
	// Publish to quorum topic
	quorumTopic := fmt.Sprintf("falak/%s/quorum", evaluation.ClusterID)
	if err := qm.node.PublishToTopic(quorumTopic, data); err != nil {
		return fmt.Errorf("failed to publish quorum verdict: %w", err)
	}
	
	log.Printf("⚖️ Published quorum verdict for %s with %d accusers", 
		evaluation.TargetNodeID, len(accuserIDs))
	
	// Trigger failure announcement
	return qm.publishFailureAnnouncement(evaluation, verdict)
}

// publishFailureAnnouncement publishes the final failure announcement
func (qm *QuorumManager) publishFailureAnnouncement(evaluation *QuorumEvaluation, verdict *models.QuorumVerdict) error {
	// Create failure announcement
	announcement := &models.FailureAnnouncement{
		ClusterId:         evaluation.ClusterID,
		TargetNodeId:      evaluation.TargetNodeID,
		Hlc:               qm.getHLC(),
		TargetIncarnation: verdict.TargetIncarnation,
		QuorumRef:         qm.generateQuorumReference(verdict),
		AnnouncerSig:      qm.signFailureAnnouncement(evaluation.TargetNodeID, evaluation.ClusterID),
	}
	
	// Serialize and publish
	data, err := proto.Marshal(announcement)
	if err != nil {
		return fmt.Errorf("failed to marshal failure announcement: %w", err)
	}
	
	// Publish to failure topic
	failureTopic := fmt.Sprintf("falak/%s/failure", evaluation.ClusterID)
	if err := qm.node.PublishToTopic(failureTopic, data); err != nil {
		return fmt.Errorf("failed to publish failure announcement: %w", err)
	}
	
	log.Printf("📢 Published FAILURE ANNOUNCEMENT for %s", evaluation.TargetNodeID)
	
	// Force failure status in local health registry
	if qm.node.healthRegistry != nil {
		qm.node.healthRegistry.ForceFailureStatus(evaluation.TargetNodeID)
	}
	
	// Remove from active quorums
	qm.mu.Lock()
	delete(qm.activeQuorums, evaluation.TargetNodeID)
	qm.mu.Unlock()
	
	return nil
}

// ProcessQuorumVerdict processes an incoming quorum verdict
func (qm *QuorumManager) ProcessQuorumVerdict(data []byte) error {
	var verdict models.QuorumVerdict
	if err := proto.Unmarshal(data, &verdict); err != nil {
		return fmt.Errorf("failed to unmarshal quorum verdict: %w", err)
	}
	
	log.Printf("⚖️ Received quorum verdict: %s declared %s as %s with %d accusers", 
		"cluster", verdict.TargetNodeId, verdict.Verdict, len(verdict.AccuserIds))
	
	// Verify the verdict (basic validation)
	if !qm.verifyQuorumVerdict(&verdict) {
		log.Printf("❌ Invalid quorum verdict for %s", verdict.TargetNodeId)
		return fmt.Errorf("invalid quorum verdict")
	}
	
	// If this verdict is about a peer we know, update our health registry
	if qm.node.healthRegistry != nil {
		if verdict.Verdict == "failed" {
			qm.node.healthRegistry.ForceFailureStatus(verdict.TargetNodeId)
			log.Printf("💀 Applied quorum verdict: marked %s as FAILED", verdict.TargetNodeId)
		}
	}
	
	return nil
}

// ProcessFailureAnnouncement processes an incoming failure announcement
func (qm *QuorumManager) ProcessFailureAnnouncement(data []byte) error {
	var announcement models.FailureAnnouncement
	if err := proto.Unmarshal(data, &announcement); err != nil {
		return fmt.Errorf("failed to unmarshal failure announcement: %w", err)
	}
	
	log.Printf("📢 Received FAILURE ANNOUNCEMENT for %s", announcement.TargetNodeId)
	
	// Verify the announcement
	if !qm.verifyFailureAnnouncement(&announcement) {
		log.Printf("❌ Invalid failure announcement for %s", announcement.TargetNodeId)
		return fmt.Errorf("invalid failure announcement")
	}
	
	// Apply failure status locally
	if qm.node.healthRegistry != nil {
		qm.node.healthRegistry.ForceFailureStatus(announcement.TargetNodeId)
		log.Printf("💀 Applied failure announcement: marked %s as FAILED", announcement.TargetNodeId)
	}
	
	// Remove from phonebook if configured to do so
	// TODO: Add configuration for automatic phonebook cleanup
	
	return nil
}

// handleQuorumTimeout handles timeout for quorum evaluations
func (qm *QuorumManager) handleQuorumTimeout(targetNodeID string) {
	timer := time.NewTimer(qm.quorumTimeout)
	defer timer.Stop()
	
	select {
	case <-timer.C:
		qm.mu.Lock()
		evaluation, exists := qm.activeQuorums[targetNodeID]
		if !exists {
			qm.mu.Unlock()
			return
		}
		
		evaluation.mu.Lock()
		if !evaluation.QuorumReached {
			evaluation.Decision = QuorumDecisionEnum.Timeout
			accuserCount := len(evaluation.Accusations)
			zoneCount := len(evaluation.ZoneCount)
			
			log.Printf("⏰ Quorum TIMEOUT for %s (%d accusers from %d zones - insufficient)", 
				targetNodeID, accuserCount, zoneCount)
			
			// Remove from active quorums
			delete(qm.activeQuorums, targetNodeID)
		}
		evaluation.mu.Unlock()
		qm.mu.Unlock()
		
	case <-qm.node.ctx.Done():
		return
	}
}

// Utility methods

func (qm *QuorumManager) getNodeZone(nodeID string) string {
	// Try to get zone from phonebook
	if qm.node.phonebook != nil {
		if peer, exists := qm.node.phonebook.GetPeer(nodeID); exists {
			if peer.DataCenter != "" {
				return peer.DataCenter
			}
		}
	}
	
	// Try to get from health registry
	if qm.node.healthRegistry != nil {
		if peerHealth, exists := qm.node.healthRegistry.GetPeerHealth(nodeID); exists {
			if peerHealth.Zone != "" {
				return peerHealth.Zone
			}
		}
	}
	
	// Default zone
	return "default"
}

func (qm *QuorumManager) getNodeTrustScore(nodeID string) float64 {
	// Try to get trust score from phonebook
	if qm.node.phonebook != nil {
		if peer, exists := qm.node.phonebook.GetPeer(nodeID); exists {
			return peer.TrustScore
		}
	}
	
	// Default trust score for unknown nodes
	return 0.7
}

func (qm *QuorumManager) getTargetIncarnation(targetNodeID string) uint64 {
	// Try to get incarnation from health registry
	if qm.node.healthRegistry != nil {
		if peerHealth, exists := qm.node.healthRegistry.GetPeerHealth(targetNodeID); exists {
			return peerHealth.Incarnation
		}
	}
	
	// Default incarnation
	return 1
}

func (qm *QuorumManager) getHLC() uint64 {
	if qm.node.heartbeatManager != nil {
		return qm.node.heartbeatManager.GetCurrentHLC()
	}
	return uint64(time.Now().UnixNano() / 1000000)
}

func (qm *QuorumManager) generateAggregateSignature(evaluation *QuorumEvaluation) []byte {
	// TODO: Implement proper aggregate signature
	// For now, create a hash of all signatures
	var allSigs []byte
	for _, vote := range evaluation.Accusations {
		allSigs = append(allSigs, vote.Signature...)
	}
	hash := sha256.Sum256(allSigs)
	return hash[:]
}

func (qm *QuorumManager) generateQuorumReference(verdict *models.QuorumVerdict) []byte {
	// Create a hash reference to the quorum verdict
	data, _ := proto.Marshal(verdict)
	hash := sha256.Sum256(data)
	return hash[:]
}

func (qm *QuorumManager) signFailureAnnouncement(targetNodeID, clusterID string) []byte {
	// TODO: Use real libp2p signing
	data := fmt.Sprintf("%s:%s:%s:%d", clusterID, targetNodeID, qm.node.id, qm.getHLC())
	hash := sha256.Sum256([]byte(data))
	return hash[:]
}

func (qm *QuorumManager) verifyQuorumVerdict(verdict *models.QuorumVerdict) bool {
	// Basic validation
	if len(verdict.AccuserIds) < qm.minAccusers {
		return false
	}
	
	// Check zone diversity
	zoneCount := make(map[string]bool)
	for _, zone := range verdict.AccuserZones {
		zoneCount[zone] = true
	}
	
	if len(zoneCount) < qm.minZones {
		return false
	}
	
	// TODO: Verify aggregate signature
	return len(verdict.QuorumAggSig) == 32
}

func (qm *QuorumManager) verifyFailureAnnouncement(announcement *models.FailureAnnouncement) bool {
	// TODO: Implement proper signature verification
	return len(announcement.AnnouncerSig) == 32
}

// GetActiveQuorums returns information about ongoing quorum evaluations
func (qm *QuorumManager) GetActiveQuorums() map[string]*QuorumEvaluation {
	qm.mu.RLock()
	defer qm.mu.RUnlock()
	
	result := make(map[string]*QuorumEvaluation)
	for nodeID, evaluation := range qm.activeQuorums {
		evaluation.mu.RLock()
		result[nodeID] = &QuorumEvaluation{
			TargetNodeID:  evaluation.TargetNodeID,
			ClusterID:     evaluation.ClusterID,
			StartTime:     evaluation.StartTime,
			Decision:      evaluation.Decision,
			QuorumReached: evaluation.QuorumReached,
			// Don't copy Accusations map to avoid deep copy complexity
		}
		evaluation.mu.RUnlock()
	}
	
	return result
}

// CleanupExpiredQuorums removes old quorum evaluations
func (qm *QuorumManager) CleanupExpiredQuorums() {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	
	now := time.Now()
	for nodeID, evaluation := range qm.activeQuorums {
		evaluation.mu.RLock()
		age := now.Sub(evaluation.StartTime)
		evaluation.mu.RUnlock()
		
		if age > qm.quorumTimeout*2 {
			delete(qm.activeQuorums, nodeID)
			log.Printf("🧹 Cleaned up expired quorum evaluation for %s", nodeID)
		}
	}
}