package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// TestValidateTTL_ValidFormats validates correct TTL formats
func TestValidateTTL_ValidFormats(t *testing.T) {
	tests := []struct {
		name string
		ttl  string
	}{
		{"Seconds only", "30s"},
		{"Minutes only", "15m"},
		{"Hours only", "2h"},
		{"Days only", "7d"},
		{"Hours and minutes", "2h30m"},
		{"Days and hours", "1d12h"},
		{"Days, hours, minutes", "1d2h30m"},
		{"All units", "1d2h30m15s"},
		{"Large hours", "48h"},
		{"Large days", "30d"},
		{"Single digit", "1h"},
		{"Double digit", "10m"},
		{"Triple digit", "100h"},
		{"Short minute", "1m"},
		{"Short hour", "1h"},
		{"Short day", "1d"},
		{"Multiple hours and minutes", "3h45m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if err != nil {
				t.Errorf("validateTTL(%q) returned error: %v, want nil", tt.ttl, err)
			}
		})
	}
}

// TestValidateTTL_InvalidFormats validates rejection of incorrect formats
func TestValidateTTL_InvalidFormats(t *testing.T) {
	tests := []struct {
		name        string
		ttl         string
		expectedErr string
	}{
		{"Empty string", "", "TTL must be in format"},
		{"No unit", "30", "TTL must be in format"},
		{"Invalid unit - w", "1w", "TTL must be in format"},
		{"Invalid unit - y", "1y", "TTL must be in format"},
		{"Invalid unit - M", "1M", "TTL must be in format"},
		{"Space between number and unit", "2 h", "TTL must be in format"},
		{"Space between components", "2h 30m", "TTL must be in format"},
		{"Decimal number", "2.5h", "TTL must be in format"},
		{"Negative number", "-1h", "TTL must be in format"},
		{"Just unit", "h", "TTL must be in format"},
		{"Unit before number", "h2", "TTL must be in format"},
		{"Multiple spaces", "2h  30m", "TTL must be in format"},
		{"Comma separator", "2h,30m", "TTL must be in format"},
		{"Special characters", "2h!30m", "TTL must be in format"},
		{"Uppercase units", "2H", "TTL must be in format"},
		{"Mixed case", "2H30m", "TTL must be in format"},
		{"Zero seconds", "0s", "TTL must be greater than 0"},
		{"Zero minutes", "0m", "TTL must be greater than 0"},
		{"Zero hours", "0h", "TTL must be greater than 0"},
		{"Zero days", "0d", "TTL must be greater than 0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if err == nil {
				t.Errorf("validateTTL(%q) returned nil, expected error containing %q", tt.ttl, tt.expectedErr)
			} else if tt.expectedErr != "" && !strings.Contains(err.Error(), tt.expectedErr) {
				t.Errorf("validateTTL(%q) error = %v, want error containing %q", tt.ttl, err, tt.expectedErr)
			}
		})
	}
}

// TestValidateTTL_EdgeCases validates edge cases and boundary conditions
func TestValidateTTL_EdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		ttl       string
		shouldErr bool
	}{
		{"Very large number", "999999h", false},
		{"Maximum reasonable", "365d", false},
		{"One second", "1s", false},
		{"Multiple of same unit", "1h2h", false}, // Weird but valid format
		{"Out of order units", "30m2h", false},   // Order doesn't matter for validation
		{"Duplicate units", "1h1h", false},       // Weird but technically valid
		{"Very long chain", "1d1h1m1s", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if tt.shouldErr && err == nil {
				t.Errorf("validateTTL(%q) returned nil, expected error", tt.ttl)
			}
			if !tt.shouldErr && err != nil {
				t.Errorf("validateTTL(%q) returned error: %v, expected nil", tt.ttl, err)
			}
		})
	}
}

// TestValidateTTL_TotalDuration validates that total duration is calculated correctly
func TestValidateTTL_TotalDuration(t *testing.T) {
	// These are all valid formats, we're just ensuring they parse without error
	tests := []struct {
		name string
		ttl  string
	}{
		{"60 seconds = 1 minute", "60s"},
		{"60 minutes = 1 hour", "60m"},
		{"24 hours = 1 day", "24h"},
		{"Combined equals 1 day", "23h60m"}, // 23h + 60m = 24h
		{"Multiple components", "1d23h59m59s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if err != nil {
				t.Errorf("validateTTL(%q) returned error: %v, want nil", tt.ttl, err)
			}
		})
	}
}

// TestFormatTTLDuration validates TTL formatting for display
// TestValidateTTL_CommonUseCases validates typical user input patterns
func TestValidateTTL_CommonUseCases(t *testing.T) {
	tests := []struct {
		name      string
		ttl       string
		shouldErr bool
	}{
		{"Quick task - 30 min", "30m", false},
		{"Short session - 2 hours", "2h", false},
		{"Work day - 8 hours", "8h", false},
		{"Overnight - 12 hours", "12h", false},
		{"Full day", "24h", false},
		{"Weekend", "2d", false},
		{"Week", "7d", false},
		{"Two weeks", "14d", false},
		{"Month", "30d", false},
		{"Development session", "4h", false},
		{"Extended session", "3h30m", false},
		{"Typo - forgot unit", "2", true},
		{"Typo - wrong unit", "2hrs", true},
		{"Typo - plural", "2hours", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if tt.shouldErr && err == nil {
				t.Errorf("validateTTL(%q) returned nil, expected error", tt.ttl)
			}
			if !tt.shouldErr && err != nil {
				t.Errorf("validateTTL(%q) returned error: %v, expected nil", tt.ttl, err)
			}
		})
	}
}

// TestValidateTTL_ZeroComponents validates handling of zero-value components
func TestValidateTTL_ZeroComponents(t *testing.T) {
	tests := []struct {
		name      string
		ttl       string
		shouldErr bool
	}{
		{"Just zero seconds", "0s", true},
		{"Just zero minutes", "0m", true},
		{"Just zero hours", "0h", true},
		{"Just zero days", "0d", true},
		{"Zero with valid - start", "0s30m", false}, // Total > 0
		{"Zero with valid - middle", "2h0m30s", false},
		{"Zero with valid - end", "2h30m0s", false},
		{"All zeros", "0d0h0m0s", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if tt.shouldErr && err == nil {
				t.Errorf("validateTTL(%q) returned nil, expected error", tt.ttl)
			}
			if !tt.shouldErr && err != nil {
				t.Errorf("validateTTL(%q) returned error: %v, expected nil", tt.ttl, err)
			}
		})
	}
}

// TestValidateTTL_UnitOrder validates that unit order doesn't matter
func TestValidateTTL_UnitOrder(t *testing.T) {
	// All of these should be valid - order doesn't matter for validation
	tests := []string{
		"2h30m",   // Normal order
		"30m2h",   // Reverse order
		"1d12h",   // Normal
		"12h1d",   // Reverse
		"30s5m2h", // Reverse order
		"2h5m30s", // Normal order
	}

	for _, ttl := range tests {
		t.Run(ttl, func(t *testing.T) {
			err := validateTTL(ttl)
			if err != nil {
				t.Errorf("validateTTL(%q) returned error: %v, want nil (order shouldn't matter)", ttl, err)
			}
		})
	}
}

// TestValidateTTL_PrefixSuffix validates rejection of extra characters
func TestValidateTTL_PrefixSuffix(t *testing.T) {
	tests := []struct {
		name string
		ttl  string
	}{
		{"Leading text", "ttl2h"},
		{"Trailing text", "2hmin"},
		{"Leading space", " 2h"},
		{"Trailing space", "2h "},
		{"Leading dash", "-2h"},
		{"Leading plus", "+2h"},
		{"Parentheses", "(2h)"},
		{"Quotes", "\"2h\""},
		{"Equals", "ttl=2h"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTTL(tt.ttl)
			if err == nil {
				t.Errorf("validateTTL(%q) returned nil, expected error due to extra characters", tt.ttl)
			}
		})
	}
}

// TestComputeExtendedTTLTags_DeadlineAdvancesAndTTLTracksTotal is the
// regression guard for spawn#506: after an extend, spawn:ttl must describe
// the TOTAL duration from launch to the new deadline — not the raw extend
// argument — so a repeated `extend <id> 8h` visibly reflects two real
// extensions instead of looking unchanged.
func TestComputeExtendedTTLTags_DeadlineAdvancesAndTTLTracksTotal(t *testing.T) {
	now := time.Date(2026, 8, 18, 6, 0, 0, 0, time.UTC)
	launch := now.Add(-1 * time.Hour) // instance has been up 1h
	firstDeadline := launch.Add(8 * time.Hour)

	instance := &aws.InstanceInfo{
		LaunchTime: launch,
		TTL:        "8h",
		Tags:       map[string]string{"spawn:ttl-deadline": firstDeadline.UTC().Format(time.RFC3339)},
	}

	tags, newDeadline := computeExtendedTTLTags(instance, "8h", 8*time.Hour, now)

	wantDeadline := firstDeadline.Add(8 * time.Hour)
	if !newDeadline.Equal(wantDeadline) {
		t.Errorf("newDeadline = %v, want %v", newDeadline, wantDeadline)
	}
	if got := tags["spawn:ttl-deadline"]; got != wantDeadline.UTC().Format(time.RFC3339) {
		t.Errorf("spawn:ttl-deadline = %q, want %q", got, wantDeadline.UTC().Format(time.RFC3339))
	}
	// spawn:ttl must be the total from launch (1h elapsed + 8h original + 8h
	// extend = 17h), NOT "8h" (the raw argument, spawn#506's bug).
	wantTTL := wantDeadline.Sub(launch).Round(time.Second).String()
	if got := tags["spawn:ttl"]; got != wantTTL {
		t.Errorf("spawn:ttl = %q, want %q (total from launch)", got, wantTTL)
	}

	// Extending AGAIN by the same amount must move the deadline a second
	// time, not appear as a no-op.
	instance2 := &aws.InstanceInfo{
		LaunchTime: launch,
		TTL:        tags["spawn:ttl"],
		Tags:       map[string]string{"spawn:ttl-deadline": tags["spawn:ttl-deadline"]},
	}
	tags2, newDeadline2 := computeExtendedTTLTags(instance2, "8h", 8*time.Hour, now)
	if !newDeadline2.After(newDeadline) {
		t.Errorf("second extend did not advance the deadline further: first=%v second=%v", newDeadline, newDeadline2)
	}
	if tags2["spawn:ttl"] == tags["spawn:ttl"] {
		t.Error("second extend's spawn:ttl is identical to the first — a repeated extend must visibly change it")
	}
}

// TestComputeExtendedTTLTags_FloorPreventsImmediateReap confirms a
// past/expired deadline doesn't reap the instance the instant it's extended.
func TestComputeExtendedTTLTags_FloorPreventsImmediateReap(t *testing.T) {
	now := time.Date(2026, 8, 18, 6, 0, 0, 0, time.UTC)
	launch := now.Add(-9 * time.Hour)
	expiredDeadline := launch.Add(8 * time.Hour) // 1h in the past

	instance := &aws.InstanceInfo{
		LaunchTime: launch,
		TTL:        "8h",
		Tags:       map[string]string{"spawn:ttl-deadline": expiredDeadline.UTC().Format(time.RFC3339)},
	}

	_, newDeadline := computeExtendedTTLTags(instance, "30m", 30*time.Minute, now)

	floor := now.Add(30 * time.Minute)
	if newDeadline.Before(floor) {
		t.Errorf("newDeadline = %v, want >= floor %v (must not reap immediately after extend)", newDeadline, floor)
	}
}

// TestComputeExtendedTTLTags_NoDeadlineTagFallsBackToTTL covers an older
// instance that predates the spawn:ttl-deadline tag.
func TestComputeExtendedTTLTags_NoDeadlineTagFallsBackToTTL(t *testing.T) {
	now := time.Date(2026, 8, 18, 6, 0, 0, 0, time.UTC)
	launch := now.Add(-2 * time.Hour)

	instance := &aws.InstanceInfo{
		LaunchTime: launch,
		TTL:        "8h",
		Tags:       map[string]string{}, // no spawn:ttl-deadline
	}

	tags, newDeadline := computeExtendedTTLTags(instance, "4h", 4*time.Hour, now)

	wantDeadline := now.Add(8 * time.Hour).Add(4 * time.Hour)
	if !newDeadline.Equal(wantDeadline) {
		t.Errorf("newDeadline = %v, want %v", newDeadline, wantDeadline)
	}
	wantTTL := wantDeadline.Sub(launch).Round(time.Second).String()
	if got := tags["spawn:ttl"]; got != wantTTL {
		t.Errorf("spawn:ttl = %q, want %q", got, wantTTL)
	}
}

// TestExtendJobArrayInstances_ReloadFailureIsTrackedNotDiscarded is the
// regression test for spawn#512: before the fix, a per-instance
// triggerReload error was discarded with `_ = triggerReload(&inst)`, so a
// caller extending a job array had no way to tell which instances actually
// picked up the new TTL on-instance. This asserts the reload failure is
// both reported (printed to stderr) and tracked (returned) even though the
// tag write itself succeeded.
func TestExtendJobArrayInstances_ReloadFailureIsTrackedNotDiscarded(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-good", Region: "us-east-1", PublicIP: "1.2.3.4"},
		{InstanceID: "i-reload-fails", Region: "us-east-1", PublicIP: "5.6.7.8"},
	}

	updateTags := func(region, instanceID string) error {
		return nil // every tag write succeeds
	}
	reload := func(inst *aws.InstanceInfo) error {
		if inst.InstanceID == "i-reload-fails" {
			return errors.New("ssh: connection refused")
		}
		return nil
	}

	var stderr bytes.Buffer
	successCount, failedInstances, reloadFailedInstances := extendJobArrayInstances(instances, updateTags, reload, &stderr)

	if successCount != 2 {
		t.Errorf("successCount = %d, want 2 (both tag writes succeeded)", successCount)
	}
	if len(failedInstances) != 0 {
		t.Errorf("failedInstances = %v, want empty (no tag write failed)", failedInstances)
	}
	if len(reloadFailedInstances) != 1 || reloadFailedInstances[0] != "i-reload-fails" {
		t.Errorf("reloadFailedInstances = %v, want [i-reload-fails]", reloadFailedInstances)
	}
	if !strings.Contains(stderr.String(), "i-reload-fails") || !strings.Contains(stderr.String(), "connection refused") {
		t.Errorf("stderr does not report the reload failure: %q", stderr.String())
	}
}

// TestExtendJobArrayInstances_TagFailureSkipsReload confirms a failed tag
// write is still tracked in failedInstances and does not attempt a reload
// (there is nothing to reload if the tag never changed).
func TestExtendJobArrayInstances_TagFailureSkipsReload(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-tag-fails", Region: "us-east-1"},
	}

	updateTags := func(region, instanceID string) error {
		return errors.New("access denied")
	}
	reloadCalled := false
	reload := func(inst *aws.InstanceInfo) error {
		reloadCalled = true
		return nil
	}

	var stderr bytes.Buffer
	successCount, failedInstances, reloadFailedInstances := extendJobArrayInstances(instances, updateTags, reload, &stderr)

	if successCount != 0 {
		t.Errorf("successCount = %d, want 0", successCount)
	}
	if len(failedInstances) != 1 || failedInstances[0] != "i-tag-fails" {
		t.Errorf("failedInstances = %v, want [i-tag-fails]", failedInstances)
	}
	if len(reloadFailedInstances) != 0 {
		t.Errorf("reloadFailedInstances = %v, want empty", reloadFailedInstances)
	}
	if reloadCalled {
		t.Error("reload should not be attempted when the tag write itself failed")
	}
}

// TestExtendJobArrayInstances_AllSucceed confirms the happy path produces no
// stderr warnings.
func TestExtendJobArrayInstances_AllSucceed(t *testing.T) {
	instances := []aws.InstanceInfo{
		{InstanceID: "i-a", Region: "us-east-1"},
		{InstanceID: "i-b", Region: "us-east-1"},
	}
	noErr := func(string, string) error { return nil }
	noErrReload := func(*aws.InstanceInfo) error { return nil }

	var stderr bytes.Buffer
	successCount, failedInstances, reloadFailedInstances := extendJobArrayInstances(instances, noErr, noErrReload, &stderr)

	if successCount != 2 || len(failedInstances) != 0 || len(reloadFailedInstances) != 0 {
		t.Errorf("got success=%d failed=%v reloadFailed=%v, want success=2 failed=[] reloadFailed=[]",
			successCount, failedInstances, reloadFailedInstances)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr should be empty on full success, got %q", stderr.String())
	}
}
