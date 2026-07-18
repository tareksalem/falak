package election

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/election/gravity"
)

// --- O4 fakes: durable-binding tracker, recording lifecycle + sink -------
//
// These exercise the O4 change: the manager releases the local claim slot
// on a Won election only AFTER the winner binding is durable, so a sibling
// multi-replica round that re-decides on slot release observes the binding
// (self-anti-affinity) and steps aside. The tracker models the durable
// replica→node placement state; it is BOTH written by the lifecycle fake's
// WinElectionWithBinding AND read by gravity.IsEligible via the real
// CapsuleTargetLookup wired into the calculator — the same plumbing the
// production electionCapsuleLookup uses.

// bindingTracker records replica→node bindings keyed by capsule name and
// doubles as a gravity.CapsuleTargetLookup so the gravity eligibility check
// sees a node as "already running" a capsule the instant a binding is made.
type bindingTracker struct {
	mu       sync.Mutex
	byName   map[string]map[capsule.ReplicaID]string // capsule name -> replica -> node
	winCalls []winBinding
}

type winBinding struct {
	ID      capsule.CapsuleID
	Replica capsule.ReplicaID
	Node    string
}

func newBindingTracker() *bindingTracker {
	return &bindingTracker{byName: make(map[string]map[capsule.ReplicaID]string)}
}

// record persists a binding (the durable side-effect of AssignReplica).
func (b *bindingTracker) record(name string, id capsule.CapsuleID, replica capsule.ReplicaID, node string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.byName[name] == nil {
		b.byName[name] = make(map[capsule.ReplicaID]string)
	}
	b.byName[name][replica] = node
	b.winCalls = append(b.winCalls, winBinding{ID: id, Replica: replica, Node: node})
}

// unbind clears a binding, modelling O3's UnassignReplica on crash recovery.
func (b *bindingTracker) unbind(name string, replica capsule.ReplicaID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m := b.byName[name]; m != nil {
		delete(m, replica)
	}
}

// NodesRunningCapsule implements gravity.CapsuleTargetLookup.
func (b *bindingTracker) NodesRunningCapsule(_ string, capsuleName string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, node := range b.byName[capsuleName] {
		if node != "" {
			out = append(out, node)
		}
	}
	return out
}

func (b *bindingTracker) nodesFor(name string) []string {
	return b.NodesRunningCapsule("", name)
}

// winCount returns how many WinElectionWithBinding calls were recorded.
func (b *bindingTracker) winCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.winCalls)
}

// recordingLifecycle implements the widened LifecycleController. Its
// WinElectionWithBinding mirrors the production electionLifecycleAdapter:
// it applies the (modelled) FSM transition AND records the durable binding
// into the shared tracker before returning.
type recordingLifecycle struct {
	tracker *bindingTracker
	store   *fakeStore

	mu      sync.Mutex
	started int
	won     int
	timeout int
}

func (l *recordingLifecycle) StartElection(capsule.CapsuleID) error {
	l.mu.Lock()
	l.started++
	l.mu.Unlock()
	return nil
}

func (l *recordingLifecycle) WinElection(capsule.CapsuleID) error {
	l.mu.Lock()
	l.won++
	l.mu.Unlock()
	return nil
}

func (l *recordingLifecycle) ElectionTimeout(capsule.CapsuleID) error {
	l.mu.Lock()
	l.timeout++
	l.mu.Unlock()
	return nil
}

func (l *recordingLifecycle) WinElectionWithBinding(id capsule.CapsuleID, replicaID capsule.ReplicaID, nodeID string) error {
	// FSM transition (Electing → Assigned), then the durable binding —
	// same order as the real adapter.
	l.mu.Lock()
	l.won++
	l.mu.Unlock()
	name := string(id)
	if c := l.store.Get(id); c != nil {
		name = c.Spec.Name
	}
	l.tracker.record(name, id, replicaID, nodeID)
	return nil
}

// recordingSink counts outcome events so tests can assert on them without a
// real event bus.
type recordingSink struct {
	mu            sync.Mutex
	won           int
	lost          int
	failed        int
	yielded       int
	wonReplicas   []string
	yieldWinners  []string
}

func (s *recordingSink) EmitWon(req Request, _ string, _ float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.won++
	s.wonReplicas = append(s.wonReplicas, req.ReplicaID)
}

func (s *recordingSink) EmitLost(Request, string) {
	s.mu.Lock()
	s.lost++
	s.mu.Unlock()
}

func (s *recordingSink) EmitFailed(Request, string) {
	s.mu.Lock()
	s.failed++
	s.mu.Unlock()
}

func (s *recordingSink) EmitYielded(_ Request, winnerNodeID string) {
	s.mu.Lock()
	s.yielded++
	s.yieldWinners = append(s.yieldWinners, winnerNodeID)
	s.mu.Unlock()
}

func (s *recordingSink) wonCount() int     { s.mu.Lock(); defer s.mu.Unlock(); return s.won }
func (s *recordingSink) lostCount() int    { s.mu.Lock(); defer s.mu.Unlock(); return s.lost }
func (s *recordingSink) failedCount() int  { s.mu.Lock(); defer s.mu.Unlock(); return s.failed }
func (s *recordingSink) yieldedCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.yielded }
func (s *recordingSink) yieldWinnerList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.yieldWinners...)
}

// gravityTestStrategy is a minimal Strategy that drives the REAL gravity
// calculator (including the real self-anti-affinity check via the wired
// CapsuleTargetLookup) and publishes after a fixed delay. Using the real
// calculator — rather than a hand-rolled eligibility check — is what makes
// the multi-replica regression exercise the actual durable mechanism.
type gravityTestStrategy struct {
	publishDelay time.Duration
}

func (s *gravityTestStrategy) Name() string { return "gravity-test" }

func (s *gravityTestStrategy) Decide(
	_ context.Context,
	req Request,
	c *capsule.Capsule,
	calc *gravity.Calculator,
	provider gravity.StateProvider,
) Decision {
	state, err := provider.LocalNode(req.ClusterPath)
	if err != nil {
		return Decision{Eligible: false, Reason: err.Error()}
	}
	res := calc.Calculate(c, state)
	if !res.Eligible {
		return Decision{Eligible: false, Reason: string(res.Inelig.Reason)}
	}
	return Decision{
		Eligible:  true,
		Score:     float64(res.Score),
		PublishAt: time.Now().Add(s.publishDelay),
		Reason:    "gravity-test: eligible",
	}
}

// makeReplicaManager wires a single-node Manager ready for HandleRequest
// (single-replica election path) against a real in-process pubsub. The
// returned tracker-backed calculator is what enforces self-anti-affinity.
func makeReplicaManager(t *testing.T, tracker *bindingTracker, opts ...ManagerOption) (*Manager, *recordingLifecycle, *recordingSink, *fakeStore) {
	t.Helper()
	_, ps, nodeID := newTestPubSub(t)

	store := newFakeStore()
	sink := &recordingSink{}
	state := healthyNodeState(nodeID, 8, 16000)
	provider := &fakeProvider{state: state}
	calc := gravity.NewCalculator(gravity.WithCapsuleTargetLookup(tracker))
	lc := &recordingLifecycle{tracker: tracker, store: store}

	combined := []ManagerOption{
		WithNodeID(nodeID),
		WithCapsuleStore(store),
		WithLifecycleController(lc),
		WithCalculator(calc),
		WithStateProvider(provider),
		WithPubSub(ps),
		WithEventSink(sink),
		WithElectionTimeout(300 * time.Millisecond),
		WithTiebreakWindow(30 * time.Millisecond),
		// Short reconcile window so the post-hoc yield phase does not keep
		// the round goroutine alive for the production default (3s) — most
		// helper-driven tests assert on in-flight drain / re-election
		// timing. Tests that specifically exercise the reconcile/yield path
		// override this via opts.
		WithReconcileWindow(40 * time.Millisecond),
		WithPublishTimeout(1 * time.Second),
	}
	combined = append(combined, opts...)

	mgr := NewManager(&gravityTestStrategy{}, combined...)
	mgr.Start(context.Background())
	t.Cleanup(mgr.Stop)

	if err := mgr.JoinCluster(state.ClusterPath); err != nil {
		t.Fatalf("JoinCluster: %v", err)
	}
	return mgr, lc, sink, store
}

// makeReplicaCapsule builds a minimal capsule that fits comfortably on the
// helper's node (8 CPU / 16 GB free).
func makeReplicaCapsule(t *testing.T, name string) *capsule.Capsule {
	t.Helper()
	c := &capsule.Capsule{
		ID:        capsule.NewCapsuleID(),
		ClusterID: "test/dc1/c",
		Spec: capsule.CapsuleSpec{
			Name:      name,
			Image:     name + ":v1",
			Orbit:     "api",
			Resources: capsule.ResourceRequirements{CPUCores: 1, MemoryMB: 64},
		},
	}
	capsule.DefaultSpec(&c.Spec)
	return c
}

// inflightCount reads the manager's in-flight election count under its lock.
func inflightCount(m *Manager) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.inflight)
}

// --- O4 tests ------------------------------------------------------------

// TestReport_WonReleasesLocalClaimSlot is the focused unit assertion that a
// Won outcome (a) records the durable binding and (b) releases the local
// claim slot — and that the binding is present at the moment the slot frees.
// Driving report() directly avoids any pubsub timing so the ordering
// invariant is asserted deterministically.
func TestReport_WonReleasesLocalClaimSlot(t *testing.T) {
	tracker := newBindingTracker()
	store := newFakeStore()
	lc := &recordingLifecycle{tracker: tracker, store: store}
	sink := &recordingSink{}

	mgr := NewManager(&stubStrategy{name: "s"},
		WithNodeID("node-x"),
		WithLifecycleController(lc),
		WithEventSink(sink),
		WithCapsuleStore(store),
	)

	c := makeReplicaCapsule(t, "won-app")
	store.put(c)
	req := Request{CapsuleID: c.ID, ReplicaID: "0", ClusterPath: c.ClusterID}

	// The round holds the slot before it reports a win.
	if !mgr.tryClaimCapsule(c.ID) {
		t.Fatal("precondition: slot should be claimable on a fresh manager")
	}
	if !mgr.hasLocalClaim(c.ID) {
		t.Fatal("precondition: slot should be held before report")
	}

	mgr.report(req, OutcomeEnum.Won(), "node-x", 87.5, "")

	if mgr.hasLocalClaim(c.ID) {
		t.Fatal("report(Won) must release the local claim slot (O4)")
	}
	if got := tracker.nodesFor("won-app"); len(got) != 1 || got[0] != "node-x" {
		t.Fatalf("binding must be durable before the slot is released, got %v", got)
	}
	if tracker.winCount() != 1 {
		t.Fatalf("WinElectionWithBinding should have been invoked once, got %d", tracker.winCount())
	}
	if sink.wonCount() != 1 {
		t.Fatalf("expected exactly 1 Won emitted, got %d", sink.wonCount())
	}
}

// TestReElection_SameCapsuleSameNode_AfterWin reproduces the single-node
// crash-recovery scenario at the election-manager level: a node wins replica
// 0, the slot is released (O4) so it no longer dead-locks, the binding is
// cleared (O3's UnassignReplica), and a re-election for the SAME
// (capsule, replica) must win again.
func TestReElection_SameCapsuleSameNode_AfterWin(t *testing.T) {
	tracker := newBindingTracker()
	mgr, _, sink, store := makeReplicaManager(t, tracker)

	c := makeReplicaCapsule(t, "reelect-app")
	store.put(c)
	req := Request{
		CapsuleID:   c.ID,
		ReplicaID:   "0",
		ClusterPath: c.ClusterID,
		Reason:      ReasonEnum.NodeFailure(),
		CreatedAt:   time.Now(),
	}

	// Round 1: the node wins replica 0.
	if err := mgr.HandleRequest(req); err != nil {
		t.Fatalf("HandleRequest (round 1): %v", err)
	}
	waitFor(t, 3*time.Second, "first win not emitted", func() bool { return sink.wonCount() >= 1 })

	// O4: the slot must be released after the Win (this is what unblocks a
	// later re-election on the same node).
	waitFor(t, 1*time.Second, "slot not released after win", func() bool { return !mgr.hasLocalClaim(c.ID) })
	if got := tracker.nodesFor("reelect-app"); len(got) != 1 || got[0] != mgr.nodeID {
		t.Fatalf("after win, binding = %v, want [%s]", got, mgr.nodeID)
	}

	// The round must fully clear from in-flight so the re-fire is not deduped.
	waitFor(t, 2*time.Second, "in-flight not cleared", func() bool { return inflightCount(mgr) == 0 })

	// O3: clear the stale binding (UnassignReplica on crash recovery) so the
	// node is eligible to re-place the replica it just lost.
	tracker.unbind("reelect-app", "0")

	// Round 2: the SAME (capsule, replica) re-fires and must win again.
	winsBefore := sink.wonCount()
	if err := mgr.HandleRequest(req); err != nil {
		t.Fatalf("HandleRequest (round 2): %v", err)
	}
	waitFor(t, 3*time.Second, "re-election did not win again (deadlock)", func() bool {
		return sink.wonCount() > winsBefore
	})

	if got := tracker.nodesFor("reelect-app"); len(got) != 1 || got[0] != mgr.nodeID {
		t.Fatalf("after re-election, binding = %v, want [%s]", got, mgr.nodeID)
	}
	if tracker.winCount() < 2 {
		t.Fatalf("expected two winning bindings across the two rounds, got %d", tracker.winCount())
	}
	waitFor(t, 1*time.Second, "slot not released after re-win", func() bool { return !mgr.hasLocalClaim(c.ID) })
}

// TestMultiReplica_AntiAffinity_SameNodeNeverWinsBoth is the regression gate
// for the invariant the retained slot used to protect: a single best-fit
// node must not win EVERY replica of a multi-replica capsule. With O4 the
// slot is released on Win, so the guarantee now rests entirely on durable
// self-anti-affinity — the winner binding is persisted before the slot
// frees, so the sibling round re-decides against it and steps aside.
//
// Run with -count=20 to shake out any ordering race from moving the binding
// ahead of the release.
func TestMultiReplica_AntiAffinity_SameNodeNeverWinsBoth(t *testing.T) {
	tracker := newBindingTracker()
	mgr, _, sink, store := makeReplicaManager(t, tracker)

	c := makeReplicaCapsule(t, "multi-app")
	store.put(c)

	mkReq := func(replica string) Request {
		return Request{
			CapsuleID:   c.ID,
			ReplicaID:   replica,
			ClusterPath: c.ClusterID,
			Reason:      ReasonEnum.Initial(),
			CreatedAt:   time.Now(),
		}
	}

	// Fire both replica rounds in parallel on the SAME single node.
	if err := mgr.HandleRequest(mkReq("0")); err != nil {
		t.Fatalf("HandleRequest replica 0: %v", err)
	}
	if err := mgr.HandleRequest(mkReq("1")); err != nil {
		t.Fatalf("HandleRequest replica 1: %v", err)
	}

	// One round wins; the other (single node, no remote claim) resolves to
	// Failed/Lost after re-deciding ineligible.
	waitFor(t, 3*time.Second, "rounds did not resolve to one win + one step-aside", func() bool {
		return sink.wonCount() == 1 && (sink.failedCount()+sink.lostCount()) >= 1
	})

	// The decisive assertion: the same node did NOT win both replicas.
	if got := tracker.winCount(); got != 1 {
		t.Fatalf("same node won %d replicas; durable self-anti-affinity must cap it at 1", got)
	}
	if w := sink.wonCount(); w != 1 {
		t.Fatalf("expected exactly 1 Won, got %d (replicas=%v)", w, sink.wonReplicas)
	}
	if got := tracker.nodesFor("multi-app"); len(got) != 1 || got[0] != mgr.nodeID {
		t.Fatalf("winner binding = %v, want exactly [%s]", got, mgr.nodeID)
	}
}
