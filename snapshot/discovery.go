package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"go.uber.org/zap"
)

const (
	// SnapshotQueryProtocol is the libp2p stream protocol for on-demand
	// snapshot discovery. A requesting node opens this stream and sends
	// a SnapshotQuery; the responder replies with SnapshotQueryResponse.
	SnapshotQueryProtocol = protocol.ID("/falak/snapshot/query/1.0")
)

// SnapshotAvailable is the gossip message broadcast when a node creates
// or receives a snapshot. Every node in the cluster maintains an
// in-memory index from these messages.
type SnapshotAvailable struct {
	CapsuleID string `json:"capsule_id"`
	Tag       string `json:"tag"`
	NodeID    string `json:"node_id"`
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum"`
	Timestamp int64  `json:"timestamp"` // unix millis
}

// SnapshotQuery is the request sent over the query protocol.
type SnapshotQuery struct {
	CapsuleID string `json:"capsule_id"`
	Tag       string `json:"tag"`
}

// SnapshotQueryResponse is the reply over the query protocol.
type SnapshotQueryResponse struct {
	Available bool   `json:"available"`
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum"`
}

// indexKey is the in-memory index key for snapshot availability.
type indexKey struct {
	CapsuleID string
	Tag       string
}

// indexEntry is an entry in the in-memory availability index.
type indexEntry struct {
	NodeID    string
	Size      int64
	Checksum  string
	Timestamp time.Time
}

// Signer signs content with the local node's private key.
type Signer interface {
	Sign(content []byte) ([]byte, error)
}

// Verifier verifies that a signature matches a sender's public key.
type Verifier interface {
	Verify(senderID string, content, signature []byte) bool
}

// signedMessage wraps a SnapshotAvailable with a signature.
type signedMessage struct {
	Payload   []byte `json:"payload"`   // JSON-encoded SnapshotAvailable
	Signature []byte `json:"signature"` // signature over Payload
	SenderID  string `json:"sender_id"`
}

// Discovery manages snapshot availability gossip and on-demand queries.
// It maintains an in-memory index of which peers hold which snapshots,
// fed by gossip messages. When gossip is stale or incomplete, a fallback
// query protocol asks peers directly.
type Discovery struct {
	host     host.Host
	store    *Store
	signer   Signer
	verifier Verifier
	logger   *zap.Logger

	mu    sync.RWMutex
	index map[indexKey][]indexEntry // capsule+tag → list of holders

	topic  *pubsub.Topic
	sub    *pubsub.Subscription
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// DiscoveryOption configures a Discovery.
type DiscoveryOption func(*Discovery)

// WithDiscoveryLogger sets the logger.
func WithDiscoveryLogger(logger *zap.Logger) DiscoveryOption {
	return func(d *Discovery) { d.logger = logger }
}

// WithDiscoverySigner sets the signer for outgoing gossip messages.
func WithDiscoverySigner(s Signer) DiscoveryOption {
	return func(d *Discovery) { d.signer = s }
}

// WithDiscoveryVerifier sets the verifier for incoming gossip messages.
func WithDiscoveryVerifier(v Verifier) DiscoveryOption {
	return func(d *Discovery) { d.verifier = v }
}

// NewDiscovery creates a new Discovery instance. It joins the snapshot
// gossip topic and registers the query stream handler.
func NewDiscovery(
	h host.Host,
	ps *pubsub.PubSub,
	store *Store,
	clusterPath string,
	opts ...DiscoveryOption,
) (*Discovery, error) {
	topicName := fmt.Sprintf("falak/%s/snapshot", clusterPath)

	topic, err := ps.Join(topicName)
	if err != nil {
		return nil, fmt.Errorf("snapshot discovery: join topic %s: %w", topicName, err)
	}
	sub, err := topic.Subscribe()
	if err != nil {
		topic.Close()
		return nil, fmt.Errorf("snapshot discovery: subscribe: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d := &Discovery{
		host:   h,
		store:  store,
		logger: zap.NewNop(),
		index:  make(map[indexKey][]indexEntry),
		topic:  topic,
		sub:    sub,
		ctx:    ctx,
		cancel: cancel,
	}
	for _, opt := range opts {
		opt(d)
	}

	// Register the query handler so peers can ask us directly.
	h.SetStreamHandler(SnapshotQueryProtocol, d.handleQuery)

	// Start the gossip consumer loop.
	d.wg.Add(1)
	go d.consumeGossip()

	return d, nil
}

// BroadcastAvailable publishes a signed SnapshotAvailable message to
// the cluster gossip topic. Called after a snapshot is created or pulled.
func (d *Discovery) BroadcastAvailable(capsuleID, tag, checksum string, size int64) error {
	msg := SnapshotAvailable{
		CapsuleID: capsuleID,
		Tag:       tag,
		NodeID:    d.host.ID().String(),
		Size:      size,
		Checksum:  checksum,
		Timestamp: time.Now().UnixMilli(),
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("snapshot discovery: marshal: %w", err)
	}

	// Sign the payload to prevent injection of fake availability by
	// compromised peers. Receivers verify against the sender's public
	// key from the phonebook.
	var data []byte
	if d.signer != nil {
		sig, err := d.signer.Sign(payload)
		if err != nil {
			return fmt.Errorf("snapshot discovery: sign: %w", err)
		}
		signed := signedMessage{
			Payload:   payload,
			Signature: sig,
			SenderID:  d.host.ID().String(),
		}
		data, err = json.Marshal(signed)
		if err != nil {
			return fmt.Errorf("snapshot discovery: marshal signed: %w", err)
		}
	} else {
		data = payload
	}

	return d.topic.Publish(d.ctx, data)
}

// FindHolders returns the list of node IDs that hold a snapshot for the
// given (capsule, tag) according to the gossip index. Returns an empty
// slice if no holders are known.
func (d *Discovery) FindHolders(capsuleID, tag string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	entries := d.index[indexKey{capsuleID, tag}]
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.NodeID)
	}
	return out
}

// PruneNode removes every index entry held by the given node across all
// (capsule, tag) keys. It is the load-bearing half of the O11 HA story:
// when a node fails or departs, the puller must stop targeting it.
// Without this, addToIndex keeps returning a dead holder, the re-election
// winner pulls from a corpse, the pull fails, and the capsule cold-starts
// even though a replica existed elsewhere.
//
// The node-side wiring subscribes this method to the NodeFailed and
// NodeDeparting events (event-driven, no cross-module call). Returns the
// number of index entries pruned — useful for observability and tests.
func (d *Discovery) PruneNode(nodeID string) int {
	if nodeID == "" {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	pruned := 0
	for key, entries := range d.index {
		kept := entries[:0]
		for _, e := range entries {
			if e.NodeID == nodeID {
				pruned++
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(d.index, key)
			continue
		}
		// kept aliases entries' backing array; copy to a right-sized slice
		// so the dropped tail does not retain stale entries.
		trimmed := make([]indexEntry, len(kept))
		copy(trimmed, kept)
		d.index[key] = trimmed
	}

	if pruned > 0 {
		d.logger.Info("snapshot index: pruned failed/departed holder",
			zap.String("node", nodeID),
			zap.Int("count", pruned))
	}
	return pruned
}

// QueryPeer sends a direct query to a specific peer asking if it holds
// the given snapshot. Returns the response or an error if the peer is
// unreachable or doesn't have it.
func (d *Discovery) QueryPeer(ctx context.Context, peerID peer.ID, capsuleID, tag string) (*SnapshotQueryResponse, error) {
	stream, err := d.host.NewStream(ctx, peerID, SnapshotQueryProtocol)
	if err != nil {
		return nil, fmt.Errorf("snapshot query: open stream to %s: %w", peerID, err)
	}
	defer stream.Close()

	q := SnapshotQuery{CapsuleID: capsuleID, Tag: tag}
	if err := json.NewEncoder(stream).Encode(q); err != nil {
		return nil, fmt.Errorf("snapshot query: encode request: %w", err)
	}

	var resp SnapshotQueryResponse
	if err := json.NewDecoder(stream).Decode(&resp); err != nil {
		return nil, fmt.Errorf("snapshot query: decode response: %w", err)
	}
	return &resp, nil
}

// QueryFirstResponder queries all known peers concurrently and returns
// the first positive response. Used when the gossip index has no
// candidates. Returns ("", error) if no peer has the snapshot.
func (d *Discovery) QueryFirstResponder(ctx context.Context, capsuleID, tag string) (peer.ID, error) {
	peers := d.host.Network().Peers()
	if len(peers) == 0 {
		return "", fmt.Errorf("snapshot query: no peers available")
	}

	type result struct {
		peerID peer.ID
		ok     bool
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan result, len(peers))
	for _, p := range peers {
		p := p
		go func() {
			resp, err := d.QueryPeer(ctx, p, capsuleID, tag)
			if err == nil && resp.Available {
				results <- result{peerID: p, ok: true}
			} else {
				results <- result{peerID: p, ok: false}
			}
		}()
	}

	remaining := len(peers)
	for remaining > 0 {
		select {
		case r := <-results:
			if r.ok {
				return r.peerID, nil
			}
			remaining--
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "", fmt.Errorf("snapshot query: no peer has snapshot %s/%s", capsuleID, tag)
}

// Stop shuts down the gossip consumer and removes the query handler.
func (d *Discovery) Stop() {
	d.cancel()
	d.sub.Cancel()
	d.topic.Close()
	d.host.RemoveStreamHandler(SnapshotQueryProtocol)
	d.wg.Wait()
}

// consumeGossip reads SnapshotAvailable messages from the PubSub topic
// and updates the in-memory index. If a verifier is configured, messages
// must carry a valid signature or they are rejected.
func (d *Discovery) consumeGossip() {
	defer d.wg.Done()
	for {
		msg, err := d.sub.Next(d.ctx)
		if err != nil {
			return // context cancelled or sub closed
		}
		if msg.ReceivedFrom == d.host.ID() {
			continue
		}

		avail, ok := d.parseAndVerify(msg.Data)
		if !ok {
			continue
		}

		d.addToIndex(avail)
		d.logger.Debug("snapshot gossip: indexed",
			zap.String("capsule_id", avail.CapsuleID),
			zap.String("tag", avail.Tag),
			zap.String("node_id", avail.NodeID))
	}
}

// parseAndVerify extracts a SnapshotAvailable from raw gossip data.
// If a verifier is configured, the data must be a signed envelope; the
// signature is checked against the sender's public key. Returns
// (avail, true) on success, or (zero, false) if the message is
// invalid or the signature fails.
func (d *Discovery) parseAndVerify(data []byte) (SnapshotAvailable, bool) {
	var avail SnapshotAvailable

	if d.verifier != nil {
		// Expect a signed envelope.
		var signed signedMessage
		if err := json.Unmarshal(data, &signed); err != nil || len(signed.Payload) == 0 {
			d.logger.Debug("snapshot gossip: invalid signed envelope", zap.Error(err))
			return avail, false
		}
		if !d.verifier.Verify(signed.SenderID, signed.Payload, signed.Signature) {
			d.logger.Warn("snapshot gossip: signature verification failed",
				zap.String("sender", signed.SenderID))
			return avail, false
		}
		if err := json.Unmarshal(signed.Payload, &avail); err != nil {
			d.logger.Debug("snapshot gossip: invalid payload in signed message", zap.Error(err))
			return avail, false
		}
	} else {
		// No verifier — accept unsigned messages (test/dev mode).
		if err := json.Unmarshal(data, &avail); err != nil {
			d.logger.Debug("snapshot gossip: invalid message", zap.Error(err))
			return avail, false
		}
	}
	return avail, true
}

// addToIndex inserts or updates an entry in the availability index.
func (d *Discovery) addToIndex(avail SnapshotAvailable) {
	d.mu.Lock()
	defer d.mu.Unlock()

	key := indexKey{avail.CapsuleID, avail.Tag}
	entries := d.index[key]

	// Update existing entry for this node, or append a new one.
	for i, e := range entries {
		if e.NodeID == avail.NodeID {
			entries[i] = indexEntry{
				NodeID:    avail.NodeID,
				Size:      avail.Size,
				Checksum:  avail.Checksum,
				Timestamp: time.UnixMilli(avail.Timestamp),
			}
			d.index[key] = entries
			return
		}
	}
	d.index[key] = append(entries, indexEntry{
		NodeID:    avail.NodeID,
		Size:      avail.Size,
		Checksum:  avail.Checksum,
		Timestamp: time.UnixMilli(avail.Timestamp),
	})
}

// handleQuery is the libp2p stream handler for direct snapshot queries.
// It checks the local store and responds with availability info.
func (d *Discovery) handleQuery(stream network.Stream) {
	defer stream.Close()
	stream.SetDeadline(time.Now().Add(30 * time.Second))

	var q SnapshotQuery
	if err := json.NewDecoder(stream).Decode(&q); err != nil {
		d.logger.Debug("snapshot query handler: decode failed", zap.Error(err))
		return
	}

	rec, err := d.store.Get(q.CapsuleID, q.Tag)
	if err != nil || rec == nil {
		json.NewEncoder(stream).Encode(SnapshotQueryResponse{Available: false})
		return
	}

	json.NewEncoder(stream).Encode(SnapshotQueryResponse{
		Available: true,
		Size:      rec.Size,
		Checksum:  rec.Checksum,
	})
}
