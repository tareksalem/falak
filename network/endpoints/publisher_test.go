package endpoints

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
)

// sampleRecord returns a minimal-but-valid EndpointRecord for tests.
func sampleRecord(replicaID, state string) *endpointpb.EndpointRecord {
	return &endpointpb.EndpointRecord{
		ClusterPath: "c1",
		GroupId:     "g1",
		CapsuleName: "api",
		ReplicaId:   replicaID,
		BridgeIp:    "10.0.0.5",
		SwimState:   state,
		NamedPorts: []*endpointpb.NamedPort{
			{Name: "http", ContainerPort: 8080, Protocol: "tcp"},
		},
		TtlSeconds: 30,
	}
}

func TestPublisher_PublishWritesToTopic(t *testing.T) {
	ps := newFakePubSub()
	signer := &stubSigner{prefix: []byte("SIG:")}
	verifier := newStubVerifier("SIG:", "node-a")
	pub := NewPublisher(
		WithPubSub(ps),
		WithSigner(signer),
		WithLocalNodeID("node-a"),
		WithTTL(300*time.Millisecond),
		WithRefreshDivisor(3),
	)
	defer pub.Stop()

	if err := pub.Publish(context.Background(), sampleRecord("r1", SwimStateAlive)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	topic := BuildTopic("c1", "g1")
	pubs := ps.publishedFor(topic)
	if len(pubs) != 1 {
		t.Fatalf("expected 1 published msg on %q, got %d", topic, len(pubs))
	}

	rt, payload, sender, err := ParseEnvelope(pubs[0], verifier)
	if err != nil {
		t.Fatalf("envelope parse: %v", err)
	}
	if rt != EnvelopeTypeRecord || sender != "node-a" {
		t.Errorf("unexpected envelope: type=%q sender=%q", rt, sender)
	}
	var rec endpointpb.EndpointRecord
	if err := proto.Unmarshal(payload, &rec); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if rec.ReplicaId != "r1" || rec.NodeId != "node-a" {
		t.Errorf("unexpected payload: %+v", &rec)
	}
}

func TestPublisher_MissingFieldsRejected(t *testing.T) {
	ps := newFakePubSub()
	pub := NewPublisher(
		WithPubSub(ps),
		WithSigner(&stubSigner{prefix: []byte("S:")}),
		WithLocalNodeID("node-a"),
	)
	defer pub.Stop()

	err := pub.Publish(context.Background(), &endpointpb.EndpointRecord{ReplicaId: "r1"})
	if err == nil {
		t.Fatal("expected error for missing fields")
	}
}

func TestPublisher_RefreshRepublishes(t *testing.T) {
	ps := newFakePubSub()
	pub := NewPublisher(
		WithPubSub(ps),
		WithSigner(&stubSigner{prefix: []byte("S:")}),
		WithLocalNodeID("node-a"),
		WithTTL(150*time.Millisecond),
		WithRefreshDivisor(3),
	)
	defer pub.Stop()

	if err := pub.Publish(context.Background(), sampleRecord("r1", SwimStateAlive)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// First publish is synchronous; refresh interval is 50ms. Wait
	// 250ms and we expect at least 3 publishes total (1 initial + 4-5 ticks).
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := len(ps.publishedFor(BuildTopic("c1", "g1"))); got >= 3 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected >=3 publishes after refresh window, got %d",
		len(ps.publishedFor(BuildTopic("c1", "g1"))))
}

func TestPublisher_UpdateStateMutatesAndRepublishes(t *testing.T) {
	ps := newFakePubSub()
	verifier := newStubVerifier("S:", "node-a")
	pub := NewPublisher(
		WithPubSub(ps),
		WithSigner(&stubSigner{prefix: []byte("S:")}),
		WithLocalNodeID("node-a"),
		WithTTL(time.Hour), // freeze refresh out of the picture
	)
	defer pub.Stop()

	if err := pub.Publish(context.Background(), sampleRecord("r1", SwimStateAlive)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := pub.UpdateState(context.Background(), "r1", "suspect"); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	topic := BuildTopic("c1", "g1")
	pubs := ps.publishedFor(topic)
	if len(pubs) != 2 {
		t.Fatalf("expected 2 publishes (initial + state change), got %d", len(pubs))
	}
	_, payload, _, err := ParseEnvelope(pubs[1], verifier)
	if err != nil {
		t.Fatalf("parse 2nd envelope: %v", err)
	}
	var rec endpointpb.EndpointRecord
	if err := proto.Unmarshal(payload, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.SwimState != "suspect" {
		t.Errorf("swim_state = %q, want suspect", rec.SwimState)
	}
}

func TestPublisher_UpdateStateUnknownReplica(t *testing.T) {
	ps := newFakePubSub()
	pub := NewPublisher(
		WithPubSub(ps),
		WithSigner(&stubSigner{prefix: []byte("S:")}),
		WithLocalNodeID("node-a"),
	)
	defer pub.Stop()
	err := pub.UpdateState(context.Background(), "nope", "suspect")
	if err == nil {
		t.Fatal("expected error for unknown replica")
	}
}

func TestPublisher_UpdateStateIdempotent(t *testing.T) {
	ps := newFakePubSub()
	pub := NewPublisher(
		WithPubSub(ps),
		WithSigner(&stubSigner{prefix: []byte("S:")}),
		WithLocalNodeID("node-a"),
		WithTTL(time.Hour),
	)
	defer pub.Stop()

	_ = pub.Publish(context.Background(), sampleRecord("r1", SwimStateAlive))
	// Same state — should not republish.
	if err := pub.UpdateState(context.Background(), "r1", SwimStateAlive); err != nil {
		t.Fatal(err)
	}
	if got := len(ps.publishedFor(BuildTopic("c1", "g1"))); got != 1 {
		t.Errorf("expected 1 publish, got %d", got)
	}
}

func TestPublisher_WithdrawCancelsAndAnnounces(t *testing.T) {
	ps := newFakePubSub()
	verifier := newStubVerifier("S:", "node-a")
	pub := NewPublisher(
		WithPubSub(ps),
		WithSigner(&stubSigner{prefix: []byte("S:")}),
		WithLocalNodeID("node-a"),
		WithTTL(80*time.Millisecond),
		WithRefreshDivisor(2),
	)
	defer pub.Stop()

	_ = pub.Publish(context.Background(), sampleRecord("r1", SwimStateAlive))

	// Wait one refresh tick so we know the refresh loop is running.
	time.Sleep(80 * time.Millisecond)

	if err := pub.Withdraw(context.Background(), "c1", "g1", "api", "r1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	// Capture the count after Withdraw — should not grow further.
	topic := BuildTopic("c1", "g1")
	count := len(ps.publishedFor(topic))
	time.Sleep(150 * time.Millisecond)
	if got := len(ps.publishedFor(topic)); got != count {
		t.Errorf("publishes grew after Withdraw: was %d, now %d", count, got)
	}

	// The most recent published payload should be a withdrawal.
	pubs := ps.publishedFor(topic)
	rt, _, _, err := ParseEnvelope(pubs[len(pubs)-1], verifier)
	if err != nil {
		t.Fatalf("parse last envelope: %v", err)
	}
	if rt != EnvelopeTypeWithdrawal {
		t.Errorf("expected last envelope to be withdrawal, got %q", rt)
	}
}

func TestPublisher_WithdrawUnknownIsNoop(t *testing.T) {
	ps := newFakePubSub()
	pub := NewPublisher(
		WithPubSub(ps),
		WithSigner(&stubSigner{prefix: []byte("S:")}),
		WithLocalNodeID("node-a"),
	)
	defer pub.Stop()
	if err := pub.Withdraw(context.Background(), "c1", "g1", "api", "ghost"); err != nil {
		t.Fatalf("withdraw of unknown should be no-op, got %v", err)
	}
	if got := len(ps.publishedFor(BuildTopic("c1", "g1"))); got != 0 {
		t.Errorf("expected 0 publishes, got %d", got)
	}
}

func TestPublisher_PublishReplacesPreviousRecord(t *testing.T) {
	ps := newFakePubSub()
	pub := NewPublisher(
		WithPubSub(ps),
		WithSigner(&stubSigner{prefix: []byte("S:")}),
		WithLocalNodeID("node-a"),
		WithTTL(time.Hour),
	)
	defer pub.Stop()

	if err := pub.Publish(context.Background(), sampleRecord("r1", SwimStateAlive)); err != nil {
		t.Fatal(err)
	}
	// Publish again with the same replicaID — should replace cleanly.
	r2 := sampleRecord("r1", SwimStateAlive)
	r2.BridgeIp = "10.0.0.99"
	if err := pub.Publish(context.Background(), r2); err != nil {
		t.Fatalf("re-publish: %v", err)
	}

	pubs := ps.publishedFor(BuildTopic("c1", "g1"))
	if len(pubs) != 2 {
		t.Fatalf("expected 2 publishes, got %d", len(pubs))
	}
}

func TestPublisher_StopCancelsEverything(t *testing.T) {
	ps := newFakePubSub()
	pub := NewPublisher(
		WithPubSub(ps),
		WithSigner(&stubSigner{prefix: []byte("S:")}),
		WithLocalNodeID("node-a"),
		WithTTL(60*time.Millisecond),
		WithRefreshDivisor(2),
		WithStopGracePeriod(time.Second),
	)

	_ = pub.Publish(context.Background(), sampleRecord("r1", SwimStateAlive))
	_ = pub.Publish(context.Background(), sampleRecord("r2", SwimStateAlive))

	// Run the refresh loops for a beat so there are actual goroutines to stop.
	time.Sleep(80 * time.Millisecond)
	pub.Stop()

	// After Stop, Publish must fail.
	if err := pub.Publish(context.Background(), sampleRecord("r3", SwimStateAlive)); err == nil {
		t.Error("expected Publish after Stop to fail")
	}

	// Refresh should no longer add publishes.
	count := len(ps.publishedFor(BuildTopic("c1", "g1")))
	time.Sleep(120 * time.Millisecond)
	if got := len(ps.publishedFor(BuildTopic("c1", "g1"))); got != count {
		t.Errorf("publishes grew after Stop: was %d now %d", count, got)
	}

	// Stop is idempotent.
	pub.Stop()
}

func TestPublisher_InitialPublishErrorBubblesUp(t *testing.T) {
	ps := newFakePubSub()
	pub := NewPublisher(
		WithPubSub(ps),
		WithSigner(&stubSigner{prefix: []byte("S:")}),
		WithLocalNodeID("node-a"),
	)
	defer pub.Stop()

	sentinel := errors.New("inject")
	ps.injectPublishError(BuildTopic("c1", "g1"), sentinel)
	err := pub.Publish(context.Background(), sampleRecord("r1", SwimStateAlive))
	if err == nil {
		t.Fatal("expected first publish to surface injected error")
	}
}
