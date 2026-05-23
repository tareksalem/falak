package endpoints

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
)

// waitUntil polls cond until true or the deadline elapses. Test helper.
func waitUntil(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestSubscriber_DeliversRecordToRegistry(t *testing.T) {
	ps := newFakePubSub()
	reg := NewRegistry(WithSweepInterval(time.Hour))
	defer reg.Stop()
	verifier := newStubVerifier("S:", "node-a")
	signer := &stubSigner{prefix: []byte("S:")}

	sub := NewSubscriber(
		WithSubscriberPubSub(ps),
		WithVerifier(verifier),
		WithRegistry(reg),
		WithSubscriberLocalNodeID("node-b"),
	)
	defer sub.Stop()

	if err := sub.Subscribe(context.Background(), "c1", "g1"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	rec := &endpointpb.EndpointRecord{
		ClusterPath: "c1", GroupId: "g1", CapsuleName: "api", ReplicaId: "r1",
		NodeId: "node-a", BridgeIp: "10.0.0.5", SwimState: SwimStateAlive,
		EmittedAt:  timestamppb.Now(),
		TtlSeconds: 30,
	}
	payload, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	data, err := BuildEnvelope(EnvelopeTypeRecord, payload, "node-a", signer)
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.Publish(context.Background(), BuildTopic("c1", "g1"), data); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, func() bool { return reg.Len() == 1 }, time.Second)
	eps := reg.Lookup("c1", "g1", "api")
	if len(eps) != 1 || eps[0].BridgeIP != "10.0.0.5" {
		t.Errorf("registry missing expected endpoint: %+v", eps)
	}
}

func TestSubscriber_HandlesWithdrawal(t *testing.T) {
	ps := newFakePubSub()
	reg := NewRegistry(WithSweepInterval(time.Hour))
	defer reg.Stop()
	verifier := newStubVerifier("S:", "node-a")
	signer := &stubSigner{prefix: []byte("S:")}

	sub := NewSubscriber(
		WithSubscriberPubSub(ps),
		WithVerifier(verifier),
		WithRegistry(reg),
		WithSubscriberLocalNodeID("node-b"),
	)
	defer sub.Stop()

	if err := sub.Subscribe(context.Background(), "c1", "g1"); err != nil {
		t.Fatal(err)
	}

	// Pre-populate via direct insert.
	reg.Insert(pbRecord("c1", "g1", "api", "r1", SwimStateAlive, time.Now(), 30*time.Second))

	w := &endpointpb.EndpointWithdrawal{
		ClusterPath: "c1", GroupId: "g1", CapsuleName: "api", ReplicaId: "r1",
		NodeId: "node-a", EmittedAt: timestamppb.Now(),
	}
	payload, _ := proto.Marshal(w)
	data, err := BuildEnvelope(EnvelopeTypeWithdrawal, payload, "node-a", signer)
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.Publish(context.Background(), BuildTopic("c1", "g1"), data); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return reg.Len() == 0 }, time.Second)
}

func TestSubscriber_BadSignatureDropped(t *testing.T) {
	ps := newFakePubSub()
	reg := NewRegistry(WithSweepInterval(time.Hour))
	defer reg.Stop()
	verifier := newStubVerifier("S:", "node-a") // only node-a allowed

	sub := NewSubscriber(
		WithSubscriberPubSub(ps),
		WithVerifier(verifier),
		WithRegistry(reg),
		WithSubscriberLocalNodeID("node-b"),
	)
	defer sub.Stop()

	if err := sub.Subscribe(context.Background(), "c1", "g1"); err != nil {
		t.Fatal(err)
	}

	// Sign with the right key but claim sender is node-c — verifier rejects.
	signer := &stubSigner{prefix: []byte("S:")}
	rec := &endpointpb.EndpointRecord{
		ClusterPath: "c1", GroupId: "g1", CapsuleName: "api", ReplicaId: "r1",
		SwimState: SwimStateAlive,
	}
	payload, _ := proto.Marshal(rec)
	bad, err := BuildEnvelope(EnvelopeTypeRecord, payload, "node-c", signer)
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.Publish(context.Background(), BuildTopic("c1", "g1"), bad); err != nil {
		t.Fatal(err)
	}

	time.Sleep(80 * time.Millisecond)
	if reg.Len() != 0 {
		t.Errorf("bad-signature envelope reached registry: len=%d", reg.Len())
	}
}

func TestSubscriber_SelfLoopFiltered(t *testing.T) {
	ps := newFakePubSub()
	reg := NewRegistry(WithSweepInterval(time.Hour))
	defer reg.Stop()
	verifier := newStubVerifier("S:", "node-a")
	signer := &stubSigner{prefix: []byte("S:")}

	sub := NewSubscriber(
		WithSubscriberPubSub(ps),
		WithVerifier(verifier),
		WithRegistry(reg),
		WithSubscriberLocalNodeID("node-a"), // same as sender
	)
	defer sub.Stop()
	if err := sub.Subscribe(context.Background(), "c1", "g1"); err != nil {
		t.Fatal(err)
	}

	rec := &endpointpb.EndpointRecord{
		ClusterPath: "c1", GroupId: "g1", CapsuleName: "api", ReplicaId: "r1",
		SwimState: SwimStateAlive,
	}
	payload, _ := proto.Marshal(rec)
	data, _ := BuildEnvelope(EnvelopeTypeRecord, payload, "node-a", signer)
	_ = ps.Publish(context.Background(), BuildTopic("c1", "g1"), data)

	time.Sleep(80 * time.Millisecond)
	if reg.Len() != 0 {
		t.Errorf("self-loop reached registry: len=%d", reg.Len())
	}
}

func TestSubscriber_SubscribeIdempotent(t *testing.T) {
	ps := newFakePubSub()
	reg := NewRegistry(WithSweepInterval(time.Hour))
	defer reg.Stop()
	sub := NewSubscriber(
		WithSubscriberPubSub(ps),
		WithRegistry(reg),
	)
	defer sub.Stop()

	if err := sub.Subscribe(context.Background(), "c1", "g1"); err != nil {
		t.Fatal(err)
	}
	// Second subscribe must not fail and must not create a second goroutine.
	if err := sub.Subscribe(context.Background(), "c1", "g1"); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriber_UnsubscribeAndStop(t *testing.T) {
	ps := newFakePubSub()
	reg := NewRegistry(WithSweepInterval(time.Hour))
	defer reg.Stop()
	sub := NewSubscriber(
		WithSubscriberPubSub(ps),
		WithRegistry(reg),
		WithSubscriberStopGracePeriod(time.Second),
	)

	if err := sub.Subscribe(context.Background(), "c1", "g1"); err != nil {
		t.Fatal(err)
	}
	if err := sub.Unsubscribe(context.Background(), "c1", "g1"); err != nil {
		t.Fatal(err)
	}
	// Unsubscribe of unknown is fine.
	if err := sub.Unsubscribe(context.Background(), "c1", "g1"); err != nil {
		t.Fatal(err)
	}

	if err := sub.Subscribe(context.Background(), "c1", "g2"); err != nil {
		t.Fatal(err)
	}
	sub.Stop()
	sub.Stop() // idempotent

	// After Stop, Subscribe should fail.
	if err := sub.Subscribe(context.Background(), "c1", "g3"); err == nil {
		t.Error("expected Subscribe after Stop to fail")
	}
}

func TestSubscriber_MissingPubSubRejected(t *testing.T) {
	reg := NewRegistry()
	defer reg.Stop()
	sub := NewSubscriber(WithRegistry(reg))
	if err := sub.Subscribe(context.Background(), "c1", "g1"); err == nil {
		t.Error("expected error for missing pubsub")
	}
}

func TestSubscriber_MissingRegistryRejected(t *testing.T) {
	sub := NewSubscriber(WithSubscriberPubSub(newFakePubSub()))
	if err := sub.Subscribe(context.Background(), "c1", "g1"); err == nil {
		t.Error("expected error for missing registry")
	}
}
