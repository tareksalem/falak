package snapshot

import "testing"

func TestJobQueue_PriorityOrder(t *testing.T) {
	q := newJobQueue(10)
	// Enqueue in mixed priority order.
	q.push(replicationJob{capsuleID: "low", priority: 1, seq: 1})
	q.push(replicationJob{capsuleID: "high", priority: 3, seq: 2})
	q.push(replicationJob{capsuleID: "mid", priority: 2, seq: 3})
	q.push(replicationJob{capsuleID: "high2", priority: 3, seq: 4})

	want := []string{"high", "high2", "mid", "low"}
	for i, w := range want {
		job, ok := q.pop()
		if !ok {
			t.Fatalf("pop %d: queue empty", i)
		}
		if job.capsuleID != w {
			t.Errorf("pop %d: got %s, want %s", i, job.capsuleID, w)
		}
	}
}

func TestJobQueue_OverflowDropsLeastUrgent(t *testing.T) {
	q := newJobQueue(2)
	a, _, _ := q.push(replicationJob{capsuleID: "a", priority: 1, seq: 1})
	b, _, _ := q.push(replicationJob{capsuleID: "b", priority: 1, seq: 2})
	if !a || !b {
		t.Fatal("first two pushes should be accepted")
	}

	// Queue full with two priority-1 jobs. A priority-3 job must evict the
	// least-urgent (newest priority-1 == "b").
	accepted, dropped, droppedOK := q.push(replicationJob{capsuleID: "c", priority: 3, seq: 3})
	if !accepted {
		t.Fatal("higher-priority job should be accepted on overflow")
	}
	if !droppedOK || dropped == nil || dropped.capsuleID != "b" {
		t.Fatalf("expected to drop least-urgent 'b', got %+v", dropped)
	}

	// A lower-or-equal-priority job must be rejected when full.
	accepted, _, _ = q.push(replicationJob{capsuleID: "d", priority: 1, seq: 4})
	if accepted {
		t.Error("equal/lower-priority job should be rejected when queue is full")
	}

	// Drain: should be c (3) then a (1).
	j1, _ := q.pop()
	j2, _ := q.pop()
	if j1.capsuleID != "c" || j2.capsuleID != "a" {
		t.Errorf("drain order = %s,%s; want c,a", j1.capsuleID, j2.capsuleID)
	}
}

func TestJobQueue_CloseUnblocksPop(t *testing.T) {
	q := newJobQueue(4)
	done := make(chan struct{})
	go func() {
		_, ok := q.pop() // blocks until close
		if ok {
			t.Errorf("pop should report closed (ok=false)")
		}
		close(done)
	}()
	q.close()
	<-done
}
