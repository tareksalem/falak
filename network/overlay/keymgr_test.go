package overlay

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

const testRootKey = "cluster-root-key-fixture-32-bytes!"

func TestKeyManager_RequiresRootKey(t *testing.T) {
	_, err := NewKeyManager()
	require.ErrorIs(t, err, ErrKeyManagerNotConfigured)
}

func TestKeyManager_DerivePairKey_Deterministic(t *testing.T) {
	k, err := NewKeyManager(WithClusterRootKey([]byte(testRootKey)))
	require.NoError(t, err)

	a, err := k.DerivePairKey("g1", "node-a", "node-b")
	require.NoError(t, err)
	require.Len(t, a, pairKeyLen)

	b, err := k.DerivePairKey("g1", "node-a", "node-b")
	require.NoError(t, err)
	require.True(t, bytes.Equal(a, b), "derivation must be deterministic")
}

func TestKeyManager_DerivePairKey_OrderIndependent(t *testing.T) {
	k, err := NewKeyManager(WithClusterRootKey([]byte(testRootKey)))
	require.NoError(t, err)

	ab, err := k.DerivePairKey("g1", "node-a", "node-b")
	require.NoError(t, err)
	ba, err := k.DerivePairKey("g1", "node-b", "node-a")
	require.NoError(t, err)
	require.True(t, bytes.Equal(ab, ba),
		"swapping the unordered pair must not change the derived key")
}

func TestKeyManager_DerivePairKey_DifferentGroupYieldsDifferentKey(t *testing.T) {
	k, err := NewKeyManager(WithClusterRootKey([]byte(testRootKey)))
	require.NoError(t, err)

	g1, err := k.DerivePairKey("g1", "node-a", "node-b")
	require.NoError(t, err)
	g2, err := k.DerivePairKey("g2", "node-a", "node-b")
	require.NoError(t, err)
	require.False(t, bytes.Equal(g1, g2),
		"different groupID must produce a different key")
}

func TestKeyManager_DerivePairKey_DifferentRootYieldsDifferentKey(t *testing.T) {
	k1, err := NewKeyManager(WithClusterRootKey([]byte("root-A")))
	require.NoError(t, err)
	k2, err := NewKeyManager(WithClusterRootKey([]byte("root-B")))
	require.NoError(t, err)

	key1, err := k1.DerivePairKey("g1", "node-a", "node-b")
	require.NoError(t, err)
	key2, err := k2.DerivePairKey("g1", "node-a", "node-b")
	require.NoError(t, err)
	require.False(t, bytes.Equal(key1, key2),
		"different cluster root keys must produce different pair keys")
}

func TestKeyManager_DerivePairKey_RejectsEmptyInputs(t *testing.T) {
	k, err := NewKeyManager(WithClusterRootKey([]byte(testRootKey)))
	require.NoError(t, err)

	_, err = k.DerivePairKey("", "a", "b")
	require.Error(t, err)
	_, err = k.DerivePairKey("g", "", "b")
	require.Error(t, err)
	_, err = k.DerivePairKey("g", "a", "")
	require.Error(t, err)
}

func TestKeyManager_DerivePairKey_NotConfiguredSentinel(t *testing.T) {
	// Bypass the constructor to assert the defensive check at the call site.
	k := &KeyManager{}
	_, err := k.DerivePairKey("g", "a", "b")
	require.True(t, errors.Is(err, ErrKeyManagerNotConfigured))
}
