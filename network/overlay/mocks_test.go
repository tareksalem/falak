package overlay

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMockDeviceManager_SatisfiesInterface(t *testing.T) {
	var _ DeviceManager = NewMockDeviceManager()
}

func TestMockIPsecManager_SatisfiesInterface(t *testing.T) {
	var _ IPsecManager = NewMockIPsecManager()
}

func TestMockDeviceManager_RecordsCreateAndFDB(t *testing.T) {
	m := NewMockDeviceManager()
	ctx := context.Background()

	name, err := m.Create(ctx, "br-test", 0xabc, "10.0.0.1", DefaultVXLANPort, 1450)
	require.NoError(t, err)
	require.NotEmpty(t, name)
	require.True(t, m.HasDevice(name))

	require.NoError(t, m.AddFDBEntry(ctx, name, "10.0.0.2"))
	require.True(t, m.HasFDBEntry(name, "10.0.0.2"))

	require.NoError(t, m.RemoveFDBEntry(ctx, name, "10.0.0.2"))
	require.False(t, m.HasFDBEntry(name, "10.0.0.2"))

	// Double-remove is success.
	require.NoError(t, m.RemoveFDBEntry(ctx, name, "10.0.0.2"))

	require.NoError(t, m.Destroy(ctx, name))
	require.False(t, m.HasDevice(name))
}

func TestMockDeviceManager_AddFDBOnMissingDevice(t *testing.T) {
	m := NewMockDeviceManager()
	err := m.AddFDBEntry(context.Background(), "mock-vx-nope", "10.0.0.2")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrDeviceNotFound))
}

func TestMockDeviceManager_FailNext(t *testing.T) {
	m := NewMockDeviceManager()
	boom := errors.New("boom")
	m.FailNext = func(kind string) error {
		if kind == "create" {
			return boom
		}
		return nil
	}
	_, err := m.Create(context.Background(), "br", 1, "1.1.1.1", DefaultVXLANPort, 1450)
	require.ErrorIs(t, err, boom)
}

func TestMockIPsecManager_RecordsInstallRemove(t *testing.T) {
	m := NewMockIPsecManager()
	ctx := context.Background()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	require.NoError(t, m.InstallSA(ctx, "1.1.1.1", "2.2.2.2", 4789, key))
	require.True(t, m.HasSA("1.1.1.1", "2.2.2.2", 4789))
	got, err := m.SAKey("1.1.1.1", "2.2.2.2", 4789)
	require.NoError(t, err)
	require.Equal(t, key, got)

	require.NoError(t, m.RemoveSA(ctx, "1.1.1.1", "2.2.2.2", 4789))
	require.False(t, m.HasSA("1.1.1.1", "2.2.2.2", 4789))

	installOps := m.OpsByKind("install")
	require.Len(t, installOps, 1)
	removeOps := m.OpsByKind("remove")
	require.Len(t, removeOps, 1)
}

func TestMockIPsecManager_ProbeConflictResponse(t *testing.T) {
	m := NewMockIPsecManager()
	conflict, err := m.ProbeConflict(context.Background(), "1.1.1.1", "2.2.2.2", 4789)
	require.NoError(t, err)
	require.False(t, conflict)

	m.ConflictResponse = true
	conflict, err = m.ProbeConflict(context.Background(), "1.1.1.1", "2.2.2.2", 4789)
	require.NoError(t, err)
	require.True(t, conflict)
}
