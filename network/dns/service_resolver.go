// This file defines the ServiceResolver contract that lets the DNS
// responder answer queries for Service names without depending on the
// service package. The real implementation lives in the wiring layer
// (11B.21); the dns package only ships an interface plus a sentinel
// no-op resolver so callers can construct a Server with no Service
// table wired and still get a sensible NXDOMAIN.
//
// The visibility check is intentionally pushed inside LookupService:
// the DNS responder hands the caller's bridge identity to the resolver
// and lets the resolver decide whether the name is visible. Keeping
// that decision in the resolver means dns/server.go stays oblivious to
// Service-side concepts (visibility scopes, group ownership, trivial
// vs proxy-routed). The DNS handler only learns "this name resolved
// to these IPs" or "this name is not visible to this caller".
package dns

import "net/netip"

// ServiceDNSInfo is the resolver's read-only view of a single Service
// for one DNS lookup. Returned by ServiceResolver.LookupService when
// the name matches a Service AND visibility admits the caller.
type ServiceDNSInfo struct {
	// ServiceID is the stable identifier — used by callers for log
	// correlation against the proxy stats registry.
	ServiceID string
	// Visibility records the Service's declared scope ("group" or
	// "cluster"). External is rejected at admission and never surfaces
	// here.
	Visibility string
	// Group is the Service's owning group (the group the Service is
	// attached to via its backends or explicit override).
	Group string
	// Trivial flags the single-backend, weight-100, static, cluster
	// Services that qualify for proxy-bypass per
	// service-networking decision #36. When Trivial is true callers
	// should resolve TrivialName via the existing capsule path; when
	// false they should answer with ProxyIP.
	Trivial bool
	// TrivialName is the bare capsule name to delegate the resolve to
	// when Trivial == true.
	TrivialName string
	// TrivialGroupID is the group the trivial backend lives in. May
	// differ from Group when the Service is cluster-scoped and the
	// backend lives elsewhere.
	TrivialGroupID string
	// ProxyIP is the local proxy listen IP for non-trivial Services.
	// The DNS handler echoes this as the A-record answer.
	ProxyIP netip.Addr
}

// ServiceResolver is the dns→service adapter the DNS responder calls
// for every A query before falling through to the capsule-name path.
// Implementations are expected to:
//
//   - Return (zero, false) when name is not a known Service. The
//     responder then tries the capsule path (Phase 11A behaviour).
//   - Return (zero, false) when name IS a known Service but visibility
//     forbids the caller. The responder treats both the not-found
//     case and the visibility-denied case identically (NXDOMAIN); we
//     do not leak existence to forbidden callers.
//   - Return (info, true) on a visibility-admitted match. The
//     responder consumes Trivial / ProxyIP to shape the response.
//
// Implementations MUST be safe for concurrent use: the DNS handler
// calls LookupService from every responder goroutine.
type ServiceResolver interface {
	// LookupService resolves name within the caller's scope. clusterPath
	// and callerGroup are derived from the listen-socket → bridge map.
	LookupService(clusterPath, callerGroup, name string) (ServiceDNSInfo, bool)
}

// NoopServiceResolver is a ServiceResolver that never matches. Wiring
// up a Server without WithServiceResolver yields this sentinel so the
// responder skips the Service path on every query without nil checks
// scattered across the handler.
type NoopServiceResolver struct{}

// LookupService always returns (zero, false) — the not-a-Service path.
func (NoopServiceResolver) LookupService(_, _, _ string) (ServiceDNSInfo, bool) {
	return ServiceDNSInfo{}, false
}

// ServiceVisibilityAdmits is a pure helper that mirrors the Service
// visibility check on the proxy side. The dns package and the proxy
// package both consume it so the policy stays in one place. It is
// exported so the wiring layer can reuse the check when building its
// concrete ServiceResolver implementation.
//
//   - visibility=="cluster": every caller is admitted.
//   - visibility=="group":   caller's group must equal info.Group.
//   - anything else:         denied (defensive — external is rejected
//     at spec-validation time and should not reach here).
func ServiceVisibilityAdmits(callerGroup string, info ServiceDNSInfo) bool {
	switch info.Visibility {
	case "cluster":
		return true
	case "group":
		if callerGroup == "" || info.Group == "" {
			return false
		}
		return callerGroup == info.Group
	default:
		return false
	}
}
