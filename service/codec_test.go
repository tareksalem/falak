package service

import (
	"testing"
	"time"
)

func TestServiceCodec_RoundTrip(t *testing.T) {
	now := time.Unix(1715800000, 0).UTC()
	original := &Service{
		ID:        "svc-1",
		ClusterID: "us-east/dc1/prod",
		Status:    ServiceStatusEnum.Active(),
		Version:   "7",
		CreatedAt: now,
		UpdatedAt: now.Add(time.Minute),
		Spec: ServiceSpec{
			Name:       "payments",
			Visibility: VisibilityEnum.Cluster(),
			Group:      "billing",
			Ports: []ServicePort{
				{Name: "http", Port: 8080, Protocol: ProtocolEnum.TCP()},
				{Name: "grpc", Port: 8081, Protocol: ProtocolEnum.TCP()},
			},
			Backends: []ServiceBackend{
				{
					Capsule:           "payments-v1",
					CapturedCapsuleID: "cap-001",
					PortMap:           map[string]string{"http": "http"},
					Weight:            90,
				},
				{Capsule: "payments-v2", Weight: 10},
			},
			Strategy: &Strategy{
				Type: StrategyTypeEnum.Canary(),
				Canary: &CanaryStrategy{
					Target:          "payments-v2",
					From:            "payments-v1",
					Step:            10,
					Interval:        5 * time.Minute,
					SuccessCriteria: []string{"error_rate<1%"},
					AbortOn:         []string{"error_rate>5%"},
				},
			},
			Timeouts: ServiceTimeouts{Idle: 5 * time.Minute, Connect: 5 * time.Second},
		},
		BackendStates: []BackendState{
			{
				Name:              "payments-v1",
				Resolution:        BackendResolutionEnum.Resolved(),
				CapturedCapsuleID: "cap-001",
				LastResolvedAt:    now,
			},
			{
				Name:           "payments-v2",
				Resolution:     BackendResolutionEnum.Unresolved(),
				LastResolvedAt: now,
			},
		},
	}

	pb := serviceToProto(original)
	if pb == nil {
		t.Fatal("serviceToProto returned nil")
	}
	out := serviceFromProto(pb)
	if out == nil {
		t.Fatal("serviceFromProto returned nil")
	}

	if out.ID != original.ID {
		t.Errorf("ID: got %q want %q", out.ID, original.ID)
	}
	if out.ClusterID != original.ClusterID {
		t.Errorf("ClusterID: got %q want %q", out.ClusterID, original.ClusterID)
	}
	if out.Status != original.Status {
		t.Errorf("Status: got %q want %q", out.Status, original.Status)
	}
	if out.Spec.Name != original.Spec.Name {
		t.Errorf("Spec.Name: got %q want %q", out.Spec.Name, original.Spec.Name)
	}
	if len(out.Spec.Ports) != 2 {
		t.Errorf("Ports: got %d want 2", len(out.Spec.Ports))
	}
	if len(out.Spec.Backends) != 2 {
		t.Errorf("Backends: got %d want 2", len(out.Spec.Backends))
	}
	if out.Spec.Backends[0].CapturedCapsuleID != "cap-001" {
		t.Errorf("CapturedCapsuleID lost: got %q", out.Spec.Backends[0].CapturedCapsuleID)
	}
	if out.Spec.Backends[0].Weight != 90 {
		t.Errorf("Weight: got %d want 90", out.Spec.Backends[0].Weight)
	}
	if out.Spec.Backends[0].PortMap["http"] != "http" {
		t.Errorf("PortMap dropped: got %v", out.Spec.Backends[0].PortMap)
	}
	if out.Spec.Strategy == nil || out.Spec.Strategy.Canary == nil {
		t.Fatal("Strategy.Canary lost in round-trip")
	}
	if out.Spec.Strategy.Canary.Interval != 5*time.Minute {
		t.Errorf("Canary.Interval: got %v want 5m", out.Spec.Strategy.Canary.Interval)
	}
	if out.Spec.Strategy.Canary.AbortOn[0] != "error_rate>5%" {
		t.Errorf("Canary.AbortOn lost: %v", out.Spec.Strategy.Canary.AbortOn)
	}
	if out.Spec.Timeouts.Idle != 5*time.Minute {
		t.Errorf("Timeouts.Idle: got %v want 5m", out.Spec.Timeouts.Idle)
	}
	if len(out.BackendStates) != 2 {
		t.Errorf("BackendStates: got %d want 2", len(out.BackendStates))
	}
	if out.BackendStates[0].Resolution != BackendResolutionEnum.Resolved() {
		t.Errorf("BackendStates[0].Resolution: got %q", out.BackendStates[0].Resolution)
	}
}

func TestServiceCodec_NilSafe(t *testing.T) {
	if serviceToProto(nil) != nil {
		t.Error("serviceToProto(nil) should be nil")
	}
	if serviceFromProto(nil) != nil {
		t.Error("serviceFromProto(nil) should be nil")
	}
}

func TestServiceCodec_BlueGreen(t *testing.T) {
	svc := &Service{
		ID: "svc-bg", ClusterID: "c",
		Status: ServiceStatusEnum.Active(),
		Spec: ServiceSpec{
			Name:     "payments",
			Ports:    []ServicePort{{Name: "http", Port: 8080, Protocol: ProtocolEnum.TCP()}},
			Backends: []ServiceBackend{{Capsule: "v1", Weight: 100}, {Capsule: "v2"}},
			Strategy: &Strategy{
				Type:      StrategyTypeEnum.BlueGreen(),
				BlueGreen: &BlueGreenStrategy{Active: "v1", Drain: 30 * time.Second},
			},
		},
	}
	out := serviceFromProto(serviceToProto(svc))
	if out.Spec.Strategy.BlueGreen == nil {
		t.Fatal("BlueGreen lost")
	}
	if out.Spec.Strategy.BlueGreen.Drain != 30*time.Second {
		t.Errorf("Drain: got %v want 30s", out.Spec.Strategy.BlueGreen.Drain)
	}
	if out.Spec.Strategy.BlueGreen.Active != "v1" {
		t.Errorf("Active: got %q want v1", out.Spec.Strategy.BlueGreen.Active)
	}
}
