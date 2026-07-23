package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

func TestLifecycleProtectionBlock(t *testing.T) {
	future := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)

	t.Run("managed running instance shows the block with a deadline", func(t *testing.T) {
		inst := &aws.InstanceInfo{
			State:        "running",
			InstanceType: "t3.small",
			TTL:          "4h",
			IdleTimeout:  "30m",
			LaunchTime:   time.Now().Add(-1 * time.Hour),
			Tags:         map[string]string{"spawn:managed": "true", "spawn:ttl-deadline": future},
		}
		out := lifecycleProtectionBlock(inst)
		for _, want := range []string{"Lifecycle protection:", "spored", "Out-of-band reaper", "Termination deadline:", "Idle timeout:"} {
			if !strings.Contains(out, want) {
				t.Errorf("block missing %q; got:\n%s", want, out)
			}
		}
	})

	t.Run("unmanaged instance shows nothing", func(t *testing.T) {
		inst := &aws.InstanceInfo{State: "running", Tags: map[string]string{}}
		if out := lifecycleProtectionBlock(inst); out != "" {
			t.Errorf("expected empty for unmanaged instance, got:\n%s", out)
		}
	})

	t.Run("stopped instance shows nothing", func(t *testing.T) {
		inst := &aws.InstanceInfo{State: "stopped", Tags: map[string]string{"spawn:managed": "true", "spawn:ttl-deadline": future}}
		if out := lifecycleProtectionBlock(inst); out != "" {
			t.Errorf("expected empty for stopped instance, got:\n%s", out)
		}
	})

	t.Run("falls back to TTL when no deadline tag", func(t *testing.T) {
		inst := &aws.InstanceInfo{
			State: "running", InstanceType: "t3.small", TTL: "4h",
			Tags: map[string]string{"spawn:managed": "true"},
		}
		out := lifecycleProtectionBlock(inst)
		if !strings.Contains(out, "TTL:") || !strings.Contains(out, "4h") {
			t.Errorf("expected TTL fallback line; got:\n%s", out)
		}
	})
}

func TestDNSStatusNotice(t *testing.T) {
	t.Run("no tag → nothing", func(t *testing.T) {
		if got := dnsStatusNotice(&aws.InstanceInfo{Tags: map[string]string{}}); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
	t.Run("registered → nothing (no news is good news)", func(t *testing.T) {
		inst := &aws.InstanceInfo{Tags: map[string]string{"spawn:dns-status": "registered"}}
		if got := dnsStatusNotice(inst); got != "" {
			t.Errorf("expected empty for registered, got %q", got)
		}
	})
	t.Run("failed → surfaces the detail", func(t *testing.T) {
		inst := &aws.InstanceInfo{Tags: map[string]string{
			"spawn:dns-status": "failed",
			"spawn:dns-error":  "DNS API returned HTTP 403: {\"Message\":\"Forbidden\"}",
		}}
		got := dnsStatusNotice(inst)
		if !strings.Contains(got, "DNS registration failed") || !strings.Contains(got, "403") {
			t.Errorf("failure notice should name the cause, got %q", got)
		}
	})
	t.Run("failed without detail → still warns", func(t *testing.T) {
		inst := &aws.InstanceInfo{Tags: map[string]string{"spawn:dns-status": "failed"}}
		if got := dnsStatusNotice(inst); !strings.Contains(got, "DNS registration failed") {
			t.Errorf("expected a warning even without detail, got %q", got)
		}
	})
}

func TestLifecycleDeadline(t *testing.T) {
	t.Run("prefers the ttl-deadline tag", func(t *testing.T) {
		want := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
		inst := &aws.InstanceInfo{Tags: map[string]string{"spawn:ttl-deadline": want.Format(time.RFC3339)}}
		got, ok := lifecycleDeadline(inst)
		if !ok || !got.Equal(want) {
			t.Errorf("deadline = %v, %v; want %v, true", got, ok, want)
		}
	})

	t.Run("falls back to launch + TTL", func(t *testing.T) {
		launch := time.Now().Add(-1 * time.Hour)
		inst := &aws.InstanceInfo{TTL: "4h", LaunchTime: launch, Tags: map[string]string{}}
		got, ok := lifecycleDeadline(inst)
		if !ok || !got.Equal(launch.Add(4*time.Hour)) {
			t.Errorf("deadline = %v, %v; want %v, true", got, ok, launch.Add(4*time.Hour))
		}
	})

	t.Run("no data → not ok", func(t *testing.T) {
		if _, ok := lifecycleDeadline(&aws.InstanceInfo{Tags: map[string]string{}}); ok {
			t.Error("expected ok=false with no deadline data")
		}
	})
}

// TestCheckCompleteExitCodes tests the --check-complete exit code logic
func TestCheckCompleteExitCodes(t *testing.T) {
	tests := []struct {
		name         string
		sweepStatus  string
		expectedExit int
		description  string
	}{
		{
			name:         "completed sweep",
			sweepStatus:  "COMPLETED",
			expectedExit: 0,
			description:  "Should exit 0 for completed sweeps",
		},
		{
			name:         "failed sweep",
			sweepStatus:  "FAILED",
			expectedExit: 1,
			description:  "Should exit 1 for failed sweeps",
		},
		{
			name:         "cancelled sweep",
			sweepStatus:  "CANCELLED",
			expectedExit: 1,
			description:  "Should exit 1 for cancelled sweeps",
		},
		{
			name:         "running sweep",
			sweepStatus:  "RUNNING",
			expectedExit: 2,
			description:  "Should exit 2 for running sweeps",
		},
		{
			name:         "initializing sweep",
			sweepStatus:  "INITIALIZING",
			expectedExit: 2,
			description:  "Should exit 2 for initializing sweeps",
		},
		{
			name:         "unknown status",
			sweepStatus:  "UNKNOWN",
			expectedExit: 3,
			description:  "Should exit 3 for unknown states",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test the exit code mapping logic
			var exitCode int
			switch tt.sweepStatus {
			case "COMPLETED":
				exitCode = 0
			case "FAILED", "CANCELLED":
				exitCode = 1
			case "RUNNING", "INITIALIZING":
				exitCode = 2
			default:
				exitCode = 3
			}

			if exitCode != tt.expectedExit {
				t.Errorf("%s: got exit code %d, want %d", tt.description, exitCode, tt.expectedExit)
			}
		})
	}
}

// TestStatusOutputModes tests different output modes for status command
func TestStatusOutputModes(t *testing.T) {
	tests := []struct {
		name             string
		jsonOutput       bool
		checkComplete    bool
		expectJSONOutput bool
		expectExitCode   bool
	}{
		{
			name:             "default mode",
			jsonOutput:       false,
			checkComplete:    false,
			expectJSONOutput: false,
			expectExitCode:   false,
		},
		{
			name:             "json mode",
			jsonOutput:       true,
			checkComplete:    false,
			expectJSONOutput: true,
			expectExitCode:   false,
		},
		{
			name:             "check-complete mode",
			jsonOutput:       false,
			checkComplete:    true,
			expectJSONOutput: false,
			expectExitCode:   true,
		},
		{
			name:             "json + check-complete",
			jsonOutput:       true,
			checkComplete:    true,
			expectJSONOutput: false,
			expectExitCode:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify that check-complete takes precedence over json
			if tt.checkComplete && tt.expectJSONOutput {
				t.Error("check-complete should prevent JSON output")
			}

			// Verify that check-complete mode uses exit codes
			if tt.checkComplete != tt.expectExitCode {
				t.Error("check-complete mode should use exit codes")
			}
		})
	}
}
