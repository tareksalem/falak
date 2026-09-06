// Package runtime defines the container lifecycle interface used by Falak
// to run capsules. The interface is backend-agnostic: the v1 implementation
// uses Podman + CRIU, but the interface can be satisfied by containerd,
// gVisor, or any OCI-compatible runtime.
//
// Every node scores only itself during election; the runtime is what
// turns "this node won replica X" into a live container. It also owns
// checkpoint/restore (snapshot creation and fast startup) and
// observability (stats + logs).
package runtime

import (
	"context"
	"errors"
	"time"
)

// ErrContainerNotFound is the sentinel returned by Inspect (and any other
// container-scoped call) when the backend reports that the container no
// longer exists — e.g. Podman returns HTTP 404 after the container has
// been removed. Callers use errors.Is to distinguish a genuinely-gone
// container (terminal, triggers re-election) from a transient backend
// error (retryable).
var ErrContainerNotFound = errors.New("runtime: container not found")

// ContainerStatus represents the current state of a container.
type ContainerStatus string

const (
	containerStatusCreated    ContainerStatus = "created"
	containerStatusRunning    ContainerStatus = "running"
	containerStatusStopped    ContainerStatus = "stopped"
	containerStatusCheckpoint ContainerStatus = "checkpointed"
	containerStatusFailed     ContainerStatus = "failed"
	containerStatusUnknown    ContainerStatus = "unknown"
)

type containerStatusEnum struct{}

// ContainerStatusEnum is the public accessor for ContainerStatus values.
var ContainerStatusEnum containerStatusEnum

func (containerStatusEnum) Created() ContainerStatus    { return containerStatusCreated }
func (containerStatusEnum) Running() ContainerStatus    { return containerStatusRunning }
func (containerStatusEnum) Stopped() ContainerStatus    { return containerStatusStopped }
func (containerStatusEnum) Checkpointed() ContainerStatus { return containerStatusCheckpoint }
func (containerStatusEnum) Failed() ContainerStatus     { return containerStatusFailed }
func (containerStatusEnum) Unknown() ContainerStatus    { return containerStatusUnknown }

// ContainerEventAction enumerates the lifecycle transitions a backend can
// report on its event stream. Backends map their native event names onto
// these values; anything not relevant to crash/removal detection maps to
// Other (and is dropped by the consumer).
type ContainerEventAction string

const (
	containerEventDied    ContainerEventAction = "died"
	containerEventRemoved ContainerEventAction = "removed"
	containerEventStarted ContainerEventAction = "started"
	containerEventStopped ContainerEventAction = "stopped"
	containerEventOther   ContainerEventAction = "other"
)

type containerEventActionEnum struct{}

// ContainerEventActionEnum is the public accessor for ContainerEventAction values.
var ContainerEventActionEnum containerEventActionEnum

// Died reports a container that exited (carries a real exit code).
func (containerEventActionEnum) Died() ContainerEventAction { return containerEventDied }

// Removed reports a container that was deleted — terminal, cannot be restarted.
func (containerEventActionEnum) Removed() ContainerEventAction { return containerEventRemoved }

// Started reports a container that began execution.
func (containerEventActionEnum) Started() ContainerEventAction { return containerEventStarted }

// Stopped reports a container that was stopped (without removal).
func (containerEventActionEnum) Stopped() ContainerEventAction { return containerEventStopped }

// Other is the catch-all for backend actions Falak does not act on
// (create, init, kill, cleanup, health_status, …). Consumers drop these.
func (containerEventActionEnum) Other() ContainerEventAction { return containerEventOther }

// ContainerEvent is a single backend lifecycle event for one container.
// ContainerID is the Falak container name (equal to containerID(capsuleID,
// replicaID), e.g. "falak-<cap>-<rep>") — the ownership key the handler
// uses, NOT the backend's opaque hash. ExitCode is meaningful only for a
// Died action; it is 0 otherwise.
type ContainerEvent struct {
	// ContainerID is the Falak container name (falak-<cap>-<rep>).
	ContainerID string

	// Action is the lifecycle transition this event represents.
	Action ContainerEventAction

	// ExitCode is the container's exit code, valid only when Action is Died.
	ExitCode int

	// Time is when the backend recorded the event.
	Time time.Time
}

// NetworkMode selects the container's network isolation model.
type NetworkMode string

const (
	networkModeBridge NetworkMode = "bridge"
	networkModeHost   NetworkMode = "host"
)

type networkModeEnum struct{}

// NetworkModeEnum is the public accessor for NetworkMode values.
var NetworkModeEnum networkModeEnum

func (networkModeEnum) Bridge() NetworkMode { return networkModeBridge }
func (networkModeEnum) Host() NetworkMode   { return networkModeHost }

// PortMapping maps a container port to a host port.
type PortMapping struct {
	// Name is a human-readable label for the mapping (e.g. "http", "grpc").
	Name string

	// ContainerPort is the port inside the container.
	ContainerPort uint16

	// HostPort is the port on the host. Zero means auto-assign.
	HostPort uint16

	// Protocol is "tcp" (default) or "udp".
	Protocol string
}

// PortBinding is a resolved container-to-host port mapping observed after a
// container is running. Unlike PortMapping (the requested spec), an
// auto-assigned host port here carries the concrete allocation the runtime
// chose at start, and a restored container carries its freshly re-published
// host port. The handler discovers these via a post-start readback and hands
// them to a ReplicaNetworkRecorder for per-replica observability.
type PortBinding struct {
	// Name is the human-readable label carried over from the spec port.
	Name string

	// ContainerPort is the port inside the container.
	ContainerPort uint16

	// HostPort is the resolved host port. Zero means unresolved (an auto port
	// that had not been bound before the readback budget expired).
	HostPort uint16
}

// ResourceLimits defines the CPU and memory constraints for a container.
// Reservation is the guaranteed minimum (used by gravity for scoring).
// Max is the hard ceiling (CPU throttled, memory OOM-killed).
// If Max is zero, it defaults to the reservation (no bursting).
type ResourceLimits struct {
	// CPUCores is the reserved CPU cores (e.g. 0.5, 1, 2).
	CPUCores float64

	// CPUCoresMax is the hard CPU limit. Zero = same as CPUCores.
	CPUCoresMax float64

	// MemoryMB is the reserved memory in megabytes.
	MemoryMB int64

	// MemoryMBMax is the hard memory limit. Zero = same as MemoryMB.
	MemoryMBMax int64
}

// EffectiveCPUMax returns the hard CPU limit, defaulting to the
// reservation when no explicit max is set.
func (r ResourceLimits) EffectiveCPUMax() float64 {
	if r.CPUCoresMax > 0 {
		return r.CPUCoresMax
	}
	return r.CPUCores
}

// EffectiveMemoryMax returns the hard memory limit, defaulting to the
// reservation when no explicit max is set.
func (r ResourceLimits) EffectiveMemoryMax() int64 {
	if r.MemoryMBMax > 0 {
		return r.MemoryMBMax
	}
	return r.MemoryMB
}

// HealthCheckType selects the probe mechanism.
type HealthCheckType string

const (
	healthCheckHTTP HealthCheckType = "http"
	healthCheckTCP  HealthCheckType = "tcp"
)

type healthCheckTypeEnum struct{}

// HealthCheckTypeEnum is the public accessor for HealthCheckType values.
var HealthCheckTypeEnum healthCheckTypeEnum

func (healthCheckTypeEnum) HTTP() HealthCheckType { return healthCheckHTTP }
func (healthCheckTypeEnum) TCP() HealthCheckType  { return healthCheckTCP }

// HealthCheck defines how the runtime probes container liveness.
type HealthCheck struct {
	Type         HealthCheckType
	Path         string        // HTTP only — the GET path (e.g. "/health")
	Port         uint16        // Port to probe
	Interval     time.Duration // Time between probes (default 10s)
	Timeout      time.Duration // Max wait for a probe response (default 3s)
	Retries      int           // Consecutive failures before unhealthy (default 3)
	InitialDelay time.Duration // Grace period before first probe (default 5s)
}

// Stats is a point-in-time snapshot of container resource usage.
type Stats struct {
	Timestamp  time.Time
	CPUPercent float64 // 0–100 per core (e.g. 200% = 2 full cores)
	MemoryMB   int64   // Resident memory in MB
	MemoryMax  int64   // Memory limit in MB
	NetworkRx  int64   // Bytes received since start
	NetworkTx  int64   // Bytes transmitted since start
	DiskRead   int64   // Bytes read since start
	DiskWrite  int64   // Bytes written since start
}

// LogEntry is a single line from a container's stdout or stderr.
type LogEntry struct {
	Timestamp time.Time
	Stream    string // "stdout" or "stderr"
	Line      string
}

// LogRetention configures how container logs are stored on disk.
type LogRetention struct {
	MaxFileSizeMB int // Max size of a single log file (default 10)
	MaxFiles      int // Max number of rotated files (default 10)
}

// ContainerInfo describes the full state of a container as returned by
// Inspect. Mirrors the level of detail that `docker inspect` or
// `podman inspect` provides, scoped to what Falak needs.
type ContainerInfo struct {
	// Identity
	ID    string
	Name  string
	Image string

	// Lifecycle
	Status     ContainerStatus
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	Pid        int
	RestartCount int

	// Networking
	IP       string            // primary container IP (bridge mode); empty for host
	Ports    []PortMapping     // actual port mappings in effect
	Hostname string
	DNS      []string

	// Resources (actual limits applied by cgroups)
	CPULimit    float64 // cores
	MemoryLimit int64   // MB

	// Metadata
	Labels map[string]string
	Env    map[string]string
}

// --- Runtime interface ---------------------------------------------------

// Runtime is the container lifecycle interface. Implementations are
// backend-specific (Podman, containerd, gVisor, mock). The interface
// covers the full lifecycle: pull → create → start → stop → remove,
// plus checkpoint/restore for snapshot-based fast startup, and
// observability (stats + logs).
//
// All methods accept a context for cancellation and timeout. Container
// IDs are Falak-assigned strings (typically "<capsule_id>-<replica_id>").
type Runtime interface {
	// Pull fetches an OCI image from a registry. If the image is already
	// present locally, this is a no-op (unless force-pull is requested
	// via options). Registry credentials are passed via PullOption.
	Pull(ctx context.Context, image string, opts ...PullOption) error

	// Create sets up a container from the given image without starting
	// it. Network mode, port mappings, resource limits, environment
	// variables, and other configuration are passed via CreateOption.
	Create(ctx context.Context, id string, image string, opts ...CreateOption) error

	// Start begins execution of a previously created container.
	Start(ctx context.Context, id string) error

	// Stop sends SIGTERM, waits a grace period, then SIGKILL. The grace
	// period is passed via StopOption (default 10s).
	Stop(ctx context.Context, id string, opts ...StopOption) error

	// Remove deletes a stopped container and its associated resources.
	Remove(ctx context.Context, id string) error

	// Checkpoint captures the running container's state (memory, file
	// descriptors, network connections via CRIU) to the given path.
	// The container is stopped after checkpoint.
	Checkpoint(ctx context.Context, id string, snapshotPath string) error

	// Restore creates and starts a container from a previously captured
	// checkpoint at the given path.
	Restore(ctx context.Context, id string, snapshotPath string, opts ...RestoreOption) error

	// Stats returns a channel of periodic resource usage snapshots for
	// a running container. The channel is closed when the context is
	// cancelled or the container stops.
	Stats(ctx context.Context, id string) (<-chan Stats, error)

	// Logs returns a channel of log lines (stdout + stderr) from a
	// container. Supports follow mode (tail) via LogOption. The channel
	// is closed when the context is cancelled or the container stops.
	Logs(ctx context.Context, id string, opts ...LogOption) (<-chan LogEntry, error)

	// Inspect returns the current state of a container. When the backend
	// reports the container no longer exists, the returned error wraps
	// ErrContainerNotFound (test with errors.Is).
	Inspect(ctx context.Context, id string) (ContainerInfo, error)

	// Events streams container lifecycle events until ctx is cancelled or
	// the backend stream ends; the channel is closed on either. The
	// stream is best-effort (it can drop on a backend socket restart), so
	// consumers MUST pair it with a periodic reconcile sweep for
	// correctness. Backend-agnostic: Podman, containerd, and the mock
	// each implement it. ContainerID on each event is the Falak container
	// name, not the backend's hash.
	Events(ctx context.Context) (<-chan ContainerEvent, error)
}

// --- Functional options --------------------------------------------------

// PullOption configures an image pull operation.
type PullOption func(*PullConfig)

// PullConfig holds the resolved pull options. Implementations call
// ApplyPullOptions to build one from the variadic PullOption slice.
type PullConfig struct {
	Username  string
	Password  string
	ForcePull bool
}

// WithRegistryAuth sets registry credentials for pulling from a private
// registry.
func WithRegistryAuth(username, password string) PullOption {
	return func(c *PullConfig) {
		c.Username = username
		c.Password = password
	}
}

// WithForcePull forces re-pulling even if the image exists locally.
func WithForcePull() PullOption {
	return func(c *PullConfig) { c.ForcePull = true }
}

// ApplyPullOptions applies the given options and returns the resolved config.
func ApplyPullOptions(opts ...PullOption) PullConfig {
	var cfg PullConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// CreateOption configures container creation.
type CreateOption func(*CreateConfig)

// CreateConfig holds the resolved container creation options.
type CreateConfig struct {
	NetworkMode  NetworkMode
	Ports        []PortMapping
	Resources    ResourceLimits
	Env          map[string]string
	Labels       map[string]string
	WorkingDir   string
	Command      []string
	LogRetention LogRetention

	// DNSFlags carries opaque Podman create flags ("--dns=...",
	// "--dns-search=", "--dns-option=...") that the per-group network
	// manager injects so the container's /etc/resolv.conf points at the
	// link-local DNS responder (169.254.169.250). Empty for standalone
	// capsules.
	DNSFlags []string
}

// WithNetworkMode sets the container's network isolation mode.
func WithNetworkMode(mode NetworkMode) CreateOption {
	return func(c *CreateConfig) { c.NetworkMode = mode }
}

// WithPortMappings sets the container's port mappings.
func WithPortMappings(ports ...PortMapping) CreateOption {
	return func(c *CreateConfig) { c.Ports = ports }
}

// WithResourceLimits sets CPU and memory constraints.
func WithResourceLimits(limits ResourceLimits) CreateOption {
	return func(c *CreateConfig) { c.Resources = limits }
}

// WithEnv sets environment variables for the container.
func WithEnv(env map[string]string) CreateOption {
	return func(c *CreateConfig) { c.Env = env }
}

// WithLabels sets metadata labels on the container.
func WithLabels(labels map[string]string) CreateOption {
	return func(c *CreateConfig) { c.Labels = labels }
}

// WithWorkingDir sets the working directory inside the container.
func WithWorkingDir(dir string) CreateOption {
	return func(c *CreateConfig) { c.WorkingDir = dir }
}

// WithCommand overrides the container's entrypoint/command.
func WithCommand(cmd ...string) CreateOption {
	return func(c *CreateConfig) { c.Command = cmd }
}

// WithLogRetention sets the log rotation policy for the container.
func WithLogRetention(retention LogRetention) CreateOption {
	return func(c *CreateConfig) { c.LogRetention = retention }
}

// WithDNSFlags carries opaque Podman create flags ("--dns=...",
// "--dns-search=", "--dns-option=...") that the per-group network
// manager injects so the container's /etc/resolv.conf points at the
// link-local DNS responder. A nil or empty slice is a no-op: standalone
// capsules retain Podman's default networking. The flags are copied
// defensively so callers can reuse their slice.
func WithDNSFlags(flags []string) CreateOption {
	return func(c *CreateConfig) {
		if len(flags) == 0 {
			return
		}
		c.DNSFlags = append(c.DNSFlags[:0:0], flags...)
	}
}

// ApplyCreateOptions applies the given options and returns the resolved config.
func ApplyCreateOptions(opts ...CreateOption) CreateConfig {
	cfg := CreateConfig{
		NetworkMode: networkModeBridge,
		Env:         make(map[string]string),
		Labels:      make(map[string]string),
		LogRetention: LogRetention{
			MaxFileSizeMB: 10,
			MaxFiles:      10,
		},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// StopOption configures container stop behavior.
type StopOption func(*StopConfig)

// StopConfig holds the resolved stop options.
type StopConfig struct {
	GracePeriod time.Duration
}

// WithGracePeriod sets the time between SIGTERM and SIGKILL.
func WithGracePeriod(d time.Duration) StopOption {
	return func(c *StopConfig) { c.GracePeriod = d }
}

// ApplyStopOptions applies the given options and returns the resolved config.
func ApplyStopOptions(opts ...StopOption) StopConfig {
	cfg := StopConfig{GracePeriod: 10 * time.Second}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// RestoreOption configures container restore behavior.
type RestoreOption func(*RestoreConfig)

// RestoreConfig holds the resolved restore options.
type RestoreConfig struct {
	NetworkMode NetworkMode
	Ports       []PortMapping
	Env         map[string]string
}

// WithRestoreNetworkMode sets the network mode for the restored container.
func WithRestoreNetworkMode(mode NetworkMode) RestoreOption {
	return func(c *RestoreConfig) { c.NetworkMode = mode }
}

// WithRestorePortMappings sets port mappings for the restored container.
func WithRestorePortMappings(ports ...PortMapping) RestoreOption {
	return func(c *RestoreConfig) { c.Ports = ports }
}

// WithRestoreEnv sets environment variables for the restored container.
func WithRestoreEnv(env map[string]string) RestoreOption {
	return func(c *RestoreConfig) { c.Env = env }
}

// ApplyRestoreOptions applies the given options and returns the resolved config.
func ApplyRestoreOptions(opts ...RestoreOption) RestoreConfig {
	var cfg RestoreConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// LogOption configures log streaming.
type LogOption func(*LogConfig)

// LogConfig holds the resolved log options.
type LogConfig struct {
	Follow bool
	Since  time.Time
	Tail   int
	Stream string // "stdout", "stderr", or "" for both
}

// WithFollow enables follow mode (like tail -f).
func WithFollow() LogOption {
	return func(c *LogConfig) { c.Follow = true }
}

// WithSince only returns logs after the given timestamp.
func WithSince(t time.Time) LogOption {
	return func(c *LogConfig) { c.Since = t }
}

// WithTail returns only the last N lines.
func WithTail(n int) LogOption {
	return func(c *LogConfig) { c.Tail = n }
}

// WithStream filters to a specific stream ("stdout" or "stderr").
func WithStream(s string) LogOption {
	return func(c *LogConfig) { c.Stream = s }
}

// ApplyLogOptions applies the given options and returns the resolved config.
func ApplyLogOptions(opts ...LogOption) LogConfig {
	var cfg LogConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// --- VMManager interface -------------------------------------------------

// VMStatus represents the current state of a managed VM.
type VMStatus string

const (
	vmStatusRunning VMStatus = "running"
	vmStatusStopped VMStatus = "stopped"
	vmStatusFailed  VMStatus = "failed"
	vmStatusUnknown VMStatus = "unknown"
)

type vmStatusEnum struct{}

// VMStatusEnum is the public accessor for VMStatus values.
var VMStatusEnum vmStatusEnum

func (vmStatusEnum) Running() VMStatus { return vmStatusRunning }
func (vmStatusEnum) Stopped() VMStatus { return vmStatusStopped }
func (vmStatusEnum) Failed() VMStatus  { return vmStatusFailed }
func (vmStatusEnum) Unknown() VMStatus { return vmStatusUnknown }

// VMManager manages a lightweight Linux VM on non-Linux platforms
// (macOS, Windows). On Linux this interface is not used — the Runtime
// operates natively. VMManager is a separate interface so Linux nodes
// pay no abstraction cost.
type VMManager interface {
	// Create provisions a new VM with the given configuration.
	Create(ctx context.Context, opts ...VMOption) error

	// Start boots the VM. The Runtime can be used after Start returns.
	Start(ctx context.Context) error

	// Stop gracefully shuts down the VM.
	Stop(ctx context.Context) error

	// Health returns the current status of the VM.
	Health(ctx context.Context) (VMStatus, error)
}

// VMOption configures a VM.
type VMOption func(*VMConfig)

// VMConfig holds the resolved VM options.
type VMConfig struct {
	CPUs     int
	MemoryMB int64
	DiskGB   int64
	Name     string
}

// WithVMCPUs sets the number of virtual CPUs.
func WithVMCPUs(n int) VMOption {
	return func(c *VMConfig) { c.CPUs = n }
}

// WithVMMemory sets the VM memory in megabytes.
func WithVMMemory(mb int64) VMOption {
	return func(c *VMConfig) { c.MemoryMB = mb }
}

// WithVMDisk sets the VM disk size in gigabytes.
func WithVMDisk(gb int64) VMOption {
	return func(c *VMConfig) { c.DiskGB = gb }
}

// WithVMName sets the VM name.
func WithVMName(name string) VMOption {
	return func(c *VMConfig) { c.Name = name }
}

// ApplyVMOptions applies the given options and returns the resolved config.
func ApplyVMOptions(opts ...VMOption) VMConfig {
	cfg := VMConfig{
		CPUs:     2,
		MemoryMB: 2048,
		DiskGB:   20,
		Name:     "falak",
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

