package queue

import (
	"testing"
	"time"
)

func TestCalculateBackoff_Fixed(t *testing.T) {
	cfg := &RetryConfig{
		Backoff:   "fixed",
		BaseDelay: "3s",
	}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: 3 * time.Second},
		{attempt: 2, want: 3 * time.Second},
		{attempt: 5, want: 3 * time.Second},
	}

	for _, tt := range tests {
		got, err := CalculateBackoff(cfg, tt.attempt)
		if err != nil {
			t.Errorf("CalculateBackoff() error = %v", err)
			continue
		}
		if got != tt.want {
			t.Errorf("CalculateBackoff(attempt=%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestCalculateBackoff_Exponential(t *testing.T) {
	cfg := &RetryConfig{
		Backoff:   "exponential",
		BaseDelay: "1s",
		MaxDelay:  "1m",
	}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: 1 * time.Second},   // 1 * 2^0
		{attempt: 2, want: 2 * time.Second},   // 1 * 2^1
		{attempt: 3, want: 4 * time.Second},   // 1 * 2^2
		{attempt: 4, want: 8 * time.Second},   // 1 * 2^3
		{attempt: 5, want: 16 * time.Second},  // 1 * 2^4
		{attempt: 10, want: 60 * time.Second}, // Capped at max_delay
	}

	for _, tt := range tests {
		got, err := CalculateBackoff(cfg, tt.attempt)
		if err != nil {
			t.Errorf("CalculateBackoff() error = %v", err)
			continue
		}
		if got != tt.want {
			t.Errorf("CalculateBackoff(attempt=%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestCalculateBackoff_ExponentialJitter(t *testing.T) {
	cfg := &RetryConfig{
		Backoff:   "exponential-jitter",
		BaseDelay: "1s",
		MaxDelay:  "1m",
		Jitter:    0.3, // +/- 30%
	}

	// Run multiple times to test jitter randomization
	for i := 0; i < 10; i++ {
		attempt := 3
		got, err := CalculateBackoff(cfg, attempt)
		if err != nil {
			t.Errorf("CalculateBackoff() error = %v", err)
			continue
		}

		// Expected: 4s * (1 +/- 0.3) = 2.8s - 5.2s
		expectedBase := 4 * time.Second
		minExpected := time.Duration(float64(expectedBase) * 0.7) // 2.8s
		maxExpected := time.Duration(float64(expectedBase) * 1.3) // 5.2s

		if got < minExpected || got > maxExpected {
			t.Errorf("CalculateBackoff(attempt=%d) = %v, want between %v and %v", attempt, got, minExpected, maxExpected)
		}
	}
}

func TestCalculateBackoff_JitterCapping(t *testing.T) {
	tests := []struct {
		name   string
		jitter float64
		want   float64
	}{
		{name: "negative jitter", jitter: -0.5, want: 0.0},
		{name: "zero jitter", jitter: 0.0, want: 0.0},
		{name: "normal jitter", jitter: 0.3, want: 0.3},
		{name: "excessive jitter", jitter: 1.5, want: 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &RetryConfig{
				Backoff:   "exponential-jitter",
				BaseDelay: "1s",
				Jitter:    tt.jitter,
			}

			// Jitter is applied internally, we just verify it doesn't error
			_, err := CalculateBackoff(cfg, 1)
			if err != nil {
				t.Errorf("CalculateBackoff() error = %v", err)
			}
		})
	}
}

func TestCalculateBackoff_MaxDelay(t *testing.T) {
	cfg := &RetryConfig{
		Backoff:   "exponential",
		BaseDelay: "1s",
		MaxDelay:  "10s",
	}

	// Attempt 10: would be 512s without cap
	got, err := CalculateBackoff(cfg, 10)
	if err != nil {
		t.Errorf("CalculateBackoff() error = %v", err)
	}
	want := 10 * time.Second
	if got != want {
		t.Errorf("CalculateBackoff(attempt=10) = %v, want %v (should be capped)", got, want)
	}
}

func TestCalculateBackoff_DefaultValues(t *testing.T) {
	cfg := &RetryConfig{
		Backoff: "exponential",
		// No BaseDelay or MaxDelay specified
	}

	got, err := CalculateBackoff(cfg, 1)
	if err != nil {
		t.Errorf("CalculateBackoff() error = %v", err)
	}
	// Should use default base_delay of 5s
	want := 5 * time.Second
	if got != want {
		t.Errorf("CalculateBackoff() = %v, want %v (default)", got, want)
	}
}

func TestCalculateBackoff_NilConfig(t *testing.T) {
	got, err := CalculateBackoff(nil, 1)
	if err != nil {
		t.Errorf("CalculateBackoff() error = %v", err)
	}
	// Should return default 5s
	want := 5 * time.Second
	if got != want {
		t.Errorf("CalculateBackoff(nil) = %v, want %v", got, want)
	}
}

func TestCalculateBackoff_InvalidDuration(t *testing.T) {
	cfg := &RetryConfig{
		Backoff:   "fixed",
		BaseDelay: "invalid",
	}

	_, err := CalculateBackoff(cfg, 1)
	if err == nil {
		t.Error("CalculateBackoff() expected error for invalid base_delay, got nil")
	}
}

func TestCalculateBackoff_UnknownStrategy(t *testing.T) {
	cfg := &RetryConfig{
		Backoff: "quantum-tunneling",
	}

	_, err := CalculateBackoff(cfg, 1)
	if err == nil {
		t.Error("CalculateBackoff() expected error for unknown strategy, got nil")
	}
}

func TestShouldRetry_AllowAll(t *testing.T) {
	cfg := &RetryConfig{
		// No retry restrictions
	}

	tests := []int{0, 1, 2, 127, 255}
	for _, exitCode := range tests {
		if !ShouldRetry(cfg, exitCode) {
			t.Errorf("ShouldRetry(%d) = false, want true (should retry all by default)", exitCode)
		}
	}
}

func TestShouldRetry_NilConfig(t *testing.T) {
	// Nil config should retry everything
	if !ShouldRetry(nil, 1) {
		t.Error("ShouldRetry(nil, 1) = false, want true")
	}
}

func TestShouldRetry_DontRetryList(t *testing.T) {
	cfg := &RetryConfig{
		DontRetryOnCodes: []int{2, 127},
	}

	tests := []struct {
		exitCode int
		want     bool
	}{
		{exitCode: 0, want: true},    // Success - would retry if it failed
		{exitCode: 1, want: true},    // Generic error - retry
		{exitCode: 2, want: false},   // In dont_retry list
		{exitCode: 127, want: false}, // In dont_retry list
		{exitCode: 137, want: true},  // Not in dont_retry list
	}

	for _, tt := range tests {
		got := ShouldRetry(cfg, tt.exitCode)
		if got != tt.want {
			t.Errorf("ShouldRetry(%d) = %v, want %v", tt.exitCode, got, tt.want)
		}
	}
}

func TestShouldRetry_RetryOnList(t *testing.T) {
	cfg := &RetryConfig{
		RetryOnCodes: []int{1, 137, 143},
	}

	tests := []struct {
		exitCode int
		want     bool
	}{
		{exitCode: 1, want: true},    // In retry list
		{exitCode: 2, want: false},   // Not in retry list
		{exitCode: 137, want: true},  // SIGKILL - in retry list
		{exitCode: 143, want: true},  // SIGTERM - in retry list
		{exitCode: 127, want: false}, // Not in retry list
	}

	for _, tt := range tests {
		got := ShouldRetry(cfg, tt.exitCode)
		if got != tt.want {
			t.Errorf("ShouldRetry(%d) = %v, want %v", tt.exitCode, got, tt.want)
		}
	}
}

func TestShouldRetry_PriorityDontRetry(t *testing.T) {
	// dont_retry_on_codes should have priority over retry_on_codes
	cfg := &RetryConfig{
		RetryOnCodes:     []int{1, 2, 3},
		DontRetryOnCodes: []int{2}, // Explicitly exclude 2 even though it's in retry list
	}

	tests := []struct {
		exitCode int
		want     bool
	}{
		{exitCode: 1, want: true},  // In retry list, not in dont_retry
		{exitCode: 2, want: false}, // In both lists, dont_retry wins
		{exitCode: 3, want: true},  // In retry list, not in dont_retry
		{exitCode: 4, want: false}, // Not in retry list
	}

	for _, tt := range tests {
		got := ShouldRetry(cfg, tt.exitCode)
		if got != tt.want {
			t.Errorf("ShouldRetry(%d) = %v, want %v (dont_retry should have priority)", tt.exitCode, got, tt.want)
		}
	}
}

func TestShouldRetry_CommonExitCodes(t *testing.T) {
	// Test realistic retry configuration for common scenarios
	cfg := &RetryConfig{
		RetryOnCodes: []int{
			1,   // Generic error
			137, // SIGKILL
			143, // SIGTERM
		},
		DontRetryOnCodes: []int{
			2,   // Syntax error / misuse of shell builtin
			127, // Command not found
		},
	}

	tests := []struct {
		exitCode int
		want     bool
		desc     string
	}{
		{exitCode: 1, want: true, desc: "generic error"},
		{exitCode: 2, want: false, desc: "syntax error"},
		{exitCode: 127, want: false, desc: "command not found"},
		{exitCode: 137, want: true, desc: "SIGKILL"},
		{exitCode: 143, want: true, desc: "SIGTERM"},
		{exitCode: 255, want: false, desc: "unknown error not in list"},
	}

	for _, tt := range tests {
		got := ShouldRetry(cfg, tt.exitCode)
		if got != tt.want {
			t.Errorf("ShouldRetry(%d) [%s] = %v, want %v", tt.exitCode, tt.desc, got, tt.want)
		}
	}
}
