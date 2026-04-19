package node

import (
	"github.com/libp2p/go-libp2p/core/crypto"

	"github.com/tareksalem/falak/node/phonebook"
)

// capsuleSigner implements orbit.Signer using the local node's private key.
type capsuleSigner struct {
	privateKey crypto.PrivKey
}

// newCapsuleSigner creates an orbit.Signer backed by the given private key.
func newCapsuleSigner(key crypto.PrivKey) *capsuleSigner {
	return &capsuleSigner{privateKey: key}
}

// Sign signs the given canonical content.
func (s *capsuleSigner) Sign(content []byte) ([]byte, error) {
	return s.privateKey.Sign(content)
}

// capsuleVerifier implements orbit.Verifier using the phonebook to look up sender public keys.
type capsuleVerifier struct {
	phonebook   phonebook.IPhonebook
	clusterPath string
}

// newCapsuleVerifier creates an orbit.Verifier for the given cluster.
func newCapsuleVerifier(pb phonebook.IPhonebook, clusterPath string) *capsuleVerifier {
	return &capsuleVerifier{
		phonebook:   pb,
		clusterPath: clusterPath,
	}
}

// Verify checks that the signature over content matches the sender's public key.
func (v *capsuleVerifier) Verify(senderID string, content, signature []byte) bool {
	if len(signature) == 0 {
		return false
	}

	entry, err := v.phonebook.Get(senderID, v.clusterPath)
	if err != nil || entry == nil {
		return false
	}

	pubKey, err := crypto.UnmarshalPublicKey(entry.PublicKey)
	if err != nil {
		return false
	}

	ok, err := pubKey.Verify(content, signature)
	return err == nil && ok
}
