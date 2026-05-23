package service

import "errors"

// Spec-validation sentinels returned by ValidateSpec (joined via
// errors.Join so callers can match any individual one via errors.Is).
var (
	ErrNameRequired                   = errors.New("service: name is required")
	ErrPortRequired                   = errors.New("service: at least one port is required")
	ErrBackendRequired                = errors.New("service: at least one backend is required")
	ErrInvalidVisibility              = errors.New("service: visibility must be group or cluster")
	ErrExternalVisibilityNotSupported = errors.New("service: external visibility is reserved and not supported in v1")
	ErrCanaryTargetUnknown            = errors.New("service: canary target backend not present in backends")
	ErrCanaryFromUnknown              = errors.New("service: canary from backend not present in backends")
	ErrBlueGreenActiveUnknown         = errors.New("service: blue-green active backend not present in backends")
	ErrInvalidWeight                  = errors.New("service: backend weight must be between 0 and 10000")
	ErrInvalidStep                    = errors.New("service: canary step must be between 1 and 100")
	ErrInvalidPort                    = errors.New("service: port must be in (0, 65535]")
	ErrInvalidProtocol                = errors.New("service: protocol must be tcp or udp (http reserved)")
	ErrInvalidStrategyType            = errors.New("service: strategy type must be static, blue-green, or canary")
	ErrPortNameInvalid                = errors.New("service: port name must be DNS-friendly (lowercase alnum + dashes)")
	ErrBackendNameEmpty               = errors.New("service: backend capsule name must be non-empty")
	ErrCanaryConfigMissing            = errors.New("service: canary strategy requires a Canary block with target/from/step")
	ErrCanaryAbortOnRequired          = errors.New("service: canary strategy requires at least one abort_on condition")
	ErrBlueGreenConfigMissing         = errors.New("service: blue-green strategy requires a BlueGreen block with active")
)
