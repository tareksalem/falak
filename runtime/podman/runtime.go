// Package podman implements the runtime.Runtime interface by talking
// directly to the Podman REST API over a unix socket. No external SDK
// dependency — just net/http + JSON.
//
// Podman exposes a Docker-compatible API at its socket. We use the
// Podman-native endpoints where they diverge (checkpoint, restore).
//
// Socket locations:
//   - Rootful: /run/podman/podman.sock
//   - Rootless: $XDG_RUNTIME_DIR/podman/podman.sock
//   - macOS/Windows: inside Podman Machine (reached via `podman machine ssh`)
package podman

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/tareksalem/falak/runtime"
)

// Runtime implements runtime.Runtime via the Podman REST API.
type Runtime struct {
	client     *http.Client
	socketPath string
	logger     *zap.Logger

	mu         sync.Mutex
	containers map[string]*containerMeta
}

// containerMeta caches container state between API calls.
type containerMeta struct {
	id        string // podman container ID (returned by create)
	name      string // falak-assigned name
	image     string
	createdAt time.Time
}

// Option configures a Podman Runtime.
type Option func(*Runtime)

// WithSocketPath overrides the auto-detected Podman socket path.
func WithSocketPath(path string) Option {
	return func(r *Runtime) { r.socketPath = path }
}

// WithLogger sets the logger.
func WithLogger(logger *zap.Logger) Option {
	return func(r *Runtime) { r.logger = logger }
}

// New creates a Podman runtime that communicates over the given socket.
// If no socket path is provided, it auto-detects rootless → rootful.
func New(opts ...Option) *Runtime {
	r := &Runtime{
		logger:     zap.NewNop(),
		containers: make(map[string]*containerMeta),
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.socketPath == "" {
		r.socketPath = detectSocket()
	}

	// HTTP client that dials the unix socket.
	r.client = &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", r.socketPath)
			},
		},
		Timeout: 0, // no global timeout; per-request context controls it
	}

	return r
}

// detectSocket returns the first available Podman socket path.
func detectSocket() string {
	// Rootless first.
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		sock := filepath.Join(xdg, "podman", "podman.sock")
		if _, err := os.Stat(sock); err == nil {
			return sock
		}
	}
	// Rootful fallback.
	return "/run/podman/podman.sock"
}

// apiURL builds a URL for the Podman REST API. All endpoints used by
// Falak's runtime are libpod-flavoured (images/pull, containers/create,
// containers/{id}/checkpoint, etc. — names that don't exist in the
// Docker-compat namespace), so every request must be prefixed with
// `/v5.0.0/libpod`. Dropping the `/libpod` segment yields 405 on every
// call because the bare `/v5.0.0/<x>` paths map to the Docker-compat
// API which uses different verbs and request shapes. The host is
// ignored — we dial the unix socket directly.
func apiURL(path string, query url.Values) string {
	u := url.URL{
		Scheme:   "http",
		Host:     "d",
		Path:     "/v5.0.0/libpod" + path,
		RawQuery: query.Encode(),
	}
	return u.String()
}

// --- Runtime interface implementation ------------------------------------

// Pull fetches an OCI image via Podman.
func (r *Runtime) Pull(ctx context.Context, image string, opts ...runtime.PullOption) error {
	cfg := runtime.ApplyPullOptions(opts...)
	r.logger.Info("podman: pulling image", zap.String("image", image))

	q := url.Values{"reference": {image}}
	if cfg.ForcePull {
		q.Set("policy", "always")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL("/images/pull", q), nil)
	if err != nil {
		return fmt.Errorf("podman pull: %w", err)
	}

	if cfg.Username != "" {
		req.SetBasicAuth(cfg.Username, cfg.Password)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("podman pull: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // drain the streaming response

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("podman pull: status %d", resp.StatusCode)
	}
	return nil
}

// PullStreaming fetches an OCI image via Podman's libpod pull endpoint and
// invokes onProgress for every JSON-Lines progress record returned by the
// daemon. progressDetail.current and progressDetail.total are forwarded as
// (current, total) on each call. Records without numeric progress (e.g.
// the terminal "stream":"..." status line) yield a (0, 0) call so callers
// can implement rate-limited heartbeats without inspecting the payload.
//
// onProgress is called synchronously from the parser goroutine; it must
// not block. The method returns when the daemon closes the response body
// or the context is cancelled. Network and protocol errors are wrapped
// with %w so callers can use errors.Is.
//
// When onProgress is nil, PullStreaming degrades to a plain Pull (no
// callback overhead). The method always drains the response body before
// returning so the underlying HTTP connection can be reused.
func (r *Runtime) PullStreaming(ctx context.Context, image string, onProgress func(current, total int64), opts ...runtime.PullOption) error {
	cfg := runtime.ApplyPullOptions(opts...)
	r.logger.Info("podman: streaming pull", zap.String("image", image))

	q := url.Values{"reference": {image}}
	if cfg.ForcePull {
		q.Set("policy", "always")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL("/images/pull", q), nil)
	if err != nil {
		return fmt.Errorf("podman pull: %w", err)
	}
	if cfg.Username != "" {
		req.SetBasicAuth(cfg.Username, cfg.Password)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("podman pull: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("podman pull: status %d", resp.StatusCode)
	}

	if onProgress == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}

	if err := parsePullProgress(resp.Body, onProgress); err != nil {
		return fmt.Errorf("podman pull: stream: %w", err)
	}
	return nil
}

// parsePullProgress reads the Podman pull endpoint's JSON-Lines progress
// feed from r and invokes onProgress for every record. Each record carries
// an optional `progressDetail` object with `current` and `total` byte
// counts; records without that object pass (0, 0) so the caller still
// observes liveness. EOF and io.ErrUnexpectedEOF are treated as the
// daemon's clean close of the stream.
//
// Pulled out as a free function so the test suite can drive it from a
// canned buffer without standing up an HTTP fixture.
func parsePullProgress(r io.Reader, onProgress func(current, total int64)) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	for {
		var record struct {
			ProgressDetail *struct {
				Current json.Number `json:"current"`
				Total   json.Number `json:"total"`
			} `json:"progressDetail"`
		}
		if err := dec.Decode(&record); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		var current, total int64
		if record.ProgressDetail != nil {
			if v, parseErr := record.ProgressDetail.Current.Int64(); parseErr == nil {
				current = v
			}
			if v, parseErr := record.ProgressDetail.Total.Int64(); parseErr == nil {
				total = v
			}
		}
		onProgress(current, total)
	}
}

// Create sets up a container without starting it.
func (r *Runtime) Create(ctx context.Context, id string, image string, opts ...runtime.CreateOption) error {
	cfg := runtime.ApplyCreateOptions(opts...)
	r.logger.Info("podman: creating container", zap.String("id", id), zap.String("image", image))

	// Build the specgen-compatible JSON body.
	spec := map[string]interface{}{
		"name":  id,
		"image": image,
	}

	// Network mode.
	if cfg.NetworkMode == runtime.NetworkModeEnum.Host() {
		spec["netns"] = map[string]string{"nsmode": "host"}
	}

	// Port mappings.
	if len(cfg.Ports) > 0 {
		var portMappings []map[string]interface{}
		for _, p := range cfg.Ports {
			pm := map[string]interface{}{
				"container_port": p.ContainerPort,
				"protocol":       p.Protocol,
			}
			if p.HostPort > 0 {
				pm["host_port"] = p.HostPort
			}
			portMappings = append(portMappings, pm)
		}
		spec["portmappings"] = portMappings
	}

	// Resource limits.
	rl := map[string]interface{}{}
	if cfg.Resources.EffectiveCPUMax() > 0 {
		// Podman uses CPU period/quota. 1 core = period 100000, quota 100000.
		period := uint64(100000)
		quota := int64(cfg.Resources.EffectiveCPUMax() * float64(period))
		rl["cpu_period"] = period
		rl["cpu_quota"] = quota
	}
	if cfg.Resources.EffectiveMemoryMax() > 0 {
		rl["memory"] = cfg.Resources.EffectiveMemoryMax() * 1024 * 1024 // bytes
	}
	if len(rl) > 0 {
		spec["resource_limits"] = rl
	}

	// Environment variables.
	if len(cfg.Env) > 0 {
		spec["env"] = cfg.Env
	}

	// Command override.
	if len(cfg.Command) > 0 {
		spec["command"] = cfg.Command
	}

	// Working directory.
	if cfg.WorkingDir != "" {
		spec["work_dir"] = cfg.WorkingDir
	}

	// Labels.
	if len(cfg.Labels) > 0 {
		spec["labels"] = cfg.Labels
	}

	body, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("podman create: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiURL("/containers/create", nil),
		strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("podman create: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("podman create: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("podman create: status %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		ID string `json:"Id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	r.mu.Lock()
	r.containers[id] = &containerMeta{
		id:        result.ID,
		name:      id,
		image:     image,
		createdAt: time.Now(),
	}
	r.mu.Unlock()

	return nil
}

// Start begins execution of a container.
func (r *Runtime) Start(ctx context.Context, id string) error {
	r.logger.Info("podman: starting container", zap.String("id", id))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiURL("/containers/"+id+"/start", nil), nil)
	if err != nil {
		return fmt.Errorf("podman start: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("podman start: %w", err)
	}
	defer resp.Body.Close()

	// 204 = started, 304 = already running. Both are fine.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("podman start: status %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// Stop sends SIGTERM, waits grace period, then SIGKILL.
func (r *Runtime) Stop(ctx context.Context, id string, opts ...runtime.StopOption) error {
	cfg := runtime.ApplyStopOptions(opts...)
	r.logger.Info("podman: stopping container", zap.String("id", id))

	timeout := int(cfg.GracePeriod.Seconds())
	q := url.Values{"timeout": {strconv.Itoa(timeout)}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiURL("/containers/"+id+"/stop", q), nil)
	if err != nil {
		return fmt.Errorf("podman stop: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("podman stop: %w", err)
	}
	defer resp.Body.Close()

	// 204 = stopped, 304 = already stopped.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("podman stop: status %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// Remove deletes a stopped container.
func (r *Runtime) Remove(ctx context.Context, id string) error {
	r.logger.Info("podman: removing container", zap.String("id", id))

	q := url.Values{"force": {"true"}, "v": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		apiURL("/containers/"+id, q), nil)
	if err != nil {
		return fmt.Errorf("podman remove: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("podman remove: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("podman remove: status %d: %s", resp.StatusCode, respBody)
	}

	r.mu.Lock()
	delete(r.containers, id)
	r.mu.Unlock()

	return nil
}

// Checkpoint captures the container's state via CRIU and exports the
// checkpoint archive to snapshotPath. Uses Podman's checkpoint API
// with leaveRunning=false (container stops after checkpoint).
//
// Podman v5 API: POST /containers/{name}/checkpoint
// The response body contains the checkpoint archive when export=true.
func (r *Runtime) Checkpoint(ctx context.Context, id string, snapshotPath string) error {
	r.logger.Info("podman: checkpointing container",
		zap.String("id", id), zap.String("path", snapshotPath))

	q := url.Values{
		"export":       {"true"},
		"leaveRunning": {"false"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiURL("/containers/"+id+"/checkpoint", q), nil)
	if err != nil {
		return fmt.Errorf("podman checkpoint: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("podman checkpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("podman checkpoint: status %d: %s", resp.StatusCode, respBody)
	}

	// Write the exported checkpoint archive to disk.
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0700); err != nil {
		return fmt.Errorf("podman checkpoint: mkdir: %w", err)
	}
	f, err := os.Create(snapshotPath)
	if err != nil {
		return fmt.Errorf("podman checkpoint: create file: %w", err)
	}

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(snapshotPath) // clean up partial file on failure
		return fmt.Errorf("podman checkpoint: write: %w", err)
	}
	f.Close()
	return nil
}

// Restore creates and starts a container from a CRIU checkpoint archive.
//
// Podman v5 API: POST /containers/{name}/restore. The libpod restore
// endpoint types `import` as a BOOL and reads the checkpoint archive
// from the REQUEST BODY (Content-Type application/x-tar). We therefore
// stream the archive at snapshotPath in the body and set import=true —
// the symmetric counterpart of Checkpoint, which uses export=true and
// reads the archive from the RESPONSE body.
//
// The restored container's HOST-PORT publishing is NOT captured in the CRIU
// checkpoint: an auto-assigned host port is a runtime allocation, and even a
// fixed mapping must be re-declared on the import path. We therefore forward
// cfg.Ports as repeated `publishPorts` query values so the restored container
// re-publishes them (verified live: import=true DOES accept publishPorts →
// HTTP 200 + a fresh host port in Inspect). Without this, a snapshot-restored
// replica comes up with no published host port (O15).
func (r *Runtime) Restore(ctx context.Context, id string, snapshotPath string, opts ...runtime.RestoreOption) error {
	cfg := runtime.ApplyRestoreOptions(opts...)

	r.logger.Info("podman: restoring container",
		zap.String("id", id), zap.String("path", snapshotPath),
		zap.Int("published_ports", len(cfg.Ports)))

	f, err := os.Open(snapshotPath)
	if err != nil {
		return fmt.Errorf("podman restore: open snapshot: %w", err)
	}
	defer f.Close()

	q := url.Values{
		"import": {"true"},
		"name":   {id},
	}
	// Re-declare published ports on the import path. url.Values supports
	// repeats, so each mapping is one publishPorts value in Podman's
	// "[hostPort:]containerPort[/proto]" form.
	for _, p := range cfg.Ports {
		q.Add("publishPorts", formatPublishPort(p))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		apiURL("/containers/"+id+"/restore", q), f)
	if err != nil {
		return fmt.Errorf("podman restore: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-tar")
	// Set ContentLength so the upload is sent with a fixed length instead
	// of chunked transfer encoding (some libpod versions reject chunked).
	if st, statErr := os.Stat(snapshotPath); statErr == nil {
		req.ContentLength = st.Size()
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("podman restore: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("podman restore: status %d: %s", resp.StatusCode, respBody)
	}

	r.mu.Lock()
	r.containers[id] = &containerMeta{
		name:      id,
		createdAt: time.Now(),
	}
	r.mu.Unlock()

	return nil
}

// formatPublishPort renders a PortMapping as a Podman publishPorts value in
// "[hostPort:]containerPort[/proto]" form:
//   - HostPort > 0  → "<hostPort>:<containerPort>" (user-specified, honored verbatim)
//   - HostPort == 0 → "<containerPort>"            (auto — Podman picks a free host port)
//
// The protocol suffix is appended only when the protocol is non-empty and not
// the implicit "tcp", matching Podman's own defaulting so the common case
// stays terse.
func formatPublishPort(p runtime.PortMapping) string {
	var b strings.Builder
	if p.HostPort > 0 {
		fmt.Fprintf(&b, "%d:%d", p.HostPort, p.ContainerPort)
	} else {
		fmt.Fprintf(&b, "%d", p.ContainerPort)
	}
	if p.Protocol != "" && p.Protocol != "tcp" {
		b.WriteByte('/')
		b.WriteString(p.Protocol)
	}
	return b.String()
}

// Stats returns a channel of periodic resource usage snapshots.
func (r *Runtime) Stats(ctx context.Context, id string) (<-chan runtime.Stats, error) {
	r.logger.Debug("podman: stats stream", zap.String("id", id))

	q := url.Values{"stream": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiURL("/containers/"+id+"/stats", q), nil)
	if err != nil {
		return nil, fmt.Errorf("podman stats: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("podman stats: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("podman stats: status %d", resp.StatusCode)
	}

	ch := make(chan runtime.Stats, 8)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		dec := json.NewDecoder(resp.Body)
		for {
			var raw struct {
				CPU     float64 `json:"cpu_percent"`
				MemUsage uint64 `json:"mem_usage"`
				MemLimit uint64 `json:"mem_limit"`
				NetInput uint64 `json:"net_input"`
				NetOutput uint64 `json:"net_output"`
				BlockInput uint64 `json:"block_input"`
				BlockOutput uint64 `json:"block_output"`
			}
			if err := dec.Decode(&raw); err != nil {
				return
			}
			select {
			case ch <- runtime.Stats{
				Timestamp:  time.Now(),
				CPUPercent: raw.CPU,
				MemoryMB:   int64(raw.MemUsage / (1024 * 1024)),
				MemoryMax:  int64(raw.MemLimit / (1024 * 1024)),
				NetworkRx:  int64(raw.NetInput),
				NetworkTx:  int64(raw.NetOutput),
				DiskRead:   int64(raw.BlockInput),
				DiskWrite:  int64(raw.BlockOutput),
			}:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

// Logs returns a channel of log lines from the container.
func (r *Runtime) Logs(ctx context.Context, id string, opts ...runtime.LogOption) (<-chan runtime.LogEntry, error) {
	cfg := runtime.ApplyLogOptions(opts...)
	r.logger.Debug("podman: logs stream", zap.String("id", id))

	q := url.Values{
		"stdout": {"true"},
		"stderr": {"true"},
	}
	if cfg.Follow {
		q.Set("follow", "true")
	}
	if !cfg.Since.IsZero() {
		q.Set("since", cfg.Since.Format(time.RFC3339))
	}
	if cfg.Tail > 0 {
		q.Set("tail", strconv.Itoa(cfg.Tail))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiURL("/containers/"+id+"/logs", q), nil)
	if err != nil {
		return nil, fmt.Errorf("podman logs: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("podman logs: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("podman logs: status %d", resp.StatusCode)
	}

	ch := make(chan runtime.LogEntry, 64)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		// Podman log stream uses Docker multiplexed format: 8-byte header
		// [stream_type(1), 0, 0, 0, size(4 big-endian)] followed by the
		// frame payload. We read each frame properly with io.ReadFull to
		// avoid straddling frame boundaries on partial reads.
		hdr := make([]byte, 8)
		for {
			if _, err := io.ReadFull(resp.Body, hdr); err != nil {
				return // EOF or context cancelled
			}
			streamType := hdr[0]
			frameSize := binary.BigEndian.Uint32(hdr[4:8])
			if frameSize == 0 || frameSize > 1<<20 { // sanity: max 1MB per frame
				continue
			}
			payload := make([]byte, frameSize)
			if _, err := io.ReadFull(resp.Body, payload); err != nil {
				return
			}

			stream := "stdout"
			if streamType == 2 {
				stream = "stderr"
			}
			line := strings.TrimRight(string(payload), "\n")
			select {
			case ch <- runtime.LogEntry{
				Timestamp: time.Now(),
				Stream:    stream,
				Line:      line,
			}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	return ch, nil
}

// Inspect returns the current state of a container.
func (r *Runtime) Inspect(ctx context.Context, id string) (runtime.ContainerInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiURL("/containers/"+id+"/json", nil), nil)
	if err != nil {
		return runtime.ContainerInfo{}, fmt.Errorf("podman inspect: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return runtime.ContainerInfo{}, fmt.Errorf("podman inspect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return runtime.ContainerInfo{}, fmt.Errorf("podman inspect %s: %w", id, runtime.ErrContainerNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return runtime.ContainerInfo{}, fmt.Errorf("podman inspect: status %d", resp.StatusCode)
	}

	var raw struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		Image string `json:"ImageName"`
		State struct {
			Status     string    `json:"Status"`
			Running    bool      `json:"Running"`
			Paused     bool      `json:"Paused"`
			ExitCode   int       `json:"ExitCode"`
			Pid        int       `json:"Pid"`
			StartedAt  time.Time `json:"StartedAt"`
			FinishedAt time.Time `json:"FinishedAt"`
		} `json:"State"`
		Created    time.Time         `json:"Created"`
		Config     struct {
			Hostname string            `json:"Hostname"`
			Env      []string          `json:"Env"`
			Labels   map[string]string `json:"Labels"`
		} `json:"Config"`
		NetworkSettings struct {
			IPAddress string `json:"IPAddress"`
			Ports     map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
		HostConfig struct {
			CPUQuota  int64 `json:"CpuQuota"`
			CPUPeriod int64 `json:"CpuPeriod"`
			Memory    int64 `json:"Memory"`
		} `json:"HostConfig"`
		RestartCount int `json:"RestartCount"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return runtime.ContainerInfo{}, fmt.Errorf("podman inspect: decode: %w", err)
	}

	status := runtime.ContainerStatusEnum.Unknown()
	switch {
	case raw.State.Running:
		status = runtime.ContainerStatusEnum.Running()
	case raw.State.Paused:
		status = runtime.ContainerStatusEnum.Stopped()
	case raw.State.Status == "exited" && raw.State.ExitCode != 0:
		status = runtime.ContainerStatusEnum.Failed()
	case raw.State.Status == "exited":
		status = runtime.ContainerStatusEnum.Stopped()
	case raw.State.Status == "created":
		status = runtime.ContainerStatusEnum.Created()
	}

	// Parse env vars from "KEY=VALUE" list.
	env := make(map[string]string)
	for _, e := range raw.Config.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}

	// Parse port mappings.
	var ports []runtime.PortMapping
	for containerPort, bindings := range raw.NetworkSettings.Ports {
		parts := strings.SplitN(containerPort, "/", 2)
		cp, _ := strconv.Atoi(parts[0])
		proto := "tcp"
		if len(parts) > 1 {
			proto = parts[1]
		}
		for _, b := range bindings {
			hp, _ := strconv.Atoi(b.HostPort)
			ports = append(ports, runtime.PortMapping{
				ContainerPort: uint16(cp),
				HostPort:      uint16(hp),
				Protocol:      proto,
			})
		}
	}

	// CPU limit from quota/period.
	var cpuLimit float64
	if raw.HostConfig.CPUPeriod > 0 {
		cpuLimit = float64(raw.HostConfig.CPUQuota) / float64(raw.HostConfig.CPUPeriod)
	}

	info := runtime.ContainerInfo{
		ID:           raw.ID,
		Name:         strings.TrimPrefix(raw.Name, "/"),
		Image:        raw.Image,
		Status:       status,
		CreatedAt:    raw.Created,
		StartedAt:    raw.State.StartedAt,
		FinishedAt:   raw.State.FinishedAt,
		ExitCode:     raw.State.ExitCode,
		Pid:          raw.State.Pid,
		RestartCount: raw.RestartCount,
		IP:           raw.NetworkSettings.IPAddress,
		Ports:        ports,
		Hostname:     raw.Config.Hostname,
		CPULimit:     cpuLimit,
		MemoryLimit:  raw.HostConfig.Memory / (1024 * 1024),
		Labels:       raw.Config.Labels,
		Env:          env,
	}

	return info, nil
}

// compile-time check
var _ runtime.Runtime = (*Runtime)(nil)
