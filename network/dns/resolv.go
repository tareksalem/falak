package dns

import (
	"fmt"
	"os"
	"path/filepath"
)

// LinkLocalDNSAddr is the fixed link-local address every per-group
// bridge routes to the local DNS responder. Containers' resolv.conf
// points at this address regardless of the host node, which is what
// makes CRIU snapshot restore safe: a container restored on a new
// node finds the new node's responder without any post-restore
// rewrite.
const LinkLocalDNSAddr = "169.254.169.250"

// canonicalResolvConf is the body Falak injects into every container.
//
//   - ndots:0 — bare names like "billing" must hit the responder
//     instead of being expanded through search domains.
//   - timeout:1 — keep DNS retries snappy (1s per query).
//   - attempts:2 — two tries before giving up keeps short transient
//     glitches from cascading into application errors.
const canonicalResolvConf = "nameserver " + LinkLocalDNSAddr + "\n" +
	"options ndots:0 timeout:1 attempts:2\n"

// BuildResolvConf returns the canonical /etc/resolv.conf body Falak
// injects into every container it manages. The body is byte-identical
// across nodes so snapshot restores never require a rewrite.
func BuildResolvConf() string {
	return canonicalResolvConf
}

// WriteResolvConf writes the canonical resolv.conf body to path with
// mode 0644. The parent directory must exist. The write is idempotent
// (always overwrites) so it can be called from a snapshot-restore
// fallback path without coordination. Returns an error wrapped with
// the target path on failure.
func WriteResolvConf(path string) error {
	if path == "" {
		return fmt.Errorf("dns: WriteResolvConf requires non-empty path")
	}
	if dir := filepath.Dir(path); dir != "" {
		if _, err := os.Stat(dir); err != nil {
			return fmt.Errorf("dns: resolv.conf parent dir %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(canonicalResolvConf), 0o644); err != nil {
		return fmt.Errorf("dns: write resolv.conf %q: %w", path, err)
	}
	return nil
}

// PodmanDNSFlags returns the Podman create/run flags that make Podman
// manage the container's /etc/resolv.conf to point at the link-local
// DNS address. This is the preferred path; WriteResolvConf is the
// fallback used by snapshot-restore prep where Podman is not driving
// the resolv.conf generation.
func PodmanDNSFlags() []string {
	return []string{
		"--dns=" + LinkLocalDNSAddr,
		"--dns-search=",
		"--dns-option=ndots:0",
	}
}
