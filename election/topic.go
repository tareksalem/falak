package election

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	electionpb "github.com/tareksalem/falak/election/proto/electionpb"
)

// topicNameFn constructs the pubsub topic name for a cluster. Defined as
// a function variable so the tests can inject a deterministic prefix
// without depending on shared.
var topicNameFn = func(clusterPath string) string {
	return fmt.Sprintf("falak/%s/election", clusterPath)
}

// Signer signs the canonical bytes of an election message envelope with
// the local node's private key. It mirrors the Signer used by the orbit
// package so the node can implement both interfaces with the same
// backing key.
type Signer interface {
	Sign(content []byte) ([]byte, error)
}

// Verifier verifies that a signature over the canonical bytes of an
// election envelope matches the sender's public key.
type Verifier interface {
	Verify(senderID string, content, signature []byte) bool
}

// messageKey identifies a single in-flight election so incoming messages
// can be routed to the right listener. Pair of capsule ID and replica ID.
type messageKey struct {
	CapsuleID string
	ReplicaID string
}

// listener is a subscribe entry for an in-flight election. The Manager
// creates one when it starts an election round and removes it when the
// round ends. Messages dispatched to a closed listener are dropped.
type listener struct {
	claims   chan *electionpb.Claim
	failures chan *electionpb.ElectionFailed
	closed   bool
	mu       sync.Mutex
}

func newListener(buffer int) *listener {
	return &listener{
		claims:   make(chan *electionpb.Claim, buffer),
		failures: make(chan *electionpb.ElectionFailed, buffer),
	}
}

// dispatch delivers a claim or failure to the listener, never blocking.
// When the listener channel is full or closed the message is dropped.
// This is safe because the Manager only needs to see the first claim
// it cares about — subsequent duplicates are redundant.
func (l *listener) dispatchClaim(claim *electionpb.Claim) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	select {
	case l.claims <- claim:
	default:
	}
}

func (l *listener) dispatchFailure(f *electionpb.ElectionFailed) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	select {
	case l.failures <- f:
	default:
	}
}

func (l *listener) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	close(l.claims)
	close(l.failures)
}

// bufferedMessage is a message received for a key with no active
// listener. We keep recent ones around so that when a listener finally
// registers (because its local election round just started), the queued
// messages are delivered immediately. This closes the publish-before-
// listen race that otherwise lets concurrent elections think they won.
type bufferedMessage struct {
	receivedAt time.Time
	claim      *electionpb.Claim      // set for claim messages
	failed     *electionpb.ElectionFailed // set for failure messages
}

// bufferTTL is how long an unlistened-for message is kept before it's
// garbage-collected. Chosen to be long enough for a delayed local
// election to register its listener (typical delay: tens of milliseconds),
// but short enough that the buffer does not grow unbounded under churn.
const bufferTTL = 3 * time.Second

// bufferSweepInterval is how often the cleanup loop drops expired
// buffered messages.
const bufferSweepInterval = 1 * time.Second

// ClusterTopic owns one node's view of a cluster's election pubsub
// topic. It handles joining/leaving the topic, publishing signed Claim
// and ElectionFailed messages, and routing incoming messages to the
// per-election listener that the Manager set up for that round.
//
// Each cluster the node joins gets exactly one ClusterTopic instance —
// libp2p-pubsub permits only one Join per topic per host.
//
// Incoming messages for (capsule, replica) pairs that have no active
// listener are briefly buffered (bufferTTL) so a race where a remote
// claim arrives before the local election round registers its listener
// does not silently lose the claim. When a listener finally registers,
// any buffered messages for its key are replayed immediately.
type ClusterTopic struct {
	clusterPath string
	nodeID      string
	ps          *pubsub.PubSub
	topic       *pubsub.Topic
	sub         *pubsub.Subscription
	signer      Signer
	verifier    Verifier
	logger      *zap.Logger

	mu        sync.RWMutex
	listeners map[messageKey]*listener
	buffered  map[messageKey][]bufferedMessage

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewClusterTopic opens the election topic for a cluster. Returns an
// error when the topic or subscription cannot be created.
//
// The caller is responsible for Stopping the returned ClusterTopic when
// the cluster is left.
func NewClusterTopic(
	parent context.Context,
	clusterPath string,
	nodeID string,
	ps *pubsub.PubSub,
	signer Signer,
	verifier Verifier,
	logger *zap.Logger,
) (*ClusterTopic, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	topicName := topicNameFn(clusterPath)
	topic, err := ps.Join(topicName)
	if err != nil {
		return nil, fmt.Errorf("election topic: join %s: %w", topicName, err)
	}
	sub, err := topic.Subscribe()
	if err != nil {
		_ = topic.Close()
		return nil, fmt.Errorf("election topic: subscribe %s: %w", topicName, err)
	}

	ctx, cancel := context.WithCancel(parent)
	t := &ClusterTopic{
		clusterPath: clusterPath,
		nodeID:      nodeID,
		ps:          ps,
		topic:       topic,
		sub:         sub,
		signer:      signer,
		verifier:    verifier,
		logger:      logger,
		listeners:   make(map[messageKey]*listener),
		buffered:    make(map[messageKey][]bufferedMessage),
		ctx:         ctx,
		cancel:      cancel,
	}

	t.wg.Add(2)
	go func() {
		defer t.wg.Done()
		t.loop()
	}()
	go func() {
		defer t.wg.Done()
		t.bufferSweeper()
	}()

	logger.Info("election topic opened",
		zap.String("cluster", clusterPath),
		zap.String("topic", topicName))
	return t, nil
}

// ClusterPath returns the cluster this topic serves.
func (t *ClusterTopic) ClusterPath() string { return t.clusterPath }

// Listen registers a listener for the given (capsule, replica). The
// returned channel receives every matching Claim and ElectionFailed
// message until Unlisten is called. The returned function must be
// invoked exactly once to release the listener.
//
// Registering a second listener for the same key replaces the first.
// This matches the Manager's deduplication invariant: only one election
// round is in flight per (capsule, replica) at any time.
//
// Any recently-buffered messages for the key are delivered synchronously
// before Listen returns, closing the publish-before-listen race where a
// remote claim could otherwise be dropped.
func (t *ClusterTopic) Listen(capsuleID, replicaID string, bufferSize int) (claims <-chan *electionpb.Claim, failures <-chan *electionpb.ElectionFailed, cancel func()) {
	if bufferSize <= 0 {
		bufferSize = 8
	}
	key := messageKey{CapsuleID: capsuleID, ReplicaID: replicaID}
	l := newListener(bufferSize)

	t.mu.Lock()
	if existing, ok := t.listeners[key]; ok {
		existing.close()
		t.logger.Debug("election listener replaced",
			zap.String("cluster", t.clusterPath),
			zap.String("capsule_id", capsuleID),
			zap.String("replica_id", replicaID))
	}
	t.listeners[key] = l
	queued := t.buffered[key]
	delete(t.buffered, key)
	t.mu.Unlock()

	// Replay any messages that arrived before the listener was registered.
	if len(queued) > 0 {
		t.logger.Debug("replaying buffered election messages",
			zap.String("cluster", t.clusterPath),
			zap.String("capsule_id", capsuleID),
			zap.String("replica_id", replicaID),
			zap.Int("count", len(queued)))
	}
	for _, msg := range queued {
		if msg.claim != nil {
			l.dispatchClaim(msg.claim)
		}
		if msg.failed != nil {
			l.dispatchFailure(msg.failed)
		}
	}

	return l.claims, l.failures, func() {
		t.mu.Lock()
		if current, ok := t.listeners[key]; ok && current == l {
			delete(t.listeners, key)
		}
		t.mu.Unlock()
		l.close()
	}
}

// PublishClaim signs and publishes a Claim message to the election topic.
// Returns an error when the signer fails, the message cannot be marshaled,
// or the pubsub publish call itself fails.
func (t *ClusterTopic) PublishClaim(ctx context.Context, claim *electionpb.Claim) error {
	return t.publish(ctx, "claim", claim)
}

// PublishFailure signs and publishes an ElectionFailed message.
func (t *ClusterTopic) PublishFailure(ctx context.Context, failed *electionpb.ElectionFailed) error {
	return t.publish(ctx, "failed", failed)
}

// publish is the shared serialization + sign + publish pipeline for
// both Claim and ElectionFailed messages.
func (t *ClusterTopic) publish(ctx context.Context, msgType string, payload proto.Message) error {
	rawPayload, err := proto.Marshal(payload)
	if err != nil {
		return fmt.Errorf("election topic: marshal payload: %w", err)
	}

	envelope := &electionpb.ElectionMessage{
		Type:      msgType,
		Payload:   rawPayload,
		SenderId:  t.nodeID,
		Timestamp: timestamppb.Now(),
	}

	if t.signer != nil {
		sig, err := t.signer.Sign(canonicalContent(envelope))
		if err != nil {
			return fmt.Errorf("election topic: sign: %w", err)
		}
		envelope.Signature = sig
	}

	data, err := proto.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("election topic: marshal envelope: %w", err)
	}

	if err := t.topic.Publish(ctx, data); err != nil {
		return fmt.Errorf("election topic: publish: %w", err)
	}
	return nil
}

// Stop cancels the message loop, leaves the topic, and waits for the
// loop goroutine to exit. Idempotent.
func (t *ClusterTopic) Stop() {
	if t.cancel == nil {
		return
	}
	t.cancel()
	t.cancel = nil
	t.sub.Cancel()
	_ = t.topic.Close()
	t.wg.Wait()

	t.mu.Lock()
	for _, l := range t.listeners {
		l.close()
	}
	t.listeners = make(map[messageKey]*listener)
	t.buffered = make(map[messageKey][]bufferedMessage)
	t.mu.Unlock()

	t.logger.Info("election topic closed",
		zap.String("cluster", t.clusterPath))
}

// loop runs the pubsub message receive loop, dispatching incoming
// Claims and ElectionFailed messages to the matching listeners.
func (t *ClusterTopic) loop() {
	for {
		msg, err := t.sub.Next(t.ctx)
		if err != nil {
			if t.ctx.Err() != nil {
				return
			}
			t.logger.Warn("election topic recv error",
				zap.String("cluster", t.clusterPath),
				zap.Error(err))
			continue
		}

		// Ignore our own published messages — we already know our decision.
		if msg.ReceivedFrom.String() == t.nodeID {
			continue
		}

		t.handle(msg.Data)
	}
}

// handle parses one received message, verifies the signature, and
// dispatches the inner payload to the matching listener. All errors
// are logged at debug level and the loop continues.
func (t *ClusterTopic) handle(data []byte) {
	var envelope electionpb.ElectionMessage
	if err := proto.Unmarshal(data, &envelope); err != nil {
		t.logger.Debug("election: malformed envelope", zap.Error(err))
		return
	}
	if envelope.SenderId == t.nodeID {
		return
	}
	if envelope.Timestamp != nil {
		if age := time.Since(envelope.Timestamp.AsTime()); age > 30*time.Second {
			t.logger.Debug("election: dropping stale message",
				zap.String("sender", envelope.SenderId),
				zap.Duration("age", age))
			return
		}
	}
	if t.verifier != nil {
		if !t.verifier.Verify(envelope.SenderId, canonicalContent(&envelope), envelope.Signature) {
			t.logger.Debug("election: rejecting message with invalid signature",
				zap.String("sender", envelope.SenderId))
			return
		}
	}

	switch envelope.Type {
	case "claim":
		var claim electionpb.Claim
		if err := proto.Unmarshal(envelope.Payload, &claim); err != nil {
			t.logger.Debug("election: bad claim payload", zap.Error(err))
			return
		}
		t.dispatchClaim(&claim)

	case "failed":
		var failed electionpb.ElectionFailed
		if err := proto.Unmarshal(envelope.Payload, &failed); err != nil {
			t.logger.Debug("election: bad failure payload", zap.Error(err))
			return
		}
		t.dispatchFailure(&failed)

	default:
		t.logger.Debug("election: unknown message type", zap.String("type", envelope.Type))
	}
}

func (t *ClusterTopic) dispatchClaim(claim *electionpb.Claim) {
	key := messageKey{CapsuleID: claim.CapsuleId, ReplicaID: claim.ReplicaId}
	t.mu.Lock()
	l := t.listeners[key]
	if l == nil {
		t.buffered[key] = append(t.buffered[key], bufferedMessage{
			receivedAt: time.Now(),
			claim:      claim,
		})
		t.mu.Unlock()
		t.logger.Debug("buffered election claim (no active listener)",
			zap.String("cluster", t.clusterPath),
			zap.String("capsule_id", claim.CapsuleId),
			zap.String("replica_id", claim.ReplicaId),
			zap.String("sender", claim.NodeId),
			zap.Float64("score", claim.GravityScore))
		return
	}
	t.mu.Unlock()
	t.logger.Debug("dispatching election claim",
		zap.String("cluster", t.clusterPath),
		zap.String("capsule_id", claim.CapsuleId),
		zap.String("replica_id", claim.ReplicaId),
		zap.String("sender", claim.NodeId),
		zap.Float64("score", claim.GravityScore))
	l.dispatchClaim(claim)
}

func (t *ClusterTopic) dispatchFailure(f *electionpb.ElectionFailed) {
	key := messageKey{CapsuleID: f.CapsuleId, ReplicaID: f.ReplicaId}
	t.mu.Lock()
	l := t.listeners[key]
	if l == nil {
		t.buffered[key] = append(t.buffered[key], bufferedMessage{
			receivedAt: time.Now(),
			failed:     f,
		})
		t.mu.Unlock()
		t.logger.Debug("buffered election failure (no active listener)",
			zap.String("cluster", t.clusterPath),
			zap.String("capsule_id", f.CapsuleId),
			zap.String("replica_id", f.ReplicaId),
			zap.String("sender", f.SenderId),
			zap.String("reason", f.Reason))
		return
	}
	t.mu.Unlock()
	t.logger.Debug("dispatching election failure",
		zap.String("cluster", t.clusterPath),
		zap.String("capsule_id", f.CapsuleId),
		zap.String("replica_id", f.ReplicaId),
		zap.String("sender", f.SenderId),
		zap.String("reason", f.Reason))
	l.dispatchFailure(f)
}

// bufferSweeper periodically drops expired buffered messages so the
// buffer does not grow unbounded under churn. Runs on the topic's
// WaitGroup so Stop waits for clean exit.
func (t *ClusterTopic) bufferSweeper() {
	ticker := time.NewTicker(bufferSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.ctx.Done():
			return
		case <-ticker.C:
			t.sweepBuffer()
		}
	}
}

// sweepBuffer drops messages older than bufferTTL from the buffered map.
// Empty key entries are removed to keep the map compact.
func (t *ClusterTopic) sweepBuffer() {
	cutoff := time.Now().Add(-bufferTTL)
	dropped := 0
	t.mu.Lock()
	for key, msgs := range t.buffered {
		kept := msgs[:0]
		for _, m := range msgs {
			if m.receivedAt.After(cutoff) {
				kept = append(kept, m)
			} else {
				dropped++
			}
		}
		if len(kept) == 0 {
			delete(t.buffered, key)
		} else {
			t.buffered[key] = kept
		}
	}
	t.mu.Unlock()

	if dropped > 0 {
		t.logger.Debug("swept expired election messages",
			zap.String("cluster", t.clusterPath),
			zap.Int("dropped", dropped),
			zap.Duration("ttl", bufferTTL))
	}
}

// canonicalContent builds the length-prefixed canonical byte sequence
// signed/verified for an election envelope. Mirrors the scheme used by
// orbit and health so a single Signer/Verifier implementation can serve
// all of Falak's pubsub subsystems.
func canonicalContent(msg *electionpb.ElectionMessage) []byte {
	buf := make([]byte, 0, 4+len(msg.Type)+4+len(msg.Payload)+4+len(msg.SenderId)+4+8)
	buf = appendField(buf, []byte(msg.Type))
	buf = appendField(buf, msg.Payload)
	buf = appendField(buf, []byte(msg.SenderId))
	var tsBytes []byte
	if msg.Timestamp != nil {
		tsBytes = make([]byte, 8)
		binary.BigEndian.PutUint64(tsBytes, uint64(msg.Timestamp.AsTime().UnixNano()))
	}
	buf = appendField(buf, tsBytes)
	return buf
}

func appendField(buf, data []byte) []byte {
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	buf = append(buf, lenBuf...)
	buf = append(buf, data...)
	return buf
}
