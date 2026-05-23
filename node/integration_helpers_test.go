package node

// integration_helpers_test.go centralizes the polling and synchronisation
// helpers used by the node-package integration test suite. Every
// integration test in this package must prefer one of these helpers
// over a bare `time.Sleep`.
//
// Policy (audit major #2): `time.Sleep` is BANNED in integration tests
// EXCEPT when the test is exercising a deliberate timing window (e.g.
// blue-green drain, abort cadence). Those sites carry a comment that
// names the specific timing invariant being asserted. Every other
// "wait for state" must use waitForT (or one of the existing
// per-fixture helpers built on top of it).
//
// The polling cadence (10ms) is short enough that test wall-clock time
// is bounded by convergence latency, not by sleep granularity.

import (
	"testing"
	"time"
)

// waitForT polls fn until it returns true or the timeout expires. The
// caller MUST pass *testing.T so a timeout fails the test immediately
// with a meaningful location. fn is expected to be cheap; callers that
// need a label on the assertion can wrap waitForT in a per-fixture
// helper (see the existing waitForGroupOn / waitForCapsuleStatus /
// waitForContainersRunning helpers in this package).
//
// 10ms polling granularity keeps wall-clock waste under control while
// staying well below the dispatch latency of the orbit subscriber loop.
func waitForT(t *testing.T, fn func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitForT: %s (timeout %v)", msg, timeout)
}

// waitForCondT is the boolean-return cousin used in negative
// assertions ("the condition NEVER becomes true within window") where
// the test must keep running on timeout rather than failing.
func waitForCondT(timeout time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fn()
}

// waitForGossipMeshSettle is a documented timing buffer for gossipsub
// mesh formation between freshly-joined orbit subscribers. Gossipsub
// exposes no public "mesh formed" observable today, so the test suite
// pays a static wait between joinOrbit and any publish that must reach
// every peer. The buffer length is sized for the mesh-heartbeat cadence
// (default 1s, with grafts settling on the second tick).
//
// Per audit major #2: every site that previously called time.Sleep for
// mesh-formation reasons MUST route through this helper so the policy
// is searchable. If gossipsub later exposes a mesh-state callback, the
// implementation switches in one place.
func waitForGossipMeshSettle(peers int) {
	switch {
	case peers <= 1:
		time.Sleep(500 * time.Millisecond)
	case peers == 2:
		time.Sleep(2 * time.Second)
	default:
		time.Sleep(3 * time.Second)
	}
}
