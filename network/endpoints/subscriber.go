package endpoints

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/tareksalem/falak/network/internal/samplelog"
	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
)

// DefaultSubscriberStopGrace bounds the wait in Stop for in-flight
// subscribe goroutines to exit cleanly.
const DefaultSubscriberStopGrace = 5 * time.Second

// Subscriber joins the per-group endpoint topics and feeds verified
// records and withdrawals into the Registry mirror. One Subscriber
// instance services many groups; Subscribe / Unsubscribe are safe to
// call concurrently.
type Subscriber struct {
	pubsub          PubSub
	verifier        Verifier
	registry        *Registry
	logger          *zap.Logger
	localNodeID     string
	stopGracePeriod time.Duration

	mu      sync.Mutex
	subs    map[string]*subscription // keyed by topic name
	wg      sync.WaitGroup
	closed  bool

	// errSampler bounds noisy per-message error logs (unmarshal
	// failures, signature rejection) to at most one ERROR per second
	// per error class. A misbehaving peer can flood the topic with
	// malformed envelopes; unsampled logs would amplify the attack.
	errSampler *samplelog.Sampler
}

// subscription is the live state for one joined topic.
type subscription struct {
	topic  string
	cancel context.CancelFunc
	done   chan struct{}
}

// SubscriberOption configures a Subscriber.
type SubscriberOption func(*Subscriber)

// WithSubscriberPubSub sets the pubsub transport.
func WithSubscriberPubSub(p PubSub) SubscriberOption {
	return func(s *Subscriber) { s.pubsub = p }
}

// WithVerifier sets the signature verifier.
func WithVerifier(v Verifier) SubscriberOption {
	return func(s *Subscriber) { s.verifier = v }
}

// WithRegistry binds the Subscriber to its target Registry.
func WithRegistry(r *Registry) SubscriberOption {
	return func(s *Subscriber) { s.registry = r }
}

// WithSubscriberLogger sets the zap logger.
func WithSubscriberLogger(l *zap.Logger) SubscriberOption {
	return func(s *Subscriber) { s.logger = l }
}

// WithSubscriberLocalNodeID sets the local node ID, used to skip
// envelopes echoed back from the local publisher.
func WithSubscriberLocalNodeID(id string) SubscriberOption {
	return func(s *Subscriber) { s.localNodeID = id }
}

// WithSubscriberStopGracePeriod bounds the Stop wait.
func WithSubscriberStopGracePeriod(d time.Duration) SubscriberOption {
	return func(s *Subscriber) { s.stopGracePeriod = d }
}

// NewSubscriber constructs a Subscriber. PubSub and Registry are
// required for production. Verifier may be nil in tests; in that case
// no signature check is performed (mirrors capsule/orbit).
func NewSubscriber(opts ...SubscriberOption) *Subscriber {
	s := &Subscriber{
		logger:          zap.NewNop(),
		stopGracePeriod: DefaultSubscriberStopGrace,
		subs:            make(map[string]*subscription),
		errSampler:      samplelog.NewSampler(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.stopGracePeriod <= 0 {
		s.stopGracePeriod = DefaultSubscriberStopGrace
	}
	return s
}

// Subscribe joins the per-group endpoint topic and dispatches incoming
// envelopes to the Registry. Idempotent: a second Subscribe for the
// same (cluster, group) is a no-op.
func (s *Subscriber) Subscribe(ctx context.Context, clusterPath, groupID string) error {
	if s.pubsub == nil {
		return errors.New("endpoints: subscriber missing pubsub")
	}
	if s.registry == nil {
		return errors.New("endpoints: subscriber missing registry")
	}
	topic := BuildTopic(clusterPath, groupID)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("endpoints: subscriber closed")
	}
	if _, ok := s.subs[topic]; ok {
		s.mu.Unlock()
		return nil
	}
	subCtx, cancel := context.WithCancel(context.Background())
	ch, err := s.pubsub.Subscribe(subCtx, topic)
	if err != nil {
		cancel()
		s.mu.Unlock()
		return fmt.Errorf("endpoints: subscribe topic %q: %w", topic, err)
	}
	sub := &subscription{topic: topic, cancel: cancel, done: make(chan struct{})}
	s.subs[topic] = sub
	s.mu.Unlock()

	s.wg.Add(1)
	go s.readLoop(subCtx, sub, ch)

	s.logger.Info("endpoint subscribed",
		zap.String("cluster", clusterPath),
		zap.String("group", groupID),
		zap.String("topic", topic))
	return nil
}

// Unsubscribe leaves the per-group topic. Registry entries are NOT
// evicted on unsubscribe — they expire naturally via TTL. Idempotent.
func (s *Subscriber) Unsubscribe(ctx context.Context, clusterPath, groupID string) error {
	topic := BuildTopic(clusterPath, groupID)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	sub, ok := s.subs[topic]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.subs, topic)
	s.mu.Unlock()
	sub.cancel()
	<-sub.done
	s.logger.Info("endpoint unsubscribed",
		zap.String("cluster", clusterPath),
		zap.String("group", groupID))
	return nil
}

// Stop cancels every subscription and waits up to the grace period for
// the read goroutines to exit. Idempotent.
func (s *Subscriber) Stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for _, sub := range s.subs {
		sub.cancel()
	}
	s.subs = make(map[string]*subscription)
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(s.stopGracePeriod):
		s.logger.Warn("endpoint subscriber stop timed out",
			zap.Duration("grace", s.stopGracePeriod))
	}
}

func (s *Subscriber) readLoop(ctx context.Context, sub *subscription, ch <-chan []byte) {
	defer s.wg.Done()
	defer close(sub.done)
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			s.dispatch(data)
		}
	}
}

// dispatch parses one envelope and routes the payload into the
// registry. Verification, type dispatch, and self-loop filtering all
// happen here so the read loop stays a single tight select.
func (s *Subscriber) dispatch(data []byte) {
	recordType, payload, senderID, err := ParseEnvelope(data, s.verifier)
	if err != nil {
		s.logger.Debug("endpoint envelope rejected",
			zap.Error(err))
		return
	}
	// Drop self-loops: the publisher already wrote to the local
	// registry path directly; gossip just amplifies state to peers.
	if s.localNodeID != "" && senderID == s.localNodeID {
		return
	}
	switch recordType {
	case EnvelopeTypeRecord:
		var rec endpointpb.EndpointRecord
		if err := proto.Unmarshal(payload, &rec); err != nil {
			if s.errSampler.Allow("record_unmarshal") {
				s.logger.Warn("endpoint record unmarshal failed",
					zap.String("peer", senderID),
					zap.Int64("dropped", s.errSampler.Suppressed("record_unmarshal")),
					zap.Error(err))
			}
			return
		}
		s.registry.Insert(&rec)
	case EnvelopeTypeWithdrawal:
		var w endpointpb.EndpointWithdrawal
		if err := proto.Unmarshal(payload, &w); err != nil {
			if s.errSampler.Allow("withdrawal_unmarshal") {
				s.logger.Warn("endpoint withdrawal unmarshal failed",
					zap.String("peer", senderID),
					zap.Int64("dropped", s.errSampler.Suppressed("withdrawal_unmarshal")),
					zap.Error(err))
			}
			return
		}
		s.registry.Withdraw(&w)
	}
}
