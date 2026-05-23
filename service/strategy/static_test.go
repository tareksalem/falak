package strategy

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/service"
)

func TestStatic(t *testing.T) {
	t.Parallel()

	mkSpec := func(weights ...struct {
		name string
		w    int32
	}) service.ServiceSpec {
		spec := service.ServiceSpec{Name: "svc"}
		for _, b := range weights {
			spec.Backends = append(spec.Backends, service.ServiceBackend{
				Capsule: b.name,
				Weight:  b.w,
			})
		}
		return spec
	}

	type weightPair = struct {
		name string
		w    int32
	}

	tests := []struct {
		name    string
		initial []weightPair
		update  []weightPair
		want    LiveWeights
	}{
		{
			name:    "frozen weights from spec",
			initial: []weightPair{{"a", 100}, {"b", 50}},
			want:    LiveWeights{"a": 100, "b": 50},
		},
		{
			name:    "update reshapes weights",
			initial: []weightPair{{"a", 100}},
			update:  []weightPair{{"a", 30}, {"b", 70}},
			want:    LiveWeights{"a": 30, "b": 70},
		},
		{
			name:    "zero weight excluded",
			initial: []weightPair{{"a", 100}, {"unresolved", 0}},
			want:    LiveWeights{"a": 100},
		},
		{
			name:    "negative weight excluded",
			initial: []weightPair{{"a", 100}, {"identity-changed", -1}},
			want:    LiveWeights{"a": 100},
		},
		{
			name:    "empty backends yields empty map",
			initial: nil,
			want:    LiveWeights{},
		},
		{
			name:    "update to empty clears all",
			initial: []weightPair{{"a", 100}},
			update:  []weightPair{},
			want:    LiveWeights{},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := NewStatic(service.NewServiceID(), mkSpec(tc.initial...))
			if err := eng.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = eng.Stop() })

			if tc.update != nil {
				if err := eng.Update(mkSpec(tc.update...)); err != nil {
					t.Fatalf("Update: %v", err)
				}
			}

			got := eng.LiveWeights()
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("LiveWeights mismatch: got %v want %v", got, tc.want)
			}

			// State() must clone — mutating the returned map must not
			// affect future calls.
			st := eng.State()
			if st.Type != string(service.StrategyTypeEnum.Static()) {
				t.Fatalf("State.Type = %q want %q", st.Type, service.StrategyTypeEnum.Static())
			}
			if st.Phase != "active" {
				t.Fatalf("State.Phase = %q want active", st.Phase)
			}
			st.CurrentWeights["mutate"] = 999
			if _, leaked := eng.LiveWeights()["mutate"]; leaked {
				t.Fatalf("State.CurrentWeights leaked into engine state")
			}
		})
	}
}

func TestStatic_LiveWeightsIsCopy(t *testing.T) {
	t.Parallel()
	spec := service.ServiceSpec{
		Name:     "svc",
		Backends: []service.ServiceBackend{{Capsule: "a", Weight: 100}},
	}
	eng := NewStatic(service.NewServiceID(), spec)
	w := eng.LiveWeights()
	w["a"] = 0
	if eng.LiveWeights()["a"] != 100 {
		t.Fatalf("LiveWeights() returned aliased map")
	}
}

// TestStatic_LiveWeightsConcurrentWithUpdate verifies that LiveWeights
// is safe to call from many goroutines while Update mutates the
// underlying map. Locks in this concurrent-test set the floor for the
// audit's concurrency-hazard finding (major #5): the proxy reads
// LiveWeights on every backend pick from arbitrary goroutines while
// the control plane reshapes the engine state.
func TestStatic_LiveWeightsConcurrentWithUpdate(t *testing.T) {
	t.Parallel()
	spec := service.ServiceSpec{
		Name: "svc",
		Backends: []service.ServiceBackend{
			{Capsule: "v1", Weight: 100},
			{Capsule: "v2", Weight: 0},
		},
	}
	eng := NewStatic(service.NewServiceID(), spec)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers — typical proxy-pick fan-out.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = eng.LiveWeights()
				_ = eng.State()
			}
		}()
	}

	// Writers — control-plane spec edits.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				next := service.ServiceSpec{
					Name: "svc",
					Backends: []service.ServiceBackend{
						{Capsule: "v1", Weight: int32(100 - (j+seed)%100)},
						{Capsule: "v2", Weight: int32((j + seed) % 100)},
					},
				}
				_ = eng.Update(next)
			}
		}(i)
	}

	time.Sleep(50 * time.Millisecond) // deliberate timing window for concurrent exercise
	close(stop)
	wg.Wait()
}
