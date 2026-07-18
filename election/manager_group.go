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

	// Reserve the local group-claim slot before publishing. If another
	// round on this node beat us to it, park on the slot's release channel
	// (bounded by the round deadline) rather than jumping straight to the
	// remote verdict. On release we RE-RUN CalculateCombinedFit against the
	// current node state — which now includes any reservation the prior
	// round recorded on Won — and re-decide:
	//   - still eligible  → the prior round did not commit this node to a
	//     conflicting group (or committed THIS group, which the caller's
	//     re-election intends to re-place): acquire the slot and publish.
	//   - now ineligible  → a live reservation from a DIFFERENT concurrent
	//     group over-commits the node: step aside to the remote verdict.
	//
	// This is the group twin of the single-replica park-wake-redecide loop
	// in runElection (manager.go), minus the per-replica fan-out: a group
	// is claimed as a unit, so there is no replica dimension to re-decide.
	// It is what makes releasing the slot on Won safe — a same-node
	// re-election can re-acquire and re-place the group instead of timing
	// out with "no claim heard" (O5).
	//
	// NOTE: the "different concurrent group over-commits the node" branch
	// relies on CalculateCombinedFit observing the committed capacity of an
	// existing reservation. That requires the StateProvider to subtract the
	// node's held pendingReservations from free capacity. The production
	// provider (node/metrics/provider.go) does not yet do this, so today the
	// over-commit refusal on this branch is only exercised by tests wired
	// with a reservation-aware provider. Two concurrent rounds for the SAME
	// group cannot reach this loop in production — HandleGroupClaimRequest
	// dedupes by groupInflight under m.mu before any round spawns — so the
	// same-group self-dedup is guaranteed upstream; this loop's re-decide is
	// the different-group anti-over-commit dimension only.
	for !m.tryLocalGroupClaim(req.GroupID) {
		m.logger.Debug("local group-claim slot taken while waiting; awaiting sibling outcome",
			zap.String("group", string(req.GroupID)))
		if !m.waitForGroupClaimReleased(ctx, req.GroupID, deadline) {
			m.waitForRemoteGroupVerdict(ctx, req, deadline, claimsCh)
			return
		}
		// Slot released — re-decide against the latest node state. Refresh
		// the state snapshot so the re-run observes the current reservation
		// picture rather than the stale snapshot from round start.
		state, err = m.provider.LocalNode(req.ClusterPath)
		if err != nil {
			m.reportGroup(req, GroupClaimOutcomeEnum.Failed(), "", 0, "local node state lookup failed: "+err.Error())
			return
		}
		fit = calc.CalculateCombinedFit(members, state)
		if !fit.Result.Eligible {
			m.logger.Debug("group no longer eligible after sibling round resolved; awaiting remote verdict",
				zap.String("group", string(req.GroupID)),
				zap.String("ineligible_member", fit.IneligibleMember))
			m.waitForRemoteGroupVerdict(ctx, req, deadline, claimsCh)
			return
		}
		// Refresh the score so the claim we publish reflects the re-run.
		score = float64(fit.Result.Score)
	}

	publishedAt := time.Now()
	// STABILITY INVARIANT (O14): publish the INTENDED publishAt (computed
	// above from delayFromScore), NOT the wall-clock publishedAt. The
	// group tiebreak keys on (score, intended-publishAt, nodeID); peers
	// compare our claim's TimestampMicros against their intended publishAt,
	// and isBetterGroup below compares rivals against our publishAt
	// (intended). Publishing the actual wall-clock time made the group path
	// asymmetric exactly like the single-replica path (O14) — pre-publish
	// used intended (publishAt) while post-publish used actual, so the same
	// 3-cycle split-brain was reachable. publishAt is stable across the CAS
	// re-decide loop above (only `score` is refreshed there, never
	// publishAt), so the published value equals the value compared below.
	claim := &electionpb.GroupClaim{
		GroupId:         string(req.GroupID),
		ClusterPath:     req.ClusterPath,
		NodeId:          m.nodeID,
		GravityScore:    score,
		TimestampMicros: publishAt.UnixMicro(),
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
			// Release the local group slot only AFTER the reservation is
			// durably recorded. Order is load-bearing (O5): a same-node
			// re-election parked in waitForGroupClaimReleased can only
			// re-decide once the slot frees, and by then the reservation is
			// visible so its CalculateCombinedFit re-run reflects the
			// committed capacity. Releasing here (instead of retaining the
			// slot across a Won round) is what unblocks a group re-election
			// on the same node after a member crash. reportGroup's Won arm
			// does NOT release — that would double-release.
			m.releaseLocalGroupClaim(req.GroupID)
			m.reportGroup(req, GroupClaimOutcomeEnum.Won(), m.nodeID, score, "")
			m.reconcileGroupAfterWin(ctx, req, score, publishAt, claimsCh)
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
			// O14: compare against the INTENDED publishAt (matching what we
			// published as TimestampMicros), NOT the wall-clock publishedAt.
			// Using publishedAt here was the group-path asymmetry the
			// original candidate fix missed — pre-publish (:162) already used
			// publishAt, so the two surfaces disagreed and the 3-cycle
			// split-brain stayed reachable on the group path.
			if isBetterGroup(rival, score, publishAt, m.nodeID) {
				m.releaseLocalGroupClaim(req.GroupID)
				m.recordReservation(req, rival.NodeId, rival.GravityScore)
				m.reportGroup(req, GroupClaimOutcomeEnum.Lost(), rival.NodeId, rival.GravityScore, "")
				return
			}
		case <-time.After(remaining):
			m.recordReservation(req, m.nodeID, score)
			// Release AFTER the reservation is recorded (see the sibling
			// Won arm above for the O5 ordering rationale).
			m.releaseLocalGroupClaim(req.GroupID)
			m.reportGroup(req, GroupClaimOutcomeEnum.Won(), m.nodeID, score, "")
			m.reconcileGroupAfterWin(ctx, req, score, publishAt, claimsCh)
			return
		}
	}
}

// reconcileGroupAfterWin is the O14c post-hoc yield window for the group
// path — the group twin of reconcileAfterWin. It runs AFTER reportGroup(Won)
// (the members have been dispatched to StartGroup) and keeps draining rival
// group claims for reconcileWindow. If a strictly-better rival arrives
// (its gossip was delayed past the tiebreak window), the local node yields:
//
//   - It re-points the group's capacity reservation to the winner via a
//     SINGLE recordReservation(req, rival...) call. recordReservation
//     cancels the prior (self) watchdog and re-arms a fresh one for the
//     winner atomically (no clear-then-record window, no spurious
//     GroupClaimFailed), and this is the IDENTICAL call the tiebreak-loop
//     Lost arm already makes — reservation bookkeeping stays inside the
//     Manager, where pendingReservations lives.
//   - It emits GroupClaimYielded so the node bridge stops every member it
//     started (through the O2 ignore-set) and mirrors the winner as remote.
//     It does NOT fire a re-election and does NOT consume a placement-retry
//     slot — the winner is already known.
//
// isBetterGroup is reused VERBATIM against the SAME published (score,
// publishAt): under the O14 strict total order, exactly one of two rival
// group claims yields. Yielding is TERMINAL (one-shot); the function
// returns and the round never re-elects off the yield.
func (m *Manager) reconcileGroupAfterWin(
	ctx context.Context,
	req GroupClaimRequest,
	score float64,
	publishAt time.Time,
	claimsCh <-chan *electionpb.GroupClaim,
) {
	// Drop the group in-flight dedup slot so a fresh group re-election
	// (member crash, node failure) is not deduped while this goroutine
	// drains late rivals. The goroutine's deferred removeGroupInflight is
	// idempotent, so the double removal is safe. Mirrors the single-replica
	// dropInflightForReconcile.
	m.removeGroupInflight(req.GroupID)
	if m.reconcileWindow <= 0 {
		return
	}
	deadline := time.Now().Add(m.reconcileWindow)
	m.logger.Debug("group reconcile-after-win window opened",
		zap.String("group", string(req.GroupID)),
		zap.Duration("window", m.reconcileWindow))
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			m.logger.Debug("group reconcile-after-win window closed; durable winner",
				zap.String("group", string(req.GroupID)))
			return
		}
		select {
		case <-ctx.Done():
			return
		case rival, ok := <-claimsCh:
			if !ok {
				return
			}
			if isBetterGroup(rival, score, publishAt, m.nodeID) {
				// Strictly-worse node: un-win. Re-point the reservation to
				// the winner (single atomic re-record) BEFORE emitting the
				// yield so the reservation reflects the winner the instant
				// the node bridge stops the local members.
				m.recordReservation(req, rival.NodeId, rival.GravityScore)
				m.logger.Info("group election yielded to strictly-better rival after win (O14c)",
					zap.String("group", string(req.GroupID)),
					zap.String("winner", rival.NodeId),
					zap.Float64("rival_score", rival.GravityScore),
					zap.Float64("our_score", score))
				m.groupSink.EmitGroupYielded(req, rival.NodeId)
				return
			}
		case <-time.After(remaining):
			m.logger.Debug("group reconcile-after-win window closed; durable winner",
				zap.String("group", string(req.GroupID)))
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
// invoked; they should wait via waitForGroupClaimReleased before
// retrying or stepping aside.
//
// Mirrors tryClaimCapsule: it also installs a fresh release channel for
// this generation so a round parked in waitForGroupClaimReleased wakes
// when the slot frees. The channel is (re)installed only when absent,
// matching the single-replica generation semantics.
func (m *Manager) tryLocalGroupClaim(id capsule.CapsuleID) bool {
	m.localGroupClaimsMu.Lock()
	defer m.localGroupClaimsMu.Unlock()
	if m.localGroupClaims[id] {
		return false
	}
	m.localGroupClaims[id] = true
	if _, ok := m.groupClaimReleased[id]; !ok {
		m.groupClaimReleased[id] = make(chan struct{})
	}
	return true
}

// releaseLocalGroupClaim clears the local group-claim flag so a future
// round (e.g. after a remote loss + retry on a different node, or a
// same-node re-election after this round won) can publish.
//
// Mirrors releaseCapsuleClaim: closing the per-group release channel
// wakes every goroutine parked in waitForGroupClaimReleased, then the
// channel is dropped so the next tryLocalGroupClaim allocates a fresh
// one for the next generation of waiters. Idempotent: a group with no
// installed channel (never claimed, or already released) is a no-op, so
// this never closes a closed channel.
func (m *Manager) releaseLocalGroupClaim(id capsule.CapsuleID) {
	m.localGroupClaimsMu.Lock()
	defer m.localGroupClaimsMu.Unlock()
	delete(m.localGroupClaims, id)
	if ch, ok := m.groupClaimReleased[id]; ok {
		close(ch)
		delete(m.groupClaimReleased, id)
	}
}

// waitForGroupClaimReleased blocks until the local group-claim slot for
// group id is released, ctx is cancelled, or the deadline passes.
// Returns true when the slot was released (caller may re-decide and
// retry); false on cancellation or timeout (caller should step aside to
// waitForRemoteGroupVerdict).
//
// Mirrors waitForCapsuleClaimReleased: the wait is broadcast — every
// goroutine parked for the same group wakes when the slot frees and each
// retries tryLocalGroupClaim independently; only one succeeds and the
// rest park again on the next generation channel.
func (m *Manager) waitForGroupClaimReleased(ctx context.Context, id capsule.CapsuleID, deadline time.Time) bool {
	m.localGroupClaimsMu.Lock()
	if !m.localGroupClaims[id] {
		// Already released between the caller's tryClaim and this wait.
		m.localGroupClaimsMu.Unlock()
		return true
	}
	ch, ok := m.groupClaimReleased[id]
	if !ok {
		// Defensive: the holder didn't install a channel. Treat as
		// released so the caller retries immediately.
		m.localGroupClaimsMu.Unlock()
		return true
	}
	m.localGroupClaimsMu.Unlock()

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	case <-time.After(remaining):
		return false
	}
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
