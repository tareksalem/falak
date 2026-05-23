//go:build linux

package overlay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// vxlanNamePrefix + 6 hex digits of the 24-bit VNI fits Linux's
// 15-byte IFNAMSIZ limit (9 + 6 = 15). Hex keeps it short/reversible.
const vxlanNamePrefix = "falak-vx-"

// allZeroMAC marks a permanent FDB entry as the default-VTEP for a
// VXLAN device — Linux's pattern for "send all unknown-destination
// frames over this tunnel to this remote VTEP IP".
var allZeroMAC = net.HardwareAddr{0, 0, 0, 0, 0, 0}

// linuxDeviceManager is the production DeviceManager backed by
// vishvananda/netlink. Stateless; the per-group orchestrator (11A.7)
// owns naming, locking, and teardown order.
type linuxDeviceManager struct {
	logger *zap.Logger
}

// DeviceManagerOption configures a DeviceManager.
type DeviceManagerOption func(*linuxDeviceManager)

// WithDeviceManagerLogger replaces the default no-op logger.
func WithDeviceManagerLogger(l *zap.Logger) DeviceManagerOption {
	return func(m *linuxDeviceManager) {
		if l != nil {
			m.logger = l
		}
	}
}

// NewDeviceManager constructs the platform DeviceManager. On Linux
// this wraps netlink; on every other GOOS the !linux build tag
// returns a stub whose every method yields ErrUnsupported.
func NewDeviceManager(opts ...DeviceManagerOption) DeviceManager {
	m := &linuxDeviceManager{logger: zap.NewNop()}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Create installs a kernel VXLAN device attached to bridgeName.
// Looks up the bridge, builds the netlink.Vxlan, adds it, sets the
// master, brings it up. Any post-LinkAdd failure rolls the device
// back so the kernel never carries a half-configured tunnel.
func (m *linuxDeviceManager) Create(ctx context.Context, bridgeName string, vni uint32, localIP string, port int, mtu int) (string, error) {
	if bridgeName == "" {
		return "", errors.New("overlay: empty bridge name")
	}
	if vni == 0 || vni&^vniMask24 != 0 {
		return "", fmt.Errorf("overlay: invalid VNI %d (must be 0 < vni < 0x1000000)", vni)
	}
	if mtu <= 0 {
		return "", fmt.Errorf("overlay: invalid MTU %d", mtu)
	}
	local := net.ParseIP(localIP)
	if local == nil {
		return "", fmt.Errorf("overlay: parse local IP %q", localIP)
	}
	if v4 := local.To4(); v4 != nil {
		local = v4
	}
	udpPort := port
	if udpPort <= 0 {
		udpPort = DefaultVXLANPort
	}
	bridgeLink, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return "", fmt.Errorf("overlay: lookup bridge %s: %w", bridgeName, err)
	}
	name := fmt.Sprintf("%s%x", vxlanNamePrefix, vni)
	if len(name) > 15 {
		return "", fmt.Errorf("overlay: vxlan name %q exceeds IFNAMSIZ", name)
	}
	link := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: name, MTU: mtu},
		VxlanId:   int(vni), Port: udpPort, SrcAddr: local, Learning: false,
	}
	if err := netlink.LinkAdd(link); err != nil {
		return "", fmt.Errorf("overlay: link add %s: %w", name, err)
	}
	if err := m.attachAndUp(link, bridgeLink); err != nil {
		if delErr := netlink.LinkDel(link); delErr != nil {
			m.logger.Error("overlay: vxlan rollback delete failed",
				zap.String("device", name), zap.Error(delErr))
		}
		return "", err
	}
	m.logger.Info("vxlan device created",
		zap.String("device", name), zap.String("bridge", bridgeName),
		zap.Uint32("vni", vni), zap.String("local", local.String()),
		zap.Int("port", udpPort), zap.Int("mtu", mtu))
	return name, nil
}

func (m *linuxDeviceManager) attachAndUp(link, bridge netlink.Link) error {
	if err := netlink.LinkSetMaster(link, bridge); err != nil {
		return fmt.Errorf("overlay: attach %s to %s: %w", link.Attrs().Name, bridge.Attrs().Name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("overlay: bring up %s: %w", link.Attrs().Name, err)
	}
	return nil
}

// Destroy removes the VXLAN device by name; missing device = nil
// (reaper-idempotence).
func (m *linuxDeviceManager) Destroy(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("overlay: empty device name")
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		if isLinkNotFound(err) {
			m.logger.Debug("vxlan destroy on missing device is a no-op", zap.String("device", name))
			return nil
		}
		return fmt.Errorf("overlay: lookup %s: %w", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		if isLinkNotFound(err) {
			return nil
		}
		return fmt.Errorf("overlay: delete %s: %w", name, err)
	}
	m.logger.Info("vxlan device destroyed", zap.String("device", name))
	return nil
}

// AddFDBEntry appends a permanent neighbor with the all-zeros MAC
// pointing at peerExternalIP — Linux's VTEP-discovery pattern when
// no multicast underlay is available.
func (m *linuxDeviceManager) AddFDBEntry(ctx context.Context, devName, peerExternalIP string) error {
	neigh, peer, err := m.buildFDBNeigh(devName, peerExternalIP)
	if err != nil {
		return err
	}
	if err := netlink.NeighAppend(neigh); err != nil {
		return fmt.Errorf("overlay: fdb append %s -> %s: %w", devName, peer, err)
	}
	m.logger.Info("vxlan fdb entry added", zap.String("device", devName), zap.String("peer", peer))
	return nil
}

// RemoveFDBEntry deletes the entry installed by AddFDBEntry. Missing
// device or missing entry both return nil per reaper-idempotence.
func (m *linuxDeviceManager) RemoveFDBEntry(ctx context.Context, devName, peerExternalIP string) error {
	neigh, peer, err := m.buildFDBNeigh(devName, peerExternalIP)
	if err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			m.logger.Debug("vxlan fdb remove on missing device is a no-op",
				zap.String("device", devName), zap.String("peer", peer))
			return nil
		}
		return err
	}
	if err := netlink.NeighDel(neigh); err != nil {
		if isFDBNotFound(err) {
			m.logger.Debug("vxlan fdb remove on missing entry is a no-op",
				zap.String("device", devName), zap.String("peer", peer))
			return nil
		}
		return fmt.Errorf("overlay: fdb del %s -> %s: %w", devName, peer, err)
	}
	m.logger.Info("vxlan fdb entry removed", zap.String("device", devName), zap.String("peer", peer))
	return nil
}

// buildFDBNeigh validates inputs and constructs the AF_BRIDGE neigh
// row shared by AddFDBEntry and RemoveFDBEntry. Returns ErrDeviceNotFound
// (idempotent in the remove path) when the device is gone, and a
// validation error otherwise.
func (m *linuxDeviceManager) buildFDBNeigh(devName, peerExternalIP string) (*netlink.Neigh, string, error) {
	if devName == "" {
		return nil, "", errors.New("overlay: empty device name")
	}
	peer := net.ParseIP(peerExternalIP)
	if peer == nil {
		return nil, "", fmt.Errorf("overlay: parse peer IP %q", peerExternalIP)
	}
	if v4 := peer.To4(); v4 != nil {
		peer = v4
	}
	link, err := netlink.LinkByName(devName)
	if err != nil {
		if isLinkNotFound(err) {
			return nil, peer.String(), fmt.Errorf("%w: %s", ErrDeviceNotFound, devName)
		}
		return nil, peer.String(), fmt.Errorf("overlay: lookup %s: %w", devName, err)
	}
	return &netlink.Neigh{
		LinkIndex: link.Attrs().Index, Family: unix.AF_BRIDGE,
		State: netlink.NUD_PERMANENT, Flags: netlink.NTF_SELF,
		IP:           peer,
		HardwareAddr: append(net.HardwareAddr(nil), allZeroMAC...),
	}, peer.String(), nil
}

// isLinkNotFound and isFDBNotFound recognize the ENODEV / ENOENT
// surfaces vishvananda/netlink reports for missing links and missing
// FDB rows. Both spellings are stable across the v1.x line.
func isLinkNotFound(err error) bool {
	if err == nil {
		return false
	}
	var lnf netlink.LinkNotFoundError
	if errors.As(err, &lnf) {
		return true
	}
	m := err.Error()
	return strings.Contains(m, "Link not found") ||
		strings.Contains(m, "no such network interface") ||
		strings.Contains(m, "no such device")
}

func isFDBNotFound(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "no such file or directory") ||
		strings.Contains(m, "no such entry") ||
		strings.Contains(m, "not found")
}
