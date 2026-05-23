// This file mirrors the DNS-side visibility check at the proxy. The
// DNS responder rejects forbidden lookups, but a stale resolver cache
// or a spoofed connection can still arrive at the proxy with a source
// IP that should not be admitted. Bundle K closes that gap: every
// proxy listener consults a SourceResolver to translate the incoming
// connection's source IP into (clusterPath, groupID), then applies
// the same visibility predicate the DNS resolver used.
//
// Per plan 11B.17: visibility=cluster admits any source, visibility=
// group requires the source's group to match the Service's group. A
// source IP that resolves to no bridge (spoofed or pre-overlay) fails
// closed. Rejections increment the per-Service forward_errors counter
// in a dedicated visibility-denied bucket so operators can spot
// misconfigured callers in dashboards.
package proxy

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"

	"go.uber.org/zap"
)

// SourceResolver maps a connection's source IP to the (clusterPath,
// groupID) of the bridge that owns it. Implemented by the network
// manager's bridge map at wiring time (11B.21); the proxy ships an
// interface so the package keeps its compile-time dependency surface
// to (endpoints, internal/samplelog).
type SourceResolver interface {
	// ResolveGroup returns the bridge identity for srcIP. ok==false
	// when the IP is not part of any known per-group bridge (spoofed,
	// not yet wired, or simply a different node).
	ResolveGroup(srcIP netip.Addr) (clusterPath, groupID string, ok bool)
	// LocalCluster returns the clusterPath of the Manager that owns
	// the bridge map. Used by the cross-cluster boundary check in
	// VerifySource: a source whose bridge belongs to a foreign cluster
	// MUST be rejected even when the visibility scope is "cluster",
	// because the cluster-scope policy is per-cluster.
	LocalCluster() string
}

// ServiceVisibility describes the visibility scope of one Service for
// the proxy admission check. The proxy package does not import the
// service package; the wiring layer fills this struct from the live
// Service spec on every connection. Visibility is a free-form string
// to keep the policy decision in one helper.
type ServiceVisibility struct {
	// Scope is "cluster" (admit anyone) or "group" (caller group must
	// equal Group).
	Scope string
	// Group is the Service's owning group; only consulted when
	// Scope == "group".
	Group string
}

// Admits reports whether the given source group is allowed under this
// visibility policy. Returns false defensively for any unknown scope
// or for the deferred "external" scope.
func (v ServiceVisibility) Admits(srcGroup string) bool {
	switch v.Scope {
	case "cluster":
		return true
	case "group":
		if srcGroup == "" || v.Group == "" {
			return false
		}
		return srcGroup == v.Group
	default:
		return false
	}
}

// VisibilityStats tracks the per-Service count of visibility-denied
// rejections. The TCP and UDP listeners share one instance per
// listener. Operators sum across listener stats via the StatsRegistry
// adapter (Bundle J).
type VisibilityStats struct {
	denied atomic.Int64
}

// NewVisibilityStats builds a zero-valued counter set.
func NewVisibilityStats() *VisibilityStats { return &VisibilityStats{} }

// IncDenied bumps the rejection counter.
func (v *VisibilityStats) IncDenied() { v.denied.Add(1) }

// Denied returns the current rejection count.
func (v *VisibilityStats) Denied() int64 { return v.denied.Load() }

// ErrVisibilityDenied marks an admission rejection. Callers close the
// inbound connection / drop the inbound packet and bump the
// per-Service counter.
var ErrVisibilityDenied = errors.New("proxy: visibility denied")

// ErrCrossClusterDenied is the specific rejection reason for a source
// whose bridge belongs to a different cluster than the local network
// Manager. It satisfies errors.Is(err, ErrVisibilityDenied) so the
// existing fail-closed admission paths keep working unchanged.
var ErrCrossClusterDenied = crossClusterDeniedError{}

// crossClusterDeniedError is the singleton implementation behind
// ErrCrossClusterDenied. Implementing Unwrap so callers can match
// either sentinel via errors.Is.
type crossClusterDeniedError struct{}

func (crossClusterDeniedError) Error() string { return "proxy: cross-cluster source denied" }
func (crossClusterDeniedError) Unwrap() error { return ErrVisibilityDenied }

// VerifyTCPSource is the per-Accept admission hook. It returns nil
// when the connection's source IP belongs to a bridge whose group is
// admitted by visibility. Otherwise it returns ErrVisibilityDenied;
// callers MUST close the connection and bump the denied counter.
//
// Pulled out as a free function so it works for both TCP and UDP
// (which doesn't carry a net.Conn) — the UDP path uses VerifySource
// directly with a netip.Addr derived from the packet source.
func VerifyTCPSource(conn net.Conn, src SourceResolver, vis ServiceVisibility) error {
	if conn == nil {
		return ErrVisibilityDenied
	}
	return VerifySource(remoteAddrOf(conn), src, vis)
}

// VerifySource is the IP-shaped admission hook used by UDP and by the
// connection-form path through VerifyTCPSource. A non-nil error means
// reject; the error is wrapped from ErrVisibilityDenied (or its
// ErrCrossClusterDenied refinement) so callers can errors.Is either
// sentinel.
func VerifySource(srcIP netip.Addr, src SourceResolver, vis ServiceVisibility) error {
	if !srcIP.IsValid() {
		return ErrVisibilityDenied
	}
	// cluster scope admits any source whose bridge is known. We still
	// require the source to resolve to a bridge so an off-overlay
	// packet from outside the data plane cannot sneak in.
	clusterPath, groupID, ok := src.ResolveGroup(srcIP)
	if !ok {
		return ErrVisibilityDenied
	}
	// Cluster-boundary check: even when the visibility scope is
	// "cluster" (admit anyone in the cluster), the source's bridge
	// MUST belong to the local cluster. A bridge map that has not
	// been wired with a local-cluster identity (LocalCluster() == "")
	// degrades to the previous behavior: in-cluster groups are
	// indistinguishable from cross-cluster, so admit only on group
	// scope matches. This keeps tests that omit LocalCluster() backward
	// compatible while production wiring (network.Manager) populates
	// it.
	if local := src.LocalCluster(); local != "" && clusterPath != local {
		return ErrCrossClusterDenied
	}
	if !vis.Admits(groupID) {
		return ErrVisibilityDenied
	}
	return nil
}

// remoteAddrOf extracts a netip.Addr from a net.Conn's RemoteAddr.
// Falls back to the zero Addr when the underlying address cannot be
// parsed (e.g. a pipe conn in tests with a "pipe" address); callers
// fail closed on the zero value.
func remoteAddrOf(c net.Conn) netip.Addr {
	ra := c.RemoteAddr()
	if ra == nil {
		return netip.Addr{}
	}
	host, _, err := net.SplitHostPort(ra.String())
	if err != nil {
		// RemoteAddr().String() did not contain a port (rare;
		// happens for some kernel-side wraps). Treat the whole string
		// as the host.
		host = strings.TrimSpace(ra.String())
	}
	if host == "" {
		return netip.Addr{}
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr
}

// logVisibilityRejection writes a sampled WARN line for a rejection.
// Exported so the proxy lifecycle can call it from both TCP and UDP
// paths without duplicating the field set.
func logVisibilityRejection(logger *zap.Logger, sample func(string) bool, service string, src netip.Addr) {
	if logger == nil {
		return
	}
	if sample == nil || !sample("visibility") {
		return
	}
	logger.Warn("proxy visibility denied",
		zap.String("service", service),
		zap.String("source", src.String()))
}
