package proxy

import (
	"math/rand"
	"testing"
)

// newTestSWRR constructs a SWRR with a deterministic seed for tests.
func newTestSWRR(t *testing.T, weights map[string]int32, seed int64) *SWRR {
	t.Helper()
	return NewSWRR(weights, rand.New(rand.NewSource(seed)))
}

func TestSWRR_Distribution_9010(t *testing.T) {
	s := newTestSWRR(t, map[string]int32{"a": 90, "b": 10}, 42)
	counts := map[string]int{}
	const N = 1000
	for i := 0; i < N; i++ {
		name, ok := s.Pick()
		if !ok {
			t.Fatalf("Pick() returned !ok on iteration %d", i)
		}
		counts[name]++
	}
	// 90/10 split over 1000 should yield ~900/100 ±5% — 5% absolute
	// tolerance covers the cold-start offset noise.
	if counts["a"] < 850 || counts["a"] > 950 {
		t.Fatalf("expected ~900 picks for a, got %d", counts["a"])
	}
	if counts["b"] < 50 || counts["b"] > 150 {
		t.Fatalf("expected ~100 picks for b, got %d", counts["b"])
	}
}

func TestSWRR_Distribution_333334(t *testing.T) {
	s := newTestSWRR(t, map[string]int32{"a": 33, "b": 33, "c": 34}, 7)
	counts := map[string]int{}
	const N = 1000
	for i := 0; i < N; i++ {
		name, _ := s.Pick()
		counts[name]++
	}
	for _, n := range []string{"a", "b", "c"} {
		// Fairness across three near-equal weights: each ~333 ±5%.
		if counts[n] < 300 || counts[n] > 360 {
			t.Fatalf("expected ~333 picks for %s, got %d", n, counts[n])
		}
	}
}

func TestSWRR_WeightZeroExcluded(t *testing.T) {
	s := newTestSWRR(t, map[string]int32{"a": 100, "b": 0, "c": 50}, 1)
	for i := 0; i < 200; i++ {
		name, ok := s.Pick()
		if !ok {
			t.Fatalf("Pick !ok at %d", i)
		}
		if name == "b" {
			t.Fatalf("weight-0 backend b should never be picked")
		}
	}
}

func TestSWRR_EmptyOrAllZero(t *testing.T) {
	s := newTestSWRR(t, map[string]int32{}, 1)
	if _, ok := s.Pick(); ok {
		t.Fatalf("empty SWRR should return !ok")
	}
	s2 := newTestSWRR(t, map[string]int32{"a": 0, "b": 0}, 1)
	if _, ok := s2.Pick(); ok {
		t.Fatalf("all-zero SWRR should return !ok")
	}
}

func TestSWRR_HotSwapPreservesRotation(t *testing.T) {
	s := newTestSWRR(t, map[string]int32{"a": 90, "b": 10}, 99)
	// Burn a few picks so the entries are mid-rotation.
	for i := 0; i < 5; i++ {
		_, _ = s.Pick()
	}
	// Snapshot CurrentWeight via internal access (test-only).
	s.mu.Lock()
	before := make(map[string]int32, len(s.entries))
	for _, e := range s.entries {
		before[e.Name] = e.CurrentWeight
	}
	s.mu.Unlock()

	// Hot-swap to same set + different weights — existing entries
	// retain CurrentWeight (rotation not reset).
	s.Update(map[string]int32{"a": 50, "b": 50})
	s.mu.Lock()
	after := make(map[string]int32, len(s.entries))
	for _, e := range s.entries {
		after[e.Name] = e.CurrentWeight
	}
	s.mu.Unlock()
	for n, cw := range before {
		if after[n] != cw {
			t.Fatalf("Update reset CurrentWeight for %s: before=%d after=%d", n, cw, after[n])
		}
	}
}

func TestSWRR_HotSwapAddsAndRemoves(t *testing.T) {
	s := newTestSWRR(t, map[string]int32{"a": 50, "b": 50}, 11)
	s.Update(map[string]int32{"b": 30, "c": 70})

	w := s.Weights()
	if _, ok := w["a"]; ok {
		t.Fatalf("removed backend a should be gone")
	}
	if w["b"] != 30 || w["c"] != 70 {
		t.Fatalf("Weights() mismatch after update: %v", w)
	}

	counts := map[string]int{}
	for i := 0; i < 500; i++ {
		name, _ := s.Pick()
		counts[name]++
	}
	if counts["a"] != 0 {
		t.Fatalf("removed backend a was picked %d times", counts["a"])
	}
	if counts["b"] < 130 || counts["b"] > 170 {
		t.Fatalf("expected ~150 picks for b, got %d", counts["b"])
	}
	if counts["c"] < 320 || counts["c"] > 380 {
		t.Fatalf("expected ~350 picks for c, got %d", counts["c"])
	}
}

// TestSWRR_ColdStartNoFirstBias verifies that across many fresh SWRR
// instances with a 90/10 split, the very first pick is not always
// the high-weight backend — the cold-start offset must scramble it.
func TestSWRR_ColdStartNoFirstBias(t *testing.T) {
	const trials = 200
	var firstWasA int
	for trial := 0; trial < trials; trial++ {
		// Distinct seed per trial; rand.NewSource(trial+1) covers a range
		// of starting offsets without fixing one path.
		s := newTestSWRR(t, map[string]int32{"a": 90, "b": 10}, int64(trial+1))
		name, ok := s.Pick()
		if !ok {
			t.Fatalf("trial %d: Pick !ok", trial)
		}
		if name == "a" {
			firstWasA++
		}
	}
	// Without cold-start randomization, firstWasA would equal trials.
	// With it, we expect roughly 90% × trials = ~180; require at least
	// one trial picked b first to prove the bias is broken.
	if firstWasA >= trials {
		t.Fatalf("first pick was always 'a' across %d trials — cold-start randomization broken", trials)
	}
	// Sanity: still mostly a, since 90/10 is heavily skewed.
	if firstWasA < trials*3/4 {
		t.Fatalf("first pick was 'a' only %d / %d times — distribution looks wrong", firstWasA, trials)
	}
}

func TestSWRR_NilRng(t *testing.T) {
	// NewSWRR must accept nil rng (falls back to a fixed seed) without
	// panicking. Behavior-wise it's still deterministic.
	s := NewSWRR(map[string]int32{"a": 1}, nil)
	if name, ok := s.Pick(); !ok || name != "a" {
		t.Fatalf("nil rng SWRR misbehaved: name=%q ok=%v", name, ok)
	}
}
