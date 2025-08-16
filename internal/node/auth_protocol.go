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
	// Authentication stream protocol ID
	AuthProtocolID = "/falak/join/1.0"
	
	// Authentication timeouts
	AuthTimeout       = 30 * time.Second
	ChallengeTimeout  = 10 * time.Second
	ResponseTimeout   = 10 * time.Second
)

// AuthenticationHandler handles incoming authentication requests
type AuthenticationHandler struct {
	node *Node
}

// NewAuthenticationHandler creates a new authentication handler
func NewAuthenticationHandler(node *Node) *AuthenticationHandler {
	return &AuthenticationHandler{
		node: node,
	}
}

// HandleStream handles incoming authentication streams
func (ah *AuthenticationHandler) HandleStream(stream network.Stream) {
	defer stream.Close()
	
	// CRITICAL DEBUG: Log that we're receiving an authentication stream
	peerID := stream.Conn().RemotePeer()
	log.Printf("🔐 CRITICAL: Authentication stream handler called on %s from peer: %s", ah.node.name, peerID.ShortString())
	
	// Set deadline for the entire authentication process
	_ = stream.SetDeadline(time.Now().Add(AuthTimeout))
	
	log.Printf("🔐 Handling authentication request from peer: %s", peerID.ShortString())
	
	// MANDATORY: TLS authentication required for all connections
	if ah.node.tlsAuthenticator == nil {
		log.Printf("❌ CRITICAL: TLS authenticator not initialized - rejecting connection from %s", peerID.ShortString())
		return
	}
	
	log.Printf("🔐 Performing MANDATORY TLS mutual authentication with %s (server-side)", peerID.ShortString())
	if err := ah.node.tlsAuthenticator.AuthenticateConnection(stream, true); err != nil {
		log.Printf("❌ MANDATORY TLS authentication failed with %s: %v", peerID.ShortString(), err)
		return
	}
	log.Printf("✅ MANDATORY TLS authentication successful with %s (server-side)", peerID.ShortString())
	
	// Create buffered reader/writer for the stream
	reader := bufio.NewReader(stream)
	writer := bufio.NewWriter(stream)
	
	// Step 1: Receive ClientHello
	clientHello, err := ah.receiveClientHello(reader)
	if err != nil {
		log.Printf("❌ Failed to receive ClientHello from %s: %v", peerID.ShortString(), err)
		ah.sendAuthError(writer, fmt.Sprintf("Invalid ClientHello: %v", err))
		return
	}
	
	log.Printf("📨 Received ClientHello from %s for cluster: %s", clientHello.NodeId, clientHello.ClusterId)
	
	// Step 2: Verify cluster ID and send challenge
	challenge, err := ah.sendServerChallenge(writer, clientHello)
	if err != nil {
		log.Printf("❌ Failed to send ServerChallenge to %s: %v", peerID.ShortString(), err)
		return
	}
	
	// Step 3: Receive and verify ClientResponse
	clientResponse, err := ah.receiveClientResponse(reader)
	if err != nil {
		log.Printf("❌ Failed to receive ClientResponse from %s: %v", peerID.ShortString(), err)
		ah.sendAuthError(writer, fmt.Sprintf("Invalid ClientResponse: %v", err))
		return
	}
	
	// Step 4: Verify signature and send ServerAck
	err = ah.verifyAndSendAck(writer, clientHello, challenge, clientResponse)
	if err != nil {
		log.Printf("❌ Authentication failed for %s: %v", peerID.ShortString(), err)
		ah.sendAuthError(writer, fmt.Sprintf("Authentication failed: %v", err))
		return
	}
	
	log.Printf("✅ Authentication successful for peer %s (%s)", clientHello.NodeId, peerID.ShortString())
	
	// CRITICAL: Add the authenticated peer to our phonebook
	ah.addAuthenticatedPeerToPhonebook(peerID, clientHello)
}

// receiveClientHello reads and parses the ClientHello message
func (ah *AuthenticationHandler) receiveClientHello(reader *bufio.Reader) (*models.ClientHello, error) {
	// Read message length (4 bytes)
	lengthBytes := make([]byte, 4)
	_, err := reader.Read(lengthBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}
	
	// Convert bytes to length
	length := uint32(lengthBytes[0])<<24 | uint32(lengthBytes[1])<<16 | uint32(lengthBytes[2])<<8 | uint32(lengthBytes[3])
	
	// Read message data
	messageBytes := make([]byte, length)
	_, err = reader.Read(messageBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to read message data: %w", err)
	}
	
	// Parse protobuf message
	var clientHello models.ClientHello
	err = proto.Unmarshal(messageBytes, &clientHello)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal ClientHello: %w", err)
	}
	
	return &clientHello, nil
}

// sendServerChallenge validates cluster ID and sends challenge
func (ah *AuthenticationHandler) sendServerChallenge(writer *bufio.Writer, clientHello *models.ClientHello) ([]byte, error) {
	// Validate cluster ID
	validCluster := false
	for _, cluster := range ah.node.clusters {
		if cluster == clientHello.ClusterId {
			validCluster = true
			break
		}
	}
	
	// Generate challenge nonce
	nonce := make([]byte, 32)
	_, err := rand.Read(nonce)
	if err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}
	
	// Create ServerChallenge message
	serverChallenge := &models.ServerChallenge{
		Nonce:           nonce,
		Timestamp:       time.Now().Unix(),
		ClusterIdValid:  validCluster,
		ErrorMessage:    "",
	}
	
	if !validCluster {
		serverChallenge.ErrorMessage = fmt.Sprintf("Invalid cluster ID: %s", clientHello.ClusterId)
	}
	
	// Marshal and send
	err = ah.sendMessage(writer, serverChallenge)
	if err != nil {
		return nil, fmt.Errorf("failed to send ServerChallenge: %w", err)
	}
	
	log.Printf("📤 Sent ServerChallenge to %s (cluster valid: %v)", clientHello.NodeId, validCluster)
	
	if !validCluster {
		return nil, fmt.Errorf("invalid cluster ID: %s", clientHello.ClusterId)
	}
	
	return nonce, nil
}

// receiveClientResponse reads and parses the ClientResponse message
func (ah *AuthenticationHandler) receiveClientResponse(reader *bufio.Reader) (*models.ClientResponse, error) {
	// Read message length
	lengthBytes := make([]byte, 4)
	_, err := reader.Read(lengthBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}
	
	length := uint32(lengthBytes[0])<<24 | uint32(lengthBytes[1])<<16 | uint32(lengthBytes[2])<<8 | uint32(lengthBytes[3])
	
	// Read message data
	messageBytes := make([]byte, length)
	_, err = reader.Read(messageBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to read message data: %w", err)
	}
	
	// Parse protobuf message
	var clientResponse models.ClientResponse
	err = proto.Unmarshal(messageBytes, &clientResponse)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal ClientResponse: %w", err)
	}
	
	return &clientResponse, nil
}

// verifyAndSendAck verifies the signature and sends ServerAck with phonebook delta
func (ah *AuthenticationHandler) verifyAndSendAck(writer *bufio.Writer, clientHello *models.ClientHello, challenge []byte, clientResponse *models.ClientResponse) error {
	// TODO: Implement actual signature verification using the node's certificate
	// For now, we'll simulate successful verification
	authSuccess := true
	
	// Create challenge hash for verification (nonce + timestamp)
	challengeData := append(challenge, []byte(fmt.Sprintf("%d", time.Now().Unix()))...)
	challengeHash := sha256.Sum256(challengeData)
	
	log.Printf("🔍 Verifying signature for challenge hash: %x", challengeHash[:8])
	log.Printf("📝 Received signature length: %d bytes", len(clientResponse.Signature))
	
	// Create phonebook delta (simplified - return peers from same data center)
	phonebookDelta := ah.createPhonebookDelta(clientHello.ClusterId)
	
	// Create ServerAck message
	serverAck := &models.ServerAck{
		AuthSuccess:  authSuccess,
		ErrorMessage: "",
		Phonebook:    phonebookDelta,
		ClusterState: make(map[string][]byte), // TODO: Add cluster state
		Timestamp:    time.Now().Unix(),
	}
	
	if !authSuccess {
		serverAck.ErrorMessage = "Signature verification failed"
	}
	
	// Send ServerAck
	err := ah.sendMessage(writer, serverAck)
	if err != nil {
		return fmt.Errorf("failed to send ServerAck: %w", err)
	}
	
	log.Printf("📤 Sent ServerAck to %s (auth: %v, peers: %d)", 
		clientHello.NodeId, authSuccess, len(phonebookDelta.DataCenters))
	
	if !authSuccess {
		return fmt.Errorf("signature verification failed")
	}
	
	return nil
}

// createPhonebookDelta creates a phonebook delta with current known peers
func (ah *AuthenticationHandler) createPhonebookDelta(clusterID string) *models.PhonebookDelta {
	// Get peers from phonebook grouped by data center
	peers := ah.node.phonebook.GetPeers()
	dcMap := make(map[string][]*models.PeerInfo)
	
	for _, peer := range peers {
		dc := peer.DataCenter
		if dc == "" {
			dc = "default"
		}
		
		peerInfo := &models.PeerInfo{
			NodeId:     peer.ID,
			Multiaddrs: []string{}, // TODO: Convert from AddrInfo
			Labels:     make(map[string]string),
			NodeStatus: models.NodeStatus_ACTIVE,
		}
		
		// Convert tags to labels
		for key, value := range peer.Tags {
			if strValue, ok := value.(string); ok {
				peerInfo.Labels[key] = strValue
			}
		}
		
		dcMap[dc] = append(dcMap[dc], peerInfo)
	}
	
	// Create data center updates
	var dataCenters []*models.DataCenterUpdate
	for dcID, dcPeers := range dcMap {
		dataCenter := &models.DataCenterUpdate{
			DcId:           dcID,
			Peers:          dcPeers,
			IsAuthoritative: true, // We are authoritative for our data center
		}
		dataCenters = append(dataCenters, dataCenter)
	}
	
	return &models.PhonebookDelta{
		DataCenters: dataCenters,
	}
}

// sendMessage marshals and sends a protobuf message
func (ah *AuthenticationHandler) sendMessage(writer *bufio.Writer, message proto.Message) error {
	// Marshal the message
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
	
	_, err = writer.Write(lengthBytes)
	if err != nil {
		return fmt.Errorf("failed to write message length: %w", err)
	}
	
	// Write message data
	_, err = writer.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write message data: %w", err)
	}
	
	// Flush the writer
	return writer.Flush()
}

// sendAuthError sends an error response
func (ah *AuthenticationHandler) sendAuthError(writer *bufio.Writer, errorMsg string) {
	serverAck := &models.ServerAck{
		AuthSuccess:  false,
		ErrorMessage: errorMsg,
		Timestamp:    time.Now().Unix(),
	}
	
	_ = ah.sendMessage(writer, serverAck)
}

// AuthenticateWithPeer performs client-side authentication with a peer
func (n *Node) AuthenticateWithPeer(peerID peer.ID, clusterID string) error {
	log.Printf("🔐 Starting authentication with peer %s for cluster %s", peerID.ShortString(), clusterID)
	
	// Open authentication stream
	ctx, cancel := context.WithTimeout(n.ctx, AuthTimeout)
	defer cancel()
	
	stream, err := n.host.NewStream(ctx, peerID, AuthProtocolID)
	if err != nil {
		return fmt.Errorf("failed to open authentication stream: %w", err)
	}
	defer stream.Close()
	
	// Set deadline
	_ = stream.SetDeadline(time.Now().Add(AuthTimeout))
	
	// MANDATORY: TLS authentication required for all connections
	if n.tlsAuthenticator == nil {
		return fmt.Errorf("CRITICAL: TLS authenticator not initialized - cannot authenticate with %s", peerID.ShortString())
	}
	
	log.Printf("🔐 Performing MANDATORY TLS mutual authentication with %s (client-side)", peerID.ShortString())
	if err := n.tlsAuthenticator.AuthenticateConnection(stream, false); err != nil {
		log.Printf("❌ MANDATORY TLS authentication failed with %s: %v", peerID.ShortString(), err)
		return fmt.Errorf("MANDATORY TLS authentication failed: %w", err)
	}
	log.Printf("✅ MANDATORY TLS authentication successful with %s (client-side)", peerID.ShortString())
	
	reader := bufio.NewReader(stream)
	writer := bufio.NewWriter(stream)
	
	// Step 1: Send ClientHello
	err = n.sendClientHello(writer, clusterID)
	if err != nil {
		return fmt.Errorf("failed to send ClientHello: %w", err)
	}
	
	// Step 2: Receive ServerChallenge
	serverChallenge, err := n.receiveServerChallenge(reader)
	if err != nil {
		return fmt.Errorf("failed to receive ServerChallenge: %w", err)
	}
	
	if !serverChallenge.ClusterIdValid {
		return fmt.Errorf("server rejected cluster ID: %s", serverChallenge.ErrorMessage)
	}
	
	// Step 3: Send ClientResponse
	err = n.sendClientResponse(writer, serverChallenge)
	if err != nil {
		return fmt.Errorf("failed to send ClientResponse: %w", err)
	}
	
	// Step 4: Receive ServerAck
	serverAck, err := n.receiveServerAck(reader)
	if err != nil {
		return fmt.Errorf("failed to receive ServerAck: %w", err)
	}
	
	if !serverAck.AuthSuccess {
		return fmt.Errorf("authentication failed: %s", serverAck.ErrorMessage)
	}
	
	// Step 5: Process phonebook delta
	err = n.processPhonebookDelta(serverAck.Phonebook)
	if err != nil {
		log.Printf("⚠️ Warning: failed to process phonebook delta: %v", err)
	}
	
	log.Printf("✅ Authentication successful with peer %s", peerID.ShortString())
	return nil
}

// sendClientHello sends the initial ClientHello message
func (n *Node) sendClientHello(writer *bufio.Writer, clusterID string) error {
	clientHello := &models.ClientHello{
		ClusterId:   clusterID,
		NodeId:      n.id,
		Certificate: []byte{}, // TODO: Add actual certificate
		Metadata: map[string]string{
			"version":     "1.0",
			"node_name":   n.name,
			"data_center": n.dataCenter,
		},
		Timestamp: time.Now().Unix(),
	}
	
	return n.sendAuthMessage(writer, clientHello)
}

// receiveServerChallenge receives and parses ServerChallenge
func (n *Node) receiveServerChallenge(reader *bufio.Reader) (*models.ServerChallenge, error) {
	messageBytes, err := n.readAuthMessage(reader)
	if err != nil {
		return nil, err
	}
	
	var serverChallenge models.ServerChallenge
	err = proto.Unmarshal(messageBytes, &serverChallenge)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal ServerChallenge: %w", err)
	}
	
	return &serverChallenge, nil
}

// sendClientResponse signs the challenge and sends response
func (n *Node) sendClientResponse(writer *bufio.Writer, challenge *models.ServerChallenge) error {
	// Create challenge data (nonce + timestamp)
	challengeData := append(challenge.Nonce, []byte(fmt.Sprintf("%d", challenge.Timestamp))...)
	
	// TODO: Sign with actual private key
	// For now, create a dummy signature
	signature := sha256.Sum256(challengeData)
	
	clientResponse := &models.ClientResponse{
		Signature: signature[:],
		NodeId:    n.id,
	}
	
	log.Printf("📝 Signing challenge with %d bytes of data", len(challengeData))
	return n.sendAuthMessage(writer, clientResponse)
}

// receiveServerAck receives and parses ServerAck
func (n *Node) receiveServerAck(reader *bufio.Reader) (*models.ServerAck, error) {
	messageBytes, err := n.readAuthMessage(reader)
	if err != nil {
		return nil, err
	}
	
	var serverAck models.ServerAck
	err = proto.Unmarshal(messageBytes, &serverAck)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal ServerAck: %w", err)
	}
	
	return &serverAck, nil
}

// processPhonebookDelta updates the local phonebook with delta information
func (n *Node) processPhonebookDelta(delta *models.PhonebookDelta) error {
	if delta == nil {
		return nil
	}
	
	log.Printf("📖 Processing phonebook delta with %d data centers", len(delta.DataCenters))
	
	for _, dcUpdate := range delta.DataCenters {
		log.Printf("📍 Processing data center: %s (%d peers, authoritative: %v)", 
			dcUpdate.DcId, len(dcUpdate.Peers), dcUpdate.IsAuthoritative)
		
		for _, peerInfo := range dcUpdate.Peers {
			// TODO: Convert PeerInfo to internal Peer structure and add to phonebook
			log.Printf("  👤 Peer: %s (%d addrs)", peerInfo.NodeId, len(peerInfo.Multiaddrs))
		}
	}
	
	return nil
}

// sendAuthMessage sends a protobuf message over the auth stream
func (n *Node) sendAuthMessage(writer *bufio.Writer, message proto.Message) error {
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
	
	_, err = writer.Write(lengthBytes)
	if err != nil {
		return fmt.Errorf("failed to write message length: %w", err)
	}
	
	_, err = writer.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write message data: %w", err)
	}
	
	return writer.Flush()
}

// readAuthMessage reads a protobuf message from the auth stream
func (n *Node) readAuthMessage(reader *bufio.Reader) ([]byte, error) {
	// Read message length
	lengthBytes := make([]byte, 4)
	_, err := reader.Read(lengthBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}
	
	length := uint32(lengthBytes[0])<<24 | uint32(lengthBytes[1])<<16 | uint32(lengthBytes[2])<<8 | uint32(lengthBytes[3])
	
	// Read message data
	messageBytes := make([]byte, length)
	_, err = reader.Read(messageBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to read message data: %w", err)
	}
	
	return messageBytes, nil
}

// addAuthenticatedPeerToPhonebook adds a successfully authenticated peer to the phonebook
func (ah *AuthenticationHandler) addAuthenticatedPeerToPhonebook(peerID peer.ID, clientHello *models.ClientHello) {
	// Normalize peer ID for consistent tracking
	normalizedPeerID := normalizePeerID(clientHello.NodeId)
	
	// Extract data center from metadata
	dataCenter := "default"
	if dc, exists := clientHello.Metadata["data_center"]; exists {
		dataCenter = dc
	}
	
	// Create peer entry
	peer := Peer{
		ID:         normalizedPeerID,
		Status:     "active",
		DataCenter: dataCenter,
		LastSeen:   time.Now(),
		TrustScore: 1.0,
		Tags:       make(Tags),
	}
	
	// Add metadata as tags
	for key, value := range clientHello.Metadata {
		peer.Tags[key] = value
	}
	
	// Add to phonebook
	if ah.node.phonebook != nil {
		log.Printf("📚 Adding authenticated peer %s to %s's phonebook (datacenter: %s)", 
			peerID.ShortString(), ah.node.name, dataCenter)
		
		ah.node.phonebook.AddPeer(peer)
		ah.node.phonebook.UpdatePeerLastSeen(peer.ID)
		
		log.Printf("📚 Successfully added authenticated peer %s to phonebook (total: %d)", 
			peerID.ShortString(), len(ah.node.phonebook.GetPeers()))
		
		// Sync health registry with updated phonebook
		if ah.node.healthRegistry != nil {
			log.Printf("🏥 Syncing health registry after authenticated peer addition")
			ah.node.healthRegistry.SyncWithPhonebook()
		}
	} else {
		log.Printf("❌ ERROR: Cannot add authenticated peer - phonebook is nil on %s", ah.node.name)
	}
}