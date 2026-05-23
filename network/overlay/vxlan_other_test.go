//go:build !linux

package overlay

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVXLAN_UnsupportedOnNonLinux asserts every DeviceManager method
// returns ErrUnsupported on macOS, Windows, and other non-Linux
// hosts. The Manager (11A.7) reads this sentinel during startup to
// refuse overlay-required clusters on those platforms.
func TestVXLAN_UnsupportedOnNonLinux(t *testing.T) {
	dm := NewDeviceManager()
	ctx := context.Background()

	_, err := dm.Create(ctx, "br", 1, "127.0.0.1", DefaultVXLANPort, 1450)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrUnsupported))

	require.True(t, errors.Is(dm.Destroy(ctx, "dev"), ErrUnsupported))
	require.True(t, errors.Is(dm.AddFDBEntry(ctx, "dev", "10.0.0.1"), ErrUnsupported))
	require.True(t, errors.Is(dm.RemoveFDBEntry(ctx, "dev", "10.0.0.1"), ErrUnsupported))
}
