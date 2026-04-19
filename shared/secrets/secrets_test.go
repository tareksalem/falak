package secrets

import (
	"bytes"
	"testing"
)

func TestDeriveKey_Deterministic(t *testing.T) {
	psk := []byte("my-super-secret-psk-1234567890ab")
	k1, err := DeriveKey(psk, "test/dc1/prod")
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	k2, err := DeriveKey(psk, "test/dc1/prod")
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("same PSK + cluster should produce the same key")
	}
}

func TestDeriveKey_DifferentClusters(t *testing.T) {
	psk := []byte("my-super-secret-psk-1234567890ab")
	k1, _ := DeriveKey(psk, "test/dc1/prod")
	k2, _ := DeriveKey(psk, "test/dc2/staging")
	if bytes.Equal(k1, k2) {
		t.Error("different clusters should produce different keys")
	}
}

func TestDeriveKey_EmptyPSK(t *testing.T) {
	_, err := DeriveKey(nil, "test/dc1")
	if err == nil {
		t.Error("empty PSK should error")
	}
}

func TestDeriveKey_EmptyCluster(t *testing.T) {
	_, err := DeriveKey([]byte("psk"), "")
	if err == nil {
		t.Error("empty cluster should error")
	}
}

func TestEncryptDecrypt_Roundtrip(t *testing.T) {
	sek, _ := DeriveKey([]byte("test-psk-32-bytes-long-enough!!!"), "cluster")
	plain := []byte("DB_PASSWORD=hunter2")

	ct, err := Encrypt(sek, plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ct, plain) {
		t.Error("ciphertext should not equal plaintext")
	}

	got, err := Decrypt(sek, ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("roundtrip failed: got %q, want %q", got, plain)
	}
}

func TestEncrypt_DifferentNonce(t *testing.T) {
	sek, _ := DeriveKey([]byte("test-psk-32-bytes-long-enough!!!"), "cluster")
	plain := []byte("same-value")

	ct1, _ := Encrypt(sek, plain)
	ct2, _ := Encrypt(sek, plain)

	if bytes.Equal(ct1, ct2) {
		t.Error("encrypting the same plaintext twice should produce different ciphertexts (random nonce)")
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	sek1, _ := DeriveKey([]byte("test-psk-32-bytes-long-enough!!!"), "cluster1")
	sek2, _ := DeriveKey([]byte("test-psk-32-bytes-long-enough!!!"), "cluster2")

	ct, _ := Encrypt(sek1, []byte("secret"))

	_, err := Decrypt(sek2, ct)
	if err == nil {
		t.Error("decrypting with wrong key should fail")
	}
}

func TestDecrypt_TooShort(t *testing.T) {
	sek, _ := DeriveKey([]byte("test-psk-32-bytes-long-enough!!!"), "cluster")
	_, err := Decrypt(sek, []byte("short"))
	if err == nil {
		t.Error("ciphertext shorter than nonce should fail")
	}
}

func TestDecrypt_Corrupted(t *testing.T) {
	sek, _ := DeriveKey([]byte("test-psk-32-bytes-long-enough!!!"), "cluster")
	ct, _ := Encrypt(sek, []byte("secret"))

	// Flip a byte in the ciphertext (after the nonce).
	ct[len(ct)-1] ^= 0xff

	_, err := Decrypt(sek, ct)
	if err == nil {
		t.Error("corrupted ciphertext should fail")
	}
}

func TestEncryptDecryptString(t *testing.T) {
	sek, _ := DeriveKey([]byte("test-psk-32-bytes-long-enough!!!"), "cluster")

	ct, err := EncryptString(sek, "hello world")
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}

	got, err := DecryptString(sek, ct)
	if err != nil {
		t.Fatalf("DecryptString: %v", err)
	}
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestDeriveKey_Length(t *testing.T) {
	sek, _ := DeriveKey([]byte("test-psk-32-bytes-long-enough!!!"), "cluster")
	if len(sek) != 32 {
		t.Errorf("SEK should be 32 bytes, got %d", len(sek))
	}
}
