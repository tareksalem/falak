package mock

import (
	"context"
	"errors"
	"testing"

	"github.com/tareksalem/falak/runtime"
)

func TestPullAndCreate(t *testing.T) {
	r := New()
	ctx := context.Background()

	if err := r.Pull(ctx, "img:v1"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !r.HasPulled("img:v1") {
		t.Error("image should be marked as pulled")
	}

	if err := r.Create(ctx, "c1", "img:v1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.ContainerStatus("c1") != runtime.ContainerStatusEnum.Created() {
		t.Errorf("status should be created, got %s", r.ContainerStatus("c1"))
	}
}

func TestStartStop(t *testing.T) {
	r := New()
	ctx := context.Background()
	r.Create(ctx, "c1", "img:v1")

	if err := r.Start(ctx, "c1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if r.ContainerStatus("c1") != runtime.ContainerStatusEnum.Running() {
		t.Error("should be running")
	}

	if err := r.Stop(ctx, "c1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if r.ContainerStatus("c1") != runtime.ContainerStatusEnum.Stopped() {
		t.Error("should be stopped")
	}
}

func TestCheckpointRestore(t *testing.T) {
	r := New()
	ctx := context.Background()
	r.Create(ctx, "c1", "img:v1")
	r.Start(ctx, "c1")

	if err := r.Checkpoint(ctx, "c1", "/tmp/snap"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if r.ContainerStatus("c1") != runtime.ContainerStatusEnum.Checkpointed() {
		t.Error("should be checkpointed")
	}

	if err := r.Restore(ctx, "c2", "/tmp/snap"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if r.ContainerStatus("c2") != runtime.ContainerStatusEnum.Running() {
		t.Error("restored container should be running")
	}
}

func TestRemove(t *testing.T) {
	r := New()
	ctx := context.Background()
	r.Create(ctx, "c1", "img:v1")

	if err := r.Remove(ctx, "c1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if r.ContainerCount() != 0 {
		t.Error("container should be removed")
	}
}

func TestInspect(t *testing.T) {
	r := New()
	ctx := context.Background()
	r.Create(ctx, "c1", "img:v1")
	r.Start(ctx, "c1")

	info, err := r.Inspect(ctx, "c1")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.ID != "c1" {
		t.Errorf("ID = %q, want c1", info.ID)
	}
	if info.Status != runtime.ContainerStatusEnum.Running() {
		t.Errorf("status = %s, want running", info.Status)
	}
}

func TestSimulateCrash(t *testing.T) {
	r := New()
	ctx := context.Background()
	r.Create(ctx, "c1", "img:v1")
	r.Start(ctx, "c1")

	if err := r.SimulateCrash("c1", 137); err != nil {
		t.Fatalf("SimulateCrash: %v", err)
	}
	if r.ContainerStatus("c1") != runtime.ContainerStatusEnum.Failed() {
		t.Error("should be failed after crash")
	}
}

func TestInjectedErrors(t *testing.T) {
	errBoom := errors.New("boom")
	r := New(WithPullError(errBoom), WithStartError(errBoom))
	ctx := context.Background()

	if err := r.Pull(ctx, "img"); err != errBoom {
		t.Errorf("Pull should return injected error, got %v", err)
	}
	r.pullErr = nil // clear to allow create
	r.Create(ctx, "c1", "img")
	if err := r.Start(ctx, "c1"); err != errBoom {
		t.Errorf("Start should return injected error, got %v", err)
	}
}

func TestDuplicateCreate(t *testing.T) {
	r := New()
	ctx := context.Background()
	r.Create(ctx, "c1", "img:v1")
	if err := r.Create(ctx, "c1", "img:v1"); err == nil {
		t.Error("duplicate create should fail")
	}
}

func TestNotFound(t *testing.T) {
	r := New()
	ctx := context.Background()

	if err := r.Start(ctx, "nope"); err == nil {
		t.Error("start on nonexistent should fail")
	}
	if err := r.Stop(ctx, "nope"); err == nil {
		t.Error("stop on nonexistent should fail")
	}
	if err := r.Remove(ctx, "nope"); err == nil {
		t.Error("remove on nonexistent should fail")
	}
	if _, err := r.Inspect(ctx, "nope"); err == nil {
		t.Error("inspect on nonexistent should fail")
	}
}

// TestInspectMissingIsNotFound asserts that Inspect of an unknown id
// returns an error that satisfies errors.Is(runtime.ErrContainerNotFound),
// the contract the handler's reconcile sweep relies on to distinguish a
// removed container (terminal) from a transient backend error.
func TestInspectMissingIsNotFound(t *testing.T) {
	r := New()
	_, err := r.Inspect(context.Background(), "ghost")
	if err == nil {
		t.Fatal("expected error for missing container")
	}
	if !errors.Is(err, runtime.ErrContainerNotFound) {
		t.Fatalf("error %v does not wrap ErrContainerNotFound", err)
	}
}

func TestEventRecording(t *testing.T) {
	r := New()
	ctx := context.Background()
	r.Pull(ctx, "img:v1")
	r.Create(ctx, "c1", "img:v1")
	r.Start(ctx, "c1")

	if len(r.Calls) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(r.Calls))
	}
	if r.Calls[0].Method != "Pull" {
		t.Errorf("call 0 method = %q, want Pull", r.Calls[0].Method)
	}
	if r.Calls[1].Method != "Create" {
		t.Errorf("call 1 method = %q, want Create", r.Calls[1].Method)
	}
	if r.Calls[2].Method != "Start" {
		t.Errorf("call 2 method = %q, want Start", r.Calls[2].Method)
	}
}

func TestCheckpointRequiresRunning(t *testing.T) {
	r := New()
	ctx := context.Background()
	r.Create(ctx, "c1", "img:v1")

	if err := r.Checkpoint(ctx, "c1", "/tmp/snap"); err == nil {
		t.Error("checkpoint on non-running container should fail")
	}
}
