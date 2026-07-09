package node

import (
	"context"
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
)

// Reconnector periodically re-dials known-but-disconnected cluster peers and
// drives their session re-authentication.
//
// Bootstrap peers are dialed exactly once at startup by the authenticator, and
// the only inbound-reconnect path is the libp2p Notifiee reacting to a
// gracefully-departed peer that dials US. Neither covers the case where a seed
// node is killed and restarted with the same identity but no --bootstrap of its
// own: it dials nobody, and its former peers never re-dial it, so the cluster
// stays partitioned even though every persistent phonebook still holds the
// dead node's entry + multiaddrs.
//
// The Reconnector closes that gap. On a jittered tick it walks every joined
// cluster, builds a candidate set of dial-worthy phonebook entries unioned with
// the permanent --bootstrap seeds, re-dials each candidate that is past its
// backoff and not currently connected, and — on a successful dial — emits a
// ReauthWithPeerRequested event. The auth module's ReauthSubscriber consumes
// that event and re-authenticates the pinned peer; the Reconnector never
// touches the auth handshake itself, preserving the module boundary.
//
// A re-dialed libp2p connection is not cluster membership, which is exactly why
// the dial and the re-auth are split across the event bus.
type Reconnector struct {
	host       reconnectHost
	phonebook  phonebook.IPhonebook
	eventBus   events.Bus
	logger     *zap.Logger
	clock      reconnectClock
	joinedFunc func() []string

	// selfID is this node's own peer ID. It is never a dial candidate:
	// persistent phonebooks may carry a self-entry (the stale-self ghost),
	// and dialing it yields libp2p "dial to self attempted".
	selfID string

	interval    time.Duration
	jitter      float64
	baseBackoff time.Duration
	maxBackoff  time.Duration
	dialTimeout time.Duration

	// seedMu guards seeds. Bootstrap seeds accumulate as clusters are
	// joined (each Join carries its own --bootstrap list), so the set is
	// mutated after construction via AddBootstrapSeeds.
	seedMu sync.Mutex
	seeds  map[string]struct{}

	// backoff tracks per-peer capped-exponential dial backoff, keyed by
	// "clusterPath\x00peerKey". Pruned every tick against current
	// GetByCluster membership (plus live seeds) so it stays bounded.
	backoff map[string]*reconnectBackoff

	parentCtx context.Context
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// reconnectBackoff is the per-peer dial backoff state.
type reconnectBackoff struct {
	fails       int
	nextAttempt time.Time
}

// reconnectHost is the minimal libp2p host surface the reconnector needs.
// host.Host satisfies Connect directly; Connectedness is adapted from
// host.Network().Connectedness by hostReconnectAdapter. Tests supply a fake.
type reconnectHost interface {
	Connectedness(peer.ID) network.Connectedness
	Connect(ctx context.Context, pi peer.AddrInfo) error
}

// reconnectClock is the injectable time source. Production uses realClock;
// tests supply a deterministic clock so backoff math is verifiable without
// real sleeps.
type reconnectClock interface {
	Now() time.Time
}

// hostReconnectAdapter adapts a libp2p host.Host to reconnectHost.
type hostReconnectAdapter struct {
	h host.Host
}

// Connectedness reports the connection state to a peer.
func (a hostReconnectAdapter) Connectedness(p peer.ID) network.Connectedness {
	return a.h.Network().Connectedness(p)
}

// Connect dials the peer described by pi.
func (a hostReconnectAdapter) Connect(ctx context.Context, pi peer.AddrInfo) error {
	return a.h.Connect(ctx, pi)
}

// realClock is the production reconnectClock backed by time.Now.
type realClock struct{}

// Now returns the current wall-clock time.
func (realClock) Now() time.Time { return time.Now() }

// Default reconnector timings. Every value is overridable via a functional
// option; these are the sensible production defaults.
const (
	defaultReconnectInterval    = 15 * time.Second
	defaultReconnectJitter      = 0.2
	defaultReconnectBaseBackoff = 5 * time.Second
	defaultReconnectMaxBackoff  = 5 * time.Minute
	defaultReconnectDialTimeout = 10 * time.Second
)

// ReconnectOption configures a Reconnector.
type ReconnectOption func(*Reconnector)

// WithReconnectHost sets the libp2p host used for dialing. Accepts a full
// host.Host (adapted internally) so callers pass n.host directly.
func WithReconnectHost(h host.Host) ReconnectOption {
	return func(r *Reconnector) {
		if h != nil {
			r.host = hostReconnectAdapter{h: h}
		}
	}
}

// WithReconnectHostInterface injects a reconnectHost directly. Used by tests
// to supply a fake host; production code uses WithReconnectHost.
func WithReconnectHostInterface(h reconnectHost) ReconnectOption {
	return func(r *Reconnector) {
		r.host = h
	}
}

// WithReconnectSelfID sets this node's own peer ID so it is excluded from the
// candidate set (phonebooks may carry a self-entry; dialing self errors).
func WithReconnectSelfID(id string) ReconnectOption {
	return func(r *Reconnector) {
		r.selfID = id
	}
}

// WithReconnectPhonebook sets the phonebook the reconnector reads candidates
// from.
func WithReconnectPhonebook(pb phonebook.IPhonebook) ReconnectOption {
	return func(r *Reconnector) {
		r.phonebook = pb
	}
}

// WithReconnectEventBus sets the event bus used to publish
// ReauthWithPeerRequested events.
func WithReconnectEventBus(bus events.Bus) ReconnectOption {
	return func(r *Reconnector) {
		r.eventBus = bus
	}
}

// WithReconnectLogger sets the logger.
func WithReconnectLogger(logger *zap.Logger) ReconnectOption {
	return func(r *Reconnector) {
		r.logger = logger
	}
}

// WithReconnectContext sets the parent context for the reconnector goroutine.
func WithReconnectContext(ctx context.Context) ReconnectOption {
	return func(r *Reconnector) {
		r.parentCtx = ctx
	}
}

// WithReconnectClock injects the time source (tests supply a deterministic
// clock). Nil is ignored.
func WithReconnectClock(clock reconnectClock) ReconnectOption {
	return func(r *Reconnector) {
		if clock != nil {
			r.clock = clock
		}
	}
}

// WithJoinedClustersFunc injects the accessor that returns the set of cluster
// paths this node has joined. Called once per tick.
func WithJoinedClustersFunc(fn func() []string) ReconnectOption {
	return func(r *Reconnector) {
		r.joinedFunc = fn
	}
}

// WithBootstrapSeeds seeds the permanent bootstrap-candidate set. Seeds are
// re-fed every tick (never one-shot) and are subject to the same
// Connectedness + backoff gate as phonebook candidates.
func WithBootstrapSeeds(seeds []string) ReconnectOption {
	return func(r *Reconnector) {
		for _, s := range seeds {
			if s != "" {
				r.seeds[s] = struct{}{}
			}
		}
	}
}

// WithReconnectInterval sets the base tick interval between reconnection
// sweeps. Non-positive values are ignored.
func WithReconnectInterval(d time.Duration) ReconnectOption {
	return func(r *Reconnector) {
		if d > 0 {
			r.interval = d
		}
	}
}

// WithReconnectJitter sets the fractional jitter (0..1) applied to the tick
// interval and to each backoff deadline. Out-of-range values are ignored.
func WithReconnectJitter(f float64) ReconnectOption {
	return func(r *Reconnector) {
		if f >= 0 && f < 1 {
			r.jitter = f
		}
	}
}

// WithReconnectBaseBackoff sets the base per-peer dial backoff. Non-positive
// values are ignored.
func WithReconnectBaseBackoff(d time.Duration) ReconnectOption {
	return func(r *Reconnector) {
		if d > 0 {
			r.baseBackoff = d
		}
	}
}

// WithReconnectMaxBackoff caps the per-peer dial backoff. Non-positive values
// are ignored.
func WithReconnectMaxBackoff(d time.Duration) ReconnectOption {
	return func(r *Reconnector) {
		if d > 0 {
			r.maxBackoff = d
		}
	}
}

// WithReconnectDialTimeout sets the per-dial timeout. Non-positive values are
// ignored.
func WithReconnectDialTimeout(d time.Duration) ReconnectOption {
	return func(r *Reconnector) {
		if d > 0 {
			r.dialTimeout = d
		}
	}
}

// NewReconnector constructs a Reconnector with the given options.
func NewReconnector(opts ...ReconnectOption) *Reconnector {
	r := &Reconnector{
		logger:      zap.NewNop(),
		clock:       realClock{},
		joinedFunc:  func() []string { return nil },
		interval:    defaultReconnectInterval,
		jitter:      defaultReconnectJitter,
		baseBackoff: defaultReconnectBaseBackoff,
		maxBackoff:  defaultReconnectMaxBackoff,
		dialTimeout: defaultReconnectDialTimeout,
		seeds:       make(map[string]struct{}),
		backoff:     make(map[string]*reconnectBackoff),
	}

	for _, opt := range opts {
		opt(r)
	}

	if r.parentCtx != nil {
		r.ctx, r.cancel = context.WithCancel(r.parentCtx)
	} else {
		r.ctx, r.cancel = context.WithCancel(context.Background())
	}

	return r
}

// AddBootstrapSeeds registers additional permanent bootstrap candidates.
// Bootstrap multiaddrs arrive per-cluster at Join time, after the reconnector
// is constructed, so the node calls this as each cluster is joined. Duplicates
// are ignored.
func (r *Reconnector) AddBootstrapSeeds(seeds []string) {
	r.seedMu.Lock()
	defer r.seedMu.Unlock()
	for _, s := range seeds {
		if s != "" {
			r.seeds[s] = struct{}{}
		}
	}
}

// snapshotSeeds returns the current bootstrap seed multiaddrs.
func (r *Reconnector) snapshotSeeds() []string {
	r.seedMu.Lock()
	defer r.seedMu.Unlock()
	out := make([]string, 0, len(r.seeds))
	for s := range r.seeds {
		out = append(out, s)
	}
	return out
}

// Start validates dependencies and launches the single reconnection goroutine.
func (r *Reconnector) Start() error {
	if r.host == nil {
		return fmt.Errorf("reconnector: host is required")
	}
	if r.phonebook == nil {
		return fmt.Errorf("reconnector: phonebook is required")
	}
	if r.eventBus == nil {
		return fmt.Errorf("reconnector: eventBus is required")
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.loop()
	}()

	r.logger.Info("reconnector started",
		zap.Duration("interval", r.interval),
		zap.Duration("base_backoff", r.baseBackoff),
		zap.Duration("max_backoff", r.maxBackoff))
	return nil
}

// Stop cancels the goroutine and waits for it to exit.
func (r *Reconnector) Stop() {
	r.cancel()
	r.wg.Wait()
	r.logger.Debug("reconnector stopped")
}

// loop runs the jittered reconnection sweep until the context is cancelled.
func (r *Reconnector) loop() {
	timer := time.NewTimer(r.nextInterval())
	defer timer.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-timer.C:
			r.tick()
			timer.Reset(r.nextInterval())
		}
	}
}

// nextInterval returns the base interval with symmetric jitter applied.
func (r *Reconnector) nextInterval() time.Duration {
	return r.applyJitter(r.interval)
}

// tick runs one reconnection sweep across every joined cluster.
func (r *Reconnector) tick() {
	clusters := r.joinedFunc()
	if len(clusters) == 0 {
		return
	}

	// Collect every backoff key that is still live this tick so we can
	// prune the map against current membership + seeds at the end.
	liveKeys := make(map[string]struct{})
	seeds := r.snapshotSeeds()

	for _, clusterPath := range clusters {
		r.reconnectCluster(clusterPath, seeds, liveKeys)
	}

	r.pruneBackoff(liveKeys)
}

// reconnectCluster builds the candidate set for one cluster and dials the
// dial-worthy candidates that are past their backoff.
func (r *Reconnector) reconnectCluster(clusterPath string, seeds []string, liveKeys map[string]struct{}) {
	entries, err := r.phonebook.GetByCluster(clusterPath)
	if err != nil {
		r.logger.Warn("reconnector failed to read phonebook",
			zap.String("cluster", clusterPath),
			zap.Error(err))
		return
	}

	now := r.clock.Now()

	// Track which peer keys we have already considered so a bootstrap seed
	// that also lives in the phonebook is not dialed twice in one tick.
	seen := make(map[string]struct{})

	// 1. Phonebook candidates.
	for _, e := range entries {
		// Never dial ourselves (persistent phonebooks may carry a
		// stale self-entry).
		if e.NodeID == r.selfID {
			continue
		}

		key := backoffKey(clusterPath, e.NodeID)
		seen[e.NodeID] = struct{}{}
		liveKeys[key] = struct{}{}

		pid, err := peer.Decode(e.NodeID)
		if err != nil {
			continue
		}

		if !r.isDialWorthy(e, pid, key, now) {
			continue
		}

		// SWIM gate: for a Failed (or, defensively, any non-Active) entry
		// we flip to PendingAuth BEFORE dialing so the SWIM monitor does
		// not immediately re-probe-and-fail a peer whose re-auth is still
		// mid-flight (false-suspect race).
		if e.Status == phonebook.NodeStatusEnum.Failed() {
			if err := r.phonebook.SetStatus(e.NodeID, clusterPath, phonebook.NodeStatusEnum.PendingAuth()); err != nil {
				r.logger.Warn("reconnector failed to gate peer to PendingAuth",
					zap.String("cluster", clusterPath),
					zap.String("peer", e.NodeID),
					zap.Error(err))
			}
		}

		r.dialCandidate(clusterPath, pid, e.Addresses, key, now)
	}

	// 2. Bootstrap seeds — permanent candidates every tick, deduped
	// against the phonebook peers already considered.
	for _, seed := range seeds {
		pi, err := addrInfoFromMultiaddr(seed)
		if err != nil {
			r.logger.Warn("reconnector skipping invalid bootstrap seed",
				zap.String("seed", seed),
				zap.Error(err))
			continue
		}

		nodeID := pi.ID.String()
		if nodeID == r.selfID {
			// A seed pointing at ourselves (e.g. a node listing its own
			// address as bootstrap) — never dial self.
			continue
		}
		if _, ok := seen[nodeID]; ok {
			// Already handled as a phonebook candidate this tick.
			continue
		}
		seen[nodeID] = struct{}{}

		key := backoffKey(clusterPath, nodeID)
		liveKeys[key] = struct{}{}

		if r.host.Connectedness(pi.ID) == network.Connected {
			continue
		}
		if !r.readyForDial(key, now) {
			continue
		}

		// A seed absent from the phonebook is treated like a Failed/absent
		// peer: gate to PendingAuth if we already know it, otherwise the
		// entry simply does not exist yet and the re-auth handshake will
		// add it. We only touch status when an entry exists.
		addrs := seedAddrStrings(pi)
		r.dialCandidateAddrInfo(clusterPath, *pi, addrs, key, now)
	}
}

// isDialWorthy applies the dial-worthiness predicate to a phonebook entry.
func (r *Reconnector) isDialWorthy(e *phonebook.Entry, pid peer.ID, key string, now time.Time) bool {
	// Never resurrect a peer that left gracefully — the returning peer
	// re-dials US (handled by the Notifiee).
	if e.Status == phonebook.NodeStatusEnum.Departed() {
		return false
	}
	switch e.Status {
	case phonebook.NodeStatusEnum.Active(),
		phonebook.NodeStatusEnum.Suspected(),
		phonebook.NodeStatusEnum.Quarantined(),
		phonebook.NodeStatusEnum.Failed():
		// dial-worthy statuses
	default:
		// PendingAuth and any unknown status: skip. PendingAuth is
		// already mid-handshake; dialing again would be redundant.
		return false
	}
	if len(e.Addresses) == 0 {
		return false
	}
	if r.host.Connectedness(pid) == network.Connected {
		return false
	}
	return r.readyForDial(key, now)
}

// readyForDial reports whether the peer's backoff deadline has elapsed.
func (r *Reconnector) readyForDial(key string, now time.Time) bool {
	b, ok := r.backoff[key]
	if !ok {
		return true
	}
	return !now.Before(b.nextAttempt)
}

// dialCandidate parses the first usable multiaddr from addrs and dials it.
func (r *Reconnector) dialCandidate(clusterPath string, pid peer.ID, addrs []string, key string, now time.Time) {
	maddrs := make([]multiaddr.Multiaddr, 0, len(addrs))
	for _, a := range addrs {
		m, err := multiaddr.NewMultiaddr(a)
		if err != nil {
			continue
		}
		maddrs = append(maddrs, m)
	}
	if len(maddrs) == 0 {
		return
	}
	pi := peer.AddrInfo{ID: pid, Addrs: maddrs}
	r.dialCandidateAddrInfo(clusterPath, pi, addrs, key, now)
}

// dialCandidateAddrInfo dials a fully-formed AddrInfo, updating backoff and
// emitting ReauthWithPeerRequested on success.
func (r *Reconnector) dialCandidateAddrInfo(clusterPath string, pi peer.AddrInfo, addrs []string, key string, now time.Time) {
	dialCtx, cancel := context.WithTimeout(r.ctx, r.dialTimeout)
	err := r.host.Connect(dialCtx, pi)
	cancel()

	if err != nil {
		attempt := r.bumpBackoff(key, now)
		r.logger.Warn("reconnector dial failed",
			zap.String("cluster", clusterPath),
			zap.String("peer", pi.ID.String()),
			zap.Int("attempt", attempt),
			zap.Error(err))
		return
	}

	r.logger.Info("reconnector re-dialed peer, requesting re-auth",
		zap.String("cluster", clusterPath),
		zap.String("peer", pi.ID.String()))

	// The dial succeeded but the session is not yet authenticated. Hand off
	// to the auth module over the event bus — module boundary: the
	// reconnector dials, auth re-authenticates. On re-auth success the auth
	// handler promotes the entry to Active. A successful dial resets this
	// peer's backoff immediately so we do not penalise a peer that connected
	// fine; if the subsequent re-auth fails and the peer later returns to a
	// dial-worthy state, the backoff simply starts fresh.
	r.resetBackoff(key)

	r.eventBus.Publish(events.ReauthWithPeerRequested{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: clusterPath,
		PeerID:      pi.ID.String(),
	})
}

// bumpBackoff advances the per-peer capped-exponential backoff and returns the
// new attempt count.
func (r *Reconnector) bumpBackoff(key string, now time.Time) int {
	b, ok := r.backoff[key]
	if !ok {
		b = &reconnectBackoff{}
		r.backoff[key] = b
	}
	b.fails++

	// delay = min(base * 2^(fails-1), maxBackoff), then apply jitter.
	shift := b.fails - 1
	if shift > 62 {
		shift = 62 // guard the int64 shift below
	}
	delay := time.Duration(math.MaxInt64)
	base := int64(r.baseBackoff)
	if base > 0 && shift < 63 {
		mult := int64(1) << uint(shift)
		// Overflow-safe multiply: base*mult may overflow int64.
		if mult != 0 && base <= int64(math.MaxInt64)/mult {
			delay = time.Duration(base * mult)
		}
	}
	if delay > r.maxBackoff {
		delay = r.maxBackoff
	}
	delay = r.applyJitter(delay)
	b.nextAttempt = now.Add(delay)
	return b.fails
}

// resetBackoff clears the backoff for a peer (called on a successful dial /
// re-auth so the peer is immediately eligible again).
func (r *Reconnector) resetBackoff(key string) {
	delete(r.backoff, key)
}

// pruneBackoff drops backoff entries for peers no longer present in any
// cluster's membership or seed set this tick — keeps the map bounded.
func (r *Reconnector) pruneBackoff(liveKeys map[string]struct{}) {
	for key := range r.backoff {
		if _, ok := liveKeys[key]; !ok {
			delete(r.backoff, key)
		}
	}
}

// applyJitter applies symmetric fractional jitter to d using crypto/rand.
// A zero or out-of-range jitter returns d unchanged.
func (r *Reconnector) applyJitter(d time.Duration) time.Duration {
	if r.jitter <= 0 || d <= 0 {
		return d
	}
	span := int64(float64(d) * r.jitter) // magnitude of the +/- window
	if span <= 0 {
		return d
	}
	// Random value in [-span, +span].
	n, err := rand.Int(rand.Reader, big.NewInt(2*span+1))
	if err != nil {
		return d
	}
	delta := n.Int64() - span
	out := int64(d) + delta
	if out < 1 {
		out = 1
	}
	return time.Duration(out)
}

// backoffKey builds the composite backoff map key for a peer in a cluster.
// The NUL separator cannot appear in either component.
func backoffKey(clusterPath, nodeID string) string {
	return clusterPath + "\x00" + nodeID
}

// addrInfoFromMultiaddr parses a full "/ip4/.../p2p/<id>" bootstrap multiaddr
// into a peer.AddrInfo.
func addrInfoFromMultiaddr(addr string) (*peer.AddrInfo, error) {
	m, err := multiaddr.NewMultiaddr(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid multiaddr %q: %w", addr, err)
	}
	pi, err := peer.AddrInfoFromP2pAddr(m)
	if err != nil {
		return nil, fmt.Errorf("invalid peer info %q: %w", addr, err)
	}
	return pi, nil
}

// seedAddrStrings renders an AddrInfo's addresses back to strings (for logging
// / passthrough). Not load-bearing; the Connect call uses the AddrInfo.
func seedAddrStrings(pi *peer.AddrInfo) []string {
	out := make([]string, 0, len(pi.Addrs))
	for _, a := range pi.Addrs {
		out = append(out, a.String())
	}
	return out
}
