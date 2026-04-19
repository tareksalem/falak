package events

import (
	"context"
	"testing"
	"time"
)

type probeEvent struct {
	BaseEvent
	label string
}

func (e probeEvent) EventType() string { return "test.probe" }

func newTestBus(t *testing.T) Bus {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewBus(WithContext(ctx))
}

func TestBus_Subscribe_DefaultBuffer(t *testing.T) {
	b := newTestBus(t)
	defer b.Close()

	ch := b.Subscribe("test.probe")
	if cap(ch) != DefaultSubscriberBufferSize {
		t.Fatalf("expected default buffer %d, got %d",
			DefaultSubscriberBufferSize, cap(ch))
	}
}

func TestBus_Subscribe_CustomBuffer(t *testing.T) {
	b := newTestBus(t)
	defer b.Close()

	ch := b.Subscribe("test.probe", WithSubscriberBufferSize(5))
	if cap(ch) != 5 {
		t.Fatalf("expected custom buffer 5, got %d", cap(ch))
	}

	// Non-positive is ignored.
	ch0 := b.Subscribe("test.probe", WithSubscriberBufferSize(0))
	if cap(ch0) != DefaultSubscriberBufferSize {
		t.Fatalf("non-positive size should fall back to default, got %d", cap(ch0))
	}
}

func TestBus_Dropped_BumpsWhenSubscriberFull(t *testing.T) {
	b := newTestBus(t)
	defer b.Close()

	// Tiny buffer, never drained — so anything after the first fill is dropped.
	b.Subscribe("test.probe", WithSubscriberBufferSize(1))

	const overflow = 20
	for i := 0; i < overflow; i++ {
		b.Publish(probeEvent{BaseEvent: NewBaseEvent()})
	}

	// Give the dispatcher time to process.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.Dropped() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := b.Dropped()
	if got == 0 {
		t.Fatalf("expected non-zero drop count after %d publishes into buf=1, got 0", overflow)
	}
	// First event fits in the buffer; remaining overflow-1 are candidates
	// to be dropped. Allow for dispatcher scheduling variance.
	if got > uint64(overflow) {
		t.Fatalf("drop count %d exceeds published count %d", got, overflow)
	}
}

func TestBus_Dropped_ZeroWhenSubscriberHasRoom(t *testing.T) {
	b := newTestBus(t)
	defer b.Close()

	// Buffer large enough to hold every publish at once so fanout never
	// sees a full channel regardless of scheduler timing.
	const n = 50
	ch := b.Subscribe("test.probe", WithSubscriberBufferSize(n))

	for i := 0; i < n; i++ {
		b.Publish(probeEvent{BaseEvent: NewBaseEvent()})
	}

	// Drain everything to confirm delivery.
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("subscriber only received %d of %d events", i, n)
		}
	}

	if dropped := b.Dropped(); dropped != 0 {
		t.Fatalf("expected 0 drops with generous buffer, got %d", dropped)
	}
}
