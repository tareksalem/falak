package service

import (
	"errors"
	"fmt"
	"strings"
)

// DefaultSpec applies project-default values to a ServiceSpec in
// place. Safe to call repeatedly and on a partially-filled spec.
// Defaults match `.claude/plans/service-networking.md`:
//   - Visibility defaults to Cluster (Decision #2).
//   - Strategy defaults to {Type: Static} when nil.
//   - Timeouts default to 5min idle and 5s connect (Decision #29).
//   - BlueGreen.Drain defaults to 30s (Decision #13).
//   - Backend.Weight defaults to 100 when zero.
//   - ServicePort.Protocol defaults to TCP when empty.
//
// DefaultSpec is a no-op on a nil receiver.
func DefaultSpec(spec *ServiceSpec) {
	if spec == nil {
		return
	}
	if spec.Visibility == "" {
		spec.Visibility = VisibilityEnum.Cluster()
	}
	if spec.Strategy == nil {
		spec.Strategy = &Strategy{Type: StrategyTypeEnum.Static()}
	}
	if spec.Strategy.Type == "" {
		spec.Strategy.Type = StrategyTypeEnum.Static()
	}
	if spec.Strategy.Type == StrategyTypeEnum.BlueGreen() &&
		spec.Strategy.BlueGreen != nil && spec.Strategy.BlueGreen.Drain <= 0 {
		spec.Strategy.BlueGreen.Drain = DefaultBlueGreenDrain
	}
	if spec.Timeouts.Idle <= 0 {
		spec.Timeouts.Idle = DefaultIdleTimeout
	}
	if spec.Timeouts.Connect <= 0 {
		spec.Timeouts.Connect = DefaultConnectTimeout
	}
	for i := range spec.Ports {
		if spec.Ports[i].Protocol == "" {
			spec.Ports[i].Protocol = ProtocolEnum.TCP()
		}
	}
	for i := range spec.Backends {
		if spec.Backends[i].Weight == 0 {
			spec.Backends[i].Weight = DefaultBackendWeight
		}
	}
}

// ValidateSpec validates a ServiceSpec and returns every violation
// joined with errors.Join. Callers can match individual sentinels via
// errors.Is. ValidateSpec does NOT mutate the spec — call DefaultSpec
// first if defaults should apply.
//
// Decision #14/#17: external visibility is rejected here as the
// "reserved-but-not-yet-supported" boundary.
// Decision #27: port_map shorthand is trusted at this layer; the
// manager performs the capsule-side check at admission.
func ValidateSpec(spec *ServiceSpec) error {
	if spec == nil {
		return ErrNameRequired
	}
	var errs []error
	if strings.TrimSpace(spec.Name) == "" {
		errs = append(errs, ErrNameRequired)
	}
	errs = append(errs, validateVisibility(spec.Visibility)...)
	errs = append(errs, validatePorts(spec.Ports)...)
	errs = append(errs, validateBackends(spec.Backends)...)
	errs = append(errs, validateStrategy(spec.Strategy, spec.Backends)...)
	return errors.Join(errs...)
}

func validateVisibility(v Visibility) []error {
	if v == "" {
		return nil
	}
	if v == VisibilityEnum.External() {
		return []error{ErrExternalVisibilityNotSupported}
	}
	if !v.IsAdmitted() {
		return []error{ErrInvalidVisibility}
	}
	return nil
}

func validatePorts(ports []ServicePort) []error {
	if len(ports) == 0 {
		return []error{ErrPortRequired}
	}
	var errs []error
	for i, p := range ports {
		if p.Port == 0 {
			errs = append(errs, fmt.Errorf("port %d: %w", i, ErrInvalidPort))
		}
		if !portNameDNSFriendly(p.Name) {
			errs = append(errs, fmt.Errorf("port %d (%q): %w", i, p.Name, ErrPortNameInvalid))
		}
		if p.Protocol != "" && !p.Protocol.IsAdmitted() {
			errs = append(errs, fmt.Errorf("port %d (%q): %w", i, p.Name, ErrInvalidProtocol))
		}
	}
	return errs
}

func validateBackends(backends []ServiceBackend) []error {
	if len(backends) == 0 {
		return []error{ErrBackendRequired}
	}
	var errs []error
	for i, b := range backends {
		if strings.TrimSpace(b.Capsule) == "" {
			errs = append(errs, fmt.Errorf("backend %d: %w", i, ErrBackendNameEmpty))
		}
		if b.Weight < 0 || b.Weight > 10000 {
			errs = append(errs, fmt.Errorf("backend %d (%q): %w", i, b.Capsule, ErrInvalidWeight))
		}
	}
	return errs
}

func validateStrategy(s *Strategy, backends []ServiceBackend) []error {
	if s == nil {
		return nil
	}
	if !s.Type.Valid() {
		return []error{ErrInvalidStrategyType}
	}
	switch s.Type {
	case StrategyTypeEnum.Static():
		return nil
	case StrategyTypeEnum.BlueGreen():
		return validateBlueGreen(s.BlueGreen, backends)
	case StrategyTypeEnum.Canary():
		return validateCanary(s.Canary, backends)
	}
	return nil
}

func validateBlueGreen(bg *BlueGreenStrategy, backends []ServiceBackend) []error {
	if bg == nil {
		return []error{ErrBlueGreenConfigMissing}
	}
	if !backendNameKnown(bg.Active, backends) {
		return []error{ErrBlueGreenActiveUnknown}
	}
	return nil
}

func validateCanary(c *CanaryStrategy, backends []ServiceBackend) []error {
	if c == nil {
		return []error{ErrCanaryConfigMissing}
	}
	var errs []error
	if !backendNameKnown(c.Target, backends) {
		errs = append(errs, ErrCanaryTargetUnknown)
	}
	if !backendNameKnown(c.From, backends) {
		errs = append(errs, ErrCanaryFromUnknown)
	}
	if c.Step <= 0 || c.Step > 100 {
		errs = append(errs, ErrInvalidStep)
	}
	if len(c.AbortOn) == 0 {
		errs = append(errs, ErrCanaryAbortOnRequired)
	}
	return errs
}

// backendNameKnown reports whether name matches any backend.Capsule.
func backendNameKnown(name string, backends []ServiceBackend) bool {
	if name == "" {
		return false
	}
	for _, b := range backends {
		if b.Capsule == name {
			return true
		}
	}
	return false
}

// portNameDNSFriendly accepts non-empty lowercase ASCII alnum with
// optional internal dashes; no leading/trailing dash.
func portNameDNSFriendly(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
