package election

import (
	"time"

	"github.com/tareksalem/falak/capsule"
)

// GroupClaimRequest is the input to the group election protocol. It is the
// per-group analogue of Request: a single round of distributed election
// in which the unit elected is a same-node CapsuleGroup rather than a
// single capsule replica.
//
// Every field mirrors Request where the semantics overlap. The two
// group-specific additions are MemberIDs (every member capsule that must
// be co-placed on the winning node) and ExcludeNodes (failed nodes from
// previous rounds, used to drive re-election after a node failure).
type GroupClaimRequest struct {
	// GroupID identifies the parent group capsule whose members need
	// atomic placement. It matches the GroupClaim wire message's group_id.
	GroupID capsule.CapsuleID

	// MemberIDs lists every member capsule in topological dependency
	// order. The combined-fit calculator sums resources across every ID;
	// the winning node's runtime walks the same list to start members
	// in dependency order.
	MemberIDs []capsule.CapsuleID

	// ClusterPath is the cluster the election runs in. The election
	// topic is opened per cluster, so this also selects the topic.
	ClusterPath string

	// Reason is the originating cause of the request. Carried for
	// observability; never used to alter scoring.
	Reason Reason

	// ExcludeNodes lists node IDs the manager must not elect. Populated
	// after a rollback or node failure so the next round cannot retry
	// the same node that just failed.
	ExcludeNodes []string

	// Priority is a hint for ordering pending requests. Higher values
	// are processed first. Re-elections after a failure use a higher
	// priority than initial placements.
	Priority int

	// CreatedAt is when the request was issued.
	CreatedAt time.Time
}

// GroupClaimOutcome is the final verdict of a GroupClaim election round.
// Every GroupClaimRequest resolves to exactly one outcome — won, lost,
// or failed.
type GroupClaimOutcome string

// Private backing constants for the GroupClaimOutcome enum. External
// callers access them through the GroupClaimOutcomeEnum accessor.
const (
	groupClaimOutcomeWon    GroupClaimOutcome = "won"
	groupClaimOutcomeLost   GroupClaimOutcome = "lost"
	groupClaimOutcomeFailed GroupClaimOutcome = "failed"
)

// groupClaimOutcomeEnum is the unexported carrier struct used to expose
// the valid GroupClaimOutcome values as methods on a single package-level
// accessor.
type groupClaimOutcomeEnum struct{}

// GroupClaimOutcomeEnum is the public accessor for GroupClaimOutcome
// values. Use election.GroupClaimOutcomeEnum.Won() instead of a bare
// constant.
var GroupClaimOutcomeEnum groupClaimOutcomeEnum

// Won returns the "won" outcome — the local node was selected to host
// every member of the group.
func (groupClaimOutcomeEnum) Won() GroupClaimOutcome { return groupClaimOutcomeWon }

// Lost returns the "lost" outcome — another node was selected.
func (groupClaimOutcomeEnum) Lost() GroupClaimOutcome { return groupClaimOutcomeLost }

// Failed returns the "failed" outcome — no node could be elected.
func (groupClaimOutcomeEnum) Failed() GroupClaimOutcome { return groupClaimOutcomeFailed }

// GroupClaimResult is the Manager's report of a completed group election
// round. Passed to the GroupClaimSink and used for structured logging.
type GroupClaimResult struct {
	// GroupID identifies the group the election was for.
	GroupID capsule.CapsuleID

	// Outcome is the final verdict for the round.
	Outcome GroupClaimOutcome

	// WinnerNodeID is the node selected to host the group. Set on Won
	// and Lost outcomes; empty on Failed.
	WinnerNodeID string

	// Score is the combined-fit gravity score the winning node attached
	// to its claim. Meaningful on Won; mirrored on Lost when known.
	Score float64

	// FailureReason is the human-readable explanation for a Failed
	// outcome. Empty on Won and Lost.
	FailureReason string

	// DecidedAt is when the manager reached the verdict.
	DecidedAt time.Time
}

// GroupClaimSink receives lifecycle events emitted by the manager for
// group elections. The node bridges these onto the node event bus so
// downstream subscribers (runtime, observability) can react.
//
// The interface mirrors EventSink one-to-one for the per-capsule
// election outcome — Won/Lost/Failed.
type GroupClaimSink interface {
	// EmitGroupWon is called when the local node was selected to host
	// every member of the group. nodeID is the local node ID; score
	// is the combined-fit score the manager computed.
	EmitGroupWon(req GroupClaimRequest, nodeID string, score float64)

	// EmitGroupLost is called when another node won the election.
	// nodeID is the winning node's ID.
	EmitGroupLost(req GroupClaimRequest, nodeID string)

	// EmitGroupFailed is called when no node could be elected.
	// reason carries a human-readable explanation.
	EmitGroupFailed(req GroupClaimRequest, reason string)
}

// noopGroupSink is the default group sink — silently drops events. The
// Manager installs it unconditionally so HandleGroupClaimRequest never
// crashes when the node bridge has not wired a real sink yet.
type noopGroupSink struct{}

func (noopGroupSink) EmitGroupWon(GroupClaimRequest, string, float64) {}
func (noopGroupSink) EmitGroupLost(GroupClaimRequest, string)         {}
func (noopGroupSink) EmitGroupFailed(GroupClaimRequest, string)       {}
