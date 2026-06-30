package metrics

import (
	"testing"

	"github.com/tareksalem/falak/node/phonebook"
)

// applyProviderOptions builds a Provider with the metrics-package defaults
// (neutralReliability = 1.0) and then applies the given options, mirroring
// what NewProvider does without requiring a Manager or phonebook. This keeps
// the buildState read-path tests deterministic and dependency-free.
func applyProviderOptions(opts ...ProviderOption) *Provider {
	p := &Provider{
		capsuleCounts:               noCapsuleCounts{},
		neutralReliability:          1.0,
		neutralExecutionReliability: 0.8,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// TestBuildStateReliabilityPrior verifies the optimistic reliability prior
// (O9-B): nodes with no connection history get the neutral prior, nodes with
// history keep their honest measured SuccessRate, and the option overrides the
// default. See provider.go buildState for the rationale.
func TestBuildStateReliabilityPrior(t *testing.T) {
	const clusterPath = "/falak/test"
	const nodeID = "node-1"

	tests := []struct {
		name      string
		opts      []ProviderOption
		entry     *phonebook.Entry
		wantScore float64
	}{
		{
			name: "fresh node with no connection history uses neutral default",
			entry: &phonebook.Entry{
				ConnectionAttempts: 0,
				SuccessRate:        0,
			},
			wantScore: 1.0,
		},
		{
			name: "node with history uses honest success rate",
			entry: &phonebook.Entry{
				ConnectionAttempts: 4,
				SuccessRate:        0.5,
			},
			wantScore: 0.5,
		},
		{
			name: "node with history and zero success rate stays zero",
			entry: &phonebook.Entry{
				ConnectionAttempts: 3,
				SuccessRate:        0,
			},
			wantScore: 0,
		},
		{
			name: "WithNeutralReliability override respected for zero-attempt node",
			opts: []ProviderOption{WithNeutralReliability(0.7)},
			entry: &phonebook.Entry{
				ConnectionAttempts: 0,
				SuccessRate:        0,
			},
			wantScore: 0.7,
		},
		{
			name: "override does not apply once node has history",
			opts: []ProviderOption{WithNeutralReliability(0.7)},
			entry: &phonebook.Entry{
				ConnectionAttempts: 2,
				SuccessRate:        0.9,
			},
			wantScore: 0.9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := applyProviderOptions(tt.opts...)
			storedBefore := tt.entry.SuccessRate

			state := p.buildState(nodeID, clusterPath, tt.entry, Snapshot{})

			if state.ReliabilityScore != tt.wantScore {
				t.Fatalf("ReliabilityScore = %v, want %v", state.ReliabilityScore, tt.wantScore)
			}
			// The read path must never mutate the stored SuccessRate; that
			// field feeds health/eviction/sync and has to stay honest.
			if tt.entry.SuccessRate != storedBefore {
				t.Fatalf("stored SuccessRate mutated: got %v, want %v",
					tt.entry.SuccessRate, storedBefore)
			}
		})
	}
}

// TestNewProviderDefaultNeutralReliability confirms NewProvider seeds the
// optimistic prior at 1.0 so the default behavior matches the gravity
// factorReliability documentation without any option.
func TestNewProviderDefaultNeutralReliability(t *testing.T) {
	t.Parallel()
	p := NewProvider(nil, nil, "node-1")
	if p.neutralReliability != 1.0 {
		t.Fatalf("default neutralReliability = %v, want 1.0", p.neutralReliability)
	}
}
