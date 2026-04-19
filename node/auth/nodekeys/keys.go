// Package nodekeys handles per-cluster node key pair generation and storage.
package nodekeys

import (
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/libp2p/go-libp2p/core/crypto"
)

const (
	// KeyFileName is the name of the private key file.
	KeyFileName = "node.key"
	// CertFileName is the name of the certificate file.
	CertFileName = "certificate.pem"
	// CACertFileName is the name of the cluster CA certificate file (auto mode).
	CACertFileName = "ca.pem"
	// ClustersDir is the subdirectory for cluster-specific data.
	ClustersDir = "clusters"
)

// NodeKeyPair represents a node's key pair for a specific cluster.
type NodeKeyPair struct {
	PrivateKey  crypto.PrivKey
	PublicKey   crypto.PubKey
	ClusterPath string
}

// Manager handles node key operations for clusters.
type Manager struct {
	dataDir string
}

// NewManager creates a new node key manager.
func NewManager(dataDir string) *Manager {
	return &Manager{dataDir: dataDir}
}

// clusterDirName converts a cluster path to a flat directory name.
// e.g., "us-east/dc1/prod" becomes "us-east-dc1-prod"
func clusterDirName(clusterPath string) string {
	return strings.ReplaceAll(clusterPath, "/", "-")
}

// ClusterDir returns the directory path for a cluster's keys.
func (m *Manager) ClusterDir(clusterPath string) string {
	return filepath.Join(m.dataDir, ClustersDir, clusterDirName(clusterPath))
}

// KeyPath returns the path to the private key file for a cluster.
func (m *Manager) KeyPath(clusterPath string) string {
	return filepath.Join(m.ClusterDir(clusterPath), KeyFileName)
}

// CertPath returns the path to the certificate file for a cluster.
func (m *Manager) CertPath(clusterPath string) string {
	return filepath.Join(m.ClusterDir(clusterPath), CertFileName)
}

// CACertPath returns the path to the cluster CA certificate file for a cluster.
func (m *Manager) CACertPath(clusterPath string) string {
	return filepath.Join(m.ClusterDir(clusterPath), CACertFileName)
}

// LoadOrCreate loads an existing key pair or creates a new one for the cluster.
func (m *Manager) LoadOrCreate(clusterPath string) (*NodeKeyPair, error) {
	keyPath := m.KeyPath(clusterPath)

	// Try to load existing key
	if _, err := os.Stat(keyPath); err == nil {
		return m.Load(clusterPath)
	}

	// Generate new key pair
	return m.Generate(clusterPath)
}

// Load loads an existing key pair from disk.
func (m *Manager) Load(clusterPath string) (*NodeKeyPair, error) {
	keyPath := m.KeyPath(clusterPath)

	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}

	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("unexpected PEM block type: %s", block.Type)
	}

	privKey, err := crypto.UnmarshalPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal private key: %w", err)
	}

	return &NodeKeyPair{
		PrivateKey:  privKey,
		PublicKey:   privKey.GetPublic(),
		ClusterPath: clusterPath,
	}, nil
}

// Generate creates a new Ed25519 key pair and saves it to disk.
func (m *Manager) Generate(clusterPath string) (*NodeKeyPair, error) {
	// Generate new Ed25519 key pair
	privKey, pubKey, err := crypto.GenerateKeyPairWithReader(crypto.Ed25519, -1, rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate key pair: %w", err)
	}

	keyPair := &NodeKeyPair{
		PrivateKey:  privKey,
		PublicKey:   pubKey,
		ClusterPath: clusterPath,
	}

	// Save to disk
	if err := m.Save(keyPair); err != nil {
		return nil, err
	}

	return keyPair, nil
}

// Save saves a key pair to disk.
func (m *Manager) Save(keyPair *NodeKeyPair) error {
	clusterDir := m.ClusterDir(keyPair.ClusterPath)

	// Create cluster directory if it doesn't exist
	if err := os.MkdirAll(clusterDir, 0700); err != nil {
		return fmt.Errorf("failed to create cluster directory: %w", err)
	}

	// Marshal private key
	keyBytes, err := crypto.MarshalPrivateKey(keyPair.PrivateKey)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}

	// Encode as PEM
	pemBlock := &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: keyBytes,
	}

	// Write to file with restricted permissions
	keyPath := m.KeyPath(keyPair.ClusterPath)
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(pemBlock), 0600); err != nil {
		return fmt.Errorf("failed to write key file: %w", err)
	}

	return nil
}

// Exists checks if a key pair exists for the cluster.
func (m *Manager) Exists(clusterPath string) bool {
	_, err := os.Stat(m.KeyPath(clusterPath))
	return err == nil
}

// CertExists checks if a certificate exists for the cluster.
func (m *Manager) CertExists(clusterPath string) bool {
	_, err := os.Stat(m.CertPath(clusterPath))
	return err == nil
}

// PublicKeyBytes returns the marshaled public key bytes.
func (kp *NodeKeyPair) PublicKeyBytes() ([]byte, error) {
	return crypto.MarshalPublicKey(kp.PublicKey)
}

// Sign signs data with the node's private key.
func (kp *NodeKeyPair) Sign(data []byte) ([]byte, error) {
	return kp.PrivateKey.Sign(data)
}
