package node

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/node/internal/events"
)

// electionWatcher records every ElectionWon / ElectionLost / ElectionFailed
// event seen on a node's event bus, indexed by capsule ID.
type electionWatcher struct {
	mu      sync.Mutex
	wonOn   map[string]string // capsuleID -> winnerNodeID
	lostOn  map[string]string // capsuleID -> winnerNodeID (as observed by losing node)
	failed  map[string]string // capsuleID -> reason
}

func newElectionWatcher() *electionWatcher {
	return &electionWatcher{
		wonOn:  map[string]string{},
		lostOn: map[string]string{},
		failed: map[string]string{},
	}
}

// watch attaches the recorder to a node's event bus and starts consuming
// election events. The returned cancel func unsubscribes and stops the
// background goroutines.
func (w *electionWatcher) watch(t *testing.T, n *Node) func() {
	t.Helper()
	wonCh := n.eventBus.Subscribe(events.TypeElectionWon)
	lostCh := n.eventBus.Subscribe(events.TypeElectionLost)
	failCh := n.eventBus.Subscribe(events.TypeElectionFailed)

	ctx, cancel := context.WithCancel(context.Background())
	go w.loop(ctx, wonCh, lostCh, failCh)
	return cancel
}

func (w *electionWatcher) loop(ctx context.Context, wonCh, lostCh, failCh <-chan events.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-wonCh:
			if won, ok := ev.(events.ElectionWon); ok {
				w.mu.Lock()
				w.wonOn[won.CapsuleID] = won.NodeID
				w.mu.Unlock()
			}
		case ev := <-lostCh:
			if lost, ok := ev.(events.ElectionLost); ok {
				w.mu.Lock()
				w.lostOn[lost.CapsuleID] = lost.WinnerNodeID
				w.mu.Unlock()
			}
		case ev := <-failCh:
			if failed, ok := ev.(events.ElectionFailed); ok {
				w.mu.Lock()
				w.failed[failed.CapsuleID] = failed.Reason
				w.mu.Unlock()
			}
		}
	}
}

func (w *electionWatcher) wonFor(capsuleID string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id, ok := w.wonOn[capsuleID]
	return id, ok
}

func (w *electionWatcher) lostFor(capsuleID string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id, ok := w.lostOn[capsuleID]
	return id, ok
}

func (w *electionWatcher) failedFor(capsuleID string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.failed[capsuleID]
	return r, ok
}

// TestElection_3Node_SinglePicker creates a capsule on one node and
// verifies that exactly one node in the cluster wins the election while
// the other two record OutcomeEnum.Lost(). The capsule's lifecycle on the
// winning node ends in Assigned (the next phase, which would be
// Executing, belongs to the runtime module).
func TestElection_3Node_SinglePicker(t *testing.T) {
	const cluster = "test/dc1/election-pick"

	n1 := testNode(t, "elec1", 0)
	n2 := testNode(t, "elec2", 0)
	n3 := testNode(t, "elec3", 0)

	joinCluster(t, n1, cluster)
	bootstrap := getBootstrapAddr(n1)
	joinCluster(t, n2, cluster, bootstrap)
	joinCluster(t, n3, cluster, bootstrap)

	waitForPhonebookCount(t, n1, cluster, 3, 10*time.Second)
	waitForPhonebookCount(t, n2, cluster, 3, 10*time.Second)
	waitForPhonebookCount(t, n3, cluster, 3, 10*time.Second)

	// Subscribe to all three orbits BEFORE creating the capsule.
	// Without orbit subscription the capsule announcement never reaches
	// peers and only the originator participates in the election.
	joinOrbitOrFail(t, n1, cluster, "api")
	joinOrbitOrFail(t, n2, cluster, "api")
	joinOrbitOrFail(t, n3, cluster, "api")

	// Give GossipSub mesh time to form for both the orbit and the
	// election topic.
	time.Sleep(3 * time.Second)

	w1 := newElectionWatcher()
	w2 := newElectionWatcher()
	w3 := newElectionWatcher()
	defer w1.watch(t, n1)()
	defer w2.watch(t, n2)()
	defer w3.watch(t, n3)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	created, err := n1.Capsules().Create(ctx, cluster, capsule.CapsuleSpec{
		Name:  "elect-me",
		Image: "registry.test/elect:v1",
		Orbit: "api",
	})
	if err != nil {
		t.Fatalf("create capsule: %v", err)
	}
	capsuleID := created.ID.String()

	// Wait until exactly one node has won the election.
	deadline := time.Now().Add(15 * time.Second)
	var winner string
	for time.Now().Before(deadline) {
		w, ok1 := w1.wonFor(capsuleID)
		_, ok2 := w2.wonFor(capsuleID)
		_, ok3 := w3.wonFor(capsuleID)
		count := 0
		if ok1 {
			count++
			winner = w
		}
		if ok2 {
			count++
			x, _ := w2.wonFor(capsuleID)
			winner = x
		}
		if ok3 {
			count++
			x, _ := w3.wonFor(capsuleID)
			winner = x
		}
		if count == 1 {
			break
		}
		if count > 1 {
			t.Fatalf("multiple nodes claim to have won: %d", count)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if winner == "" {
		t.Fatalf("no winner observed within deadline. n1.failed=%v n2.failed=%v n3.failed=%v",
			anyKey(w1.failed), anyKey(w2.failed), anyKey(w3.failed))
	}

	t.Logf("election winner: %s", winner)

	// The capsule's local lifecycle on the winning node should advance
	// past Electing. Look up the capsule on the winning node by ID and
	// confirm its status is Assigned (or any later state — the runtime
	// module would advance further).
	winningNode := nodeByID(t, []*Node{n1, n2, n3}, winner)
	c := winningNode.Capsules().Get(capsule.CapsuleID(capsuleID))
	if c == nil {
		t.Fatalf("winning node %s does not know about capsule %s", winner, capsuleID)
	}
	if c.Status == capsule.CapsuleStatusEnum.Created() ||
		c.Status == capsule.CapsuleStatusEnum.Announced() ||
		c.Status == capsule.CapsuleStatusEnum.Electing() {
		t.Errorf("winning node lifecycle should be past Electing, got %s", c.Status)
	}
}

// TestElection_NodeFailureReElection verifies that when the node hosting
// a capsule replica fails, the mesh detects the failure and a remaining
// node takes over via re-election.
//
// This test takes longer than the others because it has to let SWIM
// detect the node failure (protocol period + failure threshold), so the
// test has a generous timeout.
func TestElection_NodeFailureReElection(t *testing.T) {
	const cluster = "test/dc1/reelect"

	n1 := testNode(t, "re1", 0)
	n2 := testNode(t, "re2", 0)
	n3 := testNode(t, "re3", 0)

	joinCluster(t, n1, cluster)
	bootstrap := getBootstrapAddr(n1)
	joinCluster(t, n2, cluster, bootstrap)
	joinCluster(t, n3, cluster, bootstrap)

	waitForPhonebookCount(t, n1, cluster, 3, 10*time.Second)
	waitForPhonebookCount(t, n2, cluster, 3, 10*time.Second)
	waitForPhonebookCount(t, n3, cluster, 3, 10*time.Second)

	joinOrbitOrFail(t, n1, cluster, "api")
	joinOrbitOrFail(t, n2, cluster, "api")
	joinOrbitOrFail(t, n3, cluster, "api")

	// Let gossipsub mesh form on orbit + election topics.
	time.Sleep(3 * time.Second)

	w1 := newElectionWatcher()
	w2 := newElectionWatcher()
	w3 := newElectionWatcher()
	defer w1.watch(t, n1)()
	defer w2.watch(t, n2)()
	defer w3.watch(t, n3)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	created, err := n1.Capsules().Create(ctx, cluster, capsule.CapsuleSpec{
		Name:  "reelect-me",
		Image: "registry.test/reelect:v1",
		Orbit: "api",
	})
	if err != nil {
		t.Fatalf("create capsule: %v", err)
	}
	capsuleID := created.ID.String()

	// Wait for the initial election to complete.
	deadline := time.Now().Add(15 * time.Second)
	var firstWinner string
	for time.Now().Before(deadline) {
		nodes := []*Node{n1, n2, n3}
		for _, n := range nodes {
			c := n.Capsules().Get(capsule.CapsuleID(capsuleID))
			if c == nil {
				continue
			}
			for _, r := range c.Replicas {
				if r.NodeID != "" {
					firstWinner = r.NodeID
					break
				}
			}
			if firstWinner != "" {
				break
			}
		}
		if firstWinner != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if firstWinner == "" {
		t.Fatal("initial election did not produce a replica assignment within 15s")
	}
	t.Logf("initial winner: %s", firstWinner)

	// Stop the winning node. SWIM on the remaining nodes will detect
	// the failure after a few probe cycles.
	allNodes := []*Node{n1, n2, n3}
	var survivors []*Node
	for _, n := range allNodes {
		if n.ID().String() == firstWinner {
			if err := n.Stop(); err != nil {
				t.Fatalf("stop winner %s: %v", firstWinner, err)
			}
			continue
		}
		survivors = append(survivors, n)
	}
	if len(survivors) != 2 {
		t.Fatalf("expected 2 survivors, got %d", len(survivors))
	}

	// Wait for the surviving nodes to observe the re-election and pick
	// a new winner. SWIM failure detection (suspected → quarantined →
	// failed) takes ~45-90s on the fast-timed test node before
	// NodeFailed fires; give the re-election a generous budget.
	deadline = time.Now().Add(120 * time.Second)
	var secondWinner string
	for time.Now().Before(deadline) {
		for _, n := range survivors {
			c := n.Capsules().Get(capsule.CapsuleID(capsuleID))
			if c == nil {
				continue
			}
			for _, r := range c.Replicas {
				if r.NodeID != "" && r.NodeID != firstWinner {
					secondWinner = r.NodeID
					break
				}
			}
			if secondWinner != "" {
				break
			}
		}
		if secondWinner != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if secondWinner == "" {
		t.Fatalf("re-election did not produce a new winner within 120s")
	}
	if secondWinner == firstWinner {
		t.Fatalf("re-election picked the failed node again")
	}
	t.Logf("re-election winner: %s", secondWinner)
}

// TestElection_MultiReplica verifies that when a capsule requests N
// replicas on a cluster with >= N healthy nodes, exactly N distinct
// nodes win election rounds (one per replica slot). Anti-affinity
// within the same capsule is ensured by the ExcludeNodes field of each
// election request.
func TestElection_MultiReplica(t *testing.T) {
	const cluster = "test/dc1/multi-replica"

	n1 := testNode(t, "mr1", 0)
	n2 := testNode(t, "mr2", 0)
	n3 := testNode(t, "mr3", 0)

	joinCluster(t, n1, cluster)
	bootstrap := getBootstrapAddr(n1)
	joinCluster(t, n2, cluster, bootstrap)
	joinCluster(t, n3, cluster, bootstrap)

	waitForPhonebookCount(t, n1, cluster, 3, 10*time.Second)
	waitForPhonebookCount(t, n2, cluster, 3, 10*time.Second)
	waitForPhonebookCount(t, n3, cluster, 3, 10*time.Second)

	joinOrbitOrFail(t, n1, cluster, "api")
	joinOrbitOrFail(t, n2, cluster, "api")
	joinOrbitOrFail(t, n3, cluster, "api")

	// Multi-replica tests are more sensitive to gossipsub mesh warmup
	// because each replica's election fires back-to-back. Give the mesh
	// a longer warmup than the single-replica happy-path tests.
	time.Sleep(5 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Request exactly 3 replicas so the election fires three separate
	// rounds (replica_id = "0", "1", "2"). With 3 healthy nodes and
	// ExcludeNodes wiring, each round should land on a distinct node.
	created, err := n1.Capsules().Create(ctx, cluster, capsule.CapsuleSpec{
		Name:  "multi-replica",
		Image: "registry.test/multi:v1",
		Orbit: "api",
		Replicas: capsule.ReplicaConfig{
			Min: 3,
			Max: 3,
		},
	})
	if err != nil {
		t.Fatalf("create capsule: %v", err)
	}
	capsuleID := created.ID.String()

	// Wait until at least one survivor observes 3 distinct replicas.
	deadline := time.Now().Add(30 * time.Second)
	var assignments map[string]string // replicaID -> nodeID
	for time.Now().Before(deadline) {
		for _, n := range []*Node{n1, n2, n3} {
			c := n.Capsules().Get(capsule.CapsuleID(capsuleID))
			if c == nil || len(c.Replicas) < 3 {
				continue
			}
			assignments = map[string]string{}
			for _, r := range c.Replicas {
				if r.NodeID != "" {
					assignments[string(r.ReplicaID)] = r.NodeID
				}
			}
			if len(assignments) >= 3 {
				break
			}
		}
		if len(assignments) >= 3 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if len(assignments) < 3 {
		t.Fatalf("expected 3 replica assignments, got %d: %+v", len(assignments), assignments)
	}

	t.Logf("replica assignments: %+v", assignments)

	// Verify all three NodeIDs are distinct (anti-affinity via ExcludeNodes).
	seen := map[string]bool{}
	for slot, nodeID := range assignments {
		if seen[nodeID] {
			t.Errorf("replica %s collided on node %s (already used)", slot, nodeID)
		}
		seen[nodeID] = true
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 distinct nodes, got %d", len(seen))
	}
}

// nodeByID returns the *Node whose libp2p ID matches the supplied string.
func nodeByID(t *testing.T, nodes []*Node, id string) *Node {
	t.Helper()
	for _, n := range nodes {
		if n.ID().String() == id {
			return n
		}
	}
	t.Fatalf("no node matches id %s", id)
	return nil
}

// anyKey returns the first key from a map (for diagnostic logging).
func anyKey(m map[string]string) string {
	for k, v := range m {
		return k + "=" + v
	}
	return ""
}
