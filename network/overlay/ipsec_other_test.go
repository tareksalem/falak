//go:build !linux

package overlay

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIPsecOther_AllMethodsUnsupported(t *testing.T) {
	m := NewIPsecManager()
	ctx := context.Background()
	require.ErrorIs(t, m.InstallSA(ctx, "1.1.1.1", "2.2.2.2", 4789, make([]byte, 32)), ErrIPsecUnsupported)
	require.ErrorIs(t, m.RemoveSA(ctx, "1.1.1.1", "2.2.2.2", 4789), ErrIPsecUnsupported)
	_, err := m.ProbeConflict(ctx, "1.1.1.1", "2.2.2.2", 4789)
	require.ErrorIs(t, err, ErrIPsecUnsupported)
}
