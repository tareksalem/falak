package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	servicepb "github.com/tareksalem/falak/service/proto/servicepb"
)

// fakeTopic is an in-memory publisherTopic used to capture publish calls.
type fakeTopic struct {
	mu       sync.Mutex
	messages [][]byte
}

func (f *fakeTopic) Publish(_ context.Context, data []byte, _ ...pubsub.PubOpt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make([]byte, len(data))
	copy(c, data)
	f.messages = append(f.messages, c)
	return nil
}

func (f *fakeTopic) snapshot() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.messages))
	copy(out, f.messages)
	return out
}

// recordingSink captures Manager-like callbacks from the Subscriber.
type recordingSink struct {
	mu          sync.Mutex
	received    []*Service
	withdrawals []ServiceID
	receiveErr  error
}

func (r *recordingSink) Receive(svc *Service) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.receiveErr != nil {
		return r.receiveErr
	}
	r.received = append(r.received, svc)
	return nil
}

func (r *recordingSink) HandleWithdrawal(id ServiceID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.withdrawals = append(r.withdrawals, id)
	return nil
}

func (r *recordingSink) snapshot() ([]*Service, []ServiceID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rcv := append([]*Service(nil), r.received...)
	wd := append([]ServiceID(nil), r.withdrawals...)
	return rcv, wd
}

type stubSigner struct {
	tag []byte
	err error
}

func (s *stubSigner) Sign(content []byte) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	// Make signature content-dependent so verifier can match it.
	out := make([]byte, 0, len(s.tag)+len(content))
	out = append(out, s.tag...)
	out = append(out, content...)
	return out, nil
}

type stubVerifier struct{ tag []byte }

func (v *stubVerifier) Verify(_ string, content, signature []byte) bool {
	if len(signature) < len(v.tag) {
		return false
	}
	if string(signature[:len(v.tag)]) != string(v.tag) {
		return false
	}
	return string(signature[len(v.tag):]) == string(content)
}

func sampleService() *Service {
	return &Service{
		ID:        "svc-1",
		ClusterID: "c1",
		Status:    ServiceStatusEnum.Active(),
		Spec: ServiceSpec{
			Name:     "payments",
			Ports:    []ServicePort{{Name: "http", Port: 8080, Protocol: ProtocolEnum.TCP()}},
			Backends: []ServiceBackend{{Capsule: "payments-v1", Weight: 100}},
			Strategy: &Strategy{Type: StrategyTypeEnum.Static()},
		},
	}
}

func TestPublisher_PublishUpdate_RoundTrip(t *testing.T) {
	topic := &fakeTopic{}
	sink := &recordingSink{}
	signer := &stubSigner{tag: []byte("sig:")}
	verifier := &stubVerifier{tag: []byte("sig:")}

	p := NewPublisher(topic,
		WithPublisherSigner(signer),
		WithPublisherNodeID("node-A"),
	)
	if err := p.PublishUpdate(context.Background(), sampleService()); err != nil {
		t.Fatalf("PublishUpdate: %v", err)
	}
	msgs := topic.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	sub := NewSubscriber(nil, sink, WithSubscriberVerifier(verifier))
	if err := sub.Handle(msgs[0]); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	rcv, _ := sink.snapshot()
	if len(rcv) != 1 || rcv[0].Spec.Name != "payments" {
		t.Errorf("did not round-trip: got %+v", rcv)
	}
}

func TestPublisher_PublishWithdrawal_RoundTrip(t *testing.T) {
	topic := &fakeTopic{}
	sink := &recordingSink{}
	signer := &stubSigner{tag: []byte("sig:")}
	verifier := &stubVerifier{tag: []byte("sig:")}

	p := NewPublisher(topic, WithPublisherSigner(signer), WithPublisherNodeID("node-A"))
	if err := p.PublishWithdrawal(context.Background(), "svc-99"); err != nil {
		t.Fatalf("PublishWithdrawal: %v", err)
	}
	msgs := topic.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	sub := NewSubscriber(nil, sink, WithSubscriberVerifier(verifier))
	if err := sub.Handle(msgs[0]); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	_, wd := sink.snapshot()
	if len(wd) != 1 || wd[0] != "svc-99" {
		t.Errorf("withdrawal not delivered: got %+v", wd)
	}
}

func TestSubscriber_RejectsBadSignature(t *testing.T) {
	topic := &fakeTopic{}
	sink := &recordingSink{}
	signer := &stubSigner{tag: []byte("good:")}
	verifier := &stubVerifier{tag: []byte("other:")}

	p := NewPublisher(topic, WithPublisherSigner(signer), WithPublisherNodeID("node-A"))
	if err := p.PublishUpdate(context.Background(), sampleService()); err != nil {
		t.Fatalf("PublishUpdate: %v", err)
	}
	msgs := topic.snapshot()
	sub := NewSubscriber(nil, sink, WithSubscriberVerifier(verifier))
	err := sub.Handle(msgs[0])
	if err == nil || !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
	rcv, _ := sink.snapshot()
	if len(rcv) != 0 {
		t.Errorf("sink should not have received: %+v", rcv)
	}
}

func TestSubscriber_RejectsStaleEnvelope(t *testing.T) {
	sink := &recordingSink{}
	sub := NewSubscriber(nil, sink, WithSubscriberMaxAge(100*time.Millisecond))

	// Build a stale envelope manually.
	payload, err := proto.Marshal(&servicepb.ServiceUpdate{Service: serviceToProto(sampleService())})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := &servicepb.Envelope{
		Type:      EnvelopeTypeUpdate,
		Payload:   payload,
		SenderId:  "node-A",
		Timestamp: timestamppb.New(time.Now().Add(-time.Hour)),
	}
	data, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	err = sub.Handle(data)
	if err == nil || !errors.Is(err, ErrStaleEnvelope) {
		t.Fatalf("expected ErrStaleEnvelope, got %v", err)
	}
}

// trackedSubscription drives the Subscriber loop with controllable Next
// behaviour. Cancel must signal Next to return an error.
type trackedSubscription struct {
	nextCalls atomic.Int32
	canceled  chan struct{}
	once      sync.Once
}

func newTrackedSubscription() *trackedSubscription {
	return &trackedSubscription{canceled: make(chan struct{})}
}

func (t *trackedSubscription) Next(ctx context.Context) (*pubsub.Message, error) {
	t.nextCalls.Add(1)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.canceled:
		return nil, errors.New("subscription canceled")
	}
}

func (t *trackedSubscription) Cancel() {
	t.once.Do(func() { close(t.canceled) })
}

func TestSubscriber_StopCancelsCleanly(t *testing.T) {
	sub := NewSubscriber(newTrackedSubscription(), &recordingSink{})
	sub.Start(context.Background())
	done := make(chan struct{})
	go func() {
		sub.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Subscriber.Stop did not return within 2s")
	}
}

// activeProviderFunc adapts a function to the activeProvider interface.
type activeProviderFunc func() []*Service

func (f activeProviderFunc) ActiveServices() []*Service { return f() }

func TestPublisher_StopCancelsRepublishLoop(t *testing.T) {
	topic := &fakeTopic{}
	called := atomic.Int32{}
	provider := activeProviderFunc(func() []*Service {
		called.Add(1)
		return nil
	})
	p := NewPublisher(topic,
		WithPublisherNodeID("node-A"),
		WithPublisherRepublishInterval(20*time.Millisecond),
		WithPublisherActiveProvider(provider),
	)
	p.Start(context.Background())
	time.Sleep(80 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publisher.Stop did not return within 2s")
	}
	if called.Load() == 0 {
		t.Error("republish provider was never called")
	}
}
