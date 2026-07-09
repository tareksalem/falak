package node

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap/zaptest"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
)

// --- test doubles ---------------------------------------------------------

// fakeClock is a deterministic, advanceable clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeHost records dial calls and returns programmed results.
type fakeHost struct {
	mu           sync.Mutex
	connected    map[peer.ID]bool // peers reported as network.Connected
	dialErr      map[peer.ID]error
	defaultErr   error
	dialedPeers  []peer.ID
	dialedCounts map[peer.ID]int
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		connected:    make(map[peer.ID]bool),
		dialErr:      make(map[peer.ID]error),
		dialedCounts: make(map[peer.ID]int),
	}
}

func (h *fakeHost) Connectedness(p peer.ID) network.Connectedness {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connected[p] {
		return network.Connected
	}
	return network.NotConnected
}

func (h *fakeHost) Connect(_ context.Context, pi peer.AddrInfo) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dialedPeers = append(h.dialedPeers, pi.ID)
	h.dialedCounts[pi.ID]++
	if err, ok := h.dialErr[pi.ID]; ok {
		return err
	}
	return h.defaultErr
}

func (h *fakeHost) dialCount(p peer.ID) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dialedCounts[p]
}

func (h *fakeHost) totalDials() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.dialedPeers)
}

// fakePhonebook implements phonebook.IPhonebook with an in-memory map. Only
// GetByCluster and SetStatus are exercised by the reconnector; the rest are
// present to satisfy the interface and (where trivial) work as expected.
type fakePhonebook struct {
	mu      sync.Mutex
	byKey   map[string]*phonebook.Entry // "cluster\x00node" -> entry
	getErr  error
	setLog  []string // records SetStatus calls as "node:status"
	setErr  error
}

func newFakePhonebook() *fakePhonebook {
	return &fakePhonebook{byKey: make(map[string]*phonebook.Entry)}
}

func pbKey(node, cluster string) string { return cluster + "\x00" + node }

func (p *fakePhonebook) put(e *phonebook.Entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := *e
	p.byKey[pbKey(e.NodeID, e.ClusterPath)] = &cp
}

func (p *fakePhonebook) remove(node, cluster string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byKey, pbKey(node, cluster))
}

func (p *fakePhonebook) GetByCluster(clusterPath string) ([]*phonebook.Entry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.getErr != nil {
		return nil, p.getErr
	}
	var out []*phonebook.Entry
	for _, e := range p.byKey {
		if e.ClusterPath == clusterPath {
			cp := *e
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (p *fakePhonebook) SetStatus(nodeID, clusterPath string, status phonebook.NodeStatus) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.setErr != nil {
		return p.setErr
	}
	p.setLog = append(p.setLog, nodeID+":"+string(status))
	if e, ok := p.byKey[pbKey(nodeID, clusterPath)]; ok {
		e.Status = status
	}
	return nil
}

func (p *fakePhonebook) setStatusLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.setLog...)
}

// Unused IPhonebook methods — present only to satisfy the interface.
func (p *fakePhonebook) Get(string, string) (*phonebook.Entry, error) { return nil, errors.New("unused") }
func (p *fakePhonebook) Exists(string, string) (bool, error)          { return false, nil }
func (p *fakePhonebook) GetByNode(string) ([]*phonebook.Entry, error) { return nil, nil }
func (p *fakePhonebook) GetBestPeers(string, int) ([]*phonebook.Entry, error) {
	return nil, nil
}
func (p *fakePhonebook) Add(*phonebook.Entry) error                        { return nil }
func (p *fakePhonebook) Update(*phonebook.Entry) error                     { return nil }
func (p *fakePhonebook) Remove(string, string) error                       { return nil }
func (p *fakePhonebook) RemoveAllForNode(string) error                     { return nil }
func (p *fakePhonebook) RecordConnectionAttempt(string, string, bool) error { return nil }
func (p *fakePhonebook) RecordDisconnect(string, string) error             { return nil }
func (p *fakePhonebook) SetReliabilityScore(string, string, float64) error { return nil }
func (p *fakePhonebook) GetByStatus(string, phonebook.NodeStatus) ([]*phonebook.Entry, error) {
	return nil, nil
}
func (p *fakePhonebook) RecordProbe(string, string, bool) error { return nil }
func (p *fakePhonebook) Prune(time.Duration, int) (int, error)  { return 0, nil }
func (p *fakePhonebook) Count() (int, error)                    { return 0, nil }
func (p *fakePhonebook) CountByCluster(string) (int, error)     { return 0, nil }
func (p *fakePhonebook) Close() error                           { return nil }

// --- helpers --------------------------------------------------------------

const testCluster = "test/dc1/prod"

// peerA / peerB are valid, stable peer IDs derived deterministically from a
// fixed 32-byte ed25519 seed, so the same ID is produced on every run without
// hardcoding base58 strings.
var (
	peerA = mustSeedPeer(0x11)
	peerB = mustSeedPeer(0x22)
)

func mustSeedPeer(seed byte) peer.ID {
	raw := make([]byte, ed25519.SeedSize)
	for i := range raw {
		raw[i] = seed
	}
	priv := ed25519.NewKeyFromSeed(raw)
	pk, err := crypto.UnmarshalEd25519PrivateKey(priv)
	if err != nil {
		panic("failed to build test key: " + err.Error())
	}
	id, err := peer.IDFromPrivateKey(pk)
	if err != nil {
		panic("failed to derive test peer id: " + err.Error())
	}
	return id
}

func addrFor(p peer.ID) string {
	return "/ip4/127.0.0.1/tcp/4001/p2p/" + p.String()
}

func entryFor(p peer.ID, status phonebook.NodeStatus, addrs ...string) *phonebook.Entry {
	return &phonebook.Entry{
		NodeID:      p.String(),
		ClusterPath: testCluster,
		Status:      status,
		Addresses:   addrs,
	}
}

// newTestReconnector builds a reconnector wired to the given fakes with a
// deterministic clock and no jitter (so backoff math is exact).
func newTestReconnector(t *testing.T, h reconnectHost, pb phonebook.IPhonebook, bus events.Bus, clk reconnectClock, opts ...ReconnectOption) *Reconnector {
	t.Helper()
	base := []ReconnectOption{
		WithReconnectHostInterface(h),
		WithReconnectPhonebook(pb),
		WithReconnectEventBus(bus),
		WithReconnectClock(clk),
		WithReconnectJitter(0),
		WithReconnectBaseBackoff(5 * time.Second),
		WithReconnectMaxBackoff(5 * time.Minute),
		WithReconnectDialTimeout(time.Second),
		WithJoinedClustersFunc(func() []string { return []string{testCluster} }),
		WithReconnectLogger(zaptest.NewLogger(t)),
	}
	return NewReconnector(append(base, opts...)...)
}

// collectReauth waits up to a short deadline for exactly `want`
// ReauthWithPeerRequested events (the event bus fans out asynchronously), then
// drains any extras. Returns the collected events.
func collectReauth(t *testing.T, ch <-chan events.Event, want int) []events.ReauthWithPeerRequested {
	t.Helper()
	var out []events.ReauthWithPeerRequested
	deadline := time.After(2 * time.Second)
	for len(out) < want {
		select {
		case ev := <-ch:
			if e, ok := ev.(events.ReauthWithPeerRequested); ok {
				out = append(out, e)
			}
		case <-deadline:
			return out
		}
	}
	// Drain any surplus already queued so callers can assert exact counts.
	for {
		select {
		case ev := <-ch:
			if e, ok := ev.(events.ReauthWithPeerRequested); ok {
				out = append(out, e)
			}
		case <-time.After(50 * time.Millisecond):
			return out
		}
	}
}

// --- tests ----------------------------------------------------------------

func TestReconnector_DialsDisconnectedPeer(t *testing.T) {
	h := newFakeHost()
	pb := newFakePhonebook()
	pb.put(entryFor(peerA, phonebook.NodeStatusEnum.Active(), addrFor(peerA)))

	bus := events.NewBus()
	defer bus.Close()
	reauthCh := bus.Subscribe(events.TypeReauthWithPeer)

	clk := newFakeClock()
	r := newTestReconnector(t, h, pb, bus, clk)

	r.tick()

	if got := h.dialCount(peerA); got != 1 {
		t.Fatalf("expected 1 dial to peerA, got %d", got)
	}

	evs := collectReauth(t, reauthCh, 1)
	if len(evs) != 1 {
		t.Fatalf("expected 1 ReauthWithPeerRequested, got %d", len(evs))
	}
	if evs[0].PeerID != peerA.String() || evs[0].ClusterPath != testCluster {
		t.Fatalf("unexpected event %+v", evs[0])
	}
}

func TestReconnector_SkipsDeparted(t *testing.T) {
	h := newFakeHost()
	pb := newFakePhonebook()
	pb.put(entryFor(peerA, phonebook.NodeStatusEnum.Departed(), addrFor(peerA)))

	bus := events.NewBus()
	defer bus.Close()
	clk := newFakeClock()
	r := newTestReconnector(t, h, pb, bus, clk)

	r.tick()

	if got := h.totalDials(); got != 0 {
		t.Fatalf("expected no dials for Departed peer, got %d", got)
	}
}

func TestReconnector_SkipsConnected(t *testing.T) {
	h := newFakeHost()
	h.connected[peerA] = true
	pb := newFakePhonebook()
	pb.put(entryFor(peerA, phonebook.NodeStatusEnum.Active(), addrFor(peerA)))

	bus := events.NewBus()
	defer bus.Close()
	clk := newFakeClock()
	r := newTestReconnector(t, h, pb, bus, clk)

	r.tick()

	if got := h.totalDials(); got != 0 {
		t.Fatalf("expected no dials for already-connected peer, got %d", got)
	}
}

func TestReconnector_SkipsSelfEntry(t *testing.T) {
	h := newFakeHost()
	pb := newFakePhonebook()
	// A stale self-entry in the phonebook must never be dialed.
	pb.put(entryFor(peerA, phonebook.NodeStatusEnum.Active(), addrFor(peerA)))

	bus := events.NewBus()
	defer bus.Close()
	clk := newFakeClock()
	r := newTestReconnector(t, h, pb, bus, clk, WithReconnectSelfID(peerA.String()))

	r.tick()

	if got := h.totalDials(); got != 0 {
		t.Fatalf("expected no dial to self entry, got %d", got)
	}
}

func TestReconnector_SkipsNoAddresses(t *testing.T) {
	h := newFakeHost()
	pb := newFakePhonebook()
	pb.put(entryFor(peerA, phonebook.NodeStatusEnum.Active())) // no addrs

	bus := events.NewBus()
	defer bus.Close()
	clk := newFakeClock()
	r := newTestReconnector(t, h, pb, bus, clk)

	r.tick()

	if got := h.totalDials(); got != 0 {
		t.Fatalf("expected no dials for peer with no addresses, got %d", got)
	}
}

func TestReconnector_FailedPeerGatedToPendingAuth(t *testing.T) {
	h := newFakeHost()
	pb := newFakePhonebook()
	pb.put(entryFor(peerA, phonebook.NodeStatusEnum.Failed(), addrFor(peerA)))

	bus := events.NewBus()
	defer bus.Close()
	clk := newFakeClock()
	r := newTestReconnector(t, h, pb, bus, clk)

	r.tick()

	log := pb.setStatusLog()
	want := peerA.String() + ":" + string(phonebook.NodeStatusEnum.PendingAuth())
	found := false
	for _, l := range log {
		if l == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected SetStatus(PendingAuth) for Failed peer, got %v", log)
	}
	if got := h.dialCount(peerA); got != 1 {
		t.Fatalf("expected Failed peer to still be dialed, got %d", got)
	}
}

func TestReconnector_BackoffIncreasesOnFailureAndResetsOnSuccess(t *testing.T) {
	h := newFakeHost()
	h.defaultErr = errors.New("dial refused")
	pb := newFakePhonebook()
	pb.put(entryFor(peerA, phonebook.NodeStatusEnum.Active(), addrFor(peerA)))

	bus := events.NewBus()
	defer bus.Close()
	clk := newFakeClock()
	r := newTestReconnector(t, h, pb, bus, clk)

	// Tick 1: dial fails -> backoff = base (5s), nextAttempt = now+5s.
	r.tick()
	if h.dialCount(peerA) != 1 {
		t.Fatalf("tick1: expected 1 dial, got %d", h.dialCount(peerA))
	}
	key := backoffKey(testCluster, peerA.String())
	b := r.backoff[key]
	if b == nil || b.fails != 1 {
		t.Fatalf("tick1: expected fails=1, got %+v", b)
	}

	// Tick 2 immediately: still within backoff window -> no new dial.
	r.tick()
	if h.dialCount(peerA) != 1 {
		t.Fatalf("tick2 (within backoff): expected no new dial, got %d", h.dialCount(peerA))
	}

	// Advance past the first backoff and tick again: dial #2 fails ->
	// fails=2, backoff = 10s.
	clk.advance(6 * time.Second)
	r.tick()
	if h.dialCount(peerA) != 2 {
		t.Fatalf("tick3: expected dial #2, got %d", h.dialCount(peerA))
	}
	b = r.backoff[key]
	if b == nil || b.fails != 2 {
		t.Fatalf("tick3: expected fails=2, got %+v", b)
	}
	// nextAttempt should be now + 10s (base*2^1).
	wantNext := clk.Now().Add(10 * time.Second)
	if !b.nextAttempt.Equal(wantNext) {
		t.Fatalf("tick3: expected nextAttempt %v, got %v", wantNext, b.nextAttempt)
	}

	// Now let the dial succeed. Advance past backoff, flip host to accept.
	clk.advance(11 * time.Second)
	h.defaultErr = nil
	r.tick()
	if h.dialCount(peerA) != 3 {
		t.Fatalf("tick4: expected dial #3, got %d", h.dialCount(peerA))
	}
	if _, ok := r.backoff[key]; ok {
		t.Fatalf("tick4: expected backoff reset on success, still present: %+v", r.backoff[key])
	}
}

func TestReconnector_BackoffPrunedWhenPeerLeaves(t *testing.T) {
	h := newFakeHost()
	h.defaultErr = errors.New("dial refused")
	pb := newFakePhonebook()
	pb.put(entryFor(peerA, phonebook.NodeStatusEnum.Active(), addrFor(peerA)))

	bus := events.NewBus()
	defer bus.Close()
	clk := newFakeClock()
	r := newTestReconnector(t, h, pb, bus, clk)

	r.tick()
	key := backoffKey(testCluster, peerA.String())
	if _, ok := r.backoff[key]; !ok {
		t.Fatalf("expected backoff entry after failed dial")
	}

	// Peer leaves the phonebook (SWIM removed it). Next tick must prune.
	pb.remove(peerA.String(), testCluster)
	r.tick()
	if _, ok := r.backoff[key]; ok {
		t.Fatalf("expected backoff pruned after peer left phonebook")
	}
}

func TestReconnector_BootstrapSeedCandidateWhenAbsent(t *testing.T) {
	h := newFakeHost()
	pb := newFakePhonebook() // empty phonebook

	bus := events.NewBus()
	defer bus.Close()
	reauthCh := bus.Subscribe(events.TypeReauthWithPeer)
	clk := newFakeClock()

	r := newTestReconnector(t, h, pb, bus, clk,
		WithBootstrapSeeds([]string{addrFor(peerB)}))

	r.tick()

	if got := h.dialCount(peerB); got != 1 {
		t.Fatalf("expected seed peerB to be dialed once, got %d", got)
	}

	evs := collectReauth(t, reauthCh, 1)
	if len(evs) != 1 || evs[0].PeerID != peerB.String() {
		t.Fatalf("expected reauth event for seed peerB, got %+v", evs)
	}
}

func TestReconnector_SeedDedupedAgainstPhonebook(t *testing.T) {
	h := newFakeHost()
	pb := newFakePhonebook()
	pb.put(entryFor(peerB, phonebook.NodeStatusEnum.Active(), addrFor(peerB)))

	bus := events.NewBus()
	defer bus.Close()
	clk := newFakeClock()

	r := newTestReconnector(t, h, pb, bus, clk,
		WithBootstrapSeeds([]string{addrFor(peerB)}))

	r.tick()

	// peerB is both a phonebook entry and a seed — must be dialed exactly
	// once per tick, not twice.
	if got := h.dialCount(peerB); got != 1 {
		t.Fatalf("expected seed/phonebook peer dialed once, got %d", got)
	}
}
