// This file implements the UDP forwarder for the L4 proxy (plan
// 11B.14). UDP is connectionless on the wire but the proxy still
// needs flow-level state: an SWRR pick per flow (not per packet) so a
// client's datagrams stay on the same backend; a return-path
// goroutine per flow that reads the backend's responses and forwards
// them back to the client; and an idle-flow sweeper that closes
// long-quiet flows. The implementation:
//
//   - Inbound loop: ReadFrom on the bound PacketConn; per source
//     address either look up an existing flow or create a new one
//     (SWRR + Selector pick + backend dial).
//   - Per-flow backend conn: a fresh PacketConn dialed once via the
//     Dialer. Each datagram from the client is written to backend;
//     a per-flow goroutine reads from backend and writes back to the
//     client.
//   - Idle expiry: a background sweeper iterates flows every
//     SweepInterval and closes any whose lastSeen is older than the
//     idle timeout.
//
// Failures are sampled through samplelog.Sampler — no per-packet logs
// (decision #28). Stop closes the public PacketConn, cancels the
// context, closes every flow backend, and waits for goroutines to
// exit.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/network/internal/samplelog"
)

// Defaults.
const (
	// DefaultUDPSweepInterval is how often the idle-flow sweeper runs.
	DefaultUDPSweepInterval = 30 * time.Second
	// udpReadBufSize is the per-packet read buffer. Standard MTU is
	// 1500; 65535 leaves room for jumbo-MTU paths.
	udpReadBufSize = 65535
)

// PacketDialer dials a connected UDP socket to a backend. Production
// wires net.Dialer; tests inject fakes.
type PacketDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.PacketConn, error)
}

// udpPacketDialer adapts net.Dialer.DialContext into the PacketDialer
// shape. net.Dialer.DialContext("udp", ...) returns *net.UDPConn,
// which satisfies net.PacketConn.
type udpPacketDialer struct{ d net.Dialer }

func (u *udpPacketDialer) DialContext(ctx context.Context, network, addr string) (net.PacketConn, error) {
	conn, err := u.d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	pc, ok := conn.(net.PacketConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy udp: dialer returned non-PacketConn %T", conn)
	}
	return pc, nil
}

// UDPListenerOption configures a UDPListener.
type UDPListenerOption func(*UDPListener)

// WithUDPLogger sets the zap logger.
func WithUDPLogger(l *zap.Logger) UDPListenerOption {
	return func(u *UDPListener) {
		if l != nil {
			u.logger = l
		}
	}
}

// WithUDPSelector wires the replica selector.
func WithUDPSelector(s *Selector) UDPListenerOption {
	return func(u *UDPListener) { u.selector = s }
}

// WithUDPResolver wires the service resolver.
func WithUDPResolver(r ServiceResolver) UDPListenerOption {
	return func(u *UDPListener) { u.resolver = r }
}

// WithUDPStats wires the per-Service stats.
func WithUDPStats(s *Stats) UDPListenerOption {
	return func(u *UDPListener) { u.stats = s }
}

// WithUDPDialer overrides the packet dialer.
func WithUDPDialer(d PacketDialer) UDPListenerOption {
	return func(u *UDPListener) {
		if d != nil {
			u.dialer = d
		}
	}
}

// WithUDPIdleTimeout overrides DefaultIdleTimeout.
func WithUDPIdleTimeout(d time.Duration) UDPListenerOption {
	return func(u *UDPListener) {
		if d > 0 {
			u.idleTimeout = d
		}
	}
}

// WithUDPSweepInterval overrides DefaultUDPSweepInterval.
func WithUDPSweepInterval(d time.Duration) UDPListenerOption {
	return func(u *UDPListener) {
		if d > 0 {
			u.sweepInterval = d
		}
	}
}

// WithUDPPortName binds the listener to a service-port name.
func WithUDPPortName(name string) UDPListenerOption {
	return func(u *UDPListener) { u.portName = name }
}

// WithUDPSampler injects the log sampler.
func WithUDPSampler(s *samplelog.Sampler) UDPListenerOption {
	return func(u *UDPListener) {
		if s != nil {
			u.sampler = s
		}
	}
}

// WithUDPNowFunc injects the clock used by the idle sweeper.
func WithUDPNowFunc(fn func() time.Time) UDPListenerOption {
	return func(u *UDPListener) {
		if fn != nil {
			u.now = fn
		}
	}
}

// WithUDPSourceResolver wires the visibility source-IP resolver.
func WithUDPSourceResolver(s SourceResolver) UDPListenerOption {
	return func(u *UDPListener) { u.srcResolver = s }
}

// WithUDPVisibility sets the Service-visibility policy for inbound
// packets.
func WithUDPVisibility(v ServiceVisibility) UDPListenerOption {
	return func(u *UDPListener) { u.visibility = v }
}

// WithUDPVisibilityStats injects the visibility-denied counter shared
// with the matching TCPListener.
func WithUDPVisibilityStats(v *VisibilityStats) UDPListenerOption {
	return func(u *UDPListener) {
		if v != nil {
			u.visStats = v
		}
	}
}

// udpFlow tracks one (client_ip, client_port) flow.
type udpFlow struct {
	backendName string
	replicaID   string
	backendConn net.PacketConn
	clientAddr  net.Addr
	lastSeen    atomic.Int64 // unix-nano
}

// UDPListener forwards inbound UDP to a SWRR/Selector-picked replica.
// One per (Service, service-port, bridge-gateway-IP).
type UDPListener struct {
	logger        *zap.Logger
	selector      *Selector
	resolver      ServiceResolver
	stats         *Stats
	dialer        PacketDialer
	sampler       *samplelog.Sampler
	idleTimeout   time.Duration
	sweepInterval time.Duration
	portName      string
	now           func() time.Time
	srcResolver   SourceResolver
	visibility    ServiceVisibility
	visStats      *VisibilityStats

	mu      sync.Mutex
	swrr    *SWRR
	weights map[string]int32

	pconn net.PacketConn

	flowMu sync.Mutex
	flows  map[string]*udpFlow

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
	stopped bool
}

// NewUDPListener constructs a UDPListener.
func NewUDPListener(opts ...UDPListenerOption) *UDPListener {
	u := &UDPListener{
		logger:        zap.NewNop(),
		dialer:        &udpPacketDialer{},
		sampler:       samplelog.NewSampler(),
		idleTimeout:   DefaultIdleTimeout,
		sweepInterval: DefaultUDPSweepInterval,
		now:           time.Now,
		flows:         make(map[string]*udpFlow),
	}
	for _, o := range opts {
		o(u)
	}
	return u
}

// Start binds to pconn and launches the read loop + sweeper.
// Idempotent; returns ErrUDPAlreadyStarted on a second call.
func (u *UDPListener) Start(pconn net.PacketConn) error {
	u.mu.Lock()
	if u.started {
		u.mu.Unlock()
		return ErrUDPAlreadyStarted
	}
	if u.selector == nil || u.resolver == nil || u.stats == nil {
		u.mu.Unlock()
		return ErrUDPMisconfigured
	}
	u.pconn = pconn
	u.ctx, u.cancel = context.WithCancel(context.Background())
	u.started = true
	u.mu.Unlock()

	u.wg.Add(2)
	go u.readLoop()
	go u.sweepLoop()
	return nil
}

// Stop closes the listener and drains every flow.
func (u *UDPListener) Stop() error {
	u.mu.Lock()
	if !u.started || u.stopped {
		u.mu.Unlock()
		return nil
	}
	u.stopped = true
	u.cancel()
	if u.pconn != nil {
		_ = u.pconn.Close()
	}
	u.mu.Unlock()

	u.flowMu.Lock()
	for k, f := range u.flows {
		_ = f.backendConn.Close()
		delete(u.flows, k)
	}
	u.flowMu.Unlock()

	u.wg.Wait()
	return nil
}

func (u *UDPListener) readLoop() {
	defer u.wg.Done()
	buf := make([]byte, udpReadBufSize)
	for {
		n, addr, err := u.pconn.ReadFrom(buf)
		if err != nil {
			if isClosedListener(err) {
				return
			}
			if u.sampler.Allow("udp-read") {
				u.logger.Warn("proxy udp read failed", zap.Error(err))
			}
			continue
		}
		if n == 0 {
			continue
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		u.handleDatagram(addr, payload)
	}
}

// handleDatagram routes a single inbound datagram to its flow's
// backend, creating a fresh flow if this is the first packet from
// addr.
func (u *UDPListener) handleDatagram(client net.Addr, payload []byte) {
	if !u.admitSource(client) {
		return
	}
	key := client.String()
	flow := u.getFlow(key)
	if flow == nil {
		flow = u.createFlow(client, key)
		if flow == nil {
			return
		}
	}
	flow.lastSeen.Store(u.now().UnixNano())
	if _, err := flow.backendConn.WriteTo(payload, nil); err != nil {
		// Connected PacketConn's WriteTo with nil addr should succeed;
		// any error is a forward failure.
		u.stats.ForwardErrors.Add(1)
		u.selector.RecordForwardFailure(flow.backendName, flow.replicaID)
		if u.sampler.Allow("udp-forward") {
			u.logger.Warn("proxy udp forward failed",
				zap.String("backend", flow.backendName),
				zap.String("replica", flow.replicaID),
				zap.Error(err))
		}
		u.removeFlow(key)
		return
	}
	u.stats.BytesSent.Add(int64(len(payload)))
}

func (u *UDPListener) getFlow(key string) *udpFlow {
	u.flowMu.Lock()
	defer u.flowMu.Unlock()
	return u.flows[key]
}

// createFlow opens a new backend connection for a fresh client. On
// any failure the flow is not stored — the next packet will retry.
func (u *UDPListener) createFlow(client net.Addr, key string) *udpFlow {
	snap, ok := u.resolver.Resolve()
	if !ok {
		u.stats.ConnectErrors.Add(1)
		if u.sampler.Allow("udp-no-service") {
			u.logger.Warn("proxy udp service not resolvable")
		}
		return nil
	}

	weights := make(map[string]int32, len(snap.Backends))
	portMaps := make(map[string]map[string]string, len(snap.Backends))
	for _, b := range snap.Backends {
		if b.Weight > 0 {
			weights[b.Capsule] = b.Weight
			portMaps[b.Capsule] = b.PortMap
		}
	}
	u.ensureSWRR(weights)

	backendName, ok := u.pickBackend()
	if !ok {
		u.stats.ConnectErrors.Add(1)
		if u.sampler.Allow("udp-no-backend") {
			u.logger.Warn("proxy udp no backend",
				zap.String("service", snap.ServiceID))
		}
		return nil
	}
	u.stats.AddPick(backendName)

	ep, err := u.selector.Pick(snap.ClusterPath, snap.GroupID, backendName)
	if err != nil {
		u.stats.ForwardErrorsNoReplica.Add(1)
		if u.sampler.Allow("udp-no-replica") {
			u.logger.Warn("proxy udp no replica",
				zap.String("service", snap.ServiceID),
				zap.String("backend", backendName))
		}
		return nil
	}

	port, found := selectNamedPort(ep, u.portName, portMaps[backendName])
	if !found {
		u.stats.ConnectErrors.Add(1)
		return nil
	}

	addr := net.JoinHostPort(ep.BridgeIP, portToString(port))
	ctx, cancel := context.WithTimeout(u.ctx, DefaultConnectTimeout)
	backendConn, err := u.dialer.DialContext(ctx, "udp", addr)
	cancel()
	if err != nil {
		u.selector.RecordDialFailure(backendName, ep.ReplicaID)
		u.stats.ConnectErrors.Add(1)
		u.stats.OutlierEjections.Add(1)
		if u.sampler.Allow("udp-dial") {
			u.logger.Warn("proxy udp dial failed",
				zap.String("addr", addr), zap.Error(err))
		}
		return nil
	}

	flow := &udpFlow{
		backendName: backendName,
		replicaID:   ep.ReplicaID,
		backendConn: backendConn,
		clientAddr:  client,
	}
	flow.lastSeen.Store(u.now().UnixNano())

	u.flowMu.Lock()
	// Re-check in case a concurrent createFlow already stored one for
	// this key (two simultaneous first-packets from the same source).
	if existing, ok := u.flows[key]; ok {
		_ = backendConn.Close()
		u.flowMu.Unlock()
		return existing
	}
	u.flows[key] = flow
	u.flowMu.Unlock()

	u.stats.ConnectionsOpened.Add(1)
	u.stats.ConnectionsActive.Add(1)
	u.wg.Add(1)
	go u.returnPath(flow, key)
	return flow
}

// returnPath copies datagrams from the backend back to the client.
// Exits when the backend is closed or the listener is stopped.
func (u *UDPListener) returnPath(flow *udpFlow, key string) {
	defer u.wg.Done()
	defer func() {
		u.stats.ConnectionsActive.Add(-1)
		u.stats.ConnectionsClosed.Add(1)
	}()
	buf := make([]byte, udpReadBufSize)
	for {
		if u.idleTimeout > 0 {
			_ = flow.backendConn.SetReadDeadline(time.Now().Add(u.idleTimeout))
		}
		n, _, err := flow.backendConn.ReadFrom(buf)
		if n > 0 {
			flow.lastSeen.Store(u.now().UnixNano())
			if _, werr := u.pconn.WriteTo(buf[:n], flow.clientAddr); werr != nil {
				if u.sampler.Allow("udp-write-client") {
					u.logger.Warn("proxy udp write to client failed",
						zap.Error(werr))
				}
				u.removeFlow(key)
				return
			}
			u.stats.BytesReceived.Add(int64(n))
		}
		if err != nil {
			if isClosedListener(err) || u.ctx.Err() != nil {
				u.removeFlow(key)
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				// Idle timeout on backend read — sweeper will pick the
				// flow up; exit cleanly.
				u.removeFlow(key)
				return
			}
			if u.sampler.Allow("udp-backend-read") {
				u.logger.Warn("proxy udp backend read failed",
					zap.String("backend", flow.backendName),
					zap.String("replica", flow.replicaID),
					zap.Error(err))
			}
			u.removeFlow(key)
			return
		}
	}
}

func (u *UDPListener) removeFlow(key string) {
	u.flowMu.Lock()
	flow, ok := u.flows[key]
	if ok {
		delete(u.flows, key)
	}
	u.flowMu.Unlock()
	if ok {
		_ = flow.backendConn.Close()
	}
}

// sweepLoop periodically reaps flows whose lastSeen is past the idle
// timeout window.
func (u *UDPListener) sweepLoop() {
	defer u.wg.Done()
	t := time.NewTicker(u.sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-u.ctx.Done():
			return
		case <-t.C:
			u.sweep()
		}
	}
}

func (u *UDPListener) sweep() {
	if u.idleTimeout <= 0 {
		return
	}
	now := u.now()
	threshold := now.Add(-u.idleTimeout)
	u.flowMu.Lock()
	var expired []*udpFlow
	for k, f := range u.flows {
		last := time.Unix(0, f.lastSeen.Load())
		if last.Before(threshold) {
			expired = append(expired, f)
			delete(u.flows, k)
		}
	}
	u.flowMu.Unlock()
	for _, f := range expired {
		_ = f.backendConn.Close()
	}
}

// pickBackend returns the next backend from SWRR.
func (u *UDPListener) pickBackend() (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.swrr == nil {
		return "", false
	}
	return u.swrr.Pick()
}

// ensureSWRR builds / hot-swaps the SWRR.
func (u *UDPListener) ensureSWRR(weights map[string]int32) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.swrr == nil {
		u.swrr = NewSWRR(weights, nil)
		u.weights = copyWeights(weights)
		return
	}
	if !weightsEqual(u.weights, weights) {
		u.swrr.Update(weights)
		u.weights = copyWeights(weights)
	}
}

// ActiveFlows returns the current count of tracked flows. Used by
// tests and diagnostics.
func (u *UDPListener) ActiveFlows() int {
	u.flowMu.Lock()
	defer u.flowMu.Unlock()
	return len(u.flows)
}

// admitSource enforces Service visibility on an inbound UDP packet
// before any flow / SWRR work runs. Returns true to admit, false to
// silently drop (a rejected datagram bumps the denied counter; UDP
// has no half-close to surface a rejection to the sender).
func (u *UDPListener) admitSource(addr net.Addr) bool {
	if u.srcResolver == nil {
		return true
	}
	src := netipFromNetAddr(addr)
	if err := VerifySource(src, u.srcResolver, u.visibility); err != nil {
		if u.visStats != nil {
			u.visStats.IncDenied()
		}
		if u.stats != nil {
			u.stats.ConnectErrors.Add(1)
		}
		logVisibilityRejection(u.logger, u.sampler.Allow, snapshotServiceID(u.resolver), src)
		return false
	}
	return true
}

// netipFromNetAddr converts a *net.UDPAddr / generic net.Addr to a
// netip.Addr without going through string round-tripping when the
// concrete type carries an IP.
func netipFromNetAddr(addr net.Addr) netip.Addr {
	if addr == nil {
		return netip.Addr{}
	}
	if ua, ok := addr.(*net.UDPAddr); ok {
		if a, ok := netip.AddrFromSlice(ua.IP); ok {
			return a.Unmap()
		}
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a
}

// ErrUDPAlreadyStarted is returned by UDPListener.Start on a second
// call.
var ErrUDPAlreadyStarted = errors.New("proxy: udp listener already started")

// ErrUDPMisconfigured is returned by UDPListener.Start when required
// options were not supplied.
var ErrUDPMisconfigured = errors.New("proxy: udp listener missing selector / resolver / stats")
