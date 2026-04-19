// Package clusterkeys provides cluster root key derivation and certificate management.
package clusterkeys

import (
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	// ClusterRootKeyInfo is the info string for HKDF derivation of cluster root keys.
	ClusterRootKeyInfo = "falak-cluster-root-key-v1"

	// HMACKeyInfo is the info string for HKDF derivation of the HMAC key used in
	// challenge-response authentication. This is a separate derivation from the
	// cluster root key to enable zeroing the raw PSK after derivation.
	HMACKeyInfo = "falak-cluster-hmac-key-v1"

	// HMACKeySize is the size of the derived HMAC key in bytes (256-bit).
	HMACKeySize = 32

	// Ed25519SeedSize is the size of an Ed25519 seed.
	Ed25519SeedSize = 32
)

// ClusterRootKey represents the root key pair for a cluster, derived from PSK.
type ClusterRootKey struct {
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

// DeriveClusterRootKey derives a deterministic Ed25519 key pair from a PSK and cluster path.
// All nodes with the same PSK and cluster path will derive the same root key.
func DeriveClusterRootKey(psk []byte, clusterPath string) (*ClusterRootKey, error) {
	if len(psk) == 0 {
		return nil, fmt.Errorf("PSK cannot be empty")
	}
	if clusterPath == "" {
		return nil, fmt.Errorf("cluster path cannot be empty")
	}

	// Use HKDF to derive a seed for Ed25519 key generation
	// Salt: cluster path (ensures different keys per cluster even with same PSK)
	// Info: fixed string for domain separation
	salt := []byte(clusterPath)
	info := []byte(ClusterRootKeyInfo)

	hkdfReader := hkdf.New(sha256.New, psk, salt, info)

	seed := make([]byte, Ed25519SeedSize)
	if _, err := io.ReadFull(hkdfReader, seed); err != nil {
		return nil, fmt.Errorf("failed to derive key seed: %w", err)
	}

	// Generate Ed25519 key pair from seed (deterministic)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)

	return &ClusterRootKey{
		PublicKey:  publicKey,
		PrivateKey: privateKey,
	}, nil
}

// DeriveHMACKey derives a deterministic HMAC key from a PSK and cluster path.
// This key is used for the challenge-response phase of authentication.
// By deriving a separate HMAC key, the raw PSK can be zeroed from memory
// after derivation, while still supporting the challenge-response protocol.
// All nodes with the same PSK and cluster path will derive the same HMAC key.
func DeriveHMACKey(psk []byte, clusterPath string) ([]byte, error) {
	if len(psk) == 0 {
		return nil, fmt.Errorf("PSK cannot be empty")
	}
	if clusterPath == "" {
		return nil, fmt.Errorf("cluster path cannot be empty")
	}

	salt := []byte(clusterPath)
	info := []byte(HMACKeyInfo)

	hkdfReader := hkdf.New(sha256.New, psk, salt, info)

	hmacKey := make([]byte, HMACKeySize)
	if _, err := io.ReadFull(hkdfReader, hmacKey); err != nil {
		return nil, fmt.Errorf("failed to derive HMAC key: %w", err)
	}

	return hmacKey, nil
}

// Sign signs data with the cluster root private key.
func (k *ClusterRootKey) Sign(data []byte) []byte {
	return ed25519.Sign(k.PrivateKey, data)
}

// Verify verifies a signature against the cluster root public key.
func (k *ClusterRootKey) Verify(data, signature []byte) bool {
	return ed25519.Verify(k.PublicKey, data, signature)
}

// VerifyWithPublicKey verifies a signature using a provided public key.
func VerifyWithPublicKey(publicKey ed25519.PublicKey, data, signature []byte) bool {
	return ed25519.Verify(publicKey, data, signature)
}
