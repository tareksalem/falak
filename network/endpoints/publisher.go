package endpoints

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	endpointpb "github.com/tareksalem/falak/network/proto/endpointpb"
)

// PubSub is the minimal pubsub surface the publisher and subscriber
// depend on. Production wires this to a libp2p gossipsub adapter; tests
// inject an in-memory fake (see fakepubsub_test.go).
type PubSub interface {
	// Publish blocks while the topic accepts the bytes, then returns
	// any error from the underlying transport.
	Publish(ctx context.Context, topic string, data []byte) error
	// Subscribe returns a channel that yields every message received
	// on topic. The channel is closed when ctx is canceled.
	Subscribe(ctx context.Context, topic string) (<-chan []byte, error)
}

// Defaults for the publisher. Documented as constants so plan readers
// can sanity-check them without diving into option code.
const (
	// DefaultPublisherTTL is the wire TTL advertised in every record.
	// Subscribers evict records older than 2 × TTL when no withdrawal
	// arrives, so this also bounds how long a missing peer haunts the
	// registry.
	DefaultPublisherTTL = 30 * time.Second
	// DefaultPublisherRefreshDivisor controls the refresh cadence: the
	// publisher re-publishes every (TTL / refreshDivisor) seconds.
	// Default 3 → refresh every 10s for a 30s TTL.
	DefaultPublisherRefreshDivisor = 3
	// DefaultPublisherStopGrace is the maximum time Stop blocks waiting
	// for outstanding refresh goroutines to exit before returning.
	DefaultPublisherStopGrace = 5 * time.Second
)

// Publisher owns one refresh goroutine per actively published record.
// Each goroutine reissues the EndpointRecord on the per-group topic
// every TTL/refreshDivisor seconds until the record is withdrawn or
// Stop is called.
type Publisher struct {
	pubsub          PubSub
	signer          Signer
	logger          *zap.Logger
	localNodeID     string
	ttl             time.Duration
	refreshDivisor  int
	stopGracePeriod time.Duration

	mu      sync.Mutex
	records map[string]*publishedRecord // keyed by recordKey()
	wg      sync.WaitGroup
	closed  bool
}

// publishedRecord is the live state for one replica announcement.
type publishedRecord struct {
	record *endpointpb.EndpointRecord
	cancel context.CancelFunc
	done   chan struct{} // closed when the refresh goroutine exits
}

// PublisherOption configures a Publisher.
type PublisherOption func(*Publisher)

// WithPubSub sets the underlying pubsub transport.
func WithPubSub(p PubSub) PublisherOption {
	return func(pb *Publisher) { pb.pubsub = p }
}

// WithSigner sets the envelope signer.
func WithSigner(s Signer) PublisherOption {
	return func(pb *Publisher) { pb.signer = s }
}

// WithLogger sets the zap logger.
func WithLogger(l *zap.Logger) PublisherOption {
	return func(pb *Publisher) { pb.logger = l }
}

// WithLocalNodeID sets the publisher's own node ID; embedded in every
// envelope so receivers can look up the verification key.
func WithLocalNodeID(id string) PublisherOption {
	return func(pb *Publisher) { pb.localNodeID = id }
}

// WithTTL sets the wire TTL advertised in each record.
func WithTTL(d time.Duration) PublisherOption {
	return func(pb *Publisher) { pb.ttl = d }
}

// WithRefreshDivisor sets the refresh cadence: refresh every
// TTL / refreshDivisor. Values < 1 fall back to the default.
func WithRefreshDivisor(n int) PublisherOption {
	return func(pb *Publisher) { pb.refreshDivisor = n }
}

// WithStopGracePeriod bounds how long Stop waits for refresh
// goroutines to exit.
func WithStopGracePeriod(d time.Duration) PublisherOption {
	return func(pb *Publisher) { pb.stopGracePeriod = d }
}

// NewPublisher constructs a Publisher with the given options.
// PubSub, Signer, and LocalNodeID are required for production use; the
// constructor itself never returns an error so options can be chained
// freely in tests.
func NewPublisher(opts ...PublisherOption) *Publisher {
	p := &Publisher{
		logger:          zap.NewNop(),
		ttl:             DefaultPublisherTTL,
		refreshDivisor:  DefaultPublisherRefreshDivisor,
		stopGracePeriod: DefaultPublisherStopGrace,
		records:         make(map[string]*publishedRecord),
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.refreshDivisor < 1 {
		p.refreshDivisor = DefaultPublisherRefreshDivisor
	}
	if p.ttl <= 0 {
		p.ttl = DefaultPublisherTTL
	}
	if p.stopGracePeriod <= 0 {
		p.stopGracePeriod = DefaultPublisherStopGrace
	}
	return p
}

// Publish sends the given EndpointRecord on the per-group topic and
// starts a refresh goroutine that republishes it every TTL/divisor
// until Withdraw or Stop. Re-publishing the same recordKey replaces
// the existing entry (the old refresh goroutine is canceled cleanly).
//
// The publisher takes ownership of a deep clone of record so the
// caller may mutate or discard its copy after Publish returns.
func (p *Publisher) Publish(ctx context.Context, record *endpointpb.EndpointRecord) error {
	if p.pubsub == nil {
		return errors.New("endpoints: publisher missing pubsub")
	}
	if record == nil {
		return errors.New("endpoints: nil record")
	}
	if record.ReplicaId == "" || record.CapsuleName == "" || record.GroupId == "" || record.ClusterPath == "" {
		return errors.New("endpoints: incomplete record (cluster/group/capsule/replica required)")
	}

	rec := proto.Clone(record).(*endpointpb.EndpointRecord)
	rec.NodeId = p.localNodeID
	if rec.TtlSeconds == 0 {
		rec.TtlSeconds = int32(p.ttl.Seconds())
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("endpoints: publisher closed")
	}
	key := recordKey(rec.ClusterPath, rec.GroupId, rec.CapsuleName, rec.ReplicaId)
	if existing, ok := p.records[key]; ok {
		existing.cancel()
		p.mu.Unlock()
		<-existing.done
		p.mu.Lock()
	}

	refreshCtx, cancel := context.WithCancel(context.Background())
	pr := &publishedRecord{
		record: rec,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	p.records[key] = pr
	p.mu.Unlock()

	// Publish synchronously once so the caller sees an error on first
	// failure; the refresh goroutine handles transient errors after.
	if err := p.send(ctx, rec); err != nil {
		p.mu.Lock()
		delete(p.records, key)
		p.mu.Unlock()
		cancel()
		close(pr.done)
		return err
	}

	p.wg.Add(1)
	go p.refreshLoop(refreshCtx, key, pr)

	p.logger.Debug("endpoint record published",
		zap.String("cluster", rec.ClusterPath),
		zap.String("group", rec.GroupId),
		zap.String("capsule", rec.CapsuleName),
		zap.String("replica", rec.ReplicaId),
		zap.String("swim_state", rec.SwimState))
	return nil
}

// UpdateState mutates the swim_state of the record currently tracked
// for replicaID and republishes immediately. Idempotent if the state
// is unchanged. Returns an error if no record matches replicaID.
func (p *Publisher) UpdateState(ctx context.Context, replicaID, state string) error {
	if replicaID == "" {
		return errors.New("endpoints: empty replicaID")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("endpoints: publisher closed")
	}
	var found *publishedRecord
	for _, pr := range p.records {
		if pr.record.ReplicaId == replicaID {
			found = pr
			break
		}
	}
	if found == nil {
		p.mu.Unlock()
		return fmt.Errorf("endpoints: no record for replica %q", replicaID)
	}
	if found.record.SwimState == state {
		p.mu.Unlock()
		return nil
	}
	found.record.SwimState = state
	found.record.EmittedAt = timestamppb.Now()
	rec := proto.Clone(found.record).(*endpointpb.EndpointRecord)
	p.mu.Unlock()

	if err := p.send(ctx, rec); err != nil {
		return fmt.Errorf("endpoints: republish on state change: %w", err)
	}
	p.logger.Debug("endpoint state updated",
		zap.String("replica", replicaID),
		zap.String("swim_state", state))
	return nil
}

// Withdraw publishes an EndpointWithdrawal for (cluster,group,capsule,
// replica) and cancels the refresh goroutine for that record. Calling
// Withdraw on an unknown replicaID is a no-op (idempotent) — useful for
// crash-restart paths that may double-fire teardown.
func (p *Publisher) Withdraw(ctx context.Context, clusterPath, groupID, capsuleName, replicaID string) error {
	if p.pubsub == nil {
		return errors.New("endpoints: publisher missing pubsub")
	}
	key := recordKey(clusterPath, groupID, capsuleName, replicaID)

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("endpoints: publisher closed")
	}
	pr, ok := p.records[key]
	if !ok {
		p.mu.Unlock()
		return nil // idempotent
	}
	delete(p.records, key)
	pr.cancel()
	p.mu.Unlock()

	withdrawal := &endpointpb.EndpointWithdrawal{
		ClusterPath: clusterPath,
		GroupId:     groupID,
		CapsuleName: capsuleName,
		ReplicaId:   replicaID,
		NodeId:      p.localNodeID,
		EmittedAt:   timestamppb.Now(),
	}
	payload, err := proto.Marshal(withdrawal)
	if err != nil {
		return fmt.Errorf("endpoints: marshal withdrawal: %w", err)
	}
	data, err := BuildEnvelope(EnvelopeTypeWithdrawal, payload, p.localNodeID, p.signer)
	if err != nil {
		return err
	}
	if err := p.pubsub.Publish(ctx, BuildTopic(clusterPath, groupID), data); err != nil {
		return fmt.Errorf("endpoints: publish withdrawal: %w", err)
	}
	<-pr.done
	p.logger.Info("endpoint withdrawn",
		zap.String("cluster", clusterPath),
		zap.String("group", groupID),
		zap.String("capsule", capsuleName),
		zap.String("replica", replicaID))
	return nil
}

// Stop cancels every refresh goroutine and waits up to the configured
// grace period for them to exit. Subsequent calls to Publish return an
// error. Stop is idempotent — second and later calls return nil.
func (p *Publisher) Stop() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	for _, pr := range p.records {
		pr.cancel()
	}
	p.records = make(map[string]*publishedRecord)
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(p.stopGracePeriod):
		p.logger.Warn("endpoint publisher stop timed out",
			zap.Duration("grace", p.stopGracePeriod))
	}
}

// send marshals + signs + publishes one record. Used by Publish, the
// refresh loop, and UpdateState.
func (p *Publisher) send(ctx context.Context, rec *endpointpb.EndpointRecord) error {
	rec.EmittedAt = timestamppb.Now()
	payload, err := proto.Marshal(rec)
	if err != nil {
		return fmt.Errorf("endpoints: marshal record: %w", err)
	}
	data, err := BuildEnvelope(EnvelopeTypeRecord, payload, p.localNodeID, p.signer)
	if err != nil {
		return err
	}
	if err := p.pubsub.Publish(ctx, BuildTopic(rec.ClusterPath, rec.GroupId), data); err != nil {
		return fmt.Errorf("endpoints: publish record: %w", err)
	}
	return nil
}

// refreshLoop reissues the record every (TTL/refreshDivisor) until ctx
// is canceled. Errors are logged but do not exit the loop — a transient
// pubsub error must not silently drop the announcement.
func (p *Publisher) refreshLoop(ctx context.Context, key string, pr *publishedRecord) {
	defer p.wg.Done()
	defer close(pr.done)

	interval := p.ttl / time.Duration(p.refreshDivisor)
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				return
			}
			cur, ok := p.records[key]
			if !ok || cur != pr {
				p.mu.Unlock()
				return
			}
			rec := proto.Clone(pr.record).(*endpointpb.EndpointRecord)
			p.mu.Unlock()

			if err := p.send(ctx, rec); err != nil {
				p.logger.Warn("endpoint refresh failed",
					zap.String("replica", rec.ReplicaId),
					zap.Error(err))
			}
		}
	}
}

// recordKey is the canonical identity of a published record. The tuple
// (cluster, group, capsule, replica) is unique across all publishers in
// the cluster, so the registry can use it as a map key without
// collisions.
func recordKey(cluster, group, capsule, replica string) string {
	return cluster + "|" + group + "|" + capsule + "|" + replica
}
