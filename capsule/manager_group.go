package capsule

import (
	"context"
	"errors"
	"fmt"
	"time"

	enums "github.com/tareksalem/falak/capsule/enums"
	"go.uber.org/zap"
)

// CreateGroup admits a group capsule and materializes every member as a
// standalone capsule that flows through the standard Manager.Create path.
//
// The flow is:
//
//  1. Validate groupName as a DNS label (it will appear in DNS records and
//     gossip envelopes).
//  2. Apply DefaultSpec to each member's embedded CapsuleSpec so member
//     defaults match the standalone admission path.
//  3. Validate the GroupSpec via ValidateGroupSpec — this re-runs ValidateSpec
//     on every member's now-defaulted spec, rejects depends_on cycles, and
//     enforces unique DNS-friendly member names.
//  4. Persist a group-kind Capsule at status Created. The group capsule
//     itself does NOT pass through ValidateSpec (it has no workload fields,
//     no orbit, no image); group-level invariants are handled by
//     ValidateGroupSpec on the sub-spec and by setting Kind/Group correctly
//     here.
//  5. For each member, build a standalone CapsuleSpec inheriting baseLabels
//     merged with member-level labels (member labels win on collision), set
//     Kind=Capsule + GroupID + GroupMember=true, and call Manager.Create.
//     Any failure rolls back: every already-created member is deleted, then
//     the group capsule itself, before returning the joined error.
//  6. Once every member is materialized, populate groupSpec.MemberIDs in
//     spec order, persist the updated group spec via store.Update, and
//     auto-announce the group with Manager.Announce so the FSM lands at
//     Announced (matching standalone capsule behaviour).
//
// On success the returned slice contains the materialized member capsules
// in the same order as groupSpec.Members. The group capsule's
// Spec.Group.MemberIDs reflects that same order.
func (m *Manager) CreateGroup(
	ctx context.Context,
	clusterID string,
	groupName string,
	groupSpec GroupSpec,
	baseLabels Labels,
) (group *Capsule, members []*Capsule, err error) {
	if clusterID == "" {
		return nil, nil, fmt.Errorf("clusterID is required")
	}
	if !isDNSLabel(groupName) {
		return nil, nil, fmt.Errorf("group name %q must be a DNS-friendly label (lowercase letters, digits, hyphens; 1-63 chars; cannot start or end with hyphen)", groupName)
	}

	// Apply defaults to each member's spec BEFORE validation so the validator
	// sees the same shape Manager.Create would. We mutate a local copy and
	// hand it back into groupSpec.Members so subsequent steps see defaults.
	for i := range groupSpec.Members {
		spec := groupSpec.Members[i].Spec
		DefaultSpec(&spec)
		groupSpec.Members[i].Spec = spec
	}

	if vErr := ValidateGroupSpec(&groupSpec); vErr != nil {
		return nil, nil, fmt.Errorf("invalid group spec: %w", vErr)
	}

	// Build the group capsule. Group capsules carry no workload — Orbit and
	// Image are intentionally empty and ValidateSpec is NOT invoked on this
	// spec; ValidateGroupSpec already covers what matters for groups, and
	// ValidateCapsuleSpecGroupFields would reject a group spec carrying any
	// workload fields if a future refactor wants to invoke it here.
	groupID := NewCapsuleID()
	groupSpec.MemberIDs = nil // populated after members are created

	// Copy baseLabels so we don't share the user's map with the group capsule.
	groupLabels := LabelsMerge(baseLabels, nil)

	now := time.Now()
	groupCapsule := &Capsule{
		ID:        groupID,
		ClusterID: clusterID,
		Spec: CapsuleSpec{
			Name:   groupName,
			Tier:   enums.TierEnum.Standard(),
			Labels: groupLabels,
			Kind:   CapsuleKindEnum.Group(),
			Group:  &groupSpec,
			// Groups travel on a reserved system orbit so peers learn
			// the membership graph alongside individual members.
			Orbit: SystemGroupOrbit,
		},
		Status:    enums.CapsuleStatusEnum.Created(),
		Replicas:  nil,
		Momentum:  MomentumState{LastAdjusted: now},
		Version:   "1",
		CreatedAt: now,
		UpdatedAt: now,
	}

	if storeErr := m.store.Create(groupCapsule); storeErr != nil {
		return nil, nil, fmt.Errorf("failed to store group capsule: %w", storeErr)
	}

	// Install a fresh lifecycle for the group at Created so the auto-announce
	// at the end transitions Created -> Announced validly.
	m.lifecyclesMu.Lock()
	m.lifecycles[groupID] = m.newLifecycle(groupID, enums.CapsuleStatusEnum.Created())
	m.lifecyclesMu.Unlock()

	m.logger.Info("group capsule created",
		zap.String("group", groupID.String()),
		zap.String("name", groupName),
		zap.String("cluster", clusterID),
		zap.Int("members", len(groupSpec.Members)))

	m.metrics.IncCreated(clusterID)
	m.emit(EventCapsuleCreated, groupCapsule)

	// Materialize members. Each goes through Manager.Create which validates,
	// defaults, persists, installs a lifecycle, emits CapsuleCreated, and
	// auto-announces.
	created := make([]*Capsule, 0, len(groupSpec.Members))
	for i, member := range groupSpec.Members {
		memberSpec := member.Spec // already defaulted above
		memberSpec.Kind = CapsuleKindEnum.Capsule()
		memberSpec.GroupID = groupID
		memberSpec.GroupMember = true
		memberSpec.Group = nil

		// Inherit group-level labels; member labels win on key collision.
		memberSpec.Labels = LabelsMerge(groupLabels, memberSpec.Labels)

		mc, createErr := m.Create(ctx, clusterID, memberSpec)
		if createErr != nil {
			rollbackErr := m.rollbackGroupCreate(ctx, groupID, created)
			joined := fmt.Errorf("failed to create group member %d (%q): %w", i, member.Name, createErr)
			if rollbackErr != nil {
				joined = errors.Join(joined, fmt.Errorf("rollback: %w", rollbackErr))
			}
			return nil, nil, joined
		}
		created = append(created, mc)
	}

	// Populate MemberIDs in spec order and persist the updated group spec.
	memberIDs := make([]CapsuleID, len(created))
	for i, c := range created {
		memberIDs[i] = c.ID
	}
	groupSpec.MemberIDs = memberIDs
	groupCapsule.Spec.Group = &groupSpec
	if updErr := m.store.Update(groupCapsule); updErr != nil {
		rollbackErr := m.rollbackGroupCreate(ctx, groupID, created)
		joined := fmt.Errorf("failed to persist group member ids: %w", updErr)
		if rollbackErr != nil {
			joined = errors.Join(joined, fmt.Errorf("rollback: %w", rollbackErr))
		}
		return nil, nil, joined
	}

	// Auto-announce the group: Created -> Announced. Members already
	// auto-announced in Manager.Create.
	if annErr := m.Announce(groupID); annErr != nil {
		m.logger.Warn("auto-announce after group create failed",
			zap.String("group", groupID.String()),
			zap.Error(annErr))
	}

	return groupCapsule, created, nil
}

// rollbackGroupCreate undoes a partial CreateGroup. It deletes every
// already-materialized member capsule and the group capsule itself.
//
// Each Delete is best-effort: errors are logged but not returned, except for
// the final aggregated error which the caller joins with the original
// failure. Rollback never panics — the goal is to leave the store in a
// consistent shape, not to surface every cleanup hiccup.
func (m *Manager) rollbackGroupCreate(ctx context.Context, groupID CapsuleID, members []*Capsule) error {
	var errs []error
	for _, mc := range members {
		if delErr := m.Delete(ctx, mc.ID); delErr != nil {
			m.logger.Warn("rollback: failed to delete group member",
				zap.String("group", groupID.String()),
				zap.String("capsule", mc.ID.String()),
				zap.Error(delErr))
			errs = append(errs, fmt.Errorf("delete member %s: %w", mc.ID, delErr))
		}
	}
	if delErr := m.store.Delete(groupID); delErr != nil {
		m.logger.Warn("rollback: failed to delete group capsule",
			zap.String("group", groupID.String()),
			zap.Error(delErr))
		errs = append(errs, fmt.Errorf("delete group %s: %w", groupID, delErr))
	}
	m.lifecyclesMu.Lock()
	delete(m.lifecycles, groupID)
	m.lifecyclesMu.Unlock()
	return errors.Join(errs...)
}

// deleteGroup tears down a group capsule and reconciles its members.
//
// Behaviour depends on group.Spec.Group.CascadeDelete:
//
//   - true (default): every member ID listed in Spec.Group.MemberIDs is
//     deleted via the standard Manager.Delete path — store delete, lifecycle
//     drop, EventCapsuleDeleted. Downstream subscribers (orbit withdrawal,
//     election forget) react to that event independently. After all members
//     are processed the group capsule itself is removed the same way.
//
//   - false: each member is detached from the group rather than deleted.
//     Spec.GroupID is cleared, Spec.GroupMember flipped to false, and the
//     member is persisted via store.Update. EventCapsuleUpdated is then
//     emitted so the node handler (wired in 10.8) can re-announce the
//     now-standalone capsule on its orbit. Members keep running, replicas
//     intact. The group capsule is then removed via the standard delete
//     path.
//
// In both modes deleteGroup is reaper-idempotent: a member that has already
// been removed by another node winning the same withdrawal race is treated
// as success (matched via errors.Is(err, ErrNotFound)). Per-member errors
// other than not-found are collected and joined into the return value, but
// processing continues so a single bad member never blocks the rest of the
// group from being torn down.
//
// A nil group.Spec.Group is a programming error — a Kind=Group capsule with
// no GroupSpec cannot describe its members and we refuse to guess.
func (m *Manager) deleteGroup(ctx context.Context, group *Capsule) error {
	if group.Spec.Group == nil {
		return fmt.Errorf("group capsule %s has nil GroupSpec", group.ID)
	}

	cascade := group.Spec.Group.CascadeDelete
	memberIDs := group.Spec.Group.MemberIDs

	m.logger.Info("group capsule delete starting",
		zap.String("group", group.ID.String()),
		zap.String("name", group.Spec.Name),
		zap.String("cluster", group.ClusterID),
		zap.Bool("cascade", cascade),
		zap.Int("members", len(memberIDs)))

	var errs []error

	if cascade {
		// Cascade path: delete every member through the standard path.
		// Manager.Delete emits EventCapsuleDeleted for each, so orbit
		// withdrawal and election-forget subscribers react uniformly.
		for _, memberID := range memberIDs {
			if delErr := m.Delete(ctx, memberID); delErr != nil {
				if errors.Is(delErr, ErrNotFound) {
					// Reaper idempotence: another node already deleted
					// this member. Treat as success.
					m.logger.Debug("cascade: member already gone",
						zap.String("group", group.ID.String()),
						zap.String("capsule", memberID.String()))
					continue
				}
				m.logger.Warn("cascade: failed to delete group member",
					zap.String("group", group.ID.String()),
					zap.String("capsule", memberID.String()),
					zap.Error(delErr))
				errs = append(errs, fmt.Errorf("delete member %s: %w", memberID, delErr))
			}
		}
	} else {
		// Non-cascade path: detach each member so it survives as a
		// standalone capsule.
		//
		// Two events fire per detached member:
		//
		//   - EventCapsuleUpdated carries the now-standalone capsule. The
		//     node-level capsule handler's existing EventCapsuleUpdated
		//     branch re-announces the spec on its orbit so the rest of
		//     the mesh observes the cleared GroupID.
		//   - EventCapsuleGroupReleased is the explicit "released from
		//     group" signal. Its Meta[MetaPreviousGroupID] carries the
		//     group the member used to belong to (Spec.GroupID has been
		//     cleared by the time the event fires). Phase 11A bridge
		//     teardown subscribers and observability paths use this
		//     event to distinguish a release from a regular spec edit.
		for _, memberID := range memberIDs {
			member := m.store.Get(memberID)
			if member == nil {
				// Reaper idempotence: member already gone is success.
				m.logger.Debug("non-cascade: member already gone",
					zap.String("group", group.ID.String()),
					zap.String("capsule", memberID.String()))
				continue
			}

			previousGroupID := member.Spec.GroupID

			m.mu.Lock()
			member.Spec.GroupID = ""
			member.Spec.GroupMember = false
			member.UpdatedAt = time.Now()
			m.mu.Unlock()

			if updErr := m.store.Update(member); updErr != nil {
				m.logger.Warn("non-cascade: failed to detach member",
					zap.String("group", group.ID.String()),
					zap.String("capsule", memberID.String()),
					zap.Error(updErr))
				errs = append(errs, fmt.Errorf("detach member %s: %w", memberID, updErr))
				continue
			}

			m.logger.Info("group member detached",
				zap.String("group", group.ID.String()),
				zap.String("capsule", memberID.String()),
				zap.String("name", member.Spec.Name))

			m.emit(EventCapsuleUpdated, member)
			m.emitWithMeta(EventCapsuleGroupReleased, member, map[string]string{
				MetaPreviousGroupID: previousGroupID.String(),
			})
		}
	}

	// Remove the group capsule itself. Use the same primitives the standard
	// standalone delete path uses so subscribers see one EventCapsuleDeleted
	// for the group regardless of which branch ran above.
	if delErr := m.store.Delete(group.ID); delErr != nil {
		errs = append(errs, fmt.Errorf("delete group %s: %w", group.ID, delErr))
	} else {
		m.lifecyclesMu.Lock()
		delete(m.lifecycles, group.ID)
		m.lifecyclesMu.Unlock()

		m.logger.Info("group capsule deleted",
			zap.String("group", group.ID.String()),
			zap.String("name", group.Spec.Name),
			zap.String("cluster", group.ClusterID),
			zap.Bool("cascade", cascade))

		m.emit(EventCapsuleDeleted, group)
	}

	return errors.Join(errs...)
}

// GetGroup returns the group capsule and its materialized member capsules.
//
// The first return value is the group capsule (or nil if no capsule with id
// exists or the capsule is not Kind=Group). The second return value is the
// member slice from Store.ListByGroup(id), already sorted by Spec.Name
// ascending. When the group capsule is not found, members is nil.
func (m *Manager) GetGroup(id CapsuleID) (*Capsule, []*Capsule) {
	c := m.store.Get(id)
	if c == nil {
		return nil, nil
	}
	if c.Spec.Kind != CapsuleKindEnum.Group() {
		return nil, nil
	}
	return c, m.store.ListByGroup(id)
}
