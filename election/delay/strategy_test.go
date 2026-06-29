package delay

import (
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/election"
)

// newCapsuleWithCreatedAt builds a minimal capsule carrying the given
// CreatedAt for anchor tests. Only the fields electionAnchor reads matter.
func newCapsuleWithCreatedAt(created time.Time) *capsule.Capsule {
	return &capsule.Capsule{
		ID:        capsule.CapsuleID("cap-anchor"),
		CreatedAt: created,
	}
}

func TestElectionAnchor(t *testing.T) {
	now := time.Now()
	recent := now.Add(-200 * time.Millisecond)
	stale := now.Add(-2 * defaultMaxAnchorAge)

	tests := []struct {
		name        string
		anchorOn    bool
		reason      election.Reason
		createdAt   time.Time
		wantAnchored bool
		// when anchored, the returned time must equal createdAt
	}{
		{"initial+recent uses announcement", true, election.ReasonEnum.Initial(), recent, true},
		{"initial+zero falls back", true, election.ReasonEnum.Initial(), time.Time{}, false},
		{"initial+stale falls back", true, election.ReasonEnum.Initial(), stale, false},
		{"node-failure never anchors", true, election.ReasonEnum.NodeFailure(), recent, false},
		{"scale-up never anchors", true, election.ReasonEnum.ScaleUp(), recent, false},
		{"anchoring disabled falls back", false, election.ReasonEnum.Initial(), recent, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(WithAnchorToAnnouncement(tt.anchorOn))
			req := election.Request{Reason: tt.reason}
			c := newCapsuleWithCreatedAt(tt.createdAt)

			got, anchored := s.electionAnchor(req, c)
			if anchored != tt.wantAnchored {
				t.Fatalf("anchored = %v, want %v", anchored, tt.wantAnchored)
			}
			if tt.wantAnchored && !got.Equal(tt.createdAt) {
				t.Fatalf("anchor time = %v, want createdAt %v", got, tt.createdAt)
			}
			if !tt.wantAnchored {
				// Fallback must be ~now, not the (possibly past) createdAt.
				if time.Since(got) > time.Second {
					t.Fatalf("fallback anchor not ~now: %v", got)
				}
			}
		})
	}
}

// TestAnchoredPublishOrderingByScore is the core property: two nodes that
// anchor to the SAME announcement time publish in gravity-score order
// regardless of when each started its round. Without anchoring, a node
// starting earlier could publish first despite a worse score.
func TestAnchoredPublishOrderingByScore(t *testing.T) {
	s := New() // anchoring on by default
	created := time.Now().Add(-100 * time.Millisecond)

	// High-score node computes its wait from the shared anchor.
	high := s.electionPublishAt(t, created, 90, "0")
	// Low-score node starts its round 50ms LATER but anchors to the same
	// announcement time, so it must still publish AFTER the high-score node.
	time.Sleep(5 * time.Millisecond)
	low := s.electionPublishAt(t, created, 10, "0")

	if !high.Before(low) {
		t.Fatalf("expected high-score publish (%v) before low-score publish (%v)", high, low)
	}
}

// electionPublishAt is a test helper computing the absolute publish time
// the strategy would pick for a given anchor, score, and replica slot —
// mirroring Decide's math without needing a gravity calculator.
func (s *Strategy) electionPublishAt(t *testing.T, anchor time.Time, score float64, replicaID string) time.Time {
	t.Helper()
	wait := waitForScore(score, s.maxWait) + replicaSlotStagger(replicaID, s.replicaStagger)
	return anchor.Add(wait)
}

func TestWaitForScoreMonotonic(t *testing.T) {
	// Higher score must never produce a longer wait.
	prev := waitForScore(0, defaultMaxWait)
	for sc := 0; sc <= 100; sc += 10 {
		w := waitForScore(float64(sc), defaultMaxWait)
		if w > prev {
			t.Fatalf("wait increased with score at %d: %v > %v", sc, w, prev)
		}
		prev = w
	}
	if waitForScore(100, defaultMaxWait) != 0 {
		t.Fatalf("score 100 should wait 0, got %v", waitForScore(100, defaultMaxWait))
	}
}
