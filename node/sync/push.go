package sync

import (
	"context"
	stdsync "sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
	"github.com/tareksalem/falak/node/proto/syncpb"
	"github.com/tareksalem/falak/shared"
)

// fanOutNewMember implements Layer 1 of the O13 join-convergence design: the
// voucher, having just admitted a new member, actively pushes that member to
// every existing Active peer over a dedicated push stream (reverse of the
// pull sync). This gives deterministic convergence — no peer has to wait for
// the best-effort Step-2 PubSub broadcast or the anti-entropy tail.
//
// It is best-effort per target: a failed push is logged at WARN and left for
// the Layer-2 burst backstop to heal. Concurrency is bounded by a semaphore
// so a mass join does not open an unbounded number of streams at once.
func (s *Syncer) fanOutNewMember(admit events.MemberAdmitted) {
	clusterPath := admit.ClusterPath
	newMember := admit.NewMember

	entries, err := s.phonebook.GetByCluster(clusterPath)
	if err != nil {
		s.logger.Warn("member push: failed to list cluster peers",
			zap.String("cluster", clusterPath),
			zap.String("node", newMember.NodeID),
			zap.Error(err))
		return
	}

	selfID := s.host.ID().String()

	// Targets = Active members, excluding self and the new member itself.
	// Only Active peers are pushed to (never PendingAuth/Suspected/
	// Quarantined/Failed/Departed) — pushing to a still-settling peer would
	// race its own admission, and pushing to a failing peer wastes a stream.
	var targets []*phonebook.Entry
	for _, e := range entries {
		if e.NodeID == selfID || e.NodeID == newMember.NodeID {
			continue
		}
		if e.Status != phonebook.NodeStatusEnum.Active() {
			continue
		}
		targets = append(targets, e)
	}

	pushMember := memberInfoToProto(newMember)

	// Bounded fan-out: semaphore caps concurrent streams.
	concurrency := s.memberPushConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)

	var delivered atomic.Int64
	var innerWg stdsync.WaitGroup

	for _, t := range targets {
		select {
		case <-s.ctx.Done():
			innerWg.Wait()
			return
		case sem <- struct{}{}:
		}

		innerWg.Add(1)
		go func(target *phonebook.Entry) {
			defer innerWg.Done()
			defer func() { <-sem }()

			if s.pushMemberTo(clusterPath, target, pushMember, newMember.NodeID) {
				delivered.Add(1)
			}
		}(t)
	}

	innerWg.Wait()

	s.eventBus.Publish(events.MemberPushFanoutDone{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: clusterPath,
		NewMemberID: newMember.NodeID,
		Targets:     len(targets),
		Delivered:   int(delivered.Load()),
	})

	s.logger.Info("member push fan-out complete",
		zap.String("cluster", clusterPath),
		zap.String("node", newMember.NodeID),
		zap.Int("targets", len(targets)),
		zap.Int("delivered", int(delivered.Load())))
}

// pushMemberTo opens a push stream to one target and delivers the new member.
// Returns true on a successful, accepted delivery. Best-effort: failures are
// logged at WARN and left to the Layer-2 backstop.
func (s *Syncer) pushMemberTo(clusterPath string, target *phonebook.Entry, member *syncpb.MemberInfo, newMemberID string) bool {
	peerID, err := peer.Decode(target.NodeID)
	if err != nil {
		s.logger.Debug("member push: invalid target peer ID",
			zap.String("nodeId", target.NodeID),
			zap.Error(err))
		return false
	}

	// Ensure we can dial the target.
	s.addPeerAddresses(peerID, target.Addresses)

	ctx, cancel := context.WithTimeout(s.ctx, s.memberPushTimeout)
	defer cancel()

	stream, err := s.host.NewStream(ctx, peerID, PushProtocolID)
	if err != nil {
		s.logger.Warn("member push: failed to open stream (backstop will heal)",
			zap.String("cluster", clusterPath),
			zap.String("peer", target.NodeID),
			zap.String("node", newMemberID),
			zap.Error(err))
		return false
	}
	defer stream.Close()

	if dl, ok := ctx.Deadline(); ok {
		if err := stream.SetDeadline(dl); err != nil {
			s.logger.Debug("member push: failed to set stream deadline",
				zap.String("peer", target.NodeID),
				zap.Error(err))
		}
	}

	push := &syncpb.SyncPush{
		ClusterPath: clusterPath,
		Member:      member,
		PushId:      uuid.New().String(),
	}

	if err := shared.WriteProto(stream, push); err != nil {
		s.logger.Warn("member push: failed to send push",
			zap.String("cluster", clusterPath),
			zap.String("peer", target.NodeID),
			zap.String("node", newMemberID),
			zap.Error(err))
		return false
	}

	var ack syncpb.SyncPushAck
	if err := shared.ReadProto(stream, &ack); err != nil {
		s.logger.Warn("member push: failed to read ack",
			zap.String("cluster", clusterPath),
			zap.String("peer", target.NodeID),
			zap.String("node", newMemberID),
			zap.Error(err))
		return false
	}

	if !ack.Accepted {
		s.logger.Warn("member push rejected by peer",
			zap.String("cluster", clusterPath),
			zap.String("peer", target.NodeID),
			zap.String("node", newMemberID),
			zap.String("error", ack.Error))
		return false
	}

	s.eventBus.Publish(events.MemberPushDelivered{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: clusterPath,
		NewMemberID: newMemberID,
		TargetPeer:  target.NodeID,
	})

	s.logger.Debug("member push delivered",
		zap.String("cluster", clusterPath),
		zap.String("peer", target.NodeID),
		zap.String("node", newMemberID))
	return true
}

// handlePushStream handles an incoming SyncPush (Layer 1 receiver). It applies
// the SAME receiver-auth gate as the pull path (phonebook.Exists for the
// sender+cluster) and inserts the pushed member via the idempotent
// processMember path — no separate dedup guard.
func (s *Syncer) handlePushStream(stream network.Stream) {
	defer stream.Close()

	remotePeer := stream.Conn().RemotePeer()

	// Rate-limit on the same limiter as pull sync so a misbehaving peer can't
	// flood us with pushes either.
	if !s.rateLimiter.Allow(remotePeer.String()) {
		s.logger.Warn("rate limited member push",
			zap.String("peer", remotePeer.String()))
		s.sendPushAck(stream, "", false, "rate limited")
		return
	}

	var push syncpb.SyncPush
	if err := shared.ReadProto(stream, &push); err != nil {
		s.logger.Error("failed to read member push", zap.Error(err))
		return
	}

	s.logger.Debug("received member push",
		zap.String("peer", remotePeer.String()),
		zap.String("cluster", push.ClusterPath),
		zap.String("pushId", push.PushId))

	// Receiver-auth gate: the sender must be an authenticated member of the
	// cluster it is pushing into. Identical gate to handleSyncStream — no
	// bypass for pushes.
	exists, err := s.phonebook.Exists(remotePeer.String(), push.ClusterPath)
	if err != nil {
		s.logger.Error("member push: failed to check peer in phonebook",
			zap.String("peer", remotePeer.String()),
			zap.Error(err))
		s.sendPushAck(stream, push.PushId, false, "internal error")
		return
	}
	if !exists {
		s.logger.Warn("rejected member push from unauthenticated peer",
			zap.String("peer", remotePeer.String()),
			zap.String("cluster", push.ClusterPath))
		s.sendPushAck(stream, push.PushId, false, "peer not authenticated for cluster")
		return
	}

	if push.Member == nil {
		s.sendPushAck(stream, push.PushId, false, "missing member")
		return
	}

	// Insert via the existing idempotent path (Exists→Update else Add).
	added, err := s.processMember(push.ClusterPath, push.Member)
	if err != nil {
		s.logger.Error("member push: failed to process member",
			zap.String("cluster", push.ClusterPath),
			zap.String("node", push.Member.NodeId),
			zap.Error(err))
		s.sendPushAck(stream, push.PushId, false, "failed to store member")
		return
	}

	s.sendPushAck(stream, push.PushId, true, "")

	s.logger.Info("member push accepted",
		zap.String("peer", remotePeer.String()),
		zap.String("cluster", push.ClusterPath),
		zap.String("node", push.Member.NodeId),
		zap.Bool("new", added))

	// Emit a sync-completed observable so operators/metrics see the member
	// count change, mirroring the pull path.
	if added {
		s.eventBus.Publish(events.SyncCompleted{
			BaseEvent:   events.NewBaseEvent(),
			ClusterPath: push.ClusterPath,
			MemberCount: 1,
			NewMembers:  1,
			SyncedFrom:  remotePeer.String(),
		})
	}
}

// sendPushAck writes a SyncPushAck to the stream. Best-effort — a failed ack
// write just means the pusher will retry via the backstop.
func (s *Syncer) sendPushAck(stream network.Stream, pushID string, accepted bool, errMsg string) {
	ack := &syncpb.SyncPushAck{
		PushId:   pushID,
		Accepted: accepted,
		Error:    errMsg,
	}
	if err := shared.WriteProto(stream, ack); err != nil {
		s.logger.Debug("failed to send push ack", zap.Error(err))
	}
}

// memberInfoToProto converts an events.MemberInfo (carried on MemberAdmitted)
// into the syncpb.MemberInfo wire type. A freshly-admitted member is Active
// and last-seen now.
func memberInfoToProto(m events.MemberInfo) *syncpb.MemberInfo {
	out := &syncpb.MemberInfo{
		NodeId:    m.NodeID,
		Addresses: m.Addresses,
		PublicKey: m.PublicKey,
		JoinedAt:  m.JoinedAt,
		LastSeen:  timestamppb.Now(),
		Status:    string(phonebook.NodeStatusEnum.Active()),
	}
	if m.Capabilities != nil {
		out.Capabilities = &syncpb.Capabilities{
			CpuCores:   m.Capabilities.CPUCores,
			MemoryMb:   m.Capabilities.MemoryMB,
			DiskGb:     m.Capabilities.DiskGB,
			Datacenter: m.Capabilities.Datacenter,
			Tags:       m.Capabilities.Tags,
			Metadata:   m.Capabilities.Metadata,
		}
	}
	return out
}
