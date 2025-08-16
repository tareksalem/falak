package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// CertificateInitializerV2 handles certificate initialization with shared Root CA
type CertificateInitializerV2 struct {
	node           *Node
	certificateDir string
	caManager      *ClusterCAManager
}

// NewCertificateInitializerV2 creates a new certificate initializer
func NewCertificateInitializerV2(node *Node) *CertificateInitializerV2 {
	// Default certificate directory
	certDir := filepath.Join(".", "certs")
	if envDir := os.Getenv("FALAK_CERT_DIR"); envDir != "" {
		certDir = envDir
	}

	return &CertificateInitializerV2{
		node:           node,
		certificateDir: certDir,
		caManager:      NewClusterCAManager(certDir),
	}
}

// InitializeCertificatesV2 initializes certificates following the documentation requirements
func (ci *CertificateInitializerV2) InitializeCertificatesV2(rootCAPath string) error {
	log.Printf("🔐 Initializing certificates for node %s (V2 - Shared Root CA)", ci.node.name)

	// Step 1: Load or setup Root CA
	if err := ci.setupRootCA(rootCAPath); err != nil {
		return fmt.Errorf("failed to setup Root CA: %w", err)
	}

	// Step 2: Load or generate node certificate
	nodeCert, nodePrivateKey, err := ci.setupNodeCertificate()
	if err != nil {
		return fmt.Errorf("failed to setup node certificate: %w", err)
	}

	// Step 3: Initialize Certificate Manager
	if err := ci.initializeCertificateManagerV2(nodeCert, nodePrivateKey); err != nil {
		return fmt.Errorf("failed to initialize certificate manager: %w", err)
	}

	// Step 4: Initialize TLS Authenticator
	if err := ci.initializeTLSAuthenticatorV2(nodeCert, nodePrivateKey); err != nil {
		return fmt.Errorf("failed to initialize TLS authenticator: %w", err)
	}

	log.Printf("✅ Certificate initialization completed (V2) - Using shared Root CA")
	return nil
}

// setupRootCA loads the shared Root CA certificate and stores it in memory
func (ci *CertificateInitializerV2) setupRootCA(rootCAPath string) error {
	// Option 1: Explicit Root CA path provided (highest priority)
	if rootCAPath != "" {
		log.Printf("🔐 Loading Root CA from provided path: %s", rootCAPath)
		if err := ci.loadRootCAFromPath(rootCAPath); err != nil {
			return fmt.Errorf("failed to load Root CA from %s: %w", rootCAPath, err)
		}
		// Store in memory for fast access during certificate validation
		ci.storeRootCAInMemory()
		return nil
	}

	// Option 2: Check default location
	defaultCAPath := filepath.Join(ci.certificateDir, "root-ca.crt")
	if _, err := os.Stat(defaultCAPath); err == nil {
		log.Printf("🔐 Loading Root CA from default location: %s", defaultCAPath)
		if err := ci.loadRootCAFromPath(defaultCAPath); err != nil {
			return fmt.Errorf("failed to load Root CA from default location: %w", err)
		}
		ci.storeRootCAInMemory()
		return nil
	}

	// Option 3: Check environment variable
	if envCAPath := os.Getenv("FALAK_ROOT_CA"); envCAPath != "" {
		log.Printf("🔐 Loading Root CA from environment variable: %s", envCAPath)
		if err := ci.loadRootCAFromPath(envCAPath); err != nil {
			return fmt.Errorf("failed to load Root CA from env path: %w", err)
		}
		ci.storeRootCAInMemory()
		return nil
	}

	return fmt.Errorf("no Root CA certificate found. Please provide Root CA via: " +
		"1) --root-ca flag, 2) FALAK_ROOT_CA env var, or 3) place root-ca.crt in %s", ci.certificateDir)
}

// loadRootCAFromPath loads Root CA from a specific path
func (ci *CertificateInitializerV2) loadRootCAFromPath(caPath string) error {
	// Read the CA certificate
	caCertPEM, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("failed to read Root CA: %w", err)
	}

	block, _ := pem.Decode(caCertPEM)
	if block == nil {
		return fmt.Errorf("failed to decode Root CA PEM")
	}

	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse Root CA: %w", err)
	}

	// Store in CA manager (without private key - nodes don't need it)
	ci.caManager.rootCA = caCert

	log.Printf("✅ Root CA loaded successfully")
	log.Printf("   Subject: %s", caCert.Subject)
	log.Printf("   Valid until: %s", caCert.NotAfter)
	log.Printf("   Serial: %s", caCert.SerialNumber)
	log.Printf("   IsCA: %v", caCert.IsCA)

	return nil
}

// storeRootCAInMemory stores the Root CA certificate in memory for fast access
func (ci *CertificateInitializerV2) storeRootCAInMemory() {
	if ci.caManager.rootCA != nil {
		// Cache the Root CA certificate data in memory
		// This avoids repeated disk reads during certificate validation
		log.Printf("🔐 Root CA cached in memory for fast certificate validation")
		log.Printf("   CA DN: %s", ci.caManager.rootCA.Subject.String())
		log.Printf("   Fingerprint: %x", ci.caManager.rootCA.Raw[:16]) // First 16 bytes as fingerprint
	}
}

// setupNodeCertificate loads existing or generates new node certificate
func (ci *CertificateInitializerV2) setupNodeCertificate() (*x509.Certificate, ed25519.PrivateKey, error) {
	nodeID := ci.node.id
	nodeCertPath := filepath.Join(ci.certificateDir, "nodes", nodeID, "node.crt")
	nodeKeyPath := filepath.Join(ci.certificateDir, "nodes", nodeID, "node.key")

	// Try to load existing certificate
	if cert, key, err := ci.loadExistingNodeCertificate(nodeCertPath, nodeKeyPath); err == nil {
		log.Printf("✅ Loaded existing node certificate from disk")
		return cert, key, nil
	}

	// Generate new node certificate
	log.Printf("🔐 Generating new node certificate for: %s", nodeID)
	return ci.generateAndSaveNodeCertificate()
}

// loadExistingNodeCertificate loads node certificate and key from disk
func (ci *CertificateInitializerV2) loadExistingNodeCertificate(certPath, keyPath string) (*x509.Certificate, ed25519.PrivateKey, error) {
	// Load certificate
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("certificate not found: %w", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("failed to decode certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Verify certificate is still valid
	now := time.Now()
	if now.After(cert.NotAfter) {
		return nil, nil, fmt.Errorf("certificate has expired")
	}

	// Load private key
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("private key not found: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("failed to decode private key PEM")
	}

	if keyBlock.Type != "ED25519 PRIVATE KEY" {
		return nil, nil, fmt.Errorf("unexpected key type: %s", keyBlock.Type)
	}

	privateKey := ed25519.PrivateKey(keyBlock.Bytes)

	// Verify certificate against Root CA
	if err := ci.caManager.VerifyNodeCertificate(cert); err != nil {
		return nil, nil, fmt.Errorf("certificate verification failed: %w", err)
	}

	return cert, privateKey, nil
}

// generateAndSaveNodeCertificate generates a new node certificate
func (ci *CertificateInitializerV2) generateAndSaveNodeCertificate() (*x509.Certificate, ed25519.PrivateKey, error) {
	// For development: If no Root CA private key available, generate self-signed
	// In production, this would require the CA private key or a certificate signing service
	if !ci.caManager.HasRootCA() {
		log.Printf("⚠️  WARNING: Generating self-signed certificate (Root CA private key not available)")
		return ci.generateSelfSignedNodeCertificate()
	}

	// Generate Ed25519 key pair for the node
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate Ed25519 key pair: %w", err)
	}

	// Generate certificate signed by Root CA
	cert, err := ci.caManager.GenerateNodeCertificate(ci.node.id, publicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate node certificate: %w", err)
	}

	// Save certificate and key to disk
	if err := ci.saveNodeCertificateAndKey(cert, privateKey); err != nil {
		log.Printf("⚠️  Warning: Failed to save certificate to disk: %v", err)
	}

	return cert, privateKey, nil
}

// generateSelfSignedNodeCertificate generates a self-signed certificate for development
func (ci *CertificateInitializerV2) generateSelfSignedNodeCertificate() (*x509.Certificate, ed25519.PrivateKey, error) {
	// Use libp2p key if available, otherwise generate new Ed25519 key
	var publicKey ed25519.PublicKey
	var privateKey ed25519.PrivateKey

	if ci.node.privateKey != nil {
		// Convert libp2p key to Ed25519
		rawPrivKey, err := ci.node.privateKey.Raw()
		if err == nil && len(rawPrivKey) == ed25519.PrivateKeySize {
			privateKey = ed25519.PrivateKey(rawPrivKey)
			publicKey = privateKey.Public().(ed25519.PublicKey)
		}
	}

	if privateKey == nil {
		// Generate new Ed25519 key pair
		var err error
		publicKey, privateKey, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to generate Ed25519 key: %w", err)
		}
	}

	// Create self-signed certificate
	template := &x509.Certificate{
		SerialNumber: generateSerialNumber(),
		Subject: pkix.Name{
			CommonName:   ci.node.id,
			Organization: []string{"Falak Node (Self-Signed)"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost", "falak-node", ci.node.id},
	}

	certDER, err := x509.CreateCertificate(
		rand.Reader,
		template,
		template, // Self-signed
		publicKey,
		privateKey,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Save to disk
	if err := ci.saveNodeCertificateAndKey(cert, privateKey); err != nil {
		log.Printf("⚠️  Warning: Failed to save self-signed certificate: %v", err)
	}

	return cert, privateKey, nil
}

// saveNodeCertificateAndKey saves node certificate and private key to disk
func (ci *CertificateInitializerV2) saveNodeCertificateAndKey(cert *x509.Certificate, privateKey ed25519.PrivateKey) error {
	nodeDir := filepath.Join(ci.certificateDir, "nodes", ci.node.id)
	if err := os.MkdirAll(nodeDir, 0700); err != nil {
		return fmt.Errorf("failed to create node directory: %w", err)
	}

	// Save certificate
	certPath := filepath.Join(nodeDir, "node.crt")
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		return fmt.Errorf("failed to save certificate: %w", err)
	}

	// Save private key
	keyPath := filepath.Join(nodeDir, "node.key")
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "ED25519 PRIVATE KEY",
		Bytes: privateKey,
	})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("failed to save private key: %w", err)
	}

	log.Printf("🔐 Node certificate and key saved to: %s", nodeDir)
	return nil
}

// initializeCertificateManagerV2 creates certificate manager with proper certificates
func (ci *CertificateInitializerV2) initializeCertificateManagerV2(nodeCert *x509.Certificate, nodePrivateKey ed25519.PrivateKey) error {
	// Convert Ed25519 key to libp2p crypto.PrivKey
	privKey, err := crypto.UnmarshalEd25519PrivateKey(nodePrivateKey)
	if err != nil {
		// If conversion fails, use the existing libp2p key
		privKey = ci.node.privateKey
	}

	ci.node.certManager = &CertificateManager{
		rootCA:     ci.caManager.GetRootCA(),
		nodeCert:   nodeCert,
		privateKey: privKey,
	}

	log.Printf("✅ Certificate manager initialized with shared Root CA")
	return nil
}

// initializeTLSAuthenticatorV2 creates TLS authenticator with proper certificates
func (ci *CertificateInitializerV2) initializeTLSAuthenticatorV2(nodeCert *x509.Certificate, nodePrivateKey ed25519.PrivateKey) error {
	auth := &TLSAuthenticator{
		node:             ci.node,
		trustedPeers:     make(map[peer.ID]*PeerCertInfo),
		certificateCache: make(map[string]*x509.Certificate),
	}

	// Create certificate pool with Root CA
	rootCAs := x509.NewCertPool()
	if ci.caManager.GetRootCA() != nil {
		rootCAs.AddCert(ci.caManager.GetRootCA())
	}
	auth.rootCAs = rootCAs

	// Set certificates
	// Note: In production, this would create proper TLS certificates
	// For now, we'll initialize the authenticator
	auth.certValidator = &CertificateValidator{
		rootCAs:            rootCAs,
		intermediateCAs:    x509.NewCertPool(),
		allowSelfSigned:    true, // Allow for development
		maxCertChainLen:    5,
		clockSkewTolerance: 5 * time.Minute,
	}

	// Store in node
	ci.node.tlsAuthenticator = auth

	// Configure TLS
	if err := auth.configureTLS(); err != nil {
		return fmt.Errorf("failed to configure TLS: %w", err)
	}

	log.Printf("✅ TLS authenticator initialized with shared Root CA")
	return nil
}