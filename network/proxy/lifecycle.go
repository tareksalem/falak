// This file implements the per-node proxy lifecycle (plan 11B.18).
// One ProxyManager instance owns every TCP/UDP listener on the node.
// Listeners are keyed on (serviceID, portName, bridgeIP); the manager
// reconciles desired state (the active Service set × the active
// bridge gateway set) into the live listener map.
//
// The manager is driven by:
//
//   - Explicit calls: EnsureService / ReleaseService / OnBridgeAdded /
//     OnBridgeRemoved. The wiring layer (11B.21) invokes these from
//     the service-event subscriber and the bridge-event subscriber.
//   - Start / Stop for whole-node lifecycle.
//
// All entry points are idempotent — repeated calls converge to the
// same listener set with no leaked goroutines (reaper-safe per
// IMPLEMENTATION_RULES). Stop closes every listener, drains in-flight
// connections via TCPListener.Stop / UDPListener.Stop, and waits.
//
// Type definitions, functional options and helpers live in
// lifecycle_types.go to keep this file focused on the reconciler.
package proxy

import (
	"fmt"
	"net/netip"
	"sync"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/network/endpoints"
	"github.com/tareksalem/falak/network/internal/samplelog"
)

// ProxyManager owns every per-Service listener on the node and
// reconciles them in response to Service and bridge events. Safe for
// concurrent use; every external entry point holds the same mutex.
type ProxyManager struct {
	logger          *zap.Logger
	stats           *StatsRegistry
	selectorBuilder EndpointSelectorBuilder
	resolverBuilder ServiceResolverBuilder
	strategies      StrategyGetter
	registry        *endpoints.Registry
	srcResolver     SourceResolver
	bridges         BridgeGatewayProvider
	events          ProxyEventSource
	sampler         *samplelog.Sampler

	mu        sync.Mutex
	listeners map[listenerKey]*proxyEntry
	services  map[string]ServiceSpec // last-known spec per serviceID
	cancels   []func()
	started   bool
	stopped   bool
}

// NewProxyManager builds a ProxyManager. Call Start to begin
// subscribing and reconciling.
func NewProxyManager(opts ...ManagerOption) *ProxyManager {
	m := &ProxyManager{
		logger:    zap.NewNop(),
		bridges:   func() []netip.Addr { return nil },
		sampler:   samplelog.NewSampler(),
		listeners: make(map[listenerKey]*proxyEntry),
		services:  make(map[string]ServiceSpec),
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Start spins up the event subscribers (if an event source is wired).
// Required options: stats, selectorBuilder, resolverBuilder. Returns
// ErrManagerMisconfigured when any required option is missing.
func (m *ProxyManager) Start() error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	if m.stats == nil || m.selectorBuilder == nil || m.resolverBuilder == nil {
		m.mu.Unlock()
		return ErrManagerMisconfigured
	}
	m.started = true
	m.mu.Unlock()

	if m.events != nil {
		cancelSvc := m.events.SubscribeServiceEvents(m.handleServiceEvent)
		cancelBr := m.events.SubscribeBridgeEvents(m.handleBridgeEvent)
		m.mu.Lock()
		m.cancels = append(m.cancels, cancelSvc, cancelBr)
		m.mu.Unlock()
	}
	m.logger.Info("proxy manager started")
	return nil
}

// EnsureService creates / updates the listener set for spec.
// Idempotent: invoking with an unchanged spec is a no-op. Visibility
// "external" is rejected with ErrExternalVisibilityNotSupported.
func (m *ProxyManager) EnsureService(spec ServiceSpec) error {
	if spec.Visibility == "external" {
		return ErrExternalVisibilityNotSupported
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return ErrManagerStopped
	}
	m.services[spec.ID] = spec
	gateways := m.snapshotGateways()
	desired := desiredKeys(spec, gateways)
	currentForSvc := m.keysForServiceLocked(spec.ID)
	m.mu.Unlock()

	// Open every desired key that is not already up.
	for k := range desired {
		if _, ok := currentForSvc[k]; ok {
			continue
		}
		if err := m.openListener(spec, k); err != nil {
			return fmt.Errorf("open listener %s/%s on %s: %w", spec.ID, k.portName, k.bridgeIP, err)
		}
	}
	// Close any current key for this Service that is no longer desired
	// (e.g. port removed, bridge removed).
	for k := range currentForSvc {
		if _, ok := desired[k]; ok {
			continue
		}
		m.closeListener(k)
	}
	m.logger.Info("proxy: service ensured",
		zap.String("service", spec.ID),
		zap.Int("ports", len(spec.Ports)),
		zap.Int("bridges", len(gateways)))
	return nil
}

// ReleaseService stops every listener owned by serviceID. Idempotent;
// calling on an unknown serviceID returns nil.
func (m *ProxyManager) ReleaseService(serviceID string) error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	keys := m.keysForServiceLocked(serviceID)
	delete(m.services, serviceID)
	m.mu.Unlock()

	for k := range keys {
		m.closeListener(k)
	}
	if m.stats != nil {
		m.stats.Delete(serviceID)
	}
	m.logger.Info("proxy: service released", zap.String("service", serviceID))
	return nil
}

// OnBridgeAdded spins up listeners for every active Service on the
// new bridge gateway IP. Idempotent — listeners already up are not
// recreated.
func (m *ProxyManager) OnBridgeAdded(addr netip.Addr) error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	specs := make([]ServiceSpec, 0, len(m.services))
	for _, s := range m.services {
		specs = append(specs, s)
	}
	m.mu.Unlock()

	for _, spec := range specs {
		for _, p := range spec.Ports {
			k := listenerKey{serviceID: spec.ID, portName: p.Name, bridgeIP: addr}
			m.mu.Lock()
			_, exists := m.listeners[k]
			m.mu.Unlock()
			if exists {
				continue
			}
			if err := m.openListener(spec, k); err != nil {
				return fmt.Errorf("bridge-added %s on %s: %w", spec.ID, addr, err)
			}
		}
	}
	m.logger.Info("proxy: bridge added", zap.String("addr", addr.String()))
	return nil
}

// OnBridgeRemoved closes every listener that was bound to addr.
// Idempotent.
func (m *ProxyManager) OnBridgeRemoved(addr netip.Addr) error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	var keys []listenerKey
	for k := range m.listeners {
		if k.bridgeIP == addr {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()

	for _, k := range keys {
		m.closeListener(k)
	}
	m.logger.Info("proxy: bridge removed", zap.String("addr", addr.String()))
	return nil
}

// Stop closes every listener and waits for goroutines and in-flight
// connections to drain. Idempotent.
func (m *ProxyManager) Stop() error {
	m.mu.Lock()
	if !m.started || m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.stopped = true
	cancels := m.cancels
	m.cancels = nil
	keys := make([]listenerKey, 0, len(m.listeners))
	for k := range m.listeners {
		keys = append(keys, k)
	}
	m.mu.Unlock()

	for _, c := range cancels {
		c()
	}
	for _, k := range keys {
		m.closeListener(k)
	}
	m.logger.Info("proxy manager stopped")
	return nil
}

// ActiveListeners returns a snapshot of currently bound listener
// keys. Used by tests and diagnostics.
func (m *ProxyManager) ActiveListeners() []listenerKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]listenerKey, 0, len(m.listeners))
	for k := range m.listeners {
		out = append(out, k)
	}
	return out
}

// handleServiceEvent is the subscriber the event source calls.
func (m *ProxyManager) handleServiceEvent(kind string, spec ServiceSpec) {
	switch kind {
	case "create", "update":
		if err := m.EnsureService(spec); err != nil {
			if m.sampler.Allow("ensure-service") {
				m.logger.Warn("proxy: ensure service failed",
					zap.String("service", spec.ID), zap.Error(err))
			}
		}
	case "delete":
		_ = m.ReleaseService(spec.ID)
	default:
		if m.sampler.Allow("unknown-svc-event") {
			m.logger.Warn("proxy: unknown service event kind", zap.String("kind", kind))
		}
	}
}

// handleBridgeEvent is the bridge-side subscriber.
func (m *ProxyManager) handleBridgeEvent(added bool, addr netip.Addr) {
	var err error
	if added {
		err = m.OnBridgeAdded(addr)
	} else {
		err = m.OnBridgeRemoved(addr)
	}
	if err != nil && m.sampler.Allow("bridge-event") {
		m.logger.Warn("proxy: bridge event failed",
			zap.Bool("added", added),
			zap.String("addr", addr.String()),
			zap.Error(err))
	}
}

// closeListener tears down the entry for key. Idempotent.
func (m *ProxyManager) closeListener(key listenerKey) {
	m.mu.Lock()
	entry, ok := m.listeners[key]
	if ok {
		delete(m.listeners, key)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	closeEntry(entry)
}

// keysForServiceLocked returns a snapshot of the listener keys
// belonging to serviceID. Caller must hold m.mu.
func (m *ProxyManager) keysForServiceLocked(serviceID string) map[listenerKey]struct{} {
	out := make(map[listenerKey]struct{})
	for k := range m.listeners {
		if k.serviceID == serviceID {
			out[k] = struct{}{}
		}
	}
	return out
}

// snapshotGateways copies the BridgeGatewayProvider's current view.
// Caller need not hold m.mu — the provider is responsible for thread
// safety.
func (m *ProxyManager) snapshotGateways() []netip.Addr {
	if m.bridges == nil {
		return nil
	}
	return m.bridges()
}
