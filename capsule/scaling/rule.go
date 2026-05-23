// Package scaling provides scaling rule evaluation and monitoring for capsule autoscaling.
// Scaling rules define conditions (CPU, memory, traffic, latency, connections, queue depth,
// schedule) that trigger scale up, scale down, or scale-to-zero actions.
package scaling

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tareksalem/falak/capsule"
	"github.com/tareksalem/falak/capsule/enums"
)

// Operator represents a comparison operator in a condition.
type Operator string

const (
	opGreaterThan Operator = ">"
	opLessThan    Operator = "<"
	opEqual       Operator = "=="
	opGreaterEq   Operator = ">="
	opLessEq      Operator = "<="
)

// Condition is a parsed scaling condition (e.g., "cpu > 70%").
type Condition struct {
	Metric   string
	Operator Operator
	Value    float64
	Unit     string // "%", "ms", "s", "m", "" (raw number)
	Raw      string // original condition string
}

// Rule is an evaluated scaling rule with parsed conditions.
type Rule struct {
	Name       string
	Trigger    enums.TriggerMode
	Conditions []Condition
	Action     enums.ScalingAction
	Cooldown   time.Duration
}

// FromSpec converts a capsule.ScalingRule into a Rule with parsed conditions.
func FromSpec(spec capsule.ScalingRule) (Rule, error) {
	conditions := make([]Condition, 0, len(spec.Conditions))
	for _, raw := range spec.Conditions {
		cond, err := ParseCondition(raw)
		if err != nil {
			return Rule{}, fmt.Errorf("rule %q: %w", spec.Name, err)
		}
		conditions = append(conditions, cond)
	}

	return Rule{
		Name:       spec.Name,
		Trigger:    spec.Trigger,
		Conditions: conditions,
		Action:     spec.Action,
		Cooldown:   spec.Cooldown,
	}, nil
}

// FromSpecList converts a slice of capsule.ScalingRule into Rules.
func FromSpecList(specs []capsule.ScalingRule) ([]Rule, error) {
	rules := make([]Rule, 0, len(specs))
	for _, spec := range specs {
		rule, err := FromSpec(spec)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// ParseCondition parses a condition string like "cpu > 70%" or "rps > 1000" or "idle > 5m".
// Supported formats:
//   - "metric operator value[unit]" e.g., "cpu > 70%", "rps > 1000", "latency.p95 > 200ms"
//   - "schedule: expression" e.g., "schedule: weekdays 8-18"
func ParseCondition(raw string) (Condition, error) {
	raw = strings.TrimSpace(raw)

	// Handle schedule conditions specially
	if strings.HasPrefix(raw, "schedule:") {
		return Condition{
			Metric: "schedule",
			Raw:    raw,
		}, nil
	}

	// Parse "metric operator value[unit]"
	parts := tokenize(raw)
	if len(parts) != 3 {
		return Condition{}, fmt.Errorf("invalid condition %q: expected 'metric operator value'", raw)
	}

	metric := parts[0]
	op, err := parseOperator(parts[1])
	if err != nil {
		return Condition{}, fmt.Errorf("invalid condition %q: %w", raw, err)
	}

	value, unit, err := parseValue(parts[2])
	if err != nil {
		return Condition{}, fmt.Errorf("invalid condition %q: %w", raw, err)
	}

	return Condition{
		Metric:   metric,
		Operator: op,
		Value:    value,
		Unit:     unit,
		Raw:      raw,
	}, nil
}

func tokenize(s string) []string {
	var tokens []string
	for _, part := range strings.Fields(s) {
		if part != "" {
			tokens = append(tokens, part)
		}
	}
	return tokens
}

func parseOperator(s string) (Operator, error) {
	switch s {
	case ">":
		return opGreaterThan, nil
	case "<":
		return opLessThan, nil
	case "==":
		return opEqual, nil
	case ">=":
		return opGreaterEq, nil
	case "<=":
		return opLessEq, nil
	default:
		return "", fmt.Errorf("unknown operator %q", s)
	}
}

func parseValue(s string) (float64, string, error) {
	// Try to extract unit suffix
	for _, unit := range []string{"%", "ms", "m", "s"} {
		if strings.HasSuffix(s, unit) {
			numStr := strings.TrimSuffix(s, unit)
			val, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return 0, "", fmt.Errorf("invalid number %q", numStr)
			}
			return val, unit, nil
		}
	}

	// No unit — raw number
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, "", fmt.Errorf("invalid number %q", s)
	}
	return val, "", nil
}

// Evaluate checks if a condition is satisfied given a metric value.
func (c Condition) Evaluate(metricValue float64) bool {
	switch c.Operator {
	case opGreaterThan:
		return metricValue > c.Value
	case opLessThan:
		return metricValue < c.Value
	case opEqual:
		return metricValue == c.Value
	case opGreaterEq:
		return metricValue >= c.Value
	case opLessEq:
		return metricValue <= c.Value
	default:
		return false
	}
}
