// Falak CUE Schema Definitions
//
// Import this package in your configuration file to get
// schema validation, type checking, and IDE autocomplete.
//
// Usage:
//   import "github.com/tareksalem/falak/cue"
//
//   cue.#Node & {
//       name: "my-node"
//       ...
//   }

package cue

import "strings"

// #Node defines the complete configuration for a Falak node.
#Node: {
	// name is the unique identifier for this node.
	// Used for deterministic peer ID generation and data directory naming.
	name: string & strings.MinRunes(1)

	// port is the TCP port for libp2p communication.
	// 0 means a random available port will be chosen.
	port: (int & >=0 & <=65535) | *0

	// listen is a multiaddr string that overrides port.
	// Example: "/ip4/0.0.0.0/tcp/4001"
	listen?: string

	// data_dir is the path for persistent storage (phonebook, keys, certs).
	// Default: ~/.local/share/falak/<name> (Linux) or ~/Library/Application Support/falak/<name> (macOS)
	data_dir?: string

	// region identifies the geographic region of this node.
	region: string | *"default"

	// datacenter identifies the datacenter within the region.
	datacenter: string | *"default"

	// log_level controls the verbosity of logging output.
	log_level: "debug" | "info" | "warn" | "error" | *"info"

	// clusters defines the clusters this node should join.
	// Keys are cluster paths (e.g., "prod/dc1", "staging/dc2").
	// A node can join multiple clusters simultaneously.
	clusters: [string]: #Cluster

	// labels are key-value metadata for this node.
	// Used by capsule placement rules to target specific nodes.
	labels?: [string]: string

	// orbits is a list of orbits this node subscribes to.
	// The node will receive capsule announcements for these orbits.
	orbits?: [...string]

	// health configures the SWIM-based health monitoring protocol.
	// All values have sensible defaults and are dynamically scaled by cluster size.
	health?: #Health

	// metrics configures the node-level resource sampler that feeds the
	// election gravity calculator. Defaults: enabled, 1s interval.
	metrics?: #Metrics
}

// #Metrics configures the node resource sampler.
#Metrics: {
	// enabled toggles the entire subsystem. When false the collector
	// loop never starts and no PubSub messages are exchanged.
	enabled?: bool

	// interval is how often the collector samples local metrics and
	// publishes them to peers. Smaller values produce fresher data at
	// the cost of slightly more PubSub traffic. Minimum: 100ms.
	interval?: string
}

// #Cluster defines the configuration for joining a specific cluster.
#Cluster: {
	// psk is the Pre-Shared Key for cluster authentication.
	// Must be at least 32 characters. All nodes in a cluster share the same PSK.
	// PSK is used for HMAC challenge-response during the auth handshake.
	psk: string & strings.MinRunes(32)

	// bootstrap is a list of multiaddr strings for existing cluster nodes.
	// Required for joining an existing cluster. Empty for the first node.
	// Example: ["/ip4/10.0.0.1/tcp/4001/p2p/12D3KooW..."]
	bootstrap?: [...string]

	// certificates configures the PKI mode for this cluster.
	// Omit for auto mode (PSK-derived certificates).
	// Set for external CA mode (user-provided certificates).
	certificates?: #Certificates

	// capsules defines the capsules to create in this cluster after joining.
	// Keys are capsule names. Only this node needs to declare them — they'll
	// be gossiped to the rest of the mesh via the orbit.
	capsules?: [string]: #Capsule

	// election configures the election subsystem for this cluster: which
	// algorithm to use, timeouts, and gravity weight overrides. All fields
	// are optional; omitting the block uses built-in defaults (delay-based
	// algorithm, 10s election timeout, default weights).
	election?: #Election
}

// #Election configures the election subsystem for a cluster.
#Election: {
	// algorithm selects the election strategy for this cluster. Only
	// the "delay" strategy is supported: each eligible node waits
	// inversely proportional to its own gravity score then claims.
	// The field is retained for forward compatibility.
	algorithm?: "delay" | *"delay"

	// timeout is the upper bound on a single election round. Duration
	// string (e.g. "10s", "30s"). Default 10s.
	timeout?: string

	// weights overrides the built-in gravity factor weights. Only the
	// fields you set are overridden; others use the defaults. Useful
	// for tuning placement behavior in environments with unusual
	// constraints (e.g. memory-heavy workloads, latency-critical
	// clusters).
	weights?: #ElectionWeights
}

// #ElectionWeights overrides gravity factor weights at the cluster level.
// Every field is optional. All defaults are sensible for a general
// workload; only set fields you want to change.
#ElectionWeights: {
	cpu_headroom?:          number
	memory_headroom?:       number
	disk_headroom?:         number
	soft_placement_match?:  number
	affinity_proximity?:    number
	hardware_label_match?:  number
	load_penalty?:          number
	reliability?:           number
	diversity?:             number
}

// #Certificates configures external PKI for a cluster.
// When present, the cluster uses external CA mode instead of auto (PSK-derived).
// All paths are to PEM-encoded files.
#Certificates: {
	// ca_cert is the path to the CA certificate.
	// Required. All nodes in the cluster must use the same CA.
	ca_cert: string & strings.MinRunes(1)

	// ca_key is the path to the CA private key.
	// Optional. When set, this node can sign certificates for joining nodes (voucher mode).
	// When absent, all joining nodes must provide pre-signed certificates.
	ca_key?: string

	// node_cert is the path to this node's pre-signed certificate.
	// Optional. When set, this certificate is used instead of auto-generating one.
	node_cert?: string

	// node_key is the path to this node's private key.
	// Optional. When set, this key is used instead of auto-generating one.
	node_key?: string
}

// #Capsule defines a deployable application specification.
#Capsule: {
	// name is the unique identifier for this capsule.
	name: string & strings.MinRunes(1)

	// image is the container image reference.
	image: string & strings.MinRunes(1)

	// image_alias is the original tag the user provided (for display).
	image_alias?: string

	// image_digest is the resolved content-addressable digest (e.g., sha256:...).
	image_digest?: string

	// command overrides the container entrypoint. Empty = use image default.
	command?: [...string]

	// orbit is the flat-named topic where this capsule travels.
	orbit: string & strings.MinRunes(1)

	// tier sets the priority level. Determines default momentum.
	// critical=90, standard=50, background=20.
	tier: "critical" | "standard" | "background" | *"standard"

	// labels are key-value metadata for this capsule.
	labels?: [string]: string

	// resources defines minimum resource requirements.
	resources?: #Resources

	// replicas defines how many instances to run.
	replicas?: #Replicas

	// scaling defines autoscaling rules.
	scaling?: #Scaling

	// placement defines where the capsule should be deployed.
	placement?: [...#PlacementRule]

	// runtime configures the container execution.
	runtime?: #Runtime

	// advanced contains optional momentum tuning.
	advanced?: #Advanced
}

// #Resources defines resource constraints for a capsule.
// Reservation fields are the guaranteed minimum (used by gravity).
// Max fields are hard ceilings (container throttled/OOM-killed).
#Resources: {
	cpu?:        int & >=0
	cpu_max?:    int & >=0
	memory?:     string   // e.g., "512MB", "2GB"
	memory_max?: string
	disk?:       string   // e.g., "1GB"
}

// #Replicas defines how many instances of a capsule to run.
#Replicas: {
	// Use min/max for autoscaling.
	min?: int & >=0
	max?: int & >=0

	// OR use exact for a fixed count.
	exact?: int & >=0
}

// #Scaling defines autoscaling rules for a capsule.
#Scaling: {
	rules: [...#ScalingRule]
}

// #ScalingRule defines a named group of conditions that trigger scaling.
#ScalingRule: {
	// name identifies this rule (shown in logs/alerts).
	name: string & strings.MinRunes(1)

	// trigger determines how conditions are evaluated.
	// "all" = all must be true; "any" = any one triggers.
	trigger: "any" | "all" | *"all"

	// conditions are metric expressions (e.g., "cpu > 70%", "rps > 1000").
	conditions: [...string] & [_, ...]

	// action is what happens when conditions match.
	action: "scaleUp" | "scaleDown" | "scaleToZero"

	// cooldown prevents flapping. Duration string (e.g., "60s", "5m").
	cooldown?: string
}

// #PlacementRule defines where a capsule should be deployed.
#PlacementRule: {
	// name describes this rule (for logs and error messages).
	name?: string

	// type is the entity being targeted.
	type: "node" | "cluster" | "datacenter" | "capsule"

	// mode is only for type=capsule. Determines affinity direction.
	mode?: "near" | "away"

	// targets selects entities by name. Combined with labels (if present),
	// both must match.
	targets?: [...string]

	// labels selects entities by labels.
	// For type=capsule with near/away, use "same" as value to compare.
	labels?: [string]: string

	// required determines if this is a hard or soft rule. Default: true.
	required?: bool
}

// #Runtime configures capsule container execution.
#Runtime: {
	// env sets plain environment variables for the container.
	env?: [string]: string

	// network configures the container's network isolation.
	network?: #NetworkConfig

	// health_check configures container liveness probing.
	health_check?: #HealthCheck

	// failure_policy controls restart and re-election behavior.
	failure_policy?: #FailurePolicy

	// log_retention configures container log rotation on disk.
	log_retention?: #LogRetention

	// stats_interval sets how often container metrics are sampled.
	// Duration string (e.g., "5s", "10s"). Default "5s".
	stats_interval?: string

	// registry sets private registry credentials (encrypted with SEK).
	registry?: #Registry

	// snapshot configures per-capsule snapshot behavior.
	snapshot?: #SnapshotConfig
}

// #NetworkConfig configures container network isolation.
#NetworkConfig: {
	// mode selects the network model.
	mode: "bridge" | "host" | *"bridge"

	// ports defines port mappings (bridge mode only).
	ports?: [...#PortMapping]
}

// #PortMapping maps a container port to a host port.
#PortMapping: {
	name?:     string
	container: int & >0 & <=65535
	host?:     int & >=0 & <=65535  // 0 = auto-assign
	protocol:  "tcp" | "udp" | *"tcp"
}

// #HealthCheck configures container liveness probing.
#HealthCheck: {
	// type selects the probe mechanism.
	type: "http" | "tcp"

	// path is the HTTP GET path (http type only).
	path?: string

	// port is the port to probe.
	port: int & >0 & <=65535

	// interval is the time between probes. Default "10s".
	interval?: string

	// timeout is the max wait per probe. Default "3s".
	timeout?: string

	// retries is consecutive failures before unhealthy. Default 3.
	retries?: int & >=1

	// initial_delay is the grace period before first probe. Default "5s".
	initial_delay?: string
}

// #FailurePolicy controls restart and re-election behavior on failure.
#FailurePolicy: {
	// restart_limit is max local restarts before re-electing. Default 3.
	restart_limit?: int & >=0

	// max_node_attempts is max different nodes to try. Default 3.
	max_node_attempts?: int & >=1

	// graceful_timeout is SIGTERM → SIGKILL grace period. Default "10s".
	graceful_timeout?: string
}

// #LogRetention configures container log rotation.
#LogRetention: {
	// max_file_size_mb is the max size of a single log file. Default 10.
	max_file_size_mb?: int & >=1

	// max_files is the max number of rotated files. Default 10.
	max_files?: int & >=1
}

// #Registry holds private registry credentials.
#Registry: {
	url:      string
	username: string
	password: string
}

// #SnapshotConfig configures per-capsule snapshot behavior.
#SnapshotConfig: {
	// max_per_capsule is the max snapshot tags kept. Default 3.
	max_per_capsule?: int & >=1

	// ttl is the snapshot expiry. Duration string. Default "72h".
	ttl?: string
}

// #Advanced contains optional momentum tuning for power users.
#Advanced: {
	momentum?: #MomentumConfig
}

// #MomentumConfig provides fine-grained control over capsule priority.
#MomentumConfig: {
	// base overrides the tier default (0-100).
	base?: int & >=0 & <=100

	// boost_on_traffic increases momentum when the capsule receives traffic.
	boost_on_traffic?: bool

	// reduce_on_idle decreases momentum when the capsule is idle.
	reduce_on_idle?: bool

	// idle_timeout is how long before idle reduction kicks in (e.g., "5m").
	idle_timeout?: string
}

// #Health configures the SWIM-based health monitoring protocol.
// All fields are optional with sensible defaults.
// Thresholds are dynamically scaled by cluster size at runtime.
#Health: {
	// protocol_period is how often each node probes a random peer.
	// Smaller values detect failures faster but increase network traffic.
	protocol_period: string | *"2s"

	// ping_timeout is the maximum time to wait for a ping response.
	ping_timeout: string | *"500ms"

	// quarantine_timeout is how long a node stays quarantined before being removed.
	// This is the base value — dynamically scaled by cluster size.
	quarantine_timeout: string | *"30s"

	// score_increment is the score added per failed probe.
	score_increment: float | *0.5

	// suspected_threshold is the base score to enter the suspected state.
	// Dynamically scaled: smaller clusters use lower effective thresholds.
	suspected_threshold: float | *4.0

	// quarantine_threshold is the base score to trigger quarantine.
	// Dynamically scaled: smaller clusters use lower effective thresholds.
	quarantine_threshold: float | *10.0

	// max_responders is how many longest-lived nodes self-select for indirect probing.
	max_responders: (int & >=1) | *2

	// quarantine_check_interval is how often to check quarantined nodes for timeout.
	quarantine_check_interval: string | *"5s"

	// quarantine_probe_interval is how often to publish probe requests for quarantined peers.
	quarantine_probe_interval: string | *"10s"
}
