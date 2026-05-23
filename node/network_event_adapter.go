package node

import (
	"context"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/network"
	"github.com/tareksalem/falak/node/internal/events"
)

// networkEventAdapter implements network.EventSource by translating the
// node-internal event bus into the manager's CapsuleEvent shape. One
// adapter per NetworkHandler; the lifecycle is owned by the handler.
//
// Per-event subscriptions are spun up in start() and torn down in
// stop(); each subscription pump fans out to every registered On*
// callback, mirroring how the in-memory event source in
// network/manager_test.go works (so callers exercising the production
// path see the same semantics as the unit-test path).
type networkEventAdapter struct {
	parentCtx   context.Context
	bus         events.Bus
	localNodeID string
	logger      *zap.Logger

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped bool

	receivedCBs []func(network.CapsuleEvent)
	runningCBs  []func(network.CapsuleEvent)
	stoppedCBs  []func(network.CapsuleEvent)
	deletedCBs  []func(network.CapsuleEvent)
	joinedCBs   []func(network.CapsuleEvent)
	leftCBs     []func(network.CapsuleEvent)
}

// newNetworkEventAdapter constructs an adapter. start() must be called
// to activate the subscriptions; stop() cancels them and waits for the
// per-event goroutines to exit.
func newNetworkEventAdapter(parent context.Context, bus events.Bus, localNodeID string, logger *zap.Logger) *networkEventAdapter {
	return &networkEventAdapter{
		parentCtx:   parent,
		bus:         bus,
		localNodeID: localNodeID,
		logger:      logger,
	}
}

// start spins one goroutine per event type. Each goroutine reads from
// the node bus until ctx cancels, fanning every received event out to
// the registered callbacks.
func (a *networkEventAdapter) start() {
	ctx, cancel := context.WithCancel(a.parentCtx)
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()

	a.spawn(ctx, events.TypeCapsuleReceived, a.translateReceived)
	a.spawn(ctx, events.TypeCapsuleRunning, a.translateRunning)
	a.spawn(ctx, events.TypeCapsuleStopped, a.translateStopped)
	a.spawn(ctx, events.TypeCapsuleWithdrawn, a.translateWithdrawn)
	a.spawn(ctx, events.TypeNewMemberReceived, a.translateMemberReceived)
	a.spawn(ctx, events.TypeNodeFailed, a.translateNodeFailed)
	a.spawn(ctx, events.TypeNodeRecovered, a.translateNodeRecovered)
}

// spawn subscribes to one event type and dispatches incoming events
// through translate. Each pump shares the adapter's WaitGroup so Stop
// blocks until every callback returns.
func (a *networkEventAdapter) spawn(ctx context.Context, eventType string, translate func(events.Event)) {
	ch := a.bus.Subscribe(eventType)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				translate(ev)
			}
		}
	}()
}

// stop cancels every pump and waits for clean exit. Idempotent.
func (a *networkEventAdapter) stop() {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	a.stopped = true
	cancel := a.cancel
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	a.wg.Wait()
}

// translateReceived projects a CapsuleReceived bus event into a
// network.CapsuleEvent and fans it out to OnCapsuleReceived callbacks.
func (a *networkEventAdapter) translateReceived(ev events.Event) {
	got, ok := ev.(events.CapsuleReceived)
	if !ok {
		return
	}
	a.fanout(a.receivedCBs, network.CapsuleEvent{
		ClusterPath: got.ClusterPath,
		CapsuleName: got.CapsuleName,
	})
}

// translateRunning projects a CapsuleRunning bus event.
func (a *networkEventAdapter) translateRunning(ev events.Event) {
	got, ok := ev.(events.CapsuleRunning)
	if !ok {
		return
	}
	a.fanout(a.runningCBs, network.CapsuleEvent{CapsuleName: got.CapsuleID})
}

// translateStopped projects a CapsuleExecutionFailed bus event onto the
// Stopped callback (the node bus emits Stopped via the failure path).
func (a *networkEventAdapter) translateStopped(ev events.Event) {
	got, ok := ev.(events.CapsuleExecutionFailed)
	if !ok {
		return
	}
	a.fanout(a.stoppedCBs, network.CapsuleEvent{CapsuleName: got.CapsuleID})
}

// translateWithdrawn projects a CapsuleWithdrawn bus event onto the
// Deleted callback. Withdraw == delete from the network manager's
// perspective: bridge teardown is ref-counted, capsule removal frees a
// ref so the bridge eventually disappears.
func (a *networkEventAdapter) translateWithdrawn(ev events.Event) {
	got, ok := ev.(events.CapsuleWithdrawn)
	if !ok {
		return
	}
	a.fanout(a.deletedCBs, network.CapsuleEvent{
		ClusterPath: got.ClusterPath,
		CapsuleName: got.CapsuleID,
	})
}

// translateMemberReceived projects a NewMemberReceived bus event onto
// OnPeerJoined, skipping self-events.
func (a *networkEventAdapter) translateMemberReceived(ev events.Event) {
	got, ok := ev.(events.NewMemberReceived)
	if !ok {
		return
	}
	if got.NodeID == "" || got.NodeID == a.localNodeID {
		return
	}
	a.fanout(a.joinedCBs, network.CapsuleEvent{
		ClusterPath: got.ClusterPath,
		NodeID:      got.NodeID,
		NodeIP:      firstAddr(got.Addresses),
	})
}

// translateNodeFailed projects a NodeFailed bus event onto OnPeerLeft.
func (a *networkEventAdapter) translateNodeFailed(ev events.Event) {
	got, ok := ev.(events.NodeFailed)
	if !ok {
		return
	}
	if got.NodeID == "" || got.NodeID == a.localNodeID {
		return
	}
	a.fanout(a.leftCBs, network.CapsuleEvent{
		ClusterPath: got.ClusterPath,
		NodeID:      got.NodeID,
	})
}

// translateNodeRecovered projects a NodeRecovered bus event onto
// OnPeerJoined — a recovered peer rejoins the overlay membership.
func (a *networkEventAdapter) translateNodeRecovered(ev events.Event) {
	got, ok := ev.(events.NodeRecovered)
	if !ok {
		return
	}
	if got.NodeID == "" || got.NodeID == a.localNodeID {
		return
	}
	a.fanout(a.joinedCBs, network.CapsuleEvent{
		ClusterPath: got.ClusterPath,
		NodeID:      got.NodeID,
	})
}

// OnCapsuleReceived registers a callback for "capsule announcement
// observed locally" events. Returns a cancel function that
// unregisters this callback.
func (a *networkEventAdapter) OnCapsuleReceived(fn func(network.CapsuleEvent)) func() {
	return a.register(&a.receivedCBs, fn)
}

// OnCapsuleRunning registers a callback for "capsule reached Running"
// events.
func (a *networkEventAdapter) OnCapsuleRunning(fn func(network.CapsuleEvent)) func() {
	return a.register(&a.runningCBs, fn)
}

// OnCapsuleStopped registers a callback for "capsule transitioned to
// Stopped" events.
func (a *networkEventAdapter) OnCapsuleStopped(fn func(network.CapsuleEvent)) func() {
	return a.register(&a.stoppedCBs, fn)
}

// OnCapsuleDeleted registers a callback for "capsule withdrawn /
// deleted" events.
func (a *networkEventAdapter) OnCapsuleDeleted(fn func(network.CapsuleEvent)) func() {
	return a.register(&a.deletedCBs, fn)
}

// OnPeerJoined registers a callback for "new peer announced /
// recovered" events.
func (a *networkEventAdapter) OnPeerJoined(fn func(network.CapsuleEvent)) func() {
	return a.register(&a.joinedCBs, fn)
}

// OnPeerLeft registers a callback for "peer failed / departed" events.
func (a *networkEventAdapter) OnPeerLeft(fn func(network.CapsuleEvent)) func() {
	return a.register(&a.leftCBs, fn)
}

func (a *networkEventAdapter) register(list *[]func(network.CapsuleEvent), fn func(network.CapsuleEvent)) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	idx := len(*list)
	*list = append(*list, fn)
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if idx < len(*list) {
			(*list)[idx] = nil
		}
	}
}

func (a *networkEventAdapter) fanout(list []func(network.CapsuleEvent), ev network.CapsuleEvent) {
	a.mu.Lock()
	cbs := make([]func(network.CapsuleEvent), len(list))
	copy(cbs, list)
	a.mu.Unlock()
	for _, cb := range cbs {
		if cb != nil {
			cb(ev)
		}
	}
}

// firstAddr picks the first non-empty multiaddr from the slice and
// extracts its raw IP. The node phonebook records multiaddrs as
// strings; the network manager only needs the underlay IP for VXLAN.
func firstAddr(addrs []string) string {
	for _, a := range addrs {
		if a == "" {
			continue
		}
		parts := strings.Split(a, "/")
		for i, p := range parts {
			if (p == "ip4" || p == "ip6") && i+1 < len(parts) {
				return parts[i+1]
			}
		}
		return a
	}
	return ""
}

// Compile-time check: networkEventAdapter satisfies network.EventSource.
var _ network.EventSource = (*networkEventAdapter)(nil)
