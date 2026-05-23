package dns

import "net/netip"

// BridgeInfo identifies the per-group bridge the DNS query arrived on.
// It is the input side of every visibility decision: the responder
// derives the caller's identity from which listen socket received the
// query, then asks the predicate whether that caller can see the
// target.
type BridgeInfo struct {
	// ClusterPath identifies the cluster the bridge belongs to.
	ClusterPath string
	// GroupID is the capsule group's stable identifier.
	GroupID string
	// IP is the bridge gateway IP (the listen socket's local IP). Set
	// for diagnostics; the predicate is not required to consult it.
	IP netip.Addr
}

// VisibilityPredicate decides whether the caller bridge may resolve a
// record whose target lives in targetGroupID. It is a pure function:
// no I/O, no locks, no subscribing. State (group membership, Service
// visibility) is captured in the closure when the predicate is built.
type VisibilityPredicate func(callerBridge BridgeInfo, targetGroupID string) bool

// DefaultPredicate is Phase 11A's policy: bare capsule names are
// group-private — a caller can resolve a target only when the caller's
// bridge and the target share a GroupID. Phase 11B layers per-Service
// visibility on top of this (a Service with visibility:cluster can be
// resolved cross-group); 11A intentionally keeps the surface tight.
func DefaultPredicate(callerBridge BridgeInfo, targetGroupID string) bool {
	if callerBridge.GroupID == "" || targetGroupID == "" {
		return false
	}
	return callerBridge.GroupID == targetGroupID
}

// AllowAllPredicate accepts every query regardless of group. It is
// exported for two callers: tests that need the visibility check out
// of the way, and an opt-in "cluster scope" override the operator can
// flip on at the manager level. Production wiring should prefer
// DefaultPredicate.
func AllowAllPredicate(_ BridgeInfo, _ string) bool {
	return true
}

// ServiceVisibilityPredicate decides whether the given caller bridge
// may resolve a Service. The dns server consults ServiceResolver
// first; the resolver itself bakes the visibility check in (via
// ServiceVisibilityAdmits). This predicate is exported so the wiring
// layer (11B.21) can reuse the exact same check when composing a
// ServiceResolver from the service.Manager, and so the proxy can
// reach for the same policy when deciding whether to admit an
// incoming connection (see network/proxy/visibility.go).
//
// Returns true when:
//
//   - service.Visibility == "cluster" (every caller is admitted), or
//   - service.Visibility == "group" AND caller's GroupID equals
//     service.Group AND both are non-empty.
//
// Returns false otherwise (including for the deferred "external"
// scope and for empty / unknown values, which should never reach this
// helper but are rejected defensively).
func ServiceVisibilityPredicate(callerBridge BridgeInfo, service ServiceDNSInfo) bool {
	return ServiceVisibilityAdmits(callerBridge.GroupID, service)
}
