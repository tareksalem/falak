package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// --- fakes ---------------------------------------------------------------

type fakeProvider struct {
	cands  []Candidate
	dc     string
	region string
}

func (f *fakeProvider) Candidates(_, _ string) []Candidate { return f.cands }
func (f *fakeProvider) LocalDatacenter() string            { return f.dc }
func (f *fakeProvider) LocalRegion() string                { return f.region }

type fakeIndex struct {
	mu      sync.Mutex
	holders []string
}

func (f *fakeIndex) FindHolders(_, _ string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.holders...)
}

type fakeBroadcaster struct {
	mu    sync.Mutex
	calls []string // capsule/tag
}

func (f *fakeBroadcaster) BroadcastAvailable(capsuleID, tag, checksum string, size int64) error {
	f.mu.Lock()
	f.calls = append(f.calls, capsuleID+"/"+tag)
	f.mu.Unlock()
	return nil
}

func (f *fakeBroadcaster) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeTransport records pushes, can force per-target failures, and can gate
// pushes on a channel to exercise the concurrency cap.
type fakeTransport struct {
	mu          sync.Mutex
	pushed      []string
	failUntil   map[string]int
	inflight    int
	maxInflight int
	gate        chan struct{}
	pushedCh    chan string
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{failUntil: map[string]int{}, pushedCh: make(chan string, 64)}
}

func (f *fakeTransport) Push(ctx context.Context, target Candidate, _ ReplicateRequest) error {
	f.mu.Lock()
	f.inflight++
	if f.inflight > f.maxInflight {
		f.maxInflight = f.inflight
	}
	gate := f.gate
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
	}()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	f.mu.Lock()
	if rem := f.failUntil[target.NodeID]; rem > 0 {
		f.failUntil[target.NodeID] = rem - 1
		f.mu.Unlock()
		return errors.New("forced failure")
	}
	f.pushed = append(f.pushed, target.NodeID)
	f.mu.Unlock()
	select {
	case f.pushedCh <- target.NodeID:
	default:
	}
	return nil
}

func (f *fakeTransport) pushedNodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pushed...)
}

func (f *fakeTransport) currentInflight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inflight
}

func (f *fakeTransport) peakInflight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInflight
}

// --- tests ---------------------------------------------------------------

func TestReplicator_ReplicatesKTargets(t *testing.T) {
	tr := newFakeTransport()
	prov := &fakeProvider{
		dc: "dc-holder",
		cands: []Candidate{
			{NodeID: "n1", Datacenter: "dc-a", DiskMBFree: 9000, Gravity: 1, Active: true},
			{NodeID: "n2", Datacenter: "dc-b", DiskMBFree: 9000, Gravity: 1, Active: true},
			{NodeID: "n3", Datacenter: "dc-c", DiskMBFree: 9000, Gravity: 1, Active: true},
		},
	}
	r := NewReplicator(
		WithReplicationCandidateProvider(prov),
		WithReplicationIndex(&fakeIndex{}),
		WithReplicationTransport(tr),
		WithReplicationFactor(2),
		WithReplicationConcurrency(1),
		WithReplicationPrePushJitter(0),
		WithReplicationRetries(0),
	)
	defer r.Stop()

	r.Replicate("cap1", "v1", "chk", 1234)

	// Expect exactly K=2 pushes.
	for i := 0; i < 2; i++ {
		select {
		case <-tr.pushedCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d/2 pushes observed", i)
		}
	}
	// Give the worker a beat to confirm it does not push a 3rd.
	time.Sleep(100 * time.Millisecond)
	pushed := tr.pushedNodes()
	if len(pushed) != 2 {
		t.Fatalf("expected exactly 2 pushes (K), got %d: %v", len(pushed), pushed)
	}
	// Failure-domain spread: the two targets are in distinct DCs.
	if pushed[0] == pushed[1] {
		t.Errorf("two replicas placed on the same node: %v", pushed)
	}
}

func TestReplicator_ShortfallWarns(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	tr := newFakeTransport()
	prov := &fakeProvider{
		dc: "dc-holder",
		cands: []Candidate{
			{NodeID: "only", Datacenter: "dc-a", DiskMBFree: 9000, Gravity: 1, Active: true},
		},
	}
	r := NewReplicator(
		WithReplicationCandidateProvider(prov),
		WithReplicationIndex(&fakeIndex{}),
		WithReplicationTransport(tr),
		WithReplicationFactor(2), // want 2 copies but only 1 candidate
		WithReplicationConcurrency(1),
		WithReplicationPrePushJitter(0),
		WithReplicationRetries(0),
		WithReplicationLogger(zap.New(core)),
	)
	defer r.Stop()

	r.Replicate("cap1", "v1", "chk", 10)

	// One push happens (best-effort), then a shortfall WARN.
	select {
	case <-tr.pushedCh:
	case <-time.After(3 * time.Second):
		t.Fatal("expected the single best-effort push")
	}

	deadline := time.After(3 * time.Second)
	for {
		if hasLogContaining(logs, "shortfall") {
			break
		}
		select {
		case <-deadline:
			t.Fatal("expected a shortfall WARN log")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestReplicator_ConcurrencyCap(t *testing.T) {
	tr := newFakeTransport()
	tr.gate = make(chan struct{}) // block every push until released
	prov := &fakeProvider{
		dc: "dc-holder",
		cands: []Candidate{
			{NodeID: "target", Datacenter: "dc-a", DiskMBFree: 9000, Gravity: 1, Active: true},
		},
	}
	const cap = 2
	r := NewReplicator(
		WithReplicationCandidateProvider(prov),
		WithReplicationIndex(&fakeIndex{}),
		WithReplicationTransport(tr),
		WithReplicationFactor(1),
		WithReplicationConcurrency(cap),
		WithReplicationPrePushJitter(0),
		WithReplicationRetries(0),
	)
	defer func() {
		close(tr.gate) // release blocked pushes so workers can exit
		r.Stop()
	}()

	for i := 0; i < 8; i++ {
		r.Replicate("cap"+string(rune('a'+i)), "v1", "chk", 1)
	}

	// Wait until the cap is saturated.
	deadline := time.After(3 * time.Second)
	for tr.currentInflight() < cap {
		select {
		case <-deadline:
			t.Fatalf("never reached %d concurrent pushes (got %d)", cap, tr.currentInflight())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Hold a moment and assert it never exceeds the cap.
	time.Sleep(150 * time.Millisecond)
	if peak := tr.peakInflight(); peak > cap {
		t.Errorf("concurrency cap violated: peak inflight %d > cap %d", peak, cap)
	}
}

func TestReplicator_OnReplicaReceived_PinsAndRebroadcasts(t *testing.T) {
	store, baseDir := tempStore(t)
	_ = baseDir
	now := time.Now()
	store.Put(Record{CapsuleID: "cap1", Tag: "v1", Path: "/x", Checksum: "chk", Size: 99, CreatedAt: now, LastAccessed: now, TTL: 72 * time.Hour})

	bc := &fakeBroadcaster{}
	r := &Replicator{store: store, broadcaster: bc, logger: zap.NewNop()}

	r.onReplicaReceived("cap1", "v1", "chk", 99)

	got, _ := store.Get("cap1", "v1")
	if got == nil || !got.Pinned {
		t.Fatalf("standby copy should be pinned, got %+v", got)
	}
	if bc.count() != 1 {
		t.Errorf("receiver should re-broadcast availability exactly once, got %d", bc.count())
	}
}

// TestReplicator_EndToEnd_RealHosts is the transport-level O11 proof: a
// snapshot captured on host A is pushed to host B, which pulls the bytes
// (reusing TransferServer + PullSnapshot), pins them as a standby, and
// re-broadcasts. After this, B holds the snapshot LOCALLY — so a
// re-election winner on B restores without a cold start.
func TestReplicator_EndToEnd_RealHosts(t *testing.T) {
	hostA := newTestHost(t)
	hostB := newTestHost(t)
	connectHosts(t, hostA, hostB)

	// Holder A: a real snapshot on disk + a transfer server.
	storeA, _ := tempStore(t)
	if err := storeA.EnsureDir("cap1", "v1"); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storeA.SnapshotPath("cap1", "v1"), "checkpoint.tar"),
		[]byte("fake-criu-archive-contents"), 0600); err != nil {
		t.Fatalf("write snapshot file: %v", err)
	}
	now := time.Now()
	storeA.Put(Record{CapsuleID: "cap1", Tag: "v1", Path: storeA.SnapshotPath("cap1", "v1"),
		Checksum: "ignored-by-transfer", Size: 26, CreatedAt: now, LastAccessed: now, TTL: 72 * time.Hour})
	NewTransferServer(hostA, storeA)

	// Receiver B: a replicator (registers the replicate handler + pulls).
	storeB, _ := tempStore(t)
	bcB := &fakeBroadcaster{}
	repB := NewReplicator(
		WithReplicationHost(hostB),
		WithReplicationStore(storeB),
		WithReplicationBroadcaster(bcB),
	)
	defer repB.Stop()

	// Holder A: a replicator that pushes to B.
	bProvider := &fakeProvider{
		dc: "dc-a",
		cands: []Candidate{
			{NodeID: hostB.ID().String(), PeerID: hostB.ID(), Datacenter: "dc-b",
				DiskMBFree: 9000, Gravity: 1, Active: true},
		},
	}
	repA := NewReplicator(
		WithReplicationHost(hostA),
		WithReplicationStore(storeA),
		WithReplicationIndex(&fakeIndex{}),
		WithReplicationCandidateProvider(bProvider),
		WithReplicationBroadcaster(&fakeBroadcaster{}),
		WithReplicationFactor(1),
		WithReplicationConcurrency(1),
		WithReplicationPrePushJitter(0),
		WithReplicationRetries(1),
	)
	defer repA.Stop()

	repA.Replicate("cap1", "v1", "ignored-by-transfer", 26)

	// B should end up with the snapshot locally, pinned.
	deadline := time.After(10 * time.Second)
	for {
		rec, _ := storeB.Get("cap1", "v1")
		if rec != nil {
			if !rec.Pinned {
				t.Errorf("replicated standby on B should be pinned")
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("B never received the replicated snapshot")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// B re-broadcast its new holdership.
	if bcB.count() == 0 {
		t.Error("receiver B should have re-broadcast availability")
	}

	// The on-disk bytes actually transferred.
	got, err := os.ReadFile(filepath.Join(storeB.SnapshotPath("cap1", "v1"), "checkpoint.tar"))
	if err != nil {
		t.Fatalf("read replicated file: %v", err)
	}
	if string(got) != "fake-criu-archive-contents" {
		t.Errorf("replicated file contents mismatch: %q", string(got))
	}
}

// --- helpers -------------------------------------------------------------

func newTestHost(t *testing.T) host.Host {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func connectHosts(t *testing.T, a, b host.Host) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Connect(ctx, peer.AddrInfo{ID: b.ID(), Addrs: b.Addrs()}); err != nil {
		t.Fatalf("connect: %v", err)
	}
}

func hasLogContaining(logs *observer.ObservedLogs, substr string) bool {
	for _, e := range logs.All() {
		if strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}
