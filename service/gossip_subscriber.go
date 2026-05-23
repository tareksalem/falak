package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	servicepb "github.com/tareksalem/falak/service/proto/servicepb"
)

// subscriberSink receives dispatched gossip messages from the Subscriber.
// Implemented by *Manager but defined here so the gossip layer does not
// import the manager type directly during tests.
type subscriberSink interface {
	Receive(svc *Service) error
	HandleWithdrawal(id ServiceID) error
}

// subscriberSubscription is the subset of *pubsub.Subscription used by
// the Subscriber loop. Defined for fake injection in tests.
type subscriberSubscription interface {
	Next(ctx context.Context) (*pubsub.Message, error)
	Cancel()
}

// Subscriber reads Service envelopes off the topic and dispatches them to
// a Manager. Verifies signatures and rejects stale messages.
type Subscriber struct {
	sub      subscriberSubscription
	sink     subscriberSink
	verifier Verifier
	logger   *zap.Logger
	nodeID   string
	maxAge   time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// SubscriberOption configures a Subscriber.
type SubscriberOption func(*Subscriber)

// WithSubscriberVerifier installs the envelope verifier.
func WithSubscriberVerifier(v Verifier) SubscriberOption {
	return func(s *Subscriber) { s.verifier = v }
}

// WithSubscriberLogger installs the zap logger.
func WithSubscriberLogger(logger *zap.Logger) SubscriberOption {
	return func(s *Subscriber) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// WithSubscriberNodeID sets the local node ID for self-message filtering.
func WithSubscriberNodeID(id string) SubscriberOption {
	return func(s *Subscriber) { s.nodeID = id }
}

// WithSubscriberMaxAge overrides the stale-envelope cutoff.
func WithSubscriberMaxAge(d time.Duration) SubscriberOption {
	return func(s *Subscriber) {
		if d > 0 {
			s.maxAge = d
		}
	}
}

// NewSubscriber constructs a Subscriber bound to the supplied
// subscription and dispatch sink. Call Start to begin processing.
func NewSubscriber(sub subscriberSubscription, sink subscriberSink, opts ...SubscriberOption) *Subscriber {
	s := &Subscriber{
		sub:    sub,
		sink:   sink,
		logger: zap.NewNop(),
		maxAge: DefaultMaxEnvelopeAge,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start begins the receive loop. Must only be called once.
func (s *Subscriber) Start(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go s.loop()
}

// Stop cancels the receive loop and waits for it to exit. The underlying
// pubsub subscription is also cancelled.
func (s *Subscriber) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.sub != nil {
		s.sub.Cancel()
	}
	s.wg.Wait()
}

func (s *Subscriber) loop() {
	defer s.wg.Done()
	for {
		msg, err := s.sub.Next(s.ctx)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			s.logger.Error("service gossip receive error", zap.Error(err))
			continue
		}
		if msg.ReceivedFrom.String() == s.nodeID {
			continue
		}
		if err := s.handle(msg.Data); err != nil {
			s.logger.Warn("service gossip handle failed", zap.Error(err))
		}
	}
}

// handle parses, validates, and dispatches a single envelope payload.
func (s *Subscriber) handle(data []byte) error {
	var env servicepb.Envelope
	if err := proto.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("service gossip: unmarshal envelope: %w", err)
	}
	if env.Timestamp != nil {
		age := time.Since(env.Timestamp.AsTime())
		if age > s.maxAge {
			return fmt.Errorf("%w: age=%s", ErrStaleEnvelope, age)
		}
	}
	if s.verifier != nil {
		sig := env.Signature
		env.Signature = nil
		content := canonicalEnvelopeContent(&env)
		env.Signature = sig
		if len(sig) == 0 || !s.verifier.Verify(env.SenderId, content, sig) {
			return fmt.Errorf("%w: sender=%s", ErrInvalidSignature, env.SenderId)
		}
	}
	switch env.Type {
	case EnvelopeTypeUpdate:
		var update servicepb.ServiceUpdate
		if err := proto.Unmarshal(env.Payload, &update); err != nil {
			return fmt.Errorf("service gossip: unmarshal update: %w", err)
		}
		svc := serviceFromProto(update.Service)
		if svc == nil {
			return errors.New("service gossip: update payload missing service")
		}
		return s.sink.Receive(svc)
	case EnvelopeTypeWithdrawal:
		var w servicepb.ServiceWithdrawal
		if err := proto.Unmarshal(env.Payload, &w); err != nil {
			return fmt.Errorf("service gossip: unmarshal withdrawal: %w", err)
		}
		if w.ServiceId == "" {
			return errors.New("service gossip: withdrawal payload missing service_id")
		}
		return s.sink.HandleWithdrawal(ServiceID(w.ServiceId))
	default:
		return fmt.Errorf("service gossip: unknown envelope type %q", env.Type)
	}
}

// Handle is the test-facing entry point for delivering a raw envelope
// directly to the Subscriber's dispatch path without a live subscription.
func (s *Subscriber) Handle(data []byte) error { return s.handle(data) }
