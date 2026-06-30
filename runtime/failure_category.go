package runtime

import "strings"

// FailureCategory classifies why a capsule start/run failed, set at the
// failure SITE (where the real error is in hand) rather than parsed from
// reason strings downstream. The node-local execution-reliability tracker
// uses the category to decide whether a failure reflects on the NODE
// (count it) or on the CAPSULE itself (ignore it — it would fail on every
// node, so penalizing this node is wrong).
//
// Enum pattern: private consts + private carrier struct + public var
// accessor (FailureCategoryEnum).
type FailureCategory string

const (
	// failureNodeAttributable: the node/runtime is at fault — runc/cgroup/
	// namespace create errors, start errors, checkpoint/restore failures,
	// snapshot-pull failures, spec-lookup failures, and disk/IO/no-space/
	// permission errors (including "image pull failed: no space left on
	// device", which surfaces at the pull site but is a node condition).
	// These COUNT against the node's execution reliability.
	failureNodeAttributable FailureCategory = "node_attributable"

	// failureCapsuleGlobal: the capsule itself is at fault — image manifest
	// not found / 404, invalid spec, bad entrypoint/command. These fail on
	// EVERY node, so they are EXCLUDED from a node's execution reliability.
	failureCapsuleGlobal FailureCategory = "capsule_global"

	// failureAmbiguous: cannot attribute cleanly — pull network/registry-down,
	// generic create/start failures. COUNTED as failures (the safer default;
	// time-decay forgives transients).
	failureAmbiguous FailureCategory = "ambiguous"
)

type failureCategoryEnum struct{}

// FailureCategoryEnum is the public accessor for FailureCategory values.
var FailureCategoryEnum failureCategoryEnum

// NodeAttributable returns the node-at-fault category (counts against the
// node's execution reliability).
func (failureCategoryEnum) NodeAttributable() FailureCategory { return failureNodeAttributable }

// CapsuleGlobal returns the capsule-at-fault category (excluded from a
// node's execution reliability — it fails everywhere).
func (failureCategoryEnum) CapsuleGlobal() FailureCategory { return failureCapsuleGlobal }

// Ambiguous returns the unattributable category (counted as a failure;
// decay forgives transients).
func (failureCategoryEnum) Ambiguous() FailureCategory { return failureAmbiguous }

// String returns the category's wire value.
func (c FailureCategory) String() string { return string(c) }

// classifyPullError sub-classifies an image-pull error. Disk-full /
// permission errors are NodeAttributable (the node cannot store the image),
// manifest-unknown / not-found / 401 / 403 are CapsuleGlobal (the image ref
// is bad regardless of node), and everything else (network, registry down,
// timeout) is Ambiguous.
//
// Matching is done on the lower-cased error string because the underlying
// runtime (Podman REST) returns opaque strings, not typed errors. This is
// the one place string inspection is acceptable: it happens AT the failure
// site, not downstream of an already-stringified reason.
func classifyPullError(err error) FailureCategory {
	if err == nil {
		return failureAmbiguous
	}
	s := strings.ToLower(err.Error())
	switch {
	case containsAny(s, "no space", "no space left", "disk full", "enospc", "permission denied", "not permitted"):
		return failureNodeAttributable
	case containsAny(s, "manifest unknown", "manifest not found", "not found", "404", "401", "403", "unauthorized", "forbidden"):
		return failureCapsuleGlobal
	default:
		return failureAmbiguous
	}
}

// containsAny reports whether s contains any of the given substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
