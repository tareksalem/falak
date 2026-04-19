package authentication

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"google.golang.org/protobuf/proto"

	pb "github.com/tareksalem/falak/internal/node/protobuf/models"
)

// AuthClient handles outgoing authentication requests to other nodes
type AuthClient struct {
	manager    *AuthenticationManager
	host       host.Host
	protocolID protocol.ID
	cryptoMgr  *CryptoManager
}

// NewAuthClient creates a new authentication client
func NewAuthClient(manager *AuthenticationManager, host host.Host, cryptoMgr *CryptoManager) *AuthClient {
	return &AuthClient{
		manager:    manager,
		host:       host,
		protocolID: protocol.ID(manager.config.ProtocolID),
		cryptoMgr:  cryptoMgr,
	}
}

// AuthenticatePeer initiates authentication with the specified peer
func (ac *AuthClient) AuthenticatePeer(peerID peer.ID) error {
	log.Printf("🔐 Starting client authentication with peer: %s", peerID.ShortString())

	// Create session for this authentication
	session, err := ac.manager.createSession(peerID, true) // true = isClient
	if err != nil {
		return fmt.Errorf("failed to create client session: %w", err)
	}

	// Perform authentication in a goroutine to avoid blocking
	go func() {
		if err := ac.performAuthentication(session); err != nil {
			log.Printf("❌ Client authentication failed for peer %s: %v", peerID.ShortString(), err)

			// Emit failure event
			event := NewAuthEvent(EventTypeEnum.AuthenticationFailed, peerID, session.ID).
				WithClusterID(session.ClusterID).
				WithError(err).
				WithData(AuthenticationFailedData{
					Reason:    err.Error(),
					Retryable: ac.isRetryableError(err),
				})
			ac.manager.EmitEvent(event)

			// Disconnect the entire peer connection due to authentication failure
			ac.host.Network().ClosePeer(peerID)
			log.Printf("🔌 Disconnected peer %s due to authentication failure", peerID.ShortString())

			// Schedule retry if retry manager is available and enabled
			if retryManager := ac.manager.GetRetryManager(); retryManager != nil {
				retryFunc := func() error {
					return ac.AuthenticatePeer(peerID)
				}
				retryManager.ScheduleRetry(peerID, err, retryFunc)
			}

			// Clean up session
			ac.manager.cleanupSession(session.ID)
		}
	}()

	return nil
}

// performAuthentication executes the complete 4-step authentication protocol
func (ac *AuthClient) performAuthentication(session *AuthSession) error {
	// Step 0: Establish connection and stream
	stream, err := ac.establishStream(session.PeerID)
	if err != nil {
		return fmt.Errorf("failed to establish stream: %w", err)
	}

	// Set stream on session
	session.SetStream(stream)

	// Set overall authentication timeout
	deadline := time.Now().Add(ac.manager.config.HandshakeTimeout)
	stream.SetDeadline(deadline)

	// Step 1: Send ClientHello
	if err := ac.sendClientHello(stream, session); err != nil {
		stream.Close()
		return fmt.Errorf("failed to send ClientHello: %w", err)
	}

	// Step 2: Receive ServerChallenge
	serverChallenge, err := ac.receiveServerChallenge(stream)
	if err != nil {
		stream.Close()
		return fmt.Errorf("failed to receive ServerChallenge: %w", err)
	}

	// Validate ServerChallenge
	if err := ac.validateServerChallenge(serverChallenge, session); err != nil {
		stream.Close()
		return fmt.Errorf("ServerChallenge validation failed: %w", err)
	}

	// Step 3: Send ClientResponse
	if err := ac.sendClientResponse(stream, session, serverChallenge); err != nil {
		stream.Close()
		return fmt.Errorf("failed to send ClientResponse: %w", err)
	}

	// Step 4: Receive ServerAck
	serverAck, err := ac.receiveServerAck(stream)
	if err != nil {
		stream.Close()
		return fmt.Errorf("failed to receive ServerAck: %w", err)
	}

	// Process ServerAck
	if err := ac.processServerAck(serverAck, session); err != nil {
		stream.Close()
		return fmt.Errorf("ServerAck processing failed: %w", err)
	}

	// Authentication successful
	log.Printf("✅ Client authentication completed for peer: %s", session.PeerID.ShortString())

	// Mark success in retry manager to clear any retry state
	if retryManager := ac.manager.GetRetryManager(); retryManager != nil {
		retryManager.MarkSuccess(session.PeerID)
	}

	// Emit completion event
	event := NewAuthEvent(EventTypeEnum.AuthenticationCompleted, session.PeerID, session.ID).
		WithClusterID(session.ClusterID).
		WithData(AuthenticationCompletedData{
			ClusterID:    session.ClusterID,
			AuthDuration: session.GetDuration(),
			TrustLevel:   1.0, // Full trust after successful auth
		})
	ac.manager.EmitEvent(event)

	// Keep session alive for authenticated peer (don't cleanup)
	// The stream remains open for future communication
	log.Printf("🔗 Keeping authenticated session alive for peer: %s", session.PeerID.ShortString())

	return nil
}

// establishStream creates a new stream to the target peer
func (ac *AuthClient) establishStream(peerID peer.ID) (network.Stream, error) {
	log.Printf("🔗 Establishing stream to peer: %s", peerID.ShortString())

	// Create context with timeout for connection
	ctx, cancel := context.WithTimeout(context.Background(), ac.manager.config.HandshakeTimeout)
	defer cancel()

	// Open new stream with the authentication protocol
	stream, err := ac.host.NewStream(ctx, peerID, ac.protocolID)
	if err != nil {
		return nil, fmt.Errorf("failed to open stream to peer %s: %w", peerID, err)
	}

	log.Printf("✅ Stream established to peer: %s", peerID.ShortString())
	return stream, nil
}

// sendClientHello sends the initial hello message to the server
func (ac *AuthClient) sendClientHello(stream network.Stream, session *AuthSession) error {
	// Set message timeout
	deadline := time.Now().Add(ac.manager.config.MessageTimeout)
	stream.SetDeadline(deadline)

	// Get the certificate for the target cluster/datacenter
	clusterID := ac.manager.config.GetPrimaryCluster()
	dataCenterID := ac.manager.config.GetPrimaryDataCenter()

	var certificate []byte
	if ac.cryptoMgr != nil {
		certificate = ac.cryptoMgr.GetNodeCertificateForCluster(clusterID, dataCenterID)
		if certificate == nil {
			return fmt.Errorf("no certificate available for cluster %s, datacenter %s", clusterID, dataCenterID)
		}
	} else {
		// Fallback for when crypto manager is not yet integrated
		certificate = []byte{} // Will be replaced in Task 6.7
	}

	// Create ClientHello message
	clientHello := &pb.ClientHello{
		ClusterId:   clusterID,
		NodeId:      ac.manager.config.NodeID,
		Certificate: certificate,
		Metadata:    make(map[string]string),
		Timestamp:   time.Now().Unix(),
	}

	// Add metadata
	clientHello.Metadata["version"] = "1.0"
	clientHello.Metadata["capabilities"] = "basic"

	// Send message
	if err := ac.sendMessage(stream, clientHello); err != nil {
		return fmt.Errorf("failed to send ClientHello: %w", err)
	}

	// Update session state
	session.UpdateState(AuthStateEnum.ClientHelloSent)
	session.ClusterID = clientHello.ClusterId

	// Emit event
	event := NewAuthEvent(EventTypeEnum.ClientHelloSent, session.PeerID, session.ID).
		WithClusterID(session.ClusterID)
	ac.manager.EmitEvent(event)

	log.Printf("📤 Sent ClientHello to peer: %s, cluster: %s",
		session.PeerID.ShortString(), clientHello.ClusterId)

	return nil
}

// receiveServerChallenge receives and parses the server challenge
func (ac *AuthClient) receiveServerChallenge(stream network.Stream) (*pb.ServerChallenge, error) {
	// Set message timeout
	deadline := time.Now().Add(ac.manager.config.MessageTimeout)
	stream.SetDeadline(deadline)

	// Read message
	data, err := ac.readMessage(stream)
	if err != nil {
		return nil, fmt.Errorf("failed to read ServerChallenge: %w", err)
	}

	// Parse protobuf
	var serverChallenge pb.ServerChallenge
	if err := proto.Unmarshal(data, &serverChallenge); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ServerChallenge: %w", err)
	}

	log.Printf("📥 Received ServerChallenge from peer, cluster valid: %v",
		serverChallenge.ClusterIdValid)

	return &serverChallenge, nil
}

// validateServerChallenge validates the received server challenge
func (ac *AuthClient) validateServerChallenge(serverChallenge *pb.ServerChallenge, session *AuthSession) error {
	// Check if cluster ID is valid
	if !serverChallenge.ClusterIdValid {
		return ErrClusterMismatch(session.ClusterID, "rejected by server")
	}

	// Check for error message
	if serverChallenge.ErrorMessage != "" {
		return ErrProtocolViolation(session.PeerID.String(), session.ID, serverChallenge.ErrorMessage)
	}

	// Validate challenge data
	if len(serverChallenge.Nonce) == 0 {
		return ErrInvalidChallenge(session.PeerID.String(), session.ID)
	}

	// Validate timestamp (basic freshness check)
	msgTime := time.Unix(serverChallenge.Timestamp, 0)
	if time.Since(msgTime) > 5*time.Minute {
		return ErrProtocolViolation(session.PeerID.String(), session.ID, "ServerChallenge timestamp too old")
	}

	// Store challenge in session
	session.SetChallenge(serverChallenge.Nonce)

	// Emit event
	event := NewAuthEvent(EventTypeEnum.ServerChallengeReceived, session.PeerID, session.ID).
		WithClusterID(session.ClusterID)
	ac.manager.EmitEvent(event)

	return nil
}

// sendClientResponse signs the challenge and sends response to server
func (ac *AuthClient) sendClientResponse(stream network.Stream, session *AuthSession, serverChallenge *pb.ServerChallenge) error {
	// Set message timeout
	deadline := time.Now().Add(ac.manager.config.MessageTimeout)
	stream.SetDeadline(deadline)

	// Get the challenge
	challenge := session.GetChallenge()
	if len(challenge) == 0 {
		return ErrInvalidChallenge(session.PeerID.String(), session.ID)
	}

	// Generate signature using crypto manager
	var signature []byte
	var err error

	if ac.cryptoMgr != nil {
		// Use real Ed25519 signature
		clusterID := session.ClusterID
		dataCenterID := ac.manager.config.GetPrimaryDataCenter()
		signature, err = ac.cryptoMgr.CreateSignedChallengeResponse(
			challenge,
			ac.manager.config.NodeID,
			clusterID,
			dataCenterID,
			session.ID,
		)
		if err != nil {
			return fmt.Errorf("failed to generate signature: %w", err)
		}
	} else {
		// Fallback placeholder signature for when crypto manager is not yet integrated
		signature = make([]byte, 64) // Ed25519 signature size
		for i := range signature {
			signature[i] = challenge[i%len(challenge)] ^ byte(i)
		}
	}

	// Create ClientResponse message
	clientResponse := &pb.ClientResponse{
		Signature: signature,
		NodeId:    ac.manager.config.NodeID,
	}

	// Send message
	if err := ac.sendMessage(stream, clientResponse); err != nil {
		return fmt.Errorf("failed to send ClientResponse: %w", err)
	}

	// Update session state
	session.UpdateState(AuthStateEnum.ClientResponseSent)

	// Emit event
	event := NewAuthEvent(EventTypeEnum.ClientResponseSent, session.PeerID, session.ID).
		WithClusterID(session.ClusterID)
	ac.manager.EmitEvent(event)

	log.Printf("📤 Sent ClientResponse to peer: %s", session.PeerID.ShortString())
	return nil
}

// receiveServerAck receives the final acknowledgment from the server
func (ac *AuthClient) receiveServerAck(stream network.Stream) (*pb.ServerAck, error) {
	// Set message timeout
	deadline := time.Now().Add(ac.manager.config.MessageTimeout)
	stream.SetDeadline(deadline)

	// Read message
	data, err := ac.readMessage(stream)
	if err != nil {
		return nil, fmt.Errorf("failed to read ServerAck: %w", err)
	}

	// Parse protobuf
	var serverAck pb.ServerAck
	if err := proto.Unmarshal(data, &serverAck); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ServerAck: %w", err)
	}

	log.Printf("📥 Received ServerAck from peer, auth success: %v", serverAck.AuthSuccess)
	return &serverAck, nil
}

// processServerAck processes the final server acknowledgment
func (ac *AuthClient) processServerAck(serverAck *pb.ServerAck, session *AuthSession) error {
	// Check authentication result
	if !serverAck.AuthSuccess {
		return ErrProtocolViolation(session.PeerID.String(), session.ID,
			fmt.Sprintf("authentication rejected: %s", serverAck.ErrorMessage))
	}

	// Validate timestamp
	msgTime := time.Unix(serverAck.Timestamp, 0)
	if time.Since(msgTime) > 5*time.Minute {
		return ErrProtocolViolation(session.PeerID.String(), session.ID, "ServerAck timestamp too old")
	}

	// Process phonebook delta (will be implemented in Task 7)
	if serverAck.Phonebook != nil {
		log.Printf("📋 Received phonebook delta with %d datacenters (processing will be added in Task 7)",
			len(serverAck.Phonebook.DataCenters))

		// Emit phonebook update event
		event := NewAuthEvent(EventTypeEnum.PhonebookUpdateReceived, session.PeerID, session.ID).
			WithClusterID(session.ClusterID).
			WithData(PhonebookUpdateData{
				DataCenterID:  "multiple",
				PeerCount:     len(serverAck.Phonebook.DataCenters),
				Authoritative: true,
			})
		ac.manager.EmitEvent(event)
	}

	// Process cluster state
	if len(serverAck.ClusterState) > 0 {
		log.Printf("🏗️ Received cluster state with %d entries", len(serverAck.ClusterState))
	}

	// Update session state
	session.UpdateState(AuthStateEnum.Authenticated)

	// Emit event
	event := NewAuthEvent(EventTypeEnum.ServerAckReceived, session.PeerID, session.ID).
		WithClusterID(session.ClusterID)
	ac.manager.EmitEvent(event)

	return nil
}

// Message I/O utilities (same as server)

// readMessage reads a length-prefixed message from the stream
func (ac *AuthClient) readMessage(stream network.Stream) ([]byte, error) {
	// Read message length (4 bytes, big-endian)
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(stream, lengthBytes); err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}

	// Parse message length
	messageLength := uint32(lengthBytes[0])<<24 | uint32(lengthBytes[1])<<16 |
		uint32(lengthBytes[2])<<8 | uint32(lengthBytes[3])

	// Validate message length
	if messageLength == 0 || messageLength > 1024*1024 { // Max 1MB
		return nil, fmt.Errorf("invalid message length: %d", messageLength)
	}

	// Read message data
	messageData := make([]byte, messageLength)
	if _, err := io.ReadFull(stream, messageData); err != nil {
		return nil, fmt.Errorf("failed to read message data: %w", err)
	}

	return messageData, nil
}

// sendMessage sends a length-prefixed protobuf message to the stream
func (ac *AuthClient) sendMessage(stream network.Stream, message proto.Message) error {
	// Marshal message
	data, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Create length prefix (4 bytes, big-endian)
	messageLength := uint32(len(data))
	lengthBytes := []byte{
		byte(messageLength >> 24),
		byte(messageLength >> 16),
		byte(messageLength >> 8),
		byte(messageLength),
	}

	// Write length prefix
	if _, err := stream.Write(lengthBytes); err != nil {
		return fmt.Errorf("failed to write message length: %w", err)
	}

	// Write message data
	if _, err := stream.Write(data); err != nil {
		return fmt.Errorf("failed to write message data: %w", err)
	}

	return nil
}

// Helper methods

// isRetryableError determines if an authentication error is retryable
func (ac *AuthClient) isRetryableError(err error) bool {
	if authErr, ok := err.(AuthenticationError); ok {
		return authErr.Retryable
	}
	return true // Default to retryable for unknown errors
}
