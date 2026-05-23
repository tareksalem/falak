//go:build !linux

package overlay

import (
	"context"

	"go.uber.org/zap"
)

// unsupportedIPsecManager is the non-Linux IPsecManager. XFRM is
// Linux-specific; macOS/Windows callers must rely on the existing
// libp2p mTLS perimeter and refuse to start with overlay enabled.
type unsupportedIPsecManager struct {
	logger *zap.Logger
}

// IPsecManagerOption configures an IPsecManager.
type IPsecManagerOption func(*unsupportedIPsecManager)

// WithIPsecManagerLogger replaces the default no-op logger.
func WithIPsecManagerLogger(l *zap.Logger) IPsecManagerOption {
	return func(m *unsupportedIPsecManager) {
		if l != nil {
			m.logger = l
		}
	}
}

// NewIPsecManager returns the platform IPsecManager. On non-Linux
// hosts every method returns ErrIPsecUnsupported.
func NewIPsecManager(opts ...IPsecManagerOption) IPsecManager {
	m := &unsupportedIPsecManager{logger: zap.NewNop()}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// InstallSA returns ErrIPsecUnsupported.
func (unsupportedIPsecManager) InstallSA(ctx context.Context, local, peer string, port int, key []byte) error {
	return ErrIPsecUnsupported
}

// RemoveSA returns ErrIPsecUnsupported.
func (unsupportedIPsecManager) RemoveSA(ctx context.Context, local, peer string, port int) error {
	return ErrIPsecUnsupported
}

// ProbeConflict returns ErrIPsecUnsupported.
func (unsupportedIPsecManager) ProbeConflict(ctx context.Context, local, peer string, port int) (bool, error) {
	return false, ErrIPsecUnsupported
}
