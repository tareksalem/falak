package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/runtime/mock"
)

// testNodeWithRuntime creates and starts a node with a mock runtime
// injected, using fast timing for integration testing.
func testNodeWithRuntime(t *testing.T, name string, port int, rt *mock.Runtime) *Node {
	t.Helper()

	dataDir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}

	logger, _ := zap.NewDevelopment()

	n := New(
		WithName(name),
		WithListenAddrs(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port)),
		WithDataDir(dataDir),
		WithLogger(logger.Named(name)),
		WithRuntime(rt),
	)

	if err := n.Start(); err != nil {
		t.Fatalf("failed to start %s: %v", name, err)
	}
	return n
}

// TestRuntime_ElectionToContainer verifies the full flow: create a
// capsule on a 3-node cluster with mock runtime → election picks a
// winner → winner's runtime handler cold-starts the container →
// at least one mock runtime has a running container.
func TestRuntime_ElectionToContainer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	const cluster = "test/dc1/runtime"

	rt1 := mock.New()
	rt2 := mock.New()
	rt3 := mock.New()

	n1 := testNodeWithRuntime(t, "rt1", 4201, rt1)
	n2 := testNodeWithRuntime(t, "rt2", 4202, rt2)
	n3 := testNodeWithRuntime(t, "rt3", 4203, rt3)
	defer n1.Stop()
	defer n2.Stop()
	defer n3.Stop()

	joinCluster(t, n1, cluster)
	joinCluster(t, n2, cluster, fmt.Sprintf("/ip4/127.0.0.1/tcp/4201/p2p/%s", n1.ID()))
	joinCluster(t, n3, cluster, fmt.Sprintf("/ip4/127.0.0.1/tcp/4201/p2p/%s", n1.ID()))

	waitForPhonebookCount(t, n1, cluster, 3, 10*time.Second)
	waitForPhonebookCount(t, n2, cluster, 3, 10*time.Second)
	waitForPhonebookCount(t, n3, cluster, 3, 10*time.Second)

	joinOrbitOrFail(t, n1, cluster, "api")
	joinOrbitOrFail(t, n2, cluster, "api")
	joinOrbitOrFail(t, n3, cluster, "api")

	time.Sleep(3 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := n1.Capsules().Create(ctx, cluster, capsule.CapsuleSpec{
		Name:  "runtime-test",
		Image: "registry.test/app:v1",
		Orbit: "api",
	})
	if err != nil {
		t.Fatalf("create capsule: %v", err)
	}

	// Wait for at least one mock runtime to have a running container.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, rt := range []*mock.Runtime{rt1, rt2, rt3} {
			if rt.ContainerCount() > 0 {
				t.Logf("container started on a node, count=%d", rt.ContainerCount())
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Log events for debugging.
	for name, rt := range map[string]*mock.Runtime{"rt1": rt1, "rt2": rt2, "rt3": rt3} {
		t.Logf("%s events: %d, containers: %d", name, len(rt.Events), rt.ContainerCount())
		for _, ev := range rt.Events {
			t.Logf("  %s %s %s %s", ev.Method, ev.ID, ev.Image, ev.Path)
		}
	}

	t.Fatal("timeout: no mock runtime started a container after election")
}
