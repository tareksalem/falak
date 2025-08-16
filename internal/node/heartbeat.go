package node

import (
	"crypto/sha256"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/tareksalem/falak/internal/node/protobuf/models"
)

// HeartbeatManager handles periodic heartbeat publishing and processing
type HeartbeatManager struct {
	node              *Node
	mu                sync.RWMutex
	sequenceNumber    uint64
	incarnationNumber uint64
	hlc               uint64 // Hybrid Logical Clock
	stopChan          chan struct{}
	running           bool
}

// NewHeartbeatManager creates a new heartbeat manager
func NewHeartbeatManager(node *Node) *HeartbeatManager {
	return &HeartbeatManager{
		node:              node,
		sequenceNumber:    0,
		incarnationNumber: uint64(time.Now().Unix()), // Start with current timestamp
		hlc:               uint64(time.Now().UnixNano() / 1000000), // HLC in milliseconds
		stopChan:          make(chan struct{}),
		running:           false,
	}
}


// Start begins the heartbeat publishing routine
func (hm *HeartbeatManager) Start() error {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	
	if hm.running {
		return fmt.Errorf("heartbeat manager already running")
	}
	
	hm.running = true
	log.Printf("💓 Starting heartbeat manager for node %s", hm.node.name)
	
	// Start heartbeat routine for each cluster
	for _, cluster := range hm.node.clusters {
		go hm.heartbeatRoutine(cluster)
	}
	
	return nil
}

// Stop stops the heartbeat publishing routine
func (hm *HeartbeatManager) Stop() error {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	
	if !hm.running {
		return nil
	}
	
	log.Printf("💓 Stopping heartbeat manager")
	close(hm.stopChan)
	hm.running = false
	
	return nil
}

// heartbeatRoutine publishes periodic heartbeats for a specific cluster
func (hm *HeartbeatManager) heartbeatRoutine(clusterID string) {
	log.Printf("💓 Starting heartbeat routine for cluster: %s", clusterID)
	
	// Base interval: 1-2 seconds with ±20% jitter
	baseInterval := 1500 * time.Millisecond
	jitter := 300 * time.Millisecond // ±20% of 1.5s
	
	ticker := time.NewTicker(baseInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-hm.stopChan:
			log.Printf("💓 Stopping heartbeat routine for cluster: %s", clusterID)
			return
			
		case <-ticker.C:
			// Apply jitter: random value between -jitter and +jitter
			jitterValue := time.Duration((time.Now().UnixNano() % int64(jitter*2)) - int64(jitter))
			nextInterval := baseInterval + jitterValue
			ticker.Reset(nextInterval)
			
			// Publish heartbeat
			if err := hm.publishHeartbeat(clusterID); err != nil {
				log.Printf("❌ Failed to publish heartbeat for cluster %s: %v", clusterID, err)
			}
		}
	}
}

// publishHeartbeat publishes a heartbeat message to the cluster
func (hm *HeartbeatManager) publishHeartbeat(clusterID string) error {
	hm.mu.Lock()
	
	// Increment sequence number
	hm.sequenceNumber++
	
	// Update HLC (hybrid logical clock)
	currentTime := uint64(time.Now().UnixNano() / 1000000)
	if currentTime > hm.hlc {
		hm.hlc = currentTime
	} else {
		hm.hlc++
	}
	
	seq := hm.sequenceNumber
	hlc := hm.hlc
	incarnation := hm.incarnationNumber
	
	hm.mu.Unlock()
	
	// Get current load information
	load := hm.getCurrentLoad()
	
	// Get connected peers count
	peersConnected := uint32(len(hm.node.host.Network().Peers()))
	
	// Generate phonebook digest for anti-entropy
	digest := hm.generatePhonebookDigest()
	
	// Create heartbeat message
	heartbeat := &models.Heartbeat{
		ClusterId:      clusterID,
		NodeId:         hm.node.id,
		Incarnation:    incarnation,
		Hlc:            hlc,
		Seq:            seq,
		PeersConnected: peersConnected,
		Load:           load,
		Digest:         digest,
		Signature:      []byte{}, // Will be filled by signing
	}
	
	// Sign the heartbeat (TODO: use real libp2p signing)
	signature := hm.signHeartbeat(heartbeat)
	heartbeat.Signature = signature
	
	// Marshal the heartbeat
	data, err := proto.Marshal(heartbeat)
	if err != nil {
		return fmt.Errorf("failed to marshal heartbeat: %w", err)
	}
	
	// Publish to heartbeat topic
	topicName := fmt.Sprintf("falak/%s/heartbeat", clusterID)
	err = hm.node.PublishToTopic(topicName, data)
	if err != nil {
		return fmt.Errorf("failed to publish heartbeat: %w", err)
	}
	
	if seq%10 == 0 { // Log every 10th heartbeat to reduce noise
		log.Printf("💓 Published heartbeat #%d to %s (peers: %d, load: %.2f/%.2f)", 
			seq, topicName, peersConnected, load.Cpu, load.Memory)
	}
	
	return nil
}

// ProcessHeartbeat processes an incoming heartbeat message
func (hm *HeartbeatManager) ProcessHeartbeat(topicName string, messageData []byte) error {
	// Parse heartbeat message
	var heartbeat models.Heartbeat
	if err := proto.Unmarshal(messageData, &heartbeat); err != nil {
		return fmt.Errorf("failed to unmarshal heartbeat: %w", err)
	}
	
	// CRITICAL: Ignore our own heartbeats (additional safety check)
	normalizedHeartbeatNodeID := normalizePeerID(heartbeat.NodeId)
	normalizedNodeID := normalizePeerID(hm.node.id)
	if normalizedHeartbeatNodeID == normalizedNodeID {
		// This should not happen due to pubsub filtering, but adding extra safety
		return nil
	}
	
	// Verify signature
	if !hm.verifyHeartbeatSignature(&heartbeat) {
		log.Printf("⚠️ Invalid signature on heartbeat from %s", heartbeat.NodeId)
		return fmt.Errorf("invalid heartbeat signature")
	}
	
	// Update HLC with received timestamp
	hm.updateHLC(heartbeat.Hlc)
	
	// Process the heartbeat in health registry
	if hm.node.healthRegistry != nil {
		hm.node.healthRegistry.ProcessHeartbeat(&heartbeat)
	}

	// Process recovery heartbeats with self-healing manager
	if hm.node.selfHealingManager != nil {
		hm.node.selfHealingManager.ProcessRecoveryHeartbeat(&heartbeat)
	}
	
	// Log heartbeat reception (reduce noise by logging every 10th)
	if heartbeat.Seq%10 == 0 {
		log.Printf("💓 Received heartbeat #%d from %s (incarnation: %d, peers: %d)", 
			heartbeat.Seq, heartbeat.NodeId, heartbeat.Incarnation, heartbeat.PeersConnected)
	}
	
	return nil
}

// getCurrentLoad returns current system load information
func (hm *HeartbeatManager) getCurrentLoad() *models.Load {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	
	// Calculate memory usage as a fraction
	// Using Alloc / Sys as a rough approximation
	memoryUsage := float32(memStats.Alloc) / float32(memStats.Sys)
	if memoryUsage > 1.0 {
		memoryUsage = 1.0
	}
	
	// Get number of active connections
	activeConns := uint32(len(hm.node.host.Network().Conns()))
	
	// TODO: Implement proper CPU usage monitoring
	// For now, use a placeholder based on goroutines as a rough proxy
	cpuUsage := float32(runtime.NumGoroutine()) / 1000.0
	if cpuUsage > 1.0 {
		cpuUsage = 1.0
	}
	
	return &models.Load{
		Cpu:    cpuUsage,
		Memory: memoryUsage,
		Conns:  activeConns,
	}
}

// generatePhonebookDigest creates a hash digest of the current phonebook for anti-entropy
func (hm *HeartbeatManager) generatePhonebookDigest() []byte {
	// Get all peers from phonebook
	peers := hm.node.phonebook.GetPeers()
	
	// Create a simple digest by hashing peer IDs and last seen times
	hasher := sha256.New()
	for _, peer := range peers {
		hasher.Write([]byte(peer.ID))
		hasher.Write([]byte(peer.LastSeen.Format(time.RFC3339)))
	}
	
	digest := hasher.Sum(nil)
	return digest[:16] // Use first 16 bytes as digest
}

// signHeartbeat creates a signature for the heartbeat message
func (hm *HeartbeatManager) signHeartbeat(heartbeat *models.Heartbeat) []byte {
	// TODO: Implement real libp2p signing
	// For now, create a simple hash-based signature
	signatureData := fmt.Sprintf("%s:%d:%d:%d", 
		heartbeat.NodeId, heartbeat.Incarnation, heartbeat.Hlc, heartbeat.Seq)
	
	hash := sha256.Sum256([]byte(signatureData))
	return hash[:]
}

// verifyHeartbeatSignature verifies the signature on a heartbeat message
func (hm *HeartbeatManager) verifyHeartbeatSignature(heartbeat *models.Heartbeat) bool {
	// TODO: Implement real signature verification using libp2p peer identity
	// For now, just check that signature exists and has correct length
	if len(heartbeat.Signature) != 32 {
		return false
	}
	
	return true
}

// updateHLC updates the hybrid logical clock with a received timestamp
func (hm *HeartbeatManager) updateHLC(receivedHLC uint64) {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	
	currentTime := uint64(time.Now().UnixNano() / 1000000)
	
	// Update HLC according to hybrid logical clock rules
	if receivedHLC > hm.hlc {
		if receivedHLC > currentTime {
			hm.hlc = receivedHLC + 1
		} else {
			hm.hlc = currentTime
		}
	} else {
		if currentTime > hm.hlc {
			hm.hlc = currentTime
		} else {
			hm.hlc++
		}
	}
}

// GetIncarnationNumber returns the current incarnation number
func (hm *HeartbeatManager) GetIncarnationNumber() uint64 {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	return hm.incarnationNumber
}

// IncrementIncarnation increments the incarnation number (used for recovery)
func (hm *HeartbeatManager) IncrementIncarnation() uint64 {
	hm.mu.Lock()
	defer hm.mu.Unlock()
	
	hm.incarnationNumber++
	log.Printf("🔄 Incremented incarnation number to %d", hm.incarnationNumber)
	return hm.incarnationNumber
}

// GetCurrentHLC returns the current hybrid logical clock value
func (hm *HeartbeatManager) GetCurrentHLC() uint64 {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	return hm.hlc
}

// GetCurrentIncarnation returns the current incarnation number
func (hm *HeartbeatManager) GetCurrentIncarnation() uint64 {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	return hm.incarnationNumber
}

// IsRunning returns true if the heartbeat manager is currently running
func (hm *HeartbeatManager) IsRunning() bool {
	hm.mu.RLock()
	defer hm.mu.RUnlock()
	return hm.running
}