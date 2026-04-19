package events

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/reactivex/rxgo/v2"
	"go.uber.org/zap"
)

// DefaultSubscriberBufferSize is the channel buffer used when a subscriber
// does not pass WithSubscriberBufferSize.
const DefaultSubscriberBufferSize = 100

// DropWarnInterval caps how often the bus logs a "channel full" warning
// for any subscriber. The drop counter (see Bus.Dropped) is always bumped
// regardless of whether a warning fires.
const DropWarnInterval = time.Second

// SubscribeOption configures a Subscribe/SubscribeAll call.
type SubscribeOption func(*subscribeConfig)

type subscribeConfig struct {
	bufferSize int
}

// WithSubscriberBufferSize sets the buffer size of the subscriber channel.
// Passing a non-positive value reverts to DefaultSubscriberBufferSize.
func WithSubscriberBufferSize(n int) SubscribeOption {
	return func(c *subscribeConfig) {
		if n > 0 {
			c.bufferSize = n
		}
	}
}

// Bus provides publish-subscribe functionality with fanout support.
type Bus interface {
	// Publish sends an event to all subscribers.
	Publish(event Event)

	// Subscribe returns a channel that receives events of the specified type.
	// Multiple subscribers can subscribe to the same event type (fanout).
	// Pass WithSubscriberBufferSize to override the default channel buffer.
	Subscribe(eventType string, opts ...SubscribeOption) <-chan Event

	// Unsubscribe removes a subscription channel.
	Unsubscribe(eventType string, ch <-chan Event)

	// SubscribeAll returns a channel that receives all events.
	SubscribeAll(opts ...SubscribeOption) <-chan Event

	// Observable returns an rxgo Observable for the specified event type.
	// Use this for reactive programming patterns (map, filter, etc.)
	Observable(eventType string) rxgo.Observable

	// ObservableAll returns an rxgo Observable for all events.
	ObservableAll() rxgo.Observable

	// Dropped returns the cumulative number of events dropped because a
	// subscriber channel was full at dispatch time. Useful for tests and
	// observability; resets only when the bus is replaced.
	Dropped() uint64

	// Close shuts down the event bus.
	Close()
}

// bus implements Bus using rxgo.
type bus struct {
	mu          sync.RWMutex
	logger      *zap.Logger
	bufferSize  int
	inputCh     chan rxgo.Item
	observable  rxgo.Observable
	subscribers map[string][]chan Event
	allSubs     []chan Event
	parentCtx   context.Context // Set via WithContext option
	ctx         context.Context
	cancel      context.CancelFunc
	closed      bool
	wg          sync.WaitGroup

	// Drop accounting. droppedCount is bumped every time fanout cannot
	// place an event into a subscriber channel because it was full.
	// lastWarnUnix rate-limits the accompanying Warn log so a saturated
	// subscriber does not flood the log.
	droppedCount atomic.Uint64
	lastWarnUnix atomic.Int64
}

// BusOption configures a Bus.
type BusOption func(*bus)

// WithContext sets the parent context for the event bus.
// The bus will derive its internal context from this parent,
// enabling proper cancellation propagation from the parent component.
func WithContext(ctx context.Context) BusOption {
	return func(b *bus) {
		b.parentCtx = ctx
	}
}

// WithLogger sets the logger for the event bus.
func WithLogger(logger *zap.Logger) BusOption {
	return func(b *bus) {
		b.logger = logger
	}
}

// WithBufferSize sets the buffer size for the event channel.
func WithBufferSize(size int) BusOption {
	return func(b *bus) {
		b.bufferSize = size
	}
}

// NewBus creates a new event bus.
func NewBus(opts ...BusOption) Bus {
	b := &bus{
		logger:      zap.NewNop(),
		bufferSize:  1000,
		subscribers: make(map[string][]chan Event),
		allSubs:     make([]chan Event, 0),
	}

	// Apply options first to capture parentCtx if provided
	for _, opt := range opts {
		opt(b)
	}

	// Derive context from parent if provided, otherwise use Background
	if b.parentCtx != nil {
		b.ctx, b.cancel = context.WithCancel(b.parentCtx)
	} else {
		b.ctx, b.cancel = context.WithCancel(context.Background())
	}

	b.inputCh = make(chan rxgo.Item, b.bufferSize)

	// Create observable from input channel
	b.observable = rxgo.FromChannel(b.inputCh, rxgo.WithContext(b.ctx))

	// Start the fanout dispatcher
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.dispatch()
	}()

	return b
}

// Publish sends an event to all subscribers.
func (b *bus) Publish(event Event) {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return
	}
	b.mu.RUnlock()

	select {
	case b.inputCh <- rxgo.Of(event):
		b.logger.Debug("event published",
			zap.String("type", event.EventType()))
	case <-b.ctx.Done():
		return
	default:
		b.logger.Warn("event bus full, dropping event",
			zap.String("type", event.EventType()))
	}
}

// Subscribe returns a channel that receives events of the specified type.
func (b *bus) Subscribe(eventType string, opts ...SubscribeOption) <-chan Event {
	cfg := subscribeConfig{bufferSize: DefaultSubscriberBufferSize}
	for _, opt := range opts {
		opt(&cfg)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan Event, cfg.bufferSize)
	b.subscribers[eventType] = append(b.subscribers[eventType], ch)

	b.logger.Debug("subscriber added",
		zap.String("type", eventType),
		zap.Int("total", len(b.subscribers[eventType])),
		zap.Int("buffer", cfg.bufferSize))

	return ch
}

// Unsubscribe removes a subscription channel.
func (b *bus) Unsubscribe(eventType string, ch <-chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	subs := b.subscribers[eventType]
	for i, sub := range subs {
		if sub == ch {
			// Remove from slice
			b.subscribers[eventType] = append(subs[:i], subs[i+1:]...)
			// Close the channel
			close(sub)
			b.logger.Debug("subscriber removed",
				zap.String("type", eventType),
				zap.Int("remaining", len(b.subscribers[eventType])))
			return
		}
	}
}

// SubscribeAll returns a channel that receives all events.
func (b *bus) SubscribeAll(opts ...SubscribeOption) <-chan Event {
	cfg := subscribeConfig{bufferSize: DefaultSubscriberBufferSize}
	for _, opt := range opts {
		opt(&cfg)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan Event, cfg.bufferSize)
	b.allSubs = append(b.allSubs, ch)

	return ch
}

// Dropped returns the cumulative number of events dropped because a
// subscriber channel was full at dispatch time.
func (b *bus) Dropped() uint64 {
	return b.droppedCount.Load()
}

// Observable returns an rxgo Observable for the specified event type.
func (b *bus) Observable(eventType string) rxgo.Observable {
	return b.observable.Filter(func(item interface{}) bool {
		if event, ok := item.(Event); ok {
			return event.EventType() == eventType
		}
		return false
	})
}

// ObservableAll returns an rxgo Observable for all events.
func (b *bus) ObservableAll() rxgo.Observable {
	return b.observable
}

// Close shuts down the event bus.
func (b *bus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.cancel()
	close(b.inputCh)
	b.mu.Unlock()

	// Wait for dispatch goroutine to finish
	b.wg.Wait()

	// Close all subscriber channels
	b.mu.Lock()
	for _, subs := range b.subscribers {
		for _, ch := range subs {
			close(ch)
		}
	}
	for _, ch := range b.allSubs {
		close(ch)
	}
	b.mu.Unlock()

	b.logger.Debug("event bus closed")
}

// dispatch fans out events to all subscribers.
func (b *bus) dispatch() {
	for {
		select {
		case <-b.ctx.Done():
			return
		case item, ok := <-b.inputCh:
			if !ok {
				return
			}

			event, ok := item.V.(Event)
			if !ok {
				continue
			}

			b.fanout(event)
		}
	}
}

// fanout sends event to all matching subscribers.
func (b *bus) fanout(event Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	eventType := event.EventType()

	if subs, ok := b.subscribers[eventType]; ok {
		for _, ch := range subs {
			select {
			case ch <- event:
			default:
				b.recordDrop(eventType, "subscriber channel full, dropping event")
			}
		}
	}

	for _, ch := range b.allSubs {
		select {
		case ch <- event:
		default:
			b.recordDrop(eventType, "all-events subscriber channel full, dropping event")
		}
	}
}

// recordDrop bumps the drop counter and emits a rate-limited Warn. The
// counter is authoritative; the Warn is throttled to DropWarnInterval so
// a saturated subscriber cannot flood the log.
func (b *bus) recordDrop(eventType, msg string) {
	total := b.droppedCount.Add(1)

	now := time.Now().Unix()
	last := b.lastWarnUnix.Load()
	if now-last >= int64(DropWarnInterval/time.Second) &&
		b.lastWarnUnix.CompareAndSwap(last, now) {
		b.logger.Warn(msg,
			zap.String("type", eventType),
			zap.Uint64("total_dropped", total))
	}
}
