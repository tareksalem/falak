package service

import (
	"errors"
	"testing"
	"time"
)

func validSpec() ServiceSpec {
	return ServiceSpec{
		Name:       "payments",
		Visibility: VisibilityEnum.Cluster(),
		Ports:      []ServicePort{{Name: "http", Port: 8080, Protocol: ProtocolEnum.TCP()}},
		Backends: []ServiceBackend{
			{Capsule: "payments-v1", Weight: 90},
			{Capsule: "payments-v2", Weight: 10},
		},
		Strategy: &Strategy{Type: StrategyTypeEnum.Static()},
	}
}

func TestDefaultSpec_FillsMissingFields(t *testing.T) {
	var spec ServiceSpec
	spec.Ports = []ServicePort{{Name: "http", Port: 8080}}
	spec.Backends = []ServiceBackend{{Capsule: "payments-v1"}}

	DefaultSpec(&spec)

	if spec.Visibility != VisibilityEnum.Cluster() {
		t.Errorf("visibility default = %q, want cluster", spec.Visibility)
	}
	if spec.Strategy == nil || spec.Strategy.Type != StrategyTypeEnum.Static() {
		t.Errorf("strategy default = %+v, want static", spec.Strategy)
	}
	if spec.Timeouts.Idle != DefaultIdleTimeout {
		t.Errorf("idle default = %s, want %s", spec.Timeouts.Idle, DefaultIdleTimeout)
	}
	if spec.Timeouts.Connect != DefaultConnectTimeout {
		t.Errorf("connect default = %s, want %s", spec.Timeouts.Connect, DefaultConnectTimeout)
	}
	if spec.Ports[0].Protocol != ProtocolEnum.TCP() {
		t.Errorf("port protocol default = %q, want tcp", spec.Ports[0].Protocol)
	}
	if spec.Backends[0].Weight != DefaultBackendWeight {
		t.Errorf("backend weight default = %d, want %d", spec.Backends[0].Weight, DefaultBackendWeight)
	}
}

func TestDefaultSpec_BlueGreenDrainDefault(t *testing.T) {
	spec := validSpec()
	spec.Strategy = &Strategy{
		Type:      StrategyTypeEnum.BlueGreen(),
		BlueGreen: &BlueGreenStrategy{Active: "payments-v1"},
	}
	DefaultSpec(&spec)
	if spec.Strategy.BlueGreen.Drain != DefaultBlueGreenDrain {
		t.Errorf("drain default = %s, want %s", spec.Strategy.BlueGreen.Drain, DefaultBlueGreenDrain)
	}
}

func TestDefaultSpec_NilSafe(t *testing.T) {
	DefaultSpec(nil) // must not panic
}

func TestDefaultSpec_PreservesExplicitValues(t *testing.T) {
	spec := validSpec()
	spec.Timeouts = ServiceTimeouts{Idle: 30 * time.Second, Connect: 1 * time.Second}
	DefaultSpec(&spec)
	if spec.Timeouts.Idle != 30*time.Second {
		t.Errorf("idle overridden: got %s", spec.Timeouts.Idle)
	}
	if spec.Timeouts.Connect != 1*time.Second {
		t.Errorf("connect overridden: got %s", spec.Timeouts.Connect)
	}
}

func TestValidateSpec_AcceptsValidSpec(t *testing.T) {
	spec := validSpec()
	DefaultSpec(&spec)
	if err := ValidateSpec(&spec); err != nil {
		t.Errorf("ValidateSpec on valid spec returned: %v", err)
	}
}

func TestValidateSpec_NilReturnsNameRequired(t *testing.T) {
	if err := ValidateSpec(nil); !errors.Is(err, ErrNameRequired) {
		t.Errorf("ValidateSpec(nil) = %v, want %v", err, ErrNameRequired)
	}
}

func TestValidateSpec_Sentinels(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ServiceSpec)
		wantErr error
	}{
		{
			name:    "name empty",
			mutate:  func(s *ServiceSpec) { s.Name = "" },
			wantErr: ErrNameRequired,
		},
		{
			name:    "name whitespace",
			mutate:  func(s *ServiceSpec) { s.Name = "   " },
			wantErr: ErrNameRequired,
		},
		{
			name:    "no ports",
			mutate:  func(s *ServiceSpec) { s.Ports = nil },
			wantErr: ErrPortRequired,
		},
		{
			name:    "port zero",
			mutate:  func(s *ServiceSpec) { s.Ports[0].Port = 0 },
			wantErr: ErrInvalidPort,
		},
		{
			name:    "port name invalid (uppercase)",
			mutate:  func(s *ServiceSpec) { s.Ports[0].Name = "HTTP" },
			wantErr: ErrPortNameInvalid,
		},
		{
			name:    "port name invalid (leading dash)",
			mutate:  func(s *ServiceSpec) { s.Ports[0].Name = "-http" },
			wantErr: ErrPortNameInvalid,
		},
		{
			name:    "port protocol reserved http",
			mutate:  func(s *ServiceSpec) { s.Ports[0].Protocol = ProtocolEnum.HTTP() },
			wantErr: ErrInvalidProtocol,
		},
		{
			name:    "no backends",
			mutate:  func(s *ServiceSpec) { s.Backends = nil },
			wantErr: ErrBackendRequired,
		},
		{
			name:    "backend name empty",
			mutate:  func(s *ServiceSpec) { s.Backends[0].Capsule = "" },
			wantErr: ErrBackendNameEmpty,
		},
		{
			name:    "backend weight negative",
			mutate:  func(s *ServiceSpec) { s.Backends[0].Weight = -1 },
			wantErr: ErrInvalidWeight,
		},
		{
			name:    "backend weight too high",
			mutate:  func(s *ServiceSpec) { s.Backends[0].Weight = 10001 },
			wantErr: ErrInvalidWeight,
		},
		{
			name:    "visibility external rejected",
			mutate:  func(s *ServiceSpec) { s.Visibility = VisibilityEnum.External() },
			wantErr: ErrExternalVisibilityNotSupported,
		},
		{
			name:    "visibility unknown",
			mutate:  func(s *ServiceSpec) { s.Visibility = "namespace" },
			wantErr: ErrInvalidVisibility,
		},
		{
			name: "canary missing config",
			mutate: func(s *ServiceSpec) {
				s.Strategy = &Strategy{Type: StrategyTypeEnum.Canary()}
			},
			wantErr: ErrCanaryConfigMissing,
		},
		{
			name: "canary target unknown",
			mutate: func(s *ServiceSpec) {
				s.Strategy = &Strategy{Type: StrategyTypeEnum.Canary(),
					Canary: &CanaryStrategy{
						Target: "payments-v3", From: "payments-v1",
						Step: 10, AbortOn: []string{"error_rate>5"},
					}}
			},
			wantErr: ErrCanaryTargetUnknown,
		},
		{
			name: "canary from unknown",
			mutate: func(s *ServiceSpec) {
				s.Strategy = &Strategy{Type: StrategyTypeEnum.Canary(),
					Canary: &CanaryStrategy{
						Target: "payments-v2", From: "ghost",
						Step: 10, AbortOn: []string{"error_rate>5"},
					}}
			},
			wantErr: ErrCanaryFromUnknown,
		},
		{
			name: "canary step zero",
			mutate: func(s *ServiceSpec) {
				s.Strategy = &Strategy{Type: StrategyTypeEnum.Canary(),
					Canary: &CanaryStrategy{
						Target: "payments-v2", From: "payments-v1",
						Step: 0, AbortOn: []string{"x>1"},
					}}
			},
			wantErr: ErrInvalidStep,
		},
		{
			name: "canary step too high",
			mutate: func(s *ServiceSpec) {
				s.Strategy = &Strategy{Type: StrategyTypeEnum.Canary(),
					Canary: &CanaryStrategy{
						Target: "payments-v2", From: "payments-v1",
						Step: 101, AbortOn: []string{"x>1"},
					}}
			},
			wantErr: ErrInvalidStep,
		},
		{
			name: "canary abort_on missing",
			mutate: func(s *ServiceSpec) {
				s.Strategy = &Strategy{Type: StrategyTypeEnum.Canary(),
					Canary: &CanaryStrategy{
						Target: "payments-v2", From: "payments-v1", Step: 10,
					}}
			},
			wantErr: ErrCanaryAbortOnRequired,
		},
		{
			name: "blue-green missing config",
			mutate: func(s *ServiceSpec) {
				s.Strategy = &Strategy{Type: StrategyTypeEnum.BlueGreen()}
			},
			wantErr: ErrBlueGreenConfigMissing,
		},
		{
			name: "blue-green active unknown",
			mutate: func(s *ServiceSpec) {
				s.Strategy = &Strategy{Type: StrategyTypeEnum.BlueGreen(),
					BlueGreen: &BlueGreenStrategy{Active: "ghost"}}
			},
			wantErr: ErrBlueGreenActiveUnknown,
		},
		{
			name:    "strategy type invalid",
			mutate:  func(s *ServiceSpec) { s.Strategy = &Strategy{Type: "weird"} },
			wantErr: ErrInvalidStrategyType,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec()
			tc.mutate(&spec)
			err := ValidateSpec(&spec)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("ValidateSpec err = %v, want errors.Is(%v) == true", err, tc.wantErr)
			}
		})
	}
}

func TestValidateSpec_ValidCanary(t *testing.T) {
	spec := validSpec()
	spec.Strategy = &Strategy{
		Type: StrategyTypeEnum.Canary(),
		Canary: &CanaryStrategy{
			Target: "payments-v2", From: "payments-v1", Step: 10,
			AbortOn: []string{"error_rate>5"},
		},
	}
	if err := ValidateSpec(&spec); err != nil {
		t.Errorf("ValidateSpec on valid canary returned: %v", err)
	}
}

func TestValidateSpec_ValidBlueGreen(t *testing.T) {
	spec := validSpec()
	spec.Strategy = &Strategy{
		Type:      StrategyTypeEnum.BlueGreen(),
		BlueGreen: &BlueGreenStrategy{Active: "payments-v1", Drain: 10 * time.Second},
	}
	if err := ValidateSpec(&spec); err != nil {
		t.Errorf("ValidateSpec on valid blue-green returned: %v", err)
	}
}
