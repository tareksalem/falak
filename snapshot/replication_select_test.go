package snapshot

import "testing"

// seqIntn returns a deterministic intn that walks the given sequence,
// wrapping around. Out-of-range values are simply ignored by the caller's
// rejection sampling, so the sequence only needs to steer the first picks.
func seqIntn(seq []int) func(int) int {
	i := 0
	return func(n int) int {
		v := seq[i%len(seq)]
		i++
		if v >= n {
			return v % n
		}
		return v
	}
}

func cand(id, dc string, disk int64, gravity float64) Candidate {
	return Candidate{NodeID: id, Datacenter: dc, DiskMBFree: disk, Gravity: gravity, Active: true}
}

func TestSelectTargets_FailureDomainSpread(t *testing.T) {
	cands := []Candidate{
		cand("n1", "dc-a", 9000, 1),
		cand("n2", "dc-b", 9000, 1),
		cand("n3", "dc-c", 9000, 1),
	}
	// Holder is in dc-a; the first two targets must be in distinct domains
	// other than the holder's.
	out := selectTargets(cands, "dc-a", "", nil, 2048, 2, seqIntn([]int{0}))
	if len(out) < 2 {
		t.Fatalf("expected >=2 targets, got %d", len(out))
	}
	if out[0].Datacenter == "dc-a" || out[1].Datacenter == "dc-a" {
		t.Errorf("first two targets must avoid the holder's domain: %s, %s",
			out[0].Datacenter, out[1].Datacenter)
	}
	if out[0].Datacenter == out[1].Datacenter {
		t.Errorf("first two targets must be in distinct domains, both %s", out[0].Datacenter)
	}
	// The holder-domain candidate is ranked last.
	if out[len(out)-1].Datacenter != "dc-a" {
		t.Errorf("holder-domain candidate should be last, got %s", out[len(out)-1].Datacenter)
	}
}

func TestSelectTargets_DiskFilterSkipsLowDisk(t *testing.T) {
	cands := []Candidate{
		cand("low", "dc-a", 100, 100), // below threshold — must be skipped
		cand("ok", "dc-b", 5000, 1),   // eligible
	}
	out := selectTargets(cands, "dc-z", "", nil, 2048, 2, seqIntn([]int{0}))
	if len(out) != 1 {
		t.Fatalf("expected exactly 1 eligible target, got %d", len(out))
	}
	if out[0].NodeID != "ok" {
		t.Errorf("low-disk node should be skipped; got %s", out[0].NodeID)
	}
}

func TestSelectTargets_ExcludesHoldersAndInactive(t *testing.T) {
	cands := []Candidate{
		cand("eligible", "dc-a", 9000, 1),
		{NodeID: "inactive", Datacenter: "dc-b", DiskMBFree: 9000, Active: false},
		cand("already-holds", "dc-c", 9000, 1),
	}
	exclude := map[string]bool{"already-holds": true}
	out := selectTargets(cands, "dc-z", "", exclude, 2048, 2, seqIntn([]int{0}))
	if len(out) != 1 || out[0].NodeID != "eligible" {
		t.Fatalf("expected only 'eligible'; got %+v", out)
	}
}

func TestSelectTargets_PowerOfTwoNotAlwaysBest(t *testing.T) {
	// One domain, three members. The global gravity argmax is n1 (100).
	// With sampleSize=2 and a sample of {n0, n2}, the best-of-sample is n0
	// (10), NOT the global best — proving selection is power-of-two, not a
	// global argmax that would herd every replica onto the beefiest node.
	cands := []Candidate{
		cand("n0", "dc-a", 9000, 10),
		cand("n1", "dc-a", 9000, 100),
		cand("n2", "dc-a", 9000, 5),
	}
	out := selectTargets(cands, "dc-holder", "", nil, 2048, 2, seqIntn([]int{0, 2}))
	if len(out) == 0 {
		t.Fatal("expected at least one target")
	}
	if out[0].NodeID != "n0" {
		t.Errorf("power-of-two should pick the better of the sample {n0,n2} = n0, got %s", out[0].NodeID)
	}
}

func TestSelectTargets_NoEligible(t *testing.T) {
	cands := []Candidate{
		{NodeID: "x", Datacenter: "dc-a", DiskMBFree: 10, Active: true},
	}
	out := selectTargets(cands, "dc-z", "", nil, 2048, 2, seqIntn([]int{0}))
	if len(out) != 0 {
		t.Errorf("expected no eligible targets, got %d", len(out))
	}
}
