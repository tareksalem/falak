package overlay

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"sort"

	"go.uber.org/zap"
)

// pairKeyLen is the AES-256-GCM key length consumed by the IPsec
// transform (rfc4106(gcm(aes)) with a 32-byte key + 4-byte salt).
const pairKeyLen = 32

// pairKeyInfoPrefix matches plan 11A.6 verbatim. Changing it is a wire
// break: both ends derive their per-pair key from the same prefix.
const pairKeyInfoPrefix = "falak-overlay-mac:"

// ErrKeyManagerNotConfigured is returned by DerivePairKey when the
// KeyManager was constructed without a cluster root key. Production
// callers MUST supply WithClusterRootKey; the sentinel only fires when
// a test forgets to wire the option.
var ErrKeyManagerNotConfigured = errors.New("overlay: key manager not configured (cluster root key required)")

// KeyManager derives deterministic per-pair AES-GCM keys for the
// VXLAN/IPsec overlay. Keys are derived independently by both sides
// of a pair using HKDF-SHA256 over the cluster root key, so no key
// exchange protocol is required.
//
// One instance per node. Stateless after construction; safe for
// concurrent use.
type KeyManager struct {
	rootKey []byte
	logger  *zap.Logger
}

// KeyManagerOption configures a KeyManager.
type KeyManagerOption func(*KeyManager)

// WithClusterRootKey supplies the cluster root key that backs HKDF
// derivation. Required. Empty values are ignored (validation runs in
// NewKeyManager).
func WithClusterRootKey(key []byte) KeyManagerOption {
	return func(k *KeyManager) {
		if len(key) > 0 {
			k.rootKey = append([]byte(nil), key...)
		}
	}
}

// WithKeyManagerLogger replaces the default no-op logger. Nil ignored.
func WithKeyManagerLogger(l *zap.Logger) KeyManagerOption {
	return func(k *KeyManager) {
		if l != nil {
			k.logger = l
		}
	}
}

// NewKeyManager constructs a KeyManager. Returns an error if the
// cluster root key was not supplied — failing fast at construction is
// cheaper than failing per-pair at derivation time.
func NewKeyManager(opts ...KeyManagerOption) (*KeyManager, error) {
	k := &KeyManager{logger: zap.NewNop()}
	for _, opt := range opts {
		opt(k)
	}
	if len(k.rootKey) == 0 {
		return nil, ErrKeyManagerNotConfigured
	}
	return k, nil
}

// DerivePairKey returns the 32-byte AES-GCM key for the given group
// and unordered node pair. The derivation is order-independent: the
// two node IDs are sorted lexicographically before being mixed into
// the HKDF info string, so node A and node B agree without
// coordination.
//
// info = "falak-overlay-mac:" + sortedNodeIDs + ":" + groupID
//
// Returns ErrKeyManagerNotConfigured if construction was bypassed
// (defensive — NewKeyManager already rejects that case).
func (k *KeyManager) DerivePairKey(groupID, nodeA, nodeB string) ([]byte, error) {
	if len(k.rootKey) == 0 {
		return nil, ErrKeyManagerNotConfigured
	}
	if groupID == "" {
		return nil, errors.New("overlay: empty groupID")
	}
	if nodeA == "" || nodeB == "" {
		return nil, errors.New("overlay: empty nodeID")
	}
	pair := []string{nodeA, nodeB}
	sort.Strings(pair)
	info := pairKeyInfoPrefix + pair[0] + ":" + pair[1] + ":" + groupID
	// Salt is nil — the cluster root key already incorporates per-cluster
	// entropy via the cluster bootstrap (see node/auth/clusterkeys/).
	key, err := hkdf.Key(sha256.New, k.rootKey, nil, info, pairKeyLen)
	if err != nil {
		return nil, err
	}
	k.logger.Debug("derived overlay pair key",
		zap.String("group", groupID),
		zap.String("peer_lo", pair[0]),
		zap.String("peer_hi", pair[1]))
	return key, nil
}
