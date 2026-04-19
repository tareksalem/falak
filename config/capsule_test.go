package config

import (
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
)

func TestLoadFromString_WithCapsules(t *testing.T) {
	cfg, err := LoadFromString(`
		name: "test-node"
		port: 4001
		clusters: {
			"test/dc1": {
				psk: "test-psk-must-be-at-least-32-chars"
				capsules: {
					"web-api": {
						name:  "web-api"
						image: "registry.test/web-api:v1"
						orbit: "api"
						tier:  "standard"
						labels: {
							app:  "web-api"
							team: "backend"
						}
						resources: {
							cpu:    2
							memory: "512MB"
							disk:   "1GB"
						}
						replicas: {
							min: 2
							max: 5
						}
						scaling: rules: [
							{
								name: "high load"
								trigger: "all"
								conditions: ["cpu > 70%", "memory > 60%"]
								action: "scaleUp"
								cooldown: "60s"
							},
						]
						placement: [
							{
								name: "gpu nodes"
								type: "node"
								labels: gpu: "true"
							},
						]
					}
				}
			}
		}
	`)
	if err != nil {
		t.Fatalf("LoadFromString failed: %v", err)
	}

	cluster, ok := cfg.Clusters["test/dc1"]
	if !ok {
		t.Fatal("cluster not found")
	}
	if len(cluster.Capsules) != 1 {
		t.Fatalf("expected 1 capsule, got %d", len(cluster.Capsules))
	}

	cc, ok := cluster.Capsules["web-api"]
	if !ok {
		t.Fatal("capsule web-api not found")
	}

	if cc.Name != "web-api" {
		t.Errorf("name: got %q", cc.Name)
	}
	if cc.Image != "registry.test/web-api:v1" {
		t.Errorf("image: got %q", cc.Image)
	}
	if cc.Tier != "standard" {
		t.Errorf("tier: got %q", cc.Tier)
	}
	if cc.Resources == nil || cc.Resources.CPU != 2 {
		t.Errorf("resources.cpu: got %+v", cc.Resources)
	}
	if cc.Replicas == nil || cc.Replicas.Min != 2 || cc.Replicas.Max != 5 {
		t.Errorf("replicas: got %+v", cc.Replicas)
	}
	if cc.Scaling == nil || len(cc.Scaling.Rules) != 1 {
		t.Errorf("scaling rules: got %+v", cc.Scaling)
	}
	if len(cc.Placement) != 1 {
		t.Errorf("placement: got %+v", cc.Placement)
	}
}

func TestCapsuleConfig_ToCapsuleSpec(t *testing.T) {
	required := true
	cc := CapsuleConfig{
		Name:  "test-api",
		Image: "test:v1",
		Orbit: "api",
		Tier:  "critical",
		Labels: map[string]string{
			"app": "test",
		},
		Resources: &ResourcesConfig{
			CPU:    4,
			Memory: "2GB",
			Disk:   "10GB",
		},
		Replicas: &ReplicasConfig{
			Min: 3,
			Max: 10,
		},
		Scaling: &ScalingConfig{
			Rules: []ScalingRuleConfig{
				{
					Name:       "high cpu",
					Trigger:    "all",
					Conditions: []string{"cpu > 80%"},
					Action:     "scaleUp",
					Cooldown:   "90s",
				},
			},
		},
		Placement: []PlacementConfig{
			{
				Name:     "gpu nodes",
				Type:     "node",
				Labels:   map[string]string{"gpu": "true"},
				Required: &required,
			},
		},
		Runtime: &RuntimeCfg{
			Env: map[string]string{
				"LOG_LEVEL": "info",
			},
			Network: &NetworkCfg{
				Mode: "bridge",
				Ports: []PortMapCfg{
					{Name: "http", Container: 8080, Host: 8080},
				},
			},
			HealthCheck: &HealthCheckCfg{
				Type: "http",
				Path: "/health",
				Port: 8080,
				Interval: "10s",
				Timeout:  "3s",
			},
		},
		Advanced: &AdvancedConfig{
			Momentum: &MomentumConfig{
				Base:           95,
				BoostOnTraffic: true,
				ReduceOnIdle:   true,
				IdleTimeout:    "5m",
			},
		},
	}

	spec, err := cc.ToCapsuleSpec()
	if err != nil {
		t.Fatalf("ToCapsuleSpec failed: %v", err)
	}

	if spec.Name != "test-api" {
		t.Errorf("name: got %q", spec.Name)
	}
	if spec.Tier != capsule.TierEnum.Critical() {
		t.Errorf("tier: got %q", spec.Tier)
	}
	if spec.Resources.CPUCores != 4 {
		t.Errorf("cpu: got %d", spec.Resources.CPUCores)
	}
	if spec.Resources.MemoryMB != 2048 {
		t.Errorf("memory MB: got %d, want 2048", spec.Resources.MemoryMB)
	}
	if spec.Resources.DiskMB != 10240 {
		t.Errorf("disk MB: got %d, want 10240", spec.Resources.DiskMB)
	}
	if spec.Replicas.Min != 3 || spec.Replicas.Max != 10 {
		t.Errorf("replicas: %+v", spec.Replicas)
	}
	if len(spec.ScalingRules) != 1 {
		t.Fatalf("scaling rules: %+v", spec.ScalingRules)
	}
	if spec.ScalingRules[0].Cooldown != 90*time.Second {
		t.Errorf("cooldown: got %v", spec.ScalingRules[0].Cooldown)
	}
	if len(spec.PlacementRules) != 1 || !spec.PlacementRules[0].Required {
		t.Errorf("placement: %+v", spec.PlacementRules)
	}
	if spec.Runtime.Env == nil {
		t.Error("runtime env should not be nil")
	}
	if spec.MomentumConfig.Base != 95 {
		t.Errorf("momentum base: %d", spec.MomentumConfig.Base)
	}
	if spec.MomentumConfig.IdleTimeout != 5*time.Minute {
		t.Errorf("momentum idle timeout: %v", spec.MomentumConfig.IdleTimeout)
	}
}

func TestCapsuleConfig_ToCapsuleSpec_ValidatesWithManager(t *testing.T) {
	cc := CapsuleConfig{
		Name:  "valid",
		Image: "img:v1",
		Orbit: "api",
	}
	spec, err := cc.ToCapsuleSpec()
	if err != nil {
		t.Fatalf("ToCapsuleSpec failed: %v", err)
	}

	// The converted spec should pass validation.
	capsule.DefaultSpec(&spec)
	if err := capsule.ValidateSpec(&spec); err != nil {
		t.Errorf("spec validation failed: %v", err)
	}
}

func TestParseSizeToMB(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
		wantErr  bool
	}{
		{"", 0, false},
		{"100", 100, false}, // bare number is MB
		{"512MB", 512, false},
		{"2GB", 2048, false},
		{"1TB", 1024 * 1024, false},
		{"1024KB", 1, false},
		{"invalid", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseSizeToMB(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.expected {
				t.Errorf("got %d, want %d", got, tt.expected)
			}
		})
	}
}
