package config

import (
	"errors"
	"testing"
	"time"

	"github.com/tareksalem/falak/service"
)

func TestServiceConfig_ToServiceSpec_StaticDefaults(t *testing.T) {
	cfg := &ServiceConfig{
		Name: "payments",
		Ports: []ServicePortConfig{
			{Name: "http", Port: 8080},
		},
		Backends: []ServiceBackendConfig{
			{Capsule: "payments-v1"}, // weight defaults to 100
			{Capsule: "payments-v2", Weight: 10},
		},
	}
	spec, err := cfg.ToServiceSpec()
	if err != nil {
		t.Fatalf("ToServiceSpec: %v", err)
	}
	if spec.Visibility != service.VisibilityEnum.Cluster() {
		t.Errorf("visibility default mismatch: got %q", spec.Visibility)
	}
	if spec.Strategy == nil || spec.Strategy.Type != service.StrategyTypeEnum.Static() {
		t.Errorf("strategy default mismatch: %+v", spec.Strategy)
	}
	if spec.Ports[0].Protocol != service.ProtocolEnum.TCP() {
		t.Errorf("protocol default mismatch: %q", spec.Ports[0].Protocol)
	}
	if got := spec.Backends[0].Weight; got != service.DefaultBackendWeight {
		t.Errorf("weight default mismatch: got %d want %d", got, service.DefaultBackendWeight)
	}
	if got := spec.Backends[1].Weight; got != 10 {
		t.Errorf("explicit weight lost: got %d", got)
	}
	if spec.Timeouts.Idle != service.DefaultIdleTimeout || spec.Timeouts.Connect != service.DefaultConnectTimeout {
		t.Errorf("timeouts defaults mismatch: %+v", spec.Timeouts)
	}
	// Identity port-map shorthand.
	for _, b := range spec.Backends {
		if got := b.PortMap["http"]; got != "http" {
			t.Errorf("port_map shorthand missing for %q: %v", b.Capsule, b.PortMap)
		}
	}
}

func TestServiceConfig_ToServiceSpec_ExplicitOverrides(t *testing.T) {
	cfg := &ServiceConfig{
		Name:       "payments",
		Visibility: "group",
		Group:      "billing",
		Ports: []ServicePortConfig{
			{Name: "rpc", Port: 9000, Protocol: "udp"},
		},
		Backends: []ServiceBackendConfig{
			{Capsule: "payments-v1", Weight: 50, PortMap: map[string]string{"rpc": "grpc"}},
		},
		Timeouts: &ServiceTimeoutsConfig{Idle: "30s", Connect: "1s"},
	}
	spec, err := cfg.ToServiceSpec()
	if err != nil {
		t.Fatalf("ToServiceSpec: %v", err)
	}
	if spec.Visibility != service.VisibilityEnum.Group() || spec.Group != "billing" {
		t.Errorf("visibility/group mismatch: %q / %q", spec.Visibility, spec.Group)
	}
	if spec.Ports[0].Protocol != service.ProtocolEnum.UDP() {
		t.Errorf("udp protocol lost: %q", spec.Ports[0].Protocol)
	}
	if got := spec.Backends[0].PortMap["rpc"]; got != "grpc" {
		t.Errorf("explicit port_map lost: %v", spec.Backends[0].PortMap)
	}
	if spec.Timeouts.Idle != 30*time.Second || spec.Timeouts.Connect != time.Second {
		t.Errorf("timeouts override mismatch: %+v", spec.Timeouts)
	}
}

func TestServiceConfig_ToServiceSpec_ExternalRejected(t *testing.T) {
	cfg := &ServiceConfig{
		Name:       "payments",
		Visibility: "external",
		Ports:      []ServicePortConfig{{Name: "http", Port: 8080}},
		Backends:   []ServiceBackendConfig{{Capsule: "v1"}},
	}
	_, err := cfg.ToServiceSpec()
	if !errors.Is(err, ErrExternalVisibilityNotSupported) {
		t.Fatalf("expected ErrExternalVisibilityNotSupported, got %v", err)
	}
}

func TestServiceConfig_ToServiceSpec_CanaryDispatch(t *testing.T) {
	cfg := &ServiceConfig{
		Name: "payments",
		Ports: []ServicePortConfig{{Name: "http", Port: 8080}},
		Backends: []ServiceBackendConfig{
			{Capsule: "payments-v1", Weight: 100},
			{Capsule: "payments-v2", Weight: 0},
		},
		Strategy: &StrategyConfig{
			Type: "canary",
			Canary: &CanaryStrategyConfig{
				Target:   "payments-v2",
				From:     "payments-v1",
				Step:     10,
				Interval: "5m",
				AbortOn:  []string{"error_rate > 5%"},
			},
		},
	}
	spec, err := cfg.ToServiceSpec()
	if err != nil {
		t.Fatalf("ToServiceSpec: %v", err)
	}
	if spec.Strategy.Type != service.StrategyTypeEnum.Canary() {
		t.Errorf("strategy type mismatch: %q", spec.Strategy.Type)
	}
	if spec.Strategy.Canary == nil {
		t.Fatal("canary block missing")
	}
	if spec.Strategy.Canary.Interval != 5*time.Minute {
		t.Errorf("canary interval mismatch: %v", spec.Strategy.Canary.Interval)
	}
	if len(spec.Strategy.Canary.AbortOn) != 1 || spec.Strategy.Canary.AbortOn[0] != "error_rate > 5%" {
		t.Errorf("abort_on lost: %v", spec.Strategy.Canary.AbortOn)
	}
}

func TestServiceConfig_ToServiceSpec_BlueGreenDrainDefault(t *testing.T) {
	cfg := &ServiceConfig{
		Name: "payments",
		Ports: []ServicePortConfig{{Name: "http", Port: 8080}},
		Backends: []ServiceBackendConfig{
			{Capsule: "payments-v1", Weight: 100},
			{Capsule: "payments-v2", Weight: 0},
		},
		Strategy: &StrategyConfig{
			Type: "blue-green",
			BlueGreen: &BlueGreenStrategyConfig{
				Active: "payments-v1",
			},
		},
	}
	spec, err := cfg.ToServiceSpec()
	if err != nil {
		t.Fatalf("ToServiceSpec: %v", err)
	}
	if spec.Strategy.BlueGreen.Drain != service.DefaultBlueGreenDrain {
		t.Errorf("default drain mismatch: %v", spec.Strategy.BlueGreen.Drain)
	}
}

func TestServiceConfig_ToServiceSpec_InvalidPort(t *testing.T) {
	cfg := &ServiceConfig{
		Name:     "payments",
		Ports:    []ServicePortConfig{{Name: "http", Port: 0}},
		Backends: []ServiceBackendConfig{{Capsule: "v1"}},
	}
	if _, err := cfg.ToServiceSpec(); err == nil {
		t.Fatal("expected error for port=0")
	}
}

func TestServiceConfig_ToServiceSpec_RoundTripViaValidate(t *testing.T) {
	cfg := &ServiceConfig{
		Name: "payments",
		Ports: []ServicePortConfig{
			{Name: "http", Port: 8080},
		},
		Backends: []ServiceBackendConfig{
			{Capsule: "payments-v1", Weight: 100},
		},
	}
	spec, err := cfg.ToServiceSpec()
	if err != nil {
		t.Fatalf("ToServiceSpec: %v", err)
	}
	if err := service.ValidateSpec(&spec); err != nil {
		t.Fatalf("ValidateSpec failed on converted spec: %v", err)
	}
}

func TestServiceConfig_ToServiceSpec_InvalidProtocol(t *testing.T) {
	cfg := &ServiceConfig{
		Name:     "payments",
		Ports:    []ServicePortConfig{{Name: "http", Port: 8080, Protocol: "http"}},
		Backends: []ServiceBackendConfig{{Capsule: "v1"}},
	}
	if _, err := cfg.ToServiceSpec(); err == nil {
		t.Fatal("expected error for protocol=http (reserved)")
	}
}

func TestServiceConfig_ToServiceSpec_NilReceiver(t *testing.T) {
	var cfg *ServiceConfig
	if _, err := cfg.ToServiceSpec(); err == nil {
		t.Fatal("expected error on nil receiver")
	}
}
