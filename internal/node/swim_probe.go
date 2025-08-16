package node

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"log"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"

	"github.com/tareksalem/falak/internal/node/protobuf/models"
)

const (
	// SWIM protocol IDs
	PingProtocolID         = "/falak/ping/1.0"
	IndirectProbeProtocolID = "/falak/indirect-probe/1.0"
	
	// SWIM timeouts
	DirectPingTimeout    = 300 * time.Millisecond
	IndirectProbeTimeout = 500 * time.Millisecond
	IndirectFanout       = 3 // Number of peers to ask for indirect probes
)

// SWIMProber handles SWIM-style failure detection probing
type SWIMProber struct {
	node *Node
}

// NewSWIMProber creates a new SWIM prober
func NewSWIMProber(node *Node) *SWIMProber {
	return &SWIMProber{
		node: node,
	}
}

// RegisterHandlers registers SWIM protocol stream handlers
func (sp *SWIMProber) RegisterHandlers() {
	sp.node.host.SetStreamHandler(PingProtocolID, sp.handlePingRequest)
	sp.node.host.SetStreamHandler(IndirectProbeProtocolID, sp.handleIndirectProbeRequest)
	log.Printf("🏓 Registered SWIM protocol handlers: %s, %s", PingProtocolID, IndirectProbeProtocolID)
}

// DirectPing performs a direct ping to a target peer
func (sp *SWIMProber) DirectPing(targetPeerID peer.ID, clusterID string) error {
	log.Printf("🏓 Direct ping to peer %s", targetPeerID.ShortString())
	
	// Create ping request
	nonce := make([]byte, 16)
	_, err := rand.Read(nonce)
	if err != nil {
		return fmt.Errorf("failed to generate nonce: %w", err)
	}
	
	request := &models.PingRequest{
		ClusterId: clusterID,
		NodeId:    sp.node.id,
		Nonce:     nonce,
		Hlc:       sp.getHLC(),
		Signature: []byte{}, // Will be set by signing
	}
	
	// Sign the request
	request.Signature = sp.signPingMessage(request.ClusterId, request.NodeId, request.Nonce, request.Hlc)
	
	// Open stream and send ping
	ctx, cancel := context.WithTimeout(sp.node.ctx, DirectPingTimeout)
	defer cancel()
	
	stream, err := sp.node.host.NewStream(ctx, targetPeerID, PingProtocolID)
	if err != nil {
		return fmt.Errorf("failed to open ping stream: %w", err)
	}
	defer stream.Close()
	
	// Send ping request
	if err := sp.sendMessage(stream, request); err != nil {
		return fmt.Errorf("failed to send ping request: %w", err)
	}
	
	// Receive ping response
	response, err := sp.receivePingResponse(stream)
	if err != nil {
		return fmt.Errorf("failed to receive ping response: %w", err)
	}
	
	// Verify response
	if err := sp.verifyPingResponse(request, response); err != nil {
		return fmt.Errorf("invalid ping response: %w", err)
	}
	
	log.Printf("✅ Direct ping successful to peer %s", targetPeerID.ShortString())
	return nil
}

// IndirectProbe performs indirect probing via k random healthy peers
func (sp *SWIMProber) IndirectProbe(targetPeerID peer.ID, clusterID string) (bool, []*models.Witness, error) {
	log.Printf("🔄 Indirect probe for peer %s", targetPeerID.ShortString())
	
	// Get k random healthy peers
	healthyPeers := sp.getRandomHealthyPeers(IndirectFanout, targetPeerID)
	if len(healthyPeers) == 0 {
		return false, nil, fmt.Errorf("no healthy peers available for indirect probe")
	}
	
	log.Printf("🔄 Using %d peers for indirect probe: %v", len(healthyPeers), healthyPeers)
	
	// Create probe request
	nonce := make([]byte, 16)
	_, err := rand.Read(nonce)
	if err != nil {
		return false, nil, fmt.Errorf("failed to generate nonce: %w", err)
	}
	
	request := &models.IndirectProbeRequest{
		ClusterId:        clusterID,
		RequesterNodeId:  sp.node.id,
		TargetNodeId:     targetPeerID.String(),
		Nonce:            nonce,
		Hlc:              sp.getHLC(),
		Signature:        []byte{}, // Will be set by signing
	}
	
	// Sign the request
	request.Signature = sp.signProbeMessage(request.ClusterId, request.RequesterNodeId, request.TargetNodeId, request.Hlc)
	
	// Send indirect probe requests in parallel
	type probeResult struct {
		success  bool
		witness  *models.Witness
		err      error
	}
	
	resultChan := make(chan probeResult, len(healthyPeers))
	
	for _, proberPeerID := range healthyPeers {
		go func(prober peer.ID) {
			witness, success, err := sp.sendIndirectProbeRequest(prober, request)
			resultChan <- probeResult{success: success, witness: witness, err: err}
		}(proberPeerID)
	}
	
	// Collect results
	var witnesses []*models.Witness
	successCount := 0
	
	for i := 0; i < len(healthyPeers); i++ {
		select {
		case result := <-resultChan:
			if result.err != nil {
				log.Printf("⚠️ Indirect probe error: %v", result.err)
			} else {
				if result.witness != nil {
					witnesses = append(witnesses, result.witness)
				}
				if result.success {
					successCount++
				}
			}
		case <-time.After(IndirectProbeTimeout):
			log.Printf("⏰ Indirect probe timeout")
		}
	}
	
	probeSuccess := successCount > 0
	log.Printf("🔄 Indirect probe result: %d/%d successful", successCount, len(healthyPeers))
	
	return probeSuccess, witnesses, nil
}

// ProbeIfSuspected checks if a peer should be probed based on phi score
func (sp *SWIMProber) ProbeIfSuspected(nodeID string) {
	if sp.node.healthRegistry == nil {
		return
	}
	
	peerHealth, exists := sp.node.healthRegistry.GetPeerHealth(nodeID)
	if !exists {
		return
	}
	
	// Only probe if peer is suspected but not yet failed
	if peerHealth.Status != HealthStatusEnum.Suspect {
		return
	}
	
	// Get peer ID
	peerID, err := peer.Decode(nodeID)
	if err != nil {
		log.Printf("❌ Invalid peer ID for probing: %s", nodeID)
		return
	}
	
	// Determine cluster ID (use first cluster)
	clusterID := "default"
	if len(sp.node.clusters) > 0 {
		clusterID = sp.node.clusters[0]
	}
	
	log.Printf("🔍 Probing suspected peer %s (phi: %.2f)", nodeID, peerHealth.Phi)
	
	// Try direct ping first
	err = sp.DirectPing(peerID, clusterID)
	if err == nil {
		// Direct ping successful - peer is alive
		log.Printf("✅ Direct ping successful, peer %s is alive", nodeID)
		return
	}
	
	log.Printf("❌ Direct ping failed to %s: %v", nodeID, err)
	
	// Try indirect probe
	success, witnesses, err := sp.IndirectProbe(peerID, clusterID)
	if err != nil {
		log.Printf("❌ Indirect probe failed for %s: %v", nodeID, err)
		return
	}
	
	if success {
		log.Printf("✅ Indirect probe successful, peer %s is alive", nodeID)
		// TODO: Send refutation message
	} else {
		log.Printf("❌ All probes failed for peer %s, raising suspicion", nodeID)
		// TODO: Publish suspicion with witnesses
		sp.publishSuspicion(nodeID, peerHealth.Phi, witnesses)
	}
}

// handlePingRequest handles incoming ping requests
func (sp *SWIMProber) handlePingRequest(stream network.Stream) {
	defer stream.Close()
	
	peerID := stream.Conn().RemotePeer()
	log.Printf("🏓 Handling ping request from %s", peerID.ShortString())
	
	// Receive ping request
	request, err := sp.receivePingRequest(stream)
	if err != nil {
		log.Printf("❌ Failed to receive ping request: %v", err)
		return
	}
	
	// Verify signature
	if !sp.verifyPingSignature(request) {
		log.Printf("❌ Invalid ping signature from %s", peerID.ShortString())
		return
	}
	
	// Create ping response
	response := &models.PingResponse{
		ClusterId: request.ClusterId,
		NodeId:    sp.node.id,
		Nonce:     request.Nonce, // Echo the nonce
		Hlc:       sp.getHLC(),
		Signature: []byte{}, // Will be set by signing
	}
	
	// Sign the response
	response.Signature = sp.signPingMessage(response.ClusterId, response.NodeId, response.Nonce, response.Hlc)
	
	// Send ping response
	if err := sp.sendMessage(stream, response); err != nil {
		log.Printf("❌ Failed to send ping response: %v", err)
		return
	}
	
	log.Printf("✅ Sent ping response to %s", peerID.ShortString())
}

// handleIndirectProbeRequest handles incoming indirect probe requests
func (sp *SWIMProber) handleIndirectProbeRequest(stream network.Stream) {
	defer stream.Close()
	
	peerID := stream.Conn().RemotePeer()
	log.Printf("🔄 Handling indirect probe request from %s", peerID.ShortString())
	
	// Receive probe request
	request, err := sp.receiveIndirectProbeRequest(stream)
	if err != nil {
		log.Printf("❌ Failed to receive indirect probe request: %v", err)
		return
	}
	
	// Verify signature
	if !sp.verifyProbeSignature(request) {
		log.Printf("❌ Invalid probe signature from %s", peerID.ShortString())
		return
	}
	
	// Parse target peer ID
	targetPeerID, err := peer.Decode(request.TargetNodeId)
	if err != nil {
		log.Printf("❌ Invalid target peer ID in probe request: %s", request.TargetNodeId)
		return
	}
	
	// Attempt direct ping to target
	probeSuccess := false
	pingErr := sp.DirectPing(targetPeerID, request.ClusterId)
	if pingErr == nil {
		probeSuccess = true
		log.Printf("✅ Indirect probe successful: target %s is alive", request.TargetNodeId)
	} else {
		log.Printf("❌ Indirect probe failed: target %s unreachable", request.TargetNodeId)
	}
	
	// Create probe response
	response := &models.IndirectProbeResponse{
		ClusterId:     request.ClusterId,
		ProberNodeId:  sp.node.id,
		TargetNodeId:  request.TargetNodeId,
		ProbeResult:   probeSuccess,
		Hlc:           sp.getHLC(),
		Signature:     []byte{}, // Will be set by signing
	}
	
	// Sign the response
	response.Signature = sp.signProbeMessage(response.ClusterId, response.ProberNodeId, response.TargetNodeId, response.Hlc)
	
	// Send probe response
	if err := sp.sendMessage(stream, response); err != nil {
		log.Printf("❌ Failed to send indirect probe response: %v", err)
		return
	}
	
	log.Printf("✅ Sent indirect probe response to %s (result: %v)", peerID.ShortString(), probeSuccess)
}

// getRandomHealthyPeers returns k random healthy peers excluding the target
func (sp *SWIMProber) getRandomHealthyPeers(k int, exclude peer.ID) []peer.ID {
	if sp.node.healthRegistry == nil {
		return nil
	}
	
	allPeers := sp.node.healthRegistry.GetAllPeers()
	var healthyPeers []peer.ID
	
	for nodeID, peerHealth := range allPeers {
		if peerHealth.Status == HealthStatusEnum.Healthy {
			if peerID, err := peer.Decode(nodeID); err == nil && peerID != exclude {
				healthyPeers = append(healthyPeers, peerID)
			}
		}
	}
	
	// Shuffle and take k peers
	if len(healthyPeers) > k {
		// Simple random selection (not cryptographically secure, but sufficient)
		for i := len(healthyPeers) - 1; i > 0; i-- {
			j := int(time.Now().UnixNano()) % (i + 1)
			healthyPeers[i], healthyPeers[j] = healthyPeers[j], healthyPeers[i]
		}
		healthyPeers = healthyPeers[:k]
	}
	
	return healthyPeers
}

// sendIndirectProbeRequest sends an indirect probe request to a prober peer
func (sp *SWIMProber) sendIndirectProbeRequest(prober peer.ID, request *models.IndirectProbeRequest) (*models.Witness, bool, error) {
	ctx, cancel := context.WithTimeout(sp.node.ctx, IndirectProbeTimeout)
	defer cancel()
	
	stream, err := sp.node.host.NewStream(ctx, prober, IndirectProbeProtocolID)
	if err != nil {
		return nil, false, fmt.Errorf("failed to open indirect probe stream: %w", err)
	}
	defer stream.Close()
	
	// Send probe request
	if err := sp.sendMessage(stream, request); err != nil {
		return nil, false, fmt.Errorf("failed to send indirect probe request: %w", err)
	}
	
	// Receive probe response
	response, err := sp.receiveIndirectProbeResponse(stream)
	if err != nil {
		return nil, false, fmt.Errorf("failed to receive indirect probe response: %w", err)
	}
	
	// Create witness
	witness := &models.Witness{
		NodeId:      prober.String(),
		Hlc:         response.Hlc,
		ProbeResult: response.ProbeResult,
		Signature:   response.Signature,
	}
	
	return witness, response.ProbeResult, nil
}

// publishSuspicion publishes a suspicion message with witnesses
func (sp *SWIMProber) publishSuspicion(targetNodeID string, phi float64, witnesses []*models.Witness) {
	if sp.node.suspicionManager == nil {
		log.Printf("📢 Would publish suspicion for %s (phi: %.2f) with %d witnesses", 
			targetNodeID, phi, len(witnesses))
		return
	}
	
	// Determine cluster ID (use first cluster)
	clusterID := "default"
	if len(sp.node.clusters) > 0 {
		clusterID = sp.node.clusters[0]
	}
	
	// Determine suspicion reason
	reason := SuspicionReasonEnum.IndirectProbeFail
	
	// Publish suspicion through suspicion manager
	if err := sp.node.suspicionManager.PublishSuspicion(targetNodeID, reason, phi, witnesses, clusterID); err != nil {
		log.Printf("❌ Failed to publish suspicion for %s: %v", targetNodeID, err)
	}
}

// Helper methods for message serialization and signing

func (sp *SWIMProber) getHLC() uint64 {
	if sp.node.heartbeatManager != nil {
		return sp.node.heartbeatManager.GetCurrentHLC()
	}
	return uint64(time.Now().UnixNano() / 1000000)
}

func (sp *SWIMProber) signPingMessage(clusterID, nodeID string, nonce []byte, hlc uint64) []byte {
	// TODO: Use real libp2p signing
	data := fmt.Sprintf("%s:%s:%x:%d", clusterID, nodeID, nonce, hlc)
	hash := sha256.Sum256([]byte(data))
	return hash[:]
}

func (sp *SWIMProber) signProbeMessage(clusterID, nodeID, targetID string, hlc uint64) []byte {
	// TODO: Use real libp2p signing
	data := fmt.Sprintf("%s:%s:%s:%d", clusterID, nodeID, targetID, hlc)
	hash := sha256.Sum256([]byte(data))
	return hash[:]
}

func (sp *SWIMProber) verifyPingSignature(request *models.PingRequest) bool {
	// TODO: Implement real signature verification
	return len(request.Signature) == 32
}

func (sp *SWIMProber) verifyProbeSignature(request *models.IndirectProbeRequest) bool {
	// TODO: Implement real signature verification
	return len(request.Signature) == 32
}

func (sp *SWIMProber) verifyPingResponse(request *models.PingRequest, response *models.PingResponse) error {
	// Verify nonce echo
	if string(request.Nonce) != string(response.Nonce) {
		return fmt.Errorf("nonce mismatch")
	}
	
	// Verify cluster ID
	if request.ClusterId != response.ClusterId {
		return fmt.Errorf("cluster ID mismatch")
	}
	
	// TODO: Verify signature
	if len(response.Signature) != 32 {
		return fmt.Errorf("invalid signature")
	}
	
	return nil
}

// Message serialization helpers

func (sp *SWIMProber) sendMessage(stream network.Stream, message proto.Message) error {
	writer := bufio.NewWriter(stream)
	defer writer.Flush()
	
	data, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}
	
	// Write length (4 bytes, big-endian)
	length := uint32(len(data))
	lengthBytes := []byte{
		byte(length >> 24),
		byte(length >> 16),
		byte(length >> 8),
		byte(length),
	}
	
	if _, err := writer.Write(lengthBytes); err != nil {
		return fmt.Errorf("failed to write message length: %w", err)
	}
	
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("failed to write message data: %w", err)
	}
	
	return writer.Flush()
}

func (sp *SWIMProber) receivePingRequest(stream network.Stream) (*models.PingRequest, error) {
	reader := bufio.NewReader(stream)
	
	// Read message length
	lengthBytes := make([]byte, 4)
	if _, err := reader.Read(lengthBytes); err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}
	
	length := uint32(lengthBytes[0])<<24 | uint32(lengthBytes[1])<<16 | uint32(lengthBytes[2])<<8 | uint32(lengthBytes[3])
	
	// Read message data
	messageBytes := make([]byte, length)
	if _, err := reader.Read(messageBytes); err != nil {
		return nil, fmt.Errorf("failed to read message data: %w", err)
	}
	
	var request models.PingRequest
	if err := proto.Unmarshal(messageBytes, &request); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ping request: %w", err)
	}
	
	return &request, nil
}

func (sp *SWIMProber) receivePingResponse(stream network.Stream) (*models.PingResponse, error) {
	reader := bufio.NewReader(stream)
	
	// Read message length
	lengthBytes := make([]byte, 4)
	if _, err := reader.Read(lengthBytes); err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}
	
	length := uint32(lengthBytes[0])<<24 | uint32(lengthBytes[1])<<16 | uint32(lengthBytes[2])<<8 | uint32(lengthBytes[3])
	
	// Read message data
	messageBytes := make([]byte, length)
	if _, err := reader.Read(messageBytes); err != nil {
		return nil, fmt.Errorf("failed to read message data: %w", err)
	}
	
	var response models.PingResponse
	if err := proto.Unmarshal(messageBytes, &response); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ping response: %w", err)
	}
	
	return &response, nil
}

func (sp *SWIMProber) receiveIndirectProbeRequest(stream network.Stream) (*models.IndirectProbeRequest, error) {
	reader := bufio.NewReader(stream)
	
	// Read message length
	lengthBytes := make([]byte, 4)
	if _, err := reader.Read(lengthBytes); err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}
	
	length := uint32(lengthBytes[0])<<24 | uint32(lengthBytes[1])<<16 | uint32(lengthBytes[2])<<8 | uint32(lengthBytes[3])
	
	// Read message data
	messageBytes := make([]byte, length)
	if _, err := reader.Read(messageBytes); err != nil {
		return nil, fmt.Errorf("failed to read message data: %w", err)
	}
	
	var request models.IndirectProbeRequest
	if err := proto.Unmarshal(messageBytes, &request); err != nil {
		return nil, fmt.Errorf("failed to unmarshal indirect probe request: %w", err)
	}
	
	return &request, nil
}

func (sp *SWIMProber) receiveIndirectProbeResponse(stream network.Stream) (*models.IndirectProbeResponse, error) {
	reader := bufio.NewReader(stream)
	
	// Read message length
	lengthBytes := make([]byte, 4)
	if _, err := reader.Read(lengthBytes); err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}
	
	length := uint32(lengthBytes[0])<<24 | uint32(lengthBytes[1])<<16 | uint32(lengthBytes[2])<<8 | uint32(lengthBytes[3])
	
	// Read message data
	messageBytes := make([]byte, length)
	if _, err := reader.Read(messageBytes); err != nil {
		return nil, fmt.Errorf("failed to read message data: %w", err)
	}
	
	var response models.IndirectProbeResponse
	if err := proto.Unmarshal(messageBytes, &response); err != nil {
		return nil, fmt.Errorf("failed to unmarshal indirect probe response: %w", err)
	}
	
	return &response, nil
}