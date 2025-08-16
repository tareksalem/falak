package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// ClusterCAManager manages the shared Root CA for the entire cluster
type ClusterCAManager struct {
	rootCA         *x509.Certificate
	rootCAKey      ed25519.PrivateKey
	certificateDir string
}

// NewClusterCAManager creates a new cluster CA manager
func NewClusterCAManager(certDir string) *ClusterCAManager {
	return &ClusterCAManager{
		certificateDir: certDir,
	}
}

// GenerateRootCA generates a new Root CA for the cluster
// This should be done ONCE for the entire cluster and the CA should be shared
func (cm *ClusterCAManager) GenerateRootCA(clusterName string) error {
	log.Printf("🔐 Generating new Root CA for cluster: %s", clusterName)

	// Generate Ed25519 key pair for the CA
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate Ed25519 key pair: %w", err)
	}

	// Create CA certificate template
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf("Falak Cluster Root CA - %s", clusterName),
			Organization: []string{"Falak Cluster"},
			Country:      []string{"US"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour), // Valid for 10 years
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            2,
	}

	// Generate the CA certificate (self-signed)
	caCertDER, err := x509.CreateCertificate(
		rand.Reader,
		caTemplate,
		caTemplate, // Self-signed
		publicKey,
		privateKey,
	)
	if err != nil {
		return fmt.Errorf("failed to create CA certificate: %w", err)
	}

	// Parse the certificate
	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	cm.rootCA = caCert
	cm.rootCAKey = privateKey

	// Save to disk
	if err := cm.SaveRootCA(); err != nil {
		return fmt.Errorf("failed to save Root CA: %w", err)
	}

	log.Printf("✅ Root CA generated and saved for cluster: %s", clusterName)
	log.Printf("   Subject: %s", caCert.Subject)
	log.Printf("   Valid from: %s to %s", caCert.NotBefore, caCert.NotAfter)
	log.Printf("   Serial: %s", caCert.SerialNumber)

	return nil
}

// LoadRootCA loads the Root CA from disk
func (cm *ClusterCAManager) LoadRootCA() error {
	caPath := filepath.Join(cm.certificateDir, "root-ca.crt")
	keyPath := filepath.Join(cm.certificateDir, "root-ca.key")

	// Load CA certificate
	caCertPEM, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("failed to read CA certificate: %w", err)
	}

	block, _ := pem.Decode(caCertPEM)
	if block == nil {
		return fmt.Errorf("failed to decode CA certificate PEM")
	}

	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	// Load CA private key
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("failed to read CA private key: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return fmt.Errorf("failed to decode CA private key PEM")
	}

	// Parse Ed25519 private key
	if keyBlock.Type != "ED25519 PRIVATE KEY" {
		return fmt.Errorf("unexpected key type: %s", keyBlock.Type)
	}

	if len(keyBlock.Bytes) != ed25519.PrivateKeySize {
		return fmt.Errorf("invalid Ed25519 private key size")
	}

	privateKey := ed25519.PrivateKey(keyBlock.Bytes)

	cm.rootCA = caCert
	cm.rootCAKey = privateKey

	log.Printf("✅ Root CA loaded from disk")
	log.Printf("   Subject: %s", caCert.Subject)
	log.Printf("   Valid from: %s to %s", caCert.NotBefore, caCert.NotAfter)

	return nil
}

// SaveRootCA saves the Root CA certificate and key to disk
func (cm *ClusterCAManager) SaveRootCA() error {
	// Create directory if it doesn't exist
	if err := os.MkdirAll(cm.certificateDir, 0700); err != nil {
		return fmt.Errorf("failed to create certificate directory: %w", err)
	}

	// Save CA certificate
	caPath := filepath.Join(cm.certificateDir, "root-ca.crt")
	caCertPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cm.rootCA.Raw,
	})

	if err := os.WriteFile(caPath, caCertPEM, 0600); err != nil {
		return fmt.Errorf("failed to write CA certificate: %w", err)
	}

	// Save CA private key (Ed25519)
	keyPath := filepath.Join(cm.certificateDir, "root-ca.key")
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "ED25519 PRIVATE KEY",
		Bytes: cm.rootCAKey,
	})

	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("failed to write CA private key: %w", err)
	}

	log.Printf("🔐 Root CA saved to: %s", cm.certificateDir)
	return nil
}

// GenerateNodeCertificate generates a node certificate signed by the Root CA
func (cm *ClusterCAManager) GenerateNodeCertificate(nodeID string, nodePublicKey ed25519.PublicKey) (*x509.Certificate, error) {
	if cm.rootCA == nil || cm.rootCAKey == nil {
		return nil, fmt.Errorf("Root CA not loaded")
	}

	// Create node certificate template
	template := &x509.Certificate{
		SerialNumber: generateSerialNumber(),
		Subject: pkix.Name{
			CommonName:   nodeID,
			Organization: []string{"Falak Node"},
		},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour), // Valid for 1 year
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost", "falak-node", nodeID},
	}

	// Generate the certificate signed by the Root CA
	certDER, err := x509.CreateCertificate(
		rand.Reader,
		template,
		cm.rootCA,      // Signed by Root CA
		nodePublicKey,  // Node's public key
		cm.rootCAKey,   // CA's private key for signing
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create node certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse node certificate: %w", err)
	}

	log.Printf("✅ Generated node certificate for: %s", nodeID)
	log.Printf("   Issuer: %s", cert.Issuer)
	log.Printf("   Subject: %s", cert.Subject)
	log.Printf("   Valid until: %s", cert.NotAfter)

	return cert, nil
}

// SaveNodeCertificate saves a node certificate to disk
func (cm *ClusterCAManager) SaveNodeCertificate(nodeID string, cert *x509.Certificate) error {
	nodeDir := filepath.Join(cm.certificateDir, "nodes", nodeID)
	if err := os.MkdirAll(nodeDir, 0700); err != nil {
		return fmt.Errorf("failed to create node directory: %w", err)
	}

	certPath := filepath.Join(nodeDir, "node.crt")
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	})

	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		return fmt.Errorf("failed to write node certificate: %w", err)
	}

	log.Printf("🔐 Node certificate saved to: %s", certPath)
	return nil
}

// LoadNodeCertificate loads a node certificate from disk
func (cm *ClusterCAManager) LoadNodeCertificate(nodeID string) (*x509.Certificate, error) {
	certPath := filepath.Join(cm.certificateDir, "nodes", nodeID, "node.crt")
	
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read node certificate: %w", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	log.Printf("✅ Node certificate loaded from disk for: %s", nodeID)
	return cert, nil
}

// VerifyNodeCertificate verifies a node certificate against the Root CA
func (cm *ClusterCAManager) VerifyNodeCertificate(cert *x509.Certificate) error {
	if cm.rootCA == nil {
		return fmt.Errorf("Root CA not loaded")
	}

	// Create a certificate pool with our Root CA
	roots := x509.NewCertPool()
	roots.AddCert(cm.rootCA)

	// Verify the certificate
	opts := x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}

	_, err := cert.Verify(opts)
	if err != nil {
		return fmt.Errorf("certificate verification failed: %w", err)
	}

	log.Printf("✅ Certificate verified successfully for: %s", cert.Subject.CommonName)
	return nil
}

// GetRootCA returns the Root CA certificate
func (cm *ClusterCAManager) GetRootCA() *x509.Certificate {
	return cm.rootCA
}

// HasRootCA checks if a Root CA is loaded
func (cm *ClusterCAManager) HasRootCA() bool {
	return cm.rootCA != nil && cm.rootCAKey != nil
}

// ExportRootCACertificate exports just the Root CA certificate (for distribution to nodes)
func (cm *ClusterCAManager) ExportRootCACertificate(outputPath string) error {
	if cm.rootCA == nil {
		return fmt.Errorf("Root CA not loaded")
	}

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cm.rootCA.Raw,
	})

	if err := os.WriteFile(outputPath, certPEM, 0644); err != nil {
		return fmt.Errorf("failed to export Root CA certificate: %w", err)
	}

	log.Printf("✅ Root CA certificate exported to: %s", outputPath)
	log.Printf("   Distribute this certificate to all nodes in the cluster")
	return nil
}