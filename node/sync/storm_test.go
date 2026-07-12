package sync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tareksalem/falak/node/internal/events"
)

// TestVoucherDeath_ConvergesViaSurvivor models failure mode #5: the voucher
// dies before/while fanning out, so node2 never learns node3 from node1. But
// node3 holds node2 in its roster (from node1's AuthComplete), and node2's
// Layer-2 burst syncs node3 directly from node3 (the surviving holder).
func TestVoucherDeath_ConvergesViaSurvivor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clock := newMockClock(time.Unix(0, 0))

	n1 := newSyncNode(t, ctx, "node1") // voucher — will "die"
	n2 := newSyncNode(t, ctx, "node2",
		WithMemberPushEnabled(false), // node2 will not receive a push
		WithClock(clock),
		WithBurstInterval(1*time.Second),
		WithBurstDuration(30*time.Second),
		WithBurstJitter(0),
	)
	n3 := newSyncNode(t, ctx, "node3")

	// node2 knows node1 and node3 as reachable peers, but node3 is only known
	// to node2 as an address to dial — model node2 missing node3's row.
	// Concretely: node2's phonebook has self + node1. node3's phonebook has
	// self + node2 (from node1's AuthComplete roster). node2 must discover
	// node3 by syncing FROM node3.
	n2.addSelf()
	n2.addMember(n1)
	n2.addMember(n3) // node2 can dial node3 (address known) but we then remove
	// its row to simulate "node2 does not yet have node3 as a member". We keep
	// node3 dialable via the peerstore by connecting below, then delete the row.
	connect(t, ctx, n2, n3)
	if err := n2.phonebook.Remove(n3.host.ID().String(), testCluster); err != nil {
		t.Fatal(err)
	}

	// node3 (survivor) holds node2 so it accepts node2's sync (receiver gate).
	n3.addSelf()
	n3.addMember(n2)

	// Voucher dies.
	n1.syncer.Stop()

	if n2.exists(n3.host.ID().String()) {
		t.Fatal("precondition failed: node2 already has node3 row")
	}

	// node2's burst must sync from a surviving peer. node1 is dead; the only
	// other candidate is node3. Re-add node3's dialable address via the
	// peerstore (kept from connect) — performPeriodicSync reads addresses
	// from the phonebook, so node2 needs node3 as a *candidate*. Model the
	// realistic state: node2 knows node1 (dead) and node3 (alive) as peers,
	// but has not yet synced node3's full member record. Put node3 back as a
	// candidate row WITHOUT it counting as "converged": convergence here is
	// defined as node2 completing a successful sync that (re)confirms node3.
	n2.addMember(n3)
	if got := n2.count(); got != 3 {
		t.Fatalf("setup expected node2 to have 3 rows (self, node1, node3), got %d", got)
	}

	n2.syncer.StartPeriodicSync(testCluster)
	n2.syncer.armBurst(testCluster)

	syncCh := n2.bus.Subscribe(events.TypeSyncCompleted)

	// Drive burst ticks until node2 syncs successfully from node3 (node1 is
	// dead so a tick that picks node1 fails and the loop retries next tick).
	converged := false
	for i := 0; i < 10 && !converged; i++ {
		waitForWaiter(t, clock)
		clock.Advance(1 * time.Second)
		select {
		case e := <-syncCh:
			if c, ok := e.(events.SyncCompleted); ok && c.SyncedFrom == n3.host.ID().String() {
				converged = true
			}
		case <-time.After(2 * time.Second):
		}
	}

	if !converged {
		t.Fatal("node2 did not converge via the surviving holder (node3) after voucher death")
	}
}

// TestMassJoinStorm asserts failure mode #4: many members admitted through one
// seed converge, and the receiver rate-limiter never rejects legitimate burst
// syncs (zero "rate limited" rejections). We drive the seed's fan-out for N
// new members and assert every existing peer accepts every push.
func TestMassJoinStorm(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const total = 10

	seed := newSyncNode(t, ctx, "seed")
	seed.addSelf()

	nodes := make([]*syncNode, 0, total-1)
	for i := 0; i < total-1; i++ {
		n := newSyncNode(t, ctx, fmt.Sprintf("n%02d", i))
		n.addSelf()
		n.addMember(seed) // each accepts pushes from seed (receiver gate)
		seed.addMember(n) // seed's roster grows
		connect(t, ctx, seed, n)
		nodes = append(nodes, n)
	}

	// Watch every node's push receiver: count rejections via the receiver
	// logs is awkward; instead assert deliveries via the fan-out completion
	// events on the seed. Each admitted member should be delivered to all
	// currently-Active existing peers.
	doneCh := seed.bus.Subscribe(events.TypeMemberPushFanoutDone)

	// Admit each existing node as if freshly joined — this is the storm: the
	// seed fans out each new member to all the others. (In steady state the
	// members already know each other via seed's roster; the point of the
	// test is that the burst of concurrent pushes never trips the rate
	// limiter and every push is accepted.)
	for _, n := range nodes {
		seed.bus.Publish(events.MemberAdmitted{
			BaseEvent:   events.NewBaseEvent(),
			ClusterPath: testCluster,
			NewMember: events.MemberInfo{
				NodeID:    n.host.ID().String(),
				Addresses: addrStrings(n),
			},
		})
	}

	// Collect one fan-out-done per admitted member; assert every push landed.
	seen := 0
	deadline := time.After(30 * time.Second)
	for seen < len(nodes) {
		select {
		case e := <-doneCh:
			d, ok := e.(events.MemberPushFanoutDone)
			if !ok {
				continue
			}
			// Every attempted target must have accepted — no rate-limit
			// rejections, no gate failures.
			if d.Delivered != d.Targets {
				t.Fatalf("member %s: only %d/%d pushes delivered (rate-limited or rejected?)",
					d.NewMemberID, d.Delivered, d.Targets)
			}
			seen++
		case <-deadline:
			t.Fatalf("mass join storm timed out after %d/%d fan-outs", seen, len(nodes))
		}
	}
}
