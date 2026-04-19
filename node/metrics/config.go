package metrics

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Environment variables that override the configuration file values.
const (
	envInterval = "FALAK_METRICS_INTERVAL"
	envEnabled  = "FALAK_METRICS_ENABLED"
)

// Default sampling interval applied when neither the config file nor the
// environment variable specifies a value. One second is the smallest
// interval gopsutil can reliably honor across platforms while still
// keeping CPU overhead negligible (well under 1% of one core).
const defaultInterval = 1 * time.Second

// Config controls the metrics subsystem. It is loaded by NewConfig from
// (in priority order) environment variables, an explicit code-supplied
// override, and the built-in defaults. The CUE configuration loader
// supplies the explicit override at node start.
//
// Config is intentionally small: every knob has a clear single purpose,
// no implicit defaults beyond what's documented here, and validation
// happens at construction time so callers see errors immediately.
type Config struct {
	// Enabled toggles the entire subsystem. When false the collector loop
	// does not start, no metrics are stored, and no PubSub messages are
	// sent or received. The provider then returns zero-valued snapshots,
	// which the gravity calculator handles via its phonebook fallback.
	Enabled bool

	// Interval is how often the collector samples local metrics and
	// publishes them to peers. Must be at least 100ms to avoid pathological
	// CPU overhead from gopsutil itself.
	Interval time.Duration
}

// DefaultConfig returns the built-in default configuration: enabled, 1s
// interval. Used when no other source of configuration is provided.
func DefaultConfig() Config {
	return Config{
		Enabled:  true,
		Interval: defaultInterval,
	}
}

// NewConfig builds a Config by layering environment variables on top of
// the supplied base. Pass DefaultConfig() as the base for normal use, or
// a CUE-derived value when the operator has overridden defaults.
//
// Environment variables take precedence over the base because operators
// reach for env vars when they need to flip a switch quickly without
// touching configuration files.
//
// Returns an error when the environment values are malformed (e.g. an
// unparseable duration string) or when the resulting interval is below
// the minimum.
func NewConfig(base Config) (Config, error) {
	cfg := base

	if v, ok := os.LookupEnv(envInterval); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("metrics: invalid %s=%q: %w", envInterval, v, err)
		}
		cfg.Interval = d
	}

	if v, ok := os.LookupEnv(envEnabled); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("metrics: invalid %s=%q: %w", envEnabled, v, err)
		}
		cfg.Enabled = b
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// minimumInterval is the smallest acceptable sampling interval. Anything
// below this would be self-defeating: gopsutil itself takes ~tens of
// milliseconds for some calls and a sub-100ms loop would spend most of
// its time sampling instead of letting the workload run.
const minimumInterval = 100 * time.Millisecond

// Validate checks the Config for unacceptable values. Returns an error
// describing the first failure or nil when the configuration is sound.
func (c Config) Validate() error {
	if c.Interval < minimumInterval {
		return fmt.Errorf("metrics: interval %v is below minimum %v", c.Interval, minimumInterval)
	}
	return nil
}

// ErrDisabled is returned by collector / manager methods that cannot
// produce data when the subsystem is disabled. Callers can use it to
// short-circuit gracefully.
var ErrDisabled = errors.New("metrics: subsystem is disabled")
