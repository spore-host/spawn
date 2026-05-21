package queue

import (
	"fmt"
	"math"
	"math/rand" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used
	"time"
)

// CalculateBackoff calculates the retry delay based on the retry configuration
func CalculateBackoff(cfg *RetryConfig, attempt int) (time.Duration, error) {
	if cfg == nil {
		return 5 * time.Second, nil
	}

	baseDelay := 5 * time.Second
	if cfg.BaseDelay != "" {
		parsed, err := time.ParseDuration(cfg.BaseDelay)
		if err != nil {
			return 0, fmt.Errorf("invalid base_delay: %w", err)
		}
		baseDelay = parsed
	}

	maxDelay := 5 * time.Minute
	if cfg.MaxDelay != "" {
		parsed, err := time.ParseDuration(cfg.MaxDelay)
		if err != nil {
			return 0, fmt.Errorf("invalid max_delay: %w", err)
		}
		maxDelay = parsed
	}

	var delay time.Duration

	switch cfg.Backoff {
	case "fixed", "":
		delay = baseDelay

	case "exponential":
		// delay = base * 2^(attempt-1)
		multiplier := math.Pow(2, float64(attempt-1))
		delay = time.Duration(float64(baseDelay) * multiplier)

	case "exponential-jitter":
		// delay = base * 2^(attempt-1) * (1 + random(-jitter, +jitter))
		multiplier := math.Pow(2, float64(attempt-1))
		exponential := time.Duration(float64(baseDelay) * multiplier)

		// Apply jitter (default 0.0 = no jitter)
		jitter := cfg.Jitter
		if jitter < 0 {
			jitter = 0
		} else if jitter > 1 {
			jitter = 1
		}

		jitterFactor := 1.0 + (rand.Float64()*2-1)*jitter
		delay = time.Duration(float64(exponential) * jitterFactor)

	default:
		return 0, fmt.Errorf("unknown backoff strategy: %s", cfg.Backoff)
	}

	// Cap at max delay
	if delay > maxDelay {
		delay = maxDelay
	}

	// Ensure minimum delay of 100ms
	if delay < 100*time.Millisecond {
		delay = 100 * time.Millisecond
	}

	return delay, nil
}

// ShouldRetry determines if a job should be retried based on exit code
func ShouldRetry(cfg *RetryConfig, exitCode int) bool {
	if cfg == nil {
		return true // Default: retry all failures
	}

	// Check don't retry list first (higher priority)
	for _, code := range cfg.DontRetryOnCodes {
		if code == exitCode {
			return false
		}
	}

	// If retry_on_codes is specified, only retry those codes
	if len(cfg.RetryOnCodes) > 0 {
		for _, code := range cfg.RetryOnCodes {
			if code == exitCode {
				return true
			}
		}
		return false // Exit code not in whitelist
	}

	// Default: retry all failures not in dont_retry list
	return true
}
