package node

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"

	"github.com/tareksalem/falak/internal/node/protobuf/models"
)

const (
	// Phonebook synchronization protocol IDs
	PhonebookDigestProtocolID = "/falak/phonebook/digest/1.0"
	PhonebookDeltaProtocolID  = "/falak/phonebook/delta/1.0"
	
	// Protocol timeouts
	PhonebookSyncTimeout = 30 * time.Second
)

// PhonebookProtocolHandler handles phonebook synchronization protocols
type PhonebookProtocolHandler struct {
	syncer *PhonebookSyncer
}

// NewPhonebookProtocolHandler creates a new phonebook protocol handler
func NewPhonebookProtocolHandler(syncer *PhonebookSyncer) *PhonebookProtocolHandler {
	return &PhonebookProtocolHandler{
		syncer: syncer,
	}
}

// RegisterHandlers registers the phonebook protocol handlers with the node
func (pph *PhonebookProtocolHandler) RegisterHandlers() {
	host := pph.syncer.node.host
	
	// Register digest protocol handler
	host.SetStreamHandler(PhonebookDigestProtocolID, pph.HandleDigestStream)
	log.Printf("📚 Registered phonebook digest protocol handler: %s", PhonebookDigestProtocolID)
	
	// Register delta protocol handler
	host.SetStreamHandler(PhonebookDeltaProtocolID, pph.HandleDeltaStream)
	log.Printf("📚 Registered phonebook delta protocol handler: %s", PhonebookDeltaProtocolID)
}

// HandleDigestStream handles incoming digest synchronization requests
func (pph *PhonebookProtocolHandler) HandleDigestStream(stream network.Stream) {
	defer stream.Close()
	
	// Set deadline for the entire sync process
	_ = stream.SetDeadline(time.Now().Add(PhonebookSyncTimeout))
	
	peerID := stream.Conn().RemotePeer()
	log.Printf("📚 Handling digest request from peer: %s", peerID.ShortString())
	
	reader := bufio.NewReader(stream)
	writer := bufio.NewWriter(stream)
	
	// Read digest request
	digestReq, err := pph.receiveDigestRequest(reader)
	if err != nil {
		log.Printf("❌ Failed to receive digest request from %s: %v", peerID.ShortString(), err)
		return
	}
	
	// Process request and generate response
	digestResp, err := pph.processDigestRequest(digestReq, peerID)
	if err != nil {
		log.Printf("❌ Failed to process digest request from %s: %v", peerID.ShortString(), err)
		return
	}
	
	// Send digest response
	if err := pph.sendDigestResponse(writer, digestResp); err != nil {
		log.Printf("❌ Failed to send digest response to %s: %v", peerID.ShortString(), err)
		return
	}
	
	log.Printf("✅ Successfully handled digest request from %s", peerID.ShortString())
}

// HandleDeltaStream handles incoming delta synchronization requests
func (pph *PhonebookProtocolHandler) HandleDeltaStream(stream network.Stream) {
	defer stream.Close()
	
	// Set deadline for the entire sync process
	_ = stream.SetDeadline(time.Now().Add(PhonebookSyncTimeout))
	
	peerID := stream.Conn().RemotePeer()
	log.Printf("📚 Handling delta request from peer: %s", peerID.ShortString())
	
	reader := bufio.NewReader(stream)
	writer := bufio.NewWriter(stream)
	
	// Read delta request
	deltaReq, err := pph.receiveDeltaRequest(reader)
	if err != nil {
		log.Printf("❌ Failed to receive delta request from %s: %v", peerID.ShortString(), err)
		return
	}
	
	// Process request and generate response
	deltaResp, err := pph.processDeltaRequest(deltaReq, peerID)
	if err != nil {
		log.Printf("❌ Failed to process delta request from %s: %v", peerID.ShortString(), err)
		return
	}
	
	// Send delta response
	if err := pph.sendDeltaResponse(writer, deltaResp); err != nil {
		log.Printf("❌ Failed to send delta response to %s: %v", peerID.ShortString(), err)
		return
	}
	
	log.Printf("✅ Successfully handled delta request from %s", peerID.ShortString())
}

// Client-side methods for initiating synchronization

// sendDigestRequest sends a digest request to a peer and returns the response
func (ps *PhonebookSyncer) sendDigestRequest(peerID peer.ID, req *models.PhonebookDigestRequest) (*models.PhonebookDigestResponse, error) {
	// Open stream for digest protocol
	ctx, cancel := context.WithTimeout(ps.node.ctx, PhonebookSyncTimeout)
	defer cancel()
	
	stream, err := ps.node.host.NewStream(ctx, peerID, PhonebookDigestProtocolID)
	if err != nil {
		return nil, fmt.Errorf("failed to open digest stream to peer %s for cluster %s: %w", 
			peerID.ShortString(), req.ClusterId, err)
	}
	defer stream.Close()
	
	// Set deadline
	_ = stream.SetDeadline(time.Now().Add(PhonebookSyncTimeout))
	
	reader := bufio.NewReader(stream)
	writer := bufio.NewWriter(stream)
	
	// Send digest request
	if err := ps.sendMessage(writer, req); err != nil {
		return nil, fmt.Errorf("failed to send digest request to peer %s (version %d): %w", 
			peerID.ShortString(), req.Version, err)
	}
	
	// Receive digest response
	resp, err := ps.receiveDigestResponse(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to receive digest response from peer %s: %w", 
			peerID.ShortString(), err)
	}
	
	return resp, nil
}

// sendDeltaRequest sends a delta request to a peer and returns the response
func (ps *PhonebookSyncer) sendDeltaRequest(peerID peer.ID, req *models.PhonebookDeltaRequest) (*models.PhonebookDeltaResponse, error) {
	// Open stream for delta protocol
	ctx, cancel := context.WithTimeout(ps.node.ctx, PhonebookSyncTimeout)
	defer cancel()
	
	stream, err := ps.node.host.NewStream(ctx, peerID, PhonebookDeltaProtocolID)
	if err != nil {
		return nil, fmt.Errorf("failed to open delta stream: %w", err)
	}
	defer stream.Close()
	
	// Set deadline
	_ = stream.SetDeadline(time.Now().Add(PhonebookSyncTimeout))
	
	reader := bufio.NewReader(stream)
	writer := bufio.NewWriter(stream)
	
	// Send delta request
	if err := ps.sendMessage(writer, req); err != nil {
		return nil, fmt.Errorf("failed to send delta request: %w", err)
	}
	
	// Receive delta response
	resp, err := ps.receiveDeltaResponse(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to receive delta response: %w", err)
	}
	
	return resp, nil
}

// Server-side message processing methods

// processDigestRequest processes an incoming digest request
func (pph *PhonebookProtocolHandler) processDigestRequest(req *models.PhonebookDigestRequest, fromPeer peer.ID) (*models.PhonebookDigestResponse, error) {
	syncer := pph.syncer
	
	// Update local digest
	syncer.updateLocalDigest()
	
	// Create digest response with consistent timestamp
	timestamp := uint64(time.Now().UnixNano() / 1000000)
	resp := &models.PhonebookDigestResponse{
		NodeId:     syncer.node.id,
		ClusterId:  req.ClusterId,
		Digest:     syncer.localDigest,
		Version:    syncer.syncVersion,
		Datacenter: syncer.node.dataCenter,
		PeerCount:  uint32(len(syncer.node.phonebook.GetPeers())),
		NeedsDelta: !syncer.digestsEqual(req.Digest, syncer.localDigest),
		Timestamp:  timestamp,
		Signature:  syncer.signDigestMessageWithTimestamp(syncer.localDigest, timestamp),
	}
	
	log.Printf("📚 Generated digest response for %s: needs_delta=%v, peer_count=%d",
		fromPeer.ShortString(), resp.NeedsDelta, resp.PeerCount)
	
	return resp, nil
}

// processDeltaRequest processes an incoming delta request
func (pph *PhonebookProtocolHandler) processDeltaRequest(req *models.PhonebookDeltaRequest, fromPeer peer.ID) (*models.PhonebookDeltaResponse, error) {
	syncer := pph.syncer
	
	// Apply the received delta
	applied := false
	if req.Delta != nil {
		if err := syncer.applyReceivedDelta(req.Delta, fromPeer); err != nil {
			log.Printf("❌ Failed to apply delta from %s: %v", fromPeer.ShortString(), err)
		} else {
			applied = true
			log.Printf("✅ Applied delta from %s", fromPeer.ShortString())
		}
	}
	
	// Generate our own delta to send back (if needed)
	responseDelta := syncer.generateDelta(nil) // Generate delta with current peers
	
	// Create delta response
	resp := &models.PhonebookDeltaResponse{
		NodeId:    syncer.node.id,
		ClusterId: req.ClusterId,
		Version:   syncer.syncVersion,
		Delta:     responseDelta,
		Applied:   applied,
		Timestamp: uint64(time.Now().UnixNano() / 1000000),
		Signature: syncer.signDeltaMessage(responseDelta),
	}
	
	log.Printf("📚 Generated delta response for %s: applied=%v, delta_peers=%d",
		fromPeer.ShortString(), applied, len(responseDelta.GetAdded()))
	
	return resp, nil
}

// Message I/O methods

// receiveDigestRequest receives and parses a digest request
func (pph *PhonebookProtocolHandler) receiveDigestRequest(reader *bufio.Reader) (*models.PhonebookDigestRequest, error) {
	messageBytes, err := pph.readMessage(reader)
	if err != nil {
		return nil, err
	}
	
	var req models.PhonebookDigestRequest
	if err := proto.Unmarshal(messageBytes, &req); err != nil {
		return nil, fmt.Errorf("failed to unmarshal digest request: %w", err)
	}
	
	return &req, nil
}

// receiveDeltaRequest receives and parses a delta request
func (pph *PhonebookProtocolHandler) receiveDeltaRequest(reader *bufio.Reader) (*models.PhonebookDeltaRequest, error) {
	messageBytes, err := pph.readMessage(reader)
	if err != nil {
		return nil, err
	}
	
	var req models.PhonebookDeltaRequest
	if err := proto.Unmarshal(messageBytes, &req); err != nil {
		return nil, fmt.Errorf("failed to unmarshal delta request: %w", err)
	}
	
	return &req, nil
}

// receiveDigestResponse receives and parses a digest response
func (ps *PhonebookSyncer) receiveDigestResponse(reader *bufio.Reader) (*models.PhonebookDigestResponse, error) {
	messageBytes, err := ps.readMessage(reader)
	if err != nil {
		return nil, err
	}
	
	var resp models.PhonebookDigestResponse
	if err := proto.Unmarshal(messageBytes, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal digest response: %w", err)
	}
	
	return &resp, nil
}

// receiveDeltaResponse receives and parses a delta response
func (ps *PhonebookSyncer) receiveDeltaResponse(reader *bufio.Reader) (*models.PhonebookDeltaResponse, error) {
	messageBytes, err := ps.readMessage(reader)
	if err != nil {
		return nil, err
	}
	
	var resp models.PhonebookDeltaResponse
	if err := proto.Unmarshal(messageBytes, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal delta response: %w", err)
	}
	
	return &resp, nil
}

// sendDigestResponse sends a digest response
func (pph *PhonebookProtocolHandler) sendDigestResponse(writer *bufio.Writer, resp *models.PhonebookDigestResponse) error {
	return pph.sendMessage(writer, resp)
}

// sendDeltaResponse sends a delta response
func (pph *PhonebookProtocolHandler) sendDeltaResponse(writer *bufio.Writer, resp *models.PhonebookDeltaResponse) error {
	return pph.sendMessage(writer, resp)
}

// sendMessage marshals and sends a protobuf message
func (ps *PhonebookSyncer) sendMessage(writer *bufio.Writer, message proto.Message) error {
	data, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}
	
	// Validate outgoing message size
	length := uint32(len(data))
	if length > ps.maxMessageSize {
		return fmt.Errorf("outgoing message too large: %d bytes (max %d)", length, ps.maxMessageSize)
	}
	
	// Write length (4 bytes, big-endian)
	lengthBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBytes, length)
	
	if _, err := writer.Write(lengthBytes); err != nil {
		return fmt.Errorf("failed to write message length: %w", err)
	}
	
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("failed to write message data: %w", err)
	}
	
	return writer.Flush()
}

// sendMessage marshals and sends a protobuf message (for protocol handler)
func (pph *PhonebookProtocolHandler) sendMessage(writer *bufio.Writer, message proto.Message) error {
	data, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}
	
	// Validate outgoing message size
	length := uint32(len(data))
	if length > pph.syncer.maxMessageSize {
		return fmt.Errorf("outgoing message too large: %d bytes (max %d)", length, pph.syncer.maxMessageSize)
	}
	
	// Write length (4 bytes, big-endian)
	lengthBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBytes, length)
	
	if _, err := writer.Write(lengthBytes); err != nil {
		return fmt.Errorf("failed to write message length: %w", err)
	}
	
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("failed to write message data: %w", err)
	}
	
	return writer.Flush()
}

// readMessage reads a length-prefixed protobuf message with size validation
func (ps *PhonebookSyncer) readMessage(reader *bufio.Reader) ([]byte, error) {
	// Read message length (4 bytes)
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(reader, lengthBytes); err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}
	
	length := binary.BigEndian.Uint32(lengthBytes)
	
	// CRITICAL: Validate message size for DoS protection
	if length > ps.maxMessageSize {
		return nil, fmt.Errorf("message too large: %d bytes (max %d)", length, ps.maxMessageSize)
	}
	
	if length == 0 {
		return nil, fmt.Errorf("invalid message length: 0")
	}
	
	// Read message data with guaranteed complete read
	messageBytes := make([]byte, length)
	if _, err := io.ReadFull(reader, messageBytes); err != nil {
		return nil, fmt.Errorf("failed to read message data: %w", err)
	}
	
	return messageBytes, nil
}

// readMessage reads a length-prefixed protobuf message (for protocol handler)
func (pph *PhonebookProtocolHandler) readMessage(reader *bufio.Reader) ([]byte, error) {
	// Read message length (4 bytes)
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(reader, lengthBytes); err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}
	
	length := binary.BigEndian.Uint32(lengthBytes)
	
	// CRITICAL: Validate message size for DoS protection
	if length > pph.syncer.maxMessageSize {
		return nil, fmt.Errorf("message too large: %d bytes (max %d)", length, pph.syncer.maxMessageSize)
	}
	
	if length == 0 {
		return nil, fmt.Errorf("invalid message length: 0")
	}
	
	// Read message data with guaranteed complete read
	messageBytes := make([]byte, length)
	if _, err := io.ReadFull(reader, messageBytes); err != nil {
		return nil, fmt.Errorf("failed to read message data: %w", err)
	}
	
	return messageBytes, nil
}