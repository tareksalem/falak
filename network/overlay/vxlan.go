package overlay

import (
	"context"
	"errors"
)

// ErrDeviceNotFound is returned by DeviceManager.Destroy and the FDB
// remove path when the underlying netdev (or FDB entry) is already
// gone. The Manager surfaces it so callers can distinguish "we did
// the work" from "nothing to do" without relying on string matching.
//
// All idempotent paths (Destroy, RemoveFDBEntry) MUST swallow this
// internally and return nil — the sentinel exists for the rare caller
// that wants the signal.
var ErrDeviceNotFound = errors.New("overlay: vxlan device not found")

// DefaultVXLANPort is the IANA-assigned UDP port for VXLAN
// (RFC 7348 §4.1). Callers pass 0 to fall back to this value.
const DefaultVXLANPort = 4789

// DeviceManager creates and tears down kernel VXLAN netdevs and
// manages their forwarding-database entries. The Linux implementation
// lives in vxlan_linux.go and uses vishvananda/netlink; on other
// platforms every method returns ErrUnsupported via vxlan_other.go.
//
// All methods are idempotent in the failure-mode that matters for
// the reaper: Destroy and RemoveFDBEntry treat "already gone" as
// success and return nil. Create and AddFDBEntry are NOT idempotent
// at the netdev level — callers must serialize on the per-group
// lifecycle managed by 11A.7.
type DeviceManager interface {
	// Create attaches a new kernel VXLAN device to bridgeName with
	// the given VNI, local underlay IP, UDP destination port, and
	// MTU, then returns the resulting netdev name. Pass port=0 to
	// use DefaultVXLANPort. The returned name is suitable for
	// AddFDBEntry / Destroy.
	Create(ctx context.Context, bridgeName string, vni uint32, localIP string, port int, mtu int) (string, error)

	// Destroy removes the VXLAN device created by Create. A missing
	// device is success (returns nil) so the reaper can run the same
	// path on every node without coordination.
	Destroy(ctx context.Context, name string) error

	// AddFDBEntry installs a permanent FDB entry pointing the
	// all-zeros MAC at peerExternalIP — the standard Linux pattern
	// for VXLAN dynamic VTEP discovery used when an L2 multicast
	// underlay is unavailable.
	AddFDBEntry(ctx context.Context, devName, peerExternalIP string) error

	// RemoveFDBEntry deletes the entry installed by AddFDBEntry. A
	// missing entry is success per the reaper-idempotence rule.
	RemoveFDBEntry(ctx context.Context, devName, peerExternalIP string) error
}
