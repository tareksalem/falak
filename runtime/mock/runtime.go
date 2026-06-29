// Package mock provides an in-memory implementation of runtime.Runtime
// for testing. It tracks container state, simulates start/stop/checkpoint/
// restore, and optionally injects failures for error-path testing.
package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tareksalem/falak/runtime"
)

// eventBufferSize bounds the per-stream buffer for emitted container
// events. Tests emit a handful of events synchronously, so a modest
// buffer keeps EmitContainerEvent non-blocking without unbounded growth.
const eventBufferSize = 64

// container is the in-memory state of a mock container.
type container struct {
	id          string
	image       string
	config      runtime.CreateConfig
	status      runtime.ContainerStatus
	createdAt   time.Time
	startedAt   time.Time
	exitCode    int
	checkpoints map[string]bool // snapshotPath -> exists
}

// Runtime implements runtime.Runtime with in-memory state. Safe for
// concurrent use from multiple goroutines.
type Runtime struct {
	mu         sync.Mutex
	containers map[string]*container
	pulled     map[string]bool // image -> pulled

	// Injected behaviors for testing.
	pullErr        error
	pullErrByImage map[string]error
	createErr      error
	startErr       error
	stopErr        error
	checkpointErr  error
	restoreErr     error
	removeErr      error
	inspectErrByID map[string]error

	// Calls records every method call for assertion in tests.
	Calls []Call

	// eventSubs holds the live channels returned by Events. Each is fed
	// by EmitContainerEvent and closed when its context is cancelled.
	eventSubs []*eventSub
}

// eventSub is one active Events subscription. closed guards against a
// send-on-closed-channel race when the subscription's context is
// cancelled concurrently with an EmitContainerEvent fan-out.
type eventSub struct {
	ch     chan runtime.ContainerEvent
	ctx    context.Context
	closed bool
}

// Call records a method invocation on the mock runtime.
type Call struct {
	Method string
	ID     string
	Image  string
	Path   string
	At     time.Time
}

// Option configures a mock Runtime.
type Option func(*Runtime)

// WithPullError injects an error on every Pull call.
func WithPullError(err error) Option {
	return func(r *Runtime) { r.pullErr = err }
}

// WithPullErrorForImage injects an error returned only when Pull is
// called with the matching image string. Useful for tests that need
// one specific member of a group to fail while the others succeed.
// Multiple calls accumulate per image.
func WithPullErrorForImage(image string, err error) Option {
	return func(r *Runtime) {
		if r.pullErrByImage == nil {
			r.pullErrByImage = make(map[string]error)
		}
		r.pullErrByImage[image] = err
	}
}

// WithCreateError injects an error on every Create call.
func WithCreateError(err error) Option {
	return func(r *Runtime) { r.createErr = err }
}

// WithStartError injects an error on every Start call.
func WithStartError(err error) Option {
	return func(r *Runtime) { r.startErr = err }
}

// WithStopError injects an error on every Stop call.
func WithStopError(err error) Option {
	return func(r *Runtime) { r.stopErr = err }
}

// WithCheckpointError injects an error on every Checkpoint call.
func WithCheckpointError(err error) Option {
	return func(r *Runtime) { r.checkpointErr = err }
}

// WithRestoreError injects an error on every Restore call.
func WithRestoreError(err error) Option {
	return func(r *Runtime) { r.restoreErr = err }
}

// New creates a mock Runtime with the given options.
func New(opts ...Option) *Runtime {
	r := &Runtime{
		containers: make(map[string]*container),
		pulled:     make(map[string]bool),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *Runtime) record(method, id, image, path string) {
	r.Calls = append(r.Calls, Call{
		Method: method,
		ID:     id,
		Image:  image,
		Path:   path,
		At:     time.Now(),
	})
}

// Pull simulates pulling an OCI image. If a per-image override is
// registered via WithPullErrorForImage it takes precedence over the
// global WithPullError; otherwise the call succeeds and records the
// image as pulled.
func (r *Runtime) Pull(_ context.Context, image string, opts ...runtime.PullOption) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("Pull", "", image, "")
	if err, ok := r.pullErrByImage[image]; ok {
		return err
	}
	if r.pullErr != nil {
		return r.pullErr
	}
	r.pulled[image] = true
	return nil
}

// Create simulates creating a container.
func (r *Runtime) Create(_ context.Context, id string, image string, opts ...runtime.CreateOption) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("Create", id, image, "")
	if r.createErr != nil {
		return r.createErr
	}
	if _, exists := r.containers[id]; exists {
		return fmt.Errorf("mock: container %s already exists", id)
	}
	cfg := runtime.ApplyCreateOptions(opts...)
	r.containers[id] = &container{
		id:          id,
		image:       image,
		config:      cfg,
		status:      runtime.ContainerStatusEnum.Created(),
		createdAt:   time.Now(),
		checkpoints: make(map[string]bool),
	}
	return nil
}

// Start simulates starting a container.
func (r *Runtime) Start(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("Start", id, "", "")
	if r.startErr != nil {
		return r.startErr
	}
	c, ok := r.containers[id]
	if !ok {
		return fmt.Errorf("mock: container %s not found", id)
	}
	c.status = runtime.ContainerStatusEnum.Running()
	c.startedAt = time.Now()
	return nil
}

// Stop simulates stopping a container.
func (r *Runtime) Stop(_ context.Context, id string, opts ...runtime.StopOption) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("Stop", id, "", "")
	if r.stopErr != nil {
		return r.stopErr
	}
	c, ok := r.containers[id]
	if !ok {
		return fmt.Errorf("mock: container %s not found", id)
	}
	c.status = runtime.ContainerStatusEnum.Stopped()
	c.exitCode = 0
	return nil
}

// Remove simulates removing a container.
func (r *Runtime) Remove(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("Remove", id, "", "")
	if r.removeErr != nil {
		return r.removeErr
	}
	if _, ok := r.containers[id]; !ok {
		return fmt.Errorf("mock: container %s not found", id)
	}
	delete(r.containers, id)
	return nil
}

// Checkpoint simulates checkpointing a running container.
func (r *Runtime) Checkpoint(_ context.Context, id string, snapshotPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("Checkpoint", id, "", snapshotPath)
	if r.checkpointErr != nil {
		return r.checkpointErr
	}
	c, ok := r.containers[id]
	if !ok {
		return fmt.Errorf("mock: container %s not found", id)
	}
	if c.status != runtime.ContainerStatusEnum.Running() {
		return fmt.Errorf("mock: container %s is not running (status: %s)", id, c.status)
	}
	c.checkpoints[snapshotPath] = true
	c.status = runtime.ContainerStatusEnum.Checkpointed()
	return nil
}

// Restore simulates restoring a container from a checkpoint.
func (r *Runtime) Restore(_ context.Context, id string, snapshotPath string, opts ...runtime.RestoreOption) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("Restore", id, "", snapshotPath)
	if r.restoreErr != nil {
		return r.restoreErr
	}
	// In mock, any path is valid (we don't check if checkpoint exists).
	r.containers[id] = &container{
		id:          id,
		status:      runtime.ContainerStatusEnum.Running(),
		createdAt:   time.Now(),
		startedAt:   time.Now(),
		checkpoints: make(map[string]bool),
	}
	return nil
}

// Stats returns a channel that emits a single zero-value Stats then closes.
func (r *Runtime) Stats(ctx context.Context, id string) (<-chan runtime.Stats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("Stats", id, "", "")
	if _, ok := r.containers[id]; !ok {
		return nil, fmt.Errorf("mock: container %s not found", id)
	}
	ch := make(chan runtime.Stats, 1)
	ch <- runtime.Stats{
		Timestamp:  time.Now(),
		CPUPercent: 5.0,
		MemoryMB:   128,
	}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// Logs returns a channel that emits a single log entry then closes.
func (r *Runtime) Logs(ctx context.Context, id string, opts ...runtime.LogOption) (<-chan runtime.LogEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("Logs", id, "", "")
	if _, ok := r.containers[id]; !ok {
		return nil, fmt.Errorf("mock: container %s not found", id)
	}
	ch := make(chan runtime.LogEntry, 1)
	ch <- runtime.LogEntry{
		Timestamp: time.Now(),
		Stream:    "stdout",
		Line:      "mock log line",
	}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// Inspect returns the current state of a container.
func (r *Runtime) Inspect(_ context.Context, id string) (runtime.ContainerInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("Inspect", id, "", "")
	// Injected transient error takes precedence over the not-found check
	// so tests can exercise the runtime-unreachable escalation path while
	// the container is still tracked.
	if err, ok := r.inspectErrByID[id]; ok && err != nil {
		return runtime.ContainerInfo{}, err
	}
	c, ok := r.containers[id]
	if !ok {
		return runtime.ContainerInfo{}, fmt.Errorf("mock: inspect %s: %w", id, runtime.ErrContainerNotFound)
	}
	return runtime.ContainerInfo{
		ID:        c.id,
		Image:     c.image,
		Status:    c.status,
		CreatedAt: c.createdAt,
		StartedAt: c.startedAt,
		ExitCode:  c.exitCode,
	}, nil
}

// Events returns a channel fed by EmitContainerEvent. The channel is
// buffered and closed when ctx is cancelled, mirroring the production
// backends' contract. Multiple concurrent subscriptions are supported;
// EmitContainerEvent fans out to all of them.
func (r *Runtime) Events(ctx context.Context) (<-chan runtime.ContainerEvent, error) {
	sub := &eventSub{ch: make(chan runtime.ContainerEvent, eventBufferSize), ctx: ctx}
	r.mu.Lock()
	r.eventSubs = append(r.eventSubs, sub)
	r.mu.Unlock()

	go func() {
		<-ctx.Done()
		// Mark closed and close the channel under the lock so a
		// concurrent EmitContainerEvent never sends on a closed channel.
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, s := range r.eventSubs {
			if s == sub {
				r.eventSubs = append(r.eventSubs[:i], r.eventSubs[i+1:]...)
				break
			}
		}
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}()

	return sub.ch, nil
}

// --- Test helpers --------------------------------------------------------

// EmitContainerEvent fans the given event out to every live Events
// subscription. Deterministic and instant: tests use it to drive the
// handler's event consumer without a real clock. Events for closed
// (context-cancelled) subscriptions are dropped.
func (r *Runtime) EmitContainerEvent(evt runtime.ContainerEvent) {
	// Hold the lock across the send so a subscription cannot be closed
	// (close happens under the same lock) between the closed-check and
	// the send. The channel is buffered, so a non-blocking send keeps
	// this from stalling under the lock; if the buffer is full the event
	// is dropped (mirrors a best-effort backend stream).
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sub := range r.eventSubs {
		if sub.closed {
			continue
		}
		select {
		case <-sub.ctx.Done():
		case sub.ch <- evt:
		default:
		}
	}
}

// SetInspectError registers (or clears, via nil err) an error returned by
// Inspect for the given id. The injected error is returned ahead of the
// not-found check, so it must be a non-sentinel (transient) error to
// exercise the runtime-unreachable escalation path.
func (r *Runtime) SetInspectError(id string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inspectErrByID == nil {
		r.inspectErrByID = make(map[string]error)
	}
	if err == nil {
		delete(r.inspectErrByID, id)
		return
	}
	r.inspectErrByID[id] = err
}

// ContainerCount returns the number of tracked containers.
func (r *Runtime) ContainerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.containers)
}

// ContainerStatus returns the status of a container, or Unknown if not found.
func (r *Runtime) ContainerStatus(id string) runtime.ContainerStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.containers[id]
	if !ok {
		return runtime.ContainerStatusEnum.Unknown()
	}
	return c.status
}

// HasPulled returns true if the given image was pulled.
func (r *Runtime) HasPulled(image string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pulled[image]
}

// SetPullErrorForImage registers (or clears, via nil) a Pull error
// for the given image after the mock has been constructed. Used by
// tests that need to toggle the failure behaviour mid-flight (e.g.
// fail the first attempt, succeed the retry).
func (r *Runtime) SetPullErrorForImage(image string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pullErrByImage == nil {
		r.pullErrByImage = make(map[string]error)
	}
	if err == nil {
		delete(r.pullErrByImage, image)
		return
	}
	r.pullErrByImage[image] = err
}

// SimulateCrash changes a running container's status to Failed and
// returns the exit code. Used by tests to trigger the crash → re-election
// flow.
func (r *Runtime) SimulateCrash(id string, exitCode int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.containers[id]
	if !ok {
		return fmt.Errorf("mock: container %s not found", id)
	}
	c.status = runtime.ContainerStatusEnum.Failed()
	c.exitCode = exitCode
	return nil
}

// compile-time check
var _ runtime.Runtime = (*Runtime)(nil)
