package service

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"

	servicepb "github.com/tareksalem/falak/service/proto/servicepb"
)

// EnvelopeTypeUpdate identifies the gossip envelope as a ServiceUpdate.
const EnvelopeTypeUpdate = "update"

// EnvelopeTypeWithdrawal identifies the gossip envelope as a ServiceWithdrawal.
const EnvelopeTypeWithdrawal = "withdrawal"

// DefaultRepublishInterval is the default cadence at which the Publisher
// re-broadcasts every active Service to keep peers warm.
const DefaultRepublishInterval = 60 * time.Second

// DefaultMaxEnvelopeAge bounds how old a received envelope may be before
// the Subscriber rejects it as stale. Matches the orbit subsystem.
const DefaultMaxEnvelopeAge = 5 * time.Minute

// ErrInvalidSignature is returned by the Subscriber when the envelope
// signature fails verification.
var ErrInvalidSignature = errors.New("service gossip: invalid signature")

// ErrStaleEnvelope is returned by the Subscriber when the envelope is
// older than the configured max age.
var ErrStaleEnvelope = errors.New("service gossip: stale envelope")

// Signer signs the canonical content of a gossip envelope with the local
// node's private key. Mirrors capsule/orbit.Signer.
type Signer interface {
	Sign(content []byte) ([]byte, error)
}

// Verifier verifies an envelope signature against the public key
// associated with senderID. Mirrors capsule/orbit.Verifier.
type Verifier interface {
	Verify(senderID string, content []byte, signature []byte) bool
}

// PubSub is the subset of *pubsub.PubSub required by the gossip layer.
// Defined as an interface so tests can supply an in-memory fake.
type PubSub interface {
	Join(topic string, opts ...pubsub.TopicOpt) (*pubsub.Topic, error)
}

// BuildServiceTopic returns the per-cluster service-gossip topic name.
func BuildServiceTopic(clusterPath string) string {
	return fmt.Sprintf("falak/%s/services", clusterPath)
}

// canonicalEnvelopeContent builds the length-prefixed canonical byte
// sequence covered by the envelope signature. Format mirrors the orbit
// signer: [len][type][len][payload][len][sender_id][len][ts-nanos].
func canonicalEnvelopeContent(env *servicepb.Envelope) []byte {
	capacity := 4 + len(env.Type) + 4 + len(env.Payload) + 4 + len(env.SenderId) + 4 + 8
	buf := make([]byte, 0, capacity)
	buf = appendField(buf, []byte(env.Type))
	buf = appendField(buf, env.Payload)
	buf = appendField(buf, []byte(env.SenderId))
	var tsBytes []byte
	if env.Timestamp != nil {
		tsBytes = make([]byte, 8)
		binary.BigEndian.PutUint64(tsBytes, uint64(env.Timestamp.AsTime().UnixNano()))
	}
	return appendField(buf, tsBytes)
}

func appendField(buf, data []byte) []byte {
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	buf = append(buf, lenBuf...)
	buf = append(buf, data...)
	return buf
}
