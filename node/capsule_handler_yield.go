package node

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/enums"
	"github.com/tareksalem/falak/node/internal/events"
)

// O14c — post-hoc election yield handling on the capsule side.
//
// When the election manager's reconcile window observes a strictly-better
// rival AFTER this node reported Won, it emits ElectionYielded (single
// replica) or GroupClaimYielded (same-node group). The RuntimeBridge stops
// the briefly-run container(s) through the O2 ignore-set (HARD INVARIANT
// #1). This file owns the OTHER half: re-pointing the durable binding to
// the real winner and mirroring the winner as remote (HARD INVARIANT #2),
// so NodesRunningCapsule converges on the winner rather than merely losing
// the local binding. The state re-pointed here lives in the capsule
// manager, which is why this half is node-side (the group capacity
// reservation, by contrast, lives in the election manager and is
// re-pointed there).
//
// Yielding is TERMINAL: these subscribers never re-elect. The winner is
// already known (WinnerNodeID); this is a clean "un-win", identical in
// shape to handleElectionLost.

// handleElectionYielded subscribes to ElectionYielded events and, for the
// yielding node, mirrors the winner exactly as handleElectionLost does:
// UnassignReplica clears the local (now-stale) binding, AssignReplica
// records the winner, and SyncStatus(Assigned) moves the capsule FSM to
// the remote-winner view. B becomes a clean loser that briefly ran. The
// container teardown is driven independently by the RuntimeBridge.
func (h *CapsuleHandler) handleElectionYielded(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeElectionYielded)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			yielded, ok := event.(events.ElectionYielded)
			if !ok {
				continue
			}
			h.onElectionYielded(yielded)
		}
	}
}

// onElectionYielded re-points a single replica's binding to the winner and
// mirrors the winner as the remote holder. Idempotent and best-effort:
// each transition logs at Debug and continues so a partial failure never
// strands the yield.
func (h *CapsuleHandler) onElectionYielded(yielded events.ElectionYielded) {
	id := capsule.CapsuleID(yielded.CapsuleID)
	replica := capsule.ReplicaID(yielded.ReplicaID)

	h.logger.Info("election yielded; re-pointing binding to winner (O14c)",
		zap.String("capsule_id", yielded.CapsuleID),
		zap.String("replica_id", yielded.ReplicaID),
		zap.String("winner", yielded.WinnerNodeID))

	// Clear the stale local binding (this node briefly held the replica).
	// UnassignReplica no-ops an already-unbound replica.
	if err := h.manager.UnassignReplica(id, replica); err != nil {
		h.logger.Debug("election yield: clear stale binding failed",
			zap.String("capsule_id", yielded.CapsuleID),
			zap.String("replica_id", yielded.ReplicaID),
			zap.Error(err))
	}

	// HARD INVARIANT #2: record the REAL winner so NodesRunningCapsule
	// converges on the winner, not merely losing the local binding.
	if yielded.WinnerNodeID != "" {
		if err := h.manager.AssignReplica(id, replica, yielded.WinnerNodeID); err != nil {
			h.logger.Debug("election yield: record winner binding failed",
				zap.String("capsule_id", yielded.CapsuleID),
				zap.String("replica_id", yielded.ReplicaID),
				zap.String("winner", yielded.WinnerNodeID),
				zap.Error(err))
		}
	}

	// Mirror the winner as remote (identical to handleElectionLost). The
	// FSM ends in Assigned — a clean loser view.
	if err := h.manager.SyncStatus(id, enums.CapsuleStatusEnum.Assigned()); err != nil {
		h.logger.Debug("election yield: sync status to Assigned failed",
			zap.String("capsule_id", yielded.CapsuleID),
			zap.String("winner", yielded.WinnerNodeID),
			zap.Error(err))
	}
}

// handleGroupClaimYielded subscribes to GroupClaimYielded events and, for
// the yielding node, stops+downgrades+unbinds every member it started and
// mirrors the winner as remote. It reuses rollbackGroupContainers — the
// SAME stop/downgrade/unbind mechanism onMemberPlacementFailed uses — but
// deliberately NOT that path's retry-cap accounting or re-election: the
// winner is already known and the election manager has already re-pointed
// the group's capacity reservation to it inside the reconcile loop. This
// subscriber therefore fires no GroupReelectionRequested and touches no
// placementRetries.
func (h *CapsuleHandler) handleGroupClaimYielded(ctx context.Context) {
	ch := h.eventBus.Subscribe(events.TypeGroupClaimYielded)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			yielded, ok := event.(events.GroupClaimYielded)
			if !ok {
				continue
			}
			h.onGroupClaimYielded(yielded)
		}
	}
}

// onGroupClaimYielded rolls back every member the local node started for a
// yielded group and mirrors each as the remote winner. No re-election.
func (h *CapsuleHandler) onGroupClaimYielded(yielded events.GroupClaimYielded) {
	groupID := capsule.CapsuleID(yielded.GroupID)
	if groupID == "" {
		h.logger.Debug("group claim yielded event missing group_id; ignoring")
		return
	}

	group, siblings := h.manager.GetGroup(groupID)
	if group == nil {
		h.logger.Warn("group claim yielded: parent group not known locally",
			zap.String("group_id", yielded.GroupID),
			zap.String("winner", yielded.WinnerNodeID))
		return
	}

	h.mu.RLock()
	rollback := h.runtimeRollback
	h.mu.RUnlock()

	h.logger.Info("group election yielded; rolling back local members and mirroring winner (O14c)",
		zap.String("group_id", yielded.GroupID),
		zap.String("winner", yielded.WinnerNodeID),
		zap.Int("members", len(siblings)))

	// Stop + downgrade + unbind every member (the shared mechanism). This
	// clears the local containers and the stale local bindings.
	h.rollbackGroupContainers(yielded.GroupID, siblings, rollback)

	// HARD INVARIANT #2 (group flavour): mirror the winner as the remote
	// holder for every member so NodesRunningCapsule converges on the
	// winner. rollbackGroupContainers already downgraded the FSM to
	// Announced and cleared bindings; now record the winner and move each
	// member to the Assigned (remote-winner) view. Best-effort per member.
	if yielded.WinnerNodeID != "" {
		for _, sib := range siblings {
			if err := h.manager.AssignReplica(sib.ID, capsule.ReplicaID("0"), yielded.WinnerNodeID); err != nil {
				h.logger.Debug("group yield: record winner binding failed",
					zap.String("group_id", yielded.GroupID),
					zap.String("member", sib.ID.String()),
					zap.String("winner", yielded.WinnerNodeID),
					zap.Error(err))
			}
			if err := h.manager.SyncStatus(sib.ID, enums.CapsuleStatusEnum.Assigned()); err != nil {
				h.logger.Debug("group yield: sync member to Assigned failed",
					zap.String("group_id", yielded.GroupID),
					zap.String("member", sib.ID.String()),
					zap.Error(err))
			}
		}
	}
}

// rollbackGroupContainers stops and rolls back every sibling of a
// same-node group on the local node: it downgrades the FSM
// (Running → Stopped via StopCapsule; other post-Announced states →
// Announced via SyncStatus), stops the runtime container through the O2
// ignore-set (StopContainer — never a raw runtime Stop), and clears every
// replica binding (O5b). It is the shared mechanism behind both the
// MemberPlacementFailed rollback and the O14c group yield; the CALLER
// owns policy (retry accounting, re-election, winner mirroring) — this
// helper performs no re-election and touches no retry state.
//
// rollback may be nil (tests / runtime-disabled deployments): the FSM
// downgrade + binding clears still run; only the runtime container stop is
// skipped. Every step is idempotent and logs at Debug on error so the
// walk always completes.
func (h *CapsuleHandler) rollbackGroupContainers(groupID string, siblings []*capsule.Capsule, rollback runtimeGroupRollback) {
	for _, sib := range siblings {
		snap := h.manager.Get(sib.ID)
		if snap == nil {
			continue
		}
		switch snap.Status {
		case enums.CapsuleStatusEnum.Created(),
			enums.CapsuleStatusEnum.Assigned(),
			enums.CapsuleStatusEnum.Executing(),
			enums.CapsuleStatusEnum.Electing():
			if err := h.manager.SyncStatus(sib.ID, enums.CapsuleStatusEnum.Announced()); err != nil {
				h.logger.Debug("group rollback: sibling resync to Announced failed",
					zap.String("group_id", groupID),
					zap.String("sibling", sib.ID.String()),
					zap.String("status", string(snap.Status)),
					zap.Error(err))
			}
		case enums.CapsuleStatusEnum.Running():
			if err := h.manager.StopCapsule(sib.ID); err != nil {
				h.logger.Debug("group rollback: sibling stop failed",
					zap.String("group_id", groupID),
					zap.String("sibling", sib.ID.String()),
					zap.Error(err))
			}
		}

		// Drive the runtime stop regardless of FSM state — siblings in any
		// post-Assigned state may have an active container on this node,
		// and StopContainer is idempotent (it plants the ignore-set entry
		// BEFORE Stop/Remove so the teardown does not self-trigger a
		// re-election, and returns an error for unknown containers which we
		// drop at Debug level). The replica ID for same-node group members
		// is always "0" (matches StartGroup's fixed assignment).
		if rollback != nil {
			if err := rollback.StopContainer(sib.ID.String(), "0", time.Second); err != nil {
				h.logger.Debug("group rollback: sibling runtime stop failed (often expected: container not started)",
					zap.String("group_id", groupID),
					zap.String("sibling", sib.ID.String()),
					zap.Error(err))
			}
		}

		// O5b: clear every sibling replica binding. Each member's
		// per-replica election records a durable replica->node binding
		// (AssignReplica); left in place, member self-anti-affinity
		// (electionCapsuleLookup.NodesRunningCapsule) counts the still-bound
		// member. Iterate the snapshot's Replicas (Get copies under the
		// manager mutex) rather than hardcoding replica "0". Idempotent —
		// UnassignReplica no-ops an already-unbound replica.
		for _, r := range snap.Replicas {
			if err := h.manager.UnassignReplica(sib.ID, r.ReplicaID); err != nil {
				h.logger.Debug("group rollback: sibling replica unbind failed",
					zap.String("group_id", groupID),
					zap.String("sibling", sib.ID.String()),
					zap.String("replica", string(r.ReplicaID)),
					zap.Error(err))
			}
		}
	}
}
