package samplelog

import (
	"sync"
	"testing"
	"time"
)

// TestAllow_FirstCallAdmitted verifies the first Allow per key always
// returns true and records no suppressed events.
func TestAllow_FirstCallAdmitted(t *testing.T) {
	s := NewSampler(WithInterval(time.Second))
	if !s.Allow("k") {
		t.Fatal("first Allow must return true")
	}
	if got := s.Suppressed("k"); got != 0 {
		t.Fatalf("Suppressed: want 0, got %d", got)
	}
}

// TestAllow_InWindowSuppressed verifies that repeated Allow calls
// inside the interval window return false and bump the counter.
func TestAllow_InWindowSuppressed(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewSampler(
		WithInterval(time.Second),
		WithNow(func() time.Time { return now }),
	)
	if !s.Allow("k") {
		t.Fatal("first Allow must return true")
	}
	for i := 0; i < 5; i++ {
		if s.Allow("k") {
			t.Fatalf("Allow #%d in window must return false", i)
		}
	}
	if got := s.Suppressed("k"); got != 5 {
		t.Fatalf("Suppressed: want 5, got %d", got)
	}
}

// TestAllow_NextWindowReadmits verifies that after the interval
// elapses the next Allow returns true and the suppressed counter
// resets.
func TestAllow_NextWindowReadmits(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewSampler(
		WithInterval(100*time.Millisecond),
		WithNow(func() time.Time { return now }),
	)
	if !s.Allow("k") {
		t.Fatal("first Allow must return true")
	}
	for i := 0; i < 3; i++ {
		s.Allow("k")
	}
	if got := s.Suppressed("k"); got != 3 {
		t.Fatalf("Suppressed before window: want 3, got %d", got)
	}
	now = now.Add(200 * time.Millisecond)
	if !s.Allow("k") {
		t.Fatal("Allow after interval must return true")
	}
	if got := s.Suppressed("k"); got != 0 {
		t.Fatalf("Suppressed after readmit: want 0, got %d", got)
	}
}

// TestAllow_PerKeyIndependence verifies that two different keys keep
// independent counters and admission windows.
func TestAllow_PerKeyIndependence(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewSampler(
		WithInterval(time.Second),
		WithNow(func() time.Time { return now }),
	)
	s.Allow("a")
	s.Allow("a")
	s.Allow("a")
	if !s.Allow("b") {
		t.Fatal("first Allow for b must return true")
	}
	if got := s.Suppressed("a"); got != 2 {
		t.Fatalf("Suppressed(a): want 2, got %d", got)
	}
	if got := s.Suppressed("b"); got != 0 {
		t.Fatalf("Suppressed(b): want 0, got %d", got)
	}
}

// TestSuppressed_UnknownKey returns zero for keys never Allow'd.
func TestSuppressed_UnknownKey(t *testing.T) {
	s := NewSampler()
	if got := s.Suppressed("missing"); got != 0 {
		t.Fatalf("Suppressed of unknown key: want 0, got %d", got)
	}
}

// TestSampler_Concurrent runs many goroutines hammering Allow on a
// shared key and a per-goroutine key. The race detector covers the
// correctness check; the assertion is that admissions never exceed the
// number of windows the test traversed.
func TestSampler_Concurrent(t *testing.T) {
	s := NewSampler(WithInterval(time.Hour))
	var wg sync.WaitGroup
	const goroutines = 16
	const perGoroutine = 200
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				s.Allow("shared")
				if id == 0 && j == 0 {
					s.Allow("first-only")
				}
			}
		}(i)
	}
	wg.Wait()

	// At most one admission across all goroutines for "shared" since the
	// interval is enormous; remainder are suppressed.
	if got := s.Suppressed("shared"); got != int64(goroutines*perGoroutine-1) {
		t.Fatalf("Suppressed(shared): want %d, got %d",
			goroutines*perGoroutine-1, got)
	}
}

// TestNewSampler_DefaultsApplied verifies that NewSampler() without
// options uses DefaultInterval and time.Now.
func TestNewSampler_DefaultsApplied(t *testing.T) {
	s := NewSampler()
	if s.interval != DefaultInterval {
		t.Fatalf("default interval: want %v, got %v", DefaultInterval, s.interval)
	}
	if s.now == nil {
		t.Fatal("default now must be non-nil")
	}
}

// TestWithOptions_IgnoreInvalid verifies that WithInterval(<=0) and
// WithNow(nil) are silently ignored, keeping the defaults intact.
func TestWithOptions_IgnoreInvalid(t *testing.T) {
	s := NewSampler(WithInterval(0), WithNow(nil))
	if s.interval != DefaultInterval {
		t.Fatalf("interval should not change on zero: got %v", s.interval)
	}
	if s.now == nil {
		t.Fatal("now should not become nil")
	}
}
