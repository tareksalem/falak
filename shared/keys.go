package shared

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/crypto"
)

// DefaultKeyFileName is the default name for the node key file.
const DefaultKeyFileName = "node.key"

// LoadOrGenerateKey loads a private key from file, or generates a new one if it doesn't exist.
// If the file doesn't exist, a new Ed25519 key is generated and saved to the file.
// Returns the private key and whether it was newly generated.
func LoadOrGenerateKey(keyPath string) (crypto.PrivKey, bool, error) {
	// Try to load existing key
	if keyPath != "" {
		if key, err := LoadKey(keyPath); err == nil {
			return key, false, nil
		} else if !os.IsNotExist(err) {
			return nil, false, fmt.Errorf("failed to load key: %w", err)
		}
	}

	// Generate new key
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		return nil, false, fmt.Errorf("failed to generate key: %w", err)
	}

	// Save if path provided
	if keyPath != "" {
		if err := SaveKey(priv, keyPath); err != nil {
			return nil, false, fmt.Errorf("failed to save key: %w", err)
		}
	}

	return priv, true, nil
}

// LoadKey loads a private key from a file.
func LoadKey(keyPath string) (crypto.PrivKey, error) {
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}

	key, err := crypto.UnmarshalPrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal key: %w", err)
	}

	return key, nil
}

// SaveKey saves a private key to a file.
// Creates parent directories if they don't exist.
// Sets file permissions to 0600 (owner read/write only).
func SaveKey(key crypto.PrivKey, keyPath string) error {
	data, err := crypto.MarshalPrivateKey(key)
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	// Create parent directory if needed
	dir := filepath.Dir(keyPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Write with restrictive permissions
	if err := os.WriteFile(keyPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write key file: %w", err)
	}

	return nil
}

// GenerateKey generates a new Ed25519 private key.
func GenerateKey() (crypto.PrivKey, error) {
	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, -1)
	if err != nil {
		return nil, fmt.Errorf("failed to generate key: %w", err)
	}
	return priv, nil
}
