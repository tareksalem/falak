package node

import (
	"context"
	"errors"
	"sync"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/network/proxy"
	"github.com/tareksalem/falak/service"
	"github.com/tareksalem/falak/service/strategy"
)

// ServiceConfig carries the operator-supplied Service subsystem
// configuration. Disabled by default; set Enabled=true via
// WithServiceConfig to turn the subsystem on.
type ServiceConfig struct {
	// Enabled toggles the whole subsystem.
	Enabled bool
}

// ServiceHandler owns the Service control plane on the node: the
// service.Manager (CRUD + lifecycle + identity-binding resolution), a
// per-Service strategy registry, and the proxy.ProxyManager that
// materialises L4 listeners. Events flow:
//
//	capsule.EventCapsuleReceived → service.Manager.OnCapsuleReceived
//	capsule.EventCapsuleDeleted  → service.Manager.OnCapsuleDeleted
//	service.EventServiceCreated  → proxy.EnsureService(spec) + strategy.Start
//	service.EventServiceUpdated  → proxy.EnsureService(spec) + strategy.Update
//	service.EventServiceDeleted  → proxy.ReleaseService(id) + strategy.Stop
//
// All wiring is best-effort: when prerequisites are missing the
// handler logs and skips. Stop tears down strictly in reverse order
// (proxy → publisher → subscriber → manager) so no late event ever
// races a torn-down component.
type ServiceHandler struct {
	cfg     ServiceConfig
	logger  *zap.Logger
	capsMgr *capsule.Manager

	manager    *service.Manager
	publisher  *service.Publisher
	subscriber *service.Subscriber
	proxyMgr   *proxy.ProxyManager

	mu         sync.RWMutex
	started    bool
	stopped    bool
	strategies map[service.ServiceID]strategy.Engine
}

// ServiceHandlerOption configures a ServiceHandler.
type ServiceHandlerOption func(*ServiceHandler)

// WithServiceHandlerLogger sets the zap logger.
func WithServiceHandlerLogger(l *zap.Logger) ServiceHandlerOption {
	return func(h *ServiceHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithServiceHandlerEnabled toggles the subsystem. Default false.
func WithServiceHandlerEnabled(enabled bool) ServiceHandlerOption {
	return func(h *ServiceHandler) { h.cfg.Enabled = enabled }
}

// WithServiceHandlerCapsuleManager wires the capsule manager used as
// the CapsuleLookup adapter for backend resolution.
func WithServiceHandlerCapsuleManager(m *capsule.Manager) ServiceHandlerOption {
	return func(h *ServiceHandler) { h.capsMgr = m }
}

// WithServiceHandlerManager injects an externally-built service.Manager.
// Useful for tests; production calls NewServiceHandler then Start which
// constructs one when none was supplied.
func WithServiceHandlerManager(m *service.Manager) ServiceHandlerOption {
	return func(h *ServiceHandler) { h.manager = m }
}

// WithServiceHandlerProxyManager injects a pre-built ProxyManager. Tests
// pass a fixture; production builds one in initializeServiceHandler.
func WithServiceHandlerProxyManager(p *proxy.ProxyManager) ServiceHandlerOption {
	return func(h *ServiceHandler) { h.proxyMgr = p }
}

// WithServiceHandlerPublisher injects a pre-built service gossip
// publisher. Tests omit this; production wires the libp2p topic +
// signer here.
func WithServiceHandlerPublisher(p *service.Publisher) ServiceHandlerOption {
	return func(h *ServiceHandler) { h.publisher = p }
}

// WithServiceHandlerSubscriber injects a pre-built service gossip
// subscriber. Tests omit this; production wires the libp2p subscription
// + phonebook-backed verifier here.
func WithServiceHandlerSubscriber(s *service.Subscriber) ServiceHandlerOption {
	return func(h *ServiceHandler) { h.subscriber = s }
}

// NewServiceHandler builds a handler with sensible defaults. Call Start
// to actually wire the subsystem.
func NewServiceHandler(opts ...ServiceHandlerOption) *ServiceHandler {
	h := &ServiceHandler{
		logger:     zap.NewNop(),
		strategies: make(map[service.ServiceID]strategy.Engine),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Manager returns the wrapped service.Manager (or nil when disabled).
// Used by the API facade adapter (api/core ServiceFacade impl).
func (h *ServiceHandler) Manager() *service.Manager {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.manager
}

// ProxyManager returns the wrapped proxy.ProxyManager (or nil when the
// subsystem is disabled / no proxy was wired). Used by the bridge
// adapter that pumps network.Manager bridge events into the proxy.
func (h *ServiceHandler) ProxyManager() *proxy.ProxyManager {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.proxyMgr
}

// Start wires every subsystem. Idempotent; a second call is a no-op.
// Returns an error only when the configuration is incoherent (e.g.
// Enabled without a capsule manager). When the handler is disabled the
// call is a structured-log no-op and Manager() returns nil.
func (h *ServiceHandler) Start(ctx context.Context) error {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return nil
	}
	h.started = true
	enabled := h.cfg.Enabled
	h.mu.Unlock()

	if !enabled {
		h.logger.Info("service handler disabled by config")
		return nil
	}
	if h.capsMgr == nil {
		return errors.New("service handler: capsule manager required")
	}

	if h.manager == nil {
		mgrOpts := []service.ManagerOption{
			service.WithLogger(h.logger.Named("service.manager")),
			service.WithCapsuleLookup(capsuleLookupAdapter{m: h.capsMgr}),
		}
		if h.publisher != nil {
			mgrOpts = append(mgrOpts, service.WithPublisher(h.publisher))
		}
		h.manager = service.NewManager(mgrOpts...)
	}
	h.manager.SetEventHandler(h.onManagerEvent)

	if h.publisher != nil {
		h.publisher.Start(ctx)
	}
	if h.subscriber != nil {
		h.subscriber.Start(ctx)
	}
	if h.proxyMgr != nil {
		if err := h.proxyMgr.Start(); err != nil {
			h.logger.Error("proxy manager start failed",
				zap.Error(err))
			return err
		}
	} else {
		h.logger.Warn("service handler enabled without a proxy manager; routing layer skipped")
	}

	h.logger.Info("service handler started",
		zap.Bool("proxy", h.proxyMgr != nil),
		zap.Bool("publisher", h.publisher != nil),
		zap.Bool("subscriber", h.subscriber != nil))
	return nil
}

// Stop tears the subsystem down in reverse-start order: proxy →
// subscriber → publisher → strategy engines → manager. Each step is
// best-effort so a single failure cannot block the rest. Idempotent.
func (h *ServiceHandler) Stop() error {
	h.mu.Lock()
	if !h.started || h.stopped {
		h.mu.Unlock()
		return nil
	}
	h.stopped = true
	proxyMgr := h.proxyMgr
	publisher := h.publisher
	subscriber := h.subscriber
	engines := make([]strategy.Engine, 0, len(h.strategies))
	for _, e := range h.strategies {
		if e != nil {
			engines = append(engines, e)
		}
	}
	h.strategies = make(map[service.ServiceID]strategy.Engine)
	h.mu.Unlock()

	if proxyMgr != nil {
		if err := proxyMgr.Stop(); err != nil {
			h.logger.Warn("proxy manager stop returned error", zap.Error(err))
		}
	}
	if subscriber != nil {
		subscriber.Stop()
	}
	if publisher != nil {
		publisher.Stop()
	}
	for _, e := range engines {
		if err := e.Stop(); err != nil {
			h.logger.Warn("strategy engine stop returned error", zap.Error(err))
		}
	}
	h.logger.Info("service handler stopped")
	return nil
}

// OnCapsuleReceived forwards a capsule-receipt event into the service
// manager so unresolved backends bind. Public so the node-level event
// adapter can call it directly.
func (h *ServiceHandler) OnCapsuleReceived(name, id string) {
	mgr := h.Manager()
	if mgr == nil {
		return
	}
	mgr.OnCapsuleReceived(name, id)
}

// OnCapsuleDeleted forwards a capsule-deletion event into the service
// manager so dependent Services mark backends unresolved.
func (h *ServiceHandler) OnCapsuleDeleted(name string) {
	mgr := h.Manager()
	if mgr == nil {
		return
	}
	mgr.OnCapsuleDeleted(name)
}

// StrategyFor exposes the per-Service strategy engine as `any` so the
// proxy package can read LiveWeights without importing service/strategy.
// Returns ok=false when no engine has been registered for serviceID.
func (h *ServiceHandler) StrategyFor(serviceID string) (engine any, ok bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, found := h.strategies[service.ServiceID(serviceID)]
	if !found || e == nil {
		return nil, false
	}
	return e, true
}

// Strategy satisfies proxy.StrategyGetter. Identical to StrategyFor but
// keeps the contract narrow for the proxy adapter.
func (h *ServiceHandler) Strategy(serviceID string) (engine any, ok bool) {
	return h.StrategyFor(serviceID)
}

// onManagerEvent is the service.Manager event handler. It bridges into
// the proxy manager (when one is wired) so listener state stays in
// sync with the Service set, and creates / updates / stops strategy
// engines so the proxy sees the live weight map.
func (h *ServiceHandler) onManagerEvent(evt service.ManagerEvent) {
	switch evt.Type {
	case service.EventServiceCreated, service.EventServiceUpdated, service.EventServiceReceived:
		if evt.Service != nil {
			h.handleServiceUpsert(evt.Service)
		}
	case service.EventServiceDeleted:
		h.handleServiceDelete(evt.ServiceID)
	}
}

// handleServiceUpsert pushes the latest spec into the proxy and
// (re)builds the per-Service strategy engine. On a fresh Service the
// engine is constructed from the spec; on an update the existing
// engine receives the new spec via Update so live weights flow
// without dropping in-flight progression.
func (h *ServiceHandler) handleServiceUpsert(svc *service.Service) {
	if svc == nil || svc.Spec.Strategy == nil {
		return
	}
	engine := h.ensureStrategy(svc)
	h.mu.RLock()
	proxyMgr := h.proxyMgr
	h.mu.RUnlock()

	if engine != nil {
		if err := engine.Update(svc.Spec); err != nil {
			h.logger.Warn("strategy engine update failed",
				zap.String("service", svc.ID.String()), zap.Error(err))
		}
	}

	if proxyMgr == nil {
		return
	}
	if err := proxyMgr.EnsureService(serviceToProxySpec(svc)); err != nil {
		h.logger.Warn("proxy ensure service failed",
			zap.String("service", svc.ID.String()),
			zap.String("name", svc.Spec.Name),
			zap.Error(err))
	}
}

// ensureStrategy returns the engine for svc, constructing one when
// absent. Engine construction is type-driven: Static / BlueGreen /
// Canary, each with the standard emitter so progress events flow back
// onto the service.Manager event bus.
func (h *ServiceHandler) ensureStrategy(svc *service.Service) strategy.Engine {
	h.mu.Lock()
	defer h.mu.Unlock()
	if engine, ok := h.strategies[svc.ID]; ok && engine != nil {
		return engine
	}
	emitter := &strategyEmitter{manager: h.manager}
	var engine strategy.Engine
	switch svc.Spec.Strategy.Type {
	case service.StrategyTypeEnum.BlueGreen():
		engine = strategy.NewBlueGreen(svc.ID, svc.Spec, emitter)
	case service.StrategyTypeEnum.Canary():
		// metricEval stays nil for v1 — the canary engine treats nil
		// as "no condition ever fires", which is the correct default
		// until capsule metrics are connected. abort_on still drives
		// auto-abort via the manager's zero-weight identity-change
		// mutation (Decision #14).
		engine = strategy.NewCanary(svc.ID, svc.Spec, emitter, nil)
	default:
		engine = strategy.NewStatic(svc.ID, svc.Spec)
	}
	if err := engine.Start(context.Background()); err != nil {
		h.logger.Warn("strategy engine start failed",
			zap.String("service", svc.ID.String()), zap.Error(err))
	}
	h.strategies[svc.ID] = engine
	return engine
}

// handleServiceDelete stops the per-Service strategy engine, releases
// the proxy listeners, and drops the engine registry entry.
func (h *ServiceHandler) handleServiceDelete(id service.ServiceID) {
	h.mu.Lock()
	engine := h.strategies[id]
	delete(h.strategies, id)
	proxyMgr := h.proxyMgr
	h.mu.Unlock()

	if engine != nil {
		if err := engine.Stop(); err != nil {
			h.logger.Warn("strategy engine stop returned error",
				zap.String("service", id.String()), zap.Error(err))
		}
	}
	if proxyMgr == nil {
		return
	}
	if err := proxyMgr.ReleaseService(id.String()); err != nil {
		h.logger.Warn("proxy release service failed",
			zap.String("service", id.String()), zap.Error(err))
	}
}

// serviceToProxySpec translates a service.Service into the proxy-local
// ServiceSpec view. Backends and strategy stay on the service side —
// the proxy reads them through the ServiceResolver + StrategyGetter
// wiring (set up elsewhere when a real ProxyManager is built).
func serviceToProxySpec(svc *service.Service) proxy.ServiceSpec {
	out := proxy.ServiceSpec{
		ID:          svc.ID.String(),
		Name:        svc.Spec.Name,
		Visibility:  string(svc.Spec.Visibility),
		GroupID:     svc.Spec.Group,
		ClusterPath: svc.ClusterID,
	}
	for _, p := range svc.Spec.Ports {
		out.Ports = append(out.Ports, proxy.ServicePort{
			Name:     p.Name,
			Port:     p.Port,
			Protocol: string(p.Protocol),
		})
	}
	return out
}

