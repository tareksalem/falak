package sync

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap/zaptest"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
)

const testCluster = "acme/eu-west/dc1"

// syncNode bundles the pieces one node needs to participate in a sync test:
// a real in-process libp2p host, a real SQLite phonebook (temp file), a real
// event bus, and the Syncer under test.
type syncNode struct {
	t         *testing.T
	host      host.Host
	phonebook phonebook.IPhonebook
	bus       events.Bus
	syncer    *Syncer
}

// newSyncNode builds and starts a syncNode. Extra options are appended after
// the defaults so a test can override e.g. the clock or disable member push.
func newSyncNode(t *testing.T, ctx context.Context, name string, extra ...Option) *syncNode {
	t.Helper()

	h, err := libp2p.New(libp2p.ListenAddrs(mustAddr(t, "/ip4/127.0.0.1/tcp/0")))
	if err != nil {
		t.Fatalf("%s: libp2p.New: %v", name, err)
	}
	t.Cleanup(func() { _ = h.Close() })

	pb, err := phonebook.Open(t.TempDir() + "/" + name + ".db")
	if err != nil {
		t.Fatalf("%s: phonebook.Open: %v", name, err)
	}
	t.Cleanup(func() { _ = pb.Close() })

	bus := events.NewBus(events.WithContext(ctx))
	t.Cleanup(func() { bus.Close() })

	opts := []Option{
		WithContext(ctx),
		WithHost(h),
		WithPhonebook(pb),
		WithEventBus(bus),
		WithLogger(zaptest.NewLogger(t).Named(name)),
	}
	opts = append(opts, extra...)

	s := New(opts...)
	if err := s.Start(); err != nil {
		t.Fatalf("%s: syncer.Start: %v", name, err)
	}
	t.Cleanup(func() { s.Stop() })

	return &syncNode{t: t, host: h, phonebook: pb, bus: bus, syncer: s}
}

func mustAddr(t *testing.T, s string) multiaddr.Multiaddr {
	t.Helper()
	a, err := multiaddr.NewMultiaddr(s)
	if err != nil {
		t.Fatalf("bad multiaddr %q: %v", s, err)
	}
	return a
}

// connect wires a's peerstore/connection to b so a can dial b.
func connect(t *testing.T, ctx context.Context, a, b *syncNode) {
	t.Helper()
	ai := peer.AddrInfo{ID: b.host.ID(), Addrs: b.host.Addrs()}
	if err := a.host.Connect(ctx, ai); err != nil {
		t.Fatalf("connect %s->%s: %v", a.host.ID(), b.host.ID(), err)
	}
}

// addMember inserts an Active phonebook entry for other into n's phonebook,
// carrying other's real dialable addresses so pushes/syncs can reach it.
func (n *syncNode) addMember(other *syncNode) {
	n.t.Helper()
	addrs := make([]string, 0, len(other.host.Addrs()))
	for _, a := range other.host.Addrs() {
		addrs = append(addrs, a.String())
	}
	entry := &phonebook.Entry{
		NodeID:      other.host.ID().String(),
		ClusterPath: testCluster,
		Addresses:   addrs,
		Status:      phonebook.NodeStatusEnum.Active(),
		FirstSeen:   time.Now(),
		LastSeen:    time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := n.phonebook.Add(entry); err != nil {
		n.t.Fatalf("addMember: %v", err)
	}
}

// addSelf inserts n's own Active entry (a node is in its own phonebook).
func (n *syncNode) addSelf() {
	n.t.Helper()
	addrs := make([]string, 0, len(n.host.Addrs()))
	for _, a := range n.host.Addrs() {
		addrs = append(addrs, a.String())
	}
	entry := &phonebook.Entry{
		NodeID:      n.host.ID().String(),
		ClusterPath: testCluster,
		Addresses:   addrs,
		Status:      phonebook.NodeStatusEnum.Active(),
		FirstSeen:   time.Now(),
		LastSeen:    time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := n.phonebook.Add(entry); err != nil {
		n.t.Fatalf("addSelf: %v", err)
	}
}

func (n *syncNode) count() int {
	n.t.Helper()
	entries, err := n.phonebook.GetByCluster(testCluster)
	if err != nil {
		n.t.Fatalf("GetByCluster: %v", err)
	}
	return len(entries)
}

func (n *syncNode) exists(id string) bool {
	n.t.Helper()
	ok, err := n.phonebook.Exists(id, testCluster)
	if err != nil {
		n.t.Fatalf("Exists: %v", err)
	}
	return ok
}

// waitEvent blocks until an event of the given type satisfying pred is seen or
// the deadline elapses. It returns the matching event. This is the
// deterministic hook that replaces convergence sleeps.
func waitEvent(t *testing.T, ch <-chan events.Event, timeout time.Duration, pred func(events.Event) bool) events.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatal("event channel closed before match")
			}
			if pred == nil || pred(e) {
				return e
			}
		case <-deadline:
			t.Fatal("timed out waiting for event")
		}
	}
}
