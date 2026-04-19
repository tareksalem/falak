package authentication

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// CryptoManager handles Ed25519 cryptographic operations
type CryptoManager struct {
	certManager *CertificateManager
}

// NewCryptoManager creates a new crypto manager
func NewCryptoManager(certManager *CertificateManager) *CryptoManager {
	return &CryptoManager{
		certManager: certManager,
	}
}

// SignaturePayload represents the data structure that gets signed
type SignaturePayload struct {
	Challenge   []byte    // The challenge from server
	NodeID      string    // Node identifier
	ClusterID   string    // Cluster identifier
	Timestamp   time.Time // Signature timestamp
	SessionID   string    // Session identifier for uniqueness
}

// GenerateChallenge creates a cryptographically secure random challenge
func (cm *CryptoManager) GenerateChallenge() ([]byte, error) {
	challenge := make([]byte, 32) // 256-bit challenge
	if _, err := rand.Read(challenge); err != nil {
		return nil, fmt.Errorf("failed to generate random challenge: %w", err)
	}
	return challenge, nil
}

// SignChallenge signs a challenge using the node's private key for a specific cluster/datacenter
func (cm *CryptoManager) SignChallenge(challenge []byte, nodeID, clusterID, dataCenterID, sessionID string) ([]byte, error) {
	// Get the private key for the specific cluster/datacenter
	privateKey := cm.certManager.GetNodePrivateKey(clusterID, dataCenterID)
	if privateKey == nil {
		return nil, fmt.Errorf("no private key found for cluster %s, datacenter %s", clusterID, dataCenterID)
	}

	// Create signature payload
	payload := SignaturePayload{
		Challenge:   challenge,
		NodeID:      nodeID,
		ClusterID:   clusterID,
		Timestamp:   time.Now(),
		SessionID:   sessionID,
	}

	// Serialize the payload for signing
	payloadBytes, err := cm.serializeSignaturePayload(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize signature payload: %w", err)
	}

	// Sign the payload
	signature := ed25519.Sign(privateKey, payloadBytes)

	return signature, nil
}

// VerifySignature verifies a signature against a challenge using the peer's public key
func (cm *CryptoManager) VerifySignature(signature, challenge []byte, nodeID, clusterID, sessionID string, peerPublicKey ed25519.PublicKey) error {
	// Reconstruct the signature payload that should have been signed
	// We need to try different timestamps since we don't know the exact timestamp used
	now := time.Now()

	// Try timestamps within a reasonable window (±5 minutes)
	for offset := -5 * time.Minute; offset <= 5*time.Minute; offset += time.Second {
		testTimestamp := now.Add(offset)

		payload := SignaturePayload{
			Challenge:   challenge,
			NodeID:      nodeID,
			ClusterID:   clusterID,
			Timestamp:   testTimestamp,
			SessionID:   sessionID,
		}

		// Serialize the test payload
		payloadBytes, err := cm.serializeSignaturePayload(payload)
		if err != nil {
			continue // Skip this timestamp if serialization fails
		}

		// Verify the signature
		if ed25519.Verify(peerPublicKey, payloadBytes, signature) {
			// Check if timestamp is within acceptable range (5 minutes)
			if time.Since(testTimestamp).Abs() <= 5*time.Minute {
				return nil // Signature is valid and timestamp is acceptable
			}
			return fmt.Errorf("signature valid but timestamp too old: %v", testTimestamp)
		}
	}

	return fmt.Errorf("signature verification failed")
}

// VerifySignatureWithTimestamp verifies a signature with a known timestamp (for more efficient verification)
func (cm *CryptoManager) VerifySignatureWithTimestamp(signature, challenge []byte, nodeID, clusterID, sessionID string, timestamp time.Time, peerPublicKey ed25519.PublicKey) error {
	// Check timestamp freshness first
	if time.Since(timestamp).Abs() > 5*time.Minute {
		return fmt.Errorf("signature timestamp too old: %v", timestamp)
	}

	// Reconstruct the signature payload
	payload := SignaturePayload{
		Challenge:   challenge,
		NodeID:      nodeID,
		ClusterID:   clusterID,
		Timestamp:   timestamp,
		SessionID:   sessionID,
	}

	// Serialize the payload
	payloadBytes, err := cm.serializeSignaturePayload(payload)
	if err != nil {
		return fmt.Errorf("failed to serialize signature payload: %w", err)
	}

	// Verify the signature
	if !ed25519.Verify(peerPublicKey, payloadBytes, signature) {
		return fmt.Errorf("signature verification failed")
	}

	return nil
}

// serializeSignaturePayload converts a SignaturePayload to bytes for signing/verification
func (cm *CryptoManager) serializeSignaturePayload(payload SignaturePayload) ([]byte, error) {
	// Use a deterministic serialization format
	// Format: challenge_len(4) + challenge + nodeID_len(4) + nodeID + clusterID_len(4) + clusterID + timestamp(8) + sessionID_len(4) + sessionID

	nodeIDBytes := []byte(payload.NodeID)
	clusterIDBytes := []byte(payload.ClusterID)
	sessionIDBytes := []byte(payload.SessionID)
	timestampBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(timestampBytes, uint64(payload.Timestamp.Unix()))

	// Calculate total size
	totalSize := 4 + len(payload.Challenge) + 4 + len(nodeIDBytes) + 4 + len(clusterIDBytes) + 8 + 4 + len(sessionIDBytes)

	result := make([]byte, 0, totalSize)

	// Serialize challenge
	challengeLen := make([]byte, 4)
	binary.BigEndian.PutUint32(challengeLen, uint32(len(payload.Challenge)))
	result = append(result, challengeLen...)
	result = append(result, payload.Challenge...)

	// Serialize nodeID
	nodeIDLen := make([]byte, 4)
	binary.BigEndian.PutUint32(nodeIDLen, uint32(len(nodeIDBytes)))
	result = append(result, nodeIDLen...)
	result = append(result, nodeIDBytes...)

	// Serialize clusterID
	clusterIDLen := make([]byte, 4)
	binary.BigEndian.PutUint32(clusterIDLen, uint32(len(clusterIDBytes)))
	result = append(result, clusterIDLen...)
	result = append(result, clusterIDBytes...)

	// Serialize timestamp
	result = append(result, timestampBytes...)

	// Serialize sessionID
	sessionIDLen := make([]byte, 4)
	binary.BigEndian.PutUint32(sessionIDLen, uint32(len(sessionIDBytes)))
	result = append(result, sessionIDLen...)
	result = append(result, sessionIDBytes...)

	return result, nil
}

// GenerateSecureNonce generates a cryptographically secure nonce
func (cm *CryptoManager) GenerateSecureNonce(size int) ([]byte, error) {
	if size <= 0 || size > 1024 {
		return nil, fmt.Errorf("invalid nonce size: %d (must be 1-1024)", size)
	}

	nonce := make([]byte, size)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to generate secure nonce: %w", err)
	}
	return nonce, nil
}

// HashData creates a SHA-256 hash of the input data
func (cm *CryptoManager) HashData(data []byte) []byte {
	hash := sha256.Sum256(data)
	return hash[:]
}

// VerifyPeerCertificateAndExtractKey verifies a peer's certificate and extracts the public key
func (cm *CryptoManager) VerifyPeerCertificateAndExtractKey(peerID peer.ID, certData []byte, clusterID, sessionID string) (ed25519.PublicKey, error) {
	// Validate the certificate using the certificate manager
	cert, err := cm.certManager.ValidatePeerCertificate(peerID, certData, clusterID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("certificate validation failed: %w", err)
	}

	// Extract the Ed25519 public key from the certificate
	publicKey, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, ErrKeyMismatch(peerID.String(), sessionID, "certificate does not contain Ed25519 public key")
	}

	return publicKey, nil
}

// CreateSignedChallengeResponse creates a signed response to a server challenge
func (cm *CryptoManager) CreateSignedChallengeResponse(challenge []byte, nodeID, clusterID, dataCenterID, sessionID string) ([]byte, error) {
	// This is a wrapper around SignChallenge for clarity in client code
	return cm.SignChallenge(challenge, nodeID, clusterID, dataCenterID, sessionID)
}

// ValidateChallengeResponse validates a client's signed challenge response
func (cm *CryptoManager) ValidateChallengeResponse(signature, challenge []byte, nodeID, clusterID, sessionID string, peerPublicKey ed25519.PublicKey) error {
	// This is a wrapper around VerifySignature for clarity in server code
	return cm.VerifySignature(signature, challenge, nodeID, clusterID, sessionID, peerPublicKey)
}

// GetNodePublicKeyForCluster gets the public key for a specific cluster/datacenter
func (cm *CryptoManager) GetNodePublicKeyForCluster(clusterID, dataCenterID string) ed25519.PublicKey {
	return cm.certManager.GetNodePublicKey(clusterID, dataCenterID)
}

// GetNodeCertificateForCluster gets the certificate for a specific cluster/datacenter
func (cm *CryptoManager) GetNodeCertificateForCluster(clusterID, dataCenterID string) []byte {
	return cm.certManager.GetNodeCertificate(clusterID, dataCenterID)
}

// IsValidSignatureLength checks if a signature has the correct length for Ed25519
func (cm *CryptoManager) IsValidSignatureLength(signature []byte) bool {
	return len(signature) == ed25519.SignatureSize
}

// IsValidPublicKeyLength checks if a public key has the correct length for Ed25519
func (cm *CryptoManager) IsValidPublicKeyLength(publicKey []byte) bool {
	return len(publicKey) == ed25519.PublicKeySize
}

// IsValidPrivateKeyLength checks if a private key has the correct length for Ed25519
func (cm *CryptoManager) IsValidPrivateKeyLength(privateKey []byte) bool {
	return len(privateKey) == ed25519.PrivateKeySize
}

// SecureCompare performs a constant-time comparison of two byte slices
func (cm *CryptoManager) SecureCompare(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}

	var result byte
	for i := 0; i < len(a); i++ {
		result |= a[i] ^ b[i]
	}

	return result == 0
}

// ZeroBytes securely clears a byte slice
func (cm *CryptoManager) ZeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}