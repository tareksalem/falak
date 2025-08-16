package node

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"time"
)

// generateSelfSignedCert generates a self-signed certificate for a node
func generateSelfSignedCert(privateKey crypto.PrivateKey, nodeID string) (*tls.Certificate, error) {
	// Certificate template
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   nodeID,
			Organization: []string{"Falak Node"},
			Country:      []string{"US"},
			Province:     []string{""},
			Locality:     []string{""},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour), // Valid for 1 year
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	
	// Add Subject Alternative Names
	template.DNSNames = []string{
		"localhost",
		"falak-node",
		nodeID,
	}
	
	template.IPAddresses = []net.IP{
		net.IPv4(127, 0, 0, 1),
		net.IPv6loopback,
	}
	
	// Add custom URI for libp2p peer ID
	peerURI, _ := url.Parse(fmt.Sprintf("p2p://%s", nodeID))
	template.URIs = []*url.URL{peerURI}
	
	// Generate certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, getPublicKey(privateKey), privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create certificate: %w", err)
	}
	
	// Create TLS certificate
	cert := tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  privateKey,
	}
	
	return &cert, nil
}

// getPublicKey extracts the public key from a private key
func getPublicKey(privateKey crypto.PrivateKey) crypto.PublicKey {
	switch k := privateKey.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case crypto.Signer:
		return k.Public()
	default:
		return nil
	}
}

// saveCertificateToPEM saves a certificate to PEM format
func saveCertificateToPEM(cert *x509.Certificate, filename string) error {
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	})
	
	return writeFile(filename, certPEM)
}

// savePrivateKeyToPEM saves a private key to PEM format
func savePrivateKeyToPEM(privateKey crypto.PrivateKey, filename string) error {
	var keyBytes []byte
	var keyType string
	
	switch k := privateKey.(type) {
	case *rsa.PrivateKey:
		keyBytes = x509.MarshalPKCS1PrivateKey(k)
		keyType = "RSA PRIVATE KEY"
	default:
		pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
		if err != nil {
			return fmt.Errorf("failed to marshal private key: %w", err)
		}
		keyBytes = pkcs8Bytes
		keyType = "PRIVATE KEY"
	}
	
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  keyType,
		Bytes: keyBytes,
	})
	
	return writeFile(filename, keyPEM)
}

// loadCertificateFromPEM loads a certificate from PEM format
func loadCertificateFromPEM(filename string) (*x509.Certificate, error) {
	certPEM, err := readFile(filename)
	if err != nil {
		return nil, err
	}
	
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}
	
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}
	
	return cert, nil
}

// loadPrivateKeyFromPEM loads a private key from PEM format
func loadPrivateKeyFromPEM(filename string) (crypto.PrivateKey, error) {
	keyPEM, err := readFile(filename)
	if err != nil {
		return nil, err
	}
	
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}
	
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		return x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported private key type: %s", block.Type)
	}
}

// validateCertificateChain validates a certificate chain
func validateCertificateChain(certs []*x509.Certificate, rootCAs *x509.CertPool) error {
	if len(certs) == 0 {
		return fmt.Errorf("empty certificate chain")
	}
	
	// Build intermediate pool
	intermediates := x509.NewCertPool()
	for i := 1; i < len(certs); i++ {
		intermediates.AddCert(certs[i])
	}
	
	// Verify chain
	opts := x509.VerifyOptions{
		Roots:         rootCAs,
		Intermediates: intermediates,
	}
	
	_, err := certs[0].Verify(opts)
	return err
}

// createCAConfig creates a certificate authority configuration
func createCAConfig(commonName string, validityDuration time.Duration) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"Falak CA"},
			Country:      []string{"US"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(validityDuration),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            2,
	}
}

// generateNodeCertificate generates a node certificate signed by a CA
func generateNodeCertificate(nodeID string, caCert *x509.Certificate, caPrivateKey crypto.PrivateKey, nodePrivateKey crypto.PrivateKey) (*x509.Certificate, error) {
	template := &x509.Certificate{
		SerialNumber: generateSerialNumber(),
		Subject: pkix.Name{
			CommonName:   nodeID,
			Organization: []string{"Falak Node"},
		},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost", "falak-node", nodeID},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	
	// Add custom URI for libp2p peer ID
	peerURI, _ := url.Parse(fmt.Sprintf("p2p://%s", nodeID))
	template.URIs = []*url.URL{peerURI}
	
	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, getPublicKey(nodePrivateKey), caPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create certificate: %w", err)
	}
	
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse created certificate: %w", err)
	}
	
	return cert, nil
}

// generateSerialNumber generates a random serial number for certificates
func generateSerialNumber() *big.Int {
	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return serialNumber
}

// verifyCertificateUsage verifies that a certificate has the required usage flags
func verifyCertificateUsage(cert *x509.Certificate, requiredKeyUsage x509.KeyUsage, requiredExtKeyUsage []x509.ExtKeyUsage) error {
	// Check key usage
	if cert.KeyUsage&requiredKeyUsage == 0 {
		return fmt.Errorf("certificate missing required key usage flags")
	}
	
	// Check extended key usage
	if len(requiredExtKeyUsage) > 0 {
		extKeyUsageMap := make(map[x509.ExtKeyUsage]bool)
		for _, usage := range cert.ExtKeyUsage {
			extKeyUsageMap[usage] = true
		}
		
		for _, required := range requiredExtKeyUsage {
			if !extKeyUsageMap[required] {
				return fmt.Errorf("certificate missing required extended key usage: %v", required)
			}
		}
	}
	
	return nil
}

// getCertificateInfo extracts human-readable information from a certificate
func getCertificateInfo(cert *x509.Certificate) map[string]interface{} {
	return map[string]interface{}{
		"subject":            cert.Subject.String(),
		"issuer":             cert.Issuer.String(),
		"serial_number":      cert.SerialNumber.String(),
		"not_before":         cert.NotBefore,
		"not_after":          cert.NotAfter,
		"dns_names":          cert.DNSNames,
		"ip_addresses":       cert.IPAddresses,
		"email_addresses":    cert.EmailAddresses,
		"uris":               cert.URIs,
		"key_usage":          cert.KeyUsage,
		"ext_key_usage":      cert.ExtKeyUsage,
		"is_ca":              cert.IsCA,
		"signature_algorithm": cert.SignatureAlgorithm.String(),
		"public_key_algorithm": cert.PublicKeyAlgorithm.String(),
	}
}

// File I/O functions
func writeFile(filename string, data []byte) error {
	// Write file with restrictive permissions for security
	return os.WriteFile(filename, data, 0600)
}

func readFile(filename string) ([]byte, error) {
	return os.ReadFile(filename)
}