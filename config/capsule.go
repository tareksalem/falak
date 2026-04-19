package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tareksalem/falak/capsule"
)

// ToCapsuleSpec converts a declarative CapsuleConfig (loaded from CUE) into
// a capsule.CapsuleSpec suitable for capsule.Manager.Create.
//
// Duration strings (e.g. "60s", "5m") are parsed via time.ParseDuration.
// Size strings (e.g. "512MB", "2GB") are parsed via parseSize.
//
// Returns an error if any field fails to parse.
func (c *CapsuleConfig) ToCapsuleSpec() (capsule.CapsuleSpec, error) {
	spec := capsule.CapsuleSpec{
		Name:        c.Name,
		Image:       c.Image,
		ImageAlias:  c.ImageAlias,
		ImageDigest: c.ImageDigest,
		Orbit:       c.Orbit,
		Tier:        capsule.Tier(c.Tier),
		Labels:      c.Labels,
		Command:     c.Command,
	}

	if c.Resources != nil {
		memMB, err := parseSizeToMB(c.Resources.Memory)
		if err != nil {
			return spec, fmt.Errorf("resources.memory: %w", err)
		}
		memMaxMB, err := parseSizeToMB(c.Resources.MemoryMax)
		if err != nil {
			return spec, fmt.Errorf("resources.memory_max: %w", err)
		}
		diskMB, err := parseSizeToMB(c.Resources.Disk)
		if err != nil {
			return spec, fmt.Errorf("resources.disk: %w", err)
		}
		spec.Resources = capsule.ResourceRequirements{
			CPUCores:    int32(c.Resources.CPU),
			CPUCoresMax: int32(c.Resources.CPUMax),
			MemoryMB:    memMB,
			MemoryMBMax: memMaxMB,
			DiskMB:      diskMB,
		}
	}

	if c.Replicas != nil {
		spec.Replicas = capsule.ReplicaConfig{
			Min:   int32(c.Replicas.Min),
			Max:   int32(c.Replicas.Max),
			Exact: int32(c.Replicas.Exact),
		}
	}

	if c.Scaling != nil {
		for i, rule := range c.Scaling.Rules {
			cooldown := time.Duration(0)
			if rule.Cooldown != "" {
				d, err := time.ParseDuration(rule.Cooldown)
				if err != nil {
					return spec, fmt.Errorf("scaling.rules[%d].cooldown: %w", i, err)
				}
				cooldown = d
			}
			trigger := capsule.TriggerMode(rule.Trigger)
			if trigger == "" {
				trigger = capsule.TriggerModeEnum.All()
			}
			spec.ScalingRules = append(spec.ScalingRules, capsule.ScalingRule{
				Name:       rule.Name,
				Trigger:    trigger,
				Conditions: rule.Conditions,
				Action:     capsule.ScalingAction(rule.Action),
				Cooldown:   cooldown,
			})
		}
	}

	for i, p := range c.Placement {
		required := true
		if p.Required != nil {
			required = *p.Required
		}
		spec.PlacementRules = append(spec.PlacementRules, capsule.PlacementRule{
			Name:     p.Name,
			Type:     capsule.PlacementType(p.Type),
			Mode:     capsule.PlacementMode(p.Mode),
			Names:    p.Targets,
			Labels:   p.Labels,
			Required: required,
		})
		_ = i
	}

	if c.Runtime != nil {
		spec.Runtime.Env = c.Runtime.Env

		if c.Runtime.Network != nil {
			spec.Runtime.Network.Mode = capsule.NetworkMode(c.Runtime.Network.Mode)
			for _, p := range c.Runtime.Network.Ports {
				proto := p.Protocol
				if proto == "" {
					proto = "tcp"
				}
				spec.Runtime.Network.Ports = append(spec.Runtime.Network.Ports, capsule.PortMapping{
					Name:          p.Name,
					ContainerPort: uint16(p.Container),
					HostPort:      uint16(p.Host),
					Protocol:      proto,
				})
			}
		}

		if c.Runtime.HealthCheck != nil {
			hc := c.Runtime.HealthCheck
			interval, _ := parseOptionalDuration(hc.Interval)
			timeout, _ := parseOptionalDuration(hc.Timeout)
			initialDelay, _ := parseOptionalDuration(hc.InitialDelay)
			spec.Runtime.HealthCheck = &capsule.HealthCheck{
				Type:         capsule.HealthCheckType(hc.Type),
				Path:         hc.Path,
				Port:         uint16(hc.Port),
				Interval:     interval,
				Timeout:      timeout,
				Retries:      hc.Retries,
				InitialDelay: initialDelay,
			}
		}

		if c.Runtime.FailurePolicy != nil {
			fp := c.Runtime.FailurePolicy
			graceful, _ := parseOptionalDuration(fp.GracefulTimeout)
			spec.Runtime.FailurePolicy.RestartLimit = fp.RestartLimit
			spec.Runtime.FailurePolicy.MaxNodeAttempts = fp.MaxNodeAttempts
			spec.Runtime.FailurePolicy.GracefulTimeout = graceful
		}

		if c.Runtime.LogRetention != nil {
			spec.Runtime.LogRetention.MaxFileSizeMB = c.Runtime.LogRetention.MaxFileSizeMB
			spec.Runtime.LogRetention.MaxFiles = c.Runtime.LogRetention.MaxFiles
		}

		if c.Runtime.StatsInterval != "" {
			d, err := time.ParseDuration(c.Runtime.StatsInterval)
			if err != nil {
				return spec, fmt.Errorf("runtime.stats_interval: %w", err)
			}
			spec.Runtime.StatsInterval = d
		}

		if c.Runtime.Snapshot != nil {
			spec.Runtime.SnapshotConfig.MaxPerCapsule = c.Runtime.Snapshot.MaxPerCapsule
			if c.Runtime.Snapshot.TTL != "" {
				d, err := time.ParseDuration(c.Runtime.Snapshot.TTL)
				if err != nil {
					return spec, fmt.Errorf("runtime.snapshot.ttl: %w", err)
				}
				spec.Runtime.SnapshotConfig.TTL = d
			}
		}
	}

	if c.Advanced != nil && c.Advanced.Momentum != nil {
		idleTimeout, err := parseOptionalDuration(c.Advanced.Momentum.IdleTimeout)
		if err != nil {
			return spec, fmt.Errorf("advanced.momentum.idle_timeout: %w", err)
		}
		spec.MomentumConfig = capsule.MomentumConfig{
			Base:           int32(c.Advanced.Momentum.Base),
			BoostOnTraffic: c.Advanced.Momentum.BoostOnTraffic,
			ReduceOnIdle:   c.Advanced.Momentum.ReduceOnIdle,
			IdleTimeout:    idleTimeout,
		}
	}

	return spec, nil
}

// parseOptionalDuration returns 0 for empty input, parses otherwise.
func parseOptionalDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// parseSizeToMB parses a human-friendly size string into megabytes.
// Accepts suffixes: KB, MB, GB, TB (case-insensitive, binary interpretation).
// Also accepts bare numbers as MB. Returns 0 for empty input.
func parseSizeToMB(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)

	multiplier := int64(1) // default: MB
	numStr := s

	switch {
	case strings.HasSuffix(upper, "TB"):
		multiplier = 1024 * 1024
		numStr = s[:len(s)-2]
	case strings.HasSuffix(upper, "GB"):
		multiplier = 1024
		numStr = s[:len(s)-2]
	case strings.HasSuffix(upper, "MB"):
		multiplier = 1
		numStr = s[:len(s)-2]
	case strings.HasSuffix(upper, "KB"):
		// Round up: 1 KB ≈ 1/1024 MB, but we return int64 MB, so this is 0.
		numStr = s[:len(s)-2]
		n, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid size %q", s)
		}
		return int64(n / 1024), nil
	}

	n, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(n * float64(multiplier)), nil
}
