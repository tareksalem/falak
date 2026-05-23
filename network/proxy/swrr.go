// Package proxy implements Falak's per-node L4 data-plane proxy. One
// Proxy instance per node listens on every per-group bridge gateway IP
// at every Service-exposed port and forwards TCP and UDP traffic to a
// SWRR-selected backend, then to an outlier-filtered replica.
//
// This file implements the smooth weighted round-robin (SWRR) backend
// selector used by the proxy (plan 11B.11). The algorithm is the
// canonical nginx variant — on every Pick, each entry's current_weight
// grows by its base weight; the entry with the largest current_weight
// is picked and its current_weight reduced by the total weight. This
// produces a maximally smooth distribution: consecutive picks across
// backends with weights 5/1 yield A A A A B A A A A A rather than
// A A A A A A A A A B (which the naive Boltzmann variant produces).
//
// Cold-start randomization (decision #34): a fresh SWRR with weights
// 90/10 picks the 90-weight backend nine times in a row before the
// 10-weight backend gets a turn. That artifact is technically correct
// over long horizons but operationally confusing — a "canary at 10%"
// receives zero traffic for the first nine connections. Initializing
// each entry's current_weight to a random value in [0, total_weight)
// fixes this without breaking long-horizon distribution: the algorithm
// is steady-state invariant under any starting offset.
//
// Hot-swap discipline: Update preserves existing entries' current_weight
// so a weight change does not reset the rotation cursor. New entries
// receive the same random offset treatment as cold-start; removed
// entries are dropped.
package proxy

import (
	"math/rand"
	"sort"
	"sync"
)

// swrrEntry is one backend slot in the SWRR state machine.
type swrrEntry struct {
	// Name is the backend's logical identifier (ServiceBackend.Capsule).
	Name string
	// Weight is the configured base weight. Updated on hot-swap.
	Weight int32
	// CurrentWeight is the per-pick running counter. Grows by Weight on
	// every Pick; the entry with the max is selected and reduced by the
	// total weight.
	CurrentWeight int32
}

// SWRR is the smooth weighted round-robin backend selector for one
// Service. Safe for concurrent use; Pick and Update may run from
// different goroutines.
type SWRR struct {
	rng *rand.Rand

	mu      sync.Mutex
	entries []swrrEntry
	total   int32
}

// NewSWRR constructs a SWRR with the given backend weights. Entries
// with weight ≤ 0 are excluded — they will never be picked and are not
// kept in internal state. rng must be non-nil; tests pass a seeded
// *rand.Rand for determinism.
func NewSWRR(weights map[string]int32, rng *rand.Rand) *SWRR {
	if rng == nil {
		// crypto/rand is wrong here; we want determinism in tests and
		// per-Service variation in production. Callers must supply rng.
		rng = rand.New(rand.NewSource(1))
	}
	s := &SWRR{rng: rng}
	s.replaceLocked(weights)
	return s
}

// Pick returns the next backend name. ok==false when no entry has a
// positive weight (empty SWRR or every backend at weight 0).
func (s *SWRR) Pick() (name string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total <= 0 || len(s.entries) == 0 {
		return "", false
	}
	bestIdx := -1
	var bestCW int32
	for i := range s.entries {
		s.entries[i].CurrentWeight += s.entries[i].Weight
		if bestIdx == -1 || s.entries[i].CurrentWeight > bestCW {
			bestIdx = i
			bestCW = s.entries[i].CurrentWeight
		}
	}
	if bestIdx == -1 {
		return "", false
	}
	s.entries[bestIdx].CurrentWeight -= s.total
	return s.entries[bestIdx].Name, true
}

// Update hot-swaps the weight map. Existing entries retain their
// CurrentWeight so the smoothing rotation is not reset; new entries
// get the random cold-start offset; entries no longer present are
// dropped. Weight ≤ 0 prunes the entry from the selector.
func (s *SWRR) Update(weights map[string]int32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Index existing entries for O(1) lookup.
	existing := make(map[string]int32, len(s.entries))
	for _, e := range s.entries {
		existing[e.Name] = e.CurrentWeight
	}

	var (
		newEntries []swrrEntry
		newTotal   int32
	)

	// Sort new names for stable ordering — test determinism + log
	// stability outweigh the trivial sort cost.
	names := make([]string, 0, len(weights))
	for n, w := range weights {
		if w > 0 {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	for _, n := range names {
		w := weights[n]
		newTotal += w
		entry := swrrEntry{Name: n, Weight: w}
		if cw, ok := existing[n]; ok {
			entry.CurrentWeight = cw
		}
		newEntries = append(newEntries, entry)
	}

	// Cold-start offset for entries that were not present before.
	if newTotal > 0 {
		for i := range newEntries {
			if _, retained := existing[newEntries[i].Name]; !retained {
				newEntries[i].CurrentWeight = s.rng.Int31n(newTotal)
			}
		}
	}

	s.entries = newEntries
	s.total = newTotal
}

// replaceLocked rebuilds the entry set from scratch, applying the
// cold-start offset to every entry. Called only from NewSWRR.
func (s *SWRR) replaceLocked(weights map[string]int32) {
	names := make([]string, 0, len(weights))
	for n, w := range weights {
		if w > 0 {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	var (
		newEntries []swrrEntry
		newTotal   int32
	)
	for _, n := range names {
		w := weights[n]
		newTotal += w
		newEntries = append(newEntries, swrrEntry{Name: n, Weight: w})
	}
	if newTotal > 0 {
		for i := range newEntries {
			newEntries[i].CurrentWeight = s.rng.Int31n(newTotal)
		}
	}
	s.entries = newEntries
	s.total = newTotal
}

// Weights returns a snapshot of the current configured weights.
func (s *SWRR) Weights() map[string]int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int32, len(s.entries))
	for _, e := range s.entries {
		out[e.Name] = e.Weight
	}
	return out
}
