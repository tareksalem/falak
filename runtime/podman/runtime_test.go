package podman

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tareksalem/falak/runtime"
)

// TestPullStreaming_ParsesProgressLines feeds canned JSON-Lines progress
// records into parsePullProgress and asserts the onProgress callback
// observes the (current, total) deltas in order. Drives the parser
// without an HTTP server so the test runs in milliseconds and stays in
// unit-test pipelines that do not have a Podman socket.
func TestPullStreaming_ParsesProgressLines(t *testing.T) {
	t.Parallel()

	// Realistic Podman libpod /images/pull payload: a sequence of
	// JSON-Lines records, the first carrying a "stream" header, then
	// per-layer progress, then a terminal stream line.
	feed := strings.Join([]string{
		`{"stream":"Trying to pull docker.io/library/alpine:latest..."}`,
		`{"id":"abc","status":"Pulling fs layer","progressDetail":{}}`,
		`{"id":"abc","status":"Downloading","progressDetail":{"current":1024,"total":10240}}`,
		`{"id":"abc","status":"Downloading","progressDetail":{"current":5120,"total":10240}}`,
		`{"id":"abc","status":"Downloading","progressDetail":{"current":10240,"total":10240}}`,
		`{"stream":"Pulled image: docker.io/library/alpine:latest\n"}`,
	}, "\n")

	type observed struct{ current, total int64 }
	var calls []observed
	err := parsePullProgress(strings.NewReader(feed), func(current, total int64) {
		calls = append(calls, observed{current, total})
	})
	if err != nil {
		t.Fatalf("parsePullProgress: %v", err)
	}
	if got, want := len(calls), 6; got != want {
		t.Fatalf("callback fired %d times, want %d (calls=%v)", got, want, calls)
	}

	// The three progress lines must carry the right (current, total).
	want := []observed{
		{0, 0},          // stream header
		{0, 0},          // Pulling fs layer (empty progressDetail)
		{1024, 10240},   // first Downloading tick
		{5120, 10240},   // second Downloading tick
		{10240, 10240},  // final Downloading tick
		{0, 0},          // terminal Pulled stream line
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("call[%d] = %+v, want %+v", i, calls[i], w)
		}
	}
}

// TestPullStreaming_ParsePropagatesDecodeErrors confirms that the parser
// returns a wrapped non-nil error on malformed JSON (something an
// upstream operator must see in logs).
func TestPullStreaming_ParsePropagatesDecodeErrors(t *testing.T) {
	t.Parallel()
	feed := `{"stream":"ok"}` + "\n" + `{not-json`
	err := parsePullProgress(strings.NewReader(feed), func(int64, int64) {})
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("EOF is treated as clean close; bad JSON must surface: %v", err)
	}
}

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

	// Inspect after remove — should be ErrContainerNotFound.
	_, err = rt.Inspect(ctx, containerName)
	if err == nil {
		t.Error("Inspect after Remove should fail")
	}
	if !errors.Is(err, runtime.ErrContainerNotFound) {
		t.Errorf("Inspect after Remove = %v, want ErrContainerNotFound", err)
	}
}

// TestPodman_EventsStream runs a container, kills it, and removes it,
// asserting the Events stream surfaces both a died and a remove action for
// that container. Gated on a live Podman socket.
func TestPodman_EventsStream(t *testing.T) {
	sock := skipIfNoPodman(t)

	rt := New(WithSocketPath(sock))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const image = "docker.io/library/alpine:latest"
	const name = "falak-test-events-stream"

	rt.Stop(ctx, name)
	rt.Remove(ctx, name)

	if err := rt.Pull(ctx, image); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// Subscribe BEFORE starting so we do not miss the lifecycle events.
	evCtx, evCancel := context.WithCancel(ctx)
	defer evCancel()
	events, err := rt.Events(evCtx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	if err := rt.Create(ctx, name, image, runtime.WithCommand("sleep", "60")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := rt.Start(ctx, name); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Force-remove (kills, then removes) → die + remove.
	if err := rt.Remove(ctx, name); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	sawDied, sawRemoved := false, false
	deadline := time.After(30 * time.Second)
	for !(sawDied && sawRemoved) {
		select {
		case <-deadline:
			t.Fatalf("timeout: died=%v removed=%v", sawDied, sawRemoved)
		case evt, ok := <-events:
			if !ok {
				t.Fatalf("event stream closed early: died=%v removed=%v", sawDied, sawRemoved)
			}
			if evt.ContainerID != name {
				continue
			}
			switch evt.Action {
			case runtime.ContainerEventActionEnum.Died():
				sawDied = true
			case runtime.ContainerEventActionEnum.Removed():
				sawRemoved = true
			}
		}
	}
}

// TestPodman_CheckpointRestoreRoundTrip is the O7 regression guard. It
// drives a full Checkpoint → (container gone) → Restore cycle against the
// live socket and asserts the restore returns no error and the container
// comes back Running. Before the O7 fix, Restore sent the archive path as
// the `import` query param (typed as a bool by libpod) with a nil body and
// got HTTP 400 `schema: error converting value for "import"`, so this test
// would fail on the Restore call.
//
// Checkpoint requires root (CRIU). When the test runs against a rootless
// socket, Checkpoint returns a "requires root" error from libpod; we skip
// cleanly in that case rather than failing — the regression guard only
// exercises on a rootful, CRIU-capable host.
func TestPodman_CheckpointRestoreRoundTrip(t *testing.T) {
	sock := skipIfNoPodman(t)

	rt := New(WithSocketPath(sock))
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const image = "docker.io/library/alpine:latest"
	const name = "falak-test-checkpoint-restore"

	// Clean up any leftover from a previous failed run.
	rt.Stop(ctx, name)
	rt.Remove(ctx, name)

	if err := rt.Pull(ctx, image); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if err := rt.Create(ctx, name, image, runtime.WithCommand("sleep", "120")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Ensure cleanup on every exit path.
	defer func() {
		rt.Stop(ctx, name)
		rt.Remove(ctx, name)
	}()

	if err := rt.Start(ctx, name); err != nil {
		t.Fatalf("Start: %v", err)
	}

	snapshotPath := filepath.Join(t.TempDir(), "checkpoint.tar")
	if err := rt.Checkpoint(ctx, name, snapshotPath); err != nil {
		// CRIU checkpoint requires root; rootless sockets cannot run it.
		// libpod surfaces this either as a "requires root" error or as a
		// runc/CRIU checkpoint failure. A working checkpoint is an
		// environmental prerequisite for this restore regression guard, so
		// skip cleanly rather than fail when the host cannot checkpoint.
		if isCheckpointUnsupportedError(err) {
			t.Skipf("checkpoint unsupported on this host (needs root + CRIU) — skipping O7 round-trip: %v", err)
		}
		t.Fatalf("Checkpoint: %v", err)
	}

	// After checkpoint with leaveRunning=false the container is gone.
	if _, err := rt.Inspect(ctx, name); !errors.Is(err, runtime.ErrContainerNotFound) {
		t.Fatalf("Inspect after checkpoint = %v, want ErrContainerNotFound", err)
	}

	// O7 regression guard: this returned HTTP 400 before the fix.
	if err := rt.Restore(ctx, name, snapshotPath); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	info, err := rt.Inspect(ctx, name)
	if err != nil {
		t.Fatalf("Inspect after restore: %v", err)
	}
	if info.Status != runtime.ContainerStatusEnum.Running() {
		t.Errorf("after restore: status = %s, want Running", info.Status)
	}
}

// isCheckpointUnsupportedError reports whether err indicates the host
// cannot perform a CRIU checkpoint (no root, or a runc/CRIU checkpoint
// failure typical of rootless Podman). In those cases the restore
// regression guard cannot run and skips cleanly.
func isCheckpointUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "requires root"):
		return true
	case strings.Contains(msg, "must be run as root"):
		return true
	case strings.Contains(msg, "rootless"):
		return true
	case strings.Contains(msg, "runc checkpoint"):
		return true
	case strings.Contains(msg, "criu"):
		return true
	default:
		return false
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

// --- O15: Restore forwards published ports as publishPorts --------------

// fakeSocketServer stands up an HTTP server on a throwaway unix socket and
// records the query values of the first request whose path matches want. It
// lets the port-publishing tests assert exactly what Restore puts on the wire
// without a live Podman daemon.
type fakeSocketServer struct {
	srv        *http.Server
	sock       string
	mu         sync.Mutex
	lastQuery  url.Values
	lastPath   string
	bodyLen    int64
}

func newFakeSocketServer(t *testing.T, status int) *fakeSocketServer {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "fake-podman.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	f := &fakeSocketServer{sock: sock}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.lastQuery = r.URL.Query()
		f.lastPath = r.URL.Path
		f.bodyLen = n
		f.mu.Unlock()
		w.WriteHeader(status)
	})
	f.srv = &http.Server{Handler: mux}
	go f.srv.Serve(ln)
	t.Cleanup(func() { f.srv.Close() })
	return f
}

func (f *fakeSocketServer) query() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastQuery
}

// TestFormatPublishPort covers the wire encoding for fixed, auto, and
// non-tcp protocol variants.
func TestFormatPublishPort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   runtime.PortMapping
		want string
	}{
		{"fixed tcp default", runtime.PortMapping{HostPort: 8080, ContainerPort: 80}, "8080:80"},
		{"fixed explicit tcp", runtime.PortMapping{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}, "8080:80"},
		{"fixed udp", runtime.PortMapping{HostPort: 5353, ContainerPort: 53, Protocol: "udp"}, "5353:53/udp"},
		{"auto tcp", runtime.PortMapping{ContainerPort: 80}, "80"},
		{"auto udp", runtime.PortMapping{ContainerPort: 53, Protocol: "udp"}, "53/udp"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatPublishPort(tc.in); got != tc.want {
				t.Fatalf("formatPublishPort(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRestore_ForwardsPublishPorts is the O15 regression: Restore must
// re-declare published ports on the import path (fixed as host:container,
// auto as container-only) so a snapshot-restored replica comes up with a
// host port. Runs against a fake unix socket — no live daemon needed.
func TestRestore_ForwardsPublishPorts(t *testing.T) {
	t.Parallel()

	f := newFakeSocketServer(t, http.StatusOK)
	rt := New(WithSocketPath(f.sock))

	snap := filepath.Join(t.TempDir(), "snap.tar")
	if err := os.WriteFile(snap, []byte("fake-checkpoint-archive"), 0600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	ctx := context.Background()
	err := rt.Restore(ctx, "falak-cap1-0", snap,
		runtime.WithRestorePortMappings(
			runtime.PortMapping{Name: "http", HostPort: 8080, ContainerPort: 80},          // fixed
			runtime.PortMapping{Name: "metrics", ContainerPort: 9090},                     // auto
			runtime.PortMapping{Name: "dns", ContainerPort: 53, Protocol: "udp"},          // auto/udp
		),
	)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	q := f.query()
	if got := q.Get("import"); got != "true" {
		t.Errorf("import = %q, want true", got)
	}
	if got := q.Get("name"); got != "falak-cap1-0" {
		t.Errorf("name = %q, want falak-cap1-0", got)
	}
	got := q["publishPorts"]
	want := []string{"8080:80", "9090", "53/udp"}
	if len(got) != len(want) {
		t.Fatalf("publishPorts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("publishPorts[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if f.bodyLen == 0 {
		t.Error("restore sent empty body; expected the checkpoint archive")
	}
}

// TestRestore_NoPortsOmitsPublishPorts confirms a port-free restore keeps
// the wire minimal (import + name only), unchanged from the pre-O15 shape.
func TestRestore_NoPortsOmitsPublishPorts(t *testing.T) {
	t.Parallel()

	f := newFakeSocketServer(t, http.StatusOK)
	rt := New(WithSocketPath(f.sock))

	snap := filepath.Join(t.TempDir(), "snap.tar")
	if err := os.WriteFile(snap, []byte("x"), 0600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	if err := rt.Restore(context.Background(), "falak-cap1-0", snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := f.query()["publishPorts"]; len(got) != 0 {
		t.Errorf("publishPorts = %v, want none", got)
	}
}
