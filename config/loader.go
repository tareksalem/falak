package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	"cuelang.org/go/cue/errors"
)

// Load reads a CUE config file and decodes it into a NodeConfig.
// The schema file (falak.cue) is loaded from schemaDir if provided,
// otherwise only the user's config file is validated by CUE's own type system.
func Load(configPath string, schemaDir string) (*NodeConfig, error) {
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", configPath, err)
	}

	ctx := cuecontext.New()

	// If schema directory provided, load schema first and unify
	var val cue.Value
	if schemaDir != "" {
		schemaPath := filepath.Join(schemaDir, "falak.cue")
		schemaData, err := os.ReadFile(schemaPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read schema file %s: %w", schemaPath, err)
		}

		schemaVal := ctx.CompileBytes(schemaData, cue.Filename(schemaPath))
		if schemaVal.Err() != nil {
			return nil, fmt.Errorf("schema error: %w", schemaVal.Err())
		}

		configVal := ctx.CompileBytes(configData, cue.Filename(configPath))
		if configVal.Err() != nil {
			return nil, fmt.Errorf("config parse error: %w", configVal.Err())
		}

		val = schemaVal.Unify(configVal)
	} else {
		val = ctx.CompileBytes(configData, cue.Filename(configPath))
	}

	if val.Err() != nil {
		return nil, formatCueError(val.Err())
	}

	// Validate the value is concrete and complete
	if err := val.Validate(cue.Concrete(true)); err != nil {
		return nil, formatCueError(err)
	}

	// Decode into Go struct via JSON intermediate
	jsonBytes, err := val.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config to JSON: %w", err)
	}

	var cfg NodeConfig
	if err := json.Unmarshal(jsonBytes, &cfg); err != nil {
		return nil, fmt.Errorf("failed to decode config: %w", err)
	}

	// Apply defaults for fields that may not be set
	applyDefaults(&cfg)

	// Validate semantic constraints that CUE can't express
	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// LoadFromString parses CUE config from a string. Useful for testing.
func LoadFromString(cueSource string) (*NodeConfig, error) {
	ctx := cuecontext.New()

	val := ctx.CompileString(cueSource)
	if val.Err() != nil {
		return nil, formatCueError(val.Err())
	}

	if err := val.Validate(cue.Concrete(true)); err != nil {
		return nil, formatCueError(err)
	}

	jsonBytes, err := val.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config to JSON: %w", err)
	}

	var cfg NodeConfig
	if err := json.Unmarshal(jsonBytes, &cfg); err != nil {
		return nil, fmt.Errorf("failed to decode config: %w", err)
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// applyDefaults fills in default values for fields that CUE defaults may not cover
// when loaded without the schema.
func applyDefaults(cfg *NodeConfig) {
	if cfg.Region == "" {
		cfg.Region = "default"
	}
	if cfg.Datacenter == "" {
		cfg.Datacenter = "default"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
}

// validate checks semantic constraints beyond what CUE types can express.
func validate(cfg *NodeConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("config: name is required")
	}

	if cfg.Port < 0 || cfg.Port > 65535 {
		return fmt.Errorf("config: port must be 0-65535, got %d", cfg.Port)
	}

	for path, cluster := range cfg.Clusters {
		if len(cluster.PSK) < 32 {
			return fmt.Errorf("config: cluster %q PSK must be at least 32 characters, got %d", path, len(cluster.PSK))
		}

		if cluster.Certificates != nil {
			if cluster.Certificates.CACert == "" {
				return fmt.Errorf("config: cluster %q certificates.ca_cert is required when certificates block is present", path)
			}
			if cluster.Certificates.CAKey != "" {
				if _, err := os.Stat(cluster.Certificates.CAKey); os.IsNotExist(err) {
					return fmt.Errorf("config: cluster %q ca_key file not found: %s", path, cluster.Certificates.CAKey)
				}
			}
			if cluster.Certificates.CACert != "" {
				if _, err := os.Stat(cluster.Certificates.CACert); os.IsNotExist(err) {
					return fmt.Errorf("config: cluster %q ca_cert file not found: %s", path, cluster.Certificates.CACert)
				}
			}
			if cluster.Certificates.NodeCert != "" {
				if _, err := os.Stat(cluster.Certificates.NodeCert); os.IsNotExist(err) {
					return fmt.Errorf("config: cluster %q node_cert file not found: %s", path, cluster.Certificates.NodeCert)
				}
			}
			if cluster.Certificates.NodeKey != "" {
				if _, err := os.Stat(cluster.Certificates.NodeKey); os.IsNotExist(err) {
					return fmt.Errorf("config: cluster %q node_key file not found: %s", path, cluster.Certificates.NodeKey)
				}
			}
		}
	}

	return nil
}

// formatCueError converts CUE errors into human-readable messages with file/line info.
func formatCueError(err error) error {
	if err == nil {
		return nil
	}

	cueErrs := errors.Errors(err)
	if len(cueErrs) == 0 {
		return err
	}

	msg := "config validation failed:\n"
	for _, e := range cueErrs {
		pos := errors.Positions(e)
		if len(pos) > 0 {
			for _, p := range pos {
				msg += fmt.Sprintf("  %s: %s\n", p.String(), e.Error())
			}
		} else {
			msg += fmt.Sprintf("  %s\n", e.Error())
		}
	}

	return fmt.Errorf("%s", msg)
}
