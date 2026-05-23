package election

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
	electionpb "github.com/tareksalem/falak/election/proto/electionpb"
)

// HandleGroupClaimRequest dispatches a group election request. Multiple
// requests for the same group_id collapse onto one round. The protocol
// mirrors HandleRequest but with three differences:
//
//   - eligibility is computed by CalculateCombinedFit (every member must
//     individually fit, then the combined score is run against the
//     synthetic merged capsule).
//   - the wire message is GroupClaim, not Claim.
//   - on Won (locally or remotely) the manager records a capacity
//     reservation so a concurrent group election cannot over-commit the
//     same node. The reservation expires automatically on a deadline,
//     emitting GroupClaimFailed when it does.
//
// Returns an error when required dependencies are unset or when any
// member capsule cannot be looked up in the local store; the caller
// (event subscriber on the node bridge) re-tries on the next gossip
// round.
func (m *Manager) HandleGroupClaimRequest(req GroupClaimRequest) error {
	if m.ctx == nil {
		return errors.New("election manager: not started")
	}
	if m.store == nil || m.calculator == nil || m.provider == nil {
		return errors.New("election manager: missing required dependency (store, calculator, or provider)")
	}
	if req.GroupID == "" {
		return errors.New("election manager: group request missing GroupID")
	}
	if len(req.MemberIDs) == 0 {
		return errors.New("election manager: group request missing MemberIDs")
	}

	// Resolve members from the local store. Any missing entry means we
	// have not received the announcement yet — return an error so the
	// caller can retry on the next gossip round.
	members := make([]*capsule.Capsule, 0, len(req.MemberIDs))
	for _, id := range req.MemberIDs {
		c := m.store.Get(id)
		if c == nil {
			return fmt.Errorf("election manager: group member %s not yet known locally", id)
		}
		members = append(members, c)
	}

	m.mu.Lock()
	if _, exists := m.groupInflight[req.GroupID]; exists {
		m.mu.Unlock()
		m.logger.Debug("group election already in flight, deduping",
			zap.String("group", string(req.GroupID)))
		return nil
	}
	topic, ok := m.topics[req.ClusterPath]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("election manager: cluster %s not joined", req.ClusterPath)
	}

	roundCtx, roundCancel := context.WithCancel(m.ctx)
	m.groupInflight[req.GroupID] = &groupInflightEntry{cancel: roundCancel}
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer roundCancel()
		defer m.removeGroupInflight(req.GroupID)
		m.runGroupElection(roundCtx, req, members, topic)
	}()
	return nil
}

// runGroupElection drives one group election round end-to-end. The shape
// mirrors runElection: subscribe to topic, ask the gravity calculator
// for a combined-fit verdict, wait until PublishAt (cancelled if a
// better remote claim arrives), publish, wait the tiebreak window,
// report the outcome. The reservation is recorded on Won (local or
// remote).
func (m *Manager) runGroupElection(
	ctx context.Context,
	req GroupClaimRequest,
	members []*capsule.Capsule,
	topic *ClusterTopic,
) {
	m.logger.Info("group election round started",
		zap.String("group", string(req.GroupID)),
		zap.String("cluster", req.ClusterPath),
		zap.Int("members", len(members)),
		zap.String("reason", string(req.Reason)))

	claimsCh, cancelListen := topic.ListenGroup(string(req.GroupID), 16)
	defer cancelListen()

	calc := m.calculatorFor(req.ClusterPath)
	state, err := m.provider.LocalNode(req.ClusterPath)
	if err != nil {
		m.reportGroup(req, GroupClaimOutcomeEnum.Failed(), "", 0, "local node state lookup failed: "+err.Error())
		return
	}

	// Exclude-list short-circuit: if this round's request told us not to
	// elect on the local node, treat that as ineligible immediately.
	for _, ex := range req.ExcludeNodes {
		if ex == m.nodeID {
			m.waitForRemoteGroupVerdict(ctx, req, time.Now().Add(m.timeoutFor(req.ClusterPath)), claimsCh)
			return
		}
	}

	fit := calc.CalculateCombinedFit(members, state)
	if !fit.Result.Eligible {
		reason := "group ineligible: member " + fit.IneligibleMember + " does not fit"
		m.logger.Debug("group election: local node ineligible",
			zap.String("group", string(req.GroupID)),
			zap.String("ineligible_member", fit.IneligibleMember))
		// Locally ineligible: emit Failed (no other path produces a
		// verdict for this round on this node) and exit. Remote nodes
		// that ARE eligible publish their own claims; we never see them
		// because we do not listen past this point — the goal here is
		// to surface the local diagnostic, not chain into a remote
		// outcome that the subscriber will pick up anyway through the
		// winner's publish.
		m.reportGroup(req, GroupClaimOutcomeEnum.Failed(), "", 0, reason)
		return
	}

	score := float64(fit.Result.Score)
	timeout := m.timeoutFor(req.ClusterPath)
	deadline := time.Now().Add(timeout)
	wait := delayFromScore(score, timeout)
	publishAt := time.Now().Add(wait)

	m.logger.Debug("group election: eligible",
		zap.String("group", string(req.GroupID)),
		zap.Float64("score", score),
		zap.Duration("wait", wait),
		zap.Time("publish_at", publishAt))

	// Wait until PublishAt, preempted by a better remote claim or deadline.
	if wait > 0 {
		select {
		case <-ctx.Done():
			m.reportGroup(req, GroupClaimOutcomeEnum.Failed(), "", 0, "context cancelled")
			return
		case rival, ok := <-claimsCh:
			// Closed channel (listener replaced or topic stopped) yields
			// !ok; treat as no rival and fall through to publish.
			// isBetterGroup also guards against a nil rival as
			// defense-in-depth.
			if ok && isBetterGroup(rival, score, publishAt, m.nodeID) {
				m.recordReservation(req, rival.NodeId, rival.GravityScore)
				m.reportGroup(req, GroupClaimOutcomeEnum.Lost(), rival.NodeId, rival.GravityScore, "")
				return
			}
		case <-time.After(wait):
		case <-time.After(time.Until(deadline)):
			m.reportGroup(req, GroupClaimOutcomeEnum.Failed(), "", 0, "group election timeout")
			return
		}
	}

	// Local group claim dedup: a second concurrent HandleGroupClaimRequest
	// for the same group on the same node must not publish twice.
	if !m.tryLocalGroupClaim(req.GroupID) {
		m.logger.Debug("local node already has a group claim in flight",
			zap.String("group", string(req.GroupID)))
		m.waitForRemoteGroupVerdict(ctx, req, deadline, claimsCh)
		return
	}

	publishedAt := time.Now()
	claim := &electionpb.GroupClaim{
		GroupId:         string(req.GroupID),
		ClusterPath:     req.ClusterPath,
		NodeId:          m.nodeID,
		GravityScore:    score,
		TimestampMicros: publishedAt.UnixMicro(),
	}
	for _, id := range req.MemberIDs {
		claim.MemberIds = append(claim.MemberIds, string(id))
	}
	pubCtx, pubCancel := context.WithTimeout(ctx, m.publishTimeout)
	if err := topic.PublishGroupClaim(pubCtx, claim); err != nil {
		pubCancel()
		m.releaseLocalGroupClaim(req.GroupID)
		m.logger.Warn("group claim publish failed",
			zap.String("group", string(req.GroupID)),
			zap.Error(err))
		m.reportGroup(req, GroupClaimOutcomeEnum.Failed(), "", 0, "publish failed: "+err.Error())
		return
	}
	pubCancel()

	tiebreakDeadline := publishedAt.Add(m.tiebreakWindow)
	for {
		remaining := time.Until(tiebreakDeadline)
		if remaining <= 0 {
			m.recordReservation(req, m.nodeID, score)
			m.reportGroup(req, GroupClaimOutcomeEnum.Won(), m.nodeID, score, "")
			return
		}
		select {
		case <-ctx.Done():
			m.releaseLocalGroupClaim(req.GroupID)
			m.reportGroup(req, GroupClaimOutcomeEnum.Failed(), "", 0, "context cancelled")
			return
		case rival, ok := <-claimsCh:
			if !ok {
				// Listener channel closed: topic stopping or the listener
				// was replaced. Disable this branch so the select does
				// not busy-loop on the closed receive, then let the
				// tiebreak deadline (or ctx) drive the outcome.
				claimsCh = nil
				continue
			}
			if isBetterGroup(rival, score, publishedAt, m.nodeID) {
				m.releaseLocalGroupClaim(req.GroupID)
				m.recordReservation(req, rival.NodeId, rival.GravityScore)
				m.reportGroup(req, GroupClaimOutcomeEnum.Lost(), rival.NodeId, rival.GravityScore, "")
				return
			}
		case <-time.After(remaining):
			m.recordReservation(req, m.nodeID, score)
			m.reportGroup(req, GroupClaimOutcomeEnum.Won(), m.nodeID, score, "")
			return
		}
	}
}

// waitForRemoteGroupVerdict mirrors waitForRemoteVerdict for group
// elections. It blocks until a GroupClaim arrives, the deadline passes,
// or the context is cancelled. Used by ineligible / locally-deduped
// rounds that still want to mirror the cluster's outcome.
func (m *Manager) waitForRemoteGroupVerdict(
	ctx context.Context,
	req GroupClaimRequest,
	deadline time.Time,
	claims <-chan *electionpb.GroupClaim,
) {
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			m.reportGroup(req, GroupClaimOutcomeEnum.Failed(), "", 0, "group election timeout (no claim heard)")
			return
		}
		select {
		case <-ctx.Done():
			m.reportGroup(req, GroupClaimOutcomeEnum.Failed(), "", 0, "context cancelled")
			return
		case claim, ok := <-claims:
			if !ok {
				m.reportGroup(req, GroupClaimOutcomeEnum.Failed(), "", 0, "claim channel closed")
				return
			}
			m.recordReservation(req, claim.NodeId, claim.GravityScore)
			m.reportGroup(req, GroupClaimOutcomeEnum.Lost(), claim.NodeId, claim.GravityScore, "")
			return
		case <-time.After(remaining):
			m.reportGroup(req, GroupClaimOutcomeEnum.Failed(), "", 0, "group election timeout (no claim heard)")
			return
		}
	}
}

// delayFromScore converts a gravity score in [0,100] to a wait duration
// in [0, maxWait]. Higher score = shorter wait — the best-fit node
// publishes first. Mirrors the delay-strategy formula.
func delayFromScore(score float64, maxWait time.Duration) time.Duration {
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	fraction := (100 - score) / 100
	return time.Duration(fraction * float64(maxWait))
}

// isBetterGroup applies the same deterministic tiebreak as isBetter but
// using the group-claim shape. Higher score wins, then earlier timestamp,
// then lexicographically smaller node ID.
//
// A nil rival is treated as "local is better" (returns false). This
// guards the receive path: when a group claim listener channel is
// closed by a replacement registration mid-flight (see
// ClusterTopic.ListenGroup), a concurrent reader observes a closed
// channel and yields the zero value (nil). Returning false here keeps
// the round on the local-wins branch rather than dereferencing a nil
// pointer.
func isBetterGroup(rival *electionpb.GroupClaim, ourScore float64, ourPublishAt time.Time, ourNodeID string) bool {
	if rival == nil {
		return false
	}
	if rival.GravityScore != ourScore {
		return rival.GravityScore > ourScore
	}
	rivalTime := rival.TimestampMicros
	ourTime := ourPublishAt.UnixMicro()
	if rivalTime != ourTime {
		return rivalTime < ourTime
	}
	return rival.NodeId < ourNodeID
}

// reportGroup finalizes a group election with a verdict, calling the
// configured GroupClaimSink and logging the outcome.
func (m *Manager) reportGroup(req GroupClaimRequest, outcome GroupClaimOutcome, winner string, score float64, reason string) {
	switch outcome {
	case GroupClaimOutcomeEnum.Won():
		m.groupSink.EmitGroupWon(req, winner, score)
		m.logger.Info("group election won",
			zap.String("group", string(req.GroupID)),
			zap.String("winner", winner),
			zap.Float64("score", score),
			zap.Int("members", len(req.MemberIDs)))

	case GroupClaimOutcomeEnum.Lost():
		m.groupSink.EmitGroupLost(req, winner)
		m.logger.Info("group election lost",
			zap.String("group", string(req.GroupID)),
			zap.String("winner", winner))

	case GroupClaimOutcomeEnum.Failed():
		m.releaseLocalGroupClaim(req.GroupID)
		m.groupSink.EmitGroupFailed(req, reason)
		m.logger.Warn("group election failed",
			zap.String("group", string(req.GroupID)),
			zap.String("reason", reason))
	}
}

// removeGroupInflight clears the in-flight entry for a group election
// once its round goroutine completes.
func (m *Manager) removeGroupInflight(id capsule.CapsuleID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.groupInflight, id)
}

// tryLocalGroupClaim atomically marks a group as locally claimed and
// returns true when the caller obtained the slot. Subsequent callers
// for the same group receive false until releaseLocalGroupClaim is
// invoked.
func (m *Manager) tryLocalGroupClaim(id capsule.CapsuleID) bool {
	m.localGroupClaimsMu.Lock()
	defer m.localGroupClaimsMu.Unlock()
	if m.localGroupClaims[id] {
		return false
	}
	m.localGroupClaims[id] = true
	return true
}

// releaseLocalGroupClaim clears the local group-claim flag so a future
// round (e.g. after a remote loss + retry on a different node) can
// publish.
func (m *Manager) releaseLocalGroupClaim(id capsule.CapsuleID) {
	m.localGroupClaimsMu.Lock()
	defer m.localGroupClaimsMu.Unlock()
	delete(m.localGroupClaims, id)
}

// recordReservation installs (or refreshes) a group capacity reservation
// keyed on group_id. The deadline is computed from the configured
// per-member image-pull timeout plus a constant slack. A watchdog
// goroutine fires GroupClaimFailed when the deadline expires without
// the reservation being cleared by the runtime.
//
// Heartbeat-driven extension of the reservation (via PullProgress
// events) is deferred to Phase 10.16.
func (m *Manager) recordReservation(req GroupClaimRequest, nodeID string, score float64) {
	m.pendingReservationsMu.Lock()
	if existing, ok := m.pendingReservations[req.GroupID]; ok {
		// Pre-existing reservation: cancel its watchdog before replacing
		// it so the goroutine exits promptly.
		if existing.cancel != nil {
			existing.cancel()
		}
	}

	deadline := time.Now().
		Add(time.Duration(len(req.MemberIDs)) * m.groupImagePullTimeout).
		Add(defaultGroupReservationSlack)

	watchCtx, watchCancel := context.WithCancel(m.ctx)
	res := &groupReservation{
		GroupID:     req.GroupID,
		MemberIDs:   append([]capsule.CapsuleID(nil), req.MemberIDs...),
		ClusterPath: req.ClusterPath,
		NodeID:      nodeID,
		Deadline:    deadline,
		cancel:      watchCancel,
	}
	m.pendingReservations[req.GroupID] = res
	m.pendingReservationsMu.Unlock()

	m.logger.Info("group reservation recorded",
		zap.String("group", string(req.GroupID)),
		zap.String("node", nodeID),
		zap.Int("members", len(req.MemberIDs)),
		zap.Time("deadline", deadline),
		zap.Float64("score", score))

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.watchReservation(watchCtx, req, deadline)
	}()
}

// watchReservation is the deadline watchdog for a single reservation. It
// fires a GroupClaimFailed verdict when the deadline expires without the
// reservation having been cleared elsewhere; it exits silently when its
// context is cancelled (reservation cleared or manager stopped).
func (m *Manager) watchReservation(ctx context.Context, req GroupClaimRequest, deadline time.Time) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		m.fireReservationTimeout(req)
		return
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		m.fireReservationTimeout(req)
	}
}

// fireReservationTimeout clears the reservation and emits a Failed event
// when the deadline expires before the runtime confirms every member is
// running. Idempotent: a concurrent clearReservation that already removed
// the entry causes this to no-op.
func (m *Manager) fireReservationTimeout(req GroupClaimRequest) {
	m.pendingReservationsMu.Lock()
	res, ok := m.pendingReservations[req.GroupID]
	if !ok {
		m.pendingReservationsMu.Unlock()
		return
	}
	delete(m.pendingReservations, req.GroupID)
	m.pendingReservationsMu.Unlock()

	if res.cancel != nil {
		res.cancel()
	}

	m.logger.Warn("group reservation deadline expired",
		zap.String("group", string(req.GroupID)),
		zap.String("node", res.NodeID),
		zap.Int("members", len(res.MemberIDs)))

	m.groupSink.EmitGroupFailed(req, "reservation timeout")
}

// ClearGroupReservation removes a same-node group's capacity
// reservation, cancelling the deadline watchdog. The node-side
// election handler calls this once every member of the group has
// reached Running so the reservation does not falsely expire and
// trigger a spurious GroupClaimFailed verdict.
//
// Idempotent: groups with no active reservation are silently ignored.
func (m *Manager) ClearGroupReservation(id capsule.CapsuleID) {
	m.clearReservation(id)
}

// clearReservation removes the reservation for a group, cancelling its
// watchdog. Called by ForgetCapsule (capsule deletion) and the future
// 10.15 runtime subscriber when every member reaches Running.
func (m *Manager) clearReservation(id capsule.CapsuleID) {
	m.pendingReservationsMu.Lock()
	res, ok := m.pendingReservations[id]
	if !ok {
		m.pendingReservationsMu.Unlock()
		return
	}
	delete(m.pendingReservations, id)
	m.pendingReservationsMu.Unlock()

	if res.cancel != nil {
		res.cancel()
	}
	m.logger.Debug("group reservation cleared",
		zap.String("group", string(id)),
		zap.String("node", res.NodeID))
}

// HasReservation reports whether the local manager currently holds a
// reservation for the given group. Used by the runtime adapter (Phase
// 10.15) to discover groups it should start; exposed here so tests can
// assert on the manager-side bookkeeping.
func (m *Manager) HasReservation(id capsule.CapsuleID) bool {
	m.pendingReservationsMu.Lock()
	defer m.pendingReservationsMu.Unlock()
	_, ok := m.pendingReservations[id]
	return ok
}

// ReservationDeadline returns the deadline for the given group's
// reservation, or the zero time when no reservation exists. Used by
// tests to validate the configured deadline computation.
func (m *Manager) ReservationDeadline(id capsule.CapsuleID) time.Time {
	m.pendingReservationsMu.Lock()
	defer m.pendingReservationsMu.Unlock()
	if r, ok := m.pendingReservations[id]; ok {
		return r.Deadline
	}
	return time.Time{}
}

// OnPullProgress extends the capacity-reservation deadline for the
// given group by one image-pull window. The election manager treats
// every PullProgress heartbeat as "alive, the runtime is making
// progress" — so genuinely slow image pulls do not falsely trip the
// reservation watchdog and force a rollback.
//
// Idempotent: groups with no active reservation are silently ignored.
// Safe to call many times per pull (the typical pattern is one call
// per heartbeat at runtime-configurable frequency).
//
// capsuleID is the member whose pull is in flight; it is logged for
// observability but does not influence the per-group deadline (every
// member of the group shares one reservation).
func (m *Manager) OnPullProgress(groupID capsule.CapsuleID, capsuleID string) {
	if m == nil || groupID == "" {
		return
	}

	m.pendingReservationsMu.Lock()
	res, ok := m.pendingReservations[groupID]
	if !ok {
		m.pendingReservationsMu.Unlock()
		return
	}

	// Replace the deadline watchdog: cancel the in-flight one and arm a
	// fresh one with the extended deadline. The existing pattern in
	// recordReservation drives the same lifecycle (cancel old, spawn
	// new watcher), so the heartbeat path mirrors it exactly.
	if res.cancel != nil {
		res.cancel()
	}
	extended := time.Now().Add(m.groupImagePullTimeout).Add(defaultGroupReservationSlack)
	if extended.Before(res.Deadline) {
		// Never shorten the deadline. A fast heartbeat after a slow
		// recorded deadline keeps the slower bound.
		extended = res.Deadline
	}
	watchCtx, watchCancel := context.WithCancel(m.ctx)
	res.Deadline = extended
	res.cancel = watchCancel
	m.pendingReservationsMu.Unlock()

	m.logger.Debug("group reservation deadline extended by pull heartbeat",
		zap.String("group", string(groupID)),
		zap.String("capsule_id", capsuleID),
		zap.String("node", res.NodeID),
		zap.Time("deadline", extended))

	req := GroupClaimRequest{
		GroupID:     res.GroupID,
		MemberIDs:   append([]capsule.CapsuleID(nil), res.MemberIDs...),
		ClusterPath: res.ClusterPath,
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.watchReservation(watchCtx, req, extended)
	}()
}

// ReservationNodeID returns the node ID that holds the active
// reservation for the given group, or the empty string when no
// reservation exists. Used by the capsule handler's node-failure
// path to detect orphaned same-node groups when the failed node was
// the reservation holder. Idempotent and safe to call across nodes
// (every node mirrors the same reservation state from the group-
// claim protocol).
func (m *Manager) ReservationNodeID(id capsule.CapsuleID) string {
	m.pendingReservationsMu.Lock()
	defer m.pendingReservationsMu.Unlock()
	if r, ok := m.pendingReservations[id]; ok {
		return r.NodeID
	}
	return ""
}
