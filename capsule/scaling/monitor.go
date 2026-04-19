package scaling

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
)

// ScaleEvent is emitted when a scaling rule triggers.
type ScaleEvent struct {
	CapsuleID capsule.CapsuleID
	RuleName  string
	Action    capsule.ScalingAction
	Timestamp time.Time
}

// ScaleHandler is called when a scaling event occurs.
type ScaleHandler func(event ScaleEvent)

// CapsuleMetrics pairs a capsule with its scaling rules. Metrics are looked up
// from the Monitor's MetricsRegistry on each evaluation tick, not stored here —
// this lets the runtime module swap providers without re-registering.
//
// The deprecated Metrics field is still honored as a fallback when no registry
// is configured, for tests and simple setups.
type CapsuleMetrics struct {
	CapsuleID capsule.CapsuleID
	Rules     []Rule
	Metrics   MetricsProvider // optional fallback when Monitor has no registry
}

// Monitor periodically evaluates scaling rules for all registered capsules.
// Metrics are fetched via the MetricsRegistry (if configured) so that the
// runtime module can swap metrics providers independently of rule registration.
type Monitor struct {
	mu        sync.RWMutex
	evaluator *Evaluator
	capsules  map[capsule.CapsuleID]*CapsuleMetrics
	registry  MetricsRegistry
	handler   ScaleHandler
	interval  time.Duration
	logger    *zap.Logger
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// MonitorOption configures a Monitor.
type MonitorOption func(*Monitor)

// WithMonitorInterval sets the evaluation interval.
func WithMonitorInterval(d time.Duration) MonitorOption {
	return func(m *Monitor) {
		m.interval = d
	}
}

// WithMonitorLogger sets the logger.
func WithMonitorLogger(logger *zap.Logger) MonitorOption {
	return func(m *Monitor) {
		m.logger = logger
	}
}

// WithMonitorHandler sets the scale event handler.
func WithMonitorHandler(h ScaleHandler) MonitorOption {
	return func(m *Monitor) {
		m.handler = h
	}
}

// WithMonitorRegistry sets the metrics registry used to look up MetricsProvider
// instances on each evaluation tick. When unset, the monitor falls back to the
// CapsuleMetrics.Metrics field (useful for tests).
func WithMonitorRegistry(r MetricsRegistry) MonitorOption {
	return func(m *Monitor) {
		m.registry = r
	}
}

// NewMonitor creates a new scaling monitor.
func NewMonitor(opts ...MonitorOption) *Monitor {
	m := &Monitor{
		evaluator: NewEvaluator(),
		capsules:  make(map[capsule.CapsuleID]*CapsuleMetrics),
		interval:  10 * time.Second,
		logger:    zap.NewNop(),
		handler:   func(ScaleEvent) {},
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Register adds a capsule with its scaling rules and metrics provider to the monitor.
func (m *Monitor) Register(cm *CapsuleMetrics) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.capsules[cm.CapsuleID] = cm
	m.logger.Debug("capsule registered for scaling",
		zap.String("capsule_id", string(cm.CapsuleID)),
		zap.Int("rules", len(cm.Rules)))
}

// Unregister removes a capsule from the monitor.
func (m *Monitor) Unregister(id capsule.CapsuleID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.capsules, id)
	m.evaluator.ResetAllCooldowns(id)
	m.logger.Debug("capsule unregistered from scaling",
		zap.String("capsule_id", string(id)))
}

// Start begins the periodic scaling evaluation loop.
func (m *Monitor) Start(ctx context.Context) {
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.loop()
	}()
	m.logger.Info("scaling monitor started",
		zap.Duration("interval", m.interval))
}

// Stop halts the monitor and waits for the loop to finish.
func (m *Monitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	m.logger.Info("scaling monitor stopped")
}

func (m *Monitor) loop() {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.evaluate()
		}
	}
}

func (m *Monitor) evaluate() {
	m.mu.RLock()
	// Snapshot current capsules
	capsules := make([]*CapsuleMetrics, 0, len(m.capsules))
	for _, cm := range m.capsules {
		capsules = append(capsules, cm)
	}
	m.mu.RUnlock()

	for _, cm := range capsules {
		// Prefer the registry; fall back to the deprecated per-capsule provider.
		var provider MetricsProvider
		if m.registry != nil {
			provider = m.registry.Get(cm.CapsuleID)
		}
		if provider == nil {
			provider = cm.Metrics
		}
		if provider == nil {
			// No metrics available — nothing to evaluate.
			continue
		}

		result := m.evaluator.Evaluate(cm.CapsuleID, cm.Rules, provider)
		if result != nil && result.Matched {
			event := ScaleEvent{
				CapsuleID: cm.CapsuleID,
				RuleName:  result.RuleName,
				Action:    result.Action,
				Timestamp: time.Now(),
			}
			m.logger.Info("scaling rule triggered",
				zap.String("capsule_id", string(cm.CapsuleID)),
				zap.String("rule", result.RuleName),
				zap.String("action", string(result.Action)))
			m.handler(event)
		}
	}
}
