package shared

import (
	"fmt"
	"strings"
)

const (
	// DefaultRegion is used when no region is specified
	DefaultRegion = "default"

	// DefaultDatacenter is used when no datacenter is specified
	DefaultDatacenter = "default"
)

// ClusterPath represents a parsed cluster path.
type ClusterPath struct {
	Region     string
	Datacenter string
	Cluster    string
}

// String returns the cluster path as a string.
func (cp ClusterPath) String() string {
	return fmt.Sprintf("%s/%s/%s", cp.Region, cp.Datacenter, cp.Cluster)
}

// ParseClusterPath parses a cluster path string into its components.
// Format: <region>/<datacenter>/<cluster>
// Returns error if path is invalid.
func ParseClusterPath(path string) (ClusterPath, error) {
	parts := strings.Split(path, "/")
	if len(parts) != 3 {
		return ClusterPath{}, fmt.Errorf("invalid cluster path %q: expected format region/datacenter/cluster", path)
	}

	if parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ClusterPath{}, fmt.Errorf("invalid cluster path %q: empty component", path)
	}

	return ClusterPath{
		Region:     parts[0],
		Datacenter: parts[1],
		Cluster:    parts[2],
	}, nil
}

// ExtractRegion extracts the region from a cluster path.
// Returns empty string if path is invalid.
func ExtractRegion(clusterPath string) string {
	cp, err := ParseClusterPath(clusterPath)
	if err != nil {
		return ""
	}
	return cp.Region
}

// ExtractDatacenter extracts the datacenter from a cluster path.
// Returns empty string if path is invalid.
func ExtractDatacenter(clusterPath string) string {
	cp, err := ParseClusterPath(clusterPath)
	if err != nil {
		return ""
	}
	return cp.Datacenter
}

// ExtractCluster extracts the cluster name from a cluster path.
// Returns empty string if path is invalid.
func ExtractCluster(clusterPath string) string {
	cp, err := ParseClusterPath(clusterPath)
	if err != nil {
		return ""
	}
	return cp.Cluster
}

// BuildClusterPath builds a cluster path from components.
func BuildClusterPath(region, datacenter, cluster string) string {
	if region == "" {
		region = DefaultRegion
	}
	if datacenter == "" {
		datacenter = DefaultDatacenter
	}
	return fmt.Sprintf("%s/%s/%s", region, datacenter, cluster)
}

// BuildHealthTopic builds the health PubSub topic for a cluster.
func BuildHealthTopic(clusterPath string) string {
	return fmt.Sprintf("falak/%s/health", clusterPath)
}

// BuildAuthTopic builds the auth PubSub topic for a cluster.
func BuildAuthTopic(clusterPath string) string {
	return fmt.Sprintf("falak/%s/auth", clusterPath)
}

// BuildMetricsTopic builds the resource metrics PubSub topic for a cluster.
// Used by the node metrics module to gossip ResourceUpdate messages without
// colliding with the health topic (which is already joined by the SWIM
// monitor and cannot be re-joined by another consumer in libp2p-pubsub).
func BuildMetricsTopic(clusterPath string) string {
	return fmt.Sprintf("falak/%s/metrics", clusterPath)
}
