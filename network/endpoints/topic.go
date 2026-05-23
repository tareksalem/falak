// Package endpoints implements the per-group endpoint gossip layer.
//
// Each capsule group owns a PubSub topic — falak/<cluster>/endpoints/<groupID> —
// on which every running replica announces its reachability (bridge IP,
// named ports, SWIM state). Subscribers maintain a local registry mirror
// used by the DNS responder and the L4 proxy.
//
// The envelope shape mirrors the orbit announcement primitive: a signed,
// length-prefixed canonical byte sequence over (type, payload, senderID,
// timestamp). Receivers verify the signature against the sender's public
// key (looked up in the cluster phonebook by node ID) before mutating
// their local mirror.
package endpoints

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
)

// Sentinel errors returned by ParseEnvelope. Callers use errors.Is to
// distinguish bad signatures from stale envelopes from malformed bytes.
var (
	// ErrBadSignature signals a missing, malformed, or invalid signature
	// over the envelope's canonical content.
	ErrBadSignature = errors.New("endpoints: invalid envelope signature")
	// ErrStaleEnvelope signals an envelope whose timestamp is older than
	// MaxEnvelopeAge or is more than 30 seconds in the future.
	ErrStaleEnvelope = errors.New("endpoints: stale envelope")
	// ErrMalformedEnvelope signals an envelope whose proto bytes failed
	// to unmarshal.
	ErrMalformedEnvelope = errors.New("endpoints: malformed envelope")
	// ErrUnknownEnvelopeType signals an envelope whose Type field is
	// neither "record" nor "withdrawal".
	ErrUnknownEnvelopeType = errors.New("endpoints: unknown envelope type")
)

// Envelope type tags. Receivers dispatch on these values.
const (
	// EnvelopeTypeRecord identifies an EndpointRecord payload.
	EnvelopeTypeRecord = "record"
	// EnvelopeTypeWithdrawal identifies an EndpointWithdrawal payload.
	EnvelopeTypeWithdrawal = "withdrawal"
)

// MaxEnvelopeAge bounds how old an envelope may be before receivers
// reject it. Five minutes accommodates clock skew across nodes while
// still rejecting obvious replay attempts.
const MaxEnvelopeAge = 5 * time.Minute

// maxFutureSkew bounds how far into the future an envelope's timestamp
// may sit. A tighter bound than MaxEnvelopeAge because clocks rarely
// disagree by more than a few seconds in a healthy cluster.
const maxFutureSkew = 30 * time.Second

// Signer signs the canonical content of an envelope with the local
// node's private key. Implementations must produce a signature whose
// verification key is published in the cluster phonebook under the
// sender's node ID.
type Signer interface {
	Sign(content []byte) ([]byte, error)
}

// Verifier checks that the provided signature is valid for the
// envelope's canonical content under the public key associated with
// senderID. Implementations must return false on unknown senders.
type Verifier interface {
	Verify(senderID string, content, signature []byte) bool
}

// BuildTopic returns the per-group endpoint topic name. One topic
// exists per (cluster, group) pair; subscribers join the topics of
// groups they host members in or that their local services target.
func BuildTopic(clusterPath, groupID string) string {
	return fmt.Sprintf("falak/%s/endpoints/%s", clusterPath, groupID)
}

// BuildEnvelope produces the signed wire bytes for an endpoint
// envelope. The payload is the marshalled EndpointRecord or
// EndpointWithdrawal proto; recordType selects which.
//
// The signature covers the canonical, length-prefixed encoding of
// (type, payload, senderID, timestamp) so that re-ordering or splicing
// fields cannot reproduce a valid signature.
func BuildEnvelope(recordType string, payload []byte, senderID string, signer Signer) ([]byte, error) {
	if recordType != EnvelopeTypeRecord && recordType != EnvelopeTypeWithdrawal {
		return nil, fmt.Errorf("%w: %q", ErrUnknownEnvelopeType, recordType)
	}
	if signer == nil {
		return nil, errors.New("endpoints: nil signer")
	}
	if senderID == "" {
		return nil, errors.New("endpoints: empty senderID")
	}
	env := &endpointpb.Envelope{
		Type:      recordType,
		Payload:   payload,
		SenderId:  senderID,
		Timestamp: timestamppb.Now(),
	}
	sig, err := signer.Sign(canonicalContent(env))
	if err != nil {
		return nil, fmt.Errorf("endpoints: sign envelope: %w", err)
	}
	env.Signature = sig

	data, err := proto.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("endpoints: marshal envelope: %w", err)
	}
	return data, nil
}

// ParseEnvelope decodes wire bytes, verifies the signature against the
// supplied verifier, and rejects stale envelopes. On success it returns
// the record type, the (verified) payload bytes, and the senderID so
// the caller can demarshal into the appropriate proto message.
//
// A nil verifier disables signature checking. Tests rely on that; in
// production a verifier is always wired through the subscriber.
func ParseEnvelope(data []byte, verifier Verifier) (recordType string, payload []byte, senderID string, err error) {
	var env endpointpb.Envelope
	if uerr := proto.Unmarshal(data, &env); uerr != nil {
		return "", nil, "", fmt.Errorf("%w: %v", ErrMalformedEnvelope, uerr)
	}

	if env.Type != EnvelopeTypeRecord && env.Type != EnvelopeTypeWithdrawal {
		return "", nil, "", fmt.Errorf("%w: %q", ErrUnknownEnvelopeType, env.Type)
	}

	if env.Timestamp == nil {
		return "", nil, "", fmt.Errorf("%w: missing timestamp", ErrStaleEnvelope)
	}
	now := time.Now()
	ts := env.Timestamp.AsTime()
	if now.Sub(ts) > MaxEnvelopeAge {
		return "", nil, "", fmt.Errorf("%w: age=%s", ErrStaleEnvelope, now.Sub(ts))
	}
	if ts.Sub(now) > maxFutureSkew {
		return "", nil, "", fmt.Errorf("%w: future skew=%s", ErrStaleEnvelope, ts.Sub(now))
	}

	if verifier != nil {
		if len(env.Signature) == 0 {
			return "", nil, "", fmt.Errorf("%w: missing signature", ErrBadSignature)
		}
		// Reconstruct canonical content without the signature field.
		sig := env.Signature
		env.Signature = nil
		ok := verifier.Verify(env.SenderId, canonicalContent(&env), sig)
		env.Signature = sig
		if !ok {
			return "", nil, "", fmt.Errorf("%w: sender=%q", ErrBadSignature, env.SenderId)
		}
	}
	return env.Type, env.Payload, env.SenderId, nil
}

// canonicalContent assembles the length-prefixed signing content over
// the envelope's identity fields. Mirrors capsule/orbit's canonical
// encoding (each field prefixed by a 4-byte big-endian length) so two
// distinct concatenations of (type, payload, senderID) cannot collide.
//
// Field order:
//
//	[len][type][len][payload][len][senderID][len][timestamp-nanos-be64]
func canonicalContent(env *endpointpb.Envelope) []byte {
	capacity := 4 + len(env.Type) + 4 + len(env.Payload) + 4 + len(env.SenderId) + 4 + 8
	buf := make([]byte, 0, capacity)
	buf = writeField(buf, []byte(env.Type))
	buf = writeField(buf, env.Payload)
	buf = writeField(buf, []byte(env.SenderId))

	var tsBytes []byte
	if env.Timestamp != nil {
		tsBytes = make([]byte, 8)
		binary.BigEndian.PutUint64(tsBytes, uint64(env.Timestamp.AsTime().UnixNano()))
	}
	buf = writeField(buf, tsBytes)
	return buf
}

// writeField appends [4-byte BE length][data] to buf and returns the
// new slice. Local to this package so it stays in lockstep with the
// canonicalContent format above.
func writeField(buf, data []byte) []byte {
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	buf = append(buf, lenBuf...)
	buf = append(buf, data...)
	return buf
}
