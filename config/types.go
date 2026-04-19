// Package config provides configuration types and CUE-based config loading for Falak nodes.
//
// The Go structs in this package are the stable contract between the CUE config
// file and the rest of the codebase. The CUE schema (in cue/falak.cue) validates
// user config, and the loader decodes it into these structs.
package config

// NodeConfig is the top-level configuration for a Falak node.
type NodeConfig struct {
	// Name is the unique identifier for this node.
	Name string `json:"name"`

	// Port is the TCP port for libp2p communication. 0 = random.
	Port int `json:"port"`

	// Listen is a multiaddr string that overrides Port.
	Listen string `json:"listen,omitempty"`

	// DataDir is the path for persistent storage.
	DataDir string `json:"data_dir,omitempty"`

	// Region identifies the geographic region.
	Region string `json:"region"`

	// Datacenter identifies the datacenter within the region.
	Datacenter string `json:"datacenter"`

	// LogLevel controls logging verbosity.
	LogLevel string `json:"log_level"`

	// Labels are key-value metadata for this node. Used by capsule placement rules.
	Labels map[string]string `json:"labels,omitempty"`

	// Orbits is a list of orbits this node subscribes to when joining clusters.
	Orbits []string `json:"orbits,omitempty"`

	// Clusters defines the clusters this node should join.
	// Keys are cluster paths (e.g., "prod/dc1").
	Clusters map[string]ClusterConfig `json:"clusters"`

	// Health configures the SWIM-based health monitoring protocol.
	Health *HealthConfig `json:"health,omitempty"`

	// Metrics configures the node resource sampler that feeds the
	// election gravity calculator.
	Metrics *MetricsConfig `json:"metrics,omitempty"`
}

// MetricsConfig configures the node resource sampler.
type MetricsConfig struct {
	// Enabled toggles the entire subsystem. When false no metrics are
	// collected, stored, or gossiped. nil pointer means use default (true).
	Enabled *bool `json:"enabled,omitempty"`

	// Interval is how often the collector samples and publishes. Duration
	// string (e.g. "1s", "500ms"). Minimum 100ms.
	Interval string `json:"interval,omitempty"`
}

// ClusterConfig defines the configuration for joining a specific cluster.
type ClusterConfig struct {
	// PSK is the Pre-Shared Key for cluster authentication (≥32 bytes).
	PSK string `json:"psk"`

	// Bootstrap is a list of multiaddr strings for existing cluster nodes.
	Bootstrap []string `json:"bootstrap,omitempty"`

	// Certificates configures external PKI mode. Nil = auto mode.
	Certificates *CertificatesConfig `json:"certificates,omitempty"`

	// Capsules defines the capsules to create in this cluster after joining.
	// Keys are the capsule name; values are the spec.
	Capsules map[string]CapsuleConfig `json:"capsules,omitempty"`

	// Election configures the election subsystem for this cluster: which
	// strategy to use, the election timeout, and gravity factor weight
	// overrides. Omit to use built-in defaults.
	Election *ElectionConfig `json:"election,omitempty"`
}

// ElectionConfig configures per-cluster election behavior.
type ElectionConfig struct {
	// Algorithm selects the election strategy. Only "delay" is supported;
	// any other value falls back to "delay" with a warning log. The field
	// is retained for forward compatibility.
	Algorithm string `json:"algorithm,omitempty"`

	// Timeout is the upper bound on a single election round. Parsed as
	// a Go duration string (e.g. "10s", "30s"). Empty means use the
	// default (10s).
	Timeout string `json:"timeout,omitempty"`

	// Weights overrides the built-in gravity factor weights for this
	// cluster. Fields not set inherit the defaults — no need to
	// specify every factor.
	Weights *ElectionWeights `json:"weights,omitempty"`
}

// ElectionWeights holds per-cluster gravity weight overrides. Every
// field is optional; set only the ones you want to change.
type ElectionWeights struct {
	CPUHeadroom        float64 `json:"cpu_headroom,omitempty"`
	MemoryHeadroom     float64 `json:"memory_headroom,omitempty"`
	DiskHeadroom       float64 `json:"disk_headroom,omitempty"`
	SoftPlacementMatch float64 `json:"soft_placement_match,omitempty"`
	AffinityProximity  float64 `json:"affinity_proximity,omitempty"`
	HardwareLabelMatch float64 `json:"hardware_label_match,omitempty"`
	LoadPenalty        float64 `json:"load_penalty,omitempty"`
	Reliability        float64 `json:"reliability,omitempty"`
	Diversity          float64 `json:"diversity,omitempty"`
}

// CapsuleConfig is the declarative capsule spec loaded from CUE configuration.
// It mirrors the #Capsule schema in cue/falak.cue and is converted to a
// capsule.CapsuleSpec at node startup.
type CapsuleConfig struct {
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	ImageAlias  string            `json:"image_alias,omitempty"`
	ImageDigest string            `json:"image_digest,omitempty"`
	Orbit       string            `json:"orbit"`
	Tier        string            `json:"tier,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Resources   *ResourcesConfig  `json:"resources,omitempty"`
	Replicas    *ReplicasConfig   `json:"replicas,omitempty"`
	Scaling     *ScalingConfig    `json:"scaling,omitempty"`
	Placement   []PlacementConfig `json:"placement,omitempty"`
	Runtime     *RuntimeCfg       `json:"runtime,omitempty"`
	Advanced    *AdvancedConfig   `json:"advanced,omitempty"`
}

// ResourcesConfig defines capsule resource constraints.
type ResourcesConfig struct {
	CPU       int    `json:"cpu,omitempty"`
	CPUMax    int    `json:"cpu_max,omitempty"`
	Memory    string `json:"memory,omitempty"`
	MemoryMax string `json:"memory_max,omitempty"`
	Disk      string `json:"disk,omitempty"`
}

// ReplicasConfig defines how many capsule replicas to run.
type ReplicasConfig struct {
	Min   int `json:"min,omitempty"`
	Max   int `json:"max,omitempty"`
	Exact int `json:"exact,omitempty"`
}

// ScalingConfig groups scaling rules for a capsule.
type ScalingConfig struct {
	Rules []ScalingRuleConfig `json:"rules,omitempty"`
}

// ScalingRuleConfig defines a named group of scaling conditions.
type ScalingRuleConfig struct {
	Name       string   `json:"name"`
	Trigger    string   `json:"trigger,omitempty"` // "any" | "all"
	Conditions []string `json:"conditions"`
	Action     string   `json:"action"` // "scaleUp" | "scaleDown" | "scaleToZero"
	Cooldown   string   `json:"cooldown,omitempty"`
}

// PlacementConfig defines a placement rule for capsule deployment.
type PlacementConfig struct {
	Name     string            `json:"name,omitempty"` // rule description
	Type     string            `json:"type"`           // node | cluster | datacenter | capsule
	Mode     string            `json:"mode,omitempty"`
	Targets  []string          `json:"targets,omitempty"` // entity names to match
	Labels   map[string]string `json:"labels,omitempty"`
	Required *bool             `json:"required,omitempty"`
}

// RuntimeCfg defines capsule runtime settings (named RuntimeCfg to
// avoid collision with the runtime package import).
type RuntimeCfg struct {
	Env           map[string]string  `json:"env,omitempty"`
	Network       *NetworkCfg        `json:"network,omitempty"`
	HealthCheck   *HealthCheckCfg    `json:"health_check,omitempty"`
	FailurePolicy *FailurePolicyCfg  `json:"failure_policy,omitempty"`
	LogRetention  *LogRetentionCfg   `json:"log_retention,omitempty"`
	StatsInterval string             `json:"stats_interval,omitempty"`
	Registry      *RegistryCfg       `json:"registry,omitempty"`
	Snapshot      *SnapshotCfg       `json:"snapshot,omitempty"`
}

// NetworkCfg configures container network isolation.
type NetworkCfg struct {
	Mode  string       `json:"mode,omitempty"` // "bridge" | "host"
	Ports []PortMapCfg `json:"ports,omitempty"`
}

// PortMapCfg maps a container port to a host port.
type PortMapCfg struct {
	Name      string `json:"name,omitempty"`
	Container int    `json:"container"`
	Host      int    `json:"host,omitempty"`
	Protocol  string `json:"protocol,omitempty"` // "tcp" | "udp"
}

// HealthCheckCfg configures container liveness probing.
type HealthCheckCfg struct {
	Type         string `json:"type"`                    // "http" | "tcp"
	Path         string `json:"path,omitempty"`          // http only
	Port         int    `json:"port"`
	Interval     string `json:"interval,omitempty"`      // default "10s"
	Timeout      string `json:"timeout,omitempty"`       // default "3s"
	Retries      int    `json:"retries,omitempty"`       // default 3
	InitialDelay string `json:"initial_delay,omitempty"` // default "5s"
}

// FailurePolicyCfg controls restart and re-election on failure.
type FailurePolicyCfg struct {
	RestartLimit    int    `json:"restart_limit,omitempty"`     // default 3
	MaxNodeAttempts int    `json:"max_node_attempts,omitempty"` // default 3
	GracefulTimeout string `json:"graceful_timeout,omitempty"`  // default "10s"
}

// LogRetentionCfg configures container log rotation.
type LogRetentionCfg struct {
	MaxFileSizeMB int `json:"max_file_size_mb,omitempty"` // default 10
	MaxFiles      int `json:"max_files,omitempty"`        // default 10
}

// RegistryCfg holds private registry credentials.
type RegistryCfg struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// SnapshotCfg configures per-capsule snapshot behavior.
type SnapshotCfg struct {
	MaxPerCapsule int    `json:"max_per_capsule,omitempty"` // default 3
	TTL           string `json:"ttl,omitempty"`             // default "72h"
}

// AdvancedConfig holds optional advanced capsule tuning.
type AdvancedConfig struct {
	Momentum *MomentumConfig `json:"momentum,omitempty"`
}

// MomentumConfig provides fine-grained momentum control.
type MomentumConfig struct {
	Base           int    `json:"base,omitempty"`
	BoostOnTraffic bool   `json:"boost_on_traffic,omitempty"`
	ReduceOnIdle   bool   `json:"reduce_on_idle,omitempty"`
	IdleTimeout    string `json:"idle_timeout,omitempty"`
}

// CertificatesConfig configures external PKI for a cluster.
type CertificatesConfig struct {
	// CACert is the path to the CA certificate (PEM).
	CACert string `json:"ca_cert"`

	// CAKey is the path to the CA private key (PEM). Optional.
	CAKey string `json:"ca_key,omitempty"`

	// NodeCert is the path to a pre-signed node certificate (PEM). Optional.
	NodeCert string `json:"node_cert,omitempty"`

	// NodeKey is the path to the node's private key (PEM). Optional.
	NodeKey string `json:"node_key,omitempty"`
}

// HealthConfig configures the SWIM-based health monitoring protocol.
type HealthConfig struct {
	// ProtocolPeriod is how often each node probes a random peer.
	ProtocolPeriod string `json:"protocol_period,omitempty"`

	// PingTimeout is the max time to wait for a ping response.
	PingTimeout string `json:"ping_timeout,omitempty"`

	// QuarantineTimeout is how long a node stays quarantined before removal.
	QuarantineTimeout string `json:"quarantine_timeout,omitempty"`

	// ScoreIncrement is the score added per failed probe.
	ScoreIncrement float64 `json:"score_increment,omitempty"`

	// SuspectedThreshold is the base score to enter suspected state.
	SuspectedThreshold float64 `json:"suspected_threshold,omitempty"`

	// QuarantineThreshold is the base score to trigger quarantine.
	QuarantineThreshold float64 `json:"quarantine_threshold,omitempty"`

	// MaxResponders is how many longest-lived nodes self-select for indirect probing.
	MaxResponders int `json:"max_responders,omitempty"`

	// QuarantineCheckInterval is how often to check quarantined nodes for timeout.
	QuarantineCheckInterval string `json:"quarantine_check_interval,omitempty"`

	// QuarantineProbeInterval is how often to publish probe requests for quarantined peers.
	QuarantineProbeInterval string `json:"quarantine_probe_interval,omitempty"`
}
