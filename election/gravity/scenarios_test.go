package gravity

import (
	"math"
	"testing"

	"github.com/tareksalem/falak/capsule"
)

// scenarioNode builds a NodeState with controllable free-capacity fraction,
// committed-capsule count, connection reliability and execution reliability.
// CPU/mem/disk totals are round numbers so free = frac*total stays exact for
// the fractions used by the acceptance matrix (1.0, 0.9, 0.5, 0.2, 0.1).
func scenarioNode(id string, freeFrac float64, count int, rel, exec float64) NodeState {
	const (
		cpuTotal  = int32(10)
		memTotal  = int64(10000)
		diskTotal = int64(100000)
	)
	return NodeState{
		NodeID:      id,
		ClusterPath: "test/dc1/cluster",
		Datacenter:  "dc1",
		Region:      "us-east",
		Labels:      capsule.Labels{},
		Resources: Resources{
			CPUCoresTotal: cpuTotal,
			CPUCoresFree:  int32(math.Round(float64(cpuTotal) * freeFrac)),
			MemoryMBTotal: memTotal,
			MemoryMBFree:  int64(math.Round(float64(memTotal) * freeFrac)),
			DiskMBTotal:   diskTotal,
			DiskMBFree:    int64(math.Round(float64(diskTotal) * freeFrac)),
		},
		Status:               NodeStatusEnum.Active(),
		ReliabilityScore:     rel,
		ExecutionReliability: exec,
		RunningCapsuleCount:  count,
	}
}

func scoreOf(t *testing.T, calc *Calculator, c *capsule.Capsule, node NodeState) float64 {
	t.Helper()
	res := calc.Calculate(c, node)
	if !res.Eligible {
		t.Fatalf("node %s unexpectedly ineligible: %+v", node.NodeID, res.Inelig)
	}
	return float64(res.Score)
}

// TestScenarioS1_DifferentiatedNonZero is the O9 regression: a bare capsule
// (no resource request) must still produce differentiated, strictly positive
// scores across nodes of varying free capacity / load / reliability. Before
// O9-A every bare capsule scored identically (resource factors were skipped),
// which made placement effectively random.
func TestScenarioS1_DifferentiatedNonZero(t *testing.T) {
	c := minimalCapsule("s1")
	calc := NewCalculator()

	n1 := scoreOf(t, calc, c, scenarioNode("n1", 0.9, 0, 1.0, 1.0))
	n2 := scoreOf(t, calc, c, scenarioNode("n2", 0.5, 2, 1.0, 1.0))
	n3 := scoreOf(t, calc, c, scenarioNode("n3", 0.2, 8, 0.6, 0.5))

	if !(n1 > n2 && n2 > n3) {
		t.Fatalf("expected n1 > n2 > n3, got n1=%.2f n2=%.2f n3=%.2f", n1, n2, n3)
	}
	if n3 <= 0 {
		t.Fatalf("worst node must still score > 0, got %.2f", n3)
	}
	// Loose bands around the plan's ≈95 / ≈72 / ≈37 targets.
	if n1 < 90 {
		t.Errorf("n1 should be high (~95), got %.2f", n1)
	}
	if n2 < 65 || n2 > 80 {
		t.Errorf("n2 should be mid (~72), got %.2f", n2)
	}
	if n3 < 30 || n3 > 45 {
		t.Errorf("n3 should be low-positive (~37), got %.2f", n3)
	}
}

// TestScenarioS2_IdleSpreading verifies the symmetric load factor spreads
// idle capsules: placing replicas one at a time across two empty nodes (each
// placement increments the chosen node's committed count) alternates N1, N2,
// N1, N2 because the just-loaded node immediately scores below its idle peer.
func TestScenarioS2_IdleSpreading(t *testing.T) {
	c := minimalCapsule("s2")
	calc := NewCalculator()

	counts := map[string]int{"n1": 0, "n2": 0}
	var seq []string
	for i := 0; i < 4; i++ {
		s1 := scoreOf(t, calc, c, scenarioNode("n1", 1.0, counts["n1"], 1.0, 1.0))
		s2 := scoreOf(t, calc, c, scenarioNode("n2", 1.0, counts["n2"], 1.0, 1.0))
		// Deterministic tiebreak: n1 wins ties (stable order).
		pick := "n1"
		if s2 > s1 {
			pick = "n2"
		}
		counts[pick]++
		seq = append(seq, pick)
	}

	want := []string{"n1", "n2", "n1", "n2"}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("placement %d: got %s want %s (full seq %v)", i, seq[i], want[i], seq)
		}
	}
}

// TestScenarioS3_BusyAvoidance: a heavily committed node (cnt18) scores far
// below a lightly committed one (cnt2) for the same bare capsule.
func TestScenarioS3_BusyAvoidance(t *testing.T) {
	c := minimalCapsule("s3")
	calc := NewCalculator()

	busy := scoreOf(t, calc, c, scenarioNode("n1", 1.0, 18, 1.0, 1.0))
	light := scoreOf(t, calc, c, scenarioNode("n2", 1.0, 2, 1.0, 1.0))

	if light <= busy {
		t.Fatalf("light node must beat busy node: busy=%.2f light=%.2f", busy, light)
	}
	if light-busy < 10 {
		t.Errorf("expected a wide gap (≫), got %.2f", light-busy)
	}
}

// TestScenarioS4_SickConnectionAvoidance: a node with a poor phonebook
// reliability score (0.3) loses to a healthy one by roughly the reliability
// weight's normalized share (~11 points).
func TestScenarioS4_SickConnectionAvoidance(t *testing.T) {
	c := minimalCapsule("s4")
	calc := NewCalculator()

	sick := scoreOf(t, calc, c, scenarioNode("n1", 1.0, 0, 0.3, 1.0))
	healthy := scoreOf(t, calc, c, scenarioNode("n2", 1.0, 0, 1.0, 1.0))

	gap := healthy - sick
	if gap <= 0 {
		t.Fatalf("healthy node must beat connection-sick node: sick=%.2f healthy=%.2f", sick, healthy)
	}
	if gap < 8 || gap > 15 {
		t.Errorf("expected ~11pt gap, got %.2f", gap)
	}
}

// TestScenarioS5_PlacementFailingAvoidance: a node with poor execution
// reliability (0.3) loses to a healthy one by roughly the execution
// reliability weight's normalized share (~8.6 points).
func TestScenarioS5_PlacementFailingAvoidance(t *testing.T) {
	c := minimalCapsule("s5")
	calc := NewCalculator()

	failing := scoreOf(t, calc, c, scenarioNode("n1", 1.0, 0, 1.0, 0.3))
	healthy := scoreOf(t, calc, c, scenarioNode("n2", 1.0, 0, 1.0, 1.0))

	gap := healthy - failing
	if gap <= 0 {
		t.Fatalf("healthy node must beat placement-failing node: failing=%.2f healthy=%.2f", failing, healthy)
	}
	if gap < 6 || gap > 11 {
		t.Errorf("expected ~8.6pt gap, got %.2f", gap)
	}
}

// TestScenarioS6_DoublySickCompoundingAdditiveHealth documents the conscious
// additive-health decision: a doubly-sick but empty node (rel 0.3, exec 0.3)
// still outranks a healthy but nearly-full node, while a healthy empty node
// tops both. Health is a second-order additive signal — the SWIM eligibility
// gate already hard-vetoes truly bad nodes before scoring.
func TestScenarioS6_DoublySickCompoundingAdditiveHealth(t *testing.T) {
	c := minimalCapsule("s6")
	calc := NewCalculator()

	healthyEmpty := scoreOf(t, calc, c, scenarioNode("n2", 1.0, 0, 1.0, 1.0))
	sickEmpty := scoreOf(t, calc, c, scenarioNode("n1", 1.0, 0, 0.3, 0.3))
	healthyFull := scoreOf(t, calc, c, scenarioNode("n3", 0.1, 0, 1.0, 1.0))

	if !(healthyEmpty > sickEmpty && sickEmpty > healthyFull) {
		t.Fatalf("expected healthyEmpty > sickEmpty > healthyFull, got %.2f / %.2f / %.2f",
			healthyEmpty, sickEmpty, healthyFull)
	}
	if healthyFull <= 0 {
		t.Fatalf("healthy-full node must still score > 0, got %.2f", healthyFull)
	}
}
