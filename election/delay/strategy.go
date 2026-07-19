// Package delay implements the delay-based election Strategy.
//
// The algorithm: each eligible node calculates its own gravity score for
// the capsule, converts it into a wait time inversely proportional to the
// score (higher score = shorter wait), and then publishes a Claim. The
// first claim observed by the cluster wins, with a deterministic tiebreak
// based on (score, timestamp, node ID).
//
// The wait scheme makes the algorithm self-organizing: better-fit nodes
// naturally publish first, and if the best-fit node fails before publishing,
// the second-best naturally takes over after their delay expires. No
// explicit voting or quorum is needed.
package delay

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/election"
	"github.com/tareksalem/falak/election/gravity"
)

// Default tuning constants. All are overridable per Strategy via options.
const (
	defaultMaxWait = 500 * time.Millisecond
	// Jitter is added to every wait calculation to break score ties at
	// microsecond precision. Kept tiny so it does not noticeably skew the
	// gravity-derived ordering.
	defaultMaxJitter = 200 * time.Microsecond
	// Per-replica stagger: each subsequent replica slot waits an extra
	// stagger on top of the gravity-derived delay. This lets the best-fit
	// node claim the first slot, observe its own claim (short-circuit via
	// the Manager's local claim guard), and step aside for subsequent
	// slots — allowing the next-best node to win them. Default 150ms,
	// wide enough for a gossipsub round-trip in a small cluster.
	defaultReplicaStagger = 150 * time.Millisecond
	// defaultMaxAnchorAge bounds how old a capsule's CreatedAt may be and
	// still be used as the shared election anchor (see anchorToAnnouncement).
	// Beyond this the strategy falls back to time.Now() so a daemon
	// re-announcing an old capsule on restart does not anchor every node's
	// publish time into the distant past (which would collapse the
	// score-ordered delays into an immediate free-for-all).
	defaultMaxAnchorAge = 30 * time.Second
)

// Strategy implements election.Strategy with the delay-based algorithm.
//
// All distributed coordination — publishing the claim, listening for
// rivals, applying the tiebreak, reporting the outcome — lives in
// election.Manager. The strategy itself is pure: given the same inputs
// it always returns the same Decision.
type Strategy struct {
	maxWait        time.Duration
	maxJitter      time.Duration
	replicaStagger time.Duration
	// anchorToAnnouncement, when true, computes PublishAt relative to the
	// capsule's announcement time (CreatedAt) instead of each node's local
	// time at election start, for initial elections. Because every node
	// derives the same anchor from the gossiped announcement, the
	// score-ordered publish schedule is identical cluster-wide — so gravity
	// decides the winner rather than which node happened to start its round
	// first (the originator's head-start). Defaults to true.
	anchorToAnnouncement bool
	maxAnchorAge         time.Duration
	logger               *zap.Logger
}

// Option configures a Strategy.
type Option func(*Strategy)

// WithMaxWait sets the upper bound on the wait time. A node with the
// minimum gravity score (0) waits exactly maxWait; the best node (100)
// publishes immediately. Default: 500ms.
func WithMaxWait(d time.Duration) Option {
	return func(s *Strategy) { s.maxWait = d }
}

// WithMaxJitter sets the upper bound on the random jitter added to the
// computed wait time. Defaults to 200µs — large enough to deterministically
// break ties when two nodes compute identical scores, small enough to be
// invisible at the protocol level.
func WithMaxJitter(d time.Duration) Option {
	return func(s *Strategy) { s.maxJitter = d }
}

// WithLogger sets a structured logger for per-decision diagnostics.
// When unset the strategy logs to a no-op logger so it remains safe
// to use in tests and ad-hoc callers.
func WithLogger(logger *zap.Logger) Option {
	return func(s *Strategy) { s.logger = logger }
}

// WithReplicaStagger sets the per-replica-slot delay. Each replica slot
// N waits an extra stagger*N on top of the gravity-derived delay, so
// the best-fit node can claim slot 0, let its claim propagate, step
// aside via the Manager's local claim guard, and leave slots 1..N-1
// for the next-best nodes. Default 150ms.
func WithReplicaStagger(d time.Duration) Option {
	return func(s *Strategy) { s.replicaStagger = d }
}

// WithAnchorToAnnouncement toggles anchoring the publish schedule to the
// capsule's announcement time for initial elections. Default true. Set
// false to fall back to local-time anchoring (e.g. for clusters with poor
// clock synchronization where cross-node skew exceeds maxWait).
func WithAnchorToAnnouncement(on bool) Option {
	return func(s *Strategy) { s.anchorToAnnouncement = on }
}

// WithMaxAnchorAge sets the maximum age of a capsule's CreatedAt for it to
// be used as the election anchor. Older capsules fall back to time.Now().
// Default 30s.
func WithMaxAnchorAge(d time.Duration) Option {
	return func(s *Strategy) {
		if d > 0 {
			s.maxAnchorAge = d
		}
	}
}

// New constructs a Strategy with the given options. Defaults are tuned
// for a typical cluster (500ms max wait, 200µs jitter, 150ms per-replica
// stagger, announcement-anchored ordering); production deployments can
// override per cluster via configuration.
func New(opts ...Option) *Strategy {
	s := &Strategy{
		maxWait:              defaultMaxWait,
		maxJitter:            defaultMaxJitter,
		replicaStagger:       defaultReplicaStagger,
		anchorToAnnouncement: true,
		maxAnchorAge:         defaultMaxAnchorAge,
		logger:               zap.NewNop(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Name returns the strategy identifier used in configuration to select it.
func (s *Strategy) Name() string { return "delay" }

// Decide implements election.Strategy. It computes the local node's
// gravity for the capsule and returns a Decision telling the manager
// when (if ever) to publish a claim.
//
// Flow:
//  1. Read the local NodeState from the gravity provider.
//  2. Run the gravity calculator. If the local node is ineligible
//     (resources, hard placement, health, or excluded), return
//     Eligible=false so the manager just listens for a remote winner.
//  3. Convert the gravity score into a wait duration:
//        wait = maxWait * (100 - score) / 100   plus a small random jitter
//     A perfect 100 score → wait = jitter only (publish almost immediately).
//     A 50 score → wait = ~half maxWait.
//     A 0 score → wait = maxWait.
//  4. Return the absolute PublishAt time so the manager can sleep until then.
func (s *Strategy) Decide(
	ctx context.Context,
	req election.Request,
	c *capsule.Capsule,
	calc *gravity.Calculator,
	provider gravity.StateProvider,
) election.Decision {
	state, err := provider.LocalNode(req.ClusterPath)
	if err != nil {
		s.logger.Warn("delay strategy: local node lookup failed",
			zap.String("capsule_id", string(req.CapsuleID)),
			zap.String("replica_id", req.ReplicaID),
			zap.String("cluster", req.ClusterPath),
			zap.Error(err))
		return election.Decision{
			Eligible: false,
			Reason:   "delay strategy: local node lookup failed: " + err.Error(),
		}
	}

	result := calc.Calculate(c, state)
	if !result.Eligible {
		s.logger.Debug("delay strategy: local node ineligible",
			zap.String("capsule_id", string(req.CapsuleID)),
			zap.String("replica_id", req.ReplicaID),
			zap.String("cluster", req.ClusterPath),
			zap.String("ineligibility", string(result.Inelig.Reason)),
			zap.String("detail", result.Inelig.Detail))
		return election.Decision{
			Eligible: false,
			Reason:   "delay strategy: ineligible (" + string(result.Inelig.Reason) + ": " + result.Inelig.Detail + ")",
		}
	}

	score := float64(result.Score)
	baseWait := waitForScore(score, s.maxWait)
	jit := jitter(s.maxJitter)
	slotDelay := replicaSlotStagger(req.ReplicaID, s.replicaStagger)
	wait := baseWait + slotDelay + jit

	// Anchor the publish schedule. For initial elections we anchor to the
	// capsule's announcement time so every node — originator and receivers
	// alike — computes the SAME absolute PublishAt and the winner is
	// decided purely by gravity, not by which node started its round first.
	// Re-elections (node failure, scale-up) have no shared announcement
	// anchor across independently-firing triggers, so they use local time.
	anchor, anchored := s.electionAnchor(req, c)
	publishAt := anchor.Add(wait)

	s.logger.Info("delay strategy: eligible",
		zap.String("capsule_id", string(req.CapsuleID)),
		zap.String("replica_id", req.ReplicaID),
		zap.String("cluster", req.ClusterPath),
		zap.String("reason", string(req.Reason)),
		zap.Float64("gravity_score", score),
		zap.Duration("base_wait", baseWait),
		zap.Duration("slot_delay", slotDelay),
		zap.Duration("jitter", jit),
		zap.Duration("total_wait", wait),
		zap.Bool("announcement_anchored", anchored),
		zap.Time("publish_at", publishAt))

	s.logger.Debug("delay strategy: gravity breakdown",
		zap.String("capsule_id", string(req.CapsuleID)),
		zap.String("node_id", state.NodeID),
		zap.Any("factors", result.Factors))

	return election.Decision{
		Eligible: true,
		Score:    score,
		// PublishAt schedules WHEN to publish (anchored, clock-based).
		PublishAt: publishAt,
		// Offset is the clock-independent tiebreak key (O14b): the pure
		// priority delay (baseWait + slotDelay + jitter), smaller = higher
		// priority. The Manager carries this on the wire and orders claims
		// by it, so cross-node clock skew cannot reorder the tiebreak.
		Offset: wait,
		Reason: "delay strategy: eligible",
	}
}

// replicaSlotStagger parses the replica slot index from the replica ID
// and returns stagger*slot. If the replica ID is not a plain integer
// (e.g. a UUID chosen by an external API), the stagger is zero so the
// strategy falls back to pure gravity-based ordering.
//
// The stagger is what makes multi-replica elections converge on
// distinct nodes: the best-fit node's delay for slot 0 is smallest,
// so it publishes first and short-circuits slots 1..N-1 via the
// Manager's local claim guard, leaving them to the next-best nodes.
func replicaSlotStagger(replicaID string, stagger time.Duration) time.Duration {
	if stagger <= 0 {
		return 0
	}
	slot, err := strconv.Atoi(replicaID)
	if err != nil || slot < 0 {
		return 0
	}
	return time.Duration(slot) * stagger
}

// electionAnchor returns the reference time the publish schedule is
// computed from, plus whether the shared announcement anchor was used.
//
// For initial elections with anchoring enabled it uses the capsule's
// CreatedAt — a value every node derives identically from the gossiped
// announcement — so the score-ordered schedule is cluster-wide identical.
// It falls back to local time when anchoring is disabled, the reason is
// not Initial (re-elections have no shared anchor), the timestamp is
// unset, or the capsule is older than maxAnchorAge (stale re-announce on
// restart).
func (s *Strategy) electionAnchor(req election.Request, c *capsule.Capsule) (time.Time, bool) {
	now := time.Now()
	if !s.anchorToAnnouncement || req.Reason != election.ReasonEnum.Initial() {
		return now, false
	}
	if c == nil || c.CreatedAt.IsZero() {
		return now, false
	}
	if now.Sub(c.CreatedAt) > s.maxAnchorAge {
		return now, false
	}
	return c.CreatedAt, true
}

// waitForScore computes the base (un-jittered) wait time for a given
// gravity score. The score is clamped to [0, 100] in case the calculator
// produces an out-of-range value (it shouldn't, but defending against it
// keeps Decide total).
func waitForScore(score float64, maxWait time.Duration) time.Duration {
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	fraction := (100 - score) / 100
	return time.Duration(fraction * float64(maxWait))
}

// jitter returns a small uniform-random duration in [0, maxJitter). It is
// used to break score ties at microsecond precision so that two nodes
// with identical gravity rarely publish at exactly the same instant.
//
// We use crypto/rand instead of math/rand because the jitter is part of
// a security-relevant tiebreak: an attacker observing math/rand seeds
// could predict which node "wins" a tie.
func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to zero jitter on the (vanishingly rare) read failure;
		// the deterministic node-ID tiebreak still resolves collisions.
		return 0
	}
	n := binary.BigEndian.Uint64(b[:])
	return time.Duration(n % uint64(max))
}
