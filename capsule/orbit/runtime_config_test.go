package orbit

import (
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/enums"
)

// TestSpecToProto_CarriesFullRuntimeConfig guards the gossip serialization gap
// that left non-origin nodes with no runtime config: specToProto once wrote
// only Env, so a capsule gossiped from its origin node reached every other node
// stripped of its Network.Ports (and health check, failure policy, snapshot
// config, ...). Replicas placed on those nodes then cold-started with no
// published host port — the "only 1 of N replicas has a port" bug. Every field
// set below must survive the encode.
func TestSpecToProto_CarriesFullRuntimeConfig(t *testing.T) {
	spec := &capsule.CapsuleSpec{
		Name:  "api",
		Image: "docker.io/library/nginx:alpine",
		Orbit: "default",
		Runtime: capsule.RuntimeConfig{
			Env:           map[string]string{"K": "V"},
			StatsInterval: 7 * time.Second,
			Network: capsule.NetworkConfig{
				Mode: enums.NetworkModeEnum.Bridge(),
				Ports: []capsule.PortMapping{
					{Name: "http", ContainerPort: 80, HostPort: 0, Protocol: "tcp"},
					{Name: "grpc", ContainerPort: 50051, HostPort: 18080, Protocol: "tcp"},
				},
			},
			HealthCheck: &capsule.HealthCheck{
				Type: enums.HealthCheckTypeEnum.HTTP(), Path: "/health", Port: 80,
				Interval: 10 * time.Second, Timeout: 3 * time.Second, Retries: 3,
				InitialDelay: 5 * time.Second,
			},
			FailurePolicy: capsule.FailurePolicy{
				RestartLimit: 3, MaxNodeAttempts: 2, GracefulTimeout: 10 * time.Second,
			},
			LogRetention:   capsule.LogRetention{MaxFileSizeMB: 10, MaxFiles: 5},
			SnapshotConfig: capsule.SnapshotConfig{MaxPerCapsule: 3, TTL: 72 * time.Hour},
			Registry: &capsule.RegistryAuth{
				URL: "ghcr.io", UsernameEncrypted: []byte("u"), PasswordEncrypted: []byte("p"),
			},
		},
	}

	pb := specToProto(spec)
	if pb.Runtime == nil {
		t.Fatal("Runtime is nil")
	}
	rt := pb.Runtime

	if rt.Network == nil || len(rt.Network.Ports) != 2 {
		t.Fatalf("Network.Ports not carried: %+v", rt.Network)
	}
	if rt.Network.Mode != "bridge" {
		t.Errorf("Network.Mode = %q, want bridge", rt.Network.Mode)
	}
	if p := rt.Network.Ports[0]; p.Name != "http" || p.ContainerPort != 80 || p.HostPort != 0 || p.Protocol != "tcp" {
		t.Errorf("port[0] = %+v, want http/80/0/tcp", p)
	}
	if p := rt.Network.Ports[1]; p.ContainerPort != 50051 || p.HostPort != 18080 {
		t.Errorf("port[1] = %+v, want container 50051 host 18080", p)
	}
	if rt.StatsIntervalSeconds != 7 {
		t.Errorf("StatsIntervalSeconds = %d, want 7", rt.StatsIntervalSeconds)
	}
	if rt.HealthCheck == nil || rt.HealthCheck.Type != "http" || rt.HealthCheck.Path != "/health" ||
		rt.HealthCheck.IntervalSeconds != 10 || rt.HealthCheck.Retries != 3 {
		t.Errorf("HealthCheck not carried: %+v", rt.HealthCheck)
	}
	if rt.FailurePolicy == nil || rt.FailurePolicy.RestartLimit != 3 ||
		rt.FailurePolicy.MaxNodeAttempts != 2 || rt.FailurePolicy.GracefulTimeoutSeconds != 10 {
		t.Errorf("FailurePolicy not carried: %+v", rt.FailurePolicy)
	}
	if rt.LogRetention == nil || rt.LogRetention.MaxFileSizeMb != 10 || rt.LogRetention.MaxFiles != 5 {
		t.Errorf("LogRetention not carried: %+v", rt.LogRetention)
	}
	if rt.Snapshot == nil || rt.Snapshot.MaxPerCapsule != 3 || rt.Snapshot.TtlSeconds != int32((72*time.Hour).Seconds()) {
		t.Errorf("Snapshot not carried: %+v", rt.Snapshot)
	}
	if rt.Registry == nil || rt.Registry.Url != "ghcr.io" ||
		string(rt.Registry.UsernameEncrypted) != "u" || string(rt.Registry.PasswordEncrypted) != "p" {
		t.Errorf("Registry not carried: %+v", rt.Registry)
	}
}
