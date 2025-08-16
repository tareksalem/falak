package node

import (
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// CertificateInitializer handles automatic certificate generation and initialization
type CertificateInitializer struct {
	node *Node
}

// NewCertificateInitializer creates a new certificate initializer
func NewCertificateInitializer(node *Node) *CertificateInitializer {
	return &CertificateInitializer{
		node: node,
	}
}

// InitializeCertificates ensures the node has proper certificates for TLS authentication
// This function ALWAYS creates certificates, either from provided paths or auto-generated
func (ci *CertificateInitializer) InitializeCertificates(rootCAPath, nodeCertPath, nodeKeyPath string) error {
	log.Printf("🔐 Initializing certificates for node %s", ci.node.name)

	// Phase 1: Initialize Certificate Manager (always required)
	certManager, err := ci.initializeCertificateManager(rootCAPath, nodeCertPath)
	if err != nil {
		return fmt.Errorf("failed to initialize certificate manager: %w", err)
	}
	ci.node.certManager = certManager
	log.Printf("✅ Certificate manager initialized successfully")

	// Phase 2: Initialize TLS Authenticator (always required)
	tlsAuth, err := ci.initializeTLSAuthenticator(rootCAPath, nodeCertPath, nodeKeyPath)
	if err != nil {
		return fmt.Errorf("failed to initialize TLS authenticator: %w", err)
	}
	ci.node.tlsAuthenticator = tlsAuth
	log.Printf("✅ TLS authenticator initialized successfully")

	log.Printf("🔐 Certificate initialization completed - TLS authentication is MANDATORY")
	return nil
}

// initializeCertificateManager creates or loads certificate manager
func (ci *CertificateInitializer) initializeCertificateManager(rootCAPath, nodeCertPath string) (*CertificateManager, error) {
	// Case 1: Both paths provided - load existing certificates
	if rootCAPath != "" && nodeCertPath != "" {
		log.Printf("🔐 Loading existing certificates from provided paths")
		return NewCertificateManager(rootCAPath, nodeCertPath, ci.node.privateKey)
	}

	// Case 2: No paths provided - generate self-signed certificates
	log.Printf("🔐 No certificate paths provided - generating self-signed certificates")
	return ci.generateSelfSignedCertificateManager()
}

// initializeTLSAuthenticator creates TLS authenticator with proper certificates
func (ci *CertificateInitializer) initializeTLSAuthenticator(rootCAPath, nodeCertPath, nodeKeyPath string) (*TLSAuthenticator, error) {
	// Case 1: Certificate paths provided - use them
	if rootCAPath != "" && nodeCertPath != "" && nodeKeyPath != "" {
		log.Printf("🔐 Creating TLS authenticator with provided certificate paths")
		return NewTLSAuthenticator(ci.node, rootCAPath, nodeCertPath, nodeKeyPath)
	}

	// Case 2: No paths provided - create with auto-generated certificates
	log.Printf("🔐 Creating TLS authenticator with auto-generated certificates")
	return ci.createTLSAuthenticatorWithAutoGenCerts()
}

// generateSelfSignedCertificateManager creates a certificate manager with auto-generated certificates
func (ci *CertificateInitializer) generateSelfSignedCertificateManager() (*CertificateManager, error) {
	// Convert libp2p private key to standard crypto key
	privKey, err := crypto.PrivKeyToStdKey(ci.node.privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to convert libp2p private key: %w", err)
	}

	// Generate self-signed CA certificate
	caCert, err := ci.generateSelfSignedCA(privKey)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA certificate: %w", err)
	}

	// Generate node certificate signed by the CA
	nodeCert, err := ci.generateNodeCertificate(caCert, privKey, privKey)
	if err != nil {
		return nil, fmt.Errorf("failed to generate node certificate: %w", err)
	}

	// Save certificates to temp directory for persistence
	if err := ci.saveCertificatesToDisk(caCert, nodeCert, privKey); err != nil {
		log.Printf("⚠️ Warning: failed to save certificates to disk: %v", err)
	}

	// Create certificate manager with generated certificates
	certManager := &CertificateManager{
		rootCA:     caCert,
		nodeCert:   nodeCert,
		privateKey: ci.node.privateKey,
	}

	log.Printf("🔐 Generated self-signed certificates: CA=%s, Node=%s", 
		caCert.Subject.CommonName, nodeCert.Subject.CommonName)

	return certManager, nil
}

// generateSelfSignedCA creates a self-signed CA certificate
func (ci *CertificateInitializer) generateSelfSignedCA(privateKey interface{}) (*x509.Certificate, error) {
	// Create CA certificate template
	caTemplate := createCAConfig(
		fmt.Sprintf("Falak-CA-%s", ci.node.name),
		10*365*24*time.Hour, // Valid for 10 years
	)

	// Generate CA certificate
	caCertDER, err := x509.CreateCertificate(
		nil, // crypto/rand.Reader is used internally
		caTemplate,
		caTemplate, // Self-signed
		getPublicKey(privateKey),
		privateKey,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	// Parse the certificate
	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	return caCert, nil
}

// generateNodeCertificate creates a node certificate signed by the CA
func (ci *CertificateInitializer) generateNodeCertificate(caCert *x509.Certificate, caPrivateKey, nodePrivateKey interface{}) (*x509.Certificate, error) {
	return generateNodeCertificate(ci.node.id, caCert, caPrivateKey, nodePrivateKey)
}

// createTLSAuthenticatorWithAutoGenCerts creates TLS authenticator using generated certificates
func (ci *CertificateInitializer) createTLSAuthenticatorWithAutoGenCerts() (*TLSAuthenticator, error) {
	// Create TLS authenticator without file paths (will use auto-generation)
	auth := &TLSAuthenticator{
		node:             ci.node,
		trustedPeers:     make(map[peer.ID]*PeerCertInfo),
		certificateCache: make(map[string]*x509.Certificate),
	}

	// Load system root CAs as default
	rootCAs, err := x509.SystemCertPool()
	if err != nil {
		rootCAs = x509.NewCertPool()
		log.Printf("⚠️ Warning: failed to load system root CAs, using empty pool")
	}

	// Add our generated CA to the root CAs
	if ci.node.certManager != nil && ci.node.certManager.rootCA != nil {
		rootCAs.AddCert(ci.node.certManager.rootCA)
		log.Printf("🔐 Added generated CA to root CA pool")
	}

	auth.rootCAs = rootCAs

	// Generate self-signed certificate for TLS operations
	if err := auth.generateSelfSignedCertificate(); err != nil {
		return nil, fmt.Errorf("failed to generate TLS certificate: %w", err)
	}

	// Initialize certificate validator
	auth.certValidator = &CertificateValidator{
		rootCAs:             auth.rootCAs,
		intermediateCAs:     x509.NewCertPool(),
		allowSelfSigned:     true, // Allow self-signed for auto-generated certs
		maxCertChainLen:     5,
		clockSkewTolerance:  5 * time.Minute,
	}

	// Configure TLS settings
	if err := auth.configureTLS(); err != nil {
		return nil, fmt.Errorf("failed to configure TLS: %w", err)
	}

	log.Printf("🔐 TLS authenticator created with auto-generated certificates")
	return auth, nil
}

// saveCertificatesToDisk saves generated certificates for persistence
func (ci *CertificateInitializer) saveCertificatesToDisk(caCert, nodeCert *x509.Certificate, privateKey interface{}) error {
	// Create certificates directory
	certDir := filepath.Join(".", "certs", ci.node.name)
	if err := os.MkdirAll(certDir, 0700); err != nil {
		return fmt.Errorf("failed to create cert directory: %w", err)
	}

	// Save CA certificate
	caPath := filepath.Join(certDir, "ca.crt")
	if err := saveCertificateToPEM(caCert, caPath); err != nil {
		return fmt.Errorf("failed to save CA certificate: %w", err)
	}

	// Save node certificate
	nodePath := filepath.Join(certDir, "node.crt")
	if err := saveCertificateToPEM(nodeCert, nodePath); err != nil {
		return fmt.Errorf("failed to save node certificate: %w", err)
	}

	// Save private key
	keyPath := filepath.Join(certDir, "node.key")
	if err := savePrivateKeyToPEM(privateKey, keyPath); err != nil {
		return fmt.Errorf("failed to save private key: %w", err)
	}

	log.Printf("🔐 Certificates saved to %s (ca.crt, node.crt, node.key)", certDir)
	return nil
}

// validateCertificateSetup performs post-initialization validation
func (ci *CertificateInitializer) validateCertificateSetup() error {
	// Ensure certificate manager is initialized
	if ci.node.certManager == nil {
		return fmt.Errorf("certificate manager not initialized")
	}

	// Ensure TLS authenticator is initialized
	if ci.node.tlsAuthenticator == nil {
		return fmt.Errorf("TLS authenticator not initialized")
	}

	// Validate certificate manager has required certificates
	if ci.node.certManager.rootCA == nil {
		return fmt.Errorf("root CA certificate missing")
	}

	if ci.node.certManager.nodeCert == nil {
		return fmt.Errorf("node certificate missing")
	}

	// Validate certificate expiration
	now := time.Now()
	if now.After(ci.node.certManager.nodeCert.NotAfter) {
		return fmt.Errorf("node certificate has expired: %s", ci.node.certManager.nodeCert.NotAfter)
	}

	if now.Before(ci.node.certManager.nodeCert.NotBefore) {
		return fmt.Errorf("node certificate not yet valid: %s", ci.node.certManager.nodeCert.NotBefore)
	}

	log.Printf("✅ Certificate setup validation passed")
	return nil
}

// GetCertificateInfo returns human-readable certificate information
func (ci *CertificateInitializer) GetCertificateInfo() map[string]interface{} {
	info := make(map[string]interface{})

	if ci.node.certManager != nil {
		if ci.node.certManager.rootCA != nil {
			info["root_ca"] = getCertificateInfo(ci.node.certManager.rootCA)
		}
		if ci.node.certManager.nodeCert != nil {
			info["node_cert"] = getCertificateInfo(ci.node.certManager.nodeCert)
		}
	}

	if ci.node.tlsAuthenticator != nil {
		info["tls_configured"] = true
		info["trusted_peers_count"] = len(ci.node.tlsAuthenticator.GetTrustedPeers())
	} else {
		info["tls_configured"] = false
	}

	return info
}