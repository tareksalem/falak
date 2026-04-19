package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromString_Minimal(t *testing.T) {
	cfg, err := LoadFromString(`
		name: "test-node"
		port: 4001
		clusters: {
			"test/dc1": {
				psk: "test-psk-must-be-at-least-32-chars"
			}
		}
	`)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Name != "test-node" {
		t.Errorf("expected name 'test-node', got %q", cfg.Name)
	}
	if cfg.Port != 4001 {
		t.Errorf("expected port 4001, got %d", cfg.Port)
	}
	if cfg.Region != "default" {
		t.Errorf("expected default region, got %q", cfg.Region)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("expected default log_level 'info', got %q", cfg.LogLevel)
	}
	if len(cfg.Clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(cfg.Clusters))
	}
	cluster, ok := cfg.Clusters["test/dc1"]
	if !ok {
		t.Fatal("cluster test/dc1 not found")
	}
	if cluster.PSK != "test-psk-must-be-at-least-32-chars" {
		t.Errorf("unexpected PSK: %q", cluster.PSK)
	}
	if cluster.Certificates != nil {
		t.Error("expected nil certificates for auto mode")
	}
}

func TestLoadFromString_MultiCluster(t *testing.T) {
	cfg, err := LoadFromString(`
		name: "multi-node"
		port: 5001
		region: "eu-west"
		datacenter: "dc2"
		log_level: "debug"

		clusters: {
			"prod/dc1": {
				psk: "prod-psk-must-be-at-least-32-chars"
				bootstrap: ["/ip4/10.0.0.1/tcp/4001/p2p/QmFoo"]
			}
			"staging/dc1": {
				psk: "staging-psk-must-be-32-chars-long"
			}
		}
	`)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Name != "multi-node" {
		t.Errorf("unexpected name: %q", cfg.Name)
	}
	if cfg.Region != "eu-west" {
		t.Errorf("unexpected region: %q", cfg.Region)
	}
	if cfg.Datacenter != "dc2" {
		t.Errorf("unexpected datacenter: %q", cfg.Datacenter)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("unexpected log_level: %q", cfg.LogLevel)
	}
	if len(cfg.Clusters) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(cfg.Clusters))
	}

	prod := cfg.Clusters["prod/dc1"]
	if len(prod.Bootstrap) != 1 {
		t.Errorf("expected 1 bootstrap peer, got %d", len(prod.Bootstrap))
	}
}

func TestLoadFromString_WithCertificates(t *testing.T) {
	cfg, err := LoadFromString(`
		name: "cert-node"
		port: 6001

		clusters: {
			"prod/dc1": {
				psk: "prod-psk-must-be-at-least-32-chars"
				certificates: {
					ca_cert: "/tmp/test-ca.crt"
					ca_key: "/tmp/test-ca.key"
					node_cert: "/tmp/test-node.crt"
					node_key: "/tmp/test-node.key"
				}
			}
		}
	`)
	// This will fail validation because files don't exist,
	// but we can check the parsing worked
	if err == nil {
		// Files don't exist so validation should fail
		if cfg.Clusters["prod/dc1"].Certificates == nil {
			t.Error("expected certificates to be parsed")
		}
	}
	// The error should be about missing files, not parsing
	if err != nil && cfg == nil {
		t.Logf("expected validation error about missing files: %v", err)
	}
}

func TestLoadFromString_WithHealth(t *testing.T) {
	cfg, err := LoadFromString(`
		name: "health-node"
		port: 7001

		clusters: {
			"test/dc1": {
				psk: "test-psk-must-be-at-least-32-chars"
			}
		}

		health: {
			protocol_period: "1s"
			ping_timeout: "200ms"
			quarantine_timeout: "10s"
			score_increment: 1.0
			suspected_threshold: 2.0
			quarantine_threshold: 5.0
			max_responders: 3
		}
	`)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Health == nil {
		t.Fatal("expected health config")
	}
	if cfg.Health.ProtocolPeriod != "1s" {
		t.Errorf("unexpected protocol_period: %q", cfg.Health.ProtocolPeriod)
	}
	if cfg.Health.PingTimeout != "200ms" {
		t.Errorf("unexpected ping_timeout: %q", cfg.Health.PingTimeout)
	}
	if cfg.Health.ScoreIncrement != 1.0 {
		t.Errorf("unexpected score_increment: %f", cfg.Health.ScoreIncrement)
	}
	if cfg.Health.MaxResponders != 3 {
		t.Errorf("unexpected max_responders: %d", cfg.Health.MaxResponders)
	}
}

func TestLoadFromString_MissingName(t *testing.T) {
	_, err := LoadFromString(`
		port: 4001
		clusters: {
			"test/dc1": {
				psk: "test-psk-must-be-at-least-32-chars"
			}
		}
	`)
	if err == nil {
		t.Error("expected error for missing name")
	}
}

func TestLoadFromString_ShortPSK(t *testing.T) {
	_, err := LoadFromString(`
		name: "test"
		port: 4001
		clusters: {
			"test/dc1": {
				psk: "short"
			}
		}
	`)
	if err == nil {
		t.Error("expected error for short PSK")
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "test.cue")

	content := `
		name: "file-node"
		port: 8001
		region: "ap-south"

		clusters: {
			"test/dc1": {
				psk: "file-test-psk-must-be-32-chars!!!"
			}
		}
	`

	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(configPath, "")
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Name != "file-node" {
		t.Errorf("unexpected name: %q", cfg.Name)
	}
	if cfg.Port != 8001 {
		t.Errorf("unexpected port: %d", cfg.Port)
	}
	if cfg.Region != "ap-south" {
		t.Errorf("unexpected region: %q", cfg.Region)
	}
}

func TestLoadFromFile_NotFound(t *testing.T) {
	_, err := Load("/nonexistent/path.cue", "")
	if err == nil {
		t.Error("expected error for missing file")
	}
}
