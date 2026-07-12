package sync

import (
	stdsync "sync"
	"time"
)

// Clock abstracts time so the burst anti-entropy loop (Layer 2 of the O13
// join-convergence design) can be driven deterministically in tests. The
// production implementation delegates to the standard library; tests inject
// a mockClock and advance it manually so convergence assertions never depend
// on real wall-clock sleeps.
//
// Only the primitives the syncer actually needs are exposed: reading now and
// waiting for a duration to elapse. The burst loop uses a re-armable timer
// pattern (compute next fire, wait, fire, recompute) rather than a fixed
// ticker so per-tick jitter can vary each interval.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After returns a channel that receives the current time after at least
	// d has elapsed. Equivalent to time.After for the real clock.
	After(d time.Duration) <-chan time.Time
}

// realClock is the production Clock backed by the standard library.
type realClock struct{}

// newRealClock returns a Clock backed by the standard library time package.
func newRealClock() Clock { return realClock{} }

// Now returns the current wall-clock time.
func (realClock) Now() time.Time { return time.Now() }

// After returns time.After(d).
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// mockClock is a deterministic Clock for tests. Time only advances when a
// test calls Advance. Pending After waiters whose deadline is reached by an
// Advance fire in deadline order. It is safe for concurrent use.
type mockClock struct {
	mu      stdsync.Mutex
	now     time.Time
	waiters []*mockWaiter
}

type mockWaiter struct {
	deadline time.Time
	ch       chan time.Time
}

// newMockClock returns a mockClock initialised at the given start time.
func newMockClock(start time.Time) *mockClock {
	return &mockClock{now: start}
}

// Now returns the mock clock's current time.
func (m *mockClock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

// After registers a waiter that fires once the mock clock advances past the
// deadline. A non-positive duration fires immediately.
func (m *mockClock) After(d time.Duration) <-chan time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()

	ch := make(chan time.Time, 1)
	deadline := m.now.Add(d)
	if !deadline.After(m.now) {
		ch <- m.now
		return ch
	}
	m.waiters = append(m.waiters, &mockWaiter{deadline: deadline, ch: ch})
	return ch
}

// Advance moves the mock clock forward by d and fires every waiter whose
// deadline is now due. Waiters fire with the clock's post-advance time.
func (m *mockClock) Advance(d time.Duration) {
	m.mu.Lock()
	m.now = m.now.Add(d)
	now := m.now
	remaining := m.waiters[:0]
	var due []*mockWaiter
	for _, w := range m.waiters {
		if !w.deadline.After(now) {
			due = append(due, w)
		} else {
			remaining = append(remaining, w)
		}
	}
	m.waiters = remaining
	m.mu.Unlock()

	for _, w := range due {
		w.ch <- now
	}
}
