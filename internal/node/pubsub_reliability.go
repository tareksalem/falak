package node

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// ReliableMessage wraps a message with reliability metadata
type ReliableMessage struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	Sender    string      `json:"sender"`
	Payload   interface{} `json:"payload"`
	Timestamp time.Time   `json:"timestamp"`
	RequireAck bool       `json:"require_ack"`
}

// MessageAck represents an acknowledgment
type MessageAck struct {
	MessageID string    `json:"message_id"`
	NodeID    string    `json:"node_id"`
	Timestamp time.Time `json:"timestamp"`
}

// ReliablePublisher ensures messages reach all nodes
type ReliablePublisher struct {
	node         *Node
	pendingAcks  map[string]map[string]bool // messageID -> nodeID -> acked
	mu           sync.RWMutex
	retryTimeout time.Duration
	maxRetries   int
}

// NewReliablePublisher creates a publisher with retry logic
func NewReliablePublisher(node *Node) *ReliablePublisher {
	return &ReliablePublisher{
		node:         node,
		pendingAcks:  make(map[string]map[string]bool),
		retryTimeout: 5 * time.Second,
		maxRetries:   3,
	}
}

// PublishReliable sends a message and ensures all nodes receive it
func (rp *ReliablePublisher) PublishReliable(topic string, message interface{}, requireAck bool) error {
	msgID := fmt.Sprintf("%s-%d", rp.node.ID(), time.Now().UnixNano())
	
	reliableMsg := ReliableMessage{
		ID:         msgID,
		Type:       "data",
		Sender:     rp.node.ID(),
		Payload:    message,
		Timestamp:  time.Now(),
		RequireAck: requireAck,
	}
	
	data, err := json.Marshal(reliableMsg)
	if err != nil {
		return err
	}
	
	if requireAck {
		// Track expected acknowledgments
		rp.mu.Lock()
		rp.pendingAcks[msgID] = rp.getExpectedNodes()
		rp.mu.Unlock()
		
		// Start retry timer
		go rp.ensureDelivery(topic, reliableMsg)
	}
	
	// Publish the message
	return rp.node.PublishToTopic(topic, data)
}

// getExpectedNodes returns map of nodes that should acknowledge
func (rp *ReliablePublisher) getExpectedNodes() map[string]bool {
	nodes := make(map[string]bool)
	// Get all connected peers
	for _, peer := range rp.node.host.Network().Peers() {
		nodes[peer.String()] = false
	}
	return nodes
}

// ensureDelivery retries message until all nodes acknowledge
func (rp *ReliablePublisher) ensureDelivery(topic string, msg ReliableMessage) {
	retries := 0
	ticker := time.NewTicker(rp.retryTimeout)
	defer ticker.Stop()
	
	for retries < rp.maxRetries {
		select {
		case <-ticker.C:
			rp.mu.RLock()
			pending := rp.pendingAcks[msg.ID]
			allAcked := true
			for nodeID, acked := range pending {
				if !acked {
					allAcked = false
					log.Printf("⚠️ Node %s has not acknowledged message %s", nodeID, msg.ID)
				}
			}
			rp.mu.RUnlock()
			
			if allAcked {
				log.Printf("✅ All nodes acknowledged message %s", msg.ID)
				rp.mu.Lock()
				delete(rp.pendingAcks, msg.ID)
				rp.mu.Unlock()
				return
			}
			
			// Retry
			retries++
			log.Printf("🔄 Retrying message %s (attempt %d/%d)", msg.ID, retries, rp.maxRetries)
			data, _ := json.Marshal(msg)
			rp.node.PublishToTopic(topic, data)
		}
	}
	
	log.Printf("❌ Failed to get acknowledgments for message %s after %d retries", msg.ID, rp.maxRetries)
}

// HandleAck processes acknowledgment from a node
func (rp *ReliablePublisher) HandleAck(ack MessageAck) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	
	if pending, exists := rp.pendingAcks[ack.MessageID]; exists {
		pending[ack.NodeID] = true
		log.Printf("✓ Received ACK for message %s from node %s", ack.MessageID, ack.NodeID)
	}
}

// Enhanced message handler that sends acknowledgments
func (n *Node) HandleReliableMessage(data []byte, topic string) {
	var msg ReliableMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return // Not a reliable message
	}
	
	// Process the message
	log.Printf("📨 Received reliable message %s on topic %s: %v", msg.ID, topic, msg.Payload)
	
	// Send acknowledgment if required
	if msg.RequireAck && msg.Sender != n.ID() {
		ack := MessageAck{
			MessageID: msg.ID,
			NodeID:    n.ID(),
			Timestamp: time.Now(),
		}
		
		ackData, _ := json.Marshal(ack)
		// Send ack on a dedicated ack topic
		n.PublishToTopic(topic+"/ack", ackData)
	}
}