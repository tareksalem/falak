package sync

import (
	"testing"
	"time"
)

func TestMockClock_AfterFiresOnAdvance(t *testing.T) {
	start := time.Unix(0, 0)
	c := newMockClock(start)

	ch := c.After(1 * time.Second)

	// Not yet due.
	select {
	case <-ch:
		t.Fatal("After fired before the deadline")
	default:
	}

	// Advance past the deadline.
	c.Advance(1 * time.Second)

	select {
	case got := <-ch:
		if !got.Equal(start.Add(1 * time.Second)) {
			t.Fatalf("expected fire at %v, got %v", start.Add(time.Second), got)
		}
	case <-time.After(time.Second):
		t.Fatal("After did not fire after Advance past deadline")
	}
}

func TestMockClock_ZeroDurationFiresImmediately(t *testing.T) {
	c := newMockClock(time.Unix(0, 0))
	ch := c.After(0)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("zero-duration After did not fire immediately")
	}
}

func TestMockClock_MultipleWaitersFireInOrder(t *testing.T) {
	start := time.Unix(0, 0)
	c := newMockClock(start)

	a := c.After(1 * time.Second)
	b := c.After(2 * time.Second)

	c.Advance(1 * time.Second)
	select {
	case <-a:
	case <-time.After(time.Second):
		t.Fatal("first waiter did not fire")
	}
	select {
	case <-b:
		t.Fatal("second waiter fired too early")
	default:
	}

	c.Advance(1 * time.Second)
	select {
	case <-b:
	case <-time.After(time.Second):
		t.Fatal("second waiter did not fire after second advance")
	}
}

func TestNextBurstInterval_Bounds(t *testing.T) {
	s := New(
		WithBurstInterval(1*time.Second),
		WithBurstJitter(250*time.Millisecond),
	)
	min := 1*time.Second - 250*time.Millisecond
	max := 1*time.Second + 250*time.Millisecond
	for i := 0; i < 1000; i++ {
		got := s.nextBurstInterval()
		if got < min || got > max {
			t.Fatalf("burst interval %v out of bounds [%v,%v]", got, min, max)
		}
	}
}

func TestNextBurstInterval_ZeroJitterIsExact(t *testing.T) {
	s := New(
		WithBurstInterval(1*time.Second),
		WithBurstJitter(0),
	)
	if got := s.nextBurstInterval(); got != time.Second {
		t.Fatalf("expected exact 1s with zero jitter, got %v", got)
	}
}
