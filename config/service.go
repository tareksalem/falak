package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/tareksalem/falak/service"
)

// ServiceConfig is the declarative Service spec loaded from CUE. It
// mirrors the #Service schema in cue/falak.cue and is converted to a
// service.ServiceSpec via ToServiceSpec on node startup. The struct
// is intentionally flat-JSON-friendly so the existing CUE → JSON →
// struct loader path picks it up without bespoke decoders.
type ServiceConfig struct {
	// Name is the logical Service name. Required.
	Name string `json:"name"`

	// Visibility is "group" or "cluster". Empty defaults to "cluster";
	// "external" is rejected (see ErrExternalVisibilityNotSupported).
	Visibility string `json:"visibility,omitempty"`

	// Group is the owning group when Visibility=="group". Optional —
	// admission auto-derives from a single-group backend set when omitted.
	Group string `json:"group,omitempty"`

	// Ports declares the ingress port set. At least one required.
	Ports []ServicePortConfig `json:"ports,omitempty"`

	// Backends lists the weighted capsule targets. At least one required.
	Backends []ServiceBackendConfig `json:"backends,omitempty"`

	// Strategy selects the traffic-management variant. Nil = static.
	Strategy *StrategyConfig `json:"strategy,omitempty"`

	// Timeouts overrides the default per-Service connection timeouts.
	Timeouts *ServiceTimeoutsConfig `json:"timeouts,omitempty"`
}

// ServicePortConfig declares one ingress port the Service exposes.
type ServicePortConfig struct {
	// Name is the DNS-friendly handle (e.g. "http"). Required.
	Name string `json:"name"`

	// Port is the TCP/UDP port number (1–65535). Required.
	Port int `json:"port"`

	// Protocol is "tcp" or "udp". Empty defaults to "tcp".
	Protocol string `json:"protocol,omitempty"`
}

// ServiceBackendConfig references one capsule by name with weight + port-map.
type ServiceBackendConfig struct {
	// Capsule is the bare capsule name. Required.
	Capsule string `json:"capsule"`

	// PortMap remaps Service port-name → capsule named-port. Optional;
	// empty map = identity mapping (use the Service port name on both sides).
	PortMap map[string]string `json:"port_map,omitempty"`

	// Weight is the SWRR weight (0–10000). Zero loaded from CUE defaults
	// to 100 in the converter to match the CUE schema default.
	Weight int `json:"weight,omitempty"`
}

// StrategyConfig is the declarative strategy block.
type StrategyConfig struct {
	// Type is "static" | "canary" | "blue-green". Empty defaults to "static".
	Type string `json:"type,omitempty"`

	// Canary parameters (populated when Type=="canary").
	Canary *CanaryStrategyConfig `json:"canary,omitempty"`

	// BlueGreen parameters (populated when Type=="blue-green").
	BlueGreen *BlueGreenStrategyConfig `json:"blue_green,omitempty"`
}

// CanaryStrategyConfig declares a canary rollout.
type CanaryStrategyConfig struct {
	// Target is the backend traffic moves toward.
	Target string `json:"target,omitempty"`

	// From is the backend traffic moves away from.
	From string `json:"from,omitempty"`

	// Step is the percentage moved per tick (1–100).
	Step int `json:"step,omitempty"`

	// Interval is a Go duration string (e.g. "5m"). Empty = manual mode.
	Interval string `json:"interval,omitempty"`

	// SuccessCriteria gates progression when Interval is set.
	SuccessCriteria []string `json:"success_criteria,omitempty"`

	// AbortOn fully reverts the canary when any condition matches.
	AbortOn []string `json:"abort_on,omitempty"`
}

// BlueGreenStrategyConfig declares a blue-green flip.
type BlueGreenStrategyConfig struct {
	// Active is the backend currently serving traffic.
	Active string `json:"active,omitempty"`

	// Drain is a Go duration string (e.g. "30s"). Empty = default 30s.
	Drain string `json:"drain,omitempty"`
}

// ServiceTimeoutsConfig overrides per-Service connection timeouts.
type ServiceTimeoutsConfig struct {
	// Idle closes after no traffic for this duration. Empty = default 5m.
	Idle string `json:"idle,omitempty"`

	// Connect bounds the proxy→backend dial. Empty = default 5s.
	Connect string `json:"connect,omitempty"`
}

// ErrExternalVisibilityNotSupported is returned by ToServiceSpec when
// the user requests `visibility: external`. v1 only supports group +
// cluster; external is reserved (locked decision #17 in
// service-networking.md). Re-exported so callers can match without
// importing the service package directly.
var ErrExternalVisibilityNotSupported = service.ErrExternalVisibilityNotSupported

// ToServiceSpec converts a declarative ServiceConfig into a
// service.ServiceSpec ready to hand to service.Manager.Create. The
// converter applies the CUE-side defaults so the manager's DefaultSpec
// pass never has to override an explicit user value:
//
//   - Visibility "" → "cluster"
//   - Strategy   nil → {Type: "static"}
//   - Strategy.Type "" → "static"
//   - BlueGreen.Drain "" → 30s
//   - Timeouts.Idle / Connect "" → 5m / 5s
//   - Port.Protocol "" → "tcp"
//   - Backend.PortMap nil → identity map (port → port)
//   - Backend.Weight 0 → 100
//
// Duration strings ("5m", "30s") parse via parseOptionalDuration.
// Returns ErrExternalVisibilityNotSupported when Visibility=="external".
func (c *ServiceConfig) ToServiceSpec() (service.ServiceSpec, error) {
	if c == nil {
		return service.ServiceSpec{}, errors.New("config: nil ServiceConfig")
	}
	spec := service.ServiceSpec{
		Name:  c.Name,
		Group: c.Group,
	}

	vis, err := convertVisibility(c.Visibility)
	if err != nil {
		return spec, err
	}
	spec.Visibility = vis

	spec.Ports = make([]service.ServicePort, 0, len(c.Ports))
	for i, p := range c.Ports {
		proto, err := convertProtocol(p.Protocol)
		if err != nil {
			return spec, fmt.Errorf("port %d (%q): %w", i, p.Name, err)
		}
		if p.Port < 1 || p.Port > 65535 {
			return spec, fmt.Errorf("port %d (%q): invalid port number %d", i, p.Name, p.Port)
		}
		spec.Ports = append(spec.Ports, service.ServicePort{
			Name:     p.Name,
			Port:     uint16(p.Port),
			Protocol: proto,
		})
	}

	portNames := collectPortNames(spec.Ports)
	spec.Backends = make([]service.ServiceBackend, 0, len(c.Backends))
	for _, b := range c.Backends {
		w := int32(b.Weight)
		if w == 0 {
			w = service.DefaultBackendWeight
		}
		spec.Backends = append(spec.Backends, service.ServiceBackend{
			Capsule: b.Capsule,
			PortMap: resolvePortMap(b.PortMap, portNames),
			Weight:  w,
		})
	}

	if c.Strategy != nil {
		s, err := convertStrategy(c.Strategy)
		if err != nil {
			return spec, err
		}
		spec.Strategy = s
	} else {
		spec.Strategy = &service.Strategy{Type: service.StrategyTypeEnum.Static()}
	}

	timeouts, err := convertTimeouts(c.Timeouts)
	if err != nil {
		return spec, err
	}
	spec.Timeouts = timeouts

	return spec, nil
}

// convertVisibility maps the JSON string to the typed service.Visibility,
// applying the cluster default and rejecting "external" explicitly.
func convertVisibility(v string) (service.Visibility, error) {
	switch v {
	case "":
		return service.VisibilityEnum.Cluster(), nil
	case "group":
		return service.VisibilityEnum.Group(), nil
	case "cluster":
		return service.VisibilityEnum.Cluster(), nil
	case "external":
		return "", ErrExternalVisibilityNotSupported
	}
	return "", fmt.Errorf("config: invalid visibility %q (want group|cluster)", v)
}

// convertProtocol maps the JSON string to the typed service.Protocol,
// applying the TCP default. "http" is reserved for L7 (rejected here).
func convertProtocol(p string) (service.Protocol, error) {
	switch p {
	case "":
		return service.ProtocolEnum.TCP(), nil
	case "tcp":
		return service.ProtocolEnum.TCP(), nil
	case "udp":
		return service.ProtocolEnum.UDP(), nil
	}
	return "", fmt.Errorf("config: invalid protocol %q (want tcp|udp)", p)
}

// collectPortNames returns the set of declared port names for the
// identity-mapping shorthand.
func collectPortNames(ports []service.ServicePort) []string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, p.Name)
	}
	return out
}

// resolvePortMap fills in the CUE-side identity-map shorthand: when a
// backend omits port_map, every Service port maps to its same name on
// the backend side. An explicit non-empty map is returned unchanged.
func resolvePortMap(explicit map[string]string, ports []string) map[string]string {
	if len(explicit) > 0 {
		// Copy to detach from the loader's map.
		out := make(map[string]string, len(explicit))
		for k, v := range explicit {
			out[k] = v
		}
		return out
	}
	if len(ports) == 0 {
		return nil
	}
	out := make(map[string]string, len(ports))
	for _, name := range ports {
		out[name] = name
	}
	return out
}

// convertStrategy dispatches on Type and builds the typed Strategy.
func convertStrategy(s *StrategyConfig) (*service.Strategy, error) {
	t := s.Type
	if t == "" {
		t = string(service.StrategyTypeEnum.Static())
	}
	out := &service.Strategy{}
	switch t {
	case string(service.StrategyTypeEnum.Static()):
		out.Type = service.StrategyTypeEnum.Static()
	case string(service.StrategyTypeEnum.Canary()):
		out.Type = service.StrategyTypeEnum.Canary()
		canary, err := convertCanary(s.Canary)
		if err != nil {
			return nil, err
		}
		out.Canary = canary
	case string(service.StrategyTypeEnum.BlueGreen()):
		out.Type = service.StrategyTypeEnum.BlueGreen()
		bg, err := convertBlueGreen(s.BlueGreen)
		if err != nil {
			return nil, err
		}
		out.BlueGreen = bg
	default:
		return nil, fmt.Errorf("config: invalid strategy type %q (want static|canary|blue-green)", t)
	}
	return out, nil
}

// convertCanary parses durations and lifts the struct into the typed form.
func convertCanary(c *CanaryStrategyConfig) (*service.CanaryStrategy, error) {
	if c == nil {
		return nil, errors.New("config: strategy type=canary requires a canary block")
	}
	interval, err := parseOptionalDuration(c.Interval)
	if err != nil {
		return nil, fmt.Errorf("strategy.canary.interval: %w", err)
	}
	return &service.CanaryStrategy{
		Target:          c.Target,
		From:            c.From,
		Step:            int32(c.Step),
		Interval:        interval,
		SuccessCriteria: append([]string(nil), c.SuccessCriteria...),
		AbortOn:         append([]string(nil), c.AbortOn...),
	}, nil
}

// convertBlueGreen parses the drain window and lifts the struct.
func convertBlueGreen(bg *BlueGreenStrategyConfig) (*service.BlueGreenStrategy, error) {
	if bg == nil {
		return nil, errors.New("config: strategy type=blue-green requires a blue_green block")
	}
	drain, err := parseOptionalDuration(bg.Drain)
	if err != nil {
		return nil, fmt.Errorf("strategy.blue_green.drain: %w", err)
	}
	if drain <= 0 {
		drain = service.DefaultBlueGreenDrain
	}
	return &service.BlueGreenStrategy{
		Active: bg.Active,
		Drain:  drain,
	}, nil
}

// convertTimeouts applies defaults so callers don't have to.
func convertTimeouts(t *ServiceTimeoutsConfig) (service.ServiceTimeouts, error) {
	out := service.ServiceTimeouts{
		Idle:    service.DefaultIdleTimeout,
		Connect: service.DefaultConnectTimeout,
	}
	if t == nil {
		return out, nil
	}
	idle, err := parseOptionalDuration(t.Idle)
	if err != nil {
		return out, fmt.Errorf("timeouts.idle: %w", err)
	}
	if idle > 0 {
		out.Idle = idle
	}
	connect, err := parseOptionalDuration(t.Connect)
	if err != nil {
		return out, fmt.Errorf("timeouts.connect: %w", err)
	}
	if connect > 0 {
		out.Connect = connect
	}
	return out, nil
}

// _typeAssert ensures the converter stays type-safe against the
// upstream service package. The compiler discards it.
var _ = time.Duration(0)
