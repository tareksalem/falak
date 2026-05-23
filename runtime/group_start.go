package runtime

import (
	"go.uber.org/zap"
)

// GroupClaimWon mirrors the node-side events.GroupClaimWon shape. Keeping
// the type local to the runtime package preserves the package's
// independence from node-internal events.
//
// The node-side RuntimeBridge translates the node-bus event into this
// value before invoking Handler.StartGroup.
type GroupClaimWon struct {
	// GroupID is the parent CapsuleGroup's capsule ID.
	GroupID string

	// ClusterPath is the cluster the group belongs to. Carried for log
	// context only; the runtime does not key state by cluster.
	ClusterPath string

	// MemberIDs is the topological-order list of member capsule IDs as
	// published by the election manager. The handler dispatches an
	// ElectionWon-style start for each entry in order; members whose
	// DependsOn list is not yet satisfied are parked by the existing
	// groupCoord and released as siblings reach Running.
	MemberIDs []string

	// NodeID is the local node ID that won the group election. Carried
	// through to the per-member ElectionWon synthesised below so the
	// resulting container logs identify which node executed the start.
	NodeID string

	// Score is the gravity score that won the group election. Carried
	// through to the per-member ElectionWon for observability.
	Score float64
}

// StartGroup fans out a won GroupClaim into per-member starts. The
// handler walks event.MemberIDs in the order the election manager
// published them (which mirrors the group spec's topological order)
// and dispatches each member through the standard HandleElectionWon
// pipeline.
//
// Topology enforcement is delegated to the existing groupCoord:
// members whose DependsOn list is not yet satisfied are parked, then
// released when OnDependencyRunning fires for the sibling they wait
// on. StartGroup itself does NOT block on dependency state — it kicks
// each member through the entry-point and lets the coordinator park
// or pass-through based on the live sibling view.
//
// StartGroup is safe to invoke when the handler has no GroupView
// wired: each member start falls through to startContainerNow as if
// it were a standalone capsule. In that mode the topological order is
// preserved by the order of HandleElectionWon calls but no parking
// occurs.
//
// The method does NOT mark the group's reservation cleared — that is
// the responsibility of the node-side election handler subscribing to
// EventCapsuleRunning and counting per-group Running members.
func (h *Handler) StartGroup(event GroupClaimWon) {
	if len(event.MemberIDs) == 0 {
		h.logger.Warn("runtime: StartGroup called with no members",
			zap.String("group_id", event.GroupID),
			zap.String("cluster", event.ClusterPath),
			zap.String("node_id", event.NodeID))
		return
	}

	h.logger.Info("runtime: starting group",
		zap.String("group_id", event.GroupID),
		zap.String("cluster", event.ClusterPath),
		zap.String("node_id", event.NodeID),
		zap.Float64("score", event.Score),
		zap.Int("members", len(event.MemberIDs)))

	// Mark each member as "dispatched via StartGroup" so that a
	// subsequent start-failure routes to MemberPlacementFailed
	// rollback rather than the per-replica MarkFailed path. The flag
	// is per-capsule and cleared automatically when the capsule
	// reaches Running (via OnDependencyRunning) so a future
	// re-election after Running is treated as a regular per-replica
	// failure.
	h.markGroupDispatched(event.GroupID, event.MemberIDs)

	// Walk members in the order the election manager published them.
	// The election manager packs MemberIDs in the group spec's
	// topological order (members come first, dependents come later),
	// so iterating linearly is sufficient — members whose DependsOn
	// list isn't satisfied are parked by the existing groupCoord and
	// released when their siblings reach Running via
	// OnDependencyRunning.
	for _, memberID := range event.MemberIDs {
		if memberID == "" {
			h.logger.Warn("runtime: skipping empty member ID in group",
				zap.String("group_id", event.GroupID))
			continue
		}
		// Replica ID for group members is fixed at "0" — same-node
		// groups place exactly one replica per member (the colocation
		// constraint precludes per-member multi-replica fan-out;
		// scale-up of a same-node group is a future feature handled
		// elsewhere).
		ev := ElectionWon{
			CapsuleID:   memberID,
			ReplicaID:   "0",
			ClusterPath: event.ClusterPath,
			Score:       event.Score,
			NodeID:      event.NodeID,
		}
		h.logger.Debug("runtime: dispatching group member start",
			zap.String("group_id", event.GroupID),
			zap.String("capsule_id", memberID))
		h.HandleElectionWon(ev)
	}
}
