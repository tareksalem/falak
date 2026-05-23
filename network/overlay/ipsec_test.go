//go:build linux

package overlay

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireXFRMSupport gates the kernel-touching tests behind root +
// xfrm. Tests skip when running unprivileged or on stripped-down hosts
// (CI containers without xfrm_user) — failing those would just yield
// flaky CI noise.
func requireXFRMSupport(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("requires root + xfrm")
	}
	// `ip xfrm policy list` exits 0 on hosts where xfrm_user is loaded.
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("requires iproute2")
	}
	if err := exec.Command("ip", "xfrm", "policy", "list").Run(); err != nil {
		t.Skip("xfrm not available")
	}
}

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestIPsec_InstallProbeRemoveRoundTrip(t *testing.T) {
	requireXFRMSupport(t)
	m := NewIPsecManager()
	ctx := context.Background()
	local, peer, port := "127.0.0.1", "127.0.0.2", 4789

	// Pre-test cleanup in case a prior failing run leaked state.
	_ = m.RemoveSA(ctx, local, peer, port)
	t.Cleanup(func() { _ = m.RemoveSA(ctx, local, peer, port) })

	require.NoError(t, m.InstallSA(ctx, local, peer, port, testKey(t)))

	// ProbeConflict should ignore Falak-marked rows.
	conflict, err := m.ProbeConflict(ctx, local, peer, port)
	require.NoError(t, err)
	require.False(t, conflict, "Falak's own mark must not register as conflict")

	require.NoError(t, m.RemoveSA(ctx, local, peer, port))
	// Idempotent re-remove.
	require.NoError(t, m.RemoveSA(ctx, local, peer, port))
	// Re-install after remove succeeds.
	require.NoError(t, m.InstallSA(ctx, local, peer, port, testKey(t)))
}

func TestIPsec_InstallRejectsBadKey(t *testing.T) {
	// Argument validation runs before any netlink call; safe to run
	// unprivileged.
	m := NewIPsecManager()
	require.Error(t, m.InstallSA(context.Background(), "1.1.1.1", "2.2.2.2", 4789, make([]byte, 16)))
	require.Error(t, m.InstallSA(context.Background(), "bad-ip", "2.2.2.2", 4789, make([]byte, 32)))
	require.Error(t, m.InstallSA(context.Background(), "1.1.1.1", "2.2.2.2", 0, make([]byte, 32)))
}
