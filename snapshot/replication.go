package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"go.uber.org/zap"
)

const (
	// ReplicateProtocol is the libp2p stream protocol a holder uses to ask
	// a target to pull and pin a snapshot copy. The request is small; the
	// target services it by pulling the bytes back over the existing
	// TransferProtocol (reusing TransferServer + PullSnapshot), so this
	// adds no new bulk-transfer protocol.
	ReplicateProtocol = protocol.ID("/falak/snapshot/replicate/1.0")
)

// Replication defaults. Every knob is overridable via a functional option;
// these are the sensible production values.
const (
	// defaultReplicationFactor is K — the number of EXTRA standby copies
	// beyond the holder. 2 means three total copies survive one failure.
	defaultReplicationFactor = 2
	// defaultReplicationConcurrency bounds simultaneous outbound replication
	// sequences per node (thundering-herd control). Small on purpose.
	defaultReplicationConcurrency = 2
	// defaultReplicationRetries is the number of RETRY attempts per target
	// after the first try (so 2 == up to 3 total attempts per target).
	defaultReplicationRetries = 2
	// defaultReplicationBackoff is the base inter-retry delay; the actual
	// wait is base + full-jitter over [0, base).
	defaultReplicationBackoff = 2 * time.Second
	// defaultReplicationPrePushJitter spreads the start of post-capture
	// pushes so a cluster-wide rolling deploy does not fire N×K transfers
	// in lockstep. The actual delay is uniform over [0, jitter).
	defaultReplicationPrePushJitter = 3 * time.Second
	// defaultReplicationDiskHeadroomMB skips targets with less free disk
	// than this so a large CRIU archive never lands on a nearly-full node.
	defaultReplicationDiskHeadroomMB = 2048
	// defaultReplicationSampleSize is the power-of-two-choices sample size.
	defaultReplicationSampleSize = 2
	// defaultReplicationQueueSize bounds the pending-job backlog.
	defaultReplicationQueueSize = 256
	// replicateStreamTimeout bounds one push-request round trip. It must
	// exceed a full snapshot transfer because the target pulls the bytes
	// synchronously before acking.
	replicateStreamTimeout = 12 * time.Minute
)

// CandidateProvider supplies the set of possible replication targets and
// the holder's own failure domain. Satisfied by a node-side adapter over
// the phonebook + metrics; kept as an interface so the snapshot package
// neither imports those modules nor needs them in tests.
type CandidateProvider interface {
	// Candidates returns all currently-known peers as replication
	// candidates for the given snapshot. Implementations should return
	// every peer they know about (including unhealthy / low-disk ones);
	// the Replicator applies the Active and disk-headroom filters.
	Candidates(capsuleID, tag string) []Candidate
	// LocalDatacenter / LocalRegion identify the holder's failure domain
	// so selection can prefer spreading copies away from it.
	LocalDatacenter() string
	LocalRegion() string
}

// HolderIndex reports which peers already hold a snapshot, used to compute
// how many more copies are needed to reach K. Satisfied by *Discovery.
type HolderIndex interface {
	FindHolders(capsuleID, tag string) []string
}

// Broadcaster announces snapshot availability to the cluster. A receiver
// of a replicated copy re-broadcasts so every node's index reflects ALL K
// holders, not just the original. Satisfied by *Discovery.
type Broadcaster interface {
	BroadcastAvailable(capsuleID, tag, checksum string, size int64) error
}

// ReplicateRequest is sent by the holder over ReplicateProtocol. The
// target services it by pulling (CapsuleID, Tag) back from the holder.
type ReplicateRequest struct {
	CapsuleID string `json:"capsule_id"`
	Tag       string `json:"tag"`
	Checksum  string `json:"checksum"`
	Size      int64  `json:"size"`
}

// ReplicateAck is the target's reply: OK once the copy is pulled, pinned,
// and re-broadcast, or an error describing why replication failed.
type ReplicateAck struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// replicaTransport pushes one snapshot to one target. Abstracted so unit
// tests can record pushes deterministically without a libp2p host; the
// production implementation (streamTransport) opens a ReplicateProtocol
// stream and waits for the ack.
type replicaTransport interface {
	Push(ctx context.Context, target Candidate, req ReplicateRequest) error
}

// streamTransport is the production replicaTransport. It opens a
// ReplicateProtocol stream to the target, sends the request, and waits for
// the target's ack (which the target only sends after it has pulled and
// pinned the copy).
type streamTransport struct {
	host host.Host
}

// Push implements replicaTransport over libp2p.
func (t *streamTransport) Push(ctx context.Context, target Candidate, req ReplicateRequest) error {
	stream, err := t.host.NewStream(ctx, target.PeerID, ReplicateProtocol)
	if err != nil {
		return fmt.Errorf("replicate push: open stream to %s: %w", target.NodeID, err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(replicateStreamTimeout))

	if err := json.NewEncoder(stream).Encode(req); err != nil {
		return fmt.Errorf("replicate push: encode request: %w", err)
	}
	var ack ReplicateAck
	if err := json.NewDecoder(stream).Decode(&ack); err != nil {
		return fmt.Errorf("replicate push: decode ack: %w", err)
	}
	if !ack.OK {
		return fmt.Errorf("replicate push: target rejected: %s", ack.Error)
	}
	return nil
}

// Replicator drives holder-side, push-after-capture snapshot replication
// and serves the receiver side (pull + pin + re-broadcast). It maintains K
// standby copies for high-availability fast-restart (O11).
type Replicator struct {
	host        host.Host
	store       *Store
	index       HolderIndex
	broadcaster Broadcaster
	provider    CandidateProvider
	transport   replicaTransport
	logger      *zap.Logger

	selfID string

	factor         int
	concurrency    int
	retries        int
	backoffBase    time.Duration
	prePushJitter  time.Duration
	diskHeadroomMB int64
	sampleSize     int

	rngMu sync.Mutex
	rng   *rand.Rand

	queue *jobQueue
	seq   atomic.Uint64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ReplicatorOption configures a Replicator.
type ReplicatorOption func(*Replicator)

// WithReplicationHost sets the libp2p host used for the push transport and
// the receiver stream handler.
func WithReplicationHost(h host.Host) ReplicatorOption {
	return func(r *Replicator) { r.host = h }
}

// WithReplicationStore sets the local snapshot store.
func WithReplicationStore(s *Store) ReplicatorOption {
	return func(r *Replicator) { r.store = s }
}

// WithReplicationIndex sets the holder index (for K-counting). *Discovery.
func WithReplicationIndex(i HolderIndex) ReplicatorOption {
	return func(r *Replicator) { r.index = i }
}

// WithReplicationBroadcaster sets the availability broadcaster used by the
// receiver side to re-announce a pulled copy. *Discovery.
func WithReplicationBroadcaster(b Broadcaster) ReplicatorOption {
	return func(r *Replicator) { r.broadcaster = b }
}

// WithReplicationCandidateProvider sets the target source.
func WithReplicationCandidateProvider(p CandidateProvider) ReplicatorOption {
	return func(r *Replicator) { r.provider = p }
}

// WithReplicationTransport overrides the push transport (tests inject a
// recording stub; production uses the libp2p streamTransport).
func WithReplicationTransport(t replicaTransport) ReplicatorOption {
	return func(r *Replicator) { r.transport = t }
}

// WithReplicationLogger sets the logger.
func WithReplicationLogger(l *zap.Logger) ReplicatorOption {
	return func(r *Replicator) { r.logger = l }
}

// WithReplicationFactor sets K, the number of EXTRA standby copies beyond
// the holder. Non-positive values are ignored (default 2).
func WithReplicationFactor(k int) ReplicatorOption {
	return func(r *Replicator) {
		if k > 0 {
			r.factor = k
		}
	}
}

// WithReplicationConcurrency bounds simultaneous outbound replication
// sequences. Non-positive values are ignored (default 2).
func WithReplicationConcurrency(n int) ReplicatorOption {
	return func(r *Replicator) {
		if n > 0 {
			r.concurrency = n
		}
	}
}

// WithReplicationRetries sets the per-target retry count after the first
// attempt. Negative values are ignored (default 2). Zero means a single
// attempt with no retries.
func WithReplicationRetries(n int) ReplicatorOption {
	return func(r *Replicator) {
		if n >= 0 {
			r.retries = n
		}
	}
}

// WithReplicationBackoff sets the base inter-retry delay (jittered).
// Non-positive values are ignored (default 2s).
func WithReplicationBackoff(d time.Duration) ReplicatorOption {
	return func(r *Replicator) {
		if d > 0 {
			r.backoffBase = d
		}
	}
}

// WithReplicationPrePushJitter sets the maximum pre-push delay. A
// non-positive value disables the jitter (immediate push), which tests use
// for determinism.
func WithReplicationPrePushJitter(d time.Duration) ReplicatorOption {
	return func(r *Replicator) { r.prePushJitter = d }
}

// WithReplicationDiskHeadroomMB sets the minimum free disk a target must
// have. Non-positive disables the filter (default 2048 MiB).
func WithReplicationDiskHeadroomMB(mb int64) ReplicatorOption {
	return func(r *Replicator) { r.diskHeadroomMB = mb }
}

// WithReplicationSampleSize sets the power-of-two-choices sample size.
// Values < 1 are ignored (default 2).
func WithReplicationSampleSize(n int) ReplicatorOption {
	return func(r *Replicator) {
		if n >= 1 {
			r.sampleSize = n
		}
	}
}

// WithReplicationQueueSize bounds the pending-job backlog. Non-positive
// values are ignored (default 256).
func WithReplicationQueueSize(n int) ReplicatorOption {
	return func(r *Replicator) {
		if n > 0 {
			r.queue = newJobQueue(n)
		}
	}
}

// WithReplicationRand injects the random source (determinism in tests).
func WithReplicationRand(rng *rand.Rand) ReplicatorOption {
	return func(r *Replicator) {
		if rng != nil {
			r.rng = rng
		}
	}
}

// NewReplicator constructs a Replicator, registers the receiver stream
// handler (when a host is set), and starts the worker pool. Call Stop to
// shut down. Holder and receiver roles are both live after construction:
// a node can receive replicas even if it never enqueues a push.
func NewReplicator(opts ...ReplicatorOption) *Replicator {
	r := &Replicator{
		logger:         zap.NewNop(),
		factor:         defaultReplicationFactor,
		concurrency:    defaultReplicationConcurrency,
		retries:        defaultReplicationRetries,
		backoffBase:    defaultReplicationBackoff,
		prePushJitter:  defaultReplicationPrePushJitter,
		diskHeadroomMB: defaultReplicationDiskHeadroomMB,
		sampleSize:     defaultReplicationSampleSize,
		queue:          newJobQueue(defaultReplicationQueueSize),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.transport == nil && r.host != nil {
		r.transport = &streamTransport{host: r.host}
	}

	// Establish the context BEFORE registering the receiver handler so an
	// incoming replicate stream can never observe a nil r.ctx.
	r.ctx, r.cancel = context.WithCancel(context.Background())

	if r.host != nil {
		r.selfID = r.host.ID().String()
		r.host.SetStreamHandler(ReplicateProtocol, r.handleReplicate)
	}

	for i := 0; i < r.concurrency; i++ {
		r.wg.Add(1)
		go r.worker()
	}
	return r
}

// Stop cancels in-flight pushes, closes the queue so workers exit, removes
// the receiver stream handler, and waits for all goroutines.
func (r *Replicator) Stop() {
	r.cancel()
	r.queue.close()
	if r.host != nil {
		r.host.RemoveStreamHandler(ReplicateProtocol)
	}
	r.wg.Wait()
}

// Replicate enqueues a post-capture replication job for (capsuleID, tag).
// It NEVER blocks the caller: priority is computed from the current holder
// count and the job is handed to the bounded worker pool. Safe to call
// even when the Replicator is not fully wired (missing provider/index) —
// it degrades to a no-op so the capture path is never coupled to
// replication being available.
func (r *Replicator) Replicate(capsuleID, tag, checksum string, size int64) {
	if r == nil || r.provider == nil || r.index == nil || r.transport == nil {
		return
	}

	existing := len(r.index.FindHolders(capsuleID, tag))
	need := r.factor - existing
	if need <= 0 {
		r.logger.Debug("snapshot replication: already at factor, skipping enqueue",
			zap.String("capsule", capsuleID),
			zap.String("tag", tag),
			zap.Int("holders", existing),
			zap.Int("factor", r.factor))
		return
	}

	job := replicationJob{
		capsuleID: capsuleID,
		tag:       tag,
		checksum:  checksum,
		size:      size,
		priority:  need, // fewer existing copies => higher priority
		seq:       r.seq.Add(1),
	}
	accepted, dropped, droppedOK := r.queue.push(job)
	if droppedOK && dropped != nil {
		r.logger.Warn("snapshot replication: queue full, dropped least-urgent job",
			zap.String("dropped_capsule", dropped.capsuleID),
			zap.String("dropped_tag", dropped.tag),
			zap.Int("dropped_priority", dropped.priority))
	}
	if !accepted {
		r.logger.Warn("snapshot replication: job rejected (queue full, lower priority)",
			zap.String("capsule", capsuleID),
			zap.String("tag", tag),
			zap.Int("priority", need))
		return
	}
	r.logger.Debug("snapshot replication: enqueued",
		zap.String("capsule", capsuleID),
		zap.String("tag", tag),
		zap.Int("need", need),
		zap.Int("queue_len", r.queue.length()))
}

// worker pulls jobs by priority and runs them until the queue closes.
func (r *Replicator) worker() {
	defer r.wg.Done()
	for {
		job, ok := r.queue.pop()
		if !ok {
			return
		}
		r.runJob(job)
	}
}

// runJob applies the pre-push jitter then drives the replication sequence.
func (r *Replicator) runJob(job replicationJob) {
	if !r.sleepJitter(r.prePushJitter) {
		return // context cancelled during the pre-push delay
	}
	r.replicate(job)
}

// replicate selects targets and pushes the snapshot until K copies exist
// or candidates are exhausted, warning on shortfall.
func (r *Replicator) replicate(job replicationJob) {
	if r.ctx.Err() != nil {
		return
	}

	existing := r.index.FindHolders(job.capsuleID, job.tag)
	exclude := map[string]bool{r.selfID: true}
	for _, h := range existing {
		exclude[h] = true
	}
	need := r.factor - len(existing)
	if need <= 0 {
		return
	}

	cands := r.provider.Candidates(job.capsuleID, job.tag)
	targets := selectTargets(
		cands,
		r.provider.LocalDatacenter(),
		r.provider.LocalRegion(),
		exclude,
		r.diskHeadroomMB,
		r.sampleSize,
		r.intn,
	)
	if len(targets) == 0 {
		r.logger.Warn("snapshot replication: no eligible targets",
			zap.String("capsule", job.capsuleID),
			zap.String("tag", job.tag),
			zap.Int("need", need),
			zap.Int("candidates", len(cands)))
		return
	}

	req := ReplicateRequest{
		CapsuleID: job.capsuleID,
		Tag:       job.tag,
		Checksum:  job.checksum,
		Size:      job.size,
	}

	success := 0
	for _, t := range targets {
		if success >= need {
			break
		}
		if r.ctx.Err() != nil {
			return
		}
		if r.pushWithRetry(req, t) {
			success++
			r.logger.Info("snapshot replication: copy placed",
				zap.String("capsule", job.capsuleID),
				zap.String("tag", job.tag),
				zap.String("peer", t.NodeID),
				zap.String("datacenter", t.Datacenter),
				zap.Int("total_copies", 1+len(existing)+success), // holder + standbys
				zap.Int("factor", r.factor))
		}
	}

	if success < need {
		r.logger.Warn("snapshot replication: shortfall — fewer than K copies achieved",
			zap.String("capsule", job.capsuleID),
			zap.String("tag", job.tag),
			zap.Int("total_copies", 1+len(existing)+success), // holder + standbys
			zap.Int("want", 1+r.factor),                      // holder + K
			zap.Int("candidates", len(targets)))
		return
	}
	r.logger.Info("snapshot replication: complete",
		zap.String("capsule", job.capsuleID),
		zap.String("tag", job.tag),
		zap.Int("total_copies", 1+len(existing)+success),
		zap.Int("factor", r.factor))
}

// pushWithRetry attempts to place one copy on one target, retrying with
// jittered backoff up to r.retries times. Returns true on the first
// success. Each retry logs at WARN (plan part 4).
func (r *Replicator) pushWithRetry(req ReplicateRequest, target Candidate) bool {
	attempts := r.retries + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		if r.ctx.Err() != nil {
			return false
		}
		err := r.transport.Push(r.ctx, target, req)
		if err == nil {
			return true
		}
		if attempt < attempts {
			r.logger.Warn("snapshot replication: push failed, will retry",
				zap.String("capsule", req.CapsuleID),
				zap.String("tag", req.Tag),
				zap.String("peer", target.NodeID),
				zap.Int("attempt", attempt),
				zap.Int("max", attempts),
				zap.Error(err))
			// Jittered backoff: base + uniform[0, base).
			if !r.sleepFor(r.backoffBase + time.Duration(r.intn63(int64(r.backoffBase)))) {
				return false
			}
			continue
		}
		r.logger.Warn("snapshot replication: push failed, giving up on target",
			zap.String("capsule", req.CapsuleID),
			zap.String("tag", req.Tag),
			zap.String("peer", target.NodeID),
			zap.Int("attempts", attempts),
			zap.Error(err))
	}
	return false
}

// sleepJitter waits a uniform-random duration in [0, max) (when max>0),
// returning false if the context is cancelled during the wait. With
// max<=0 it returns immediately.
func (r *Replicator) sleepJitter(max time.Duration) bool {
	if max <= 0 {
		return r.ctx.Err() == nil
	}
	d := time.Duration(r.intn63(int64(max)))
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-r.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// sleepFor waits exactly d, returning false if the context is cancelled
// during the wait. With d<=0 it returns immediately.
func (r *Replicator) sleepFor(d time.Duration) bool {
	if d <= 0 {
		return r.ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-r.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// intn returns a value in [0, n) from the guarded rng.
func (r *Replicator) intn(n int) int {
	if n <= 0 {
		return 0
	}
	r.rngMu.Lock()
	defer r.rngMu.Unlock()
	return r.rng.Intn(n)
}

// intn63 returns a value in [0, n) from the guarded rng (int64 domain).
func (r *Replicator) intn63(n int64) int64 {
	if n <= 0 {
		return 0
	}
	r.rngMu.Lock()
	defer r.rngMu.Unlock()
	return r.rng.Int63n(n)
}

// --- Receiver side -------------------------------------------------------

// handleReplicate is the ReplicateProtocol stream handler. It pulls the
// requested snapshot back from the requesting holder, pins it as a
// standby, re-broadcasts availability, and acks.
func (r *Replicator) handleReplicate(stream network.Stream) {
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(replicateStreamTimeout))

	var req ReplicateRequest
	if err := json.NewDecoder(stream).Decode(&req); err != nil {
		r.logger.Debug("replicate handler: decode request failed", zap.Error(err))
		return
	}

	holder := stream.Conn().RemotePeer()
	ctx, cancel := context.WithTimeout(r.ctx, replicateStreamTimeout)
	defer cancel()

	ack := r.serveReplicate(ctx, holder, req)
	if err := json.NewEncoder(stream).Encode(ack); err != nil {
		r.logger.Debug("replicate handler: encode ack failed", zap.Error(err))
	}
}

// serveReplicate pulls the snapshot from the holder (unless already
// present), pins it, and re-broadcasts. Returns the ack to send.
func (r *Replicator) serveReplicate(ctx context.Context, holder peer.ID, req ReplicateRequest) ReplicateAck {
	if r.store == nil {
		return ReplicateAck{OK: false, Error: "no local store"}
	}

	// Idempotent: if we already hold it (e.g. duplicate request), just
	// re-pin and re-broadcast.
	if rec, err := r.store.Get(req.CapsuleID, req.Tag); err == nil && rec != nil {
		r.onReplicaReceived(req.CapsuleID, req.Tag, rec.Checksum, rec.Size)
		return ReplicateAck{OK: true}
	}

	rec, err := PullSnapshot(ctx, r.host, holder, r.store, req.CapsuleID, req.Tag, r.logger)
	if err != nil {
		r.logger.Warn("replicate handler: pull from holder failed",
			zap.String("capsule", req.CapsuleID),
			zap.String("tag", req.Tag),
			zap.String("holder", holder.String()),
			zap.Error(err))
		return ReplicateAck{OK: false, Error: err.Error()}
	}

	r.logger.Info("snapshot replication: received standby copy",
		zap.String("capsule", req.CapsuleID),
		zap.String("tag", req.Tag),
		zap.String("holder", holder.String()),
		zap.Int64("bytes", rec.Size))

	r.onReplicaReceived(req.CapsuleID, req.Tag, rec.Checksum, rec.Size)
	return ReplicateAck{OK: true}
}

// onReplicaReceived pins the freshly-received copy as a standby (so TTL/LRU
// eviction does not silently drop below K) and re-broadcasts availability
// so every node's index reflects this holder (plan parts 5 + 7). Split out
// from the stream path so it is unit-testable without a libp2p host.
func (r *Replicator) onReplicaReceived(capsuleID, tag, checksum string, size int64) {
	if r.store != nil {
		if err := r.store.SetPinned(capsuleID, tag, true); err != nil {
			r.logger.Warn("snapshot replication: pin standby failed",
				zap.String("capsule", capsuleID),
				zap.String("tag", tag),
				zap.Error(err))
		}
	}
	if r.broadcaster != nil {
		if err := r.broadcaster.BroadcastAvailable(capsuleID, tag, checksum, size); err != nil {
			r.logger.Warn("snapshot replication: re-broadcast failed",
				zap.String("capsule", capsuleID),
				zap.String("tag", tag),
				zap.Error(err))
		}
	}
}
