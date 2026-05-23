package node

// Tests the end-to-end wiring of the Phase 11B service mesh on the
// node side: capsule event → identity binding → spec mutation →
// canary engine sees zero weight → abort fires. Without the
// post-audit fix bundle this flow stayed dormant (the architect's
// "DORMANT in production" regression): the service.Manager was
// constructed but its strategy engines were nil, so an identity
// change never drove the canary to abort.

import (
	"context"
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/service"
	"github.com/tareksalem/falak/service/strategy"
)

// fakeCapsuleManagerForService is a minimal CapsuleLookup whose only
// purpose is to satisfy the service handler's capsule-manager option.
// We construct a real *capsule.Manager with an in-memory store so the
// service handler can wire its CapsuleLookup adapter against it.
func newServiceFakeCapsuleManager(t *testing.T) *capsule.Manager {
	t.Helper()
	// NewManager defaults to an in-memory store, which is what we
	// want for this end-to-end wiring test.
	return capsule.NewManager()
}

// TestServiceHandler_CapsuleEvent_DrivesCanaryAbort exercises the full
// post-fix wiring: a Service with a canary strategy is created, a
// capsule arrives for the `from` backend, and then a second capsule
// with the same name but a different ID arrives — the identity
// change must zero the from-backend's spec weight AND fan into the
// canary engine, which auto-aborts.
func TestServiceHandler_CapsuleEvent_DrivesCanaryAbort(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	// Build the service handler with the real service.Manager but
	// without a proxy / publisher / subscriber: the wiring under
	// test is the manager-to-strategy fan-out, not the proxy plane.
	h := NewServiceHandler(
		WithServiceHandlerEnabled(true),
		WithServiceHandlerCapsuleManager(newServiceFakeCapsuleManager(t)),
	)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("ServiceHandler.Start: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop() })

	spec := service.ServiceSpec{
		Name:       "payments",
		Visibility: service.VisibilityEnum.Cluster(),
		Ports: []service.ServicePort{
			{Name: "http", Port: 8080, Protocol: service.ProtocolEnum.TCP()},
		},
		Backends: []service.ServiceBackend{
			{Capsule: "payments-v1", Weight: 50},
			{Capsule: "payments-v2", Weight: 50},
		},
		Strategy: &service.Strategy{
			Type: service.StrategyTypeEnum.Canary(),
			Canary: &service.CanaryStrategy{
				Target:  "payments-v2",
				From:    "payments-v1",
				Step:    10,
				AbortOn: []string{"error_rate > 5%"},
			},
		},
	}
	svc, err := h.Manager().Create(context.Background(), "c1", spec)
	if err != nil {
		t.Fatalf("Manager.Create: %v", err)
	}

	// On Create the handler must have spun up a real canary engine.
	engineAny, ok := h.StrategyFor(string(svc.ID))
	if !ok || engineAny == nil {
		t.Fatal("strategy engine missing after Service.Create")
	}
	engine, ok := engineAny.(strategy.Engine)
	if !ok {
		t.Fatalf("strategy engine type mismatch: got %T", engineAny)
	}

	// First capsule receipt: payments-v1 resolves to cap-001. This
	// is the normal happy path — no spec mutation, no abort.
	h.OnCapsuleReceived("payments-v1", "cap-001")

	// Second capsule receipt with a DIFFERENT ID for the same name
	// is the identity change. The manager zeroes payments-v1's
	// spec weight AND re-emits Updated, which the handler routes
	// into engine.Update. The canary engine reads weight==0 on
	// `from` and auto-aborts.
	h.OnCapsuleReceived("payments-v1", "cap-002")

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if engine.State().Phase == "aborted" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := engine.State().Phase; got != "aborted" {
		t.Fatalf("expected canary phase=aborted after identity change, got %q (state=%+v)",
			got, engine.State())
	}

	// Verify the spec mutation propagated to the persisted Service:
	got := h.Manager().Get(svc.ID)
	for _, b := range got.Spec.Backends {
		if b.Capsule == "payments-v1" && b.Weight != 0 {
			t.Errorf("payments-v1 weight should be zeroed after identity change, got %d", b.Weight)
		}
	}
}

// TestServiceHandler_StrategyFor_CreatedOnUpsert verifies an engine
// is constructed for every strategy type on the first Created event.
// Pre-fix, the registry held nil engines and StrategyFor returned
// ok=false.
func TestServiceHandler_StrategyFor_CreatedOnUpsert(t *testing.T) {
	h := NewServiceHandler(
		WithServiceHandlerEnabled(true),
		WithServiceHandlerCapsuleManager(newServiceFakeCapsuleManager(t)),
	)
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("ServiceHandler.Start: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop() })

	cases := []struct {
		name  string
		spec  service.ServiceSpec
		want  string // expected engine state Type field
	}{
		{
			name: "static",
			spec: service.ServiceSpec{
				Name:       "svc-static",
				Visibility: service.VisibilityEnum.Cluster(),
				Ports: []service.ServicePort{
					{Name: "http", Port: 8001, Protocol: service.ProtocolEnum.TCP()},
				},
				Backends: []service.ServiceBackend{{Capsule: "b1", Weight: 100}},
				Strategy: &service.Strategy{Type: service.StrategyTypeEnum.Static()},
			},
			want: string(service.StrategyTypeEnum.Static()),
		},
		{
			name: "blue-green",
			spec: service.ServiceSpec{
				Name:       "svc-bg",
				Visibility: service.VisibilityEnum.Cluster(),
				Ports: []service.ServicePort{
					{Name: "http", Port: 8002, Protocol: service.ProtocolEnum.TCP()},
				},
				Backends: []service.ServiceBackend{
					{Capsule: "blue", Weight: 100},
					{Capsule: "green", Weight: 0},
				},
				Strategy: &service.Strategy{
					Type:      service.StrategyTypeEnum.BlueGreen(),
					BlueGreen: &service.BlueGreenStrategy{Active: "blue", Drain: 10 * time.Millisecond},
				},
			},
			want: string(service.StrategyTypeEnum.BlueGreen()),
		},
		{
			name: "canary",
			spec: service.ServiceSpec{
				Name:       "svc-canary",
				Visibility: service.VisibilityEnum.Cluster(),
				Ports: []service.ServicePort{
					{Name: "http", Port: 8003, Protocol: service.ProtocolEnum.TCP()},
				},
				Backends: []service.ServiceBackend{
					{Capsule: "v1", Weight: 100},
					{Capsule: "v2", Weight: 0},
				},
				Strategy: &service.Strategy{
					Type: service.StrategyTypeEnum.Canary(),
					Canary: &service.CanaryStrategy{
						Target: "v2", From: "v1", Step: 10,
						AbortOn: []string{"never"},
					},
				},
			},
			want: string(service.StrategyTypeEnum.Canary()),
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			svc, err := h.Manager().Create(context.Background(), "c1", c.spec)
			if err != nil {
				t.Fatalf("Create %s: %v", c.name, err)
			}
			engineAny, ok := h.StrategyFor(string(svc.ID))
			if !ok || engineAny == nil {
				t.Fatalf("StrategyFor(%s) missing engine", svc.ID)
			}
			engine, ok := engineAny.(strategy.Engine)
			if !ok {
				t.Fatalf("strategy type mismatch: got %T", engineAny)
			}
			if got := engine.State().Type; got != c.want {
				t.Fatalf("engine Type=%q, want %q", got, c.want)
			}
		})
	}
}
