package capsule

import (
	"errors"
	"fmt"
	"time"

	enums "github.com/tareksalem/falak/capsule/enums"
)

// Validation errors.
var (
	ErrNameRequired         = errors.New("capsule name is required")
	ErrImageRequired        = errors.New("capsule image is required")
	ErrOrbitRequired        = errors.New("capsule orbit is required")
	ErrInvalidTier          = errors.New("invalid tier: must be critical, standard, or background")
	ErrInvalidCPU           = errors.New("CPU cores must be positive")
	ErrInvalidMemory        = errors.New("memory must be positive")
	ErrInvalidDisk          = errors.New("disk must be positive")
	ErrInvalidReplicas      = errors.New("invalid replica config: min must be <= max, and both positive")
	ErrInvalidExactReplicas = errors.New("exact replicas must be positive")
	ErrReplicaConflict      = errors.New("cannot set both exact and min/max replicas")
)

// DefaultSpec applies default values to a CapsuleSpec.
// Call this before Validate to fill in missing fields.
func DefaultSpec(spec *CapsuleSpec) {
	if spec.Tier == "" {
		spec.Tier = enums.TierEnum.Standard()
	}

	if spec.Labels == nil {
		spec.Labels = make(Labels)
	}

	// Replica defaults
	if spec.Replicas.Exact == 0 && spec.Replicas.Min == 0 && spec.Replicas.Max == 0 {
		spec.Replicas.Min = 1
		spec.Replicas.Max = 1
	}

	// Momentum defaults from tier
	if spec.MomentumConfig.Base == 0 {
		spec.MomentumConfig.Base = spec.Tier.BaseMomentum()
	}

	// Placement rule defaults
	for i := range spec.PlacementRules {
		defaultPlacementRule(&spec.PlacementRules[i])
	}

	// Scaling rule defaults
	for i := range spec.ScalingRules {
		defaultScalingRule(&spec.ScalingRules[i])
	}

	// Runtime defaults
	if spec.Runtime.Env == nil {
		spec.Runtime.Env = make(map[string]string)
	}
	if spec.Runtime.Network.Mode == "" {
		spec.Runtime.Network.Mode = enums.NetworkModeEnum.Bridge()
	}
	if spec.Runtime.StatsInterval == 0 {
		spec.Runtime.StatsInterval = 5 * time.Second
	}

	// Failure policy defaults
	if spec.Runtime.FailurePolicy.RestartLimit == 0 {
		spec.Runtime.FailurePolicy.RestartLimit = 3
	}
	if spec.Runtime.FailurePolicy.MaxNodeAttempts == 0 {
		spec.Runtime.FailurePolicy.MaxNodeAttempts = 3
	}
	if spec.Runtime.FailurePolicy.GracefulTimeout == 0 {
		spec.Runtime.FailurePolicy.GracefulTimeout = 10 * time.Second
	}

	// Log retention defaults
	if spec.Runtime.LogRetention.MaxFileSizeMB == 0 {
		spec.Runtime.LogRetention.MaxFileSizeMB = 10
	}
	if spec.Runtime.LogRetention.MaxFiles == 0 {
		spec.Runtime.LogRetention.MaxFiles = 10
	}

	// Health check defaults (only if a health check is configured)
	if spec.Runtime.HealthCheck != nil {
		if spec.Runtime.HealthCheck.Interval == 0 {
			spec.Runtime.HealthCheck.Interval = 10 * time.Second
		}
		if spec.Runtime.HealthCheck.Timeout == 0 {
			spec.Runtime.HealthCheck.Timeout = 3 * time.Second
		}
		if spec.Runtime.HealthCheck.Retries == 0 {
			spec.Runtime.HealthCheck.Retries = 3
		}
		if spec.Runtime.HealthCheck.InitialDelay == 0 {
			spec.Runtime.HealthCheck.InitialDelay = 5 * time.Second
		}
	}

	// Snapshot config defaults
	if spec.Runtime.SnapshotConfig.MaxPerCapsule == 0 {
		spec.Runtime.SnapshotConfig.MaxPerCapsule = 3
	}
	if spec.Runtime.SnapshotConfig.TTL == 0 {
		spec.Runtime.SnapshotConfig.TTL = 72 * time.Hour
	}
}

func defaultPlacementRule(rule *PlacementRule) {
	if !rule.Required {
		// Only set to true if not explicitly set to false.
		// Go zero value for bool is false, so we need a different approach.
		// Convention: Required defaults to true. Users must explicitly set required: false.
		// Since we can't distinguish "not set" from "set to false" with a plain bool,
		// we always default to true here. The parser/config layer should handle
		// the explicit false case before calling DefaultSpec.
		rule.Required = true
	}
	if rule.Labels == nil {
		rule.Labels = make(Labels)
	}
}

func defaultScalingRule(rule *ScalingRule) {
	if rule.Trigger == "" {
		rule.Trigger = enums.TriggerModeEnum.All()
	}
	if rule.Cooldown == 0 {
		rule.Cooldown = 60 * time.Second
	}
}

// ValidateSpec validates a CapsuleSpec and returns all validation errors found.
func ValidateSpec(spec *CapsuleSpec) error {
	var errs []error

	if spec.Name == "" {
		errs = append(errs, ErrNameRequired)
	}
	if spec.Image == "" {
		errs = append(errs, ErrImageRequired)
	}
	if spec.Orbit == "" {
		errs = append(errs, ErrOrbitRequired)
	}
	if spec.Tier != "" && !spec.Tier.Valid() {
		errs = append(errs, ErrInvalidTier)
	}

	// Resource validation
	if spec.Resources.CPUCores < 0 {
		errs = append(errs, ErrInvalidCPU)
	}
	if spec.Resources.MemoryMB < 0 {
		errs = append(errs, ErrInvalidMemory)
	}
	if spec.Resources.DiskMB < 0 {
		errs = append(errs, ErrInvalidDisk)
	}

	// Replica validation
	if err := validateReplicas(spec.Replicas); err != nil {
		errs = append(errs, err)
	}

	// Placement rule validation
	for i, rule := range spec.PlacementRules {
		if err := validatePlacementRule(rule, i); err != nil {
			errs = append(errs, err)
		}
	}

	// Scaling rule validation
	for i, rule := range spec.ScalingRules {
		if err := validateScalingRule(rule, i); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func validateReplicas(r ReplicaConfig) error {
	if r.Exact > 0 && (r.Min > 0 || r.Max > 0) {
		return ErrReplicaConflict
	}
	if r.Exact < 0 {
		return ErrInvalidExactReplicas
	}
	if r.Exact == 0 {
		if r.Min < 0 || r.Max < 0 {
			return ErrInvalidReplicas
		}
		if r.Min > r.Max {
			return ErrInvalidReplicas
		}
	}
	return nil
}

func validatePlacementRule(rule PlacementRule, index int) error {
	if !rule.Type.Valid() {
		return fmt.Errorf("placement rule %d: invalid type %q", index, rule.Type)
	}

	// Mode is only valid for capsule type
	if rule.Type == enums.PlacementTypeEnum.Capsule() {
		if !rule.Mode.Valid() {
			return fmt.Errorf("placement rule %d: capsule type requires mode (near or away)", index)
		}
	} else {
		if rule.Mode != "" {
			return fmt.Errorf("placement rule %d: mode is only valid for capsule type", index)
		}
	}

	// Must have at least names or labels
	if len(rule.Names) == 0 && len(rule.Labels) == 0 {
		return fmt.Errorf("placement rule %d: must specify names or labels", index)
	}

	// For capsule type with near/away, labels should use "same" keyword
	if rule.Type == enums.PlacementTypeEnum.Capsule() {
		for key, val := range rule.Labels {
			if val != SameKeyword {
				return fmt.Errorf("placement rule %d: capsule affinity label %q must use %q keyword, got %q", index, key, SameKeyword, val)
			}
		}
	}

	return nil
}

func validateScalingRule(rule ScalingRule, index int) error {
	if rule.Name == "" {
		return fmt.Errorf("scaling rule %d: name is required", index)
	}
	if !rule.Trigger.Valid() {
		return fmt.Errorf("scaling rule %d: invalid trigger %q", index, rule.Trigger)
	}
	if !rule.Action.Valid() {
		return fmt.Errorf("scaling rule %d: invalid action %q", index, rule.Action)
	}
	if len(rule.Conditions) == 0 {
		return fmt.Errorf("scaling rule %d: at least one condition required", index)
	}
	if rule.Cooldown < 0 {
		return fmt.Errorf("scaling rule %d: cooldown must be non-negative", index)
	}
	return nil
}
