package dns

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"

	"github.com/tareksalem/falak/network/endpoints"
	"github.com/tareksalem/falak/network/internal/samplelog"
)

// DNSPort is the well-known DNS port. Exported so callers wiring a
// Server into integration tests can build the same listen address the
// responder uses.
const DNSPort = 53

// DefaultTTLSeconds is the TTL Falak attaches to every A record it
// emits. Five seconds aligns with the gossip refresh cadence: a
// listener that caches an answer for one TTL window cannot fall more
// than ~5s behind the registry's view of liveness. See
// service-networking decision #12.
const DefaultTTLSeconds = 5

// EndpointRegistry is the read-only slice of *endpoints.Registry the
// DNS server depends on. Declaring an interface keeps the test suite
// free of registry construction.
type EndpointRegistry interface {
	Lookup(clusterPath, groupID, capsuleName string) []endpoints.Endpoint
}

// BridgeMapFunc maps a listen-socket local address to the
// (clusterPath, groupID) the bridge serves. The DNS server uses it to
// turn "this query arrived on socket X" into the BridgeInfo a
// VisibilityPredicate consumes.
type BridgeMapFunc func(addr netip.Addr) (clusterPath, groupID string, ok bool)

// Sentinel errors for lifecycle misuse. Callers check via errors.Is.
var (
	// ErrAlreadyStarted is returned when Start is called twice on the
	// same Server.
	ErrAlreadyStarted = errors.New("dns: server already started")
	// ErrNotStarted is returned when AddListener/RemoveListener/Stop
	// are called before Start.
	ErrNotStarted = errors.New("dns: server not started")
)

// Server is Falak's per-node DNS responder. It maintains one UDP and
// one TCP listener per per-group bridge IP and answers A queries by
// consulting the local endpoints.Registry through a visibility
// predicate.
type Server struct {
	logger     *zap.Logger
	registry   EndpointRegistry
	predicate  VisibilityPredicate
	bridgeMap  BridgeMapFunc
	svcResolve ServiceResolver
	ttlSeconds int
	bindAddrs  []netip.Addr
	randSource func() *rand.Rand
	port       int

	mu        sync.Mutex
	started   bool
	stopped   bool
	listeners map[netip.Addr]*listenerPair
	wg        sync.WaitGroup

	// errSampler bounds per-query / per-connection error logs. A
	// misbehaving caller can spray malformed packets at the responder;
	// unsampled logs would amplify the attack.
	errSampler *samplelog.Sampler
}

// listenerPair holds the UDP and TCP servers bound to a single
// gateway IP. miekg/dns servers are paired one-per-protocol; we keep
// both so Stop closes them together.
type listenerPair struct {
	udp *dns.Server
	tcp *dns.Server
}

// Option configures a Server.
type Option func(*Server)

// WithRegistry sets the endpoint registry the server consults.
func WithRegistry(r EndpointRegistry) Option {
	return func(s *Server) { s.registry = r }
}

// WithVisibility sets the predicate the server applies before
// returning a record. Defaults to DefaultPredicate.
func WithVisibility(p VisibilityPredicate) Option {
	return func(s *Server) {
		if p != nil {
			s.predicate = p
		}
	}
}

// WithLogger sets the zap logger.
func WithLogger(l *zap.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithTTLSeconds overrides DefaultTTLSeconds. Values <= 0 are ignored.
func WithTTLSeconds(ttl int) Option {
	return func(s *Server) {
		if ttl > 0 {
			s.ttlSeconds = ttl
		}
	}
}

// WithBindAddrs sets the initial bind addresses. When empty the
// server starts with the single fixed link-local address.
func WithBindAddrs(addrs []netip.Addr) Option {
	return func(s *Server) {
		if len(addrs) > 0 {
			out := make([]netip.Addr, len(addrs))
			copy(out, addrs)
			s.bindAddrs = out
		}
	}
}

// WithBridgeMap sets the listen-socket → bridge identity resolver.
// Without one, every query is treated as unknown-caller and the
// default predicate rejects it.
func WithBridgeMap(fn BridgeMapFunc) Option {
	return func(s *Server) { s.bridgeMap = fn }
}

// WithServiceResolver wires the Service-name lookup adapter. When set,
// every A query first consults the resolver; on a visibility-admitted
// match the resolver's verdict (trivial replica IPs vs proxy IP)
// shapes the response. When unset the responder behaves as in 11A —
// every A query falls straight through to the capsule-name path.
func WithServiceResolver(r ServiceResolver) Option {
	return func(s *Server) {
		if r != nil {
			s.svcResolve = r
		}
	}
}

// WithRandSource injects a per-call *rand.Rand factory. Tests use it
// to make A-record shuffling deterministic.
func WithRandSource(fn func() *rand.Rand) Option {
	return func(s *Server) {
		if fn != nil {
			s.randSource = fn
		}
	}
}

// WithPort overrides the default well-known DNS port. Tests use this
// to bind on an unprivileged ephemeral port (pass 0 for a
// kernel-assigned port). Production wiring always uses DNSPort.
func WithPort(p int) Option {
	return func(s *Server) {
		if p >= 0 {
			s.port = p
		}
	}
}

// NewServer builds a Server. Call Start to begin serving.
func NewServer(opts ...Option) *Server {
	defAddr, _ := netip.ParseAddr(LinkLocalDNSAddr)
	s := &Server{
		logger:     zap.NewNop(),
		predicate:  DefaultPredicate,
		svcResolve: NoopServiceResolver{},
		ttlSeconds: DefaultTTLSeconds,
		bindAddrs:  []netip.Addr{defAddr},
		listeners:  make(map[netip.Addr]*listenerPair),
		port:       DNSPort,
		randSource: func() *rand.Rand {
			return rand.New(rand.NewSource(time.Now().UnixNano()))
		},
		errSampler: samplelog.NewSampler(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start opens UDP+TCP listeners on every configured bind address.
// Returns ErrAlreadyStarted if Start has been called before.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return ErrAlreadyStarted
	}
	if s.stopped {
		s.mu.Unlock()
		return fmt.Errorf("dns: server already stopped")
	}
	s.started = true
	addrs := append([]netip.Addr(nil), s.bindAddrs...)
	s.mu.Unlock()

	for _, addr := range addrs {
		if err := s.AddListener(ctx, addr); err != nil {
			// Roll back partial binds: caller gets a clean failure.
			_ = s.Stop()
			return err
		}
	}
	return nil
}

// AddListener binds UDP+TCP on (addr, 53). Idempotent: a second call
// for the same address returns nil without opening new sockets.
func (s *Server) AddListener(ctx context.Context, addr netip.Addr) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return ErrNotStarted
	}
	if s.stopped {
		s.mu.Unlock()
		return fmt.Errorf("dns: server stopped")
	}
	if _, exists := s.listeners[addr]; exists {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	pair, err := s.buildListenerPair(addr)
	if err != nil {
		return err
	}

	s.mu.Lock()
	// Re-check in case a concurrent AddListener won the race.
	if _, exists := s.listeners[addr]; exists {
		s.mu.Unlock()
		shutdownPair(pair)
		return nil
	}
	s.listeners[addr] = pair
	s.mu.Unlock()

	s.wg.Add(2)
	startedUDP := make(chan error, 1)
	startedTCP := make(chan error, 1)
	pair.udp.NotifyStartedFunc = func() { close(startedUDP) }
	pair.tcp.NotifyStartedFunc = func() { close(startedTCP) }

	go func() {
		defer s.wg.Done()
		if err := pair.udp.ActivateAndServe(); err != nil {
			s.logger.Debug("dns: udp listener exit",
				zap.String("addr", addr.String()),
				zap.Error(err))
		}
	}()
	go func() {
		defer s.wg.Done()
		if err := pair.tcp.ActivateAndServe(); err != nil {
			s.logger.Debug("dns: tcp listener exit",
				zap.String("addr", addr.String()),
				zap.Error(err))
		}
	}()

	// Wait until both listeners report ready or context cancels.
	if err := waitStarted(ctx, startedUDP); err != nil {
		s.removeListenerLocked(addr)
		return fmt.Errorf("dns: udp listener for %s: %w", addr, err)
	}
	if err := waitStarted(ctx, startedTCP); err != nil {
		s.removeListenerLocked(addr)
		return fmt.Errorf("dns: tcp listener for %s: %w", addr, err)
	}

	s.logger.Info("dns: listener added", zap.String("addr", addr.String()))
	return nil
}

// RemoveListener closes UDP+TCP for addr. Idempotent.
func (s *Server) RemoveListener(addr netip.Addr) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return ErrNotStarted
	}
	pair, ok := s.listeners[addr]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.listeners, addr)
	s.mu.Unlock()

	shutdownPair(pair)
	s.logger.Info("dns: listener removed", zap.String("addr", addr.String()))
	return nil
}

// Stop closes every listener and waits for all goroutines to exit.
// Idempotent. Stop after Stop returns nil.
func (s *Server) Stop() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	if s.stopped {
		s.mu.Unlock()
		s.wg.Wait()
		return nil
	}
	s.stopped = true
	pairs := make([]*listenerPair, 0, len(s.listeners))
	for addr, p := range s.listeners {
		pairs = append(pairs, p)
		delete(s.listeners, addr)
	}
	s.mu.Unlock()

	for _, p := range pairs {
		shutdownPair(p)
	}
	s.wg.Wait()
	return nil
}

// ListenerAddrs returns a snapshot of currently bound addresses. Used
// by tests and diagnostics.
func (s *Server) ListenerAddrs() []netip.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]netip.Addr, 0, len(s.listeners))
	for a := range s.listeners {
		out = append(out, a)
	}
	return out
}

// UDPListenAddr returns the *net.UDPAddr the server bound on for
// addr. Tests use this to discover the kernel-assigned port when the
// server is started with WithPort(0). Returns nil when addr is not
// currently bound.
func (s *Server) UDPListenAddr(addr netip.Addr) *net.UDPAddr {
	s.mu.Lock()
	defer s.mu.Unlock()
	pair, ok := s.listeners[addr]
	if !ok || pair == nil || pair.udp == nil || pair.udp.PacketConn == nil {
		return nil
	}
	if ua, ok := pair.udp.PacketConn.LocalAddr().(*net.UDPAddr); ok {
		return ua
	}
	return nil
}

// buildListenerPair opens UDP+TCP sockets on (addr, 53) and wraps
// them in *dns.Server instances ready to be activated. When the
// configured port is 0 (tests), the UDP socket's kernel-assigned port
// is reused for TCP so both ends share an address.
func (s *Server) buildListenerPair(addr netip.Addr) (*listenerPair, error) {
	listenAddr := net.JoinHostPort(addr.String(), fmt.Sprintf("%d", s.port))
	udpConn, err := net.ListenPacket("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("dns: udp listen %s: %w", listenAddr, err)
	}
	tcpAddr := listenAddr
	if s.port == 0 {
		// Reuse the UDP-assigned port for the TCP listener so callers
		// have one (addr, port) to talk to.
		if ua, ok := udpConn.LocalAddr().(*net.UDPAddr); ok {
			tcpAddr = net.JoinHostPort(addr.String(), fmt.Sprintf("%d", ua.Port))
		}
	}
	tcpListener, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("dns: tcp listen %s: %w", tcpAddr, err)
	}

	handler := s.makeHandler()
	pair := &listenerPair{
		udp: &dns.Server{
			PacketConn: udpConn,
			Handler:    handler,
		},
		tcp: &dns.Server{
			Listener: tcpListener,
			Handler:  handler,
		},
	}
	return pair, nil
}

// makeHandler returns the dns.Handler that resolves every query
// against the registry.
func (s *Server) makeHandler() dns.Handler {
	return dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		s.handle(w, req)
	})
}

// handle is the responder core. It always replies (NXDOMAIN, SERVFAIL,
// or success) so the caller is never left waiting on a dropped packet.
func (s *Server) handle(w dns.ResponseWriter, req *dns.Msg) {
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true

	if len(req.Question) == 0 {
		resp.Rcode = dns.RcodeFormatError
		_ = w.WriteMsg(resp)
		return
	}
	q := req.Question[0]

	if q.Qclass != dns.ClassINET || q.Qtype != dns.TypeA {
		// We only serve A/IN today. AAAA is intentionally NOERROR with
		// no answers so glibc doesn't escalate to NXDOMAIN.
		_ = w.WriteMsg(resp)
		return
	}

	callerAddr := localAddrOf(w)
	caller, ok := s.callerBridge(callerAddr)
	if !ok {
		if s.errSampler.Allow("unknown_caller") {
			s.logger.Warn("dns: unknown caller bridge",
				zap.Stringer("addr", callerAddr),
				zap.Int64("dropped", s.errSampler.Suppressed("unknown_caller")))
		}
		resp.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(resp)
		return
	}

	capsuleName, _, parseOK := parseQName(q.Name)
	if !parseOK {
		resp.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(resp)
		return
	}

	// Phase 11B: try the Service path first. The resolver applies
	// visibility internally; not-found and visibility-denied collapse
	// to the same (zero,false) return so the responder cannot leak
	// existence to forbidden callers.
	if info, ok := s.svcResolve.LookupService(caller.ClusterPath, caller.GroupID, capsuleName); ok {
		s.answerService(w, resp, q, caller, info, capsuleName)
		return
	}

	// Phase 11A: capsule names live in the caller's own group. The
	// predicate is the gate.
	if !s.predicate(caller, caller.GroupID) {
		resp.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(resp)
		return
	}

	if s.registry == nil {
		resp.Rcode = dns.RcodeServerFailure
		_ = w.WriteMsg(resp)
		return
	}

	eps := s.registry.Lookup(caller.ClusterPath, caller.GroupID, capsuleName)
	if len(eps) == 0 {
		resp.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(resp)
		return
	}

	ips := make([]net.IP, 0, len(eps))
	for _, ep := range eps {
		ip := net.ParseIP(ep.BridgeIP)
		if ip == nil {
			continue
		}
		ips = append(ips, ip)
	}
	if len(ips) == 0 {
		resp.Rcode = dns.RcodeNameError
		_ = w.WriteMsg(resp)
		return
	}

	rng := s.randSource()
	rng.Shuffle(len(ips), func(i, j int) { ips[i], ips[j] = ips[j], ips[i] })

	ttl := uint32(s.ttlSeconds)
	for _, ip := range ips {
		rr := &dns.A{
			Hdr: dns.RR_Header{
				Name:   q.Name,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    ttl,
			},
			A: ip,
		}
		resp.Answer = append(resp.Answer, rr)
	}

	if err := w.WriteMsg(resp); err != nil {
		if s.errSampler.Allow("write_response") {
			s.logger.Warn("dns: write response failed",
				zap.String("capsule", capsuleName),
				zap.Int64("dropped", s.errSampler.Suppressed("write_response")),
				zap.Error(err))
		}
	}
}

// callerBridge resolves the listen-socket address into a BridgeInfo
// via the configured BridgeMapFunc. Without a map every query is
// unknown.
func (s *Server) callerBridge(addr netip.Addr) (BridgeInfo, bool) {
	if s.bridgeMap == nil {
		return BridgeInfo{}, false
	}
	clusterPath, groupID, ok := s.bridgeMap(addr)
	if !ok {
		return BridgeInfo{}, false
	}
	return BridgeInfo{
		ClusterPath: clusterPath,
		GroupID:     groupID,
		IP:          addr,
	}, true
}

// removeListenerLocked tears down a half-opened listener pair when
// AddListener's wait-for-ready leg fails.
func (s *Server) removeListenerLocked(addr netip.Addr) {
	s.mu.Lock()
	pair, ok := s.listeners[addr]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.listeners, addr)
	s.mu.Unlock()
	shutdownPair(pair)
}

// shutdownPair best-effort closes both members of a listenerPair.
func shutdownPair(p *listenerPair) {
	if p == nil {
		return
	}
	if p.udp != nil {
		_ = p.udp.Shutdown()
	}
	if p.tcp != nil {
		_ = p.tcp.Shutdown()
	}
}

// waitStarted blocks until ch closes or ctx fires.
func waitStarted(ctx context.Context, ch chan error) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// localAddrOf returns the listen-socket local address as a netip.Addr.
// Falls back to the unspecified address when the underlying conn does
// not expose a usable LocalAddr.
func localAddrOf(w dns.ResponseWriter) netip.Addr {
	la := w.LocalAddr()
	if la == nil {
		return netip.Addr{}
	}
	host, _, err := net.SplitHostPort(la.String())
	if err != nil {
		return netip.Addr{}
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a
}

// parseQName splits a DNS qname into capsule + optional port-name
// suffix. Returns (capsuleName, portName, ok). Phase 11A only consumes
// the capsule name; the parser already accepts the two-label form so
// the Service work in Phase 11B reuses it.
func parseQName(qname string) (string, string, bool) {
	name := strings.TrimSuffix(strings.ToLower(qname), ".")
	if name == "" {
		return "", "", false
	}
	parts := strings.Split(name, ".")
	switch len(parts) {
	case 1:
		if parts[0] == "" {
			return "", "", false
		}
		return parts[0], "", true
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return "", "", false
		}
		return parts[0], parts[1], true
	default:
		return "", "", false
	}
}
