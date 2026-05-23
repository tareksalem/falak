package bridge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// retryingFakePodman extends fakePodmanClient with the ability to fail
// DeleteNetwork a configured number of times before succeeding. It also
// records the call count so assertions can verify retry budget exactly.
// It does not embed fakePodmanClient because we need different ListNetworks
// behaviour and tighter control over the failure schedule.
type retryingFakePodman struct {
	mu          sync.Mutex
	networks    map[string]*PodmanNetwork
	listErr     error
	deleteCalls int32
	createCalls int32
	// failFirstN: DeleteNetwork fails this many times with deleteErr before
	// succeeding on subsequent calls.
	failFirstN int
	deleteErr  error
	// permanentFail: every DeleteNetwork call returns deleteErr.
	permanentFail bool
}

func newRetryingFakePodman() *retryingFakePodman {
	return &retryingFakePodman{networks: make(map[string]*PodmanNetwork)}
}

func (f *retryingFakePodman) CreateNetwork(_ context.Context, name, subnet, gateway string, mtu int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	atomic.AddInt32(&f.createCalls, 1)
	f.networks[name] = &PodmanNetwork{Name: name, Subnet: subnet, Gateway: gateway, MTU: mtu}
	return nil
}

func (f *retryingFakePodman) DeleteNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := atomic.AddInt32(&f.deleteCalls, 1)
	if f.permanentFail {
		return f.deleteErr
	}
	if f.failFirstN > 0 && int(calls) <= f.failFirstN {
		return f.deleteErr
	}
	if _, ok := f.networks[name]; !ok {
		return ErrPodmanNetworkNotFound
	}
	delete(f.networks, name)
	return nil
}

func (f *retryingFakePodman) InspectNetwork(_ context.Context, name string) (*PodmanNetwork, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.networks[name]
	if !ok {
		return nil, ErrPodmanNetworkNotFound
	}
	cp := *n
	return &cp, nil
}

// ListNetworks implements PodmanNetworkLister so the reaper can find
// falak-* networks owned by no allocation.
func (f *retryingFakePodman) ListNetworks(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]string, 0, len(f.networks))
	for n := range f.networks {
		out = append(out, n)
	}
	return out, nil
}

func (f *retryingFakePodman) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.networks[name]
	return ok
}

func (f *retryingFakePodman) deleteCount() int {
	return int(atomic.LoadInt32(&f.deleteCalls))
}

// stubNetwork manually inserts a Podman network into the fake without
// going through CreateNetwork; mirrors a leak from a previous process.
func (f *retryingFakePodman) stubNetwork(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks[name] = &PodmanNetwork{Name: name}
}

// newRetryManager wires a Manager with retrying fake Podman + fake
// iptables + a tight base delay so the retry path runs in milliseconds.
func newRetryManager(t *testing.T, pod *retryingFakePodman, ipt CommandRunner, opts ...ManagerOption) *Manager {
	t.Helper()
	db, _ := newTestDB(t)
	alloc, err := OpenAllocator(db)
	require.NoError(t, err)
	all := append([]ManagerOption{
		WithPodmanClient(pod),
		WithAllocator(alloc),
		WithIptablesRunner(ipt),
		WithDestroyBaseDelay(time.Millisecond),
	}, opts...)
	m, err := NewManager(all...)
	require.NoError(t, err)
	return m
}

func TestDestroy_TransientFailureRetriesAndSucceeds(t *testing.T) {
	pod := newRetryingFakePodman()
	// One transient "in use" failure, then success.
	pod.failFirstN = 1
	pod.deleteErr = errors.New("network is being used by container abc123")
	ipt := &fakeIptablesRunner{}

	m := newRetryManager(t, pod, ipt)
	_, err := m.Create(context.Background(), "g-tx")
	require.NoError(t, err)
	require.True(t, pod.has("falak-g-tx"))

	require.NoError(t, m.Destroy(context.Background(), "g-tx"))

	require.Equal(t, 2, pod.deleteCount(), "expected one transient failure then a successful delete")
	require.False(t, pod.has("falak-g-tx"))
	require.Empty(t, m.Dangling(), "successful destroy must not leave dangling entries")
}

func TestDestroy_PermanentFailureExhaustsAndDangles(t *testing.T) {
	pod := newRetryingFakePodman()
	pod.permanentFail = true
	pod.deleteErr = errors.New("network is being used by container forever")
	ipt := &fakeIptablesRunner{}

	m := newRetryManager(t, pod, ipt, WithDestroyRetries(3))
	_, err := m.Create(context.Background(), "g-stuck")
	require.NoError(t, err)

	err = m.Destroy(context.Background(), "g-stuck")
	require.Error(t, err)
	require.Equal(t, 3, pod.deleteCount(), "every attempt must run")
	require.Contains(t, m.Dangling(), "falak-g-stuck", "exhausted bridge must be marked dangling")

	// Stop the failures so ReapDangling can reclaim it on the next pass.
	pod.permanentFail = false
	require.NoError(t, m.ReapDangling(context.Background()))
	require.False(t, pod.has("falak-g-stuck"), "reaper must remove the dangling network")
	require.NotContains(t, m.Dangling(), "falak-g-stuck", "reap must clear the entry")
}

func TestDestroy_NetworkNotFoundIsImmediateSuccess(t *testing.T) {
	pod := newRetryingFakePodman()
	ipt := &fakeIptablesRunner{}
	m := newRetryManager(t, pod, ipt)

	_, err := m.Create(context.Background(), "g-nf")
	require.NoError(t, err)
	// Remove the network behind the manager's back so the first delete
	// returns ErrPodmanNetworkNotFound — must succeed without retry.
	pod.mu.Lock()
	delete(pod.networks, "falak-g-nf")
	pod.mu.Unlock()

	require.NoError(t, m.Destroy(context.Background(), "g-nf"))
	require.Equal(t, 1, pod.deleteCount(), "not-found must short-circuit retry")
	_, known, err := m.allocator.Get("g-nf")
	require.NoError(t, err)
	require.False(t, known, "subnet must be released on not-found path")
}

func TestDestroy_NonTransientFailureReturnsImmediately(t *testing.T) {
	pod := newRetryingFakePodman()
	pod.permanentFail = true
	pod.deleteErr = errors.New("podman daemon offline")
	ipt := &fakeIptablesRunner{}

	m := newRetryManager(t, pod, ipt, WithDestroyRetries(5))
	_, err := m.Create(context.Background(), "g-fatal")
	require.NoError(t, err)

	err = m.Destroy(context.Background(), "g-fatal")
	require.Error(t, err)
	require.Equal(t, 1, pod.deleteCount(),
		"non-transient errors must surface on first attempt")
	require.NotContains(t, m.Dangling(), "falak-g-fatal",
		"non-transient errors are caller-fatal; bridge stays allocated")
}

func TestReapDangling_ReclaimsLeakedPodmanNetwork(t *testing.T) {
	pod := newRetryingFakePodman()
	ipt := &fakeIptablesRunner{
		notFoundOn: map[int]bool{1: true, 2: true, 3: true},
	}
	m := newRetryManager(t, pod, ipt)

	// Simulate a leak from a previous process: a falak-* network the
	// allocator does not know about.
	pod.stubNetwork("falak-orphan-1")
	pod.stubNetwork("falak-orphan-2")
	// And a non-falak network the reaper must ignore.
	pod.stubNetwork("podman-default")

	require.NoError(t, m.ReapDangling(context.Background()))
	require.False(t, pod.has("falak-orphan-1"))
	require.False(t, pod.has("falak-orphan-2"))
	require.True(t, pod.has("podman-default"), "non-falak networks are out of scope")
}

func TestReapDangling_SkipsOwnedBridges(t *testing.T) {
	pod := newRetryingFakePodman()
	ipt := &fakeIptablesRunner{}
	m := newRetryManager(t, pod, ipt)
	_, err := m.Create(context.Background(), "g-keep")
	require.NoError(t, err)
	require.NoError(t, m.ReapDangling(context.Background()))
	require.True(t, pod.has("falak-g-keep"), "the allocator still owns this bridge")
}

func TestDestroy_ConcurrentWithReapDoesNotRace(t *testing.T) {
	pod := newRetryingFakePodman()
	ipt := &fakeIptablesRunner{}
	m := newRetryManager(t, pod, ipt)

	for _, g := range []string{"a", "b", "c", "d"} {
		_, err := m.Create(context.Background(), g)
		require.NoError(t, err)
	}
	// Pre-seed one leaked bridge the reaper should catch.
	pod.stubNetwork("falak-leak")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		require.NoError(t, m.Destroy(context.Background(), "a"))
		require.NoError(t, m.Destroy(context.Background(), "b"))
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 3; i++ {
			require.NoError(t, m.ReapDangling(context.Background()))
		}
	}()
	wg.Wait()

	require.False(t, pod.has("falak-leak"), "concurrent reap must catch the leak")
	require.False(t, pod.has("falak-a"))
	require.False(t, pod.has("falak-b"))
	require.True(t, pod.has("falak-c"))
	require.True(t, pod.has("falak-d"))
}

func TestDestroy_ContextCancellationStopsRetry(t *testing.T) {
	pod := newRetryingFakePodman()
	pod.permanentFail = true
	pod.deleteErr = errors.New("network is being used")
	ipt := &fakeIptablesRunner{}

	m := newRetryManager(t, pod, ipt,
		WithDestroyRetries(10), WithDestroyBaseDelay(50*time.Millisecond))
	_, err := m.Create(context.Background(), "g-cancel")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err = m.Destroy(ctx, "g-cancel")
	require.Error(t, err, "cancelled context must surface as an error")
	require.Less(t, pod.deleteCount(), 10, "context cancellation must short-circuit retries")
}
