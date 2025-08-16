package node

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"time"

	"github.com/tareksalem/falak/internal/node/protobuf/models"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"google.golang.org/protobuf/proto"
)

const NEW_JOINER_PROTOCOL = "falak/1.0.0/peers/new-joiner"
const LEAVE_PROTOCOL = "falak/1.0.0/peers/leave"
const MEMBERSHIP_PROTOCOL = "falak/1.0.0/peers/membership"
const JOINED_PROTOCOL = "falak/1.0.0/peers/joined"
const CLUSTER_PROTOCOL = "falak/1.0.0/clusters"
const CLUSTER_JOINED_PROTOCOL = "falak/1.0.0/clusters/joined"
const CLUSTER_LEFT_PROTOCOL = "falak/1.0.0/clusters/left"
const CLUSTER_MEMBERSHIP_PROTOCOL = "falak/1.0.0/clusters/membership"
const CLUSTER_LEAVE_PROTOCOL = "falak/1.0.0/clusters/leave"
const JOIN_AUTH_PROTOCOL = "/falak/join/1.0"

func (n *Node) ConnectToProtocol(protocolId string) {
	n.host.SetStreamHandler(protocol.ID(protocolId), func(s network.Stream) {
		defer s.Close()
	})
}

func (n *Node) ConnectToInitialProtocols() {
	n.ConnectToProtocol(NEW_JOINER_PROTOCOL)
	n.ConnectToProtocol(LEAVE_PROTOCOL)
	n.ConnectToProtocol(MEMBERSHIP_PROTOCOL)
	n.ConnectToProtocol(JOINED_PROTOCOL)
	n.ConnectToProtocol(CLUSTER_PROTOCOL)
	n.ConnectToProtocol(CLUSTER_JOINED_PROTOCOL)
	n.ConnectToProtocol(CLUSTER_LEFT_PROTOCOL)
	n.ConnectToProtocol(CLUSTER_MEMBERSHIP_PROTOCOL)
	n.ConnectToProtocol(CLUSTER_LEAVE_PROTOCOL)
	
	// NOTE: Authentication protocol handler is registered in node.go NewNode() function
	// Do not register it here to avoid conflicts with the proper handler
}

// handleJoinProtocol handles the authentication protocol for joining nodes
func (n *Node) handleJoinProtocol(s network.Stream) {
	defer s.Close()
	
	if n.certManager == nil {
		log.Printf("Certificate manager not initialized, rejecting authentication request")
		return
	}
	
	reader := bufio.NewReader(s)
	writer := bufio.NewWriter(s)
	
	// Read ClientHello
	clientHello, err := n.readClientHello(reader)
	if err != nil {
		log.Printf("Failed to read ClientHello: %v", err)
		return
	}
	
	// Validate cluster ID
	clusterValid := n.validateClusterID(clientHello.ClusterId)
	
	// Verify client certificate
	if clusterValid {
		if err := n.certManager.VerifyPeerCertificate(clientHello.Certificate); err != nil {
			log.Printf("Client certificate verification failed: %v", err)
			clusterValid = false
		}
	}
	
	// Generate and send challenge
	challenge, err := n.sendServerChallenge(writer, clusterValid)
	if err != nil {
		log.Printf("Failed to send ServerChallenge: %v", err)
		return
	}
	
	if !clusterValid {
		return // Stop here if cluster/cert validation failed
	}
	
	// Read client response
	clientResponse, err := n.readClientResponse(reader)
	if err != nil {
		log.Printf("Failed to read ClientResponse: %v", err)
		return
	}
	
	// Verify signature
	authSuccess := n.verifyClientSignature(clientHello, clientResponse, challenge)
	
	// Send server acknowledgment
	if err := n.sendServerAck(writer, authSuccess, clientHello.NodeId); err != nil {
		log.Printf("Failed to send ServerAck: %v", err)
		return
	}
	
	if authSuccess {
		log.Printf("Successfully authenticated peer: %s", clientHello.NodeId)
	}
}

// readClientHello reads and parses the ClientHello message
func (n *Node) readClientHello(reader *bufio.Reader) (*models.ClientHello, error) {
	// Read message length
	lengthBuf := make([]byte, 4)
	if _, err := reader.Read(lengthBuf); err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}
	
	messageLength := binary.BigEndian.Uint32(lengthBuf)
	
	// Read message data
	messageBuf := make([]byte, messageLength)
	if _, err := reader.Read(messageBuf); err != nil {
		return nil, fmt.Errorf("failed to read message data: %w", err)
	}
	
	// Parse protobuf message
	var clientHello models.ClientHello
	if err := proto.Unmarshal(messageBuf, &clientHello); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ClientHello: %w", err)
	}
	
	return &clientHello, nil
}

// sendServerChallenge generates and sends a ServerChallenge message
func (n *Node) sendServerChallenge(writer *bufio.Writer, clusterValid bool) ([]byte, error) {
	// Generate random nonce
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}
	
	challenge := &models.ServerChallenge{
		Nonce:            nonce,
		Timestamp:        time.Now().Unix(),
		ClusterIdValid:   clusterValid,
		ErrorMessage:     "",
	}
	
	if !clusterValid {
		challenge.ErrorMessage = "Invalid cluster ID or certificate"
	}
	
	// Serialize message
	data, err := proto.Marshal(challenge)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal ServerChallenge: %w", err)
	}
	
	// Write length + data
	lengthBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBuf, uint32(len(data)))
	
	if _, err := writer.Write(lengthBuf); err != nil {
		return nil, fmt.Errorf("failed to write message length: %w", err)
	}
	
	if _, err := writer.Write(data); err != nil {
		return nil, fmt.Errorf("failed to write message data: %w", err)
	}
	
	if err := writer.Flush(); err != nil {
		return nil, fmt.Errorf("failed to flush writer: %w", err)
	}
	
	return nonce, nil
}

// readClientResponse reads and parses the ClientResponse message
func (n *Node) readClientResponse(reader *bufio.Reader) (*models.ClientResponse, error) {
	// Read message length
	lengthBuf := make([]byte, 4)
	if _, err := reader.Read(lengthBuf); err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}
	
	messageLength := binary.BigEndian.Uint32(lengthBuf)
	
	// Read message data
	messageBuf := make([]byte, messageLength)
	if _, err := reader.Read(messageBuf); err != nil {
		return nil, fmt.Errorf("failed to read message data: %w", err)
	}
	
	// Parse protobuf message
	var clientResponse models.ClientResponse
	if err := proto.Unmarshal(messageBuf, &clientResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ClientResponse: %w", err)
	}
	
	return &clientResponse, nil
}

// verifyClientSignature verifies the client's signature against the challenge
func (n *Node) verifyClientSignature(clientHello *models.ClientHello, clientResponse *models.ClientResponse, nonce []byte) bool {
	// Get peer ID from node ID
	peerID, err := peer.Decode(clientHello.NodeId)
	if err != nil {
		log.Printf("Invalid peer ID: %v", err)
		return false
	}
	
	// Extract public key from peer ID
	pubKey, err := peerID.ExtractPublicKey()
	if err != nil {
		log.Printf("Failed to extract public key: %v", err)
		return false
	}
	
	// Verify signature
	if err := VerifySignature(nonce, clientResponse.Signature, pubKey); err != nil {
		log.Printf("Signature verification failed: %v", err)
		return false
	}
	
	return true
}

// sendServerAck sends the ServerAck message with authentication result
func (n *Node) sendServerAck(writer *bufio.Writer, authSuccess bool, nodeID string) error {
	ack := &models.ServerAck{
		AuthSuccess:  authSuccess,
		ErrorMessage: "",
		Phonebook:    n.getPhonebookDelta(),
		ClusterState: make(map[string][]byte),
		Timestamp:    time.Now().Unix(),
	}
	
	if !authSuccess {
		ack.ErrorMessage = "Authentication failed"
		ack.Phonebook = nil
	}
	
	// Serialize message
	data, err := proto.Marshal(ack)
	if err != nil {
		return fmt.Errorf("failed to marshal ServerAck: %w", err)
	}
	
	// Write length + data
	lengthBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBuf, uint32(len(data)))
	
	if _, err := writer.Write(lengthBuf); err != nil {
		return fmt.Errorf("failed to write message length: %w", err)
	}
	
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("failed to write message data: %w", err)
	}
	
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("failed to flush writer: %w", err)
	}
	
	return nil
}

// validateClusterID validates if the requested cluster ID is valid for this node
func (n *Node) validateClusterID(clusterID string) bool {
	if len(n.clusters) == 0 {
		return true // No cluster restrictions
	}
	
	for _, cluster := range n.clusters {
		if cluster == clusterID {
			return true
		}
	}
	
	return false
}

// getPhonebookDelta returns a partial phonebook update for authenticated peers
func (n *Node) getPhonebookDelta() *models.PhonebookDelta {
	if n.phonebook == nil {
		return &models.PhonebookDelta{
			DataCenters: []*models.DataCenterUpdate{},
		}
	}
	
	return n.phonebook.GeneratePhonebookDelta()
}

// Legacy authenticateWithPeer - kept for backward compatibility but redirects to new implementation
func (n *Node) authenticateWithPeer(peerID peer.ID) error {
	clusterID := "default"
	if len(n.clusters) > 0 {
		clusterID = n.clusters[0]
	}
	return n.AuthenticateWithPeer(peerID, clusterID)
}
