package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/spore-host/spawn/pkg/aws"
)

// #990: `spawn launch --ttl 1d` tagged spawn:ttl=1d with no deadline. The
// launch path now parses --ttl with the shared parser and refuses what it
// cannot parse, before the first AWS call (runLaunch calls
// validateLaunchDurationFlags first) and again on the assembled config.
func TestBuildLaunchConfig_TTL(t *testing.T) {
	prevTTL, prevIdle, prevType := ttl, idleTimeout, instanceType
	t.Cleanup(func() { ttl, idleTimeout, instanceType = prevTTL, prevIdle, prevType })
	instanceType = "t3.micro"

	for _, ok := range []string{"1d", "2d12h", "8h", "90m", "1.5h"} {
		ttl = ok
		cfg, err := buildLaunchConfig(nil)
		if err != nil {
			t.Errorf("--ttl %s: %v", ok, err)
			continue
		}
		if cfg.TTL != ok {
			t.Errorf("--ttl %s: config.TTL = %q", ok, cfg.TTL)
		}
		if err := validateLaunchDurationFlags(); err != nil {
			t.Errorf("--ttl %s: validateLaunchDurationFlags: %v", ok, err)
		}
	}
	for _, bad := range []string{"1w", "2weeks", "0", "-1h", "1 d"} {
		ttl = bad
		if _, err := buildLaunchConfig(nil); err == nil || !strings.Contains(err.Error(), "--ttl") {
			t.Errorf("--ttl %q: buildLaunchConfig err = %v, want an --ttl error", bad, err)
		}
		if err := validateLaunchDurationFlags(); err == nil {
			t.Errorf("--ttl %q: validateLaunchDurationFlags accepted it", bad)
		}
	}

	ttl = ""
	idleTimeout = "forever"
	if _, err := buildLaunchConfig(nil); err == nil {
		t.Error("--idle-timeout forever: accepted; spored would ignore it and enforce no idle timeout")
	}
	if err := validateLaunchDurationFlags(); err == nil {
		t.Error("--idle-timeout forever: validateLaunchDurationFlags accepted it")
	}
}

func TestParseExtendDuration_Days(t *testing.T) {
	for in, want := range map[string]time.Duration{"1d": 24 * time.Hour, "2d12h": 60 * time.Hour, "30m": 30 * time.Minute} {
		if got, err := parseExtendDuration(in); err != nil || got != want {
			t.Errorf("parseExtendDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

// Param-file ttl/idle_timeout: rejected with every problem named, rather than
// dropped (a YAML int) or tagged without a deadline (#990).
func TestValidateSweepDurations(t *testing.T) {
	pf := &ParamFileFormat{
		Defaults: map[string]interface{}{"ttl": "1d"},
		Params: []map[string]interface{}{
			{"ttl": "2d12h", "idle_timeout": "30m"},
			{"ttl": "1w"},
			{"ttl": 3600},
			{"idle_timeout": "soon"},
		},
	}
	err := validateSweepDurations(pf)
	if err == nil {
		t.Fatal("accepted a param file with three bad durations")
	}
	for _, want := range []string{"row 1", "row 2", "row 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "row 0") || strings.Contains(err.Error(), "defaults") {
		t.Errorf("error blames a valid entry:\n%v", err)
	}

	pf.Params = pf.Params[:1]
	if err := validateSweepDurations(pf); err != nil {
		t.Errorf("clean file rejected: %v", err)
	}
	cfg, err := buildLaunchConfigFromParams(aws.LaunchConfig{}, pf.Defaults, pf.Params[0], "s", "n", 0, 1)
	if err != nil || cfg.TTL != "2d12h" || cfg.IdleTimeout != "30m" {
		t.Errorf("buildLaunchConfigFromParams = TTL %q idle %q err %v", cfg.TTL, cfg.IdleTimeout, err)
	}
	if _, err := buildLaunchConfigFromParams(aws.LaunchConfig{}, nil, map[string]interface{}{"ttl": 3600}, "s", "n", 0, 1); err == nil {
		t.Error("buildLaunchConfigFromParams accepted ttl: 3600 (an int); it used to be dropped silently")
	}
}
