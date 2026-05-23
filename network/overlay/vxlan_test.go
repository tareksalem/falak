//go:build linux

package overlay

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

// requireVXLANSupport gates the kernel-touching tests behind two
// preconditions: the current process must be root (CAP_NET_ADMIN
// implied) and /sys/module/vxlan must exist (kernel module loaded
// or built-in). Failing either causes a t.Skip — unit-test pipelines
// running unprivileged stay green.
func requireVXLANSupport(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("requires root + vxlan module")
	}
	if _, err := os.Stat("/sys/module/vxlan"); err != nil {
		t.Skip("requires root + vxlan module")
	}
}

// makeTestBridge creates a fresh Linux bridge for the test and
// cleans it up via t.Cleanup. The VXLAN device under test will be
// attached as a slave.
func makeTestBridge(t *testing.T, name string) {
	t.Helper()
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
	require.NoError(t, netlink.LinkAdd(br))
	t.Cleanup(func() { _ = netlink.LinkDel(br) })
	require.NoError(t, netlink.LinkSetUp(br))
}

func TestVXLAN_CreateDestroyRoundTrip(t *testing.T) {
	requireVXLANSupport(t)
	const bridge = "falak-vxt-br"
	makeTestBridge(t, bridge)

	dm := NewDeviceManager()
	ctx := context.Background()

	name, err := dm.Create(ctx, bridge, 0x123abc, "127.0.0.1", DefaultVXLANPort, 1450)
	require.NoError(t, err)
	require.NotEmpty(t, name)

	// Sanity-check the kernel actually has it.
	link, err := netlink.LinkByName(name)
	require.NoError(t, err)
	vx, ok := link.(*netlink.Vxlan)
	require.True(t, ok)
	require.Equal(t, 0x123abc, vx.VxlanId)

	require.NoError(t, dm.Destroy(ctx, name))

	_, err = netlink.LinkByName(name)
	require.Error(t, err)
}

func TestVXLAN_DestroyMissingIsNil(t *testing.T) {
	requireVXLANSupport(t)
	dm := NewDeviceManager()
	require.NoError(t, dm.Destroy(context.Background(), "falak-vx-nope"))
}

func TestVXLAN_FDBRoundTrip(t *testing.T) {
	requireVXLANSupport(t)
	const bridge = "falak-vxt-fdb"
	makeTestBridge(t, bridge)

	dm := NewDeviceManager()
	ctx := context.Background()
	name, err := dm.Create(ctx, bridge, 0x4567, "127.0.0.1", DefaultVXLANPort, 1450)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dm.Destroy(ctx, name) })

	require.NoError(t, dm.AddFDBEntry(ctx, name, "10.0.0.42"))
	require.NoError(t, dm.RemoveFDBEntry(ctx, name, "10.0.0.42"))
	// Second remove must be idempotent.
	require.NoError(t, dm.RemoveFDBEntry(ctx, name, "10.0.0.42"))
}

func TestVXLAN_InvalidArgs(t *testing.T) {
	// Argument validation runs before any netlink call, so this test
	// is safe even when the kernel module isn't loaded.
	dm := NewDeviceManager()
	ctx := context.Background()

	_, err := dm.Create(ctx, "", 1, "127.0.0.1", DefaultVXLANPort, 1450)
	require.Error(t, err)

	_, err = dm.Create(ctx, "br", 0, "127.0.0.1", DefaultVXLANPort, 1450)
	require.Error(t, err)

	_, err = dm.Create(ctx, "br", 1, "not-an-ip", DefaultVXLANPort, 1450)
	require.Error(t, err)

	_, err = dm.Create(ctx, "br", 1, "127.0.0.1", DefaultVXLANPort, 0)
	require.Error(t, err)

	require.Error(t, dm.Destroy(ctx, ""))
	require.Error(t, dm.AddFDBEntry(ctx, "", "127.0.0.1"))
	require.Error(t, dm.AddFDBEntry(ctx, "dev", "not-an-ip"))
	require.Error(t, dm.RemoveFDBEntry(ctx, "", "127.0.0.1"))
	require.Error(t, dm.RemoveFDBEntry(ctx, "dev", "not-an-ip"))
}

// TestVXLAN_ErrDeviceNotFoundSurfaced documents that AddFDBEntry on a
// missing device returns the public sentinel so callers may detect
// it via errors.Is without inspecting message text.
func TestVXLAN_ErrDeviceNotFoundSurfaced(t *testing.T) {
	requireVXLANSupport(t)
	dm := NewDeviceManager()
	err := dm.AddFDBEntry(context.Background(), "falak-vx-missing", "10.0.0.1")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrDeviceNotFound))
}
