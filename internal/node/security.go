package node

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
)

// CertificateManager handles certificate operations for node authentication
type CertificateManager struct {
	rootCA     *x509.Certificate
	nodeCert   *x509.Certificate
	privateKey crypto.PrivKey
}

// NewCertificateManager creates a new certificate manager with the given certificates and private key
func NewCertificateManager(rootCAPath, nodeCertPath string, privateKey crypto.PrivKey) (*CertificateManager, error) {
	rootCA, err := LoadRootCA(rootCAPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load root CA: %w", err)
	}

	nodeCert, err := LoadNodeCert(nodeCertPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load node certificate: %w", err)
	}

	// Verify the node certificate against the root CA
	if err := VerifyNodeCertificate(nodeCert, rootCA); err != nil {
		return nil, fmt.Errorf("node certificate verification failed: %w", err)
	}

	return &CertificateManager{
		rootCA:     rootCA,
		nodeCert:   nodeCert,
		privateKey: privateKey,
	}, nil
}

// LoadRootCA loads and parses a root CA certificate from a PEM file
func LoadRootCA(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA file: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block from CA file")
	}

	if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("PEM block is not a certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	return cert, nil
}

// LoadNodeCert loads and parses a node certificate from a PEM file
func LoadNodeCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read certificate file: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block from certificate file")
	}

	if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("PEM block is not a certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	return cert, nil
}

// VerifyNodeCertificate verifies a node certificate against the root CA
func VerifyNodeCertificate(cert, rootCA *x509.Certificate) error {
	roots := x509.NewCertPool()
	roots.AddCert(rootCA)

	opts := x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}

	_, err := cert.Verify(opts)
	if err != nil {
		return fmt.Errorf("certificate verification failed: %w", err)
	}

	// Check if certificate is expired
	now := time.Now()
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("certificate is not yet valid")
	}
	if now.After(cert.NotAfter) {
		return fmt.Errorf("certificate has expired")
	}

	return nil
}

// SignChallenge signs a challenge using the node's private key
func (cm *CertificateManager) SignChallenge(challenge []byte) ([]byte, error) {
	return SignChallenge(challenge, cm.privateKey)
}

// SignChallenge signs a challenge using the provided private key
func SignChallenge(challenge []byte, privKey crypto.PrivKey) ([]byte, error) {
	signature, err := privKey.Sign(challenge)
	if err != nil {
		return nil, fmt.Errorf("failed to sign challenge: %w", err)
	}
	return signature, nil
}

// VerifySignature verifies a signature against a challenge using the provided public key
func VerifySignature(challenge, signature []byte, pubKey crypto.PubKey) error {
	valid, err := pubKey.Verify(challenge, signature)
	if err != nil {
		return fmt.Errorf("failed to verify signature: %w", err)
	}
	if !valid {
		return fmt.Errorf("signature verification failed")
	}
	return nil
}

// GetNodeCertPEM returns the node certificate in PEM format
func (cm *CertificateManager) GetNodeCertPEM() ([]byte, error) {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cm.nodeCert.Raw,
	}), nil
}

// GetRootCA returns the root CA certificate
func (cm *CertificateManager) GetRootCA() *x509.Certificate {
	return cm.rootCA
}

// GetNodeCert returns the node certificate
func (cm *CertificateManager) GetNodeCert() *x509.Certificate {
	return cm.nodeCert
}

// VerifyPeerCertificate verifies a peer's certificate against the root CA
func (cm *CertificateManager) VerifyPeerCertificate(certPEM []byte) error {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("failed to decode PEM block")
	}

	if block.Type != "CERTIFICATE" {
		return fmt.Errorf("PEM block is not a certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse certificate: %w", err)
	}

	return VerifyNodeCertificate(cert, cm.rootCA)
}