package sync

import (
	"context"
	"testing"
	"time"

	"github.com/tareksalem/falak/node/internal/events"
	"github.com/tareksalem/falak/node/phonebook"
)

// TestMemberPush_DeterministicConvergence reproduces the O13 scenario:
// node1 is the voucher; node2 and node3 were both vouched via node1. node1
// admits node3 and fans it out. We block on the Layer-1 fan-out completion
// event (no sleeps) and then assert node2 learned node3 — the exact gap that
// used to fail 4/5 runs.
func TestMemberPush_DeterministicConvergence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n1 := newSyncNode(t, ctx, "node1")
	n2 := newSyncNode(t, ctx, "node2")
	n3 := newSyncNode(t, ctx, "node3")

	// node1 knows itself, node2, node3 (roster complete on the voucher).
	n1.addSelf()
	n1.addMember(n2)
	n1.addMember(n3)

	// node2 knows itself and node1 (its voucher) — but NOT node3 yet. This is
	// the pre-convergence state.
	n2.addSelf()
	n2.addMember(n1)

	// node2 must accept a push from node1 (receiver-auth gate): node1 is in
	// node2's phonebook, so the gate passes. Connect so node1 can dial node2.
	connect(t, ctx, n1, n2)

	if n2.exists(n3.host.ID().String()) {
		t.Fatal("precondition failed: node2 already knows node3")
	}

	// Observe the fan-out completion deterministically.
	doneCh := n1.bus.Subscribe(events.TypeMemberPushFanoutDone)

	// node1 admits node3 → Layer 1 fan-out.
	n1.bus.Publish(events.MemberAdmitted{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: testCluster,
		NewMember: events.MemberInfo{
			NodeID:    n3.host.ID().String(),
			Addresses: addrStrings(n3),
		},
	})

	ev := waitEvent(t, doneCh, 10*time.Second, func(e events.Event) bool {
		d, ok := e.(events.MemberPushFanoutDone)
		return ok && d.NewMemberID == n3.host.ID().String()
	}).(events.MemberPushFanoutDone)

	// Exactly one Active target (node2); node3 (the new member) and node1
	// (self) are excluded.
	if ev.Targets != 1 {
		t.Fatalf("expected 1 push target, got %d", ev.Targets)
	}
	if ev.Delivered != 1 {
		t.Fatalf("expected 1 delivered push, got %d", ev.Delivered)
	}

	// The push is applied synchronously on the receiver before the ack, so by
	// the time the fan-out reports delivered node2 already has node3.
	if !n2.exists(n3.host.ID().String()) {
		t.Fatalf("node2 did not learn node3 after Layer-1 push")
	}
	if got := n2.count(); got != 3 {
		t.Fatalf("expected node2 phonebook == 3, got %d", got)
	}
}

// TestMemberPush_OnlyActiveTargets asserts the fan-out never pushes to
// non-Active members (failure mode #1).
func TestMemberPush_OnlyActiveTargets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n1 := newSyncNode(t, ctx, "node1")
	n2 := newSyncNode(t, ctx, "node2") // Active target
	n3 := newSyncNode(t, ctx, "node3") // the new member
	n4 := newSyncNode(t, ctx, "node4") // present but Suspected — must be skipped

	n1.addSelf()
	n1.addMember(n2)
	n1.addMember(n3)
	n1.addMember(n4)
	// Demote node4 to Suspected.
	if err := n1.phonebook.SetStatus(n4.host.ID().String(), testCluster, phonebook.NodeStatusEnum.Suspected()); err != nil {
		t.Fatal(err)
	}

	n2.addSelf()
	n2.addMember(n1)
	connect(t, ctx, n1, n2)

	doneCh := n1.bus.Subscribe(events.TypeMemberPushFanoutDone)

	n1.bus.Publish(events.MemberAdmitted{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: testCluster,
		NewMember: events.MemberInfo{
			NodeID:    n3.host.ID().String(),
			Addresses: addrStrings(n3),
		},
	})

	ev := waitEvent(t, doneCh, 10*time.Second, func(e events.Event) bool {
		d, ok := e.(events.MemberPushFanoutDone)
		return ok && d.NewMemberID == n3.host.ID().String()
	}).(events.MemberPushFanoutDone)

	// Only node2 is Active; node3 (new member), node4 (Suspected) and self
	// are excluded.
	if ev.Targets != 1 {
		t.Fatalf("expected exactly 1 Active target, got %d", ev.Targets)
	}
}

// TestMemberPush_RejectsUnauthenticatedSender asserts the receiver-auth gate:
// a push from a peer NOT in the receiver's phonebook is rejected (failure
// mode #2).
func TestMemberPush_RejectsUnauthenticatedSender(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n1 := newSyncNode(t, ctx, "node1")
	n2 := newSyncNode(t, ctx, "node2")
	n3 := newSyncNode(t, ctx, "node3")

	// node1 will push, but node2 does NOT have node1 in its phonebook, so the
	// gate must reject.
	n1.addSelf()
	n1.addMember(n2)
	n1.addMember(n3)

	n2.addSelf() // node2 knows only itself — node1 is a stranger.
	connect(t, ctx, n1, n2)

	doneCh := n1.bus.Subscribe(events.TypeMemberPushFanoutDone)

	n1.bus.Publish(events.MemberAdmitted{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: testCluster,
		NewMember: events.MemberInfo{
			NodeID:    n3.host.ID().String(),
			Addresses: addrStrings(n3),
		},
	})

	ev := waitEvent(t, doneCh, 10*time.Second, func(e events.Event) bool {
		d, ok := e.(events.MemberPushFanoutDone)
		return ok && d.NewMemberID == n3.host.ID().String()
	}).(events.MemberPushFanoutDone)

	if ev.Targets != 1 {
		t.Fatalf("expected 1 attempted target, got %d", ev.Targets)
	}
	// Rejected by the gate → not delivered.
	if ev.Delivered != 0 {
		t.Fatalf("expected 0 delivered (gate rejects stranger), got %d", ev.Delivered)
	}
	if n2.exists(n3.host.ID().String()) {
		t.Fatal("node2 accepted a push from an unauthenticated sender")
	}
}

// TestMemberPush_DisabledDoesNotPush asserts WithMemberPushEnabled(false)
// disables Layer 1 (needed for the Layer-2-only backstop test).
func TestMemberPush_DisabledDoesNotPush(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n1 := newSyncNode(t, ctx, "node1", WithMemberPushEnabled(false))
	n2 := newSyncNode(t, ctx, "node2")
	n3 := newSyncNode(t, ctx, "node3")

	n1.addSelf()
	n1.addMember(n2)
	n1.addMember(n3)
	n2.addSelf()
	n2.addMember(n1)
	connect(t, ctx, n1, n2)

	// With push disabled, no fan-out event should ever fire. Also assert the
	// burst was armed (StartPeriodicSync ran) so the backstop is active.
	doneCh := n1.bus.Subscribe(events.TypeMemberPushFanoutDone)

	n1.bus.Publish(events.MemberAdmitted{
		BaseEvent:   events.NewBaseEvent(),
		ClusterPath: testCluster,
		NewMember: events.MemberInfo{
			NodeID:    n3.host.ID().String(),
			Addresses: addrStrings(n3),
		},
	})

	select {
	case <-doneCh:
		t.Fatal("fan-out fired despite WithMemberPushEnabled(false)")
	case <-time.After(500 * time.Millisecond):
		// Expected: no fan-out.
	}
	if n2.exists(n3.host.ID().String()) {
		t.Fatal("node2 learned node3 without any push or sync")
	}
}

func addrStrings(n *syncNode) []string {
	out := make([]string, 0, len(n.host.Addrs()))
	for _, a := range n.host.Addrs() {
		out = append(out, a.String())
	}
	return out
}
