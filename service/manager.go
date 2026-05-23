package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ErrServiceNameExists is returned by Create when a Service with the
// requested name already exists in the cluster (Decision #16).
var ErrServiceNameExists = errors.New("service: name already exists in cluster")

// publisher is the subset of the gossip Publisher consumed by the
// manager. Declared here so tests can inject a stub.
type publisher interface {
	PublishUpdate(ctx context.Context, svc *Service) error
	PublishWithdrawal(ctx context.Context, id ServiceID) error
}

// Manager owns Service CRUD, lifecycle, identity-binding backend
// resolution, and gossip publish. It is safe for concurrent use.
type Manager struct {
	store     *Store
	publisher publisher
	logger    *zap.Logger
	metrics   MetricsSink
	capsules  CapsuleLookup

	handlerMu sync.RWMutex
	handler   EventHandler

	lifecyclesMu sync.RWMutex
	lifecycles   map[ServiceID]*Lifecycle
}

// ManagerOption configures a Manager.
type ManagerOption func(*Manager)

// WithStore sets the backing Store. Defaults to an in-memory store.
func WithStore(s *Store) ManagerOption {
	return func(m *Manager) {
		if s != nil {
			m.store = s
		}
	}
}

// WithLogger sets the zap logger.
func WithLogger(logger *zap.Logger) ManagerOption {
	return func(m *Manager) {
		if logger != nil {
			m.logger = logger
		}
	}
}

// WithPublisher installs the gossip publisher. May be nil for tests
// that don't exercise the gossip path.
func WithPublisher(p *Publisher) ManagerOption {
	return func(m *Manager) {
		if p != nil {
			m.publisher = p
		}
	}
}

// WithMetrics installs the metrics sink. Defaults to NoopMetrics.
func WithMetrics(sink MetricsSink) ManagerOption {
	return func(m *Manager) {
		if sink != nil {
			m.metrics = sink
		}
	}
}

// WithCapsuleLookup wires the capsule-store accessor used during
// backend resolution.
func WithCapsuleLookup(cl CapsuleLookup) ManagerOption {
	return func(m *Manager) { m.capsules = cl }
}

// NewManager constructs a Manager with the given options. Lifecycles
// are restored for any Services already in the store.
func NewManager(opts ...ManagerOption) *Manager {
	m := &Manager{
		store:      NewStore(),
		logger:     zap.NewNop(),
		metrics:    NoopMetrics{},
		handler:    func(ManagerEvent) {},
		lifecycles: make(map[ServiceID]*Lifecycle),
	}
	for _, opt := range opts {
		opt(m)
	}
	for _, svc := range m.store.List() {
		m.lifecycles[svc.ID] = m.newLifecycle(svc.ID, svc.Status)
	}
	return m
}

// SetEventHandler swaps the event handler at runtime. Safe to call
// concurrently with event emission.
func (m *Manager) SetEventHandler(h EventHandler) {
	m.handlerMu.Lock()
	defer m.handlerMu.Unlock()
	if h == nil {
		m.handler = func(ManagerEvent) {}
		return
	}
	m.handler = h
}

// ActiveServices returns Services currently considered routable for
// republishing on the gossip topic.
func (m *Manager) ActiveServices() []*Service {
	out := []*Service{}
	for _, svc := range m.store.List() {
		if svc.Status == ServiceStatusEnum.Active() {
			out = append(out, svc)
		}
	}
	return out
}

// Create admits a new Service. Backends are resolved leniently against
// the capsule store; unresolved backends are accepted (Decision #15).
// Returns ErrServiceNameExists on cluster-wide name conflict.
func (m *Manager) Create(ctx context.Context, clusterID string, spec ServiceSpec) (*Service, error) {
	if clusterID == "" {
		return nil, errors.New("service: clusterID is required")
	}
	DefaultSpec(&spec)
	if err := ValidateSpec(&spec); err != nil {
		return nil, fmt.Errorf("service: invalid spec: %w", err)
	}
	if existing := m.store.GetByName(spec.Name); existing != nil {
		return nil, fmt.Errorf("%w: %s", ErrServiceNameExists, spec.Name)
	}

	now := time.Now()
	svc := &Service{
		ID:        NewServiceID(),
		ClusterID: clusterID,
		Spec:      spec,
		Status:    ServiceStatusEnum.Created(),
		Version:   "1",
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.resolveAllBackends(svc, now)

	if err := m.store.Create(svc); err != nil {
		return nil, fmt.Errorf("service: persist create: %w", err)
	}

	m.lifecyclesMu.Lock()
	lc := m.newLifecycle(svc.ID, ServiceStatusEnum.Created())
	m.lifecycles[svc.ID] = lc
	m.lifecyclesMu.Unlock()
	if err := lc.Fire(TriggerActivate); err != nil {
		m.logger.Warn("service auto-activate failed",
			zap.String("service", svc.ID.String()), zap.Error(err))
	}

	m.metrics.IncCreated(clusterID)
	m.logger.Info("service created",
		zap.String("service", svc.ID.String()),
		zap.String("name", spec.Name),
		zap.String("cluster", clusterID))
	m.emit(EventServiceCreated, svc, nil)

	if m.publisher != nil {
		if err := m.publisher.PublishUpdate(ctx, svc); err != nil {
			m.logger.Warn("service create publish failed",
				zap.String("service", svc.ID.String()), zap.Error(err))
		}
	}
	return svc, nil
}

// Get returns the Service with id or nil when absent.
func (m *Manager) Get(id ServiceID) *Service { return m.store.Get(id) }

// GetByName returns the Service named name or nil when absent.
func (m *Manager) GetByName(name string) *Service { return m.store.GetByName(name) }

// List returns every Service sorted by name.
func (m *Manager) List() []*Service { return m.store.List() }

// ListByVisibility returns Services with the given Visibility.
func (m *Manager) ListByVisibility(v Visibility) []*Service { return m.store.ListByVisibility(v) }

// ListByGroup returns Services owned by the named group.
func (m *Manager) ListByGroup(group string) []*Service { return m.store.ListByGroup(group) }

// ListReferencingCapsule returns Services with at least one backend
// pointing at capsuleName.
func (m *Manager) ListReferencingCapsule(name string) []*Service {
	return m.store.ListReferencingCapsule(name)
}

// Update replaces the spec on an existing Service. Backend resolution
// is re-run; captured IDs are preserved when the backend remains in
// the spec, and dropped backends release their resolution state.
func (m *Manager) Update(ctx context.Context, id ServiceID, spec ServiceSpec) (*Service, error) {
	DefaultSpec(&spec)
	if err := ValidateSpec(&spec); err != nil {
		return nil, fmt.Errorf("service: invalid spec: %w", err)
	}
	svc := m.store.Get(id)
	if svc == nil {
		return nil, fmt.Errorf("%w: %s", ErrServiceNotFound, id)
	}
	existing := backendStatesByName(svc.BackendStates)
	spec.Backends = preserveCapturedIDs(spec.Backends, svc.Spec.Backends)
	svc.Spec = spec
	svc.UpdatedAt = time.Now()
	svc.BackendStates = mergeBackendStates(spec.Backends, existing, time.Now())

	if err := m.store.Update(svc); err != nil {
		return nil, fmt.Errorf("service: persist update: %w", err)
	}
	m.metrics.IncUpdated(svc.ClusterID)
	m.logger.Info("service updated",
		zap.String("service", svc.ID.String()),
		zap.String("name", spec.Name),
		zap.String("cluster", svc.ClusterID))
	m.emit(EventServiceUpdated, svc, nil)

	if m.publisher != nil {
		if err := m.publisher.PublishUpdate(ctx, svc); err != nil {
			m.logger.Warn("service update publish failed",
				zap.String("service", svc.ID.String()), zap.Error(err))
		}
	}
	return svc, nil
}

// Apply is the declarative upsert path. When a Service with the
// requested name exists it is Updated; otherwise it is Created.
func (m *Manager) Apply(ctx context.Context, clusterID string, spec ServiceSpec) (*Service, error) {
	existing := m.store.GetByName(spec.Name)
	if existing == nil {
		return m.Create(ctx, clusterID, spec)
	}
	return m.Update(ctx, existing.ID, spec)
}

// Delete drains and removes a Service, publishing a withdrawal so peer
// nodes drop it from their mirrors. Reaper-idempotent: a second Delete
// returns ErrServiceNotFound (matchable via errors.Is).
func (m *Manager) Delete(ctx context.Context, id ServiceID) error {
	svc := m.store.Get(id)
	if svc == nil {
		return fmt.Errorf("%w: %s", ErrServiceNotFound, id)
	}
	m.driveDrainToDeleted(id)
	if err := m.store.Delete(id); err != nil {
		return fmt.Errorf("service: persist delete: %w", err)
	}
	m.lifecyclesMu.Lock()
	delete(m.lifecycles, id)
	m.lifecyclesMu.Unlock()

	m.metrics.IncDeleted(svc.ClusterID)
	m.logger.Info("service deleted",
		zap.String("service", id.String()),
		zap.String("name", svc.Spec.Name),
		zap.String("cluster", svc.ClusterID))
	m.emit(EventServiceDeleted, svc, nil)

	if m.publisher != nil {
		if err := m.publisher.PublishWithdrawal(ctx, id); err != nil {
			m.logger.Warn("service withdrawal publish failed",
				zap.String("service", id.String()), zap.Error(err))
		}
	}
	return nil
}

// driveDrainToDeleted walks the FSM from its current state to Deleted
// via the allowed triggers. Each transition is best-effort: an illegal
// trigger from the current state is logged and skipped.
func (m *Manager) driveDrainToDeleted(id ServiceID) {
	lc := m.getLifecycle(id)
	if lc == nil {
		return
	}
	for _, trigger := range []string{TriggerStartDraining, TriggerFinishDraining} {
		if !lc.CanFire(trigger) {
			continue
		}
		if err := lc.Fire(trigger); err != nil {
			m.logger.Warn("service drain transition failed",
				zap.String("service", id.String()),
				zap.String("trigger", trigger),
				zap.Error(err))
		}
	}
}

// Receive stores or updates a Service that arrived via gossip. The
// local store is the source of truth for originated Services; peers
// always mirror the latest gossip-arrived version.
func (m *Manager) Receive(svc *Service) error {
	if svc == nil {
		return errors.New("service: nil Service in Receive")
	}
	existing := m.store.Get(svc.ID)
	if existing != nil {
		svc.UpdatedAt = time.Now()
		if err := m.store.Update(svc); err != nil {
			return fmt.Errorf("service: persist received update: %w", err)
		}
	} else {
		if err := m.store.Create(svc); err != nil {
			return fmt.Errorf("service: persist received: %w", err)
		}
		initial := svc.Status
		if !initial.Valid() {
			initial = ServiceStatusEnum.Active()
		}
		m.lifecyclesMu.Lock()
		m.lifecycles[svc.ID] = m.newLifecycle(svc.ID, initial)
		m.lifecyclesMu.Unlock()
	}
	m.metrics.IncReceived(svc.ClusterID)
	m.logger.Debug("service received",
		zap.String("service", svc.ID.String()),
		zap.String("name", svc.Spec.Name),
		zap.String("cluster", svc.ClusterID))
	m.emit(EventServiceReceived, svc, nil)
	return nil
}

// HandleWithdrawal removes a Service announced as deleted on gossip.
// Idempotent: missing IDs are silently ignored to keep the subscriber
// loop flowing.
func (m *Manager) HandleWithdrawal(id ServiceID) error {
	svc := m.store.Get(id)
	if svc == nil {
		return nil
	}
	if err := m.store.Delete(id); err != nil {
		return fmt.Errorf("service: handle withdrawal: %w", err)
	}
	m.lifecyclesMu.Lock()
	delete(m.lifecycles, id)
	m.lifecyclesMu.Unlock()
	m.logger.Info("service withdrawn via gossip",
		zap.String("service", id.String()),
		zap.String("name", svc.Spec.Name),
		zap.String("cluster", svc.ClusterID))
	m.emit(EventServiceDeleted, svc, nil)
	return nil
}

// newLifecycle constructs a Lifecycle for serviceID at initial. The
// lifecycle persists status writes back through the manager's store.
func (m *Manager) newLifecycle(id ServiceID, initial ServiceStatus) *Lifecycle {
	return NewLifecycle(id,
		WithLifecycleLogger(m.logger.Named("lifecycle")),
		WithLifecycleInitialState(initial),
		WithLifecycleHandler(func(event LifecycleEvent) {
			m.onLifecycleTransition(event)
		}),
	)
}

func (m *Manager) onLifecycleTransition(event LifecycleEvent) {
	svc := m.store.Get(event.ServiceID)
	if svc == nil {
		return
	}
	svc.Status = event.To
	svc.UpdatedAt = time.Now()
	if err := m.store.Update(svc); err != nil {
		m.logger.Error("failed to persist service lifecycle transition",
			zap.String("service", event.ServiceID.String()),
			zap.String("from", string(event.From)),
			zap.String("to", string(event.To)),
			zap.String("trigger", event.Trigger),
			zap.Error(err))
	}
}

func (m *Manager) getLifecycle(id ServiceID) *Lifecycle {
	m.lifecyclesMu.RLock()
	defer m.lifecyclesMu.RUnlock()
	return m.lifecycles[id]
}

// EmitStrategyEvent publishes a strategy-progress event onto the
// manager's event handler chain. Used by external strategy engines
// (e.g. the node-side ServiceHandler) to surface CanaryStep,
// CanaryAborted, and BlueGreenFlip events through the same fan-out
// path subscribers already listen on. The Service snapshot is read
// from the store at call time; the call is a no-op when the Service
// is unknown.
func (m *Manager) EmitStrategyEvent(eventType string, id ServiceID, meta map[string]string) {
	svc := m.store.Get(id)
	if svc == nil {
		return
	}
	m.emit(eventType, svc, meta)
}

func (m *Manager) emit(eventType string, svc *Service, meta map[string]string) {
	m.handlerMu.RLock()
	h := m.handler
	m.handlerMu.RUnlock()
	h(ManagerEvent{
		Type:      eventType,
		ServiceID: svc.ID,
		Service:   svc,
		Timestamp: time.Now(),
		Meta:      meta,
	})
}
