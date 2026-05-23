package network

import (
	"net/netip"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/network/bridge"
	"github.com/tareksalem/falak/network/endpoints"
)

// BridgeListener receives bridge add / remove callbacks from the
// network Manager. Implementations must be quick and non-blocking; the
// Manager invokes callbacks synchronously after the 0→1 / 1→0 group
// ref transitions complete. The proxy ProxyManager satisfies this
// interface via its OnBridgeAdded / OnBridgeRemoved methods, but any
// observer (metrics, logs, tests) can register one.
type BridgeListener interface {
	// OnBridgeAdded fires once per per-group bridge as soon as the
	// gateway IP becomes reachable. Errors are logged and dropped — the
	// Manager continues with the next listener.
	OnBridgeAdded(addr netip.Addr) error
	// OnBridgeRemoved fires once per teardown, after the bridge has
	// been torn down. Errors are logged and dropped.
	OnBridgeRemoved(addr netip.Addr) error
}

// EndpointRegistry returns the in-memory endpoint mirror the Manager
// owns. Used by the proxy wiring (ProxyManager via WithManagerRegistry)
// so listeners can resolve replica IPs without re-importing the
// network package's internals.
func (m *Manager) EndpointRegistry() *endpoints.Registry { return m.registry }

// ClusterPath returns the cluster identity this Manager owns. Used by
// node-side wiring helpers that need the topic name without holding a
// separate copy of the config.
func (m *Manager) ClusterPath() string { return m.clusterPath }

// RegisterBridgeListener adds l to the listener set. The Manager
// retains a reference; the caller must invoke UnregisterBridgeListener
// to release it on shutdown. Re-registering the same listener is a
// no-op. Safe for concurrent use.
func (m *Manager) RegisterBridgeListener(l BridgeListener) {
	if l == nil {
		return
	}
	m.listenersMu.Lock()
	defer m.listenersMu.Unlock()
	for _, existing := range m.listeners {
		if existing == l {
			return
		}
	}
	m.listeners = append(m.listeners, l)
}

// UnregisterBridgeListener removes l from the listener set. No-op when
// l was not previously registered.
func (m *Manager) UnregisterBridgeListener(l BridgeListener) {
	if l == nil {
		return
	}
	m.listenersMu.Lock()
	defer m.listenersMu.Unlock()
	out := m.listeners[:0]
	for _, existing := range m.listeners {
		if existing == l {
			continue
		}
		out = append(out, existing)
	}
	m.listeners = out
}

// ResolveBridgeGroup returns the (clusterPath, groupID) that owns the
// bridge whose subnet contains srcIP. Returns ok=false when no bridge
// owns the IP — callers must fail closed for visibility decisions.
// Satisfies proxy.SourceResolver via direct method-set match.
func (m *Manager) ResolveBridgeGroup(srcIP netip.Addr) (clusterPath, groupID string, ok bool) {
	if !srcIP.IsValid() {
		return "", "", false
	}
	ipSlice := srcIP.AsSlice()
	m.mu.Lock()
	defer m.mu.Unlock()
	for gid, st := range m.groups {
		if st == nil || st.bridgeInfo == nil || st.bridgeInfo.Subnet == nil {
			continue
		}
		if st.bridgeInfo.Subnet.Contains(ipSlice) {
			return m.clusterPath, gid, true
		}
	}
	return "", "", false
}

// ResolveGroup is the SourceResolver-shaped method (returns clusterPath,
// groupID, ok) the proxy expects directly. Aliased to ResolveBridgeGroup
// so callers can use either name; both have identical semantics.
func (m *Manager) ResolveGroup(srcIP netip.Addr) (clusterPath, groupID string, ok bool) {
	return m.ResolveBridgeGroup(srcIP)
}

// LocalCluster returns the clusterPath this Manager owns. Required by
// proxy.SourceResolver so the cross-cluster boundary check in the
// proxy visibility hook can reject any source whose bridge resolves to
// a foreign cluster. Equivalent to ClusterPath().
func (m *Manager) LocalCluster() string { return m.clusterPath }

// BridgeGateways returns the current set of per-group bridge gateway
// IPs, suitable as a proxy.BridgeGatewayProvider. Order is unspecified
// but stable per call. Bridges without a parseable gateway are
// skipped. The slice is freshly allocated; callers may retain it.
func (m *Manager) BridgeGateways() []netip.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]netip.Addr, 0, len(m.groups))
	for _, st := range m.groups {
		if st == nil || st.bridgeInfo == nil {
			continue
		}
		addr, ok := bridgeGatewayAddr(st.bridgeInfo)
		if !ok {
			continue
		}
		out = append(out, addr)
	}
	return out
}

// bridgeGatewayAddr translates a bridge.BridgeInfo.Gateway into a
// netip.Addr. Returns (zero, false) when the gateway is nil or not a
// well-formed IP.
func bridgeGatewayAddr(info *bridge.BridgeInfo) (netip.Addr, bool) {
	if info == nil || info.Gateway == nil {
		return netip.Addr{}, false
	}
	addr, ok := netip.AddrFromSlice(info.Gateway.To4())
	if !ok {
		addr, ok = netip.AddrFromSlice(info.Gateway.To16())
		if !ok {
			return netip.Addr{}, false
		}
	}
	return addr.Unmap(), true
}

// notifyBridgeAdded invokes every registered listener for addr. Errors
// from any individual listener are logged and dropped so a misbehaving
// listener cannot block the bridge lifecycle.
func (m *Manager) notifyBridgeAdded(addr netip.Addr) {
	m.listenersMu.RLock()
	listeners := append([]BridgeListener(nil), m.listeners...)
	m.listenersMu.RUnlock()
	for _, l := range listeners {
		if err := l.OnBridgeAdded(addr); err != nil {
			m.logger.Warn("network: bridge listener add failed",
				zap.String("addr", addr.String()), zap.Error(err))
		}
	}
}

// notifyBridgeRemoved invokes every registered listener for addr.
func (m *Manager) notifyBridgeRemoved(addr netip.Addr) {
	m.listenersMu.RLock()
	listeners := append([]BridgeListener(nil), m.listeners...)
	m.listenersMu.RUnlock()
	for _, l := range listeners {
		if err := l.OnBridgeRemoved(addr); err != nil {
			m.logger.Warn("network: bridge listener remove failed",
				zap.String("addr", addr.String()), zap.Error(err))
		}
	}
}
