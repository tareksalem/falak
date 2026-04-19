package shared

import (
	"context"
	"time"
)

// RetryConfig holds configuration for retry operations.
type RetryConfig struct {
	MaxRetries int
	Delay      time.Duration
}

// DefaultRetryConfig returns the default retry configuration.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries: 3,
		Delay:      100 * time.Millisecond,
	}
}

// Retry executes the given function with retries.
// Returns the last error if all retries fail.
func Retry(ctx context.Context, cfg RetryConfig, fn func() error) error {
	var lastErr error

	for attempt := 0; attempt < cfg.MaxRetries; attempt++ {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err

			// Don't wait after the last attempt
			if attempt < cfg.MaxRetries-1 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(cfg.Delay):
				}
			}
		}
	}

	return lastErr
}

// RetryWithResult executes the given function with retries and returns a result.
func RetryWithResult[T any](ctx context.Context, cfg RetryConfig, fn func() (T, error)) (T, error) {
	var lastErr error
	var zero T

	for attempt := 0; attempt < cfg.MaxRetries; attempt++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}
		lastErr = err

		// Don't wait after the last attempt
		if attempt < cfg.MaxRetries-1 {
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(cfg.Delay):
			}
		}
	}

	return zero, lastErr
}
