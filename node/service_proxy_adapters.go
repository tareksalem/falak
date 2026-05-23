package node

import (
	"net/netip"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/network/endpoints"
	"github.com/tareksalem/falak/network/proxy"
	"github.com/tareksalem/falak/service"
	"github.com/tareksalem/falak/service/strategy"
)

// serviceResolverAdapter satisfies proxy.ServiceResolver by consulting
// the live service.Manager state and the strategy engine owned by the
// ServiceHandler. One adapter is built per (Service, listener) tuple
// when the proxy ProxyManager invokes its resolver builder; the
// adapter retains references so each Resolve call sees the latest
// snapshot without rebuilding.
type serviceResolverAdapter struct {
	handler   *ServiceHandler
	registry  *endpoints.Registry
	serviceID string
}

// Resolve returns a routing snapshot built from the current Service
// spec and the strategy engine's LiveWeights. Returns ok=false when
// the Service has been deleted or no admissible backend remains.
func (a *serviceResolverAdapter) Resolve() (proxy.ServiceSnapshot, bool) {
	mgr := a.handler.Manager()
	if mgr == nil {
		return proxy.ServiceSnapshot{}, false
	}
	svc := mgr.Get(service.ServiceID(a.serviceID))
	if svc == nil {
		return proxy.ServiceSnapshot{}, false
	}
	var weights strategy.LiveWeights
	if engine, ok := a.handler.StrategyFor(a.serviceID); ok {
		if eng, ok := engine.(strategy.Engine); ok {
			weights = eng.LiveWeights()
		}
	}
	if weights == nil {
		weights = make(strategy.LiveWeights, len(svc.Spec.Backends))
		for _, b := range svc.Spec.Backends {
			if b.Weight > 0 {
				weights[b.Capsule] = b.Weight
			}
		}
	}
	snap := proxy.ServiceSnapshot{
		ServiceID:      svc.ID.String(),
		ClusterPath:    svc.ClusterID,
		GroupID:        svc.Spec.Group,
		IdleTimeout:    svc.Spec.Timeouts.Idle,
		ConnectTimeout: svc.Spec.Timeouts.Connect,
	}
	for _, b := range svc.Spec.Backends {
		w, ok := weights[b.Capsule]
		if !ok || w <= 0 {
			continue
		}
		snap.Backends = append(snap.Backends, proxy.BackendSnapshot{
			Capsule: b.Capsule,
			Weight:  w,
			PortMap: b.PortMap,
		})
	}
	if len(snap.Backends) == 0 {
		return snap, false
	}
	return snap, true
}

// bridgeSourceResolver adapts a NetworkBridgeProvider into the
// proxy.SourceResolver interface — the proxy's per-connection
// visibility check reads (clusterPath, groupID) for the source IP via
// this adapter.
type bridgeSourceResolver struct {
	bp NetworkBridgeProvider
}

// ResolveGroup forwards to the underlying provider; returns ok=false
// when the provider is nil so callers fail closed.
func (r bridgeSourceResolver) ResolveGroup(srcIP netip.Addr) (clusterPath, groupID string, ok bool) {
	if r.bp == nil {
		return "", "", false
	}
	return r.bp.ResolveGroup(srcIP)
}

// LocalCluster forwards to the underlying provider. Returns "" when
// the provider is nil — the proxy's boundary check treats an empty
// LocalCluster as "boundary check disabled" so a misconfigured /
// stubbed wiring degrades gracefully.
func (r bridgeSourceResolver) LocalCluster() string {
	if r.bp == nil {
		return ""
	}
	return r.bp.LocalCluster()
}

// proxyBridgeListener satisfies network.BridgeListener by forwarding
// per-group bridge add / remove callbacks into the proxy.ProxyManager.
// The proxy ensures listeners for every active Service on the new
// bridge gateway and tears them down on removal.
type proxyBridgeListener struct {
	proxy  *proxy.ProxyManager
	logger *zap.Logger
}

// OnBridgeAdded forwards the add-event to the proxy. Errors are
// logged at WARN so a transient listener-bind failure does not abort
// the bridge lifecycle.
func (l *proxyBridgeListener) OnBridgeAdded(addr netip.Addr) error {
	if l.proxy == nil {
		return nil
	}
	if err := l.proxy.OnBridgeAdded(addr); err != nil {
		l.logger.Warn("proxy bridge add failed",
			zap.String("addr", addr.String()), zap.Error(err))
		return err
	}
	return nil
}

// OnBridgeRemoved forwards the remove-event to the proxy.
func (l *proxyBridgeListener) OnBridgeRemoved(addr netip.Addr) error {
	if l.proxy == nil {
		return nil
	}
	if err := l.proxy.OnBridgeRemoved(addr); err != nil {
		l.logger.Warn("proxy bridge remove failed",
			zap.String("addr", addr.String()), zap.Error(err))
		return err
	}
	return nil
}
