package metrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/node/internal/signing"
	"github.com/tareksalem/falak/node/phonebook"
	healthpb "github.com/tareksalem/falak/node/proto/healthpb"
	"github.com/tareksalem/falak/shared"
)

// resourceUpdateMessageType is the HealthMessage.Type value used for the
// metrics gossip messages. The existing health pubsub uses other type
// strings ("score_update", "ping_request", "threshold_crossed"); this is
// a new one carried on the same envelope and topic.
const resourceUpdateMessageType = "resource_update"

// maxMessageAge is the upper bound on a received message's age before
// the receiver drops it as stale. Mirrors the value used by the existing
// health pubsub for consistency.
const maxMessageAge = 30 * time.Second

// clusterEntry holds the per-cluster pubsub state shared by the publish
// and subscribe paths. libp2p-pubsub only allows one Join per topic, so
// the topic handle must be shared between the two — we open it once,
// derive a subscription from it, and reuse it for publishes.
type clusterEntry struct {
	topic        *pubsub.Topic
	subscription *pubsub.Subscription
	cancel       context.CancelFunc
}

// Publisher signs and publishes ResourceUpdate messages onto the cluster's
// metrics pubsub topic. It owns the topic handle for each joined cluster,
// because libp2p-pubsub only permits one Join per topic per host. The
// matching Subscriber type borrows that handle via SubscriptionFor.
//
// The publisher is a thin wrapper: it builds the protobuf envelope, signs
// the canonical content with the local node's private key, and hands the
// bytes to libp2p-pubsub.
type Publisher struct {
	mu         sync.Mutex
	clusters   map[string]*clusterEntry
	ps         *pubsub.PubSub
	privateKey crypto.PrivKey
	nodeID     string
	logger     *zap.Logger
}

// PublisherOption configures a Publisher.
type PublisherOption func(*Publisher)

// WithPublisherLogger sets the logger.
func WithPublisherLogger(logger *zap.Logger) PublisherOption {
	return func(p *Publisher) {
		p.logger = logger
	}
}

// NewPublisher constructs a Publisher bound to the given pubsub instance,
// node identity, and private key. The publisher is per-node, not
// per-cluster — the same instance handles every cluster the node has
// joined, multiplexing on clusterPath.
func NewPublisher(ps *pubsub.PubSub, nodeID string, key crypto.PrivKey, opts ...PublisherOption) *Publisher {
	p := &Publisher{
		clusters:   make(map[string]*clusterEntry),
		ps:         ps,
		privateKey: key,
		nodeID:     nodeID,
		logger:     zap.NewNop(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// JoinCluster opens the metrics topic for a cluster and stores the
// topic + subscription pair so the matching Subscriber can borrow them.
// Idempotent: re-joining a cluster is a no-op. Returns an error when
// the topic or subscription cannot be created.
func (p *Publisher) JoinCluster(clusterPath string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.clusters[clusterPath]; ok {
		return nil
	}

	topicName := shared.BuildMetricsTopic(clusterPath)
	topic, err := p.ps.Join(topicName)
	if err != nil {
		return fmt.Errorf("metrics publisher: join %s: %w", topicName, err)
	}
	sub, err := topic.Subscribe()
	if err != nil {
		_ = topic.Close()
		return fmt.Errorf("metrics publisher: subscribe %s: %w", topicName, err)
	}
	p.clusters[clusterPath] = &clusterEntry{topic: topic, subscription: sub}
	p.logger.Debug("metrics publisher joined cluster",
		zap.String("cluster", clusterPath),
		zap.String("topic", topicName))
	return nil
}

// SubscriptionFor returns the subscription created when JoinCluster was
// called for this cluster. The matching Subscriber consumes from it.
// Returns nil when the cluster is not joined.
func (p *Publisher) SubscriptionFor(clusterPath string) *pubsub.Subscription {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.clusters[clusterPath]; ok {
		return e.subscription
	}
	return nil
}

// LeaveCluster cancels the subscription, closes the topic, and drops the
// cluster entry. Subsequent Publish calls for that cluster fail with
// an error.
func (p *Publisher) LeaveCluster(clusterPath string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.clusters[clusterPath]; ok {
		e.subscription.Cancel()
		_ = e.topic.Close()
		delete(p.clusters, clusterPath)
	}
}

// Publish sends a Snapshot to the cluster's metrics topic as a signed
// ResourceUpdate inside a HealthMessage envelope. Returns an error when
// the cluster is not joined or the publish call itself fails.
func (p *Publisher) Publish(ctx context.Context, clusterPath string, snap Snapshot) error {
	p.mu.Lock()
	entry, ok := p.clusters[clusterPath]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("metrics publisher: cluster %s not joined", clusterPath)
	}
	topic := entry.topic

	update := snapshotToProto(snap)
	payload, err := proto.Marshal(update)
	if err != nil {
		return fmt.Errorf("marshal resource update: %w", err)
	}

	msg := &healthpb.HealthMessage{
		Type:      resourceUpdateMessageType,
		Payload:   payload,
		SenderId:  p.nodeID,
		Timestamp: timestamppb.Now(),
	}

	sig, err := p.signEnvelope(msg)
	if err != nil {
		return fmt.Errorf("sign envelope: %w", err)
	}
	msg.Signature = sig

	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	if err := topic.Publish(ctx, data); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

// signEnvelope produces a signature over the canonical bytes of the
// HealthMessage envelope (type + payload + sender_id + timestamp). It
// uses the same length-prefixed encoding the rest of Falak uses, so
// receivers can verify with the existing helpers.
func (p *Publisher) signEnvelope(msg *healthpb.HealthMessage) ([]byte, error) {
	buf := signing.NewBuffer(len(msg.Payload) + 128)
	buf.WriteString(msg.Type)
	buf.WriteField(msg.Payload)
	buf.WriteString(msg.SenderId)
	if msg.Timestamp != nil {
		tsBytes, _ := msg.Timestamp.AsTime().MarshalBinary()
		buf.WriteTimeBinary(tsBytes)
	} else {
		buf.WriteField(nil)
	}
	return p.privateKey.Sign(buf.Bytes())
}

// Subscriber consumes ResourceUpdate messages from the metrics topic and
// writes the parsed snapshots into the metrics Store via the supplied
// callback. The Manager wires the callback to Store.UpsertPeer.
//
// The subscriber does NOT call ps.Join itself — libp2p-pubsub permits
// only one Join per topic per host, and the Publisher already joined.
// Instead the subscriber borrows the subscription from the Publisher
// via Publisher.SubscriptionFor.
//
// The subscriber owns one goroutine per joined cluster. Each goroutine
// loops on Subscription.Next until the context is cancelled.
type Subscriber struct {
	mu        sync.Mutex
	cancels   map[string]context.CancelFunc
	publisher *Publisher
	phonebook phonebook.IPhonebook
	logger    *zap.Logger
	onPeer    func(Snapshot)
	wg        sync.WaitGroup
	nodeID    string
}

// SubscriberOption configures a Subscriber.
type SubscriberOption func(*Subscriber)

// WithSubscriberLogger sets the logger.
func WithSubscriberLogger(logger *zap.Logger) SubscriberOption {
	return func(s *Subscriber) {
		s.logger = logger
	}
}

// NewSubscriber constructs a Subscriber that borrows subscriptions from
// the supplied Publisher. The onPeer callback is invoked once per
// accepted ResourceUpdate; it must be safe for concurrent use because
// multiple cluster goroutines run in parallel.
func NewSubscriber(
	publisher *Publisher,
	pb phonebook.IPhonebook,
	nodeID string,
	onPeer func(Snapshot),
	opts ...SubscriberOption,
) *Subscriber {
	s := &Subscriber{
		cancels:   make(map[string]context.CancelFunc),
		publisher: publisher,
		phonebook: pb,
		nodeID:    nodeID,
		logger:    zap.NewNop(),
		onPeer:    onPeer,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// JoinCluster starts the message loop for a cluster, borrowing the
// subscription from the publisher. Returns an error when the publisher
// has not yet joined the cluster (caller must JoinCluster the publisher
// first).
func (s *Subscriber) JoinCluster(parent context.Context, clusterPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.cancels[clusterPath]; ok {
		return nil
	}

	sub := s.publisher.SubscriptionFor(clusterPath)
	if sub == nil {
		return fmt.Errorf("metrics subscriber: publisher has not joined cluster %s", clusterPath)
	}

	ctx, cancel := context.WithCancel(parent)
	s.cancels[clusterPath] = cancel

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(ctx, clusterPath, sub)
	}()

	s.logger.Info("metrics subscriber joined cluster",
		zap.String("cluster", clusterPath))
	return nil
}

// LeaveCluster cancels the subscriber's message loop for a cluster.
// The subscription itself is owned by the publisher and will be cancelled
// by Publisher.LeaveCluster.
func (s *Subscriber) LeaveCluster(clusterPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.cancels[clusterPath]; ok {
		cancel()
		delete(s.cancels, clusterPath)
	}
}

// Stop cancels every cluster's message loop and waits for the goroutines
// to exit. Idempotent.
func (s *Subscriber) Stop() {
	s.mu.Lock()
	for path, cancel := range s.cancels {
		cancel()
		delete(s.cancels, path)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// loop is the per-cluster message loop. It blocks on sub.Next, parses
// each message, dispatches resource_update messages to onPeer, and
// silently drops everything else.
func (s *Subscriber) loop(ctx context.Context, clusterPath string, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("metrics subscriber recv error",
				zap.String("cluster", clusterPath),
				zap.Error(err))
			continue
		}

		// Skip our own messages — we don't want to write our own metrics
		// into peer_latest. The libp2p ReceivedFrom is the gossip relay,
		// not the original sender, so we double-check the envelope.
		if msg.ReceivedFrom.String() == s.nodeID {
			continue
		}

		s.handle(clusterPath, msg.Data)
	}
}

// handle parses one received message and, if it is a valid resource
// update for a peer with a verifiable signature, invokes the onPeer
// callback. All errors are logged at debug level — the loop continues
// regardless.
func (s *Subscriber) handle(clusterPath string, data []byte) {
	var envelope healthpb.HealthMessage
	if err := proto.Unmarshal(data, &envelope); err != nil {
		s.logger.Debug("invalid health envelope", zap.Error(err))
		return
	}
	if envelope.Type != resourceUpdateMessageType {
		// Not for us — leave it to the SWIM monitor.
		return
	}
	if envelope.SenderId == s.nodeID {
		// Our own message echoed back; skip.
		return
	}
	if envelope.Timestamp != nil {
		if age := time.Since(envelope.Timestamp.AsTime()); age > maxMessageAge {
			s.logger.Debug("dropping stale resource update",
				zap.String("sender", envelope.SenderId),
				zap.Duration("age", age))
			return
		}
	}

	if !s.verifySignature(clusterPath, &envelope) {
		s.logger.Debug("rejecting resource update with invalid signature",
			zap.String("sender", envelope.SenderId))
		return
	}

	var update healthpb.ResourceUpdate
	if err := proto.Unmarshal(envelope.Payload, &update); err != nil {
		s.logger.Debug("invalid resource update payload",
			zap.String("sender", envelope.SenderId),
			zap.Error(err))
		return
	}

	snap := protoToSnapshot(&update)
	if snap.NodeID == "" {
		// Defensive: trust the envelope sender if the inner message
		// did not populate NodeID.
		snap.NodeID = envelope.SenderId
	}
	s.onPeer(snap)
}

// verifySignature checks the envelope signature against the sender's
// public key from the phonebook. Reuses the same length-prefixed
// canonical encoding as the publisher.
func (s *Subscriber) verifySignature(clusterPath string, msg *healthpb.HealthMessage) bool {
	if len(msg.Signature) == 0 {
		return false
	}
	entry, err := s.phonebook.Get(msg.SenderId, clusterPath)
	if err != nil || entry == nil {
		return false
	}
	pubKey, err := crypto.UnmarshalPublicKey(entry.PublicKey)
	if err != nil {
		return false
	}

	buf := signing.NewBuffer(len(msg.Payload) + 128)
	buf.WriteString(msg.Type)
	buf.WriteField(msg.Payload)
	buf.WriteString(msg.SenderId)
	if msg.Timestamp != nil {
		tsBytes, _ := msg.Timestamp.AsTime().MarshalBinary()
		buf.WriteTimeBinary(tsBytes)
	} else {
		buf.WriteField(nil)
	}

	ok, err := pubKey.Verify(buf.Bytes(), msg.Signature)
	return err == nil && ok
}

// snapshotToProto converts a Snapshot into the wire form. Inverse of
// protoToSnapshot below.
func snapshotToProto(snap Snapshot) *healthpb.ResourceUpdate {
	return &healthpb.ResourceUpdate{
		NodeId:             snap.NodeID,
		CapturedAt:         timestamppb.New(snap.CapturedAt),
		CpuCores:           snap.CPU.Cores,
		CpuUsedPercent:     snap.CPU.UsedPercent,
		MemoryTotalMb:      snap.Memory.TotalMB,
		MemoryAvailableMb:  snap.Memory.AvailableMB,
		MemoryUsedMb:       snap.Memory.UsedMB,
		MemoryUsedPercent:  snap.Memory.UsedPercent,
		DiskTotalMb:        snap.Disk.TotalMB,
		DiskFreeMb:         snap.Disk.FreeMB,
		DiskUsedMb:         snap.Disk.UsedMB,
		DiskUsedPercent:    snap.Disk.UsedPercent,
		LoadOne:            snap.Load.One,
		LoadFive:           snap.Load.Five,
		LoadFifteen:        snap.Load.Fifteen,
		NetSentBytesPerSec: snap.Network.BytesSentPerSec,
		NetRecvBytesPerSec: snap.Network.BytesRecvPerSec,
	}
}

// protoToSnapshot converts a wire ResourceUpdate into a Snapshot.
func protoToSnapshot(u *healthpb.ResourceUpdate) Snapshot {
	snap := Snapshot{
		NodeID: u.NodeId,
		CPU: CPUStats{
			Cores:       u.CpuCores,
			UsedPercent: u.CpuUsedPercent,
		},
		Memory: MemoryStats{
			TotalMB:     u.MemoryTotalMb,
			AvailableMB: u.MemoryAvailableMb,
			UsedMB:      u.MemoryUsedMb,
			UsedPercent: u.MemoryUsedPercent,
		},
		Disk: DiskStats{
			TotalMB:     u.DiskTotalMb,
			FreeMB:      u.DiskFreeMb,
			UsedMB:      u.DiskUsedMb,
			UsedPercent: u.DiskUsedPercent,
		},
		Load: LoadStats{
			One:     u.LoadOne,
			Five:    u.LoadFive,
			Fifteen: u.LoadFifteen,
		},
		Network: NetworkStats{
			BytesSentPerSec: u.NetSentBytesPerSec,
			BytesRecvPerSec: u.NetRecvBytesPerSec,
		},
	}
	if u.CapturedAt != nil {
		snap.CapturedAt = u.CapturedAt.AsTime()
	}
	return snap
}
