// This file implements the TCP forwarder for the L4 proxy (plan
// 11B.13). One TCPListener instance owns one (Service, port,
// bridge-gateway-IP) tuple. On Accept the listener:
//
//   1. Resolves the Service snapshot through ServiceResolver. A
//      not-found Service closes the connection immediately (the
//      Service was deleted between Accept and lookup).
//   2. Picks a backend via SWRR over the current LiveWeights.
//   3. Picks a replica of that backend via the outlier-filtered
//      Selector. Up to dialRetries replicas are tried on dial
//      failure; each failure feeds Selector.RecordDialFailure.
//   4. Translates the service-port name to the backend's named port
//      via ServiceBackend.PortMap (empty map = identity).
//   5. Dials the backend bridge IP:port with a bounded dial timeout
//      and bidirectional io.Copy until either side closes.
//
// Per decision #28 the forwarder logs nothing at INFO/DEBUG on
// success — connection-rate logs are a disk-flood vector. Failures
// go through a samplelog.Sampler at a configurable rate (default
// 1 evt/sec per class); the underlying counters always bump.
//
// Buffer reuse: a sync.Pool of 64 KiB byte slices serves both
// directions of every connection. This keeps GC pressure bounded
// regardless of connection rate.
//
// Goroutine bookkeeping: a single accept goroutine + two copy
// goroutines per connection, all tracked via sync.WaitGroup. Stop
// closes the listener, cancels the context, closes every active
// connection, and waits for goroutines to exit cleanly.
package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/network/internal/samplelog"
)

// Defaults (decision #29 plus established proxy practice).
const (
	// DefaultIdleTimeout closes idle connections after no activity.
	DefaultIdleTimeout = 5 * time.Minute
	// DefaultConnectTimeout bounds the proxy→backend dial.
	DefaultConnectTimeout = 5 * time.Second
	// DefaultDialRetries is the per-connection replica retry count.
	DefaultDialRetries = 2
	// proxyBufSize is the per-direction copy buffer size.
	proxyBufSize = 64 * 1024
)

// BackendSnapshot is the proxy's read-only view of one routable
// backend at the moment of pick. Names and Ports come from the
// Service spec; the proxy never mutates this.
type BackendSnapshot struct {
	// Capsule is the backend's logical name (used as the SWRR key).
	Capsule string
	// Weight is the backend's SWRR weight at snapshot time.
	Weight int32
	// PortMap remaps service-port names → backend named-port names.
	// An empty map means identity remapping.
	PortMap map[string]string
}

// ServiceSnapshot is the proxy's read-only view of one Service for
// routing. ServiceResolver returns one of these per request.
type ServiceSnapshot struct {
	// ServiceID is the stable identifier used by the StatsRegistry.
	ServiceID string
	// ClusterPath / GroupID locate the endpoint records.
	ClusterPath string
	GroupID     string
	// Backends is the post-strategy list of routable backends.
	// LiveWeights are already baked into Weight.
	Backends []BackendSnapshot
	// IdleTimeout and ConnectTimeout reflect the Service's timeouts;
	// zero means "use proxy defaults".
	IdleTimeout    time.Duration
	ConnectTimeout time.Duration
}

// ServiceResolver yields the routing snapshot for one Service. The
// implementation is supplied by the service.Manager via an adapter;
// the proxy package has no compile-time dependency on service.
type ServiceResolver interface {
	// Resolve returns the routing snapshot. ok==false when the
	// Service has been deleted or has no resolvable backends.
	Resolve() (snap ServiceSnapshot, ok bool)
}

// Dialer abstracts the actual dial. Production wires net.Dialer;
// tests inject a fake.
type Dialer interface {
	// DialContext dials network at addr with the proxy's connect
	// timeout already applied via ctx.
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// netDialer is the stdlib default.
type netDialer struct{ d net.Dialer }

func (n *netDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return n.d.DialContext(ctx, network, addr)
}

// TCPListenerOption configures a TCPListener.
type TCPListenerOption func(*TCPListener)

// WithTCPLogger sets the zap logger.
func WithTCPLogger(l *zap.Logger) TCPListenerOption {
	return func(t *TCPListener) {
		if l != nil {
			t.logger = l
		}
	}
}

// WithTCPSelector wires the replica selector. Required.
func WithTCPSelector(s *Selector) TCPListenerOption {
	return func(t *TCPListener) { t.selector = s }
}

// WithTCPResolver wires the service resolver. Required.
func WithTCPResolver(r ServiceResolver) TCPListenerOption {
	return func(t *TCPListener) { t.resolver = r }
}

// WithTCPStats wires the per-Service stats counters. Required.
func WithTCPStats(s *Stats) TCPListenerOption {
	return func(t *TCPListener) { t.stats = s }
}

// WithTCPDialer overrides the dialer. Defaults to a stdlib net.Dialer.
func WithTCPDialer(d Dialer) TCPListenerOption {
	return func(t *TCPListener) {
		if d != nil {
			t.dialer = d
		}
	}
}

// WithTCPIdleTimeout overrides DefaultIdleTimeout.
func WithTCPIdleTimeout(d time.Duration) TCPListenerOption {
	return func(t *TCPListener) {
		if d > 0 {
			t.idleTimeout = d
		}
	}
}

// WithTCPConnectTimeout overrides DefaultConnectTimeout.
func WithTCPConnectTimeout(d time.Duration) TCPListenerOption {
	return func(t *TCPListener) {
		if d > 0 {
			t.connectTimeout = d
		}
	}
}

// WithTCPDialRetries overrides DefaultDialRetries.
func WithTCPDialRetries(n int) TCPListenerOption {
	return func(t *TCPListener) {
		if n >= 0 {
			t.dialRetries = n
		}
	}
}

// WithTCPPortName binds the listener to a service-port name. The
// listener uses this name to translate via ServiceBackend.PortMap.
func WithTCPPortName(name string) TCPListenerOption {
	return func(t *TCPListener) { t.portName = name }
}

// WithTCPSampler injects the log sampler. Defaults to a fresh one
// with samplelog.DefaultInterval.
func WithTCPSampler(s *samplelog.Sampler) TCPListenerOption {
	return func(t *TCPListener) {
		if s != nil {
			t.sampler = s
		}
	}
}

// WithTCPSourceResolver wires the visibility source-IP resolver. When
// unset, the listener admits every accepted connection (Phase 11A
// behaviour). When set together with WithTCPVisibility, every Accept
// must pass the admission check before reaching Resolve().
func WithTCPSourceResolver(s SourceResolver) TCPListenerOption {
	return func(t *TCPListener) { t.srcResolver = s }
}

// WithTCPVisibility sets the Service-visibility policy the listener
// enforces against every inbound connection's source IP. Pair with
// WithTCPSourceResolver to wire the bridge map.
func WithTCPVisibility(v ServiceVisibility) TCPListenerOption {
	return func(t *TCPListener) { t.visibility = v }
}

// WithTCPVisibilityStats injects the visibility-denied counter. The
// proxy lifecycle shares one per-listener counter so both TCP and UDP
// updates land in the same place.
func WithTCPVisibilityStats(v *VisibilityStats) TCPListenerOption {
	return func(t *TCPListener) {
		if v != nil {
			t.visStats = v
		}
	}
}

// TCPListener forwards inbound TCP to a SWRR/Selector-picked replica.
// One per (Service, service-port, bridge-gateway-IP).
type TCPListener struct {
	logger         *zap.Logger
	selector       *Selector
	resolver       ServiceResolver
	stats          *Stats
	dialer         Dialer
	sampler        *samplelog.Sampler
	idleTimeout    time.Duration
	connectTimeout time.Duration
	dialRetries    int
	portName       string
	srcResolver    SourceResolver
	visibility     ServiceVisibility
	visStats       *VisibilityStats

	mu      sync.Mutex
	swrr    *SWRR
	weights map[string]int32

	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	connMu sync.Mutex
	conns  map[net.Conn]struct{}

	started bool
	stopped bool

	bufPool *sync.Pool
}

// NewTCPListener constructs a TCPListener. The caller must Start it
// to begin accepting connections.
func NewTCPListener(opts ...TCPListenerOption) *TCPListener {
	t := &TCPListener{
		logger:         zap.NewNop(),
		dialer:         &netDialer{d: net.Dialer{Timeout: DefaultConnectTimeout}},
		sampler:        samplelog.NewSampler(),
		idleTimeout:    DefaultIdleTimeout,
		connectTimeout: DefaultConnectTimeout,
		dialRetries:    DefaultDialRetries,
		conns:          make(map[net.Conn]struct{}),
	}
	for _, o := range opts {
		o(t)
	}
	t.bufPool = &sync.Pool{
		New: func() any {
			b := make([]byte, proxyBufSize)
			return &b
		},
	}
	// Ensure the dialer uses the configured connect timeout.
	if nd, ok := t.dialer.(*netDialer); ok {
		nd.d.Timeout = t.connectTimeout
	}
	return t
}

// Start binds to lis (caller-owned) and begins accepting. Idempotent;
// returns ErrAlreadyStarted on a second call.
func (t *TCPListener) Start(lis net.Listener) error {
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return ErrTCPAlreadyStarted
	}
	if t.selector == nil || t.resolver == nil || t.stats == nil {
		t.mu.Unlock()
		return ErrTCPMisconfigured
	}
	t.listener = lis
	t.ctx, t.cancel = context.WithCancel(context.Background())
	t.started = true
	t.mu.Unlock()

	t.wg.Add(1)
	go t.acceptLoop()
	return nil
}

// Stop closes the listener and waits for goroutines + active
// connections to drain. Idempotent.
func (t *TCPListener) Stop() error {
	t.mu.Lock()
	if !t.started || t.stopped {
		t.mu.Unlock()
		return nil
	}
	t.stopped = true
	t.cancel()
	if t.listener != nil {
		_ = t.listener.Close()
	}
	t.mu.Unlock()

	// Close every live connection so any in-progress io.Copy unblocks.
	t.connMu.Lock()
	for c := range t.conns {
		_ = c.Close()
	}
	t.connMu.Unlock()

	t.wg.Wait()
	return nil
}

// acceptLoop runs in its own goroutine until the listener closes or
// the context is cancelled.
func (t *TCPListener) acceptLoop() {
	defer t.wg.Done()
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			if isClosedListener(err) {
				return
			}
			if t.sampler.Allow("accept") {
				t.logger.Warn("proxy tcp accept failed", zap.Error(err))
			}
			// Brief pause to avoid a tight loop on persistent errors.
			select {
			case <-t.ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		t.wg.Add(1)
		go t.handle(conn)
	}
}

// handle owns a single inbound connection from accept to close.
func (t *TCPListener) handle(client net.Conn) {
	defer t.wg.Done()

	t.trackConn(client)
	defer t.untrackConn(client)
	defer client.Close()

	if err := t.verifySource(client); err != nil {
		// Rejected: counter bumped inside verifySource; close cleanly.
		return
	}

	snap, ok := t.resolver.Resolve()
	if !ok {
		t.stats.ConnectErrors.Add(1)
		if t.sampler.Allow("no-service") {
			t.logger.Warn("proxy tcp service not resolvable")
		}
		return
	}
	t.stats.ConnectionsOpened.Add(1)
	t.stats.ConnectionsActive.Add(1)
	defer func() {
		t.stats.ConnectionsActive.Add(-1)
		t.stats.ConnectionsClosed.Add(1)
	}()

	// Synchronise SWRR with the snapshot's weights.
	weights := make(map[string]int32, len(snap.Backends))
	portMaps := make(map[string]map[string]string, len(snap.Backends))
	for _, b := range snap.Backends {
		if b.Weight > 0 {
			weights[b.Capsule] = b.Weight
			portMaps[b.Capsule] = b.PortMap
		}
	}
	t.ensureSWRR(weights)

	retries := t.dialRetries
	if retries < 0 {
		retries = 0
	}

	for attempt := 0; attempt <= retries; attempt++ {
		backendName, ok := t.pickBackend()
		if !ok {
			t.stats.ConnectErrors.Add(1)
			if t.sampler.Allow("no-backend") {
				t.logger.Warn("proxy tcp no backend available",
					zap.String("service", snap.ServiceID))
			}
			return
		}
		t.stats.AddPick(backendName)

		ep, err := t.selector.Pick(snap.ClusterPath, snap.GroupID, backendName)
		if err != nil {
			t.stats.ForwardErrorsNoReplica.Add(1)
			if t.sampler.Allow("no-replica") {
				t.logger.Warn("proxy tcp no healthy replica",
					zap.String("service", snap.ServiceID),
					zap.String("backend", backendName))
			}
			continue
		}

		port, found := selectNamedPort(ep, t.portName, portMaps[backendName])
		if !found {
			t.stats.ConnectErrors.Add(1)
			if t.sampler.Allow("no-port") {
				t.logger.Warn("proxy tcp port not exposed on replica",
					zap.String("service", snap.ServiceID),
					zap.String("backend", backendName),
					zap.String("replica", ep.ReplicaID))
			}
			continue
		}

		addr := net.JoinHostPort(ep.BridgeIP, portToString(port))
		ctx, cancel := context.WithTimeout(t.ctx, t.connectTimeout)
		backend, err := t.dialer.DialContext(ctx, "tcp", addr)
		cancel()
		if err != nil {
			t.selector.RecordDialFailure(backendName, ep.ReplicaID)
			t.stats.ConnectErrors.Add(1)
			t.stats.OutlierEjections.Add(1)
			if t.sampler.Allow("dial") {
				t.logger.Warn("proxy tcp dial failed",
					zap.String("service", snap.ServiceID),
					zap.String("backend", backendName),
					zap.String("replica", ep.ReplicaID),
					zap.String("addr", addr),
					zap.Error(err))
			}
			continue
		}
		t.forward(client, backend, backendName, ep.ReplicaID)
		return
	}
	// Exhausted retries.
	t.stats.ConnectErrors.Add(1)
}

// forward bidirectionally copies between client and backend until
// either direction closes. The idle timeout is enforced via rolling
// SetReadDeadline on each side.
func (t *TCPListener) forward(client, backend net.Conn, backendName, replicaID string) {
	defer backend.Close()
	t.trackConn(backend)
	defer t.untrackConn(backend)

	var fwdErr error
	var errMu sync.Mutex
	recordErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if fwdErr == nil {
			fwdErr = err
		}
		errMu.Unlock()
	}

	done := make(chan struct{}, 2)

	go func() {
		n, err := t.copyOneWay(backend, client, true)
		t.stats.BytesSent.Add(n)
		if !isExpectedCopyEnd(err) {
			recordErr(err)
		}
		_ = closeWrite(backend)
		_ = client.SetReadDeadline(time.Now())
		done <- struct{}{}
	}()
	go func() {
		n, err := t.copyOneWay(client, backend, false)
		t.stats.BytesReceived.Add(n)
		if !isExpectedCopyEnd(err) {
			recordErr(err)
		}
		_ = closeWrite(client)
		_ = backend.SetReadDeadline(time.Now())
		done <- struct{}{}
	}()
	<-done
	<-done

	if fwdErr != nil {
		t.stats.ForwardErrors.Add(1)
		t.selector.RecordForwardFailure(backendName, replicaID)
		t.stats.OutlierEjections.Add(1)
		if t.sampler.Allow("forward") {
			t.logger.Warn("proxy tcp forward failure",
				zap.String("backend", backendName),
				zap.String("replica", replicaID),
				zap.Error(fwdErr))
		}
	}
}

// copyOneWay copies src→dst with rolling idle deadlines on src and
// reuses a pooled buffer. The bool fromClient is for documentation
// at the call site only.
func (t *TCPListener) copyOneWay(dst, src net.Conn, fromClient bool) (int64, error) {
	_ = fromClient
	bufPtr := t.bufPool.Get().(*[]byte)
	defer t.bufPool.Put(bufPtr)
	buf := *bufPtr
	var total int64
	for {
		if t.idleTimeout > 0 {
			_ = src.SetReadDeadline(time.Now().Add(t.idleTimeout))
		}
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			total += int64(nw)
			if werr != nil {
				return total, werr
			}
			if nr != nw {
				return total, io.ErrShortWrite
			}
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// pickBackend reads the current SWRR; rebuilt-on-demand on the prior
// snapshot exchange.
func (t *TCPListener) pickBackend() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.swrr == nil {
		return "", false
	}
	return t.swrr.Pick()
}

// ensureSWRR rebuilds or hot-swaps the SWRR when the snapshot's
// weights differ from the cached set.
func (t *TCPListener) ensureSWRR(weights map[string]int32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.swrr == nil {
		t.swrr = NewSWRR(weights, nil)
		t.weights = copyWeights(weights)
		return
	}
	if !weightsEqual(t.weights, weights) {
		t.swrr.Update(weights)
		t.weights = copyWeights(weights)
	}
}

func (t *TCPListener) trackConn(c net.Conn) {
	t.connMu.Lock()
	t.conns[c] = struct{}{}
	t.connMu.Unlock()
}

func (t *TCPListener) untrackConn(c net.Conn) {
	t.connMu.Lock()
	delete(t.conns, c)
	t.connMu.Unlock()
}

// verifySource enforces the per-Service visibility policy on an
// inbound connection. Returns nil to admit, ErrVisibilityDenied to
// reject. When no source resolver is wired the policy is "admit
// every accepted connection" (the test-friendly default the proxy
// keeps from 11A).
func (t *TCPListener) verifySource(conn net.Conn) error {
	if t.srcResolver == nil {
		return nil
	}
	if err := VerifyTCPSource(conn, t.srcResolver, t.visibility); err != nil {
		if t.visStats != nil {
			t.visStats.IncDenied()
		}
		// Surface a per-Service counter increment as well so the
		// existing stats stream reflects the rejection.
		if t.stats != nil {
			t.stats.ConnectErrors.Add(1)
		}
		logVisibilityRejection(t.logger, t.sampler.Allow, snapshotServiceID(t.resolver), remoteAddrOf(conn))
		return err
	}
	return nil
}

// snapshotServiceID resolves the Service ID for log enrichment. We
// best-effort call Resolve() once; if the Service has already been
// torn down the empty string is fine for the log line.
func snapshotServiceID(r ServiceResolver) string {
	if r == nil {
		return ""
	}
	snap, ok := r.Resolve()
	if !ok {
		return ""
	}
	return snap.ServiceID
}

// ErrTCPAlreadyStarted is returned by TCPListener.Start on a second
// call.
var ErrTCPAlreadyStarted = errors.New("proxy: tcp listener already started")

// ErrTCPMisconfigured is returned by TCPListener.Start when required
// options were not supplied.
var ErrTCPMisconfigured = errors.New("proxy: tcp listener missing selector / resolver / stats")
