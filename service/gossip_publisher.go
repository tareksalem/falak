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
	"google.golang.org/protobuf/types/known/timestamppb"

	servicepb "github.com/tareksalem/falak/service/proto/servicepb"
)

// publisherTopic abstracts the pubsub.Topic publish surface so tests can
// inject a stub without holding a live libp2p PubSub.
type publisherTopic interface {
	Publish(ctx context.Context, data []byte, opts ...pubsub.PubOpt) error
}

// activeProvider supplies the list of currently-active Services that
// the publisher should periodically re-broadcast to keep peers warm.
type activeProvider interface {
	ActiveServices() []*Service
}

// Publisher pushes service updates and withdrawals onto the per-cluster
// gossip topic and refreshes them on a TTL.
type Publisher struct {
	topic        publisherTopic
	signer       Signer
	nodeID       string
	logger       *zap.Logger
	republishInt time.Duration
	provider     activeProvider

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// PublisherOption configures a Publisher.
type PublisherOption func(*Publisher)

// WithPublisherSigner installs the envelope signer.
func WithPublisherSigner(s Signer) PublisherOption {
	return func(p *Publisher) { p.signer = s }
}

// WithPublisherNodeID sets the local node ID stamped into envelopes.
func WithPublisherNodeID(id string) PublisherOption {
	return func(p *Publisher) { p.nodeID = id }
}

// WithPublisherLogger installs the zap logger.
func WithPublisherLogger(logger *zap.Logger) PublisherOption {
	return func(p *Publisher) {
		if logger != nil {
			p.logger = logger
		}
	}
}

// WithPublisherRepublishInterval overrides the republish cadence.
func WithPublisherRepublishInterval(d time.Duration) PublisherOption {
	return func(p *Publisher) {
		if d > 0 {
			p.republishInt = d
		}
	}
}

// WithPublisherActiveProvider installs the source of currently-active
// services used by the background republish loop.
func WithPublisherActiveProvider(a activeProvider) PublisherOption {
	return func(p *Publisher) { p.provider = a }
}

// NewPublisher constructs a Publisher bound to the given topic.
// Background republish only starts after Start is called.
func NewPublisher(topic publisherTopic, opts ...PublisherOption) *Publisher {
	p := &Publisher{
		topic:        topic,
		logger:       zap.NewNop(),
		republishInt: DefaultRepublishInterval,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Start begins the periodic republish loop. Safe to call once.
func (p *Publisher) Start(ctx context.Context) {
	if p.provider == nil {
		return
	}
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.wg.Add(1)
	go p.republishLoop()
}

// Stop cancels the republish loop and waits for it to exit.
func (p *Publisher) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

// PublishUpdate signs a ServiceUpdate envelope and writes it to the topic.
func (p *Publisher) PublishUpdate(ctx context.Context, svc *Service) error {
	if svc == nil {
		return errors.New("service gossip: nil service in PublishUpdate")
	}
	update := &servicepb.ServiceUpdate{
		Service:      serviceToProto(svc),
		SenderNodeId: p.nodeID,
	}
	payload, err := proto.Marshal(update)
	if err != nil {
		return fmt.Errorf("service gossip: marshal update: %w", err)
	}
	data, err := p.buildEnvelope(EnvelopeTypeUpdate, payload)
	if err != nil {
		return err
	}
	if err := p.topic.Publish(ctx, data); err != nil {
		return fmt.Errorf("service gossip: publish update: %w", err)
	}
	p.logger.Info("service update published",
		zap.String("service", svc.ID.String()),
		zap.String("name", svc.Spec.Name),
		zap.String("cluster", svc.ClusterID))
	return nil
}

// PublishWithdrawal signs a ServiceWithdrawal envelope and writes it.
func (p *Publisher) PublishWithdrawal(ctx context.Context, id ServiceID) error {
	if id == "" {
		return errors.New("service gossip: empty service ID in PublishWithdrawal")
	}
	w := &servicepb.ServiceWithdrawal{ServiceId: string(id), SenderNodeId: p.nodeID}
	payload, err := proto.Marshal(w)
	if err != nil {
		return fmt.Errorf("service gossip: marshal withdrawal: %w", err)
	}
	data, err := p.buildEnvelope(EnvelopeTypeWithdrawal, payload)
	if err != nil {
		return err
	}
	if err := p.topic.Publish(ctx, data); err != nil {
		return fmt.Errorf("service gossip: publish withdrawal: %w", err)
	}
	p.logger.Info("service withdrawal published", zap.String("service", string(id)))
	return nil
}

// buildEnvelope marshals a signed envelope for transport.
func (p *Publisher) buildEnvelope(envType string, payload []byte) ([]byte, error) {
	env := &servicepb.Envelope{
		Type:      envType,
		Payload:   payload,
		SenderId:  p.nodeID,
		Timestamp: timestamppb.Now(),
	}
	if p.signer != nil {
		sig, err := p.signer.Sign(canonicalEnvelopeContent(env))
		if err != nil {
			return nil, fmt.Errorf("service gossip: sign envelope: %w", err)
		}
		env.Signature = sig
	}
	out, err := proto.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("service gossip: marshal envelope: %w", err)
	}
	return out, nil
}

func (p *Publisher) republishLoop() {
	defer p.wg.Done()
	t := time.NewTicker(p.republishInt)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
			services := p.provider.ActiveServices()
			for _, svc := range services {
				if err := p.PublishUpdate(p.ctx, svc); err != nil {
					p.logger.Warn("service republish failed",
						zap.String("service", svc.ID.String()),
						zap.Error(err))
				}
			}
		}
	}
}
