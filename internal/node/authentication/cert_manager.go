package authentication

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// NodeCertificate represents a certificate and private key pair for a specific cluster/datacenter
type NodeCertificate struct {
	Certificate *x509.Certificate
	PrivateKey  ed25519.PrivateKey
	PublicKey   ed25519.PublicKey
	ClusterID   string
	DataCenter  string
}

// CertificateManager handles certificate loading, validation, and management
type CertificateManager struct {
	config *Config

	// Node's certificates for different clusters/datacenters
	nodeCertificates map[string]*NodeCertificate // "cluster:datacenter" -> NodeCertificate

	// Multiple root CAs for different clusters/datacenters
	rootCAs map[string]*x509.Certificate // "cluster" or "datacenter" -> root CA

	// Certificate chains for different contexts
	certChains map[string][]*x509.Certificate // "cluster:datacenter" -> certificate chain

	// Cache for peer certificates
	peerCertCache sync.Map // peer.ID -> *x509.Certificate

	// Mutex for thread safety
	mutex sync.RWMutex
}

// NewCertificateManager creates a new certificate manager
func NewCertificateManager(config *Config) *CertificateManager {
	return &CertificateManager{
		config:           config,
		nodeCertificates: make(map[string]*NodeCertificate),
		rootCAs:          make(map[string]*x509.Certificate),
		certChains:       make(map[string][]*x509.Certificate),
		peerCertCache:    sync.Map{},
	}
}

// Initialize loads and validates all certificates
func (cm *CertificateManager) Initialize() error {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	log.Printf("🔐 Initializing Certificate Manager...")

	// Load root CAs for all configured clusters and datacenters
	if err := cm.loadRootCAs(); err != nil {
		return fmt.Errorf("failed to load root CAs: %w", err)
	}

	// Load node certificates for all cluster/datacenter combinations
	if err := cm.loadNodeCertificates(); err != nil {
		return fmt.Errorf("failed to load node certificates: %w", err)
	}

	// Load certificate chains if specified
	if err := cm.loadCertificateChains(); err != nil {
		return fmt.Errorf("failed to load certificate chains: %w", err)
	}

	// Validate our own certificates
	if err := cm.validateNodeCertificates(); err != nil {
		return fmt.Errorf("node certificate validation failed: %w", err)
	}

	log.Printf("✅ Certificate Manager initialized successfully")
	log.Printf("   - Loaded %d node certificates", len(cm.nodeCertificates))
	log.Printf("   - Loaded %d root CAs", len(cm.rootCAs))

	for key, nodeCert := range cm.nodeCertificates {
		log.Printf("   - %s: %s (expires: %s)", key,
			nodeCert.Certificate.Subject.CommonName,
			nodeCert.Certificate.NotAfter.Format(time.RFC3339))
	}

	return nil
}

// loadRootCAs loads root CA certificates for all clusters and datacenters
func (cm *CertificateManager) loadRootCAs() error {
	log.Printf("📜 Loading root CAs for clusters and datacenters...")

	// If RootCAPath is specified directly, use it for primary cluster
	if cm.config.RootCAPath != "" {
		return cm.loadSingleRootCA(cm.config.RootCAPath, cm.config.GetPrimaryCluster())
	}

	// Otherwise, try to discover CA files based on cluster/datacenter configuration
	basePath := cm.config.CABasePath
	if basePath == "" {
		basePath = "." // Fallback to current directory
	}

	// Load CA for each cluster
	for _, cluster := range cm.config.Clusters {
		caPath := filepath.Join(basePath, fmt.Sprintf("%s-ca", cluster), "root-ca.crt")
		if err := cm.loadSingleRootCA(caPath, cluster); err != nil {
			log.Printf("⚠️ Failed to load CA for cluster %s: %v", cluster, err)
			// Continue with other clusters, don't fail completely
		}
	}

	// Load CA for each datacenter
	for _, datacenter := range cm.config.DataCenters {
		caPath := filepath.Join(basePath, fmt.Sprintf("%s-ca", datacenter), "root-ca.crt")
		if err := cm.loadSingleRootCA(caPath, datacenter); err != nil {
			log.Printf("⚠️ Failed to load CA for datacenter %s: %v", datacenter, err)
			// Continue with other datacenters, don't fail completely
		}
	}

	if len(cm.rootCAs) == 0 {
		return fmt.Errorf("no root CA certificates loaded")
	}

	log.Printf("✅ Loaded %d root CAs", len(cm.rootCAs))
	return nil
}

// loadSingleRootCA loads a single root CA certificate
func (cm *CertificateManager) loadSingleRootCA(caPath, identifier string) error {
	log.Printf("📜 Loading root CA from: %s (identifier: %s)", caPath, identifier)

	certData, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("failed to read root CA file %s: %w", caPath, err)
	}

	// Parse PEM block
	block, _ := pem.Decode(certData)
	if block == nil {
		return fmt.Errorf("failed to decode PEM block from root CA %s", caPath)
	}

	// Parse X.509 certificate
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse root CA certificate %s: %w", caPath, err)
	}

	// Validate it's a CA certificate
	if !cert.IsCA {
		return fmt.Errorf("certificate %s is not a CA certificate", caPath)
	}

	// Check expiration
	if time.Now().After(cert.NotAfter) {
		return fmt.Errorf("root CA certificate %s has expired: %s", caPath, cert.NotAfter)
	}

	cm.rootCAs[identifier] = cert
	log.Printf("✅ Root CA loaded for %s: %s", identifier, cert.Subject.CommonName)

	return nil
}

// loadNodeCertificates loads node certificates for all cluster/datacenter combinations
func (cm *CertificateManager) loadNodeCertificates() error {
	log.Printf("📜 Loading node certificates for all cluster/datacenter combinations...")

	basePath := cm.config.CABasePath
	if basePath == "" {
		basePath = "." // Fallback to current directory
	}

	// If specific paths are provided, load just that certificate for primary cluster
	if cm.config.CertificatePath != "" && cm.config.PrivateKeyPath != "" {
		primaryCluster := cm.config.GetPrimaryCluster()
		primaryDataCenter := cm.config.GetPrimaryDataCenter()
		return cm.loadSingleNodeCertificate(cm.config.CertificatePath, cm.config.PrivateKeyPath, primaryCluster, primaryDataCenter)
	}

	// Otherwise, try to discover certificates for all cluster/datacenter combinations
	for _, cluster := range cm.config.Clusters {
		for _, datacenter := range cm.config.DataCenters {
			// Try cluster-datacenter specific certificate first
			certPath := filepath.Join(basePath, fmt.Sprintf("%s-%s-ca", cluster, datacenter), "node.crt")
			keyPath := filepath.Join(basePath, fmt.Sprintf("%s-%s-ca", cluster, datacenter), "node.key")

			if _, err := os.Stat(certPath); err == nil {
				if err := cm.loadSingleNodeCertificate(certPath, keyPath, cluster, datacenter); err != nil {
					log.Printf("⚠️ Failed to load certificate for %s:%s: %v", cluster, datacenter, err)
				}
				continue
			}

			// Fallback to cluster-only certificate
			certPath = filepath.Join(basePath, fmt.Sprintf("%s-ca", cluster), "node.crt")
			keyPath = filepath.Join(basePath, fmt.Sprintf("%s-ca", cluster), "node.key")

			if _, err := os.Stat(certPath); err == nil {
				if err := cm.loadSingleNodeCertificate(certPath, keyPath, cluster, datacenter); err != nil {
					log.Printf("⚠️ Failed to load certificate for %s:%s: %v", cluster, datacenter, err)
				}
				continue
			}

			// Final fallback to datacenter-only certificate
			certPath = filepath.Join(basePath, fmt.Sprintf("%s-ca", datacenter), "node.crt")
			keyPath = filepath.Join(basePath, fmt.Sprintf("%s-ca", datacenter), "node.key")

			if _, err := os.Stat(certPath); err == nil {
				if err := cm.loadSingleNodeCertificate(certPath, keyPath, cluster, datacenter); err != nil {
					log.Printf("⚠️ Failed to load certificate for %s:%s: %v", cluster, datacenter, err)
				}
			}
		}
	}

	if len(cm.nodeCertificates) == 0 {
		return fmt.Errorf("no node certificates loaded")
	}

	log.Printf("✅ Loaded %d node certificates", len(cm.nodeCertificates))
	return nil
}

// loadSingleNodeCertificate loads a single node certificate and key for a specific cluster/datacenter
func (cm *CertificateManager) loadSingleNodeCertificate(certPath, keyPath, clusterID, dataCenterID string) error {
	log.Printf("📜 Loading node certificate from: %s (cluster: %s, datacenter: %s)", certPath, clusterID, dataCenterID)

	// Load certificate
	certData, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("failed to read certificate file: %w", err)
	}

	// Parse certificate PEM
	certBlock, _ := pem.Decode(certData)
	if certBlock == nil {
		return fmt.Errorf("failed to decode certificate PEM block")
	}

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Load private key
	log.Printf("🔑 Loading private key from: %s", keyPath)

	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("failed to read private key file: %w", err)
	}

	// Parse private key PEM
	keyBlock, _ := pem.Decode(keyData)
	if keyBlock == nil {
		return fmt.Errorf("failed to decode private key PEM block")
	}

	// Parse Ed25519 private key
	privateKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse private key: %w", err)
	}

	// Ensure it's Ed25519
	ed25519Key, ok := privateKey.(ed25519.PrivateKey)
	if !ok {
		return fmt.Errorf("private key is not Ed25519")
	}

	// Create node certificate struct
	nodeCert := &NodeCertificate{
		Certificate: cert,
		PrivateKey:  ed25519Key,
		PublicKey:   ed25519Key.Public().(ed25519.PublicKey),
		ClusterID:   clusterID,
		DataCenter:  dataCenterID,
	}

	// Store with cluster:datacenter key
	key := fmt.Sprintf("%s:%s", clusterID, dataCenterID)
	cm.nodeCertificates[key] = nodeCert

	log.Printf("✅ Node certificate loaded for %s", key)
	return nil
}

// loadCertificateChains loads certificate chains for different contexts
func (cm *CertificateManager) loadCertificateChains() error {
	if len(cm.config.CertificateChain) == 0 {
		log.Printf("📜 No certificate chains specified")
		return nil
	}

	log.Printf("📜 Loading certificate chains (%d certificates)", len(cm.config.CertificateChain))

	// For now, load the chain for the primary cluster:datacenter
	primaryKey := fmt.Sprintf("%s:%s", cm.config.GetPrimaryCluster(), cm.config.GetPrimaryDataCenter())

	certChain := make([]*x509.Certificate, len(cm.config.CertificateChain))

	for i, certPath := range cm.config.CertificateChain {
		certData, err := os.ReadFile(certPath)
		if err != nil {
			return fmt.Errorf("failed to read certificate chain[%d]: %w", i, err)
		}

		block, _ := pem.Decode(certData)
		if block == nil {
			return fmt.Errorf("failed to decode certificate chain[%d] PEM", i)
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("failed to parse certificate chain[%d]: %w", i, err)
		}

		certChain[i] = cert
	}

	cm.certChains[primaryKey] = certChain
	log.Printf("✅ Certificate chain loaded for %s", primaryKey)
	return nil
}

// validateNodeCertificates validates all node certificates against available root CAs
func (cm *CertificateManager) validateNodeCertificates() error {
	log.Printf("🔍 Validating node certificates...")

	for key, nodeCert := range cm.nodeCertificates {
		if err := cm.validateSingleNodeCertificate(key, nodeCert); err != nil {
			return fmt.Errorf("validation failed for certificate %s: %w", key, err)
		}
	}

	log.Printf("✅ All node certificates validated successfully")
	return nil
}

// validateSingleNodeCertificate validates a single node certificate
func (cm *CertificateManager) validateSingleNodeCertificate(key string, nodeCert *NodeCertificate) error {
	cert := nodeCert.Certificate

	// Check certificate expiration
	now := time.Now()
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("certificate not yet valid (valid from: %s)", cert.NotBefore)
	}
	if now.After(cert.NotAfter) {
		return fmt.Errorf("certificate has expired: %s", cert.NotAfter)
	}

	// Try to verify against appropriate root CAs
	var lastErr error
	verified := false

	// Try cluster-specific root CA first
	if rootCA, exists := cm.rootCAs[nodeCert.ClusterID]; exists {
		if err := cm.verifyAgainstRootCA(cert, rootCA, key); err == nil {
			verified = true
		} else {
			lastErr = err
		}
	}

	// Try datacenter-specific root CA
	if !verified {
		if rootCA, exists := cm.rootCAs[nodeCert.DataCenter]; exists {
			if err := cm.verifyAgainstRootCA(cert, rootCA, key); err == nil {
				verified = true
			} else {
				lastErr = err
			}
		}
	}

	// Try all available root CAs as fallback
	if !verified {
		for identifier, rootCA := range cm.rootCAs {
			if err := cm.verifyAgainstRootCA(cert, rootCA, key); err == nil {
				log.Printf("✅ Certificate %s verified against %s CA", key, identifier)
				verified = true
				break
			} else {
				lastErr = err
			}
		}
	}

	if !verified {
		return fmt.Errorf("certificate verification failed against all root CAs: %w", lastErr)
	}

	// Verify that the certificate public key matches the private key
	certPublicKey, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("certificate public key is not Ed25519")
	}

	if !certPublicKey.Equal(nodeCert.PublicKey) {
		return fmt.Errorf("certificate public key does not match private key")
	}

	return nil
}

// verifyAgainstRootCA verifies a certificate against a specific root CA
func (cm *CertificateManager) verifyAgainstRootCA(cert, rootCA *x509.Certificate, context string) error {
	roots := x509.NewCertPool()
	roots.AddCert(rootCA)

	opts := x509.VerifyOptions{
		Roots: roots,
	}

	// Add intermediate certificates if we have a chain for this context
	if certChain, exists := cm.certChains[context]; exists {
		intermediates := x509.NewCertPool()
		for _, intermediateCert := range certChain {
			intermediates.AddCert(intermediateCert)
		}
		opts.Intermediates = intermediates
	}

	_, err := cert.Verify(opts)
	return err
}

// GetNodeCertificate returns the node's certificate for a specific cluster/datacenter context
func (cm *CertificateManager) GetNodeCertificate(clusterID, dataCenterID string) []byte {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	key := fmt.Sprintf("%s:%s", clusterID, dataCenterID)
	if nodeCert, exists := cm.nodeCertificates[key]; exists {
		return nodeCert.Certificate.Raw
	}

	// Fallback to primary cluster/datacenter
	primaryKey := fmt.Sprintf("%s:%s", cm.config.GetPrimaryCluster(), cm.config.GetPrimaryDataCenter())
	if nodeCert, exists := cm.nodeCertificates[primaryKey]; exists {
		return nodeCert.Certificate.Raw
	}

	return nil
}

// GetNodePrivateKey returns the node's private key for a specific cluster/datacenter context
func (cm *CertificateManager) GetNodePrivateKey(clusterID, dataCenterID string) ed25519.PrivateKey {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	key := fmt.Sprintf("%s:%s", clusterID, dataCenterID)
	if nodeCert, exists := cm.nodeCertificates[key]; exists {
		return nodeCert.PrivateKey
	}

	// Fallback to primary cluster/datacenter
	primaryKey := fmt.Sprintf("%s:%s", cm.config.GetPrimaryCluster(), cm.config.GetPrimaryDataCenter())
	if nodeCert, exists := cm.nodeCertificates[primaryKey]; exists {
		return nodeCert.PrivateKey
	}

	return nil
}

// GetNodePublicKey returns the node's public key for a specific cluster/datacenter context
func (cm *CertificateManager) GetNodePublicKey(clusterID, dataCenterID string) ed25519.PublicKey {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	key := fmt.Sprintf("%s:%s", clusterID, dataCenterID)
	if nodeCert, exists := cm.nodeCertificates[key]; exists {
		return nodeCert.PublicKey
	}

	// Fallback to primary cluster/datacenter
	primaryKey := fmt.Sprintf("%s:%s", cm.config.GetPrimaryCluster(), cm.config.GetPrimaryDataCenter())
	if nodeCert, exists := cm.nodeCertificates[primaryKey]; exists {
		return nodeCert.PublicKey
	}

	return nil
}

// ValidatePeerCertificate validates a peer's certificate against appropriate root CAs
func (cm *CertificateManager) ValidatePeerCertificate(peerID peer.ID, certData []byte, clusterID, sessionID string) (*x509.Certificate, error) {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	log.Printf("🔍 Validating certificate for peer: %s (cluster: %s)", peerID.ShortString(), clusterID)

	// Parse the certificate
	cert, err := x509.ParseCertificate(certData)
	if err != nil {
		return nil, ErrCertificateInvalid(peerID.String(), sessionID,
			fmt.Errorf("failed to parse certificate: %w", err))
	}

	// Check expiration
	now := time.Now()
	if now.Before(cert.NotBefore) {
		return nil, ErrCertificateInvalid(peerID.String(), sessionID,
			fmt.Errorf("certificate not yet valid (valid from: %s)", cert.NotBefore))
	}
	if now.After(cert.NotAfter) {
		return nil, ErrCertificateExpired(peerID.String(), sessionID, cert.NotAfter)
	}

	// Verify against appropriate root CA(s)
	if cm.config.CertValidationMode == "strict" {
		if err := cm.validatePeerCertificateChain(cert, clusterID); err != nil {
			return nil, ErrCertificateChainInvalid(peerID.String(), sessionID, err)
		}
	}

	// Cache the validated certificate
	cm.peerCertCache.Store(peerID, cert)

	log.Printf("✅ Certificate validation successful for peer: %s", peerID.ShortString())
	return cert, nil
}

// validatePeerCertificateChain verifies the certificate against appropriate root CAs
func (cm *CertificateManager) validatePeerCertificateChain(cert *x509.Certificate, clusterID string) error {
	// Try cluster-specific root CA first
	if rootCA, exists := cm.rootCAs[clusterID]; exists {
		if err := cm.verifyAgainstRootCA(cert, rootCA, clusterID); err == nil {
			return nil // Verification successful
		}
	}

	// Try all available root CAs
	var lastErr error
	for identifier, rootCA := range cm.rootCAs {
		if err := cm.verifyAgainstRootCA(cert, rootCA, identifier); err != nil {
			lastErr = err
			continue
		}
		log.Printf("✅ Peer certificate verified against %s CA", identifier)
		return nil
	}

	return fmt.Errorf("certificate verification failed against all root CAs: %w", lastErr)
}

// GetPeerPublicKey extracts the public key from a peer's certificate
func (cm *CertificateManager) GetPeerPublicKey(peerID peer.ID) (ed25519.PublicKey, error) {
	// Try to get from cache first
	if cert, exists := cm.peerCertCache.Load(peerID); exists {
		x509Cert := cert.(*x509.Certificate)
		if publicKey, ok := x509Cert.PublicKey.(ed25519.PublicKey); ok {
			return publicKey, nil
		}
		return nil, fmt.Errorf("peer certificate does not contain Ed25519 public key")
	}

	return nil, fmt.Errorf("peer certificate not found in cache")
}

// GetAvailableRootCAs returns the list of available root CA identifiers
func (cm *CertificateManager) GetAvailableRootCAs() []string {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	var identifiers []string
	for identifier := range cm.rootCAs {
		identifiers = append(identifiers, identifier)
	}
	return identifiers
}

// GetAvailableNodeCertificates returns the list of available node certificate contexts
func (cm *CertificateManager) GetAvailableNodeCertificates() []string {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	var contexts []string
	for context := range cm.nodeCertificates {
		contexts = append(contexts, context)
	}
	return contexts
}

// GetNodeCertificateForCluster returns the best matching node certificate for a cluster
func (cm *CertificateManager) GetNodeCertificateForCluster(clusterID string) *NodeCertificate {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	// Try to find exact cluster match with any datacenter
	for key, nodeCert := range cm.nodeCertificates {
		if strings.HasPrefix(key, clusterID+":") {
			return nodeCert
		}
	}

	// Fallback to primary
	primaryKey := fmt.Sprintf("%s:%s", cm.config.GetPrimaryCluster(), cm.config.GetPrimaryDataCenter())
	if nodeCert, exists := cm.nodeCertificates[primaryKey]; exists {
		return nodeCert
	}

	return nil
}

// ClearPeerCache removes a peer's certificate from the cache
func (cm *CertificateManager) ClearPeerCache(peerID peer.ID) {
	cm.peerCertCache.Delete(peerID)
}

// IsInitialized returns true if the certificate manager is initialized
func (cm *CertificateManager) IsInitialized() bool {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	return len(cm.nodeCertificates) > 0 && len(cm.rootCAs) > 0
}

// GetCertificateFingerprint returns a SHA-256 fingerprint of a node certificate
func (cm *CertificateManager) GetCertificateFingerprint(clusterID, dataCenterID string) []byte {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	key := fmt.Sprintf("%s:%s", clusterID, dataCenterID)
	if nodeCert, exists := cm.nodeCertificates[key]; exists {
		// Return the first 16 bytes of the certificate's raw data as fingerprint
		if len(nodeCert.Certificate.Raw) >= 16 {
			return nodeCert.Certificate.Raw[:16]
		}
		return nodeCert.Certificate.Raw
	}

	return nil
}