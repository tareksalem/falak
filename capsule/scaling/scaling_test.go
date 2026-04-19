package scaling

import (
	"testing"
	"time"

	"github.com/tareksalem/falak/capsule"
)

// --- Condition Parsing Tests ---

func TestParseCondition(t *testing.T) {
	tests := []struct {
		raw      string
		metric   string
		value    float64
		unit     string
		wantErr  bool
	}{
		{"cpu > 70%", "cpu", 70, "%", false},
		{"memory > 80%", "memory", 80, "%", false},
		{"rps > 1000", "rps", 1000, "", false},
		{"latency.p95 > 200ms", "latency.p95", 200, "ms", false},
		{"connections > 500", "connections", 500, "", false},
		{"idle > 5m", "idle", 5, "m", false},
		{"cpu < 15%", "cpu", 15, "%", false},
		{"rps == 0", "rps", 0, "", false},
		{"queue_depth >= 100", "queue_depth", 100, "", false},
		{"schedule: weekdays 8-18", "schedule", 0, "", false},
		{"invalid", "", 0, "", true},
		{"cpu >", "", 0, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			cond, err := ParseCondition(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cond.Metric != tt.metric {
				t.Errorf("metric: got %q, want %q", cond.Metric, tt.metric)
			}
			if cond.Value != tt.value {
				t.Errorf("value: got %f, want %f", cond.Value, tt.value)
			}
			if cond.Unit != tt.unit {
				t.Errorf("unit: got %q, want %q", cond.Unit, tt.unit)
			}
		})
	}
}

// --- Condition Evaluation Tests ---

func TestConditionEvaluate(t *testing.T) {
	tests := []struct {
		name     string
		cond     Condition
		value    float64
		expected bool
	}{
		{"greater than - true", Condition{Operator: ">"}, 80, true},
		{"greater than - false", Condition{Operator: ">", Value: 90}, 80, false},
		{"less than - true", Condition{Operator: "<", Value: 90}, 80, true},
		{"equal - true", Condition{Operator: "==", Value: 0}, 0, true},
		{"equal - false", Condition{Operator: "==", Value: 0}, 5, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cond.Evaluate(tt.value); got != tt.expected {
				t.Errorf("Evaluate(%f) = %v, want %v", tt.value, got, tt.expected)
			}
		})
	}
}

// --- Mock MetricsProvider ---

type mockMetrics struct {
	data map[string]float64
}

func (m *mockMetrics) GetMetric(name string) (float64, bool) {
	v, ok := m.data[name]
	return v, ok
}

// --- Evaluator Tests ---

func TestEvaluator_AllMatch(t *testing.T) {
	eval := NewEvaluator()
	metrics := &mockMetrics{data: map[string]float64{
		"cpu":    75,
		"memory": 65,
	}}

	rules := []Rule{
		{
			Name:    "high load",
			Trigger: capsule.TriggerModeEnum.All(),
			Conditions: []Condition{
				{Metric: "cpu", Operator: ">", Value: 70},
				{Metric: "memory", Operator: ">", Value: 60},
			},
			Action:   capsule.ScalingActionEnum.ScaleUp(),
			Cooldown: 0,
		},
	}

	result := eval.Evaluate("capsule-1", rules, metrics)
	if result == nil || !result.Matched {
		t.Error("expected rule to match")
	}
	if result.Action != capsule.ScalingActionEnum.ScaleUp() {
		t.Error("expected scaleUp action")
	}
}

func TestEvaluator_AllNotMatch(t *testing.T) {
	eval := NewEvaluator()
	metrics := &mockMetrics{data: map[string]float64{
		"cpu":    75,
		"memory": 50, // below threshold
	}}

	rules := []Rule{
		{
			Name:    "high load",
			Trigger: capsule.TriggerModeEnum.All(),
			Conditions: []Condition{
				{Metric: "cpu", Operator: ">", Value: 70},
				{Metric: "memory", Operator: ">", Value: 60},
			},
			Action: capsule.ScalingActionEnum.ScaleUp(),
		},
	}

	result := eval.Evaluate("capsule-1", rules, metrics)
	if result != nil {
		t.Error("expected no match when not all conditions met")
	}
}

func TestEvaluator_AnyMatch(t *testing.T) {
	eval := NewEvaluator()
	metrics := &mockMetrics{data: map[string]float64{
		"latency.p95": 250,
		"rps":         500, // below threshold
	}}

	rules := []Rule{
		{
			Name:    "traffic spike",
			Trigger: capsule.TriggerModeEnum.Any(),
			Conditions: []Condition{
				{Metric: "latency.p95", Operator: ">", Value: 200},
				{Metric: "rps", Operator: ">", Value: 1000},
			},
			Action: capsule.ScalingActionEnum.ScaleUp(),
		},
	}

	result := eval.Evaluate("capsule-1", rules, metrics)
	if result == nil || !result.Matched {
		t.Error("expected rule to match with any trigger")
	}
}

func TestEvaluator_Cooldown(t *testing.T) {
	eval := NewEvaluator()
	metrics := &mockMetrics{data: map[string]float64{"cpu": 90}}

	rules := []Rule{
		{
			Name:    "high cpu",
			Trigger: capsule.TriggerModeEnum.All(),
			Conditions: []Condition{
				{Metric: "cpu", Operator: ">", Value: 70},
			},
			Action:   capsule.ScalingActionEnum.ScaleUp(),
			Cooldown: 1 * time.Minute,
		},
	}

	// First evaluation should match
	result := eval.Evaluate("capsule-1", rules, metrics)
	if result == nil {
		t.Fatal("first evaluation should match")
	}

	// Second evaluation within cooldown should not match
	result = eval.Evaluate("capsule-1", rules, metrics)
	if result != nil {
		t.Error("second evaluation should be in cooldown")
	}
}

func TestEvaluator_FirstMatchWins(t *testing.T) {
	eval := NewEvaluator()
	metrics := &mockMetrics{data: map[string]float64{"cpu": 90, "rps": 5}}

	rules := []Rule{
		{
			Name:    "high cpu",
			Trigger: capsule.TriggerModeEnum.All(),
			Conditions: []Condition{
				{Metric: "cpu", Operator: ">", Value: 70},
			},
			Action: capsule.ScalingActionEnum.ScaleUp(),
		},
		{
			Name:    "low rps",
			Trigger: capsule.TriggerModeEnum.All(),
			Conditions: []Condition{
				{Metric: "rps", Operator: "<", Value: 10},
			},
			Action: capsule.ScalingActionEnum.ScaleDown(),
		},
	}

	result := eval.Evaluate("capsule-1", rules, metrics)
	if result == nil {
		t.Fatal("expected match")
	}
	if result.RuleName != "high cpu" {
		t.Errorf("expected first rule to win, got %q", result.RuleName)
	}
}

// --- FromSpec Tests ---

func TestFromSpec(t *testing.T) {
	spec := capsule.ScalingRule{
		Name:       "test",
		Trigger:    capsule.TriggerModeEnum.All(),
		Conditions: []string{"cpu > 70%", "memory > 60%"},
		Action:     capsule.ScalingActionEnum.ScaleUp(),
		Cooldown:   time.Minute,
	}

	rule, err := FromSpec(spec)
	if err != nil {
		t.Fatalf("FromSpec failed: %v", err)
	}
	if len(rule.Conditions) != 2 {
		t.Errorf("expected 2 conditions, got %d", len(rule.Conditions))
	}
	if rule.Conditions[0].Metric != "cpu" {
		t.Errorf("expected cpu metric, got %s", rule.Conditions[0].Metric)
	}
}
