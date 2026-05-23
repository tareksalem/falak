package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// fakePodmanClient is an in-memory PodmanNetworkClient.
type fakePodmanClient struct {
	mu        sync.Mutex
	networks  map[string]*PodmanNetwork
	createErr error
	deleteErr error
	created   int
	deleted   int
}

func newFakePodman() *fakePodmanClient {
	return &fakePodmanClient{networks: make(map[string]*PodmanNetwork)}
}

func (f *fakePodmanClient) CreateNetwork(_ context.Context, name, subnet, gateway string, mtu int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.created++
	f.networks[name] = &PodmanNetwork{Name: name, Subnet: subnet, Gateway: gateway, MTU: mtu}
	return nil
}

func (f *fakePodmanClient) DeleteNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.networks[name]; !ok {
		return ErrPodmanNetworkNotFound
	}
	delete(f.networks, name)
	f.deleted++
	return nil
}

func (f *fakePodmanClient) InspectNetwork(_ context.Context, name string) (*PodmanNetwork, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.networks[name]
	if !ok {
		return nil, ErrPodmanNetworkNotFound
	}
	cp := *n
	return &cp, nil
}

func (f *fakePodmanClient) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.networks[name]
	return ok
}

// fakeIptablesRunner records every invocation and supports per-call errors.
type fakeIptablesRunner struct {
	mu      sync.Mutex
	calls   [][]string
	failNth int  // if >0, the Nth (1-indexed) call returns failErr
	failErr error
	// notFoundOn returns ErrIptablesRuleNotFound on the listed (1-indexed)
	// call numbers. Used to model "rule already absent" on Destroy.
	notFoundOn map[int]bool
}

func (f *fakeIptablesRunner) Run(_ context.Context, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	idx := len(f.calls)
	if f.notFoundOn[idx] {
		return ErrIptablesRuleNotFound
	}
	if f.failNth > 0 && idx == f.failNth {
		return f.failErr
	}
	return nil
}

func (f *fakeIptablesRunner) snapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

// newManager wires a fresh Manager backed by t.TempDir SQLite + fakes.
func newManager(t *testing.T, podman PodmanNetworkClient, ipt CommandRunner, opts ...ManagerOption) *Manager {
	t.Helper()
	db, _ := newTestDB(t)
	alloc, err := OpenAllocator(db)
	require.NoError(t, err)
	all := append([]ManagerOption{
		WithPodmanClient(podman),
		WithAllocator(alloc),
		WithIptablesRunner(ipt),
	}, opts...)
	m, err := NewManager(all...)
	require.NoError(t, err)
	return m
}

func TestManager_CreateAllocatesAndCallsPodman(t *testing.T) {
	pod := newFakePodman()
	ipt := &fakeIptablesRunner{}
	m := newManager(t, pod, ipt)

	info, err := m.Create(context.Background(), "g-api")
	require.NoError(t, err)
	require.Equal(t, "falak-g-api", info.Name)
	require.Equal(t, "10.88.0.0/24", info.Subnet.String())
	require.Equal(t, "10.88.0.1", info.Gateway.String())
	require.Equal(t, defaultBridgeMTU, info.MTU)
	require.True(t, pod.has("falak-g-api"))
	require.Equal(t, 1, pod.created)
}

func TestManager_CreateInstallsIsolationRules(t *testing.T) {
	pod := newFakePodman()
	ipt := &fakeIptablesRunner{}
	m := newManager(t, pod, ipt)

	_, err := m.Create(context.Background(), "g-iso")
	require.NoError(t, err)

	calls := ipt.snapshot()
	require.Len(t, calls, 3, "expect three iptables -I invocations")
	want := [][]string{
		{"iptables", "-I", "FORWARD", "-i", "falak-g-iso", "-o", "falak-g-iso", "-j", "ACCEPT"},
		{"iptables", "-I", "FORWARD", "-i", "falak-g-iso", "!", "-o", "falak-g-iso", "-j", "DROP"},
		{"iptables", "-I", "FORWARD", "!", "-i", "falak-g-iso", "-o", "falak-g-iso", "-j", "DROP"},
	}
	require.Equal(t, want, calls)
}

func TestManager_DestroyRemovesRulesThenNetworkThenSubnet(t *testing.T) {
	pod := newFakePodman()
	ipt := &fakeIptablesRunner{}
	m := newManager(t, pod, ipt)

	_, err := m.Create(context.Background(), "g-tear")
	require.NoError(t, err)
	preCalls := len(ipt.snapshot())
	require.Equal(t, 1, pod.created)

	require.NoError(t, m.Destroy(context.Background(), "g-tear"))

	calls := ipt.snapshot()
	deleteCalls := calls[preCalls:]
	require.Len(t, deleteCalls, 3)
	// Reverse order on Destroy.
	require.Equal(t, "-D", deleteCalls[0][1])
	require.Equal(t, "!", deleteCalls[0][3]) // first removed rule is the in-direction DROP w/ leading "!"
	require.False(t, pod.has("falak-g-tear"), "podman network should be gone")
	require.Equal(t, 1, pod.deleted)

	// Subnet released → next Allocate for the same group starts fresh.
	_, known, err := m.allocator.Get("g-tear")
	require.NoError(t, err)
	require.False(t, known)
}

func TestManager_CreateRollbackOnIptablesFailure(t *testing.T) {
	pod := newFakePodman()
	// Fail on the first iptables call → no rules persist, Podman network deleted, subnet released.
	ipt := &fakeIptablesRunner{failNth: 1, failErr: errors.New("iptables boom")}
	m := newManager(t, pod, ipt)

	_, err := m.Create(context.Background(), "g-rb")
	require.Error(t, err)
	require.Contains(t, err.Error(), "install isolation rules")
	require.False(t, pod.has("falak-g-rb"), "podman network must be rolled back")
	require.Equal(t, 1, pod.deleted)
	_, known, err := m.allocator.Get("g-rb")
	require.NoError(t, err)
	require.False(t, known, "subnet must be released")
}

func TestManager_CreateRollbackOnPodmanFailure(t *testing.T) {
	pod := newFakePodman()
	pod.createErr = errors.New("podman boom")
	ipt := &fakeIptablesRunner{}
	m := newManager(t, pod, ipt)

	_, err := m.Create(context.Background(), "g-pf")
	require.Error(t, err)
	require.Contains(t, err.Error(), "podman create")
	require.Empty(t, ipt.snapshot(), "no iptables call must be made when Podman fails")
	_, known, err := m.allocator.Get("g-pf")
	require.NoError(t, err)
	require.False(t, known, "subnet must be released after Podman failure")
}

func TestManager_DestroyUnknownGroupIsSuccess(t *testing.T) {
	pod := newFakePodman()
	ipt := &fakeIptablesRunner{
		// Iptables sweep on an unknown group will hit rule-not-found three times.
		notFoundOn: map[int]bool{1: true, 2: true, 3: true},
	}
	m := newManager(t, pod, ipt)

	require.NoError(t, m.Destroy(context.Background(), "never-created"))
}

func TestManager_DestroyPodmanNotFoundIsSuccess(t *testing.T) {
	pod := newFakePodman()
	ipt := &fakeIptablesRunner{}
	m := newManager(t, pod, ipt)

	_, err := m.Create(context.Background(), "g-nf")
	require.NoError(t, err)

	// Manually remove the Podman network behind the manager's back.
	pod.mu.Lock()
	delete(pod.networks, "falak-g-nf")
	pod.mu.Unlock()

	require.NoError(t, m.Destroy(context.Background(), "g-nf"))
	_, known, err := m.allocator.Get("g-nf")
	require.NoError(t, err)
	require.False(t, known, "subnet must be released even when Podman reports not-found")
}

func TestManager_TakeoverModeLogsInfo(t *testing.T) {
	pod := newFakePodman()
	ipt := &fakeIptablesRunner{}
	core, recorded := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	m := newManager(t, pod, ipt, WithIptablesTakeover(true), WithManagerLogger(logger))

	_, err := m.Create(context.Background(), "g-take")
	require.NoError(t, err)
	require.Len(t, ipt.snapshot(), 3, "rules still installed under takeover")
	require.NotEmpty(t, recorded.FilterMessageSnippet("iptables_takeover").All(),
		"expected an INFO log mentioning iptables_takeover")
}

func TestManager_InspectReturnsBridgeInfo(t *testing.T) {
	pod := newFakePodman()
	ipt := &fakeIptablesRunner{}
	m := newManager(t, pod, ipt)

	_, err := m.Create(context.Background(), "g-insp")
	require.NoError(t, err)

	info, err := m.Inspect(context.Background(), "g-insp")
	require.NoError(t, err)
	require.Equal(t, "falak-g-insp", info.Name)
	require.Equal(t, "10.88.0.0/24", info.Subnet.String())
	require.Equal(t, "10.88.0.1", info.Gateway.String())
	require.Equal(t, defaultBridgeMTU, info.MTU)

	_, err = m.Inspect(context.Background(), "missing")
	require.ErrorIs(t, err, ErrBridgeNotFound)
}

// TestManager_NewManagerRequiresDeps + WithBridgeMTU exercise option paths.
func TestManager_OptionsAndValidation(t *testing.T) {
	_, err := NewManager(WithAllocator(&Allocator{}))
	require.Error(t, err)
	_, err = NewManager(WithPodmanClient(newFakePodman()))
	require.Error(t, err)

	pod := newFakePodman()
	ipt := &fakeIptablesRunner{}
	m := newManager(t, pod, ipt, WithBridgeMTU(9000))
	info, err := m.Create(context.Background(), "g-mtu")
	require.NoError(t, err)
	require.Equal(t, 9000, info.MTU)
}
