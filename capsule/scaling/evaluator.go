package scaling

import (
	"sync"
	"time"

	"github.com/tareksalem/falak/capsule"
)

// MetricsProvider supplies current metric values for scaling evaluation.
type MetricsProvider interface {
	// GetMetric returns the current value for a metric name.
	// Known metrics: "cpu", "memory", "rps", "latency.p95", "latency.p99",
	// "connections", "queue_depth", "idle" (seconds idle).
	// Returns 0 and false if the metric is unknown.
	GetMetric(name string) (float64, bool)
}

// EvalResult holds the result of evaluating scaling rules for a capsule.
type EvalResult struct {
	RuleName string
	Action   capsule.ScalingAction
	Matched  bool
}

// Evaluator evaluates scaling rules against current metrics.
type Evaluator struct {
	mu        sync.RWMutex
	cooldowns map[string]time.Time // ruleKey -> last triggered time
}

// NewEvaluator creates a new scaling evaluator.
func NewEvaluator() *Evaluator {
	return &Evaluator{
		cooldowns: make(map[string]time.Time),
	}
}

// Evaluate checks all rules against current metrics for a capsule.
// Rules are evaluated top to bottom; the first matching rule wins.
// Returns nil if no rules match.
func (e *Evaluator) Evaluate(capsuleID capsule.CapsuleID, rules []Rule, metrics MetricsProvider) *EvalResult {
	now := time.Now()

	for _, rule := range rules {
		cooldownKey := string(capsuleID) + ":" + rule.Name

		if e.isInCooldown(cooldownKey, rule.Cooldown, now) {
			continue
		}

		if e.evaluateRule(rule, metrics) {
			e.recordCooldown(cooldownKey, now)
			return &EvalResult{
				RuleName: rule.Name,
				Action:   rule.Action,
				Matched:  true,
			}
		}
	}

	return nil
}

func (e *Evaluator) evaluateRule(rule Rule, metrics MetricsProvider) bool {
	switch rule.Trigger {
	case capsule.TriggerModeEnum.All():
		return e.evaluateAll(rule.Conditions, metrics)
	case capsule.TriggerModeEnum.Any():
		return e.evaluateAny(rule.Conditions, metrics)
	default:
		return false
	}
}

// evaluateAll returns true if all conditions are satisfied.
func (e *Evaluator) evaluateAll(conditions []Condition, metrics MetricsProvider) bool {
	for _, cond := range conditions {
		if cond.Metric == "schedule" {
			// Schedule evaluation deferred to future
			continue
		}
		val, ok := metrics.GetMetric(cond.Metric)
		if !ok {
			return false
		}
		if !cond.Evaluate(val) {
			return false
		}
	}
	return true
}

// evaluateAny returns true if any condition is satisfied.
func (e *Evaluator) evaluateAny(conditions []Condition, metrics MetricsProvider) bool {
	for _, cond := range conditions {
		if cond.Metric == "schedule" {
			continue
		}
		val, ok := metrics.GetMetric(cond.Metric)
		if !ok {
			continue
		}
		if cond.Evaluate(val) {
			return true
		}
	}
	return false
}

func (e *Evaluator) isInCooldown(key string, cooldown time.Duration, now time.Time) bool {
	if cooldown == 0 {
		return false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()

	lastTriggered, ok := e.cooldowns[key]
	if !ok {
		return false
	}
	return now.Before(lastTriggered.Add(cooldown))
}

func (e *Evaluator) recordCooldown(key string, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cooldowns[key] = now
}

// ResetCooldown clears the cooldown for a specific capsule and rule.
func (e *Evaluator) ResetCooldown(capsuleID capsule.CapsuleID, ruleName string) {
	key := string(capsuleID) + ":" + ruleName
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.cooldowns, key)
}

// ResetAllCooldowns clears all cooldowns for a capsule.
func (e *Evaluator) ResetAllCooldowns(capsuleID capsule.CapsuleID) {
	prefix := string(capsuleID) + ":"
	e.mu.Lock()
	defer e.mu.Unlock()
	for key := range e.cooldowns {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			delete(e.cooldowns, key)
		}
	}
}
