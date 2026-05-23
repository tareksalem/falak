// This file holds the type definitions and functional-options for
// the ProxyManager. Split out of lifecycle.go to keep that file
// focused on the reconciliation control flow and below the per-file
// LOC budget. No behaviour lives here — only data shapes, sentinel
// errors, and option setters.
package proxy

import (
	"errors"
	"net"
	"net/netip"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/network/endpoints"
	"github.com/tareksalem/falak/network/internal/samplelog"
)

// ServicePort is the proxy-local view of one Service ingress port.
// Mirrors service.ServicePort to keep the wiring layer's translation
// trivial.
type ServicePort struct {
	// Name is the DNS-friendly port handle (e.g. "http").
	Name string
	// Port is the TCP/UDP port number.
	Port uint16
	// Protocol is "tcp" or "udp" (lower-case). External protocols are
	// rejected at EnsureService.
	Protocol string
}

// ServiceSpec is the proxy-local snapshot of a Service used by the
// lifecycle reconciler. Only fields the listener layer cares about
// are captured: identity, scope, ports, visibility. Backends and
// strategy live in the StrategyGetter / ServiceResolver wiring.
type ServiceSpec struct {
	// ID is the stable Service identifier.
	ID string
	// Name is the operator-facing Service name (used in logs only).
	Name string
	// ClusterPath / GroupID locate the Service in the registry.
	ClusterPath string
	GroupID     string
	// Visibility is "cluster" or "group". "external" must be rejected.
	Visibility string
	// Ports is the per-port listener set the lifecycle must materialise.
	Ports []ServicePort
}

// StrategyGetter is the proxy-side handle on per-Service strategy
// engines. The lifecycle owns no strategies of its own — the service
// manager creates one Engine per Service and exposes it through this
// getter. The TCPListener / UDPListener consult the engine indirectly
// via the ServiceResolver wired in 11B.21.
//
// The Engine result is intentionally typed as `any` so the proxy
// package keeps zero compile-time dependency on the service package.
// The wiring layer (11B.21) type-asserts to strategy.Engine at the
// resolver-builder boundary. See 11B.18 plan note: this is a
// dependency-injection seam, not a runtime concern of the lifecycle.
type StrategyGetter interface {
	Strategy(serviceID string) (engine any, ok bool)
}

// ProxyEventSource is the lifecycle's event-feed interface. The
// wiring layer (11B.21) implements it on top of the service-manager
// + bridge-manager event buses; this package defines the contract so
// the lifecycle does not import either.
type ProxyEventSource interface {
	// SubscribeServiceEvents delivers (kind, spec) pairs to fn until
	// the returned cancel func is called. kind is "create", "update",
	// or "delete".
	SubscribeServiceEvents(fn func(kind string, spec ServiceSpec)) (cancel func())
	// SubscribeBridgeEvents delivers (added, addr) pairs to fn until
	// the returned cancel func is called.
	SubscribeBridgeEvents(fn func(added bool, addr netip.Addr)) (cancel func())
}

// BridgeGatewayProvider returns the current set of per-group bridge
// gateway IPs the proxy must listen on. The lifecycle calls it on
// EnsureService to populate listeners across every bridge.
type BridgeGatewayProvider func() []netip.Addr

// EndpointSelectorBuilder constructs the replica Selector for a given
// Service. The proxy package keeps the Selector behind a builder so
// the wiring layer can swap in a per-Service outlier-tracker.
type EndpointSelectorBuilder func(serviceID string) *Selector

// ServiceResolverBuilder yields the ServiceResolver passed to each
// TCPListener / UDPListener. The wiring layer composes the resolver
// from (Service spec, strategy engine, endpoint registry).
type ServiceResolverBuilder func(serviceID string) ServiceResolver

// ManagerOption configures a ProxyManager.
type ManagerOption func(*ProxyManager)

// WithManagerLogger sets the zap logger. Defaults to a no-op.
func WithManagerLogger(l *zap.Logger) ManagerOption {
	return func(m *ProxyManager) {
		if l != nil {
			m.logger = l
		}
	}
}

// WithManagerStats wires the per-Service stats registry. Required.
func WithManagerStats(r *StatsRegistry) ManagerOption {
	return func(m *ProxyManager) { m.stats = r }
}

// WithManagerSelectorBuilder wires the replica-selector factory. The
// builder is called once per Service on EnsureService.
func WithManagerSelectorBuilder(b EndpointSelectorBuilder) ManagerOption {
	return func(m *ProxyManager) { m.selectorBuilder = b }
}

// WithManagerResolverBuilder wires the per-Service resolver factory.
// Called once per Service on EnsureService.
func WithManagerResolverBuilder(b ServiceResolverBuilder) ManagerOption {
	return func(m *ProxyManager) { m.resolverBuilder = b }
}

// WithManagerStrategies wires the strategy getter the wiring layer
// uses to feed LiveWeights into the resolver. See package note: the
// lifecycle does not consume it directly; it stores the handle for
// the 11B.21 resolver builder.
func WithManagerStrategies(g StrategyGetter) ManagerOption {
	return func(m *ProxyManager) { m.strategies = g }
}

// WithManagerRegistry wires the endpoints registry the per-Service
// resolver consults for replica IPs.
func WithManagerRegistry(r *endpoints.Registry) ManagerOption {
	return func(m *ProxyManager) { m.registry = r }
}

// WithManagerSourceResolver wires the bridge-source IP resolver the
// listeners use for visibility checks.
func WithManagerSourceResolver(s SourceResolver) ManagerOption {
	return func(m *ProxyManager) { m.srcResolver = s }
}

// WithManagerBridgeGateways wires the provider that returns the
// current per-group bridge gateway IPs. EnsureService calls it
// snapshot-style on every reconcile.
func WithManagerBridgeGateways(fn BridgeGatewayProvider) ManagerOption {
	return func(m *ProxyManager) {
		if fn != nil {
			m.bridges = fn
		}
	}
}

// WithManagerEventSource wires the optional event-feed source. When
// unset the lifecycle is purely driven by explicit calls.
func WithManagerEventSource(src ProxyEventSource) ManagerOption {
	return func(m *ProxyManager) { m.events = src }
}

// WithManagerSampler injects the shared log sampler.
func WithManagerSampler(s *samplelog.Sampler) ManagerOption {
	return func(m *ProxyManager) {
		if s != nil {
			m.sampler = s
		}
	}
}

// ErrManagerStopped is returned when EnsureService / ReleaseService
// is called after Stop.
var ErrManagerStopped = errors.New("proxy manager: stopped")

// ErrExternalVisibilityNotSupported is returned by EnsureService when
// a Service declares the deferred "external" visibility scope.
var ErrExternalVisibilityNotSupported = errors.New("proxy manager: external visibility not supported")

// ErrManagerMisconfigured is returned by Start when required options
// were not supplied.
var ErrManagerMisconfigured = errors.New("proxy manager: missing stats / selector / resolver builder")

// listenerKey is the unique handle on one (Service, port, bridge)
// tuple. Map-friendly value type.
type listenerKey struct {
	serviceID string
	portName  string
	bridgeIP  netip.Addr
}

// proxyEntry holds one TCP-or-UDP listener and the metadata the
// reconciler needs to tear it down.
type proxyEntry struct {
	key      listenerKey
	protocol string // "tcp" or "udp"
	tcp      *TCPListener
	udp      *UDPListener
	listener net.Listener   // tcp socket (when protocol=="tcp")
	pconn    net.PacketConn // udp socket (when protocol=="udp")
}

// desiredKeys computes the (Service, port, bridge) tuples that must
// exist for spec given the current gateway set.
func desiredKeys(spec ServiceSpec, gateways []netip.Addr) map[listenerKey]struct{} {
	out := make(map[listenerKey]struct{}, len(spec.Ports)*len(gateways))
	for _, g := range gateways {
		for _, p := range spec.Ports {
			out[listenerKey{serviceID: spec.ID, portName: p.Name, bridgeIP: g}] = struct{}{}
		}
	}
	return out
}

// findPort returns the named port in spec.Ports.
func findPort(spec ServiceSpec, name string) (ServicePort, bool) {
	for _, p := range spec.Ports {
		if p.Name == name {
			return p, true
		}
	}
	return ServicePort{}, false
}

// closeEntry stops the listener inside entry and closes its socket.
// Safe to call on a partially-initialised entry.
func closeEntry(entry *proxyEntry) {
	if entry == nil {
		return
	}
	if entry.tcp != nil {
		_ = entry.tcp.Stop()
	}
	if entry.udp != nil {
		_ = entry.udp.Stop()
	}
	if entry.listener != nil {
		_ = entry.listener.Close()
	}
	if entry.pconn != nil {
		_ = entry.pconn.Close()
	}
}
