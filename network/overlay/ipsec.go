package overlay

import (
	"context"
	"errors"
)

// ErrConflict signals that an existing XFRM policy (installed by an
// operator or another agent) already covers the requested
// (local, peer, UDP/port) selector. The peer manager surfaces this so
// the operator can either carve the existing rule out of UDP/4789 or
// explicitly opt in with network.allow_xfrm_takeover (see plan 11A.6).
var ErrConflict = errors.New("overlay: conflicting non-falak xfrm policy present")

// ErrIPsecUnsupported is returned by the IPsecManager implementation
// on non-Linux platforms (Windows/macOS). Distinct from the MTU-side
// ErrUnsupported so callers can match each subsystem independently.
var ErrIPsecUnsupported = errors.New("overlay: ipsec transport unsupported on this platform")

// FalakXfrmMarkValue tags every XFRM state and policy installed by
// Falak. ProbeConflict skips Falak-marked rules; operators inspecting
// `ip xfrm policy list` see a recognizable owner. Picked outside the
// common Linux mark conventions (0x1, 0x2, etc.) to minimize collision.
const FalakXfrmMarkValue uint32 = 0xFA1A

// FalakXfrmMarkMask is the mask paired with FalakXfrmMarkValue. The
// full 16 low bits are matched so other agents can use the upper bits.
const FalakXfrmMarkMask uint32 = 0xFFFF

// IPsecManager installs and removes per-pair IPsec transport-mode SAs
// for the VXLAN overlay. Concrete implementations live in
// ipsec_linux.go (XFRM) and ipsec_other.go (stub).
//
// All methods are idempotent in the failure-mode the peer manager
// cares about: RemoveSA treats "not found" as success so the flap-
// grace teardown path can run on every node without coordination.
type IPsecManager interface {
	// InstallSA installs a matched outbound + inbound transport-mode
	// SA pair plus the matching policies for the
	// (local, peer, UDP/port) selector, using AES-GCM with the given
	// 32-byte key. Installation is idempotent at the pair level:
	// re-installing the same SAs is treated as success.
	InstallSA(ctx context.Context, local, peer string, port int, key []byte) error

	// RemoveSA tears down both halves of a previously-installed SA pair
	// plus their policies. Missing entries are success — the reaper
	// rule from plan 11A.6 demands two nodes both running the teardown
	// path safely.
	RemoveSA(ctx context.Context, local, peer string, port int) error

	// ProbeConflict returns (true, nil) when a non-Falak XFRM policy
	// already covers (local, peer, UDP/port). Used at SA install time
	// AND at node startup so operators see a clear error rather than
	// silent double-encryption.
	ProbeConflict(ctx context.Context, local, peer string, port int) (bool, error)
}
