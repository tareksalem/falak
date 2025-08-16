package node

import (
	"crypto/sha256"
	"fmt"
	"log"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/tareksalem/falak/internal/node/protobuf/models"
)

// MembershipManager handles cluster membership events
type MembershipManager struct {
	node *Node
}

// NewMembershipManager creates a new membership manager
func NewMembershipManager(node *Node) *MembershipManager {
	return &MembershipManager{
		node: node,
	}
}

// PublishNodeJoined publishes a NodeJoined event after successful authentication
func (mm *MembershipManager) PublishNodeJoined(peerID, clusterID string, connectedPeers []string) error {
	log.Printf("📢 Publishing NodeJoined event for peer %s in cluster %s", peerID, clusterID)
	
	// Create PeerInfo for the joining node
	peerInfo := &models.PeerInfo{
		NodeId:     mm.node.id,
		Multiaddrs: mm.getNodeMultiaddrs(),
		Labels:     mm.getNodeLabels(),
		NodeStatus: models.NodeStatus_ACTIVE,
	}
	
	// Create NodeJoined event
	nodeJoined := &models.NodeJoined{
		Peer:        peerInfo,
		ConnectedTo: connectedPeers,
		Incarnation: mm.getIncarnationNumber(),
	}
	
	// Create signed membership message
	membershipMsg, err := mm.createSignedMembershipMessage(nodeJoined)
	if err != nil {
		return fmt.Errorf("failed to create signed membership message: %w", err)
	}
	
	// Publish to cluster-specific joined topic
	topicName := fmt.Sprintf("falak/%s/members/joined", clusterID)
	messageData, err := proto.Marshal(membershipMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal membership message: %w", err)
	}
	
	err = mm.node.PublishToTopic(topicName, messageData)
	if err != nil {
		return fmt.Errorf("failed to publish NodeJoined event: %w", err)
	}
	
	log.Printf("✅ Successfully published NodeJoined event to topic: %s", topicName)
	return nil
}

// PublishNodeLeft publishes a NodeLeft event when gracefully leaving
func (mm *MembershipManager) PublishNodeLeft(clusterID, reason string) error {
	log.Printf("📢 Publishing NodeLeft event for cluster %s (reason: %s)", clusterID, reason)
	
	// Create NodeLeft event
	nodeLeft := &models.NodeLeft{
		NodeId:      mm.node.id,
		Reason:      reason,
		Incarnation: mm.getIncarnationNumber(),
	}
	
	// Create signed membership message
	membershipMsg, err := mm.createSignedMembershipMessage(nodeLeft)
	if err != nil {
		return fmt.Errorf("failed to create signed membership message: %w", err)
	}
	
	// Publish to cluster-specific membership topic
	topicName := fmt.Sprintf("falak/%s/members/membership", clusterID)
	messageData, err := proto.Marshal(membershipMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal membership message: %w", err)
	}
	
	err = mm.node.PublishToTopic(topicName, messageData)
	if err != nil {
		return fmt.Errorf("failed to publish NodeLeft event: %w", err)
	}
	
	log.Printf("✅ Successfully published NodeLeft event to topic: %s", topicName)
	return nil
}

// PublishNodeUpdate publishes a NodeUpdate event when capabilities change
func (mm *MembershipManager) PublishNodeUpdate(clusterID string, updatedLabels map[string]string, removedLabels []string) error {
	log.Printf("📢 Publishing NodeUpdate event for cluster %s", clusterID)
	
	// Create NodeUpdate event
	nodeUpdate := &models.NodeUpdate{
		NodeId:         mm.node.id,
		UpdatedLabels:  updatedLabels,
		RemovedLabels:  removedLabels,
		Status:         models.NodeStatus_ACTIVE,
		Incarnation:    mm.getIncarnationNumber(),
	}
	
	// Create signed membership message
	membershipMsg, err := mm.createSignedMembershipMessage(nodeUpdate)
	if err != nil {
		return fmt.Errorf("failed to create signed membership message: %w", err)
	}
	
	// Publish to cluster-specific membership topic
	topicName := fmt.Sprintf("falak/%s/members/membership", clusterID)
	messageData, err := proto.Marshal(membershipMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal membership message: %w", err)
	}
	
	err = mm.node.PublishToTopic(topicName, messageData)
	if err != nil {
		return fmt.Errorf("failed to publish NodeUpdate event: %w", err)
	}
	
	log.Printf("✅ Successfully published NodeUpdate event to topic: %s", topicName)
	return nil
}

// ProcessMembershipEvent processes incoming membership events
func (mm *MembershipManager) ProcessMembershipEvent(topicName string, messageData []byte) error {
	// Parse membership message
	var membershipMsg models.MembershipMessage
	if err := proto.Unmarshal(messageData, &membershipMsg); err != nil {
		return fmt.Errorf("failed to unmarshal membership message: %w", err)
	}
	
	// Verify signature
	if err := mm.verifyMembershipSignature(&membershipMsg); err != nil {
		log.Printf("⚠️ Invalid signature on membership message from %s: %v", membershipMsg.NodeId, err)
		return err
	}
	
	log.Printf("📨 Processing membership event from %s (type: %T)", membershipMsg.NodeId, membershipMsg.Event)
	
	// Process based on event type
	switch event := membershipMsg.Event.(type) {
	case *models.MembershipMessage_NodeJoined:
		return mm.handleNodeJoined(event.NodeJoined, &membershipMsg)
		
	case *models.MembershipMessage_NodeLeft:
		return mm.handleNodeLeft(event.NodeLeft, &membershipMsg)
		
	case *models.MembershipMessage_NodeUpdate:
		return mm.handleNodeUpdate(event.NodeUpdate, &membershipMsg)
		
	default:
		return fmt.Errorf("unknown membership event type: %T", event)
	}
}

// handleNodeJoined processes NodeJoined events
func (mm *MembershipManager) handleNodeJoined(event *models.NodeJoined, msg *models.MembershipMessage) error {
	log.Printf("👋 Node %s joined cluster (connected to %d peers)", 
		event.Peer.NodeId, len(event.ConnectedTo))
	
	// Convert to internal Peer structure
	peer := Peer{
		ID:         event.Peer.NodeId,
		Status:     "active",
		Tags:       make(map[string]interface{}),
		DataCenter: mm.extractDataCenter(event.Peer.Labels),
		LastSeen:   time.Now(),
		TrustScore: 1.0,
	}
	
	// Copy labels as tags
	for key, value := range event.Peer.Labels {
		peer.Tags[key] = value
	}
	
	// Add cluster information
	peer.Tags["incarnation"] = event.Incarnation
	peer.Tags["connected_peers"] = len(event.ConnectedTo)
	
	// Add to phonebook
	mm.node.phonebook.AddPeer(peer)
	
	log.Printf("✅ Added peer %s to phonebook (DC: %s)", peer.ID, peer.DataCenter)
	return nil
}

// handleNodeLeft processes NodeLeft events
func (mm *MembershipManager) handleNodeLeft(event *models.NodeLeft, msg *models.MembershipMessage) error {
	log.Printf("👋 Node %s left cluster (reason: %s)", event.NodeId, event.Reason)
	
	// Remove from phonebook
	mm.node.phonebook.RemovePeer(event.NodeId)
	
	log.Printf("✅ Removed peer %s from phonebook", event.NodeId)
	return nil
}

// handleNodeUpdate processes NodeUpdate events
func (mm *MembershipManager) handleNodeUpdate(event *models.NodeUpdate, msg *models.MembershipMessage) error {
	log.Printf("🔄 Node %s updated capabilities", event.NodeId)
	
	// Get existing peer
	peer, exists := mm.node.phonebook.GetPeer(event.NodeId)
	if !exists {
		log.Printf("⚠️ Received update for unknown peer %s", event.NodeId)
		return nil
	}
	
	// Update labels
	for key, value := range event.UpdatedLabels {
		peer.Tags[key] = value
	}
	
	// Remove labels
	for _, key := range event.RemovedLabels {
		delete(peer.Tags, key)
	}
	
	// Update incarnation
	peer.Tags["incarnation"] = event.Incarnation
	peer.LastSeen = time.Now()
	
	// Update in phonebook
	mm.node.phonebook.AddPeer(peer)
	
	log.Printf("✅ Updated peer %s in phonebook", event.NodeId)
	return nil
}

// createSignedMembershipMessage creates a signed membership message envelope
func (mm *MembershipManager) createSignedMembershipMessage(event interface{}) (*models.MembershipMessage, error) {
	msg := &models.MembershipMessage{
		NodeId:    mm.node.id,
		Timestamp: time.Now().Unix(),
	}
	
	// Set the appropriate event type
	switch e := event.(type) {
	case *models.NodeJoined:
		msg.Event = &models.MembershipMessage_NodeJoined{NodeJoined: e}
	case *models.NodeLeft:
		msg.Event = &models.MembershipMessage_NodeLeft{NodeLeft: e}
	case *models.NodeUpdate:
		msg.Event = &models.MembershipMessage_NodeUpdate{NodeUpdate: e}
	default:
		return nil, fmt.Errorf("unsupported event type: %T", event)
	}
	
	// Create signature (TODO: use real libp2p signing)
	signatureData := fmt.Sprintf("%s:%d", msg.NodeId, msg.Timestamp)
	signature := sha256.Sum256([]byte(signatureData))
	msg.Signature = signature[:]
	
	return msg, nil
}

// verifyMembershipSignature verifies the signature on a membership message
func (mm *MembershipManager) verifyMembershipSignature(msg *models.MembershipMessage) error {
	// TODO: Implement real signature verification using libp2p peer identity
	// For now, just check that signature exists
	if len(msg.Signature) == 0 {
		return fmt.Errorf("missing signature")
	}
	
	if len(msg.Signature) != 32 {
		return fmt.Errorf("invalid signature length: %d", len(msg.Signature))
	}
	
	return nil
}

// getNodeMultiaddrs returns the current node's multiaddresses
func (mm *MembershipManager) getNodeMultiaddrs() []string {
	addrs := mm.node.host.Addrs()
	multiaddrs := make([]string, len(addrs))
	for i, addr := range addrs {
		multiaddrs[i] = addr.String()
	}
	return multiaddrs
}

// getNodeLabels returns the current node's labels
func (mm *MembershipManager) getNodeLabels() map[string]string {
	labels := map[string]string{
		"node_name":   mm.node.name,
		"data_center": mm.node.dataCenter,
		"node_type":   "falak-node",
		"version":     "1.0",
	}
	
	// Add clusters as comma-separated string
	if len(mm.node.clusters) > 0 {
		clustersStr := ""
		for i, cluster := range mm.node.clusters {
			if i > 0 {
				clustersStr += ","
			}
			clustersStr += cluster
		}
		labels["clusters"] = clustersStr
	}
	
	// Add tags as labels if they're strings
	for key, value := range mm.node.tags {
		if strValue, ok := value.(string); ok {
			labels[key] = strValue
		}
	}
	
	return labels
}

// getIncarnationNumber returns the current incarnation number
func (mm *MembershipManager) getIncarnationNumber() uint64 {
	// Use heartbeat manager's incarnation if available
	if mm.node.heartbeatManager != nil {
		return mm.node.heartbeatManager.GetIncarnationNumber()
	}
	// Fallback to timestamp
	return uint64(time.Now().Unix())
}

// extractDataCenter extracts data center from peer labels
func (mm *MembershipManager) extractDataCenter(labels map[string]string) string {
	if dc, exists := labels["data_center"]; exists {
		return dc
	}
	return "unknown"
}