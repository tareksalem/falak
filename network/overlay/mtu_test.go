package overlay

import (
	"errors"
	"testing"

	"go.uber.org/zap/zaptest"
)

// fakeRouteResolver returns canned values to drive Resolver.Resolve
// without touching /proc.
type fakeRouteResolver struct {
	mtu int
	err error
}

func (f fakeRouteResolver) DefaultRouteMTU() (int, error) { return f.mtu, f.err }

func TestResolver_ExplicitBridgeMTU(t *testing.T) {
	r := New(
		WithBridgeMTU(1450),
		WithRouteResolver(fakeRouteResolver{mtu: 1500}),
		WithLogger(zaptest.NewLogger(t)),
	)
	plan, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.BridgeMTU != 1450 {
		t.Fatalf("BridgeMTU: got %d, want 1450", plan.BridgeMTU)
	}
	// UnderlayMTU is reported as bridge+overhead so operators see the
	// implied budget even when the bridge is forced.
	if plan.UnderlayMTU != 1450+defaultOverlayOverhead {
		t.Fatalf("UnderlayMTU: got %d, want %d", plan.UnderlayMTU, 1450+defaultOverlayOverhead)
	}
	if plan.OverlayOverhead != defaultOverlayOverhead {
		t.Fatalf("OverlayOverhead: got %d, want %d", plan.OverlayOverhead, defaultOverlayOverhead)
	}
}

func TestResolver_ExplicitUnderlayMTU(t *testing.T) {
	r := New(
		WithUnderlayMTU(1500),
		WithOverlayOverhead(110),
		WithRouteResolver(fakeRouteResolver{err: errors.New("must not be called")}),
		WithLogger(zaptest.NewLogger(t)),
	)
	plan, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.UnderlayMTU != 1500 || plan.BridgeMTU != 1390 || plan.OverlayOverhead != 110 {
		t.Fatalf("Plan: got %+v, want underlay=1500 bridge=1390 overhead=110", plan)
	}
}

func TestResolver_DetectedUnderlay(t *testing.T) {
	r := New(
		WithRouteResolver(fakeRouteResolver{mtu: 1500}),
		WithLogger(zaptest.NewLogger(t)),
	)
	plan, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.UnderlayMTU != 1500 || plan.BridgeMTU != 1390 {
		t.Fatalf("Plan: got %+v, want underlay=1500 bridge=1390", plan)
	}
}

func TestResolver_CustomOverhead(t *testing.T) {
	r := New(
		WithUnderlayMTU(9000),
		WithOverlayOverhead(50),
		WithLogger(zaptest.NewLogger(t)),
	)
	plan, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if plan.BridgeMTU != 8950 {
		t.Fatalf("BridgeMTU: got %d, want 8950", plan.BridgeMTU)
	}
}

func TestResolver_TooSmallBridge(t *testing.T) {
	r := New(
		WithUnderlayMTU(600),
		WithLogger(zaptest.NewLogger(t)),
	)
	_, err := r.Resolve()
	if !errors.Is(err, ErrBridgeMTUTooSmall) {
		t.Fatalf("err: got %v, want ErrBridgeMTUTooSmall", err)
	}
	// The computed bridge value (490) must surface in the message so
	// operators don't have to re-derive it from logs.
	if msg := err.Error(); !contains(msg, "490") {
		t.Fatalf("error message missing computed bridge value: %s", msg)
	}
}

func TestResolver_ResolverFailureWrapped(t *testing.T) {
	cause := errors.New("synthetic probe failure")
	r := New(
		WithRouteResolver(fakeRouteResolver{err: cause}),
		WithLogger(zaptest.NewLogger(t)),
	)
	_, err := r.Resolve()
	if !errors.Is(err, ErrRouteResolverFailed) {
		t.Fatalf("err: got %v, want wraps ErrRouteResolverFailed", err)
	}
	if !contains(err.Error(), "synthetic probe failure") {
		t.Fatalf("error message missing underlying cause: %s", err.Error())
	}
}

// contains is a tiny strings.Contains shim kept local so the test file
// pulls no extra imports.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
