package orbit

import (
	"bytes"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	capsulePb "github.com/tareksalem/falak/capsule/proto/capsulepb"
)

// marshalForTest is a test helper to marshal proto messages.
func marshalForTest(msg proto.Message) ([]byte, error) {
	return proto.Marshal(msg)
}

func TestCanonicalContent_Deterministic(t *testing.T) {
	ts := timestamppb.New(time.Unix(1712505600, 0))

	msg1 := &capsulePb.OrbitMessage{
		Type:      "announcement",
		Payload:   []byte("payload1"),
		SenderId:  "node-a",
		Timestamp: ts,
	}
	msg2 := &capsulePb.OrbitMessage{
		Type:      "announcement",
		Payload:   []byte("payload1"),
		SenderId:  "node-a",
		Timestamp: ts,
	}

	c1 := canonicalContent(msg1)
	c2 := canonicalContent(msg2)

	if !bytes.Equal(c1, c2) {
		t.Error("canonicalContent should be deterministic for identical messages")
	}
}

func TestCanonicalContent_DifferentPayload(t *testing.T) {
	ts := timestamppb.New(time.Unix(1712505600, 0))

	msg1 := &capsulePb.OrbitMessage{
		Type: "x", Payload: []byte("a"), SenderId: "n1", Timestamp: ts,
	}
	msg2 := &capsulePb.OrbitMessage{
		Type: "x", Payload: []byte("b"), SenderId: "n1", Timestamp: ts,
	}

	if bytes.Equal(canonicalContent(msg1), canonicalContent(msg2)) {
		t.Error("different payloads should produce different canonical bytes")
	}
}

func TestCanonicalContent_LengthPrefixPreventsAmbiguity(t *testing.T) {
	ts := timestamppb.New(time.Unix(1712505600, 0))

	// Without length prefix, these would concatenate identically:
	// "ab" + "cd" vs "a" + "bcd"
	msg1 := &capsulePb.OrbitMessage{
		Type: "ab", Payload: []byte("cd"), SenderId: "", Timestamp: ts,
	}
	msg2 := &capsulePb.OrbitMessage{
		Type: "a", Payload: []byte("bcd"), SenderId: "", Timestamp: ts,
	}

	if bytes.Equal(canonicalContent(msg1), canonicalContent(msg2)) {
		t.Error("length-prefix should prevent ambiguity")
	}
}

// --- Mock Signer/Verifier ---

type mockSigner struct {
	prefix []byte
}

func (m *mockSigner) Sign(content []byte) ([]byte, error) {
	// "Sign" by prepending a fixed prefix.
	sig := make([]byte, 0, len(m.prefix)+len(content))
	sig = append(sig, m.prefix...)
	sig = append(sig, content...)
	return sig, nil
}

type mockVerifier struct {
	prefix  []byte
	allowed string // expected sender ID
}

func (m *mockVerifier) Verify(senderID string, content, signature []byte) bool {
	if senderID != m.allowed {
		return false
	}
	// Verify by checking the signature matches prefix+content.
	if len(signature) < len(m.prefix) {
		return false
	}
	if !bytes.Equal(signature[:len(m.prefix)], m.prefix) {
		return false
	}
	return bytes.Equal(signature[len(m.prefix):], content)
}

func TestAnnouncer_SignAndVerify_Roundtrip(t *testing.T) {
	signer := &mockSigner{prefix: []byte("SIG:")}
	verifier := &mockVerifier{prefix: []byte("SIG:"), allowed: "node-a"}

	// Create an announcer configured with signer/verifier.
	ann := &Announcer{
		nodeID:   "node-a",
		signer:   signer,
		verifier: verifier,
		dedup:    NewDeduplicator(),
	}

	// Build a signed message.
	data, err := ann.buildSignedMessage("announcement", []byte("payload-bytes"))
	if err != nil {
		t.Fatalf("buildSignedMessage failed: %v", err)
	}

	// Receiver side: decode the message and verify via HandleMessage.
	// We need to provide a CapsuleAnnouncement payload for HandleMessage to parse it.
	// Instead, test the verification path directly by checking a manually built message.

	// Use a fresh dedup to avoid duplicate rejection.
	recv := &Announcer{
		nodeID:   "node-b",
		verifier: verifier,
		dedup:    NewDeduplicator(),
	}

	// Build a real announcement payload
	annPb := &capsulePb.CapsuleAnnouncement{
		AnnouncingNode: "node-a",
	}
	annPayload, err := marshalForTest(annPb)
	if err != nil {
		t.Fatal(err)
	}
	data, err = ann.buildSignedMessage("announcement", annPayload)
	if err != nil {
		t.Fatal(err)
	}

	msgType, _, err := recv.HandleMessage(data)
	if err != nil {
		t.Fatalf("HandleMessage failed: %v", err)
	}
	if msgType != "announcement" {
		t.Errorf("expected announcement, got %s", msgType)
	}
}

func TestAnnouncer_RejectsInvalidSignature(t *testing.T) {
	verifier := &mockVerifier{prefix: []byte("SIG:"), allowed: "node-a"}

	recv := &Announcer{
		nodeID:   "node-b",
		verifier: verifier,
		dedup:    NewDeduplicator(),
	}

	// Build a message with bogus signature.
	annPb := &capsulePb.CapsuleAnnouncement{AnnouncingNode: "node-a"}
	annPayload, _ := marshalForTest(annPb)

	msg := &capsulePb.OrbitMessage{
		Type:      "announcement",
		Payload:   annPayload,
		SenderId:  "node-a",
		Timestamp: timestamppb.Now(),
		Signature: []byte("BOGUS:garbage"),
	}
	data, _ := marshalForTest(msg)

	_, _, err := recv.HandleMessage(data)
	if err == nil {
		t.Error("expected HandleMessage to reject invalid signature")
	}
}

func TestAnnouncer_RejectsMissingSignature(t *testing.T) {
	verifier := &mockVerifier{prefix: []byte("SIG:"), allowed: "node-a"}
	recv := &Announcer{
		nodeID:   "node-b",
		verifier: verifier,
		dedup:    NewDeduplicator(),
	}

	annPb := &capsulePb.CapsuleAnnouncement{AnnouncingNode: "node-a"}
	annPayload, _ := marshalForTest(annPb)

	msg := &capsulePb.OrbitMessage{
		Type:      "announcement",
		Payload:   annPayload,
		SenderId:  "node-a",
		Timestamp: timestamppb.Now(),
		// No signature
	}
	data, _ := marshalForTest(msg)

	_, _, err := recv.HandleMessage(data)
	if err == nil {
		t.Error("expected HandleMessage to reject missing signature")
	}
}

func TestAnnouncer_NoVerifier_SkipsCheck(t *testing.T) {
	// When no verifier is configured, signatures are not checked.
	recv := &Announcer{
		nodeID: "node-b",
		dedup:  NewDeduplicator(),
	}

	annPb := &capsulePb.CapsuleAnnouncement{AnnouncingNode: "node-a"}
	annPayload, _ := marshalForTest(annPb)

	msg := &capsulePb.OrbitMessage{
		Type:      "announcement",
		Payload:   annPayload,
		SenderId:  "node-a",
		Timestamp: timestamppb.Now(),
	}
	data, _ := marshalForTest(msg)

	msgType, _, err := recv.HandleMessage(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msgType != "announcement" {
		t.Errorf("expected announcement, got %s", msgType)
	}
}

func TestAnnouncer_RejectsStaleMessage(t *testing.T) {
	recv := &Announcer{nodeID: "node-b", dedup: NewDeduplicator()}

	annPb := &capsulePb.CapsuleAnnouncement{AnnouncingNode: "node-a"}
	annPayload, _ := marshalForTest(annPb)

	msg := &capsulePb.OrbitMessage{
		Type:      "announcement",
		Payload:   annPayload,
		SenderId:  "node-a",
		Timestamp: timestamppb.New(time.Now().Add(-1 * time.Hour)),
	}
	data, _ := marshalForTest(msg)

	_, _, err := recv.HandleMessage(data)
	if err == nil {
		t.Error("expected rejection for stale message")
	}
}
