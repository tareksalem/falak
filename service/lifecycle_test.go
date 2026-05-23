package service

import (
	"testing"
)

func TestLifecycleHappyPath_Drain(t *testing.T) {
	id := NewServiceID()
	var events []LifecycleEvent
	lc := NewLifecycle(id, WithLifecycleHandler(func(e LifecycleEvent) {
		events = append(events, e)
	}))

	if got := lc.State(); got != ServiceStatusEnum.Created() {
		t.Fatalf("initial state = %s, want created", got)
	}

	steps := []struct {
		trigger string
		want    ServiceStatus
	}{
		{TriggerActivate, ServiceStatusEnum.Active()},
		{TriggerStartDraining, ServiceStatusEnum.Draining()},
		{TriggerFinishDraining, ServiceStatusEnum.Deleted()},
	}
	for _, step := range steps {
		if err := lc.Fire(step.trigger); err != nil {
			t.Fatalf("Fire(%s) failed: %v", step.trigger, err)
		}
		if lc.State() != step.want {
			t.Fatalf("after %s: state = %s, want %s", step.trigger, lc.State(), step.want)
		}
	}

	if len(events) == 0 {
		t.Error("expected transition events")
	}
}

func TestLifecycleCanaryAbortAndResume(t *testing.T) {
	id := NewServiceID()
	lc := NewLifecycle(id)

	if err := lc.Fire(TriggerActivate); err != nil {
		t.Fatalf("Fire(Activate): %v", err)
	}
	if err := lc.Fire(TriggerAbortCanary); err != nil {
		t.Fatalf("Fire(AbortCanary): %v", err)
	}
	if lc.State() != ServiceStatusEnum.CanaryAborted() {
		t.Errorf("state after abort = %s, want canary_aborted", lc.State())
	}
	if err := lc.Fire(TriggerResume); err != nil {
		t.Fatalf("Fire(Resume): %v", err)
	}
	if lc.State() != ServiceStatusEnum.Active() {
		t.Errorf("state after resume = %s, want active", lc.State())
	}
}

func TestLifecycleInvalidTransitionsRejected(t *testing.T) {
	id := NewServiceID()
	lc := NewLifecycle(id)

	// From Created the only legal trigger is Activate.
	bad := []string{
		TriggerStartDraining,
		TriggerFinishDraining,
		TriggerAbortCanary,
		TriggerResume,
	}
	for _, tr := range bad {
		if err := lc.Fire(tr); err == nil {
			t.Errorf("Fire(%s) from Created: expected error, got nil", tr)
		}
		if lc.State() != ServiceStatusEnum.Created() {
			t.Errorf("state changed after illegal trigger %s: %s", tr, lc.State())
		}
	}

	// Activate to reach Active.
	if err := lc.Fire(TriggerActivate); err != nil {
		t.Fatalf("Fire(Activate): %v", err)
	}
	// Activate again should fail (no self-loop).
	if err := lc.Fire(TriggerActivate); err == nil {
		t.Errorf("Fire(Activate) from Active: expected error, got nil")
	}

	// Drain to Deleted, then verify no transitions are permitted.
	if err := lc.Fire(TriggerStartDraining); err != nil {
		t.Fatalf("Fire(StartDraining): %v", err)
	}
	if err := lc.Fire(TriggerFinishDraining); err != nil {
		t.Fatalf("Fire(FinishDraining): %v", err)
	}
	for _, tr := range []string{TriggerActivate, TriggerStartDraining, TriggerFinishDraining, TriggerAbortCanary, TriggerResume} {
		if err := lc.Fire(tr); err == nil {
			t.Errorf("Fire(%s) from Deleted: expected error, got nil", tr)
		}
	}
}

func TestLifecycleCanFire(t *testing.T) {
	id := NewServiceID()
	lc := NewLifecycle(id)

	if !lc.CanFire(TriggerActivate) {
		t.Error("CanFire(Activate) from Created = false, want true")
	}
	if lc.CanFire(TriggerStartDraining) {
		t.Error("CanFire(StartDraining) from Created = true, want false")
	}

	if err := lc.Fire(TriggerActivate); err != nil {
		t.Fatalf("Fire(Activate): %v", err)
	}
	if !lc.CanFire(TriggerStartDraining) {
		t.Error("CanFire(StartDraining) from Active = false, want true")
	}
	if !lc.CanFire(TriggerAbortCanary) {
		t.Error("CanFire(AbortCanary) from Active = false, want true")
	}
}

func TestLifecycleInitialStateOverride(t *testing.T) {
	id := NewServiceID()
	lc := NewLifecycle(id, WithLifecycleInitialState(ServiceStatusEnum.Active()))
	if lc.State() != ServiceStatusEnum.Active() {
		t.Errorf("initial state = %s, want active", lc.State())
	}
	if err := lc.Fire(TriggerStartDraining); err != nil {
		t.Errorf("Fire(StartDraining) from restored Active: %v", err)
	}
}

func TestLifecycleInitialStateOverride_InvalidIgnored(t *testing.T) {
	id := NewServiceID()
	lc := NewLifecycle(id, WithLifecycleInitialState("not-a-status"))
	// Invalid override is ignored; default to Created.
	if lc.State() != ServiceStatusEnum.Created() {
		t.Errorf("initial state = %s, want created (invalid override ignored)", lc.State())
	}
}

func TestLifecycleHandlerSeesFromAndTo(t *testing.T) {
	id := NewServiceID()
	var captured LifecycleEvent
	lc := NewLifecycle(id, WithLifecycleHandler(func(e LifecycleEvent) {
		captured = e
	}))
	if err := lc.Fire(TriggerActivate); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if captured.From != ServiceStatusEnum.Created() {
		t.Errorf("captured.From = %s, want created", captured.From)
	}
	if captured.To != ServiceStatusEnum.Active() {
		t.Errorf("captured.To = %s, want active", captured.To)
	}
	if captured.Trigger != TriggerActivate {
		t.Errorf("captured.Trigger = %s, want %s", captured.Trigger, TriggerActivate)
	}
	if captured.ServiceID != id {
		t.Errorf("captured.ServiceID = %s, want %s", captured.ServiceID, id)
	}
}

func TestNewServiceID_Unique(t *testing.T) {
	a := NewServiceID()
	b := NewServiceID()
	if a == b {
		t.Errorf("NewServiceID collision: %s == %s", a, b)
	}
	if len(a.String()) != 32 {
		t.Errorf("ID len = %d, want 32 hex chars", len(a.String()))
	}
}
