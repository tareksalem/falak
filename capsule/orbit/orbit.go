// Package orbit manages GossipSub topics (orbits) where capsules travel.
// Orbits use flat naming — each orbit maps to a PubSub topic under the cluster path.
package orbit

import (
	"context"
	"fmt"
	"sync"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"go.uber.org/zap"
)

// BuildOrbitTopic builds the PubSub topic name for an orbit.
// Format: falak/<clusterPath>/orbit/<orbitName>
func BuildOrbitTopic(clusterPath, orbitName string) string {
	return fmt.Sprintf("falak/%s/orbit/%s", clusterPath, orbitName)
}

// BuildCapsuleTopic builds the PubSub topic for a specific capsule's status updates.
// Format: falak/<clusterPath>/capsule/<capsuleID>
func BuildCapsuleTopic(clusterPath, capsuleID string) string {
	return fmt.Sprintf("falak/%s/capsule/%s", clusterPath, capsuleID)
}

// orbitEntry holds the topic and subscription for a joined orbit.
type orbitEntry struct {
	topic        *pubsub.Topic
	subscription *pubsub.Subscription
	cancel       context.CancelFunc
}

// Manager manages orbit (PubSub topic) subscriptions for a cluster.
// The Manager owns a long-lived context that governs all orbit message loops;
// passing a short-lived context to Join is discouraged.
type Manager struct {
	mu          sync.RWMutex
	ps          *pubsub.PubSub
	clusterPath string
	nodeID      string
	orbits      map[string]*orbitEntry // orbitName -> entry
	logger      *zap.Logger
	handler     MessageHandler

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // tracks message loops so LeaveAll can wait
}

// MessageHandler processes incoming orbit messages.
type MessageHandler func(orbitName string, data []byte, senderID string)

// Option configures a Manager.
type Option func(*Manager)

// WithPubSub sets the libp2p PubSub instance.
func WithPubSub(ps *pubsub.PubSub) Option {
	return func(m *Manager) {
		m.ps = ps
	}
}

// WithClusterPath sets the cluster path.
func WithClusterPath(path string) Option {
	return func(m *Manager) {
		m.clusterPath = path
	}
}

// WithNodeID sets the local node ID (for filtering self-messages).
func WithNodeID(id string) Option {
	return func(m *Manager) {
		m.nodeID = id
	}
}

// WithLogger sets the logger.
func WithLogger(logger *zap.Logger) Option {
	return func(m *Manager) {
		m.logger = logger
	}
}

// WithHandler sets the message handler for incoming orbit messages.
func WithHandler(h MessageHandler) Option {
	return func(m *Manager) {
		m.handler = h
	}
}

// WithContext sets the parent context for the manager's message loops.
// All orbit message loops derive from this context and exit when it is canceled.
func WithContext(ctx context.Context) Option {
	return func(m *Manager) {
		m.ctx = ctx
	}
}

// SetHandler updates the message handler after construction.
// Safe to call concurrently with the message loop.
func (m *Manager) SetHandler(h MessageHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h == nil {
		m.handler = func(string, []byte, string) {}
		return
	}
	m.handler = h
}

// getHandler returns the current handler (thread-safe).
func (m *Manager) getHandler() MessageHandler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.handler
}

// NewManager creates a new orbit manager.
// If WithContext is not provided, a background context is used; the manager
// will then only stop when LeaveAll is called explicitly.
func NewManager(opts ...Option) *Manager {
	m := &Manager{
		orbits:  make(map[string]*orbitEntry),
		logger:  zap.NewNop(),
		handler: func(string, []byte, string) {},
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.ctx == nil {
		m.ctx = context.Background()
	}
	m.ctx, m.cancel = context.WithCancel(m.ctx)
	return m
}

// Join subscribes to an orbit topic and starts the message loop.
// The ctx parameter is only used for the Join operation itself;
// the message loop runs on the manager's long-lived context.
func (m *Manager) Join(_ context.Context, orbitName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.orbits[orbitName]; exists {
		return fmt.Errorf("already joined orbit %q", orbitName)
	}

	topicName := BuildOrbitTopic(m.clusterPath, orbitName)
	topic, err := m.ps.Join(topicName)
	if err != nil {
		return fmt.Errorf("failed to join orbit topic %q: %w", topicName, err)
	}

	sub, err := topic.Subscribe()
	if err != nil {
		topic.Close()
		return fmt.Errorf("failed to subscribe to orbit topic %q: %w", topicName, err)
	}

	// Derive a per-orbit context from the manager's long-lived context so
	// Leave can cancel just this orbit without affecting others.
	orbitCtx, cancel := context.WithCancel(m.ctx)
	entry := &orbitEntry{
		topic:        topic,
		subscription: sub,
		cancel:       cancel,
	}
	m.orbits[orbitName] = entry

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.messageLoop(orbitCtx, orbitName, sub)
	}()

	m.logger.Info("joined orbit",
		zap.String("cluster", m.clusterPath),
		zap.String("orbit", orbitName),
		zap.String("topic", topicName))
	return nil
}

// Leave unsubscribes from an orbit topic.
func (m *Manager) Leave(orbitName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, exists := m.orbits[orbitName]
	if !exists {
		return fmt.Errorf("not joined to orbit %q", orbitName)
	}

	entry.cancel()
	entry.subscription.Cancel()
	entry.topic.Close()
	delete(m.orbits, orbitName)

	m.logger.Info("left orbit",
		zap.String("cluster", m.clusterPath),
		zap.String("orbit", orbitName))
	return nil
}

// LeaveAll unsubscribes from all orbits, cancels the manager's context, and
// waits for every message loop goroutine to exit. After LeaveAll the manager
// cannot be reused — create a new one if needed.
func (m *Manager) LeaveAll() {
	m.mu.Lock()
	for name, entry := range m.orbits {
		entry.cancel()
		entry.subscription.Cancel()
		entry.topic.Close()
		m.logger.Debug("left orbit",
			zap.String("cluster", m.clusterPath),
			zap.String("orbit", name))
	}
	m.orbits = make(map[string]*orbitEntry)

	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()

	// Wait outside the lock so message loops can finish.
	m.wg.Wait()
}

// Publish sends data to an orbit topic.
func (m *Manager) Publish(ctx context.Context, orbitName string, data []byte) error {
	m.mu.RLock()
	entry, exists := m.orbits[orbitName]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("not joined to orbit %q", orbitName)
	}

	return entry.topic.Publish(ctx, data)
}

// JoinedOrbits returns the names of all currently joined orbits.
func (m *Manager) JoinedOrbits() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.orbits))
	for name := range m.orbits {
		names = append(names, name)
	}
	return names
}

// IsJoined returns true if the manager is subscribed to the given orbit.
func (m *Manager) IsJoined(orbitName string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.orbits[orbitName]
	return exists
}

func (m *Manager) messageLoop(ctx context.Context, orbitName string, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.logger.Error("orbit message error",
				zap.String("cluster", m.clusterPath),
				zap.String("orbit", orbitName),
				zap.Error(err))
			continue
		}

		// Skip self-messages
		if msg.ReceivedFrom.String() == m.nodeID {
			continue
		}

		m.getHandler()(orbitName, msg.Data, msg.ReceivedFrom.String())
	}
}
