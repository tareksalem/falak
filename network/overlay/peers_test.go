package overlay

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

const (
	testClusterPath = "/clusters/alpha"
	testGroupID     = "group-a"
	testBridgeName  = "falak-test-br"
	testLocalIP     = "10.0.0.1"
	testLocalNodeID = "node-local"
	testBridgeMTU   = 1450
)

// newPeerTestEnv builds a fully wired PeerManager with mock device +
// ipsec backends, a real (in-memory) VNI allocator, and a fixed MTU.
// Returns the manager and its mocks for assertions.
func newPeerTestEnv(t *testing.T, opts ...PeerOption) (*PeerManager, *MockDeviceManager, *MockIPsecManager, *VNIAllocator) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "peers.sqlite")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	alloc, err := OpenVNIAllocator(WithVNIDB(db))
	require.NoError(t, err)

	dev := NewMockDeviceManager()
	ipsec := NewMockIPsecManager()
	km, err := NewKeyManager(WithClusterRootKey([]byte(testRootKey)))
	require.NoError(t, err)

	base := []PeerOption{
		WithVNIAllocator(alloc),
		WithDeviceManager(dev),
		WithIPsecManager(ipsec),
		WithKeyManager(km),
		WithLocalNodeID(testLocalNodeID),
		WithLocalIP(testLocalIP),
		WithMTUResolver(MTUResolverFunc(func() int { return testBridgeMTU })),
	}
	pm, err := NewPeerManager(append(base, opts...)...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pm.Stop() })
	return pm, dev, ipsec, alloc
}

func TestPeerManager_EnsureGroupAllocatesVNIAndDevice(t *testing.T) {
	pm, dev, _, alloc := newPeerTestEnv(t)
	ctx := context.Background()

	require.NoError(t, pm.EnsureGroup(ctx, testClusterPath, testGroupID, testBridgeName))

	creates := dev.OpsByKind("create")
	require.Len(t, creates, 1, "first EnsureGroup must create exactly one device")
	require.Equal(t, testBridgeName, creates[0].BridgeName)
	require.Equal(t, testLocalIP, creates[0].LocalIP)
	require.Equal(t, testBridgeMTU, creates[0].MTU)
	require.Equal(t, DefaultVXLANPort, creates[0].Port)

	expectedVNI, _, err := alloc.Get(testClusterPath, testGroupID)
	require.NoError(t, err)
	require.Equal(t, expectedVNI, creates[0].VNI)

	// Second call is a no-op.
	require.NoError(t, pm.EnsureGroup(ctx, testClusterPath, testGroupID, testBridgeName))
	require.Len(t, dev.OpsByKind("create"), 1, "second EnsureGroup must not create again")
}

func TestPeerManager_EnsurePeerInstallsSAAndFDB(t *testing.T) {
	pm, dev, ipsec, _ := newPeerTestEnv(t)
	ctx := context.Background()
	require.NoError(t, pm.EnsureGroup(ctx, testClusterPath, testGroupID, testBridgeName))

	const peerID, peerIP = "node-peer", "10.0.0.2"
	require.NoError(t, pm.EnsurePeer(ctx, testClusterPath, testGroupID, peerID, peerIP))

	require.True(t, ipsec.HasSA(testLocalIP, peerIP, DefaultVXLANPort), "SA must be installed")
	installs := ipsec.OpsByKind("install")
	require.Len(t, installs, 1)

	// Verify the recorded key matches the deterministic derivation.
	km, err := NewKeyManager(WithClusterRootKey([]byte(testRootKey)))
	require.NoError(t, err)
	expectedKey, err := km.DerivePairKey(testGroupID, testLocalNodeID, peerID)
	require.NoError(t, err)
	require.Equal(t, expectedKey, installs[0].Key)

	fdbAdds := dev.OpsByKind("fdb_add")
	require.Len(t, fdbAdds, 1)
	require.Equal(t, peerIP, fdbAdds[0].PeerExtIP)
}

func TestPeerManager_EnsurePeerIdempotent(t *testing.T) {
	pm, dev, ipsec, _ := newPeerTestEnv(t)
	ctx := context.Background()
	require.NoError(t, pm.EnsureGroup(ctx, testClusterPath, testGroupID, testBridgeName))

	const peerID, peerIP = "node-peer", "10.0.0.2"
	require.NoError(t, pm.EnsurePeer(ctx, testClusterPath, testGroupID, peerID, peerIP))
	require.NoError(t, pm.EnsurePeer(ctx, testClusterPath, testGroupID, peerID, peerIP))

	require.Len(t, ipsec.OpsByKind("install"), 1, "second EnsurePeer must not re-install SA")
	require.Len(t, dev.OpsByKind("fdb_add"), 1, "second EnsurePeer must not re-add FDB")
}

func TestPeerManager_RemovePeerWithFlapGrace(t *testing.T) {
	pm, dev, ipsec, _ := newPeerTestEnv(t, WithFlapGrace(50*time.Millisecond))
	ctx := context.Background()
	require.NoError(t, pm.EnsureGroup(ctx, testClusterPath, testGroupID, testBridgeName))
	const peerID, peerIP = "node-peer", "10.0.0.2"
	require.NoError(t, pm.EnsurePeer(ctx, testClusterPath, testGroupID, peerID, peerIP))

	require.NoError(t, pm.RemovePeer(ctx, testClusterPath, testGroupID, peerID))

	// At 25ms (before grace expires), SA and FDB must still be installed.
	time.Sleep(25 * time.Millisecond)
	require.True(t, ipsec.HasSA(testLocalIP, peerIP, DefaultVXLANPort), "SA still installed mid-grace")
	require.True(t, dev.HasFDBEntry("mock-vx-"+toHex(_vniOf(t, pm)), peerIP), "FDB still installed mid-grace")

	// At 75ms (after grace), teardown must have run.
	time.Sleep(75 * time.Millisecond)
	require.False(t, ipsec.HasSA(testLocalIP, peerIP, DefaultVXLANPort), "SA removed post-grace")
	require.False(t, dev.HasFDBEntry("mock-vx-"+toHex(_vniOf(t, pm)), peerIP), "FDB removed post-grace")
}

func TestPeerManager_FlapCancel(t *testing.T) {
	pm, dev, ipsec, _ := newPeerTestEnv(t, WithFlapGrace(75*time.Millisecond))
	ctx := context.Background()
	require.NoError(t, pm.EnsureGroup(ctx, testClusterPath, testGroupID, testBridgeName))
	const peerID, peerIP = "node-peer", "10.0.0.2"
	require.NoError(t, pm.EnsurePeer(ctx, testClusterPath, testGroupID, peerID, peerIP))

	require.NoError(t, pm.RemovePeer(ctx, testClusterPath, testGroupID, peerID))

	// Re-Ensure within the grace window must cancel the timer.
	time.Sleep(25 * time.Millisecond)
	require.NoError(t, pm.EnsurePeer(ctx, testClusterPath, testGroupID, peerID, peerIP))

	// Wait past the original grace deadline; SA + FDB must still be live.
	time.Sleep(100 * time.Millisecond)
	require.True(t, ipsec.HasSA(testLocalIP, peerIP, DefaultVXLANPort), "flap-cancel keeps SA")
	require.True(t, dev.HasFDBEntry("mock-vx-"+toHex(_vniOf(t, pm)), peerIP), "flap-cancel keeps FDB")

	// And no Remove ops should have been recorded.
	require.Len(t, ipsec.OpsByKind("remove"), 0)
	require.Len(t, dev.OpsByKind("fdb_remove"), 0)
}

func TestPeerManager_RemoveGroupTearsDownEverything(t *testing.T) {
	pm, dev, ipsec, alloc := newPeerTestEnv(t)
	ctx := context.Background()
	require.NoError(t, pm.EnsureGroup(ctx, testClusterPath, testGroupID, testBridgeName))
	peers := map[string]string{
		"node-b": "10.0.0.2",
		"node-c": "10.0.0.3",
		"node-d": "10.0.0.4",
	}
	for id, ip := range peers {
		require.NoError(t, pm.EnsurePeer(ctx, testClusterPath, testGroupID, id, ip))
	}

	vni := _vniOf(t, pm)
	require.NoError(t, pm.RemoveGroup(ctx, testClusterPath, testGroupID))

	// Every SA + FDB entry is gone.
	for _, ip := range peers {
		require.False(t, ipsec.HasSA(testLocalIP, ip, DefaultVXLANPort), "SA must be removed")
		require.False(t, dev.HasFDBEntry("mock-vx-"+toHex(vni), ip), "FDB must be removed")
	}
	// Device destroyed.
	require.False(t, dev.HasDevice("mock-vx-"+toHex(vni)))
	require.Len(t, dev.OpsByKind("destroy"), 1)
	// VNI released.
	_, ok, err := alloc.Get(testClusterPath, testGroupID)
	require.NoError(t, err)
	require.False(t, ok, "VNI must be released")
}

func TestPeerManager_RemoveGroupCancelsPendingGraceTimers(t *testing.T) {
	pm, dev, ipsec, _ := newPeerTestEnv(t, WithFlapGrace(500*time.Millisecond))
	ctx := context.Background()
	require.NoError(t, pm.EnsureGroup(ctx, testClusterPath, testGroupID, testBridgeName))
	const peerID, peerIP = "node-peer", "10.0.0.2"
	require.NoError(t, pm.EnsurePeer(ctx, testClusterPath, testGroupID, peerID, peerIP))

	// Schedule a removal but never let the grace expire.
	require.NoError(t, pm.RemovePeer(ctx, testClusterPath, testGroupID, peerID))

	// RemoveGroup must tear down immediately, not wait the grace.
	start := time.Now()
	require.NoError(t, pm.RemoveGroup(ctx, testClusterPath, testGroupID))
	require.Less(t, time.Since(start), 200*time.Millisecond, "RemoveGroup must not wait the grace")

	require.False(t, ipsec.HasSA(testLocalIP, peerIP, DefaultVXLANPort))
	require.Len(t, ipsec.OpsByKind("remove"), 1)
	require.Len(t, dev.OpsByKind("fdb_remove"), 1)
	require.Len(t, dev.OpsByKind("destroy"), 1)
}

func TestPeerManager_StopCancelsEverything(t *testing.T) {
	pm, _, _, _ := newPeerTestEnv(t, WithFlapGrace(60*time.Second))
	ctx := context.Background()
	require.NoError(t, pm.EnsureGroup(ctx, testClusterPath, testGroupID, testBridgeName))
	require.NoError(t, pm.EnsurePeer(ctx, testClusterPath, testGroupID, "node-b", "10.0.0.2"))
	require.NoError(t, pm.RemovePeer(ctx, testClusterPath, testGroupID, "node-b"))

	done := make(chan struct{})
	go func() {
		_ = pm.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return; flap-grace timer leaked")
	}
	// Subsequent calls return ErrPeerManagerStopped.
	require.ErrorIs(t, pm.EnsureGroup(ctx, testClusterPath, "g2", testBridgeName), ErrPeerManagerStopped)
	require.ErrorIs(t, pm.EnsurePeer(ctx, testClusterPath, testGroupID, "node-c", "10.0.0.3"), ErrPeerManagerStopped)
	require.ErrorIs(t, pm.RemovePeer(ctx, testClusterPath, testGroupID, "node-b"), ErrPeerManagerStopped)
	require.ErrorIs(t, pm.RemoveGroup(ctx, testClusterPath, testGroupID), ErrPeerManagerStopped)
}

func TestPeerManager_EnsureGroupValidatesInputs(t *testing.T) {
	pm, _, _, _ := newPeerTestEnv(t)
	ctx := context.Background()
	require.Error(t, pm.EnsureGroup(ctx, "", testGroupID, testBridgeName))
	require.Error(t, pm.EnsureGroup(ctx, testClusterPath, "", testBridgeName))
	require.Error(t, pm.EnsureGroup(ctx, testClusterPath, testGroupID, ""))
}

func TestPeerManager_EnsurePeerRequiresEnsureGroup(t *testing.T) {
	pm, _, _, _ := newPeerTestEnv(t)
	err := pm.EnsurePeer(context.Background(), testClusterPath, testGroupID, "n", "1.1.1.1")
	require.Error(t, err)
}

func TestPeerManager_NewRejectsMissingOptions(t *testing.T) {
	_, err := NewPeerManager()
	require.Error(t, err)
}

// _vniOf returns the VNI the manager allocated for the test group. We
// read it back via the allocator rather than expose internal state.
func _vniOf(t *testing.T, pm *PeerManager) uint32 {
	t.Helper()
	v, ok, err := pm.vniAlloc.Get(testClusterPath, testGroupID)
	require.NoError(t, err)
	require.True(t, ok, "VNI must be allocated")
	return v
}

// toHex matches the mock device manager's naming convention.
func toHex(v uint32) string {
	const hexd = "0123456789abcdef"
	var b []byte
	if v == 0 {
		return "0"
	}
	for v > 0 {
		b = append([]byte{hexd[v&0xF]}, b...)
		v >>= 4
	}
	return string(b)
}
