// Package election provides decentralized leader-selection for capsule replicas.
//
// The election system answers the question "which node should run this
// capsule replica?" The single built-in Strategy lives in election/delay:
// every node scores only itself against the capsule spec, converts that
// score into a publish delay, and publishes a Claim. The first claim
// observed by the cluster wins, with a deterministic (score, timestamp,
// node ID) tiebreak resolving collisions.
//
// Division of responsibilities
//
// The Manager owns every bit of distributed coordination: it opens the
// per-cluster election PubSub topic, publishes Claim messages, consumes
// competing claims, runs the tiebreak, guards against a single node
// winning multiple replicas of the same capsule, and translates the
// final verdict into both a capsule lifecycle transition and an event
// on the node event bus.
//
// A Strategy is a pure function with no distributed state. It answers
// a single question per election round: "given this (capsule, local
// node), am I eligible to run it, what's my gravity score, and when
// should my claim be published?" Everything else — wire protocol,
// tiebreaks, timeouts, lifecycle bookkeeping — is the Manager's job.
// Strategies are trivial to unit-test: call Decide and assert on the
// Decision.
package election

import (
	"context"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/election/gravity"
)

// Reason explains why an election was started. It is forwarded into the
// strategy for logging and observability — never used to alter scoring.
type Reason string

// Private backing constants for the Reason enum. External callers access
// them through the ReasonEnum accessor below.
const (
	reasonInitial     Reason = "initial"
	reasonScaleUp     Reason = "scale_up"
	reasonNodeFailure Reason = "node_failure"
	reasonManual      Reason = "manual"
	reasonRebalance   Reason = "rebalance"
)

// reasonEnum is the unexported carrier struct used to expose the valid
// Reason values as methods on a single package-level accessor.
type reasonEnum struct{}

// ReasonEnum is the public accessor for Reason values. Use
// election.ReasonEnum.Initial() instead of a bare constant.
var ReasonEnum reasonEnum

// Initial returns the "initial" reason — a brand new capsule needs its
// first replicas placed.
func (reasonEnum) Initial() Reason { return reasonInitial }

// ScaleUp returns the "scale_up" reason — an existing capsule needs an
// additional replica.
func (reasonEnum) ScaleUp() Reason { return reasonScaleUp }

// NodeFailure returns the "node_failure" reason — a previously-running
// replica was orphaned by a node failure.
func (reasonEnum) NodeFailure() Reason { return reasonNodeFailure }

// Manual returns the "manual" reason — an operator triggered the
// election explicitly.
func (reasonEnum) Manual() Reason { return reasonManual }

// Rebalance returns the "rebalance" reason — a periodic optimization
// pass triggered re-election.
func (reasonEnum) Rebalance() Reason { return reasonRebalance }

// Request is everything a Strategy needs to run an election. It is the
// strategy-facing form of the ElectionRequested event — the Manager
// constructs it from the event before dispatching.
type Request struct {
	// CapsuleID identifies the capsule whose replica needs placement.
	CapsuleID capsule.CapsuleID

	// ReplicaID identifies which replica slot is being filled. Capsules
	// with multiple replicas issue one Request per slot.
	ReplicaID string

	// ClusterPath is the cluster the election runs in.
	ClusterPath string

	// Reason is the underlying cause of the election (for observability).
	Reason Reason

	// PreviousNodeID is the node that previously ran this replica, if any.
	// Populated for ReasonEnum.NodeFailure(). The failed node is naturally
	// ineligible via the phonebook status check — there is no need to
	// pass a list of excluded nodes through the protocol.
	PreviousNodeID string

	// Priority is a hint for the Manager when ordering pending requests.
	// Higher values are processed first. Failures fire as high priority.
	Priority int

	// CreatedAt is when the request was issued.
	CreatedAt time.Time
}

// Decision is a Strategy's verdict on a single Request. It tells the
// Manager three things:
//
//  1. Whether the local node is eligible at all.
//  2. What gravity score to attach to a published claim (and use for
//     tiebreak).
//  3. When, if ever, the claim should be published.
//
// The Manager handles the rest: waiting for PublishAt, publishing on
// the election topic, resolving collisions with other claims, and
// reporting the final Outcome.
type Decision struct {
	// Eligible is true when the local node may run the capsule. When
	// false the Manager does not publish a claim; instead it waits for
	// a remote claim and reports OutcomeEnum.Lost() or OutcomeEnum.Failed().
	Eligible bool

	// Score is the 0–100 gravity score the Manager will embed in the
	// published claim. Meaningful only when Eligible is true.
	Score float64

	// PublishAt is the absolute time the claim should be published.
	// Set to time.Now() (or any time in the past) for immediate
	// publication. The delay strategy sets this to a future time
	// (time.Now() + computed delay) so better-fit nodes claim first.
	// The Manager waits until PublishAt, cancels if a better remote
	// claim arrives first.
	PublishAt time.Time

	// Reason is a human-readable explanation of the decision. For
	// ineligible decisions this is the failure reason. For eligible
	// decisions it can carry diagnostic info. Included in structured
	// logs and exposed via observability.
	Reason string
}

// Strategy is the algorithm a Manager uses to decide when the local
// node should claim a capsule replica. The single built-in
// implementation is "delay" (package election/delay). New strategies
// may be introduced later; until then every Manager is constructed
// with a Strategy instance passed to NewManager.
//
// A Strategy is pure: given the same inputs it always returns the same
// Decision. It performs no I/O, no PubSub publishes, and no state
// persistence — the Manager handles all of that.
//
// Strategies are expected to be cheap (microseconds, not milliseconds).
// They are called on the hot path of every ElectionRequested event.
type Strategy interface {
	// Name returns a short identifier for this strategy. Used only by
	// observability (structured log fields, metrics labels). Two
	// strategies in the same binary should use distinct names so logs
	// remain grep-able.
	Name() string

	// Decide computes the local node's verdict for an election round.
	//
	// The passed Calculator and StateProvider are the strategy's only
	// interface to the outside world — the strategy uses them to score
	// the local node against the capsule and never reaches beyond them.
	//
	// Decide must not block. Any "wait until" behavior is expressed via
	// Decision.PublishAt, which the Manager honors.
	Decide(
		ctx context.Context,
		req Request,
		c *capsule.Capsule,
		calc *gravity.Calculator,
		provider gravity.StateProvider,
	) Decision
}

// Outcome is the final verdict of an election round, reported by the
// Manager to its EventSink and via the capsule lifecycle. Every
// ElectionRequested resolves to exactly one Outcome.
type Outcome string

// Private backing constants for the Outcome enum. External callers
// access them through the OutcomeEnum accessor below.
const (
	outcomeWon    Outcome = "won"
	outcomeLost   Outcome = "lost"
	outcomeFailed Outcome = "failed"
)

// outcomeEnum is the unexported carrier struct used to expose the valid
// Outcome values as methods on a single package-level accessor.
type outcomeEnum struct{}

// OutcomeEnum is the public accessor for Outcome values. Use
// election.OutcomeEnum.Won() instead of a bare constant.
var OutcomeEnum outcomeEnum

// Won returns the "won" outcome — the local node was selected to run
// the replica.
func (outcomeEnum) Won() Outcome { return outcomeWon }

// Lost returns the "lost" outcome — another node was selected to run
// the replica.
func (outcomeEnum) Lost() Outcome { return outcomeLost }

// Failed returns the "failed" outcome — no node could be elected (no
// eligible candidates, timeout from the manager itself, or all claims
// rejected).
func (outcomeEnum) Failed() Outcome { return outcomeFailed }

// Result is the Manager's report of a completed election. Passed to
// the EventSink and used for structured logging.
type Result struct {
	Request       Request
	Outcome       Outcome
	WinnerNodeID  string
	Score         float64
	FailureReason string
	DecidedAt     time.Time
}
