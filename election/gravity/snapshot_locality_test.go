package gravity

import (
	"math"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
)

// fixedSnapshotLookup is a SnapshotLookup that reports a single held
// snapshot (or none) with a controllable age and TTL. It lets the
// snapshot-locality tests drive the factor deterministically without a real
// snapshot store or wall clock — the age is supplied directly.
type fixedSnapshotLookup struct {
	present bool
	age     time.Duration
	ttl     time.Duration
}

func (l fixedSnapshotLookup) LocalSnapshot(_ /*capsuleID*/, _ /*tag*/ string) (SnapshotInfo, bool) {
	if !l.present {
		return SnapshotInfo{}, false
	}
	return SnapshotInfo{Age: l.age, TTL: l.ttl}, true
}

// snapValue runs a calculation and returns the unweighted snapshot_locality
// factor value plus whether the factor applied at all.
func snapValue(t *testing.T, calc *Calculator, c *capsule.Capsule, node NodeState) (float64, bool) {
	t.Helper()
	res := calc.Calculate(c, node)
	v, ok := res.Factors["snapshot_locality"]
	return v, ok
}

// TestSnapshotLocality_HolderOutscoresNonHolder proves the O10 wiring: a
// healthy node that HOLDS a fresh snapshot scores higher than an identical
// healthy node WITHOUT it. The two views differ only in whether a snapshot
// lookup is wired (the holder's), so the gap is exactly the locality bonus.
func TestSnapshotLocality_HolderOutscoresNonHolder(t *testing.T) {
	c := minimalCapsule("locality")
	// Moderately loaded node so the baseline normalized score sits below the
	// snapshot factor's value (1.0); only then can adding a 1.0-valued factor
	// raise the score. On a perfectly empty node the score is already 100 and
	// the bonus is invisible.
	node := scenarioNode("n", 0.5, 2, 1.0, 1.0)

	holderCalc := NewCalculator(WithSnapshotLookup(fixedSnapshotLookup{
		present: true, age: 0, ttl: 72 * time.Hour,
	}))
	nonHolderCalc := NewCalculator() // no snapshot lookup → factor skipped

	holder := scoreOf(t, holderCalc, c, node)
	nonHolder := scoreOf(t, nonHolderCalc, c, node)

	if !(holder > nonHolder) {
		t.Fatalf("snapshot holder must outscore non-holder, got holder=%.2f non-holder=%.2f",
			holder, nonHolder)
	}
}

// TestSnapshotLocality_DegradedHolderLosesToHealthyEmpty is the architect's
// hard requirement: a node holding a FRESH snapshot but with degraded
// connection reliability LOSES to a healthy empty non-holder. This proves
// the Step-2 health weight (Reliability 0.8) outweighs the SnapshotLocality
// bonus (0.6) — locality must never rescue a sick node.
func TestSnapshotLocality_DegradedHolderLosesToHealthyEmpty(t *testing.T) {
	c := minimalCapsule("locality")

	// Holder: full resources, empty, holds a fresh snapshot, but reliability
	// is degraded (0.3). The snapshot bonus tries to rescue it.
	holderCalc := NewCalculator(WithSnapshotLookup(fixedSnapshotLookup{
		present: true, age: 0, ttl: 72 * time.Hour,
	}))
	holderNode := scenarioNode("holder", 1.0, 0, 0.3 /*rel*/, 1.0 /*exec*/)

	// Healthy empty non-holder: no snapshot lookup wired.
	emptyCalc := NewCalculator()
	emptyNode := scenarioNode("empty", 1.0, 0, 1.0, 1.0)

	holder := scoreOf(t, holderCalc, c, holderNode)
	empty := scoreOf(t, emptyCalc, c, emptyNode)

	if !(empty > holder) {
		t.Fatalf("healthy empty non-holder must beat degraded snapshot holder, got empty=%.2f holder=%.2f",
			empty, holder)
	}
}

// TestSnapshotLocality_AgeDecay asserts the locality bonus decays with the
// snapshot's age: ~full when fresh, ~0 near the TTL, and the exact linear
// midpoint at half the TTL. Ages are supplied directly by the stub lookup,
// so the test is deterministic with no real clock.
func TestSnapshotLocality_AgeDecay(t *testing.T) {
	c := minimalCapsule("decay")
	node := scenarioNode("n", 0.5, 0, 1.0, 1.0)
	const ttl = 72 * time.Hour

	cases := []struct {
		name string
		age  time.Duration
		want float64
	}{
		{"fresh", 0, 1.0},
		{"quarter_ttl", ttl / 4, 0.75},
		{"half_ttl", ttl / 2, 0.5},
		{"near_ttl", 71 * time.Hour, 1.0 - 71.0/72.0},
		{"at_ttl", ttl, 0.0},
		{"past_ttl", ttl + time.Hour, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calc := NewCalculator(WithSnapshotLookup(fixedSnapshotLookup{
				present: true, age: tc.age, ttl: ttl,
			}))
			got, ok := snapValue(t, calc, c, node)
			if !ok {
				t.Fatalf("snapshot_locality factor did not apply")
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("decayed bonus at age %s: got %.6f want %.6f", tc.age, got, tc.want)
			}
		})
	}
}

// TestSnapshotLocality_DecayExponent verifies the configurable decay shape:
// an exponent of 2 squares the remaining-life fraction, so at half-TTL the
// bonus is 0.25 instead of the linear 0.5.
func TestSnapshotLocality_DecayExponent(t *testing.T) {
	c := minimalCapsule("decay")
	node := scenarioNode("n", 0.5, 0, 1.0, 1.0)
	const ttl = 72 * time.Hour

	calc := NewCalculator(
		WithSnapshotLookup(fixedSnapshotLookup{present: true, age: ttl / 2, ttl: ttl}),
		WithSnapshotDecayExponent(2),
	)
	got, ok := snapValue(t, calc, c, node)
	if !ok {
		t.Fatalf("snapshot_locality factor did not apply")
	}
	if math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("exponent=2 at half-TTL: got %.6f want 0.25", got)
	}
}

// TestSnapshotLocality_FallbackHorizon verifies that when a snapshot record
// carries no TTL (TTL == 0), the calculator's configured fallback horizon is
// used as the decay horizon instead.
func TestSnapshotLocality_FallbackHorizon(t *testing.T) {
	c := minimalCapsule("decay")
	node := scenarioNode("n", 0.5, 0, 1.0, 1.0)

	// No TTL on the record; fallback horizon overridden to 10h. At age 5h the
	// linear decay is 1 - 5/10 = 0.5.
	calc := NewCalculator(
		WithSnapshotLookup(fixedSnapshotLookup{present: true, age: 5 * time.Hour, ttl: 0}),
		WithSnapshotDecayHorizon(10*time.Hour),
	)
	got, ok := snapValue(t, calc, c, node)
	if !ok {
		t.Fatalf("snapshot_locality factor did not apply")
	}
	if math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("fallback horizon decay: got %.6f want 0.5", got)
	}
}

// TestSnapshotLocality_DefaultFallbackHorizon checks the default fallback
// horizon (72h) is used when no override and no record TTL are present.
func TestSnapshotLocality_DefaultFallbackHorizon(t *testing.T) {
	c := minimalCapsule("decay")
	node := scenarioNode("n", 0.5, 0, 1.0, 1.0)

	calc := NewCalculator(WithSnapshotLookup(fixedSnapshotLookup{
		present: true, age: 36 * time.Hour, ttl: 0,
	}))
	got, ok := snapValue(t, calc, c, node)
	if !ok {
		t.Fatalf("snapshot_locality factor did not apply")
	}
	// 36h of the default 72h horizon → 0.5.
	if math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("default fallback horizon decay: got %.6f want 0.5", got)
	}
}

// TestSnapshotLocality_NilLookupNotApplicable proves back-compat: a
// calculator built without a snapshot lookup does not include the
// snapshot_locality factor at all (it returns notApplicable), so other
// calculators are unaffected by the O10 change.
func TestSnapshotLocality_NilLookupNotApplicable(t *testing.T) {
	c := minimalCapsule("nolocality")
	node := scenarioNode("n", 0.5, 0, 1.0, 1.0)

	calc := NewCalculator() // no WithSnapshotLookup
	if _, ok := snapValue(t, calc, c, node); ok {
		t.Fatalf("snapshot_locality factor must not apply when no lookup is wired")
	}
}

// TestSnapshotLocality_AbsentSnapshotScoresZero verifies that when a lookup
// IS wired but the node holds no snapshot for the pair, the factor applies
// with a value of 0 (participates in the denominator, contributes nothing).
func TestSnapshotLocality_AbsentSnapshotScoresZero(t *testing.T) {
	c := minimalCapsule("absent")
	node := scenarioNode("n", 0.5, 0, 1.0, 1.0)

	calc := NewCalculator(WithSnapshotLookup(fixedSnapshotLookup{present: false}))
	got, ok := snapValue(t, calc, c, node)
	if !ok {
		t.Fatalf("snapshot_locality factor must apply (value 0) when a lookup is wired")
	}
	if got != 0 {
		t.Fatalf("absent snapshot must score 0, got %.6f", got)
	}
}
