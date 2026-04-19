package podman

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/tareksalem/falak/runtime"
)

// skipIfNoPodman skips the test if no Podman socket is found.
func skipIfNoPodman(t *testing.T) string {
	t.Helper()

	// Check common socket locations.
	candidates := []string{}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		candidates = append(candidates, xdg+"/podman/podman.sock")
	}
	candidates = append(candidates,
		"/run/podman/podman.sock",
		"/run/user/1000/podman/podman.sock",
	)

	for _, sock := range candidates {
		conn, err := net.DialTimeout("unix", sock, 2*time.Second)
		if err == nil {
			conn.Close()
			return sock
		}
	}
	t.Skip("no Podman socket found — skipping integration test")
	return ""
}

func TestPodman_PullCreateStartInspectStopRemove(t *testing.T) {
	sock := skipIfNoPodman(t)

	rt := New(WithSocketPath(sock))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const image = "docker.io/library/alpine:latest"
	const containerName = "falak-test-podman-integration"

	// Clean up any leftover from a previous failed run.
	rt.Stop(ctx, containerName)
	rt.Remove(ctx, containerName)

	// Pull.
	if err := rt.Pull(ctx, image); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// Create.
	if err := rt.Create(ctx, containerName, image,
		runtime.WithNetworkMode(runtime.NetworkModeEnum.Bridge()),
		runtime.WithEnv(map[string]string{"TEST_VAR": "hello"}),
		runtime.WithCommand("sleep", "30"),
	); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Start.
	if err := rt.Start(ctx, containerName); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Inspect — should be running.
	info, err := rt.Inspect(ctx, containerName)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.Status != runtime.ContainerStatusEnum.Running() {
		t.Errorf("expected Running, got %s", info.Status)
	}
	if info.Name != containerName {
		t.Errorf("name = %q, want %q", info.Name, containerName)
	}
	t.Logf("container running: id=%s, pid=%d, ip=%s", info.ID, info.Pid, info.IP)

	// Stop.
	if err := rt.Stop(ctx, containerName, runtime.WithGracePeriod(5*time.Second)); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Inspect after stop — should be Stopped.
	info2, err := rt.Inspect(ctx, containerName)
	if err != nil {
		t.Fatalf("Inspect after stop: %v", err)
	}
	if info2.Status != runtime.ContainerStatusEnum.Stopped() {
		t.Errorf("expected Stopped, got %s", info2.Status)
	}

	// Remove.
	if err := rt.Remove(ctx, containerName); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Inspect after remove — should fail.
	_, err = rt.Inspect(ctx, containerName)
	if err == nil {
		t.Error("Inspect after Remove should fail")
	}
}

func TestPodman_InspectEnvAndLabels(t *testing.T) {
	sock := skipIfNoPodman(t)

	rt := New(WithSocketPath(sock))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const image = "docker.io/library/alpine:latest"
	const name = "falak-test-env-labels"

	rt.Stop(ctx, name)
	rt.Remove(ctx, name)

	rt.Pull(ctx, image)
	if err := rt.Create(ctx, name, image,
		runtime.WithEnv(map[string]string{"MY_KEY": "my_value"}),
		runtime.WithLabels(map[string]string{"falak.test": "true"}),
		runtime.WithCommand("sleep", "10"),
	); err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() {
		rt.Stop(ctx, name)
		rt.Remove(ctx, name)
	}()

	rt.Start(ctx, name)

	info, err := rt.Inspect(ctx, name)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if info.Env["MY_KEY"] != "my_value" {
		t.Errorf("env MY_KEY = %q, want my_value", info.Env["MY_KEY"])
	}
	if info.Labels["falak.test"] != "true" {
		t.Errorf("label falak.test = %q, want true", info.Labels["falak.test"])
	}
}
