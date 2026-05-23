//go:build !linux

package overlay

import (
	"context"

	"go.uber.org/zap"
)

// unsupportedDeviceManager is the non-Linux DeviceManager. macOS and
// Windows have no kernel VXLAN we can drive from userspace, so every
// method returns ErrUnsupported. The Manager (11A.7) wraps this in a
// startup check that refuses overlay-required clusters on those
// hosts; the type still implements the interface so build pipelines
// stay clean.
type unsupportedDeviceManager struct {
	logger *zap.Logger
}

// DeviceManagerOption configures a DeviceManager.
type DeviceManagerOption func(*unsupportedDeviceManager)

// WithDeviceManagerLogger replaces the default no-op logger.
func WithDeviceManagerLogger(logger *zap.Logger) DeviceManagerOption {
	return func(m *unsupportedDeviceManager) {
		if logger != nil {
			m.logger = logger
		}
	}
}

// NewDeviceManager returns the platform DeviceManager. On non-Linux
// hosts every method returns ErrUnsupported.
func NewDeviceManager(opts ...DeviceManagerOption) DeviceManager {
	m := &unsupportedDeviceManager{logger: zap.NewNop()}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Create returns ErrUnsupported.
func (unsupportedDeviceManager) Create(ctx context.Context, bridgeName string, vni uint32, localIP string, port int, mtu int) (string, error) {
	return "", ErrUnsupported
}

// Destroy returns ErrUnsupported.
func (unsupportedDeviceManager) Destroy(ctx context.Context, name string) error {
	return ErrUnsupported
}

// AddFDBEntry returns ErrUnsupported.
func (unsupportedDeviceManager) AddFDBEntry(ctx context.Context, devName, peerExternalIP string) error {
	return ErrUnsupported
}

// RemoveFDBEntry returns ErrUnsupported.
func (unsupportedDeviceManager) RemoveFDBEntry(ctx context.Context, devName, peerExternalIP string) error {
	return ErrUnsupported
}
