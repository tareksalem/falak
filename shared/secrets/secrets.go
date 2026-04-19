// Package secrets provides symmetric encryption for capsule secrets and
// registry credentials. Values are encrypted before entering the mesh
// gossip and decrypted only on the executing node at container start.
//
// The Secrets Encryption Key (SEK) is derived from the cluster PSK via
// HKDF with a fixed info string. Every node in a cluster derives the
// same SEK from the same PSK, so any node can decrypt secrets for
// capsules in that cluster. The SEK is independent of the PSK in the
// sense that a future key-rotation scheme can rotate one without the
// other.
//
// Encryption uses AES-256-GCM with a random 96-bit nonce per
// encryption. The nonce is prepended to the ciphertext so Decrypt can
// extract it without additional framing.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	// SEKInfo is the HKDF info string for deriving the SEK from the PSK.
	// Changing this value changes the derived key — treat it as a
	// versioned constant. If the derivation scheme ever changes, bump
	// the suffix (e.g. "falak-secrets-v2").
	SEKInfo = "falak-secrets"

	// sekSize is the AES-256 key length in bytes.
	sekSize = 32

	// nonceSize is the AES-GCM nonce length in bytes.
	nonceSize = 12
)

// DeriveKey derives a 256-bit SEK from a PSK and cluster path using
// HKDF-SHA256. The cluster path acts as the salt so different clusters
// with the same PSK produce different keys.
//
// The returned key must be treated as secret material. Callers should
// zero it when no longer needed.
func DeriveKey(psk []byte, clusterPath string) ([]byte, error) {
	if len(psk) == 0 {
		return nil, fmt.Errorf("secrets: PSK cannot be empty")
	}
	if clusterPath == "" {
		return nil, fmt.Errorf("secrets: cluster path cannot be empty")
	}

	salt := []byte(clusterPath)
	info := []byte(SEKInfo)

	reader := hkdf.New(sha256.New, psk, salt, info)
	key := make([]byte, sekSize)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("secrets: HKDF derivation failed: %w", err)
	}
	return key, nil
}

// Encrypt encrypts plaintext with the given SEK using AES-256-GCM.
// The returned ciphertext has the random nonce prepended (first 12
// bytes) so Decrypt can extract it without additional framing.
//
// Each call generates a fresh random nonce; encrypting the same
// plaintext twice produces different ciphertexts.
func Encrypt(sek, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(sek)
	if err != nil {
		return nil, fmt.Errorf("secrets: aes cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm: %w", err)
	}

	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: nonce generation: %w", err)
	}

	// Seal appends the ciphertext+tag to the nonce prefix.
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// Decrypt decrypts ciphertext produced by Encrypt. It expects the
// nonce as the first 12 bytes of the input.
func Decrypt(sek, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("secrets: ciphertext too short")
	}

	block, err := aes.NewCipher(sek)
	if err != nil {
		return nil, fmt.Errorf("secrets: aes cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: gcm: %w", err)
	}

	nonce := ciphertext[:nonceSize]
	data := ciphertext[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, data, nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt failed (wrong key or corrupted data): %w", err)
	}
	return plaintext, nil
}

// EncryptString is a convenience wrapper that encrypts a string value.
func EncryptString(sek []byte, value string) ([]byte, error) {
	return Encrypt(sek, []byte(value))
}

// DecryptString is a convenience wrapper that decrypts to a string.
func DecryptString(sek, ciphertext []byte) (string, error) {
	plaintext, err := Decrypt(sek, ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
