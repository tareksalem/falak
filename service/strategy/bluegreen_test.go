package strategy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/service"
)

// fakeTimer is a controllable timer used in tests.
type fakeTimer struct {
	mu      sync.Mutex
	fired   bool
	stopped bool
	fn      func()
}

func (f *fakeTimer) Stop() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fired || f.stopped {
		return false
	}
	f.stopped = true
	return true
}

// fire invokes the timer function once if it has neither fired nor
// been stopped.
func (f *fakeTimer) fire() bool {
	f.mu.Lock()
	if f.fired || f.stopped {
		f.mu.Unlock()
		return false
	}
	f.fired = true
	fn := f.fn
	f.mu.Unlock()
	if fn != nil {
		fn()
	}
	return true
}

// fakeScheduler stores the most recently scheduled fakeTimer so tests
// can fire it on demand.
type fakeScheduler struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

func (s *fakeScheduler) afterFunc(_ time.Duration, fn func()) timer {
	t := &fakeTimer{fn: fn}
	s.mu.Lock()
	s.timers = append(s.timers, t)
	s.mu.Unlock()
	return t
}

func (s *fakeScheduler) latest() *fakeTimer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.timers) == 0 {
		return nil
	}
	return s.timers[len(s.timers)-1]
}

// recordingEmitter records all events for assertion.
type recordingEmitter struct {
	mu     sync.Mutex
	flips  []struct{ from, to string }
	steps  []struct{ target string }
	aborts []string
}

func (r *recordingEmitter) EmitCanaryStep(_ service.ServiceID, target string, _ LiveWeights) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, struct{ target string }{target})
}
func (r *recordingEmitter) EmitCanaryAborted(_ service.ServiceID, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aborts = append(r.aborts, reason)
}
func (r *recordingEmitter) EmitBlueGreenFlip(_ service.ServiceID, from, to string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flips = append(r.flips, struct{ from, to string }{from, to})
}

func bgSpec(active string, backends ...string) service.ServiceSpec {
	spec := service.ServiceSpec{
		Name: "bg",
		Strategy: &service.Strategy{
			Type:      service.StrategyTypeEnum.BlueGreen(),
			BlueGreen: &service.BlueGreenStrategy{Active: active, Drain: 30 * time.Second},
		},
	}
	for _, b := range backends {
		spec.Backends = append(spec.Backends, service.ServiceBackend{Capsule: b, Weight: 100})
	}
	return spec
}

func TestBlueGreen_InitialWeights(t *testing.T) {
	t.Parallel()
	sched := &fakeScheduler{}
	eng := NewBlueGreen(service.NewServiceID(), bgSpec("v1", "v1", "v2"), nil,
		WithBlueGreenClock(time.Now, sched.afterFunc))
	defer eng.Stop()

	w := eng.LiveWeights()
	if w["v1"] != 100 || w["v2"] != 0 {
		t.Fatalf("initial weights = %v want v1:100 v2:0", w)
	}
	st := eng.State()
	if st.Phase != "active" {
		t.Fatalf("Phase = %q want active", st.Phase)
	}
}

func TestBlueGreen_FlipSwitchesWeightsImmediately(t *testing.T) {
	t.Parallel()
	sched := &fakeScheduler{}
	emitter := &recordingEmitter{}
	eng := NewBlueGreen(service.NewServiceID(), bgSpec("v1", "v1", "v2"), emitter,
		WithBlueGreenClock(time.Now, sched.afterFunc))
	defer eng.Stop()

	if err := eng.Update(bgSpec("v2", "v1", "v2")); err != nil {
		t.Fatalf("Update: %v", err)
	}

	w := eng.LiveWeights()
	if w["v1"] != 0 || w["v2"] != 100 {
		t.Fatalf("post-flip weights = %v want v1:0 v2:100", w)
	}
	if got := eng.State().Phase; got != "draining" {
		t.Fatalf("Phase = %q want draining", got)
	}
	if sched.latest() == nil {
		t.Fatalf("expected drain timer scheduled")
	}
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if len(emitter.flips) != 1 || emitter.flips[0].from != "v1" || emitter.flips[0].to != "v2" {
		t.Fatalf("flip events = %v want one v1->v2", emitter.flips)
	}
}

func TestBlueGreen_DrainTimerTransitionsActive(t *testing.T) {
	t.Parallel()
	sched := &fakeScheduler{}
	eng := NewBlueGreen(service.NewServiceID(), bgSpec("v1", "v1", "v2"), nil,
		WithBlueGreenClock(time.Now, sched.afterFunc))
	defer eng.Stop()

	if err := eng.Update(bgSpec("v2", "v1", "v2")); err != nil {
		t.Fatalf("Update: %v", err)
	}
	tm := sched.latest()
	if tm == nil {
		t.Fatal("missing drain timer")
	}
	if !tm.fire() {
		t.Fatal("timer did not fire")
	}
	if got := eng.State().Phase; got != "active" {
		t.Fatalf("Phase after drain = %q want active", got)
	}
}

func TestBlueGreen_MidDrainFlipCancelsOldTimer(t *testing.T) {
	t.Parallel()
	sched := &fakeScheduler{}
	emitter := &recordingEmitter{}
	eng := NewBlueGreen(service.NewServiceID(), bgSpec("v1", "v1", "v2", "v3"), emitter,
		WithBlueGreenClock(time.Now, sched.afterFunc))
	defer eng.Stop()

	// First flip v1 -> v2.
	if err := eng.Update(bgSpec("v2", "v1", "v2", "v3")); err != nil {
		t.Fatalf("flip 1: %v", err)
	}
	first := sched.latest()
	// Second flip v2 -> v3 mid-drain.
	if err := eng.Update(bgSpec("v3", "v1", "v2", "v3")); err != nil {
		t.Fatalf("flip 2: %v", err)
	}
	second := sched.latest()
	if first == second {
		t.Fatal("expected new timer scheduled for second flip")
	}
	if !first.stopped {
		t.Fatal("expected first timer cancelled by mid-drain flip")
	}
	w := eng.LiveWeights()
	if w["v1"] != 0 || w["v2"] != 0 || w["v3"] != 100 {
		t.Fatalf("post-second-flip weights = %v want v1:0 v2:0 v3:100", w)
	}
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if len(emitter.flips) != 2 {
		t.Fatalf("flip events = %d want 2", len(emitter.flips))
	}
	if emitter.flips[1].from != "v2" || emitter.flips[1].to != "v3" {
		t.Fatalf("second flip = %+v want v2->v3", emitter.flips[1])
	}
}

func TestBlueGreen_StopCancelsPendingTimer(t *testing.T) {
	t.Parallel()
	sched := &fakeScheduler{}
	eng := NewBlueGreen(service.NewServiceID(), bgSpec("v1", "v1", "v2"), nil,
		WithBlueGreenClock(time.Now, sched.afterFunc))
	if err := eng.Update(bgSpec("v2", "v1", "v2")); err != nil {
		t.Fatalf("Update: %v", err)
	}
	tm := sched.latest()
	if err := eng.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !tm.stopped {
		t.Fatal("expected drain timer cancelled by Stop")
	}
}

func TestBlueGreen_StartIdempotent(t *testing.T) {
	t.Parallel()
	sched := &fakeScheduler{}
	eng := NewBlueGreen(service.NewServiceID(), bgSpec("v1", "v1"), nil,
		WithBlueGreenClock(time.Now, sched.afterFunc))
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start twice: %v", err)
	}
}

// TestBlueGreen_LiveWeightsConcurrentWithFlip exercises the audit
// concurrency-hazard (major #5) on the blue-green engine: many proxy
// goroutines read LiveWeights while control-plane Update/Stop calls
// flip the active backend. -race must report no data race.
func TestBlueGreen_LiveWeightsConcurrentWithFlip(t *testing.T) {
	t.Parallel()
	sched := &fakeScheduler{}
	eng := NewBlueGreen(service.NewServiceID(), bgSpec("v1", "v1", "v2"), nil,
		WithBlueGreenClock(time.Now, sched.afterFunc))
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers.
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

	// Writers — flip between v1 and v2 repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		actives := []string{"v1", "v2"}
		for j := 0; ; j++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = eng.Update(bgSpec(actives[j%2], "v1", "v2"))
		}
	}()

	time.Sleep(50 * time.Millisecond) // deliberate timing window
	close(stop)
	wg.Wait()
	_ = eng.Stop()
}
