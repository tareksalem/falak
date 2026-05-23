package dns

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// PodmanClient is the narrow surface restore_verify needs from a
// Podman adapter. The concrete adapter lands in 11A.15; pinning the
// interface here lets us ship the verification logic with a mock.
type PodmanClient interface {
	// ConnectNetwork re-attaches container to network. Idempotent on
	// the Podman side; safe to call after an unclean restore.
	ConnectNetwork(ctx context.Context, container, network string) error
	// ExecInContainer runs cmd inside container and returns the
	// captured stdout/stderr.
	ExecInContainer(ctx context.Context, container string, cmd []string) (stdout, stderr string, err error)
}

// ErrRestoreVerifyMismatch is returned by VerifyAfterRestore when the
// inside-container resolv.conf does not point at LinkLocalDNSAddr.
// Callers check via errors.Is.
var ErrRestoreVerifyMismatch = errors.New("dns: restored resolv.conf does not point at link-local DNS")

// VerifyAfterRestore re-attaches a CRIU-restored container to its
// per-group bridge and asserts that /etc/resolv.conf inside the
// container still names LinkLocalDNSAddr as its nameserver. Returns
// ErrRestoreVerifyMismatch wrapped with the offending body when the
// assertion fails.
//
// network is the per-group Podman network name (e.g. "falak-<group>");
// resolution of that name lives in the runtime adapter.
func VerifyAfterRestore(ctx context.Context, containerID, network string, client PodmanClient) error {
	if client == nil {
		return fmt.Errorf("dns: VerifyAfterRestore requires non-nil PodmanClient")
	}
	if containerID == "" {
		return fmt.Errorf("dns: VerifyAfterRestore requires non-empty containerID")
	}
	if network == "" {
		return fmt.Errorf("dns: VerifyAfterRestore requires non-empty network")
	}

	if err := client.ConnectNetwork(ctx, containerID, network); err != nil {
		return fmt.Errorf("dns: reconnect %s to %s: %w", containerID, network, err)
	}

	stdout, stderr, err := client.ExecInContainer(ctx, containerID, []string{"cat", "/etc/resolv.conf"})
	if err != nil {
		return fmt.Errorf("dns: read resolv.conf in %s (stderr=%q): %w", containerID, stderr, err)
	}
	if !resolvConfNamesLinkLocal(stdout) {
		return fmt.Errorf("%w: container=%s body=%q", ErrRestoreVerifyMismatch, containerID, stdout)
	}
	return nil
}

// resolvConfNamesLinkLocal returns true when body contains a
// "nameserver 169.254.169.250" directive (commented lines ignored).
func resolvConfNamesLinkLocal(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.EqualFold(fields[0], "nameserver") && fields[1] == LinkLocalDNSAddr {
			return true
		}
	}
	return false
}
