package strategy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/service"
)

// fakeTicker is a controllable ticker. Tests call advance() to push a
// tick onto C().
type fakeTicker struct {
	ch      chan time.Time
	stopped bool
	mu      sync.Mutex
}

func newFakeTicker() *fakeTicker { return &fakeTicker{ch: make(chan time.Time, 16)} }

func (f *fakeTicker) C() <-chan time.Time { return f.ch }
func (f *fakeTicker) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return
	}
	f.stopped = true
	close(f.ch)
}

func (f *fakeTicker) advance(n int) {
	for i := 0; i < n; i++ {
		f.mu.Lock()
		stopped := f.stopped
		f.mu.Unlock()
		if stopped {
			return
		}
		f.ch <- time.Now()
	}
}

// tickerFactory hands out fakeTickers in creation order so tests can
// drive them independently.
type tickerFactory struct {
	mu      sync.Mutex
	tickers []*fakeTicker
}

func (f *tickerFactory) newTicker(_ time.Duration) ticker {
	t := newFakeTicker()
	f.mu.Lock()
	f.tickers = append(f.tickers, t)
	f.mu.Unlock()
	return t
}

func (f *tickerFactory) get(idx int) *fakeTicker {
	f.mu.Lock()
	defer f.mu.Unlock()
	if idx >= len(f.tickers) {
		return nil
	}
	return f.tickers[idx]
}

// boolEval is a switchable MetricEvaluator.
type boolEval struct {
	mu     sync.Mutex
	result bool
}

func (b *boolEval) set(v bool) { b.mu.Lock(); b.result = v; b.mu.Unlock() }
func (b *boolEval) Evaluate(string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.result, nil
}

func canarySpec(from, target string, step int32, interval time.Duration, criteria, abortOn []string) service.ServiceSpec {
	return service.ServiceSpec{
		Name: "svc",
		Backends: []service.ServiceBackend{
			{Capsule: from, Weight: 100},
			{Capsule: target, Weight: 100},
		},
		Strategy: &service.Strategy{
			Type: service.StrategyTypeEnum.Canary(),
			Canary: &service.CanaryStrategy{
				From: from, Target: target, Step: step,
				Interval: interval, SuccessCriteria: criteria, AbortOn: abortOn,
			},
		},
	}
}

// waitFor polls the predicate up to 2s with 1ms granularity.
func waitFor(t *testing.T, pred func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waitFor timed out: %s", msg)
}

func TestCanary_AutoMode_AdvancesToTarget(t *testing.T) {
	t.Parallel()
	fac := &tickerFactory{}
	emitter := &recordingEmitter{}
	eng := NewCanary(service.NewServiceID(),
		canarySpec("v1", "v2", 10, 50*time.Millisecond, nil, nil),
		emitter, nil, WithCanaryClock(time.Now, fac.newTicker))
	t.Cleanup(func() { _ = eng.Stop() })
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return fac.get(0) != nil }, "progression ticker created")
	fac.get(0).advance(10)
	waitFor(t, func() bool {
		w := eng.LiveWeights()
		return w["v1"] == 0 && w["v2"] == 100
	}, "auto-advance to 100%")
	emitter.mu.Lock()
	steps := len(emitter.steps)
	emitter.mu.Unlock()
	if steps < 10 {
		t.Fatalf("expected ≥10 step events, got %d", steps)
	}
}

func TestCanary_GatedMode_FreezesAndResumes(t *testing.T) {
	t.Parallel()
	fac := &tickerFactory{}
	eval := &boolEval{result: false}
	emitter := &recordingEmitter{}
	eng := NewCanary(service.NewServiceID(),
		canarySpec("v1", "v2", 50, 20*time.Millisecond, []string{"success_rate > 0.99"}, nil),
		emitter, eval, WithCanaryClock(time.Now, fac.newTicker))
	t.Cleanup(func() { _ = eng.Stop() })
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return fac.get(0) != nil }, "ticker")
	// Criteria fail → frozen, no advance.
	fac.get(0).advance(3)
	waitFor(t, func() bool { return eng.State().Phase == "frozen" }, "frozen")
	if eng.LiveWeights()["v2"] != 0 {
		t.Fatalf("v2 advanced while frozen: %v", eng.LiveWeights())
	}
	// Flip eval → resume.
	eval.set(true)
	fac.get(0).advance(3)
	waitFor(t, func() bool { return eng.LiveWeights()["v2"] >= 50 }, "advance after resume")
}

func TestCanary_ManualMode_NoTimer(t *testing.T) {
	t.Parallel()
	fac := &tickerFactory{}
	emitter := &recordingEmitter{}
	eng := NewCanary(service.NewServiceID(),
		canarySpec("v1", "v2", 25, 0, nil, nil),
		emitter, nil, WithCanaryClock(time.Now, fac.newTicker))
	t.Cleanup(func() { _ = eng.Stop() })
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Manual mode: no progression goroutine, so the factory should
	// not have produced a progression ticker.
	time.Sleep(20 * time.Millisecond)
	if fac.get(0) != nil {
		t.Fatalf("manual mode should not start a progression ticker")
	}
	// Advance steps manually.
	eng.Advance()
	if w := eng.LiveWeights(); w["v2"] != 25 {
		t.Fatalf("after one Advance v2=%d want 25", w["v2"])
	}
	eng.Advance()
	eng.Advance()
	eng.Advance()
	if w := eng.LiveWeights(); w["v2"] != 100 || w["v1"] != 0 {
		t.Fatalf("after four Advances w=%v want v2:100 v1:0", w)
	}
	// Idempotent at target.
	eng.Advance()
	if w := eng.LiveWeights(); w["v2"] != 100 {
		t.Fatalf("idempotent Advance broke: %v", w)
	}
}

func TestCanary_AbortFiresFullRevert(t *testing.T) {
	t.Parallel()
	fac := &tickerFactory{}
	eval := &boolEval{result: false}
	emitter := &recordingEmitter{}
	// Manual mode (interval=0) so only the abort-watch goroutine runs
	// and the abort ticker is deterministically index 0.
	eng := NewCanary(service.NewServiceID(),
		canarySpec("v1", "v2", 25, 0, nil, []string{"errors > 10"}),
		emitter, eval, WithCanaryClock(time.Now, fac.newTicker))
	t.Cleanup(func() { _ = eng.Stop() })
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Manually advance one step so we can verify revert.
	eng.Advance()
	if eng.LiveWeights()["v2"] != 25 {
		t.Fatalf("pre-abort weights wrong: %v", eng.LiveWeights())
	}
	waitFor(t, func() bool { return fac.get(0) != nil }, "abort ticker")
	eval.set(true)
	fac.get(0).advance(1)
	waitFor(t, func() bool { return eng.State().Phase == "aborted" }, "aborted phase")
	w := eng.LiveWeights()
	if w["v1"] != 100 || w["v2"] != 0 {
		t.Fatalf("post-abort weights = %v want v1:100 v2:0", w)
	}
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if len(emitter.aborts) != 1 {
		t.Fatalf("abort events = %d want 1", len(emitter.aborts))
	}
}

func TestCanary_IdentityChangeAutoAborts(t *testing.T) {
	t.Parallel()
	fac := &tickerFactory{}
	emitter := &recordingEmitter{}
	eng := NewCanary(service.NewServiceID(),
		canarySpec("v1", "v2", 10, 50*time.Millisecond, nil, nil),
		emitter, nil, WithCanaryClock(time.Now, fac.newTicker))
	t.Cleanup(func() { _ = eng.Stop() })
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Spec where target has weight 0 == UnresolvedIdentityChanged signal.
	bad := canarySpec("v1", "v2", 10, 50*time.Millisecond, nil, nil)
	bad.Backends[1].Weight = 0
	if err := eng.Update(bad); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if eng.State().Phase != "aborted" {
		t.Fatalf("expected aborted phase, got %q", eng.State().Phase)
	}
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if len(emitter.aborts) != 1 {
		t.Fatalf("abort events = %d want 1", len(emitter.aborts))
	}
}

func TestCanary_StopCancelsInFlight(t *testing.T) {
	t.Parallel()
	fac := &tickerFactory{}
	eng := NewCanary(service.NewServiceID(),
		canarySpec("v1", "v2", 10, 0, nil, []string{"x"}),
		nil, nil, WithCanaryClock(time.Now, fac.newTicker))
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, func() bool { return fac.get(0) != nil }, "abort ticker")
	done := make(chan struct{})
	go func() {
		_ = eng.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
	// Stop must be idempotent.
	if err := eng.Stop(); err != nil {
		t.Fatalf("Stop second call: %v", err)
	}
}

func TestCanary_StartIdempotentAndPostTerminalNoop(t *testing.T) {
	t.Parallel()
	fac := &tickerFactory{}
	eng := NewCanary(service.NewServiceID(),
		canarySpec("v1", "v2", 100, 0, nil, nil),
		nil, nil, WithCanaryClock(time.Now, fac.newTicker))
	t.Cleanup(func() { _ = eng.Stop() })
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start twice: %v", err)
	}
	eng.Advance()
	if eng.State().Phase != "completed" {
		t.Fatalf("Phase after full advance = %q want completed", eng.State().Phase)
	}
	// Post-terminal Update is a no-op.
	if err := eng.Update(canarySpec("v1", "v2", 10, 0, nil, nil)); err != nil {
		t.Fatalf("Update post-terminal: %v", err)
	}
	if eng.State().Phase != "completed" {
		t.Fatalf("Phase changed after post-terminal Update: %q", eng.State().Phase)
	}
}

// TestCanary_LiveWeightsConcurrentWithAdvance exercises the audit
// concurrency-hazard (major #5) on the canary engine: many proxy
// goroutines read LiveWeights while control-plane Advance/Update/Stop
// calls reshape the engine. -race must report no data race.
func TestCanary_LiveWeightsConcurrentWithAdvance(t *testing.T) {
	t.Parallel()
	fac := &tickerFactory{}
	eng := NewCanary(service.NewServiceID(),
		canarySpec("v1", "v2", 5, 0, nil, nil), // manual mode: small steps, no timer
		nil, nil, WithCanaryClock(time.Now, fac.newTicker))
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers — proxy-pick fan-out.
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

	// Writers — Advance + Update interleaved.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			eng.Advance()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; ; j++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = eng.Update(canarySpec("v1", "v2", int32(5+(j%5)), 0, nil, nil))
		}
	}()

	time.Sleep(50 * time.Millisecond) // deliberate timing window
	close(stop)
	wg.Wait()
	_ = eng.Stop()
}
