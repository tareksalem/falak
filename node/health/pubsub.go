package health

import (
	"context"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tareksalem/falak/node/internal/signing"
	"github.com/tareksalem/falak/node/phonebook"
	"github.com/tareksalem/falak/node/proto/healthpb"
	"github.com/tareksalem/falak/shared"
)

const (
	// DefaultMaxMessageAge is the maximum age of a health PubSub message before rejection.
	DefaultMaxMessageAge = 30 * time.Second

	// DefaultPublishMaxRetries is the max retries for publishing a health message.
	DefaultPublishMaxRetries = 3

	// DefaultPublishRetryDelay is the delay between publish retries.
	DefaultPublishRetryDelay = 100 * time.Millisecond
)

// HealthPubSub manages the health PubSub topic for a cluster.
type HealthPubSub struct {
	host        host.Host
	ps          *pubsub.PubSub
	phonebook   phonebook.IPhonebook
	privateKey  crypto.PrivKey
	logger      *zap.Logger
	clusterPath string

	topic *pubsub.Topic

	// Configurable
	maxMessageAge     time.Duration
	publishMaxRetries int
	publishRetryDelay time.Duration

	// Message handler callbacks — set by the Monitor
	onScoreUpdate      func(update *healthpb.ScoreUpdate)
	onPingRequest      func(req *healthpb.PingRequest)
	onThresholdCrossed func(tc *healthpb.ThresholdCrossed)
}

// HealthPubSubConfig holds configurable PubSub parameters.
type HealthPubSubConfig struct {
	MaxMessageAge     time.Duration
	PublishMaxRetries int
	PublishRetryDelay time.Duration
}

// DefaultHealthPubSubConfig returns defaults.
func DefaultHealthPubSubConfig() HealthPubSubConfig {
	return HealthPubSubConfig{
		MaxMessageAge:     DefaultMaxMessageAge,
		PublishMaxRetries: DefaultPublishMaxRetries,
		PublishRetryDelay: DefaultPublishRetryDelay,
	}
}

// NewHealthPubSub creates a new health PubSub manager for a cluster.
func NewHealthPubSub(
	h host.Host,
	ps *pubsub.PubSub,
	pb phonebook.IPhonebook,
	privKey crypto.PrivKey,
	logger *zap.Logger,
	clusterPath string,
	config HealthPubSubConfig,
) *HealthPubSub {
	return &HealthPubSub{
		host:              h,
		ps:                ps,
		phonebook:         pb,
		privateKey:        privKey,
		logger:            logger,
		clusterPath:       clusterPath,
		maxMessageAge:     config.MaxMessageAge,
		publishMaxRetries: config.PublishMaxRetries,
		publishRetryDelay: config.PublishRetryDelay,
	}
}

// SetHandlers sets the callback functions for incoming messages.
func (hp *HealthPubSub) SetHandlers(
	onScoreUpdate func(*healthpb.ScoreUpdate),
	onPingRequest func(*healthpb.PingRequest),
	onThresholdCrossed func(*healthpb.ThresholdCrossed),
) {
	hp.onScoreUpdate = onScoreUpdate
	hp.onPingRequest = onPingRequest
	hp.onThresholdCrossed = onThresholdCrossed
}

// Subscribe joins the health topic and starts the message loop.
// Returns after subscribing; the message loop runs in the background.
func (hp *HealthPubSub) Subscribe(ctx context.Context) error {
	topicName := shared.BuildHealthTopic(hp.clusterPath)

	topic, err := hp.ps.Join(topicName)
	if err != nil {
		return err
	}
	hp.topic = topic

	sub, err := topic.Subscribe()
	if err != nil {
		topic.Close()
		return err
	}

	go hp.messageLoop(ctx, sub)

	hp.logger.Debug("subscribed to health topic",
		zap.String("cluster", hp.clusterPath),
		zap.String("topic", topicName))

	return nil
}

// Close closes the health topic.
func (hp *HealthPubSub) Close() {
	if hp.topic != nil {
		hp.topic.Close()
	}
}

// PublishScoreUpdate publishes a ScoreUpdate message.
func (hp *HealthPubSub) PublishScoreUpdate(ctx context.Context, update *healthpb.ScoreUpdate) error {
	return hp.publish(ctx, "score_update", update)
}

// PublishPingRequest publishes a PingRequest message.
func (hp *HealthPubSub) PublishPingRequest(ctx context.Context, req *healthpb.PingRequest) error {
	return hp.publish(ctx, "ping_request", req)
}

// PublishThresholdCrossed publishes a ThresholdCrossed message.
func (hp *HealthPubSub) PublishThresholdCrossed(ctx context.Context, tc *healthpb.ThresholdCrossed) error {
	return hp.publish(ctx, "threshold_crossed", tc)
}

// messageLoop processes incoming health PubSub messages.
func (hp *HealthPubSub) messageLoop(ctx context.Context, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			hp.logger.Error("health subscription error", zap.Error(err))
			continue
		}

		// Skip own messages
		if msg.ReceivedFrom == hp.host.ID() {
			continue
		}

		hp.handleMessage(msg.Data)
	}
}

// handleMessage processes a single health PubSub message.
func (hp *HealthPubSub) handleMessage(data []byte) {
	var envelope healthpb.HealthMessage
	if err := proto.Unmarshal(data, &envelope); err != nil {
		hp.logger.Debug("failed to unmarshal health message", zap.Error(err))
		return
	}

	// Replay protection
	if envelope.Timestamp == nil {
		return
	}
	msgAge := time.Since(envelope.Timestamp.AsTime())
	if msgAge > hp.maxMessageAge {
		hp.logger.Debug("rejecting expired health message",
			zap.String("sender", envelope.SenderId),
			zap.Duration("age", msgAge))
		return
	}

	// Verify signature
	if !hp.verifySignature(&envelope) {
		hp.logger.Debug("rejecting health message with invalid signature",
			zap.String("sender", envelope.SenderId))
		return
	}

	// Dispatch by type
	switch envelope.Type {
	case "score_update":
		if hp.onScoreUpdate != nil {
			var update healthpb.ScoreUpdate
			if err := proto.Unmarshal(envelope.Payload, &update); err == nil {
				hp.onScoreUpdate(&update)
			}
		}
	case "ping_request":
		if hp.onPingRequest != nil {
			var req healthpb.PingRequest
			if err := proto.Unmarshal(envelope.Payload, &req); err == nil {
				hp.onPingRequest(&req)
			}
		}
	case "threshold_crossed":
		if hp.onThresholdCrossed != nil {
			var tc healthpb.ThresholdCrossed
			if err := proto.Unmarshal(envelope.Payload, &tc); err == nil {
				hp.onThresholdCrossed(&tc)
			}
		}
	default:
		hp.logger.Debug("unknown health message type", zap.String("type", envelope.Type))
	}
}

// publish sends a health message to the cluster topic with retry.
func (hp *HealthPubSub) publish(ctx context.Context, msgType string, payload proto.Message) error {
	if hp.topic == nil {
		return nil
	}

	payloadBytes, err := proto.Marshal(payload)
	if err != nil {
		return err
	}

	envelope := &healthpb.HealthMessage{
		Type:      msgType,
		Payload:   payloadBytes,
		SenderId:  hp.host.ID().String(),
		Timestamp: timestamppb.Now(),
	}

	envelope.Signature = hp.signMessage(envelope)

	data, err := proto.Marshal(envelope)
	if err != nil {
		return err
	}

	// Publish with retry
	var lastErr error
	for attempt := 0; attempt < hp.publishMaxRetries; attempt++ {
		if err := hp.topic.Publish(ctx, data); err == nil {
			return nil
		} else {
			lastErr = err
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(hp.publishRetryDelay):
			}
		}
	}
	return lastErr
}

// signMessage signs a health message envelope using length-prefixed canonical encoding.
func (hp *HealthPubSub) signMessage(msg *healthpb.HealthMessage) []byte {
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

	sig, err := hp.privateKey.Sign(buf.Bytes())
	if err != nil {
		hp.logger.Error("failed to sign health message", zap.Error(err))
		return nil
	}
	return sig
}

// verifySignature verifies the signature on a health message.
func (hp *HealthPubSub) verifySignature(msg *healthpb.HealthMessage) bool {
	if msg.Signature == nil {
		return false
	}

	// Look up sender's public key from phonebook
	entry, err := hp.phonebook.Get(msg.SenderId, hp.clusterPath)
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
