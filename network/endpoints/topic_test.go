package endpoints

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
)

func TestBuildTopic_Format(t *testing.T) {
	got := BuildTopic("c1", "g-42")
	want := "falak/c1/endpoints/g-42"
	if got != want {
		t.Fatalf("BuildTopic = %q, want %q", got, want)
	}
}

func TestEnvelope_RoundTrip(t *testing.T) {
	signer := &stubSigner{prefix: []byte("SIG:")}
	verifier := newStubVerifier("SIG:", "node-a")

	rec := &endpointpb.EndpointRecord{
		ClusterPath: "c1",
		GroupId:     "g1",
		CapsuleName: "api",
		ReplicaId:   "r1",
		NodeId:      "node-a",
		BridgeIp:    "10.0.0.5",
		SwimState:   SwimStateAlive,
		EmittedAt:   timestamppb.Now(),
		TtlSeconds:  30,
	}
	payload, err := proto.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}

	data, err := BuildEnvelope(EnvelopeTypeRecord, payload, "node-a", signer)
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}

	gotType, gotPayload, gotSender, err := ParseEnvelope(data, verifier)
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if gotType != EnvelopeTypeRecord {
		t.Errorf("type = %q, want %q", gotType, EnvelopeTypeRecord)
	}
	if gotSender != "node-a" {
		t.Errorf("sender = %q, want node-a", gotSender)
	}

	var back endpointpb.EndpointRecord
	if err := proto.Unmarshal(gotPayload, &back); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if back.ReplicaId != "r1" || back.BridgeIp != "10.0.0.5" {
		t.Errorf("round-trip lost fields: %+v", &back)
	}
}

func TestEnvelope_TamperedPayloadRejected(t *testing.T) {
	signer := &stubSigner{prefix: []byte("SIG:")}
	verifier := newStubVerifier("SIG:", "node-a")

	data, err := BuildEnvelope(EnvelopeTypeRecord, []byte("orig"), "node-a", signer)
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}

	// Decode, mutate payload, re-encode without re-signing.
	var env endpointpb.Envelope
	if err := proto.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal env: %v", err)
	}
	env.Payload = []byte("tampered")
	bad, err := proto.Marshal(&env)
	if err != nil {
		t.Fatalf("re-marshal env: %v", err)
	}

	_, _, _, err = ParseEnvelope(bad, verifier)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature, got %v", err)
	}
}

func TestEnvelope_UnknownSenderRejected(t *testing.T) {
	signer := &stubSigner{prefix: []byte("SIG:")}
	verifier := newStubVerifier("SIG:", "node-a")

	data, err := BuildEnvelope(EnvelopeTypeRecord, []byte("p"), "node-b", signer)
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}
	_, _, _, err = ParseEnvelope(data, verifier)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature for unknown sender, got %v", err)
	}
}

func TestEnvelope_MissingSignatureRejected(t *testing.T) {
	verifier := newStubVerifier("SIG:", "node-a")
	env := &endpointpb.Envelope{
		Type:      EnvelopeTypeRecord,
		Payload:   []byte("p"),
		SenderId:  "node-a",
		Timestamp: timestamppb.Now(),
	}
	data, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, _, _, err = ParseEnvelope(data, verifier)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("expected ErrBadSignature for missing sig, got %v", err)
	}
}

func TestEnvelope_StaleRejected(t *testing.T) {
	signer := &stubSigner{prefix: []byte("SIG:")}
	verifier := newStubVerifier("SIG:", "node-a")

	// Build a fresh envelope first, then back-date the timestamp and re-sign.
	env := &endpointpb.Envelope{
		Type:      EnvelopeTypeRecord,
		Payload:   []byte("p"),
		SenderId:  "node-a",
		Timestamp: timestamppb.New(time.Now().Add(-10 * time.Minute)),
	}
	sig, err := signer.Sign(canonicalContent(env))
	if err != nil {
		t.Fatal(err)
	}
	env.Signature = sig
	data, err := proto.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}

	_, _, _, err = ParseEnvelope(data, verifier)
	if !errors.Is(err, ErrStaleEnvelope) {
		t.Fatalf("expected ErrStaleEnvelope, got %v", err)
	}
}

func TestEnvelope_FutureSkewRejected(t *testing.T) {
	signer := &stubSigner{prefix: []byte("SIG:")}
	verifier := newStubVerifier("SIG:", "node-a")

	env := &endpointpb.Envelope{
		Type:      EnvelopeTypeRecord,
		Payload:   []byte("p"),
		SenderId:  "node-a",
		Timestamp: timestamppb.New(time.Now().Add(10 * time.Minute)),
	}
	sig, _ := signer.Sign(canonicalContent(env))
	env.Signature = sig
	data, _ := proto.Marshal(env)

	_, _, _, err := ParseEnvelope(data, verifier)
	if !errors.Is(err, ErrStaleEnvelope) {
		t.Fatalf("expected ErrStaleEnvelope, got %v", err)
	}
}

func TestEnvelope_UnknownTypeRejected(t *testing.T) {
	signer := &stubSigner{prefix: []byte("SIG:")}
	_, err := BuildEnvelope("bogus", []byte("p"), "node-a", signer)
	if !errors.Is(err, ErrUnknownEnvelopeType) {
		t.Fatalf("expected ErrUnknownEnvelopeType, got %v", err)
	}

	// Receiver should also reject unknown types.
	env := &endpointpb.Envelope{
		Type:      "bogus",
		Payload:   []byte("p"),
		SenderId:  "node-a",
		Timestamp: timestamppb.Now(),
	}
	sig, _ := signer.Sign(canonicalContent(env))
	env.Signature = sig
	data, _ := proto.Marshal(env)
	_, _, _, err = ParseEnvelope(data, newStubVerifier("SIG:", "node-a"))
	if !errors.Is(err, ErrUnknownEnvelopeType) {
		t.Fatalf("expected ErrUnknownEnvelopeType on parse, got %v", err)
	}
}

func TestEnvelope_NilVerifierSkipsCheck(t *testing.T) {
	signer := &stubSigner{prefix: []byte("SIG:")}
	data, err := BuildEnvelope(EnvelopeTypeRecord, []byte("p"), "node-a", signer)
	if err != nil {
		t.Fatal(err)
	}
	gotType, gotPayload, _, err := ParseEnvelope(data, nil)
	if err != nil {
		t.Fatalf("nil verifier should skip check: %v", err)
	}
	if gotType != EnvelopeTypeRecord || string(gotPayload) != "p" {
		t.Errorf("unexpected parse result: %q %q", gotType, gotPayload)
	}
}

func TestEnvelope_MalformedBytes(t *testing.T) {
	_, _, _, err := ParseEnvelope([]byte("not a proto"), nil)
	if !errors.Is(err, ErrMalformedEnvelope) {
		t.Fatalf("expected ErrMalformedEnvelope, got %v", err)
	}
}
