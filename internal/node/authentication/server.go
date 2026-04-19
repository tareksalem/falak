package authentication

import (
	"crypto/rand"
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

// AuthServer handles incoming authentication requests from other nodes
type AuthServer struct {
	manager    *AuthenticationManager
	host       host.Host
	protocolID protocol.ID
	cryptoMgr  *CryptoManager
}

// NewAuthServer creates a new authentication server
func NewAuthServer(manager *AuthenticationManager, host host.Host, cryptoMgr *CryptoManager) *AuthServer {
	return &AuthServer{
		manager:    manager,
		host:       host,
		protocolID: protocol.ID(manager.config.ProtocolID),
		cryptoMgr:  cryptoMgr,
	}
}

// Start registers the protocol handler and starts the server
func (as *AuthServer) Start() error {
	log.Printf("🔌 Starting Authentication Server on protocol: %s", as.protocolID)

	// Register the protocol handler with libp2p
	as.host.SetStreamHandler(as.protocolID, as.handleIncomingStream)

	log.Printf("✅ Authentication Server started successfully")
	return nil
}

// Stop unregisters the protocol handler
func (as *AuthServer) Stop() error {
	log.Printf("🔌 Stopping Authentication Server...")

	// Remove the stream handler
	as.host.RemoveStreamHandler(as.protocolID)

	log.Printf("✅ Authentication Server stopped")
	return nil
}

// handleIncomingStream handles new authentication streams from clients
func (as *AuthServer) handleIncomingStream(stream network.Stream) {
	peerID := stream.Conn().RemotePeer()

	log.Printf("🔐 Incoming authentication stream from peer: %s", peerID.ShortString())

	// Set stream deadline
	deadline := time.Now().Add(as.manager.config.HandshakeTimeout)
	stream.SetDeadline(deadline)

	// Handle the authentication protocol
	if err := as.handleAuthenticationProtocol(stream, peerID); err != nil {
		log.Printf("❌ Authentication failed for peer %s: %v", peerID.ShortString(), err)

		// Close stream on error
		stream.Close()

		// Disconnect the entire peer connection due to authentication failure
		as.host.Network().ClosePeer(peerID)
		log.Printf("🔌 Disconnected peer %s due to authentication failure", peerID.ShortString())
		return
	}

	log.Printf("✅ Authentication completed for peer: %s", peerID.ShortString())
}

// handleAuthenticationProtocol implements the server side of the 4-step protocol
func (as *AuthServer) handleAuthenticationProtocol(stream network.Stream, peerID peer.ID) error {
	// Step 1: Receive ClientHello
	clientHello, err := as.receiveClientHello(stream)
	if err != nil {
		return fmt.Errorf("failed to receive ClientHello: %w", err)
	}

	// Create session for this authentication
	session, err := as.manager.createSession(peerID, false) // false = isClient
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}

	// Set the stream on the session
	session.SetStream(stream)

	// Emit ClientHello received event
	event := NewAuthEvent(EventTypeEnum.ClientHelloReceived, peerID, session.ID).
		WithClusterID(clientHello.ClusterId)
	as.manager.EmitEvent(event)

	// Validate ClientHello
	if err := as.validateClientHello(clientHello, session); err != nil {
		as.manager.cleanupSession(session.ID)
		return fmt.Errorf("ClientHello validation failed: %w", err)
	}

	// Step 2: Send ServerChallenge
	if err := as.sendServerChallenge(stream, session); err != nil {
		as.manager.cleanupSession(session.ID)
		return fmt.Errorf("failed to send ServerChallenge: %w", err)
	}

	// Step 3: Receive ClientResponse
	clientResponse, err := as.receiveClientResponse(stream)
	if err != nil {
		as.manager.cleanupSession(session.ID)
		return fmt.Errorf("failed to receive ClientResponse: %w", err)
	}

	// Validate ClientResponse
	if err := as.validateClientResponse(clientResponse, session); err != nil {
		as.manager.cleanupSession(session.ID)
		return fmt.Errorf("ClientResponse validation failed: %w", err)
	}

	// Step 4: Send ServerAck
	if err := as.sendServerAck(stream, session, true); err != nil {
		as.manager.cleanupSession(session.ID)
		return fmt.Errorf("failed to send ServerAck: %w", err)
	}

	// Authentication successful - emit completion event
	event = NewAuthEvent(EventTypeEnum.AuthenticationCompleted, peerID, session.ID).
		WithClusterID(session.ClusterID).
		WithData(AuthenticationCompletedData{
			ClusterID:    session.ClusterID,
			AuthDuration: session.GetDuration(),
			TrustLevel:   1.0, // Full trust after successful auth
		})
	as.manager.EmitEvent(event)

	// Keep session alive for authenticated peer (don't cleanup)
	// The stream remains open for future communication
	log.Printf("🔗 Keeping authenticated session alive for peer: %s", peerID.ShortString())

	return nil
}

// receiveClientHello receives and parses the ClientHello message
func (as *AuthServer) receiveClientHello(stream network.Stream) (*pb.ClientHello, error) {
	// Set message timeout
	deadline := time.Now().Add(as.manager.config.MessageTimeout)
	stream.SetDeadline(deadline)

	// Read message
	data, err := as.readMessage(stream)
	if err != nil {
		return nil, fmt.Errorf("failed to read ClientHello: %w", err)
	}

	// Parse protobuf
	var clientHello pb.ClientHello
	if err := proto.Unmarshal(data, &clientHello); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ClientHello: %w", err)
	}

	log.Printf("📨 Received ClientHello from node: %s, cluster: %s",
		clientHello.NodeId, clientHello.ClusterId)

	return &clientHello, nil
}

// validateClientHello validates the received ClientHello message
func (as *AuthServer) validateClientHello(clientHello *pb.ClientHello, session *AuthSession) error {
	// Check cluster ID
	if !as.manager.config.HasCluster(clientHello.ClusterId) {
		return ErrClusterMismatch(as.manager.config.GetPrimaryCluster(), clientHello.ClusterId)
	}

	// Validate timestamp (basic freshness check)
	msgTime := time.Unix(clientHello.Timestamp, 0)
	if time.Since(msgTime) > 5*time.Minute {
		return ErrProtocolViolation(session.PeerID.String(), session.ID, "ClientHello timestamp too old")
	}

	// Update session with cluster ID
	session.ClusterID = clientHello.ClusterId

	// Real certificate validation using crypto manager
	if len(clientHello.Certificate) > 0 {
		if as.cryptoMgr == nil {
			log.Printf("⚠️ Certificate received but crypto manager not available - falling back to basic validation")
		} else {
			// Validate the client's certificate and extract public key
			publicKey, err := as.cryptoMgr.VerifyPeerCertificateAndExtractKey(
				session.PeerID,
				clientHello.Certificate,
				clientHello.ClusterId,
				session.ID,
			)
			if err != nil {
				return ErrCertificateChainInvalid(session.PeerID.String(), session.ID, err)
			}

			// Store the verified public key in session for signature verification
			session.SetPeerPublicKey(publicKey)
			log.Printf("✅ Certificate validated and public key extracted for peer: %s",
				session.PeerID.ShortString())
		}
	} else {
		log.Printf("⚠️ No certificate provided by peer: %s", session.PeerID.ShortString())
	}

	return nil
}

// sendServerChallenge sends a challenge to the client
func (as *AuthServer) sendServerChallenge(stream network.Stream, session *AuthSession) error {
	// Generate random challenge
	challenge := make([]byte, 32) // 256-bit challenge
	if _, err := rand.Read(challenge); err != nil {
		return fmt.Errorf("failed to generate challenge: %w", err)
	}

	// Store challenge in session
	session.SetChallenge(challenge)

	// Create ServerChallenge message
	serverChallenge := &pb.ServerChallenge{
		Nonce:          challenge,
		Timestamp:      time.Now().Unix(),
		ClusterIdValid: true,
		ErrorMessage:   "",
	}

	// Send message
	if err := as.sendMessage(stream, serverChallenge); err != nil {
		return fmt.Errorf("failed to send ServerChallenge: %w", err)
	}

	// Update session state
	session.UpdateState(AuthStateEnum.ServerChallengeSent)

	// Emit event
	event := NewAuthEvent(EventTypeEnum.ServerChallengeSent, session.PeerID, session.ID).
		WithClusterID(session.ClusterID)
	as.manager.EmitEvent(event)

	log.Printf("🔐 Sent ServerChallenge to peer: %s", session.PeerID.ShortString())
	return nil
}

// receiveClientResponse receives and parses the ClientResponse message
func (as *AuthServer) receiveClientResponse(stream network.Stream) (*pb.ClientResponse, error) {
	// Set message timeout
	deadline := time.Now().Add(as.manager.config.MessageTimeout)
	stream.SetDeadline(deadline)

	// Read message
	data, err := as.readMessage(stream)
	if err != nil {
		return nil, fmt.Errorf("failed to read ClientResponse: %w", err)
	}

	// Parse protobuf
	var clientResponse pb.ClientResponse
	if err := proto.Unmarshal(data, &clientResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ClientResponse: %w", err)
	}

	log.Printf("📨 Received ClientResponse from node: %s", clientResponse.NodeId)
	return &clientResponse, nil
}

// validateClientResponse validates the signature in ClientResponse
func (as *AuthServer) validateClientResponse(clientResponse *pb.ClientResponse, session *AuthSession) error {
	// Verify the node ID matches
	if clientResponse.NodeId != session.PeerID.String() {
		return ErrProtocolViolation(session.PeerID.String(), session.ID,
			"NodeId mismatch in ClientResponse")
	}

	// Get the challenge we sent
	challenge := session.GetChallenge()
	if len(challenge) == 0 {
		return ErrInvalidChallenge(session.PeerID.String(), session.ID)
	}

	// Real signature verification using crypto manager
	if len(clientResponse.Signature) == 0 {
		return ErrInvalidResponse(session.PeerID.String(), session.ID)
	}

	// Verify the signature length
	if as.cryptoMgr != nil && !as.cryptoMgr.IsValidSignatureLength(clientResponse.Signature) {
		return ErrSignatureInvalid(session.PeerID.String(), session.ID, "invalid signature length")
	}

	if as.cryptoMgr == nil {
		log.Printf("⚠️ Signature received but crypto manager not available - skipping verification")
	} else {
		// Get the peer's public key from the session (extracted during certificate validation)
		peerPublicKey := session.GetPeerPublicKey()
		if peerPublicKey == nil {
			return ErrKeyMismatch(session.PeerID.String(), session.ID, "no public key available for signature verification")
		}

		// Verify the Ed25519 signature
		err := as.cryptoMgr.ValidateChallengeResponse(
			clientResponse.Signature,
			challenge,
			clientResponse.NodeId,
			session.ClusterID,
			session.ID,
			peerPublicKey,
		)
		if err != nil {
			return ErrSignatureInvalid(session.PeerID.String(), session.ID, err.Error())
		}

		log.Printf("✅ Signature verified successfully for peer: %s", session.PeerID.ShortString())
	}

	// Emit event
	event := NewAuthEvent(EventTypeEnum.ClientResponseReceived, session.PeerID, session.ID).
		WithClusterID(session.ClusterID)
	as.manager.EmitEvent(event)

	return nil
}

// sendServerAck sends the final acknowledgment to the client
func (as *AuthServer) sendServerAck(stream network.Stream, session *AuthSession, success bool) error {
	// Create ServerAck message
	serverAck := &pb.ServerAck{
		AuthSuccess:  success,
		ErrorMessage: "",
		Timestamp:    time.Now().Unix(),
	}

	// Add phonebook delta (will be implemented in Task 7)
	// For now, send empty phonebook
	serverAck.Phonebook = &pb.PhonebookDelta{
		DataCenters: []*pb.DataCenterUpdate{}, // Empty for now
	}

	// Add cluster state (placeholder)
	serverAck.ClusterState = make(map[string][]byte)

	// Send message
	if err := as.sendMessage(stream, serverAck); err != nil {
		return fmt.Errorf("failed to send ServerAck: %w", err)
	}

	// Update session state
	session.UpdateState(AuthStateEnum.ServerAckSent)

	// Emit event
	event := NewAuthEvent(EventTypeEnum.ServerAckSent, session.PeerID, session.ID).
		WithClusterID(session.ClusterID)
	as.manager.EmitEvent(event)

	log.Printf("✅ Sent ServerAck to peer: %s (success: %v)", session.PeerID.ShortString(), success)
	return nil
}

// Message I/O utilities

// readMessage reads a length-prefixed message from the stream
func (as *AuthServer) readMessage(stream network.Stream) ([]byte, error) {
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
func (as *AuthServer) sendMessage(stream network.Stream, message proto.Message) error {
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

// Update the AuthServer stub in manager.go to use the real implementation
func (as *AuthServer) RegisterProtocol() error {
	return as.Start()
}
